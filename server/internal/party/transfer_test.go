package party

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
)

func (s *stack) transfer(t *testing.T, token string, partyID uuid.UUID, key string, version int64, targetUserID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"user_id":"` + targetUserID + `"}`
	return s.mut(t, http.MethodPost, "/v1/parties/"+partyID.String()+"/owner", token, key, `"`+strconv.FormatInt(version, 10)+`"`, body)
}

func ownerCount(t *testing.T, s *stack, partyID uuid.UUID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM party_memberships WHERE party_id = $1 AND status = 'active' AND role = 'owner'`, partyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func partyVersion(t *testing.T, s *stack, partyID uuid.UUID) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(context.Background(), `SELECT version FROM parties WHERE id = $1`, partyID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func changeCount(t *testing.T, s *stack, recipient, entityType, operation string, entityID any) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = $2 AND operation = $3`
	args := []any{recipient, entityType, operation}
	if entityID != nil {
		q += ` AND entity_id = $4`
		args = append(args, entityID)
	}
	if err := s.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTransferOwnerSwapsRolesRevokesInvitesAndSyncs(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "xfer."+uuid.NewString())
	target := s.signIn(t, "xfer."+uuid.NewString())
	third := s.signIn(t, "xfer."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "xfer-create")
	targetMembership := addMember(t, s.pool, partyID, uuid.MustParse(target.UserID))
	addMember(t, s.pool, partyID, uuid.MustParse(third.UserID))
	inv1 := s.mustInvite(t, owner.AccessToken, partyID, "xfer-inv-1", `{"max_uses":2}`)
	inv2 := s.mustInvite(t, owner.AccessToken, partyID, "xfer-inv-2", `{"max_uses":2}`)
	before := partyVersion(t, s, partyID)

	rec := s.transfer(t, owner.AccessToken, partyID, "xfer-1", before, target.UserID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[partyEnvelope](t, rec).Party
	if got.OwnerMembershipID != targetMembership || got.Version != before+1 {
		t.Fatalf("party = %+v, want owner %s version %d", got, targetMembership, before+1)
	}
	if rec.Header().Get("ETag") != etag(before+1) {
		t.Fatalf("ETag = %q", rec.Header().Get("ETag"))
	}

	if n := ownerCount(t, s, partyID); n != 1 {
		t.Fatalf("활성 방장 = %d", n)
	}
	var newOwnerRole, oldOwnerRole string
	if err := s.pool.QueryRow(context.Background(), `SELECT role FROM party_memberships WHERE id = $1`, targetMembership).Scan(&newOwnerRole); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(context.Background(),
		`SELECT role FROM party_memberships WHERE party_id = $1 AND user_id = $2`, partyID, owner.UserID).Scan(&oldOwnerRole); err != nil {
		t.Fatal(err)
	}
	if newOwnerRole != "owner" || oldOwnerRole != "member" {
		t.Fatalf("roles new=%s old=%s", newOwnerRole, oldOwnerRole)
	}

	// 활성 초대는 모두 revoked이고, 이전 링크로는 더 이상 가입할 수 없다.
	assertCount(t, s.pool, `SELECT count(*) FROM party_invites WHERE party_id = $1 AND status = 'active'`, partyID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM party_invites WHERE party_id = $1 AND status = 'revoked' AND revoked_at IS NOT NULL`, partyID, 2)
	outsider := s.signIn(t, "xfer."+uuid.NewString())
	assertError(t, s.accept(t, outsider.AccessToken, inv1.Token, "xfer-accept"), http.StatusNotFound, httpapi.CodeNotFound)

	// 초대 tombstone은 이전 방장과 새 방장에게만 간다. 일반 멤버는 초대 존재를 알 수 없다.
	for _, inv := range []uuid.UUID{inv1.ID, inv2.ID} {
		if n := changeCount(t, s, owner.UserID, "party_invite", "tombstone", inv); n != 1 {
			t.Fatalf("이전 방장의 초대 tombstone = %d", n)
		}
		if n := changeCount(t, s, target.UserID, "party_invite", "tombstone", inv); n != 1 {
			t.Fatalf("새 방장의 초대 tombstone = %d", n)
		}
		if n := changeCount(t, s, third.UserID, "party_invite", "tombstone", inv); n != 0 {
			t.Fatalf("일반 멤버가 초대 tombstone을 받았다: %d", n)
		}
	}
	// 모든 활성 멤버가 party와 두 멤버십의 upsert를 받는다(위임 이후 version).
	for _, u := range []string{owner.UserID, target.UserID, third.UserID} {
		var n int
		if err := s.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM sync_changes
			 WHERE recipient_user_id = $1 AND entity_type = 'party' AND operation = 'upsert' AND entity_version = $2`, u, before+1).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%s의 party upsert = %d", u, n)
		}
		if c := changeCount(t, s, u, "party_membership", "upsert", targetMembership); c < 1 {
			t.Fatalf("%s가 새 방장 멤버십 upsert를 받지 못했다", u)
		}
	}
	// 알림 job은 party_id:version dedupe로 하나다. payload에 이름·token이 없다.
	assertCount(t, s.pool, `SELECT count(*) FROM outbox_jobs WHERE type = 'notify_owner_transferred' AND dedupe_key = $1`,
		partyID.String()+":"+strconv.FormatInt(before+1, 10), 1)
}

func TestTransferOwnerRejections(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "xfer.rej."+uuid.NewString())
	member := s.signIn(t, "xfer.rej."+uuid.NewString())
	other := s.signIn(t, "xfer.rej."+uuid.NewString())
	outsider := s.signIn(t, "xfer.rej."+uuid.NewString())
	left := s.signIn(t, "xfer.rej."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "xfer-rej-create")
	addMember(t, s.pool, partyID, uuid.MustParse(member.UserID))
	addMember(t, s.pool, partyID, uuid.MustParse(other.UserID))
	leftMembership := addMember(t, s.pool, partyID, uuid.MustParse(left.UserID))
	// 떠난 멤버십(status='left')은 이력이 있어도 위임 대상이 될 수 없다.
	if _, err := s.pool.Exec(context.Background(), `
		UPDATE party_memberships
		   SET status = 'left', ended_at = now(), end_reason = 'left_voluntarily', version = version + 1
		 WHERE id = $1`, leftMembership); err != nil {
		t.Fatal(err)
	}
	v := partyVersion(t, s, partyID)

	// 일반 멤버는 403, 비멤버는 존재를 숨겨 404.
	assertError(t, s.transfer(t, member.AccessToken, partyID, "r1", v, other.UserID), http.StatusForbidden, httpapi.CodeForbidden)
	assertError(t, s.transfer(t, outsider.AccessToken, partyID, "r2", v, other.UserID), http.StatusNotFound, httpapi.CodeNotFound)
	// 자기 자신, 잘못된 user_id, 활성 멤버가 아닌 대상은 모두 400이다(설계 §10).
	assertError(t, s.transfer(t, owner.AccessToken, partyID, "r3", v, owner.UserID), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.transfer(t, owner.AccessToken, partyID, "r4", v, "not-a-uuid"), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.transfer(t, owner.AccessToken, partyID, "r5", v, outsider.UserID), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.transfer(t, owner.AccessToken, partyID, "r5b", v, left.UserID), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	// 낡은 If-Match.
	assertError(t, s.transfer(t, owner.AccessToken, partyID, "r6", v+7, member.UserID), http.StatusConflict, httpapi.CodeVersionConflict)
	// If-Match가 없으면 400.
	assertError(t, s.mut(t, http.MethodPost, "/v1/parties/"+partyID.String()+"/owner", owner.AccessToken, "r7", "", `{"user_id":"`+member.UserID+`"}`),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)

	// 거부된 요청은 아무것도 바꾸지 않는다.
	if n := ownerCount(t, s, partyID); n != 1 || partyVersion(t, s, partyID) != v {
		t.Fatalf("거부 뒤 상태가 변했다: owners=%d version=%d", n, partyVersion(t, s, partyID))
	}
	var ownerUser string
	if err := s.pool.QueryRow(context.Background(), `
		SELECT user_id::text FROM party_memberships WHERE party_id = $1 AND status = 'active' AND role = 'owner'`, partyID).Scan(&ownerUser); err != nil {
		t.Fatal(err)
	}
	if ownerUser != owner.UserID {
		t.Fatalf("방장이 바뀌었다: %s", ownerUser)
	}
}

func TestTransferOwnerReplayAndSecondTransferAreForbiddenForOldOwner(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "xfer.rep."+uuid.NewString())
	target := s.signIn(t, "xfer.rep."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "xfer-rep-create")
	addMember(t, s.pool, partyID, uuid.MustParse(target.UserID))
	v := partyVersion(t, s, partyID)
	if rec := s.transfer(t, owner.AccessToken, partyID, "rep-1", v, target.UserID); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	// 같은 키 재전송에서도 현재 권한을 먼저 평가하므로 이전 방장은 403이다(§9.4).
	assertError(t, s.transfer(t, owner.AccessToken, partyID, "rep-1", v, target.UserID), http.StatusForbidden, httpapi.CodeForbidden)
	// 새 방장은 다시 넘길 수 있다.
	if rec := s.transfer(t, target.AccessToken, partyID, "rep-2", v+1, owner.UserID); rec.Code != http.StatusOK {
		t.Fatalf("되돌리기 status = %d: %s", rec.Code, rec.Body.String())
	}
	if n := ownerCount(t, s, partyID); n != 1 {
		t.Fatalf("활성 방장 = %d", n)
	}
}

func TestTransferOwnerOnDisbandedPartyIsGone(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "xfer.gone."+uuid.NewString())
	target := s.signIn(t, "xfer.gone."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "xfer-gone-create")
	addMember(t, s.pool, partyID, uuid.MustParse(target.UserID))
	v := partyVersion(t, s, partyID)
	disbandParty(t, s.pool, partyID)
	assertError(t, s.transfer(t, owner.AccessToken, partyID, "gone-1", v, target.UserID), http.StatusGone, codePartyDisbanded)
}

// 인수 조건: 동시 위임 요청 중 하나만 성공하고 방장은 항상 정확히 1명이다. 교착 재시도도 없다.
func TestConcurrentTransfersLeaveExactlyOneOwner(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "xfer.race."+uuid.NewString())
	a := s.signIn(t, "xfer.race."+uuid.NewString())
	b := s.signIn(t, "xfer.race."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "xfer-race-create")
	addMember(t, s.pool, partyID, uuid.MustParse(a.UserID))
	addMember(t, s.pool, partyID, uuid.MustParse(b.UserID))
	v := partyVersion(t, s, partyID)
	retriesBefore := mutation.Retries()

	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i, target := range []string{a.UserID, b.UserID} {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			<-start
			codes <- s.transfer(t, owner.AccessToken, partyID, "race-"+strconv.Itoa(i), v, target).Code
		}(i, target)
	}
	close(start)
	wg.Wait()
	close(codes)
	ok, other := 0, 0
	for c := range codes {
		if c == http.StatusOK {
			ok++
		} else if c == http.StatusConflict || c == http.StatusForbidden {
			other++
		} else {
			t.Fatalf("예상하지 못한 status = %d", c)
		}
	}
	if ok != 1 || other != 1 {
		t.Fatalf("성공 %d, 거부 %d", ok, other)
	}
	if n := ownerCount(t, s, partyID); n != 1 {
		t.Fatalf("활성 방장 = %d", n)
	}
	if got := mutation.Retries() - retriesBefore; got != 0 {
		t.Fatalf("교착 재시도 증가 = %d", got)
	}
}

// 위임과 초대 수락이 동시에 와도 교착하지 않고 방장이 1명이다. 위임이 먼저 커밋되면 수락은
// 404, 수락이 먼저면 새 멤버가 남고 초대는 위임이 revoked로 만든다.
func TestTransferRacingWithAcceptDoesNotDeadlock(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "xfer.acc."+uuid.NewString())
	target := s.signIn(t, "xfer.acc."+uuid.NewString())
	joiner := s.signIn(t, "xfer.acc."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "xfer-acc-create")
	addMember(t, s.pool, partyID, uuid.MustParse(target.UserID))
	inv := s.mustInvite(t, owner.AccessToken, partyID, "xfer-acc-inv", `{"max_uses":3}`)
	v := partyVersion(t, s, partyID)
	retriesBefore := mutation.Retries()

	start := make(chan struct{})
	var wg sync.WaitGroup
	var xferCode, acceptCode int
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		xferCode = s.transfer(t, owner.AccessToken, partyID, "acc-x", v, target.UserID).Code
	}()
	go func() {
		defer wg.Done()
		<-start
		acceptCode = s.accept(t, joiner.AccessToken, inv.Token, "acc-a").Code
	}()
	close(start)
	wg.Wait()

	// 수락이 Party version을 올리면 위임은 409일 수 있다. 어느 쪽이든 방장은 정확히 1명이다.
	if xferCode != http.StatusOK && xferCode != http.StatusConflict {
		t.Fatalf("위임 status = %d", xferCode)
	}
	if acceptCode != http.StatusCreated && acceptCode != http.StatusNotFound {
		t.Fatalf("수락 status = %d", acceptCode)
	}
	if n := ownerCount(t, s, partyID); n != 1 {
		t.Fatalf("활성 방장 = %d", n)
	}
	if got := mutation.Retries() - retriesBefore; got != 0 {
		t.Fatalf("교착 재시도 증가 = %d", got)
	}
	// 두 요청은 서로 겹칠 수 없다. 위임이 먼저면 초대가 revoked라 수락은 404, 수락이 먼저면
	// Party version이 올라 낡은 If-Match의 위임은 409다.
	if (xferCode == http.StatusOK) != (acceptCode == http.StatusNotFound) {
		t.Fatalf("상관 위반: 위임 %d, 수락 %d", xferCode, acceptCode)
	}
	if xferCode == http.StatusOK {
		assertCount(t, s.pool, `SELECT count(*) FROM party_invites WHERE party_id = $1 AND status = 'active'`, partyID, 0)
	}
}

// 저장된 status가 active이지만 이미 만료된 초대도 위임 때 revoked가 된다. 활성 초대 개수와
// 수락 경로는 expires_at을 따로 보지만, 위임 뒤에는 어떤 상태로도 남아 있으면 안 된다.
func TestTransferRevokesExpiredButStillActiveInvites(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "xfer.exp."+uuid.NewString())
	target := s.signIn(t, "xfer.exp."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "xfer-exp-create")
	addMember(t, s.pool, partyID, uuid.MustParse(target.UserID))
	inv := s.mustInvite(t, owner.AccessToken, partyID, "xfer-exp-inv", `{"max_uses":2}`)
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE party_invites SET expires_at = now() - interval '1 hour' WHERE id = $1`, inv.ID); err != nil {
		t.Fatal(err)
	}
	if rec := s.transfer(t, owner.AccessToken, partyID, "xfer-exp-1", partyVersion(t, s, partyID), target.UserID); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var status string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM party_invites WHERE id = $1`, inv.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "revoked" {
		t.Fatalf("status = %s, want revoked", status)
	}
}
