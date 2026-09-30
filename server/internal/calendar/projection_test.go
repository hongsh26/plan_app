package calendar

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/mutation"
)

type partyRef struct {
	ID    uuid.UUID
	Token string
}

// newParty는 owner가 Party를 만들고 초대 token을 발급한다.
func (s *stack) newParty(t *testing.T, owner session) partyRef {
	t.Helper()
	rec := s.mut(t, http.MethodPost, "/v1/parties", owner.AccessToken, `{"name":"Cal Party"}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("Party 생성 = %d: %s", rec.Code, rec.Body.String())
	}
	var pe struct {
		Party struct {
			ID uuid.UUID `json:"id"`
		} `json:"party"`
	}
	pe2 := decode[struct {
		Party struct {
			ID uuid.UUID `json:"id"`
		} `json:"party"`
	}](t, rec)
	_ = pe
	rec = s.mut(t, http.MethodPost, "/v1/parties/"+pe2.Party.ID.String()+"/invites", owner.AccessToken, `{"max_uses":10}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("초대 생성 = %d: %s", rec.Code, rec.Body.String())
	}
	inv := decode[struct {
		Invite struct {
			Token string `json:"token"`
		} `json:"invite"`
	}](t, rec)
	return partyRef{ID: pe2.Party.ID, Token: inv.Invite.Token}
}

func (s *stack) join(t *testing.T, sess session, p partyRef) {
	t.Helper()
	if rec := s.mut(t, http.MethodPost, "/v1/invites/"+p.Token+"/accept", sess.AccessToken, "", ""); rec.Code != http.StatusCreated {
		t.Fatalf("가입 = %d: %s", rec.Code, rec.Body.String())
	}
}

func (s *stack) projections(t *testing.T, partyID uuid.UUID, ownerID string) int {
	return count(t, s.pool, `SELECT count(*) FROM party_schedule_projections WHERE party_id = $1 AND owner_user_id = $2`, partyID, ownerID)
}

func (s *stack) changes(t *testing.T, recipient, op string) int {
	return count(t, s.pool, `
		SELECT count(*) FROM sync_changes
		 WHERE recipient_user_id = $1 AND entity_type = 'party_schedule_projection' AND operation = $2`, recipient, op)
}

func (s *stack) setLevel(t *testing.T, partyID uuid.UUID, userID, level string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `
		UPDATE party_visibility_settings
		   SET visibility_level = $3, share_location = false, version = version + 1
		 WHERE party_id = $1 AND user_id = $2`, partyID, userID, level); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotBuildsAndDiffsBusyProjections(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t, "cal.p."+uuid.NewString())
	bob := s.signIn(t, "cal.p."+uuid.NewString())
	s.enableCalendar(t, alice)
	p := s.newParty(t, alice)
	s.join(t, bob, p)

	s.sync(t, alice, 1, []fact{mkFact(1, 1, 10), mkFact(2, 2, 10), mkFact(3, 3, 10)}, 10)
	if n := s.projections(t, p.ID, alice.UserID); n != 3 {
		t.Fatalf("projection = %d, want 3", n)
	}
	// 공개 수준 busyOnly: 상세 ciphertext가 없고 수준이 busyOnly다.
	if n := count(t, s.pool, `
		SELECT count(*) FROM party_schedule_projections
		 WHERE party_id = $1 AND (visibility_level <> 'busyOnly' OR title_ciphertext IS NOT NULL OR location_ciphertext IS NOT NULL)`, p.ID); n != 0 {
		t.Fatalf("busyOnly 위반 row = %d", n)
	}
	// 활성 멤버 전원(본인 포함)이 upsert를 받는다. payload에는 시간과 종일·시간대뿐이다.
	for _, u := range []string{alice.UserID, bob.UserID} {
		if n := s.changes(t, u, "upsert"); n != 3 {
			t.Fatalf("%s의 projection upsert = %d, want 3", u, n)
		}
	}
	var payload string
	if err := s.pool.QueryRow(context.Background(), `
		SELECT payload::text FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = 'party_schedule_projection' LIMIT 1`, bob.UserID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal([]byte(payload), &keys); err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		switch k {
		case "party_id", "owner_user_id", "visibility_level", "start_at", "end_at", "all_day", "time_zone":
		default:
			t.Fatalf("projection payload에 허용되지 않은 필드 %q가 있다", k)
		}
	}

	versionOf := func(n byte) int64 {
		var v int64
		if err := s.pool.QueryRow(context.Background(),
			`SELECT version FROM party_schedule_projections WHERE party_id = $1 AND owner_user_id = $2 AND source_event_key = decode($3, 'hex')`,
			p.ID, alice.UserID, strings.Repeat(hexByte(n), 16)).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// 2번째 snapshot: 1 그대로, 2 이동, 3 삭제, 4 신규 → upsert 2(이동+신규), tombstone 1.
	s.sync(t, alice, 2, []fact{mkFact(1, 1, 10), mkFact(2, 2, 15), mkFact(4, 4, 10)}, 10)
	if n := s.projections(t, p.ID, alice.UserID); n != 3 {
		t.Fatalf("projection = %d, want 3", n)
	}
	if versionOf(1) != 1 || versionOf(2) != 2 || versionOf(4) != 1 {
		t.Fatalf("version 1=%d 2=%d 4=%d", versionOf(1), versionOf(2), versionOf(4))
	}
	if n := s.changes(t, bob.UserID, "upsert"); n != 5 {
		t.Fatalf("bob upsert = %d, want 5 (3 + 이동 1 + 신규 1)", n)
	}
	if n := s.changes(t, bob.UserID, "tombstone"); n != 1 {
		t.Fatalf("bob tombstone = %d, want 1", n)
	}
	// 같은 snapshot을 다시 올려도(새 revision) 바뀐 것이 없으면 새 sync 변경이 없다.
	s.sync(t, alice, 3, []fact{mkFact(1, 1, 10), mkFact(2, 2, 15), mkFact(4, 4, 10)}, 10)
	if n := s.changes(t, bob.UserID, "upsert"); n != 5 {
		t.Fatalf("변경 없는 snapshot이 sync 변경을 만들었다: %d", n)
	}
}

func hexByte(n byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[n>>4], digits[n&0xf]})
}

func TestHiddenLevelRemovesProjectionsButKeepsFactsAndDetailsDowngrades(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t, "cal.h."+uuid.NewString())
	bob := s.signIn(t, "cal.h."+uuid.NewString())
	s.enableCalendar(t, alice)
	p := s.newParty(t, alice)
	s.join(t, bob, p)
	facts := []fact{mkFact(1, 1, 10), mkFact(2, 2, 10)}
	s.sync(t, alice, 1, facts, 10)

	s.setLevel(t, p.ID, alice.UserID, "hidden")
	s.sync(t, alice, 2, facts, 10)
	if n := s.projections(t, p.ID, alice.UserID); n != 0 {
		t.Fatalf("hidden인데 projection = %d", n)
	}
	if n := s.changes(t, bob.UserID, "tombstone"); n != 2 {
		t.Fatalf("hidden 하향 tombstone = %d, want 2", n)
	}
	// hidden이어도 private facts는 계산용으로 남는다.
	if n := factCount(t, s, alice.UserID); n != 2 {
		t.Fatalf("facts = %d", n)
	}

	// details는 아직 상세가 없으므로 busyOnly로 안전하게 강등된다.
	s.setLevel(t, p.ID, alice.UserID, "details")
	s.sync(t, alice, 3, facts, 10)
	if n := s.projections(t, p.ID, alice.UserID); n != 2 {
		t.Fatalf("projection = %d", n)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM party_schedule_projections WHERE party_id = $1 AND visibility_level = 'details'`, p.ID); n != 0 {
		t.Fatalf("details projection = %d", n)
	}
}

func TestRevokeHidesProjectionsImmediatelyAndReconnectRestoresThem(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t, "cal.r."+uuid.NewString())
	bob := s.signIn(t, "cal.r."+uuid.NewString())
	s.enableCalendar(t, alice)
	p := s.newParty(t, alice)
	s.join(t, bob, p)
	s.sync(t, alice, 1, []fact{mkFact(1, 1, 10), mkFact(2, 2, 10)}, 10)
	if s.projections(t, p.ID, alice.UserID) != 2 {
		t.Fatal("사전 조건: projection 2개")
	}

	c := s.connection(t, alice)
	rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", alice.AccessToken, `{"status":"revoked"}`, itoa(c.Version))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	if n := s.projections(t, p.ID, alice.UserID); n != 0 {
		t.Fatalf("철회 뒤 projection = %d, 즉시 비노출이어야 한다", n)
	}
	if n := s.changes(t, bob.UserID, "tombstone"); n != 2 {
		t.Fatalf("bob tombstone = %d", n)
	}
	// private facts는 7일 유예 동안 남지만(설계 3 §12) 계산에는 쓰이지 않는다(fresh 아님).
	if n := factCount(t, s, alice.UserID); n != 2 {
		t.Fatalf("facts = %d", n)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM calendar_connections WHERE user_id = $1 AND revoked_at IS NOT NULL`, alice.UserID); n != 1 {
		t.Fatal("revoked_at이 기록되지 않았다")
	}
	fresh, err := FreshUsers(context.Background(), s.pool, []uuid.UUID{uuid.MustParse(alice.UserID)}, time.Now())
	if err != nil || fresh[uuid.MustParse(alice.UserID)] {
		t.Fatalf("철회된 연결이 fresh다: %v %v", fresh, err)
	}

	// 재연결: selecting → snapshot → projection이 돌아온다.
	c = s.connection(t, alice)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", alice.AccessToken, `{"status":"selecting"}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	s.sync(t, alice, 2, []fact{mkFact(1, 1, 10), mkFact(2, 2, 10)}, 10)
	if n := s.projections(t, p.ID, alice.UserID); n != 2 {
		t.Fatalf("재연결 뒤 projection = %d", n)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM calendar_connections WHERE user_id = $1 AND revoked_at IS NOT NULL`, alice.UserID); n != 0 {
		t.Fatal("재연결 뒤 revoked_at이 남았다")
	}

	// 연결 해제는 facts도 지운다.
	c = s.connection(t, alice)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", alice.AccessToken, `{"status":"disconnected"}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if factCount(t, s, alice.UserID) != 0 || s.projections(t, p.ID, alice.UserID) != 0 {
		t.Fatal("연결 해제 뒤 facts/projection이 남았다")
	}
}

func TestPartyCreateAndJoinUseActiveGenerationFacts(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t, "cal.j."+uuid.NewString())
	bob := s.signIn(t, "cal.j."+uuid.NewString())
	carol := s.signIn(t, "cal.j."+uuid.NewString()) // 캘린더를 연결하지 않은 사용자
	s.enableCalendar(t, alice)
	s.enableCalendar(t, bob)
	s.sync(t, alice, 1, []fact{mkFact(1, 1, 10), mkFact(2, 2, 10)}, 10)
	s.sync(t, bob, 1, []fact{mkFact(5, 3, 10)}, 10)

	// Party를 만드는 순간 방장의 기존 facts가 projection이 된다(설계 4 §5.5.1 T1).
	p := s.newParty(t, alice)
	if n := s.projections(t, p.ID, alice.UserID); n != 2 {
		t.Fatalf("생성 직후 projection = %d, want 2", n)
	}
	// 가입하면 신규 멤버의 projection이 생기고, 기존 멤버는 그것을, 신규 멤버는 다른 멤버의 것을 받는다.
	aliceBefore := s.changes(t, alice.UserID, "upsert")
	s.join(t, bob, p)
	if n := s.projections(t, p.ID, bob.UserID); n != 1 {
		t.Fatalf("가입 직후 bob projection = %d", n)
	}
	if n := s.changes(t, alice.UserID, "upsert") - aliceBefore; n != 1 {
		t.Fatalf("alice가 받은 신규 멤버 projection upsert = %d, want 1", n)
	}
	// bob은 본인 1개 + alice의 2개를 받는다.
	if n := s.changes(t, bob.UserID, "upsert"); n != 3 {
		t.Fatalf("bob이 받은 projection upsert = %d, want 3", n)
	}
	// 연결이 없는 사용자는 projection 없이도 가입할 수 있다.
	s.join(t, carol, p)
	if n := s.projections(t, p.ID, carol.UserID); n != 0 {
		t.Fatalf("캘린더가 없는 carol projection = %d", n)
	}
	if n := s.changes(t, carol.UserID, "upsert"); n != 3 {
		t.Fatalf("carol이 받은 projection upsert = %d, want 3 (alice 2 + bob 1)", n)
	}
	// carol이 나중에 연결하면 snapshot 완료가 projection을 만든다.
	s.enableCalendar(t, carol)
	s.sync(t, carol, 1, []fact{mkFact(7, 5, 10)}, 10)
	if n := s.projections(t, p.ID, carol.UserID); n != 1 {
		t.Fatalf("연결 뒤 carol projection = %d", n)
	}
}

func TestFreshUsers(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal.f."+uuid.NewString())
	b := s.signIn(t, "cal.f."+uuid.NewString())
	aID, bID := uuid.MustParse(a.UserID), uuid.MustParse(b.UserID)
	s.enableCalendar(t, a)
	ctx := context.Background()

	fresh, err := FreshUsers(ctx, s.pool, []uuid.UUID{aID, bID}, time.Now())
	if err != nil || len(fresh) != 0 {
		t.Fatalf("snapshot 전: %v %v", fresh, err)
	}
	s.sync(t, a, 1, []fact{mkFact(1, 1, 10)}, 10)
	if fresh, _ = FreshUsers(ctx, s.pool, []uuid.UUID{aID, bID}, time.Now()); !fresh[aID] || fresh[bID] {
		t.Fatalf("완료 뒤: %v", fresh)
	}
	// 6시간을 넘기면 stale이다. 연결 응답의 유효 상태도 stale이 된다.
	if fresh, _ = FreshUsers(ctx, s.pool, []uuid.UUID{aID}, time.Now().Add(FreshnessWindow+time.Minute)); fresh[aID] {
		t.Fatal("6시간 뒤에도 fresh다")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE calendar_connections SET last_completed_sync_at = now() - interval '7 hours' WHERE user_id = $1`, aID); err != nil {
		t.Fatal(err)
	}
	if c := s.connection(t, a); c.Status != "stale" {
		t.Fatalf("status = %s, want stale", c.Status)
	}
	if fresh, _ = FreshUsers(ctx, s.pool, []uuid.UUID{aID}, time.Now()); fresh[aID] {
		t.Fatal("stale인데 fresh다")
	}
}

func TestPruneRemovesOldSessionsButRevisionStaysMonotonic(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal.pr."+uuid.NewString())
	s.enableCalendar(t, a)
	done := s.sync(t, a, 5, []fact{mkFact(1, 1, 10)}, 10)
	open := decode[snapEnv](t, s.createSnapshot(t, a, 6, 1)).Snapshot.ID
	if rec := s.putPage(t, a, open, 0, []fact{mkFact(2, 2, 10)}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE calendar_snapshot_sessions SET created_at = now() - interval '25 hours' WHERE id = ANY($1)`, []uuid.UUID{done.ID, open}); err != nil {
		t.Fatal(err)
	}
	n, err := Prune(ctx, s.pool, time.Now().Add(-StagingRetention), 10)
	if err != nil || n != 2 {
		t.Fatalf("삭제 수 = %d, err = %v", n, err)
	}
	if count(t, s.pool, `SELECT count(*) FROM calendar_snapshot_facts_staging WHERE session_id = $1`, open) != 0 ||
		count(t, s.pool, `SELECT count(*) FROM calendar_snapshot_pages WHERE session_id = $1`, open) != 0 {
		t.Fatal("staging/page가 cascade로 지워지지 않았다")
	}
	// 완료된 facts와 연결은 그대로다.
	if factCount(t, s, a.UserID) != 1 {
		t.Fatal("prune이 활성 facts를 건드렸다")
	}
	// session row가 없어져도 지난 revision은 되살아나지 않는다.
	assertConflict := s.createSnapshot(t, a, 6, 1)
	if assertConflict.Code != http.StatusConflict {
		t.Fatalf("지난 revision 재사용 = %d", assertConflict.Code)
	}
	if rec := s.createSnapshot(t, a, 7, 1); rec.Code != http.StatusCreated {
		t.Fatalf("다음 revision = %d %s", rec.Code, rec.Body.String())
	}
}

// 인수 조건: snapshot 완료와 초대 수락이 동시에 와도 교착하지 않고, 끝난 뒤 projection이 활성
// facts와 정확히 같다.
func TestConcurrentCompleteAndJoinLeaveConsistentProjections(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t, "cal.c."+uuid.NewString())
	s.enableCalendar(t, alice)
	p := s.newParty(t, alice)
	s.sync(t, alice, 1, []fact{mkFact(1, 1, 10)}, 10)
	before := mutation.Retries()

	const rounds = 6
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		joiner := s.signIn(t, "cal.c.j."+uuid.NewString())
		facts := []fact{mkFact(1, 1, 10), mkFact(byte(10+i), 2+i, 11)}
		if i%2 == 1 {
			facts = facts[:1]
		}
		start := make(chan struct{})
		var acceptCode int
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			acceptCode = s.mut(t, http.MethodPost, "/v1/invites/"+p.Token+"/accept", joiner.AccessToken, "", "").Code
		}()
		go func(rev int64) {
			defer wg.Done()
			<-start
			s.sync(t, alice, rev, facts, 10)
		}(int64(i + 2))
		close(start)
		wg.Wait()
		if acceptCode != http.StatusCreated {
			t.Fatalf("round %d 가입 = %d", i, acceptCode)
		}
	}
	if got := mutation.Retries() - before; got != 0 {
		t.Fatalf("교착 재시도 증가 = %d", got)
	}
	// projection의 키 집합이 활성 facts와 정확히 같다.
	if n := count(t, s.pool, `
		SELECT count(*) FROM (
		  (SELECT source_event_key FROM party_schedule_projections WHERE party_id = $1 AND owner_user_id = $2
		   EXCEPT SELECT source_event_key FROM calendar_busy_facts WHERE user_id = $2)
		  UNION ALL
		  (SELECT source_event_key FROM calendar_busy_facts WHERE user_id = $2
		   EXCEPT SELECT source_event_key FROM party_schedule_projections WHERE party_id = $1 AND owner_user_id = $2)
		) d`, p.ID, alice.UserID); n != 0 {
		t.Fatalf("projection과 facts가 어긋났다: %d", n)
	}
}

// 개인정보: 로그에 source_event_key, 시간 구간, 시간대 값이 남지 않는다.
func TestLogsNeverContainFactData(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal.log."+uuid.NewString())
	s.enableCalendar(t, a)
	f := mkFact(42, 3, 14)
	s.sync(t, a, 1, []fact{f}, 10)
	// 오류 경로도 포함한다.
	s.putPage(t, a, decode[snapEnv](t, s.createSnapshot(t, a, 2, 1)).Snapshot.ID, 0, []fact{{Key: f.Key, Start: f.Start, End: f.Start, TimeZone: "Mars/Base", Availability: "busy"}})
	logs := s.logs.String()
	for _, secret := range []string{f.Key, f.Start, f.End, "Asia/Seoul", "Mars/Base", "source_event_key"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("로그에 %q가 있다", secret)
		}
	}
}

// projectionSetFromFeed는 사용자의 sync 피드를 순서대로 적용해 얻은 projection ID 집합이다.
// 클라이언트가 피드만으로 재구성하는 상태와 같다.
func projectionSetFromFeed(t *testing.T, s *stack, userID string) map[uuid.UUID]bool {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `
		SELECT entity_id, operation FROM sync_changes
		 WHERE recipient_user_id = $1 AND entity_type = 'party_schedule_projection'
		 ORDER BY txid, ordinal`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	set := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		var op string
		if err := rows.Scan(&id, &op); err != nil {
			t.Fatal(err)
		}
		if op == "upsert" {
			set[id] = true
		} else {
			delete(set, id)
		}
	}
	return set
}

func projectionIDs(t *testing.T, s *stack, partyID uuid.UUID) map[uuid.UUID]bool {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `SELECT id FROM party_schedule_projections WHERE party_id = $1`, partyID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	set := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		set[id] = true
	}
	return set
}

func sameSet(a, b map[uuid.UUID]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// 철회·거부 뒤 늦게 도착한 complete가 연결을 ready로 되살려 projection을 다시 노출하면 안 된다
// (설계 3 §12).
func TestLateCompleteAfterRevokeCannotReExposeProjections(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t, "cal.late."+uuid.NewString())
	bob := s.signIn(t, "cal.late."+uuid.NewString())
	s.enableCalendar(t, alice)
	p := s.newParty(t, alice)
	s.join(t, bob, p)
	s.sync(t, alice, 1, []fact{mkFact(1, 1, 10)}, 10)

	// 업로드 도중(열린 session) 사용자가 권한을 철회한다.
	id := decode[snapEnv](t, s.createSnapshot(t, alice, 2, 1)).Snapshot.ID
	if rec := s.putPage(t, alice, id, 0, []fact{mkFact(1, 1, 10), mkFact(2, 2, 10)}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	c := s.connection(t, alice)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", alice.AccessToken, `{"status":"revoked"}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	if s.projections(t, p.ID, alice.UserID) != 0 {
		t.Fatal("철회 뒤 projection이 남았다")
	}
	// PUT이 열린 session을 폐기했으므로 늦은 page·complete는 거부된다.
	assertError(t, s.complete(t, alice, id, 2), http.StatusConflict, codeSnapshotState)
	if st := s.connection(t, alice).Status; st != "revoked" || s.projections(t, p.ID, alice.UserID) != 0 {
		t.Fatalf("상태 = %s, projection = %d", st, s.projections(t, p.ID, alice.UserID))
	}

	// 마지막 방어선: session이 열린 채 상태만 바뀐 경우(다른 경로)에도 complete는 연결을 되살리지 못한다.
	c = s.connection(t, alice)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", alice.AccessToken, `{"status":"selecting"}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	open := decode[snapEnv](t, s.createSnapshot(t, alice, 3, 0)).Snapshot.ID
	if _, err := s.pool.Exec(context.Background(), `UPDATE calendar_connections SET source_status = 'denied' WHERE user_id = $1`, alice.UserID); err != nil {
		t.Fatal(err)
	}
	assertError(t, s.complete(t, alice, open, 0), http.StatusConflict, codeStateInvalid)
	if st := s.connection(t, alice).Status; st != "denied" || s.projections(t, p.ID, alice.UserID) != 0 {
		t.Fatalf("complete가 연결을 되살렸다: %s", st)
	}
}

// 연결을 끊었다가 다시 연결해 snapshot을 만들고 abort해도 실패하지 않는다. 끊은 연결은 완료 sync가
// 없으므로 ready로 돌아가면 안 되고 selecting이어야 한다.
func TestAbortAfterDisconnectAndReconnectReturnsToSelecting(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal.ab."+uuid.NewString())
	s.enableCalendar(t, a)
	s.sync(t, a, 1, []fact{mkFact(1, 1, 10)}, 10)

	c := s.connection(t, a)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"disconnected"}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	c = s.connection(t, a)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"selecting","sources":[{"key":"cal-1","enabled":true}],"claim_sync_device":true}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	id := decode[snapEnv](t, s.createSnapshot(t, a, 2, 1)).Snapshot.ID
	rec := s.mut(t, http.MethodPost, "/v1/calendar/snapshots/"+id.String()+"/abort", a.AccessToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("abort = %d %s", rec.Code, rec.Body.String())
	}
	if st := s.connection(t, a).Status; st != "selecting" {
		t.Fatalf("status = %s, want selecting", st)
	}
}

// 인수 조건: 같은 사용자의 complete가 그 사용자의 Party 생성·가입, 그리고 다른 사용자의 가입과
// 동시에 와도 교착하지 않고, 가입자의 sync 피드만으로 재구성한 projection이 최종 상태와 같다.
func TestCompleteRacingWithJoinAndCreateKeepsFeedsConsistent(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t, "cal.rc."+uuid.NewString())
	bob := s.signIn(t, "cal.rc."+uuid.NewString()) // 연결이 있는 사용자: T3의 connection FOR SHARE와 부딪힌다
	s.enableCalendar(t, alice)
	s.enableCalendar(t, bob)
	p := s.newParty(t, alice)
	s.sync(t, alice, 1, []fact{mkFact(1, 1, 10)}, 10)
	s.sync(t, bob, 1, []fact{mkFact(50, 1, 12)}, 10)
	before := mutation.Retries()

	var joiners []session
	var extra []partyRef
	const rounds = 6
	for i := 0; i < rounds; i++ {
		joiner := s.signIn(t, "cal.rc.j."+uuid.NewString())
		joiners = append(joiners, joiner)
		facts := []fact{mkFact(1, 1, 10), mkFact(byte(10+i), 2+i, 11)}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var acceptCode, createCode, bobJoinCode int
		wg.Add(4)
		go func() { // 다른 사용자의 가입
			defer wg.Done()
			<-start
			acceptCode = s.mut(t, http.MethodPost, "/v1/invites/"+p.Token+"/accept", joiner.AccessToken, "", "").Code
		}()
		go func(rev int64) { // 같은 사용자의 complete
			defer wg.Done()
			<-start
			s.sync(t, alice, rev, facts, 10)
		}(int64(i + 2))
		go func() { // 같은 사용자가 새 Party를 만든다(T1이 connection FOR SHARE를 잡는다)
			defer wg.Done()
			<-start
			rec := s.mut(t, http.MethodPost, "/v1/parties", alice.AccessToken, `{"name":"Extra"}`, "")
			createCode = rec.Code
		}()
		go func(rev int64) { // 연결이 있는 사용자 bob의 complete와 가입
			defer wg.Done()
			<-start
			s.sync(t, bob, rev, []fact{mkFact(50, 1, 12), mkFact(byte(60+i), 3, 9)}, 10)
			bobJoinCode = s.mut(t, http.MethodPost, "/v1/invites/"+p.Token+"/accept", bob.AccessToken, "", "").Code
		}(int64(i + 2))
		close(start)
		wg.Wait()
		if acceptCode != http.StatusCreated || createCode != http.StatusCreated {
			t.Fatalf("round %d: 가입 %d, 생성 %d", i, acceptCode, createCode)
		}
		if i == 0 && bobJoinCode != http.StatusCreated {
			t.Fatalf("bob 가입 = %d", bobJoinCode)
		}
	}
	_ = extra
	if got := mutation.Retries() - before; got != 0 {
		t.Fatalf("교착 재시도 증가 = %d", got)
	}

	// 어떤 순서로 겹쳤든 각 가입자의 피드로 재구성한 projection이 Party의 최종 projection과 같다.
	final := projectionIDs(t, s, p.ID)
	if len(final) == 0 {
		t.Fatal("사전 조건: projection이 있어야 한다")
	}
	for i, j := range joiners {
		if feed := projectionSetFromFeed(t, s, j.UserID); !sameSet(feed, final) {
			t.Fatalf("가입자 %d의 피드 재구성(%d개)이 최종 projection(%d개)과 다르다", i, len(feed), len(final))
		}
	}
	// alice의 다른 Party들도 facts와 정확히 일치한다.
	if n := count(t, s.pool, `
		SELECT count(*) FROM parties pa
		  JOIN party_memberships m ON m.party_id = pa.id AND m.user_id = $1 AND m.role = 'owner'
		 WHERE pa.id <> $2
		   AND (SELECT count(*) FROM party_schedule_projections pr WHERE pr.party_id = pa.id AND pr.owner_user_id = $1)
		       <> (SELECT count(*) FROM calendar_busy_facts WHERE user_id = $1)`, alice.UserID, p.ID); n != 0 {
		t.Fatalf("facts와 어긋난 Party = %d", n)
	}
}

// 많은 fact도 배치 발행으로 처리한다. 3명 x 1500개 = 4500개의 sync 변경을 한 번에 만든다.
func TestLargeSnapshotEmitsBatchedChanges(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t, "cal.big."+uuid.NewString())
	bob := s.signIn(t, "cal.big."+uuid.NewString())
	carol := s.signIn(t, "cal.big."+uuid.NewString())
	s.enableCalendar(t, alice)
	p := s.newParty(t, alice)
	s.join(t, bob, p)
	s.join(t, carol, p)

	facts := make([]fact, 0, 1500)
	for i := 0; i < 1500; i++ {
		f := mkFact(1, i%25, i%20)
		f.Key = base64Key(i)
		facts = append(facts, f)
	}
	begin := time.Now()
	s.sync(t, alice, 1, facts, maxFactsPerPage)
	if d := time.Since(begin); d > 30*time.Second {
		t.Fatalf("1500 facts snapshot이 %v 걸렸다", d)
	}
	if n := s.projections(t, p.ID, alice.UserID); n != 1500 {
		t.Fatalf("projection = %d", n)
	}
	for _, u := range []string{alice.UserID, bob.UserID, carol.UserID} {
		if n := s.changes(t, u, "upsert"); n != 1500 {
			t.Fatalf("%s upsert = %d, want 1500", u, n)
		}
	}
	// ordinal이 겹치지 않고(PK) 피드로 재구성한 집합이 같다.
	if !sameSet(projectionSetFromFeed(t, s, bob.UserID), projectionIDs(t, s, p.ID)) {
		t.Fatal("bob의 피드 재구성이 최종 projection과 다르다")
	}
}
