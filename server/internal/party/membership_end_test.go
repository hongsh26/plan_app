package party

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
)

func (s *stack) leave(t *testing.T, token string, partyID uuid.UUID, key string) *httptest.ResponseRecorder {
	t.Helper()
	return s.mut(t, http.MethodDelete, "/v1/parties/"+partyID.String()+"/memberships/me", token, key, "", "")
}

func (s *stack) kick(t *testing.T, token string, partyID uuid.UUID, key string, version int64, userID string) *httptest.ResponseRecorder {
	t.Helper()
	return s.mut(t, http.MethodDelete, "/v1/parties/"+partyID.String()+"/memberships/"+userID, token, key, `"`+strconv.FormatInt(version, 10)+`"`, "")
}

func (s *stack) disband(t *testing.T, token string, partyID uuid.UUID, key string, version int64) *httptest.ResponseRecorder {
	t.Helper()
	return s.mut(t, http.MethodPost, "/v1/parties/"+partyID.String()+"/disband", token, key, `"`+strconv.FormatInt(version, 10)+`"`, "")
}

// seedReadyCalendar는 사용자에게 ready 연결과 Busy fact 하나를 만든다. 이후 Party 생성·가입이
// 실제 경로(§5.5.1)로 busyOnly projection을 만든다.
func seedReadyCalendar(t *testing.T, s *stack, userID string) {
	t.Helper()
	ctx := context.Background()
	connID := uuid.New()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO calendar_connections (id, user_id, source_status, active_generation, last_completed_sync_at, time_zone)
		VALUES ($1, $2, 'ready', 1, now(), 'Asia/Seoul')`, connID, userID); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO calendar_busy_facts
		(connection_id, source_event_key, user_id, generation, start_at, end_at, all_day, time_zone, availability)
		VALUES ($1, $2, $3, 1, $4, $5, false, 'Asia/Seoul', 'busy')`,
		connID, []byte(uuid.NewString()), userID, start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func (s *stack) join(t *testing.T, ownerToken string, partyID uuid.UUID, memberToken, key string) {
	t.Helper()
	inv := s.mustInvite(t, ownerToken, partyID, key+"-invite", "")
	if rec := s.accept(t, memberToken, inv.Token, key+"-accept"); rec.Code != http.StatusCreated {
		t.Fatalf("가입 = %d: %s", rec.Code, rec.Body.String())
	}
}

func projectionCount(t *testing.T, s *stack, partyID uuid.UUID, ownerUserID string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM party_schedule_projections WHERE party_id = $1 AND owner_user_id = $2`, partyID, ownerUserID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func projectionIDOf(t *testing.T, s *stack, partyID uuid.UUID, ownerUserID string) (uuid.UUID, int64) {
	t.Helper()
	var id uuid.UUID
	var v int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id, version FROM party_schedule_projections WHERE party_id = $1 AND owner_user_id = $2`, partyID, ownerUserID).Scan(&id, &v); err != nil {
		t.Fatal(err)
	}
	return id, v
}

// tombstoneVersion은 recipient가 받은 entity tombstone의 version이다. version이 없으면 0이다.
func tombstoneVersion(t *testing.T, s *stack, recipient string, entityID uuid.UUID) int64 {
	t.Helper()
	var v *int64
	if err := s.pool.QueryRow(context.Background(), `
		SELECT entity_version FROM sync_changes
		 WHERE recipient_user_id = $1 AND entity_id = $2 AND operation = 'tombstone'`, recipient, entityID).Scan(&v); err != nil {
		t.Fatalf("%s의 %s tombstone이 없다: %v", recipient, entityID, err)
	}
	if v == nil {
		return 0
	}
	return *v
}

func membershipIDOf(t *testing.T, s *stack, partyID uuid.UUID, userID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `
		SELECT id FROM party_memberships WHERE party_id = $1 AND user_id = $2
		 ORDER BY joined_at DESC LIMIT 1`, partyID, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLeaveCleansProjectionsAndSyncsBothSides(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "leave.owner."+uuid.NewString())
	member := s.signIn(t, "leave.member."+uuid.NewString())
	seedReadyCalendar(t, s, owner.UserID)
	seedReadyCalendar(t, s, member.UserID)
	partyID := s.createParty(t, owner.AccessToken, "leave-create")
	s.join(t, owner.AccessToken, partyID, member.AccessToken, "leave")
	memberProjection, memberProjectionVersion := projectionIDOf(t, s, partyID, member.UserID)
	ownerProjection, _ := projectionIDOf(t, s, partyID, owner.UserID)
	ownerMembership := membershipIDOf(t, s, partyID, owner.UserID)
	memberMembership := membershipIDOf(t, s, partyID, member.UserID)
	before := partyVersion(t, s, partyID)

	rec := s.leave(t, member.AccessToken, partyID, "leave-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("탈퇴 = %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[membershipEnvelope](t, rec).Membership
	if got.ID != memberMembership || got.Status != "left" {
		t.Fatalf("응답 멤버십 = %+v", got)
	}
	var endReason string
	if err := s.pool.QueryRow(context.Background(), `SELECT end_reason FROM party_memberships WHERE id = $1`, memberMembership).Scan(&endReason); err != nil {
		t.Fatal(err)
	}
	if endReason != endReasonLeftVoluntarily {
		t.Fatalf("end_reason = %s", endReason)
	}

	// §10 탈퇴 인수 조건: projection·설정은 0건, 개인 Busy facts는 남는다.
	if n := projectionCount(t, s, partyID, member.UserID); n != 0 {
		t.Fatalf("탈퇴자 projection %d건", n)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM party_visibility_settings WHERE user_id = $1`, member.UserID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM calendar_busy_facts WHERE user_id = $1`, member.UserID, 1)
	if n := projectionCount(t, s, partyID, owner.UserID); n != 1 {
		t.Fatalf("남은 멤버 projection %d건", n)
	}
	if v := partyVersion(t, s, partyID); v != before+1 {
		t.Fatalf("party version = %d, want %d", v, before+1)
	}

	// 남은 멤버: 탈퇴자의 projection·멤버십 tombstone(version 있음)과 새 Party upsert.
	if v := tombstoneVersion(t, s, owner.UserID, memberProjection); v != memberProjectionVersion+1 {
		t.Fatalf("남은 멤버의 projection tombstone version = %d", v)
	}
	if v := tombstoneVersion(t, s, owner.UserID, memberMembership); v != got.Version {
		t.Fatalf("남은 멤버의 멤버십 tombstone version = %d, want %d", v, got.Version)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = '`+owner.UserID+`' AND entity_type = 'party' AND operation = 'upsert' AND entity_version = $1`, before+1, 1)

	// 탈퇴자: 본인 것은 version 있는 삭제, 서버에 남는 다른 멤버 entity는 version 없는 scope 정리.
	if v := tombstoneVersion(t, s, member.UserID, memberProjection); v != memberProjectionVersion+1 {
		t.Fatalf("탈퇴자의 본인 projection tombstone version = %d", v)
	}
	if v := tombstoneVersion(t, s, member.UserID, memberMembership); v != got.Version {
		t.Fatalf("탈퇴자의 본인 멤버십 tombstone version = %d", v)
	}
	for _, id := range []uuid.UUID{partyID, ownerMembership, ownerProjection} {
		if v := tombstoneVersion(t, s, member.UserID, id); v != 0 {
			t.Fatalf("scope 정리 tombstone %s에 version %d가 붙었다", id, v)
		}
	}
	if n := changeCount(t, s, member.UserID, entityVisibilitySetting, "tombstone", nil); n != 1 {
		t.Fatalf("탈퇴자 설정 tombstone %d건", n)
	}
	if n := changeCount(t, s, owner.UserID, entityVisibilitySetting, "tombstone", nil); n != 0 {
		t.Fatalf("다른 멤버가 탈퇴자 설정 변경을 받았다: %d", n)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM outbox_jobs WHERE type = 'notify_member_left' AND dedupe_key = $1`, memberMembership.String(), 1)

	// 탈퇴자에게 Party는 존재하지 않는다. bootstrap에도 남지 않는다.
	assertError(t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String(), member.AccessToken, nil), http.StatusNotFound, httpapi.CodeNotFound)
	boot, err := BootstrapProjections(context.Background(), s.pool, uuid.MustParse(member.UserID))
	if err != nil {
		t.Fatal(err)
	}
	if len(boot) != 0 {
		t.Fatalf("탈퇴자 bootstrap에 Party entity %d건", len(boot))
	}

	// 오프라인 큐 재전송(§9.4): 같은 키든 새 키든 200과 현재 상태이며 sync를 더 만들지 않는다.
	syncBefore := countSync(t, s.pool, member.UserID)
	for _, key := range []string{"leave-1", "leave-2"} {
		again := s.leave(t, member.AccessToken, partyID, key)
		if again.Code != http.StatusOK || decode[membershipEnvelope](t, again).Membership.Status != "left" {
			t.Fatalf("탈퇴 재전송(%s) = %d: %s", key, again.Code, again.Body.String())
		}
	}
	if n := countSync(t, s.pool, member.UserID); n != syncBefore {
		t.Fatalf("재전송이 sync를 %d건 더 만들었다", n-syncBefore)
	}
	outsider := s.signIn(t, "leave.outsider."+uuid.NewString())
	assertError(t, s.leave(t, outsider.AccessToken, partyID, "leave-outsider"), http.StatusNotFound, httpapi.CodeNotFound)
}

func TestOwnerLeaveRequiresTransferAndSoleOwnerLeaveDisbands(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "leave.solo."+uuid.NewString())
	member := s.signIn(t, "leave.solo.member."+uuid.NewString())
	seedReadyCalendar(t, s, owner.UserID)
	partyID := s.createParty(t, owner.AccessToken, "solo-create")
	s.join(t, owner.AccessToken, partyID, member.AccessToken, "solo")

	assertError(t, s.leave(t, owner.AccessToken, partyID, "solo-leave-blocked"), http.StatusConflict, codeOwnerTransferRequired)

	if rec := s.leave(t, member.AccessToken, partyID, "solo-member-leave"); rec.Code != http.StatusOK {
		t.Fatalf("멤버 탈퇴 = %d: %s", rec.Code, rec.Body.String())
	}
	inv := s.mustInvite(t, owner.AccessToken, partyID, "solo-open-invite", "")
	rec := s.leave(t, owner.AccessToken, partyID, "solo-leave")
	if rec.Code != http.StatusOK {
		t.Fatalf("단독 방장 탈퇴 = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode[membershipEnvelope](t, rec).Membership; got.Status != "left" {
		t.Fatalf("단독 방장 멤버십 = %+v", got)
	}
	var status, endReason string
	if err := s.pool.QueryRow(context.Background(), `
		SELECT p.status, m.end_reason FROM parties p JOIN party_memberships m ON m.party_id = p.id
		 WHERE p.id = $1 AND m.user_id = $2`, partyID, owner.UserID).Scan(&status, &endReason); err != nil {
		t.Fatal(err)
	}
	if status != "disbanded" || endReason != endReasonLeftVoluntarily {
		t.Fatalf("party=%s end_reason=%s", status, endReason)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM party_schedule_projections WHERE party_id = $1`, partyID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM party_visibility_settings WHERE party_id = $1`, partyID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM party_invites WHERE party_id = $1 AND status = 'active'`, partyID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM outbox_jobs WHERE type = 'notify_party_disbanded' AND payload->>'party_id' = $1`, partyID.String(), 0)
	if n := changeCount(t, s, owner.UserID, entityPartyInvite, "tombstone", inv.ID); n != 1 {
		t.Fatalf("방장 초대 tombstone %d건", n)
	}
	if v := tombstoneVersion(t, s, owner.UserID, partyID); v != partyVersion(t, s, partyID) {
		t.Fatalf("해산 Party tombstone version = %d", v)
	}

	if tv := tombstoneVersion(t, s, owner.UserID, membershipIDOf(t, s, partyID, owner.UserID)); tv == 0 {
		t.Fatal("단독 방장 멤버십 tombstone에 version이 없다")
	}
	if n := changeCount(t, s, owner.UserID, entityVisibilitySetting, "tombstone", nil); n != 1 {
		t.Fatalf("단독 방장 설정 tombstone %d건", n)
	}

	// 재전송은 해산된 Party라 410이다. 클라이언트는 탈퇴 완료로 처리한다.
	assertError(t, s.leave(t, owner.AccessToken, partyID, "solo-leave"), http.StatusGone, codePartyDisbanded)
}

func TestRemoveMemberByOwner(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "kick.owner."+uuid.NewString())
	member := s.signIn(t, "kick.member."+uuid.NewString())
	third := s.signIn(t, "kick.third."+uuid.NewString())
	seedReadyCalendar(t, s, member.UserID)
	partyID := s.createParty(t, owner.AccessToken, "kick-create")
	s.join(t, owner.AccessToken, partyID, member.AccessToken, "kick-m")
	s.join(t, owner.AccessToken, partyID, third.AccessToken, "kick-t")
	memberMembership := membershipIDOf(t, s, partyID, member.UserID)
	memberProjection, _ := projectionIDOf(t, s, partyID, member.UserID)
	v := partyVersion(t, s, partyID)

	assertError(t, s.kick(t, owner.AccessToken, partyID, "kick-self", v, owner.UserID), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.kick(t, third.AccessToken, partyID, "kick-forbidden", v, member.UserID), http.StatusForbidden, httpapi.CodeForbidden)
	if rec := s.kick(t, owner.AccessToken, partyID, "kick-stale", v-1, member.UserID); rec.Code != http.StatusConflict {
		t.Fatalf("낡은 version = %d: %s", rec.Code, rec.Body.String())
	}
	assertError(t, s.kick(t, owner.AccessToken, partyID, "kick-unknown", v, uuid.NewString()), http.StatusNotFound, httpapi.CodeNotFound)

	rec := s.kick(t, owner.AccessToken, partyID, "kick-1", v, member.UserID)
	if rec.Code != http.StatusOK {
		t.Fatalf("강퇴 = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode[membershipEnvelope](t, rec).Membership; got.ID != memberMembership || got.Status != "removed" {
		t.Fatalf("응답 = %+v", got)
	}
	if n := projectionCount(t, s, partyID, member.UserID); n != 0 {
		t.Fatalf("강퇴자 projection %d건", n)
	}
	for _, u := range []string{owner.UserID, third.UserID, member.UserID} {
		if n := changeCount(t, s, u, entityScheduleProjection, "tombstone", memberProjection); n != 1 {
			t.Fatalf("%s의 강퇴자 projection tombstone %d건", u, n)
		}
	}
	removedVersion := decode[membershipEnvelope](t, rec).Membership.Version
	if tv := tombstoneVersion(t, s, third.UserID, memberMembership); tv != removedVersion {
		t.Fatalf("남은 멤버의 강퇴자 멤버십 tombstone version = %d", tv)
	}
	for _, id := range []uuid.UUID{partyID, membershipIDOf(t, s, partyID, owner.UserID), membershipIDOf(t, s, partyID, third.UserID)} {
		if tv := tombstoneVersion(t, s, member.UserID, id); tv != 0 {
			t.Fatalf("강퇴자 scope 정리 tombstone %s에 version %d", id, tv)
		}
	}
	assertCount(t, s.pool, `SELECT count(*) FROM outbox_jobs WHERE type = 'notify_member_removed' AND dedupe_key = $1`, memberMembership.String(), 1)
	assertError(t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String(), member.AccessToken, nil), http.StatusNotFound, httpapi.CodeNotFound)
	// 강퇴된 사용자는 더 이상 공개 수준을 바꿀 수 없다.
	assertError(t, s.mut(t, http.MethodPatch, "/v1/parties/"+partyID.String()+"/visibility/me", member.AccessToken, "kick-vis", `"1"`,
		`{"visibility_level":"hidden","share_location":false}`), http.StatusNotFound, httpapi.CodeNotFound)

	// 재가입은 새 멤버십 row와 busyOnly 기본 설정으로 시작한다(§5.3).
	s.join(t, owner.AccessToken, partyID, member.AccessToken, "kick-rejoin")
	rejoined := membershipIDOf(t, s, partyID, member.UserID)
	if rejoined == memberMembership {
		t.Fatal("재가입이 기존 멤버십을 되살렸다")
	}
	setting := decode[visibilityEnvelope](t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String()+"/visibility/me", member.AccessToken, nil)).Visibility
	if setting.VisibilityLevel != visibilityBusyOnly || setting.Version != 1 {
		t.Fatalf("재가입 설정 = %+v", setting)
	}
	if n := projectionCount(t, s, partyID, member.UserID); n != 1 {
		t.Fatalf("재가입 projection %d건", n)
	}
}

func TestDisbandByOwner(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "disband.owner."+uuid.NewString())
	member := s.signIn(t, "disband.member."+uuid.NewString())
	outsider := s.signIn(t, "disband.outsider."+uuid.NewString())
	seedReadyCalendar(t, s, owner.UserID)
	seedReadyCalendar(t, s, member.UserID)
	partyID := s.createParty(t, owner.AccessToken, "disband-create")
	s.join(t, owner.AccessToken, partyID, member.AccessToken, "disband")
	inv := s.mustInvite(t, owner.AccessToken, partyID, "disband-open-invite", "")
	memberProjection, _ := projectionIDOf(t, s, partyID, member.UserID)
	v := partyVersion(t, s, partyID)

	assertError(t, s.disband(t, member.AccessToken, partyID, "disband-forbidden", v), http.StatusForbidden, httpapi.CodeForbidden)
	assertError(t, s.disband(t, outsider.AccessToken, partyID, "disband-outsider", v), http.StatusNotFound, httpapi.CodeNotFound)
	if rec := s.disband(t, owner.AccessToken, partyID, "disband-stale", v-1); rec.Code != http.StatusConflict {
		t.Fatalf("낡은 version = %d", rec.Code)
	}

	rec := s.disband(t, owner.AccessToken, partyID, "disband-1", v)
	if rec.Code != http.StatusOK {
		t.Fatalf("해산 = %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[partyEnvelope](t, rec).Party
	if got.Status != "disbanded" || got.Version != v+1 {
		t.Fatalf("응답 = %+v", got)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM party_memberships WHERE party_id = $1 AND status = 'active'`, partyID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM party_memberships WHERE party_id = $1 AND end_reason = 'party_disbanded'`, partyID, 2)
	assertCount(t, s.pool, `SELECT count(*) FROM party_schedule_projections WHERE party_id = $1`, partyID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM party_visibility_settings WHERE party_id = $1`, partyID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM outbox_jobs WHERE type = 'notify_party_disbanded' AND dedupe_key = $1`, partyID.String(), 1)
	for _, u := range []string{owner.UserID, member.UserID} {
		if tv := tombstoneVersion(t, s, u, partyID); tv != v+1 {
			t.Fatalf("%s의 Party tombstone version = %d", u, tv)
		}
		if n := changeCount(t, s, u, entityScheduleProjection, "tombstone", memberProjection); n != 1 {
			t.Fatalf("%s의 projection tombstone %d건", u, n)
		}
		if n := changeCount(t, s, u, entityPartyMembership, "tombstone", nil); n != 2 {
			t.Fatalf("%s의 멤버십 tombstone %d건, want 2", u, n)
		}
		if n := changeCount(t, s, u, entityVisibilitySetting, "tombstone", nil); n != 1 {
			t.Fatalf("%s의 본인 설정 tombstone %d건", u, n)
		}
	}
	if n := changeCount(t, s, owner.UserID, entityPartyInvite, "tombstone", inv.ID); n != 1 {
		t.Fatalf("방장 초대 tombstone %d건", n)
	}
	if n := changeCount(t, s, member.UserID, entityPartyInvite, "tombstone", nil); n != 0 {
		t.Fatalf("일반 멤버가 초대 tombstone을 받았다: %d", n)
	}

	// 과거 멤버의 mutation은 410, 한 번도 멤버가 아니었던 사용자는 404, 해산 Party 초대 수락은 404다.
	assertError(t, s.disband(t, owner.AccessToken, partyID, "disband-1", v), http.StatusGone, codePartyDisbanded)
	assertError(t, s.leave(t, member.AccessToken, partyID, "disband-member-leave"), http.StatusGone, codePartyDisbanded)
	assertError(t, s.mut(t, http.MethodPatch, "/v1/parties/"+partyID.String()+"/visibility/me", member.AccessToken, "disband-vis", `"1"`,
		`{"visibility_level":"hidden","share_location":false}`), http.StatusGone, codePartyDisbanded)
	assertError(t, s.leave(t, outsider.AccessToken, partyID, "disband-outsider-leave"), http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.accept(t, outsider.AccessToken, inv.Token, "disband-accept"), http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String(), owner.AccessToken, nil), http.StatusNotFound, httpapi.CodeNotFound)
}

// 설계 5 §13 "멤버십 종료와 공개 설정 변경 경쟁 후 projection과 설정 row가 0건"이다.
func TestLeaveRacingVisibilityChangeLeavesNoProjection(t *testing.T) {
	s := newStack(t)
	retriesBefore := mutation.Retries()
	for i := 0; i < 5; i++ {
		owner := s.signIn(t, "race.vis.owner."+uuid.NewString())
		member := s.signIn(t, "race.vis.member."+uuid.NewString())
		seedReadyCalendar(t, s, member.UserID)
		partyID := s.createParty(t, owner.AccessToken, "race-vis-create")
		s.join(t, owner.AccessToken, partyID, member.AccessToken, "race-vis")
		// hidden에서 busyOnly로 올리는 요청이 projection을 다시 만들려고 하는 경쟁을 만든다.
		path := "/v1/parties/" + partyID.String() + "/visibility/me"
		if rec := s.mut(t, http.MethodPatch, path, member.AccessToken, "race-vis-hide", `"1"`, `{"visibility_level":"hidden","share_location":false}`); rec.Code != http.StatusOK {
			t.Fatalf("hidden = %d", rec.Code)
		}

		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[0] = s.leave(t, member.AccessToken, partyID, "race-vis-leave").Code
		}()
		go func() {
			defer wg.Done()
			codes[1] = s.mut(t, http.MethodPatch, path, member.AccessToken, "race-vis-show", `"2"`, `{"visibility_level":"busyOnly","share_location":false}`).Code
		}()
		wg.Wait()
		if codes[0] != http.StatusOK {
			t.Fatalf("탈퇴 = %d", codes[0])
		}
		if codes[1] != http.StatusOK && codes[1] != http.StatusNotFound {
			t.Fatalf("설정 변경 = %d", codes[1])
		}
		if n := projectionCount(t, s, partyID, member.UserID); n != 0 {
			t.Fatalf("경쟁 후 projection %d건", n)
		}
		assertCount(t, s.pool, `SELECT count(*) FROM party_visibility_settings WHERE user_id = $1`, member.UserID, 0)
	}
	if mutation.Retries() != retriesBefore {
		t.Fatalf("잠금 규약 위반으로 재시도가 %d번 났다", mutation.Retries()-retriesBefore)
	}
}

// 설계 5 §7.3: 탈퇴가 commit된 뒤 snapshot 완료 rebuild가 projection을 되살리지 않는다.
// snapshot complete의 잠금 순서(LockUserPartiesShared -> RebuildBusyProjections)를 탈퇴와 동시에 돌린다.
// 실제 complete는 connection FOR UPDATE와 connectionLocked=true를 쓴다. 어느 순서든 결과가 0건이어야 하며,
// 잠금 충돌 자체는 TestLeaveWaitsFor*에서 결정적으로 확인한다.
func TestLeaveRacingSnapshotRebuildDoesNotResurrectProjection(t *testing.T) {
	s := newStack(t)
	retriesBefore := mutation.Retries()
	for i := 0; i < 5; i++ {
		owner := s.signIn(t, "race.snap.owner."+uuid.NewString())
		member := s.signIn(t, "race.snap.member."+uuid.NewString())
		seedReadyCalendar(t, s, member.UserID)
		partyID := s.createParty(t, owner.AccessToken, "race-snap-create")
		s.join(t, owner.AccessToken, partyID, member.AccessToken, "race-snap")
		memberID := uuid.MustParse(member.UserID)
		var deviceID uuid.UUID
		if err := s.pool.QueryRow(context.Background(), `SELECT id FROM devices WHERE user_id = $1 LIMIT 1`, memberID).Scan(&deviceID); err != nil {
			t.Fatal(err)
		}
		// rebuild가 diff를 만들도록 projection을 먼저 지워 둔다.
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM party_schedule_projections WHERE owner_user_id = $1`, memberID); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var leaveCode int
		var rebuildErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			leaveCode = s.leave(t, member.AccessToken, partyID, "race-snap-leave").Code
		}()
		go func() {
			defer wg.Done()
			_, rebuildErr = mutation.Run(context.Background(), s.pool, mutation.Request{
				UserID: memberID, DeviceID: deviceID, Key: "race-snap-rebuild-" + uuid.NewString(), Hash: []byte("rebuild"),
			}, time.Now(), func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
				if err := LockUserPartiesShared(ctx, tx, memberID); err != nil {
					return mutation.Result{}, err
				}
				if err := RebuildBusyProjections(ctx, tx, memberID, nil, false); err != nil {
					return mutation.Result{}, err
				}
				return mutation.Result{StatusCode: http.StatusOK, ResourceType: "test", ResourceID: memberID, ResourceVersion: 1}, nil
			})
		}()
		wg.Wait()
		if leaveCode != http.StatusOK || rebuildErr != nil {
			t.Fatalf("탈퇴 = %d, rebuild err = %v", leaveCode, rebuildErr)
		}
		if n := projectionCount(t, s, partyID, member.UserID); n != 0 {
			t.Fatalf("rebuild가 탈퇴자 projection %d건을 되살렸다", n)
		}
	}
	if mutation.Retries() != retriesBefore {
		t.Fatalf("잠금 규약 위반으로 재시도가 %d번 났다", mutation.Retries()-retriesBefore)
	}
}

func TestDisbandRacingAcceptDoesNotDeadlock(t *testing.T) {
	s := newStack(t)
	retriesBefore := mutation.Retries()
	for i := 0; i < 5; i++ {
		owner := s.signIn(t, "race.disband.owner."+uuid.NewString())
		joiner := s.signIn(t, "race.disband.joiner."+uuid.NewString())
		partyID := s.createParty(t, owner.AccessToken, "race-disband-create")
		inv := s.mustInvite(t, owner.AccessToken, partyID, "race-disband-invite", "")
		v := partyVersion(t, s, partyID)

		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[0] = s.disband(t, owner.AccessToken, partyID, "race-disband", v).Code
		}()
		go func() {
			defer wg.Done()
			codes[1] = s.accept(t, joiner.AccessToken, inv.Token, "race-disband-accept").Code
		}()
		wg.Wait()
		// 수락이 먼저면 Party version이 올라 해산은 409, 해산이 먼저면 수락은 404다. 어느 쪽도 500이 아니다.
		ok := (codes[0] == http.StatusOK && codes[1] == http.StatusNotFound) ||
			(codes[0] == http.StatusConflict && codes[1] == http.StatusCreated)
		if !ok {
			t.Fatalf("해산 = %d, 수락 = %d", codes[0], codes[1])
		}
		assertCount(t, s.pool, `SELECT count(*) FROM parties p WHERE p.id = $1 AND p.status = 'disbanded'
			AND EXISTS (SELECT 1 FROM party_memberships m WHERE m.party_id = p.id AND m.status = 'active')`, partyID, 0)
	}
	// mutation.Run은 교착(40P01)을 재시도로 숨기므로 재시도 0회까지 확인해야 "교착 없음"이다.
	if mutation.Retries() != retriesBefore {
		t.Fatalf("교착·직렬화 실패로 재시도가 %d번 났다", mutation.Retries()-retriesBefore)
	}
}

// waitForLockWait는 다른 세션이 행 잠금을 기다리는 상태가 될 때까지 기다린다.
func waitForLockWait(t *testing.T, s *stack) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := s.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND pid <> pg_backend_pid()
			   AND state = 'active' AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("탈퇴가 잠금을 기다리지 않았다")
}

// 결정적 경쟁 테스트: 먼저 잡힌 잠금을 탈퇴가 실제로 기다리는지 확인한다. 둘 중 하나가 회귀해도 잡는다.
//   - parties FOR SHARE: snapshot complete의 LockUserPartiesShared
//   - party_memberships FOR KEY SHARE: rebuild가 활성 멤버십을 읽을 때 거는 잠금. 일반 UPDATE(NO KEY UPDATE)는
//     이것과 충돌하지 않으므로 종료 경로의 명시적 FOR UPDATE가 필요하다.
func TestLeaveWaitsForSnapshotLocks(t *testing.T) {
	for _, tc := range []struct{ name, lockSQL string }{
		{"party_for_share", `SELECT 1 FROM parties WHERE id = $1 FOR SHARE`},
		{"membership_for_key_share", `SELECT 1 FROM party_memberships WHERE party_id = $1 AND user_id = $2 AND status = 'active' FOR KEY SHARE`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStack(t)
			owner := s.signIn(t, "lockwait.owner."+uuid.NewString())
			member := s.signIn(t, "lockwait.member."+uuid.NewString())
			partyID := s.createParty(t, owner.AccessToken, "lockwait-create")
			s.join(t, owner.AccessToken, partyID, member.AccessToken, "lockwait")

			ctx := context.Background()
			holder, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			args := []any{partyID}
			if tc.name == "membership_for_key_share" {
				args = append(args, member.UserID)
			}
			rows, err := holder.Query(ctx, tc.lockSQL, args...)
			if err != nil {
				t.Fatal(err)
			}
			rows.Close()

			done := make(chan int, 1)
			go func() { done <- s.leave(t, member.AccessToken, partyID, "lockwait-leave").Code }()
			waitForLockWait(t, s)
			select {
			case code := <-done:
				t.Fatalf("탈퇴가 잠금을 기다리지 않고 %d로 끝났다", code)
			default:
			}
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if code := <-done; code != http.StatusOK {
				t.Fatalf("잠금 해제 후 탈퇴 = %d", code)
			}
		})
	}
}
