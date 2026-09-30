package party

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
)

func (s *stack) createParty(t *testing.T, accessToken, key string) uuid.UUID {
	t.Helper()
	rec := s.mut(t, http.MethodPost, "/v1/parties", accessToken, key, "", `{"name":"Invite Party"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Party 생성 status = %d: %s", rec.Code, rec.Body.String())
	}
	return decode[partyEnvelope](t, rec).Party.ID
}

func (s *stack) createInvite(t *testing.T, accessToken string, partyID uuid.UUID, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return s.mut(t, http.MethodPost, "/v1/parties/"+partyID.String()+"/invites", accessToken, key, "", body)
}

func (s *stack) mustInvite(t *testing.T, accessToken string, partyID uuid.UUID, key, body string) inviteResponse {
	t.Helper()
	rec := s.createInvite(t, accessToken, partyID, key, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("초대 생성 status = %d: %s", rec.Code, rec.Body.String())
	}
	inv := decode[inviteEnvelope](t, rec).Invite
	if inv.Token == "" {
		t.Fatal("생성 응답에 token이 없다")
	}
	return inv
}

func (s *stack) accept(t *testing.T, accessToken, inviteToken, key string) *httptest.ResponseRecorder {
	t.Helper()
	return s.mut(t, http.MethodPost, "/v1/invites/"+inviteToken+"/accept", accessToken, key, "", "")
}

func (s *stack) activeMembers(t *testing.T, partyID uuid.UUID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM party_memberships WHERE party_id = $1 AND status = 'active'`, partyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func usedCount(t *testing.T, pool *pgxpool.Pool, inviteID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT used_count FROM party_invites WHERE id = $1`, inviteID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInviteCreateListRevokeAndTokenSecrecy(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "invite."+uuid.NewString())
	member := s.signIn(t, "invite."+uuid.NewString())
	outsider := s.signIn(t, "invite."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "p-create")
	addMember(t, s.pool, partyID, uuid.MustParse(member.UserID))

	rec := s.createInvite(t, owner.AccessToken, partyID, "inv-1", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	inv := decode[inviteEnvelope](t, rec).Invite
	if len(inv.Token) != 43 {
		t.Fatalf("token 길이 = %d, want 43 (256-bit base64url)", len(inv.Token))
	}
	// 기본 max_uses = max(1, min(남은 정원 8, 10)), 기본 만료 7일.
	if inv.MaxUses != 8 || inv.UsedCount != 0 || inv.Status != "active" {
		t.Fatalf("invite = %+v", inv)
	}
	exp, err := time.Parse(time.RFC3339, inv.ExpiresAt)
	if err != nil || time.Until(exp) < 6*24*time.Hour || time.Until(exp) > 7*24*time.Hour+time.Minute {
		t.Fatalf("expires_at = %q", inv.ExpiresAt)
	}

	// DB에는 SHA-256만 있고, 어떤 열·sync·job 어디에도 원문이 없다.
	var hash []byte
	var rowText, syncText string
	if err := s.pool.QueryRow(context.Background(), `SELECT token_hash, row_to_json(i)::text FROM party_invites i WHERE id = $1`, inv.ID).Scan(&hash, &rowText); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(inv.Token))
	if string(hash) != string(sum[:]) {
		t.Fatal("token_hash가 SHA-256(token)이 아니다")
	}
	if strings.Contains(rowText, inv.Token) {
		t.Fatal("party_invites row에 token 원문이 있다")
	}
	if err := s.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(payload::text, ''), '') FROM sync_changes WHERE recipient_user_id = $1`, owner.UserID).Scan(&syncText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(syncText, inv.Token) {
		t.Fatal("sync payload에 token 원문이 있다")
	}
	assertCount(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = 'party_invite'`, owner.UserID, 1)
	assertCount(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = 'party_invite'`, member.UserID, 0)

	// 재전송: 같은 초대의 메타데이터, token 원문은 없다.
	replay := s.createInvite(t, owner.AccessToken, partyID, "inv-1", "")
	if replay.Code != http.StatusCreated || replay.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("replay = %d/%q", replay.Code, replay.Header().Get(ReplayedHeader))
	}
	rp := decode[inviteEnvelope](t, replay).Invite
	if rp.ID != inv.ID || rp.Token != "" {
		t.Fatalf("replay invite = %+v", rp)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM party_invites WHERE party_id = $1`, partyID.String(), 1)

	// 목록에는 token이 없다. 멤버·외부인은 볼 수 없다.
	list := s.do(t, http.MethodGet, "/v1/parties/"+partyID.String()+"/invites", owner.AccessToken, nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), inv.Token) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	if got := decode[invitesEnvelope](t, list).Invites; len(got) != 1 || got[0].ID != inv.ID {
		t.Fatalf("list invites = %+v", got)
	}
	assertError(t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String()+"/invites", member.AccessToken, nil), http.StatusForbidden, httpapi.CodeForbidden)
	assertError(t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String()+"/invites", outsider.AccessToken, nil), http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.createInvite(t, member.AccessToken, partyID, "m-inv", ""), http.StatusForbidden, httpapi.CodeForbidden)
	assertError(t, s.createInvite(t, outsider.AccessToken, partyID, "o-inv", ""), http.StatusNotFound, httpapi.CodeNotFound)

	// 요청 검증.
	assertError(t, s.createInvite(t, owner.AccessToken, partyID, "bad-1", `{"max_uses":0}`), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.createInvite(t, owner.AccessToken, partyID, "bad-2", `{"max_uses":11}`), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.createInvite(t, owner.AccessToken, partyID, "bad-3", `{"expires_in_seconds":60}`), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.createInvite(t, owner.AccessToken, partyID, "bad-4", `{"expires_in_seconds":2592001}`), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.createInvite(t, owner.AccessToken, partyID, "bad-5", `{"unknown":1}`), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	custom := s.mustInvite(t, owner.AccessToken, partyID, "custom", `{"max_uses":2,"expires_in_seconds":3600}`)
	if custom.MaxUses != 2 {
		t.Fatalf("custom = %+v", custom)
	}

	// 무효화: 방장만, sync tombstone, 이후 미리보기·수락 404.
	assertError(t, s.mut(t, http.MethodDelete, "/v1/parties/"+partyID.String()+"/invites/"+inv.ID.String(), member.AccessToken, "rv-m", "", ""), http.StatusForbidden, httpapi.CodeForbidden)
	rv := s.mut(t, http.MethodDelete, "/v1/parties/"+partyID.String()+"/invites/"+inv.ID.String(), owner.AccessToken, "rv-1", "", "")
	if rv.Code != http.StatusOK || decode[inviteEnvelope](t, rv).Invite.Status != "revoked" {
		t.Fatalf("revoke = %d %s", rv.Code, rv.Body.String())
	}
	assertCount(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = 'party_invite' AND operation = 'tombstone'`, owner.UserID, 1)
	assertError(t, s.do(t, http.MethodGet, "/v1/invites/"+inv.Token+"/preview", outsider.AccessToken, nil), http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.accept(t, outsider.AccessToken, inv.Token, "acc-revoked"), http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.mut(t, http.MethodDelete, "/v1/parties/"+partyID.String()+"/invites/"+uuid.NewString(), owner.AccessToken, "rv-none", "", ""), http.StatusNotFound, httpapi.CodeNotFound)

	// token은 로그에도 없다.
	for _, tok := range []string{inv.Token, custom.Token} {
		if strings.Contains(s.logs.String(), tok) {
			t.Fatal("로그에 token 원문이 있다")
		}
	}
}

func TestInviteActiveLimitAndHourlyRateLimit(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "invite."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "p-limit")

	for i := 0; i < 3; i++ {
		s.mustInvite(t, owner.AccessToken, partyID, "lim-"+strconv.Itoa(i), "")
	}
	assertError(t, s.createInvite(t, owner.AccessToken, partyID, "lim-3", ""), http.StatusConflict, codePartyInviteLimitReached)
	assertCount(t, s.pool, `SELECT count(*) FROM party_invites WHERE party_id = $1`, partyID.String(), 3)

	// 시간당 10회: 무효화된 초대도 생성 횟수에 든다.
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO party_invites (id, party_id, token_hash, status, revoked_at, expires_at, max_uses, created_by_membership_id)
		SELECT gen_random_uuid(), $1, sha256(gen_random_uuid()::text::bytea), 'revoked', now(), now() + interval '1 day', 1,
		       (SELECT id FROM party_memberships WHERE party_id = $1 AND role = 'owner')
		  FROM generate_series(1, 7)`, partyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE party_invites SET status = 'revoked', revoked_at = now() WHERE party_id = $1 AND status = 'active'`, partyID); err != nil {
		t.Fatal(err)
	}
	assertError(t, s.createInvite(t, owner.AccessToken, partyID, "rate-1", ""), http.StatusTooManyRequests, httpapi.CodeRateLimited)
}

func TestInviteAcceptFlow(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "invite."+uuid.NewString())
	joiner := s.signIn(t, "invite."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "p-flow")
	inv := s.mustInvite(t, owner.AccessToken, partyID, "flow-inv", "")

	// 미리보기: §7.2 필드만.
	pv := s.do(t, http.MethodGet, "/v1/invites/"+inv.Token+"/preview", joiner.AccessToken, nil)
	if pv.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", pv.Code, pv.Body.String())
	}
	preview := decode[previewEnvelope](t, pv).Preview
	if preview.PartyName != "Invite Party" || preview.MemberCount != 1 || preview.InviterDisplayName != "Party Tester" {
		t.Fatalf("preview = %+v", preview)
	}
	for _, forbidden := range []string{"memberships", "user_id", "schedule"} {
		if strings.Contains(pv.Body.String(), forbidden) {
			t.Fatalf("미리보기에 %q가 노출됐다", forbidden)
		}
	}
	assertCount(t, s.pool, `SELECT count(*) FROM party_memberships WHERE party_id = $1`, partyID.String(), 1)

	// 미로그인 수락은 401이고 멤버십이 생기지 않는다.
	assertError(t, s.accept(t, "", inv.Token, "anon"), http.StatusUnauthorized, httpapi.CodeInvalidSession)
	assertCount(t, s.pool, `SELECT count(*) FROM party_memberships WHERE party_id = $1`, partyID.String(), 1)

	ownerSyncBefore := countSync(t, s.pool, owner.UserID)
	rec := s.accept(t, joiner.AccessToken, inv.Token, "acc-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body.String())
	}
	got := decode[acceptEnvelope](t, rec)
	if got.Membership.Role != "member" || got.Membership.Status != "active" || got.Party.Version != 2 {
		t.Fatalf("accept = %+v", got)
	}
	if s.activeMembers(t, partyID) != 2 || usedCount(t, s.pool, inv.ID) != 1 {
		t.Fatal("멤버 수 또는 used_count가 맞지 않는다")
	}
	assertCount(t, s.pool, `SELECT count(*) FROM party_visibility_settings WHERE party_id = $1 AND visibility_level = 'busyOnly'`, partyID.String(), 2)
	var inviteRef uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `SELECT invite_id FROM party_memberships WHERE id = $1`, got.Membership.ID).Scan(&inviteRef); err != nil || inviteRef != inv.ID {
		t.Fatalf("invite_id = %v err=%v", inviteRef, err)
	}

	// sync: 신규 멤버는 party + 멤버십 2 + 본인 설정 1, 방장은 party + 멤버십 + 초대 upsert.
	assertCount(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1`, joiner.UserID, 4)
	if delta := countSync(t, s.pool, owner.UserID) - ownerSyncBefore; delta != 3 {
		t.Fatalf("방장 sync 증가 = %d, want 3", delta)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = 'party_invite'`, joiner.UserID, 0)

	// outbox: dedupe membership_id, payload에 token 없음.
	var payload string
	if err := s.pool.QueryRow(context.Background(), `SELECT payload::text FROM outbox_jobs WHERE type = 'notify_member_joined' AND dedupe_key = $1`, got.Membership.ID.String()).Scan(&payload); err != nil {
		t.Fatalf("outbox job이 없다: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM outbox_jobs WHERE dedupe_key = $1`, got.Membership.ID.String())
	})
	if strings.Contains(payload, inv.Token) {
		t.Fatal("outbox payload에 token이 있다")
	}

	// 같은 키 재전송은 저장된 결과, 다른 키로 다시 수락하면 409 already_member이고 used_count 불변.
	replay := s.accept(t, joiner.AccessToken, inv.Token, "acc-1")
	if replay.Code != http.StatusCreated || replay.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
	assertError(t, s.accept(t, joiner.AccessToken, inv.Token, "acc-2"), http.StatusConflict, codeAlreadyMember)
	assertError(t, s.accept(t, owner.AccessToken, inv.Token, "acc-owner"), http.StatusConflict, codeAlreadyMember)
	if usedCount(t, s.pool, inv.ID) != 1 {
		t.Fatal("already_member가 used_count를 올렸다")
	}

	// 수락한 사용자가 나가면 재전송이 과거 성공 payload를 돌려주지 않는다.
	removeMembership(t, s.pool, partyID, uuid.MustParse(joiner.UserID))
	assertError(t, s.accept(t, joiner.AccessToken, inv.Token, "acc-1"), http.StatusNotFound, httpapi.CodeNotFound)

	if strings.Contains(s.logs.String(), inv.Token) {
		t.Fatal("로그에 token 원문이 있다")
	}
}

func countSync(t *testing.T, pool *pgxpool.Pool, userID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInviteInvalidStatesShareOneNotFound(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "invite."+uuid.NewString())
	u1 := s.signIn(t, "invite."+uuid.NewString())
	guest := s.signIn(t, "invite."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "p-404")

	expired := s.mustInvite(t, owner.AccessToken, partyID, "exp", `{"expires_in_seconds":3600}`)
	// scheduler가 아직 돌지 않은 상태: status는 그대로 active이고 시각만 지났다.
	if _, err := s.pool.Exec(context.Background(), `UPDATE party_invites SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM party_invites WHERE id = $1`, expired.ID).Scan(&stored); err != nil || stored != "active" {
		t.Fatalf("stored status = %q err=%v", stored, err)
	}

	single := s.mustInvite(t, owner.AccessToken, partyID, "single", `{"max_uses":1}`)
	if rec := s.accept(t, u1.AccessToken, single.Token, "single-acc"); rec.Code != http.StatusCreated {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body.String())
	}
	var exhausted string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM party_invites WHERE id = $1`, single.ID).Scan(&exhausted); err != nil || exhausted != "exhausted" {
		t.Fatalf("status = %q err=%v", exhausted, err)
	}

	unknown := "A" + strings.Repeat("b", 42)
	bodies := map[string]string{}
	for name, tok := range map[string]string{"unknown": unknown, "expired": expired.Token, "exhausted": single.Token, "garbage": strings.Repeat("x", 129)} {
		pv := s.do(t, http.MethodGet, "/v1/invites/"+tok+"/preview", guest.AccessToken, nil)
		assertError(t, pv, http.StatusNotFound, httpapi.CodeNotFound)
		ac := s.accept(t, guest.AccessToken, tok, "acc-"+name)
		assertError(t, ac, http.StatusNotFound, httpapi.CodeNotFound)
		bodies[name] = decode[httpapi.ErrorResponse](t, ac).Message
	}
	for name, msg := range bodies {
		if msg != bodies["unknown"] {
			t.Fatalf("%s 오류 메시지가 미존재와 다르다: %q", name, msg)
		}
	}

	// 해산된 Party에 남은 exhausted·활성 초대 모두 404.
	live := s.mustInvite(t, owner.AccessToken, partyID, "live", "")
	// T7 step 6: 해산은 활성 초대를 revoked로 바꾼다. 지연 트리거가 이를 요구한다.
	if _, err := s.pool.Exec(context.Background(), `UPDATE party_invites SET status = 'revoked', revoked_at = now() WHERE party_id = $1 AND status = 'active'`, partyID); err != nil {
		t.Fatal(err)
	}
	disbandParty(t, s.pool, partyID)
	for name, tok := range map[string]string{"exhausted": single.Token, "live": live.Token} {
		assertError(t, s.do(t, http.MethodGet, "/v1/invites/"+tok+"/preview", guest.AccessToken, nil), http.StatusNotFound, httpapi.CodeNotFound)
		assertError(t, s.accept(t, guest.AccessToken, tok, "dis-"+name), http.StatusNotFound, httpapi.CodeNotFound)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM party_memberships WHERE party_id = $1 AND user_id = '`+guest.UserID+`'`, partyID.String(), 0)
}

func TestInvitePartyFull(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "invite."+uuid.NewString())
	joiner := s.signIn(t, "invite."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "p-full")

	for i := 0; i < 8; i++ {
		addMember(t, s.pool, partyID, insertUser(t, s.pool))
	}
	inv := s.mustInvite(t, owner.AccessToken, partyID, "full-inv", "")
	if inv.MaxUses != 1 {
		t.Fatalf("남은 정원 1일 때 max_uses = %d, want 1", inv.MaxUses)
	}
	addMember(t, s.pool, partyID, insertUser(t, s.pool))
	assertError(t, s.accept(t, joiner.AccessToken, inv.Token, "full-acc"), http.StatusConflict, codePartyFull)
	if usedCount(t, s.pool, inv.ID) != 0 || s.activeMembers(t, partyID) != 10 {
		t.Fatal("정원 초과 수락이 상태를 바꿨다")
	}

	// 정원이 찬 Party에서는 초대 자체가 만들어지지 않는다.
	before := 0
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM party_invites WHERE party_id = $1`, partyID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	assertError(t, s.createInvite(t, owner.AccessToken, partyID, "full-inv-2", ""), http.StatusConflict, codePartyFull)
	assertCount(t, s.pool, `SELECT count(*) FROM party_invites WHERE party_id = $1`, partyID.String(), before)
	assertCount(t, s.pool, `SELECT count(*) FROM party_invites WHERE party_id = $1 AND max_uses < 1`, partyID.String(), 0)

	// 강퇴로 자리가 비면 다음 수락이 성공한다.
	var victim uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `SELECT user_id FROM party_memberships WHERE party_id = $1 AND role = 'member' LIMIT 1`, partyID).Scan(&victim); err != nil {
		t.Fatal(err)
	}
	removeMembership(t, s.pool, partyID, victim)
	if rec := s.accept(t, joiner.AccessToken, inv.Token, "full-acc-2"); rec.Code != http.StatusCreated {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body.String())
	}
}

func TestInviteJoinedPartyLimit(t *testing.T) {
	s := newStack(t)
	host := s.signIn(t, "invite."+uuid.NewString())
	filler := s.signIn(t, "invite."+uuid.NewString())
	joiner := s.signIn(t, "invite."+uuid.NewString())
	joinerID := uuid.MustParse(joiner.UserID)

	for i := 0; i < 10; i++ {
		s.createParty(t, joiner.AccessToken, "own-"+strconv.Itoa(i))
		addMember(t, s.pool, s.createParty(t, filler.AccessToken, "fill-"+strconv.Itoa(i)), joinerID)
	}
	partyID := s.createParty(t, host.AccessToken, "p-joined")
	inv := s.mustInvite(t, host.AccessToken, partyID, "joined-inv", "")
	assertError(t, s.accept(t, joiner.AccessToken, inv.Token, "joined-acc"), http.StatusConflict, codePartyJoinedLimitReached)
	assertCount(t, s.pool, `SELECT used_count FROM party_invites WHERE id = $1`, inv.ID.String(), 0)
}

func TestConcurrentAcceptLastSeatWithoutRetries(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "invite."+uuid.NewString())
	a := s.signIn(t, "invite."+uuid.NewString())
	b := s.signIn(t, "invite."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "p-race")
	for i := 0; i < 8; i++ {
		addMember(t, s.pool, partyID, insertUser(t, s.pool))
	}
	inv := s.mustInvite(t, owner.AccessToken, partyID, "race-inv", `{"max_uses":5}`)

	before := mutation.Retries()
	start := make(chan struct{})
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	for i, sess := range []sessionBody{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recs[i] = s.accept(t, sess.AccessToken, inv.Token, "race-"+strconv.Itoa(i))
		}()
	}
	close(start)
	wg.Wait()

	created, full := 0, 0
	for _, rec := range recs {
		switch rec.Code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			if decode[httpapi.ErrorResponse](t, rec).Code == codePartyFull {
				full++
			}
		}
	}
	if created != 1 || full != 1 {
		t.Fatalf("동시 수락 created=%d full=%d, want 1/1 (%s | %s)", created, full, recs[0].Body.String(), recs[1].Body.String())
	}
	if s.activeMembers(t, partyID) != 10 || usedCount(t, s.pool, inv.ID) != 1 {
		t.Fatalf("members=%d used=%d", s.activeMembers(t, partyID), usedCount(t, s.pool, inv.ID))
	}
	if after := mutation.Retries(); after != before {
		t.Fatalf("mutation.Retries %d -> %d", before, after)
	}
}
