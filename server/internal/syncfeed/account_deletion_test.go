package syncfeed

// 계획 P8(설계 4 §5.8): 계정 삭제가 Party 강제 위임·해산·멤버십 종료와 개인 데이터 삭제까지 처리하는지, 그리고
// 삭제 직전에 인증된 요청이 정리 뒤에 데이터를 되살리지 못하는지 본다. 삭제 worker는 운영과 같이 worker 역할로 돈다.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"plantogether/server/internal/account"
	"plantogether/server/internal/platform/jobs"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/postgres/pgtest"
)

const prefBody = `{"time_zone":"Asia/Seoul","weekly_intervals":[{"weekday":1,"start_minute":540,"end_minute":1080}]}`

// requestDeletion은 DELETE /v1/me다. 삭제 worker는 돌리지 않는다.
func (s *partyStack) requestDeletion(t *testing.T, token string) {
	t.Helper()
	me := s.req(t, http.MethodGet, "/v1/me", token, "", "", "")
	if rec := s.req(t, http.MethodDelete, "/v1/me", token, uuid.NewString(), etagOf(me), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("삭제 요청 = %d: %s", rec.Code, rec.Body.String())
	}
}

// runDeletion은 삭제 job 하나를 worker 역할로 끝까지 처리한다.
func (s *partyStack) runDeletion(t *testing.T, userID uuid.UUID) {
	t.Helper()
	svc, err := account.NewDeletionService(account.DeletionDeps{Pool: pgtest.Pool(t, pgtest.WorkerRoleURLEnv), Opener: s.box, Apple: noRevoke{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleJob(context.Background(), jobs.Job{Type: account.AccountDeletionJobType,
		Payload: []byte(`{"user_id":"` + userID.String() + `"}`), Attempt: 1}); err != nil {
		t.Fatalf("삭제 job: %v", err)
	}
}

func (s *partyStack) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (s *partyStack) membershipOf(t *testing.T, partyID, userID uuid.UUID) (id uuid.UUID, status, role string, endReason *string) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), `
		SELECT id, status, role, end_reason FROM party_memberships
		 WHERE party_id = $1 AND user_id = $2
		 ORDER BY joined_at DESC LIMIT 1`, partyID, userID).Scan(&id, &status, &role, &endReason); err != nil {
		t.Fatal(err)
	}
	return id, status, role, endReason
}

func (s *partyStack) assertEnded(t *testing.T, partyID, userID uuid.UUID, wantStatus, wantReason string) {
	t.Helper()
	_, status, _, reason := s.membershipOf(t, partyID, userID)
	if status != wantStatus || reason == nil || *reason != wantReason {
		t.Fatalf("멤버십 status=%s end_reason=%v, want %s/%s", status, reason, wantStatus, wantReason)
	}
}

// assertNoPersonalData는 삭제된 사용자의 Party 투영·공개 설정·캘린더·활동 시간이 남지 않았는지 본다(§5.8 4, §10 4).
func (s *partyStack) assertNoPersonalData(t *testing.T, userID uuid.UUID) {
	t.Helper()
	for _, q := range []string{
		`SELECT count(*) FROM party_schedule_projections WHERE owner_user_id = $1`,
		`SELECT count(*) FROM party_visibility_settings WHERE user_id = $1`,
		`SELECT count(*) FROM party_memberships WHERE user_id = $1 AND status = 'active'`,
		`SELECT count(*) FROM calendar_connections WHERE user_id = $1`,
		`SELECT count(*) FROM calendar_busy_facts WHERE user_id = $1`,
		`SELECT count(*) FROM availability_preferences WHERE user_id = $1`,
		`SELECT count(*) FROM availability_weekly_intervals WHERE user_id = $1`,
	} {
		if n := s.count(t, q, userID); n != 0 {
			t.Fatalf("%s => %d, want 0", q, n)
		}
	}
	var status string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id = $1`, userID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" {
		t.Fatalf("user status = %s, want deleted", status)
	}
}

// 일반 멤버의 계정 삭제: 멤버십은 left/account_deleted로 끝나고, 같은 Party 멤버는 그 멤버십과 projection의
// tombstone을 받아 bootstrap과 같아진다. 활동 시간과 캘린더 데이터는 지워진다.
func TestAccountDeletionEndsMemberMembershipAndPersonalData(t *testing.T) {
	s := newPartyStack(t)
	retriesBefore := mutation.Retries()
	a, d := s.newUser(t, "p8m.a"), s.newUser(t, "p8m.d")
	s.seedReadyCalendar(t, a.id)
	s.seedReadyCalendar(t, d.id)
	aTok := s.signIn(t, a.subject).token
	dTok := s.signIn(t, d.subject).token
	p := s.createParty(t, aTok, "M")
	_, inv := s.invite(t, aTok, p, "")
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", dTok, "", "")
	s.must(t, http.StatusOK, http.MethodPut, "/v1/availability/preferences/me", dTok, "", prefBody)

	ac := s.newClient(t, "A", aTok, 0)
	ac.expect(t, "before", p, map[string]int{"party": 1, "party_invite": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})
	dMembership, _, _, _ := s.membershipOf(t, p, d.id)

	s.requestDeletion(t, dTok)
	s.runDeletion(t, d.id)

	s.converge(t, "member deleted", ac)
	ac.expect(t, "member deleted", p, map[string]int{"party": 1, "party_invite": 1, "party_membership": 1, "party_visibility_setting": 1, "party_schedule_projection": 1})
	s.assertEnded(t, p, d.id, "left", "account_deleted")
	s.assertNoPersonalData(t, d.id)
	if n := s.count(t, `SELECT count(*) FROM outbox_jobs WHERE type = 'notify_member_left' AND dedupe_key = $1`, dMembership.String()); n != 1 {
		t.Fatalf("notify_member_left job %d개, want 1", n)
	}
	// 재시도해도 이미 끝난 멤버십을 다시 건드리지 않는다.
	s.runDeletion(t, d.id)
	s.assertEnded(t, p, d.id, "left", "account_deleted")
	if d := mutation.Retries() - retriesBefore; d != 0 {
		t.Fatalf("mutation 재시도 %d회", d)
	}
}

// 방장 계정 삭제: 삭제 진행 중인 멤버를 건너뛴 가장 먼저 가입한 활성 멤버가 방장이 되고 Party는 방장 1명으로
// 유지된다(계획 P8 완료 기준). 활성 초대는 무효화된다.
func TestAccountDeletionTransfersOwnershipToEarliestActiveMember(t *testing.T) {
	s := newPartyStack(t)
	d, e, b, c := s.newUser(t, "p8o.d"), s.newUser(t, "p8o.e"), s.newUser(t, "p8o.b"), s.newUser(t, "p8o.c")
	for _, u := range []user{d, e, b, c} {
		s.seedReadyCalendar(t, u.id)
	}
	dTok := s.signIn(t, d.subject).token
	eTok := s.signIn(t, e.subject).token
	bTok := s.signIn(t, b.subject).token
	cTok := s.signIn(t, c.subject).token
	p := s.createParty(t, dTok, "O")
	_, inv := s.invite(t, dTok, p, `{"max_uses":5}`)
	// 가입 순서: E(가장 먼저, 곧 삭제 요청) → B → C.
	for _, tok := range []string{eTok, bTok, cTok} {
		s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", tok, "", "")
	}
	s.requestDeletion(t, eTok)

	bc := s.newClient(t, "B", bTok, 0)
	cc := s.newClient(t, "C", cTok, 0)
	s.requestDeletion(t, dTok)
	s.runDeletion(t, d.id)

	s.converge(t, "owner deleted", bc, cc)
	// D의 멤버십·projection이 빠지고 E(삭제 요청만 됨)는 아직 멤버다.
	want := map[string]int{"party": 1, "party_membership": 3, "party_visibility_setting": 1, "party_schedule_projection": 3}
	bc.expect(t, "owner deleted", p, want)
	cc.expect(t, "owner deleted", p, want)

	bMembership, _, bRole, _ := s.membershipOf(t, p, b.id)
	var partyStatus string
	var owner uuid.UUID
	var version int64
	if err := s.pool.QueryRow(context.Background(), `SELECT status, owner_membership_id, version FROM parties WHERE id = $1`, p).Scan(&partyStatus, &owner, &version); err != nil {
		t.Fatal(err)
	}
	if partyStatus != "active" || owner != bMembership || bRole != "owner" {
		t.Fatalf("Party status=%s owner=%s, B 멤버십 %s role=%s", partyStatus, owner, bMembership, bRole)
	}
	if n := s.count(t, `SELECT count(*) FROM party_memberships WHERE party_id = $1 AND status = 'active' AND role = 'owner'`, p); n != 1 {
		t.Fatalf("활성 방장 %d명, want 1", n)
	}
	if n := s.count(t, `SELECT count(*) FROM party_invites WHERE party_id = $1 AND status = 'active'`, p); n != 0 {
		t.Fatalf("활성 초대 %d개, want 0", n)
	}
	if n := s.count(t, `SELECT count(*) FROM outbox_jobs WHERE type = 'notify_owner_transferred_by_deletion' AND payload->>'party_id' = $1 AND payload->>'membership_id' = $2`,
		p.String(), bMembership.String()); n != 1 {
		t.Fatalf("notify_owner_transferred_by_deletion job %d개, want 1", n)
	}
	if got := bc.model[entityKey{"party_membership", bMembership}].payload["role"]; got != "owner" {
		t.Fatalf("B 모델의 B role = %v", got)
	}
	s.assertEnded(t, p, d.id, "left", "account_deleted")
	s.assertNoPersonalData(t, d.id)

	// 이제 E의 삭제가 돈다. E는 일반 멤버라 종료만 되고 B가 계속 방장이다.
	s.runDeletion(t, e.id)
	s.converge(t, "member E deleted", bc, cc)
	bc.expect(t, "member E deleted", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})
	s.assertEnded(t, p, e.id, "left", "account_deleted")
	if n := s.count(t, `SELECT count(*) FROM party_memberships WHERE party_id = $1 AND status = 'active' AND role = 'owner'`, p); n != 1 {
		t.Fatalf("E 삭제 뒤 활성 방장 %d명, want 1", n)
	}
}

// 방장 계정 삭제인데 승계자가 없으면 해산한다. 단독 방장인 Party와, 남은 멤버가 모두 삭제 진행 중인 Party가
// 여기에 해당한다. 해산이 멤버십을 removed/party_disbanded로 끝내므로 뒤이은 그 멤버의 삭제는 아무것도 바꾸지 않는다.
func TestAccountDeletionDisbandsWhenNoActiveSuccessor(t *testing.T) {
	s := newPartyStack(t)
	e, f := s.newUser(t, "p8d.e"), s.newUser(t, "p8d.f")
	s.seedReadyCalendar(t, e.id)
	s.seedReadyCalendar(t, f.id)
	eTok := s.signIn(t, e.subject).token
	fTok := s.signIn(t, f.subject).token
	p := s.createParty(t, eTok, "D")
	solo := s.createParty(t, eTok, "S")
	_, inv := s.invite(t, eTok, p, "")
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", fTok, "", "")
	s.requestDeletion(t, fTok)

	s.requestDeletion(t, eTok)
	s.runDeletion(t, e.id)

	for _, id := range []uuid.UUID{p, solo} {
		var status string
		if err := s.pool.QueryRow(context.Background(), `SELECT status FROM parties WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "disbanded" {
			t.Fatalf("Party %s status = %s, want disbanded", id, status)
		}
		s.assertEnded(t, id, e.id, "removed", "party_disbanded")
		if n := s.count(t, `SELECT count(*) FROM party_schedule_projections WHERE party_id = $1`, id); n != 0 {
			t.Fatalf("해산 Party projection %d개", n)
		}
	}
	s.assertEnded(t, p, f.id, "removed", "party_disbanded")
	if n := s.count(t, `SELECT count(*) FROM party_invites WHERE party_id = $1 AND status = 'active'`, p); n != 0 {
		t.Fatalf("활성 초대 %d개", n)
	}
	s.assertNoPersonalData(t, e.id)

	s.runDeletion(t, f.id)
	s.assertEnded(t, p, f.id, "removed", "party_disbanded")
	s.assertNoPersonalData(t, f.id)
}

// 삭제 직전에 인증을 통과한 요청이 삭제 요청 커밋 뒤에 트랜잭션을 시작하면 423이고 아무것도 쓰지 않는다.
// 인증은 트랜잭션 밖에서 계정 상태를 보므로 mutation helper가 users row를 잡고 다시 확인해야 한다.
//
// 요청을 기기 row 잠금으로 idempotency INSERT에서 멈춘 뒤 계정 상태를 바꾼다. DELETE /v1/me는 같은 기기 row를
// 수정하므로 이 순간에는 쓸 수 없어 상태만 직접 바꾼다(삭제 요청 트랜잭션의 결과와 같다).
func TestRequestAuthenticatedBeforeDeletionCannotWriteAfterIt(t *testing.T) {
	cases := []struct {
		name, method, path, body, table string
	}{
		{"availability", http.MethodPut, "/v1/availability/preferences/me", prefBody, "availability_preferences"},
		{"calendar", http.MethodPut, "/v1/calendar/connection", `{"status":"selecting","time_zone":"Asia/Seoul"}`, "calendar_connections"},
		{"party", http.MethodPost, "/v1/parties", `{"name":"R"}`, "party_memberships"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newPartyStack(t)
			retriesBefore := mutation.Retries()
			u := s.newUser(t, "p8r."+tc.name)
			tok, deviceID := s.signInDevice(t, u.subject)

			b := s.blockDevice(t, deviceID)
			done := s.async(t, tc.method, tc.path, tok, "", tc.body)
			b.waitBlocked(t)
			if _, err := s.pool.Exec(context.Background(), `
				UPDATE users SET status = 'deletion_requested', deletion_requested_at = now(), version = version + 1
				 WHERE id = $1`, u.id); err != nil {
				t.Fatal(err)
			}
			b.release(t)
			rec := <-done
			if rec.Code != http.StatusLocked {
				t.Fatalf("삭제 뒤 커밋하려던 요청 = %d, want 423: %s", rec.Code, rec.Body.String())
			}
			if n := s.count(t, `SELECT count(*) FROM `+tc.table+` WHERE user_id = $1`, u.id); n != 0 {
				t.Fatalf("%s에 %d행이 쓰였다", tc.table, n)
			}
			if d := mutation.Retries() - retriesBefore; d != 0 {
				t.Fatalf("mutation 재시도 %d회", d)
			}
		})
	}
}
