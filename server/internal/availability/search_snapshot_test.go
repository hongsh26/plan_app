package availability

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/availability/engine"
)

// --- 검색 시나리오 헬퍼 (A3·A4 공용) ---

func (s *stack) post(t *testing.T, path, token, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

type searchParty struct {
	ID     uuid.UUID
	invite string
}

// newParty는 owner가 Party를 만들고 초대 token을 발급한다. Party row는 테스트 끝에 지운다.
func (s *stack) newParty(t *testing.T, owner session) searchParty {
	t.Helper()
	rec := s.post(t, "/v1/parties", owner.AccessToken, uuid.NewString(), `{"name":"Search Party"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Party 생성 = %d: %s", rec.Code, rec.Body.String())
	}
	id := decode[struct {
		Party struct {
			ID uuid.UUID `json:"id"`
		} `json:"party"`
	}](t, rec).Party.ID
	// 사용자 cleanup보다 늦게 등록하므로 먼저 실행된다(LIFO).
	t.Cleanup(func() { cleanupParty(t, s.pool, id) })
	rec = s.post(t, "/v1/parties/"+id.String()+"/invites", owner.AccessToken, uuid.NewString(), `{"max_uses":10}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("초대 생성 = %d: %s", rec.Code, rec.Body.String())
	}
	inv := decode[struct {
		Invite struct {
			Token string `json:"token"`
		} `json:"invite"`
	}](t, rec).Invite.Token
	return searchParty{ID: id, invite: inv}
}

func (s *stack) join(t *testing.T, sess session, p searchParty) {
	t.Helper()
	if rec := s.post(t, "/v1/invites/"+p.invite+"/accept", sess.AccessToken, uuid.NewString(), ""); rec.Code != http.StatusCreated {
		t.Fatalf("가입 = %d: %s", rec.Code, rec.Body.String())
	}
}

func (s *stack) leave(t *testing.T, sess session, p searchParty) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/parties/"+p.ID.String()+"/memberships/me", nil)
	req.Header.Set("Authorization", "Bearer "+sess.AccessToken)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("탈퇴 = %d: %s", rec.Code, rec.Body.String())
	}
}

func cleanupParty(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`DELETE FROM party_schedule_projections WHERE party_id = $1`,
		`DELETE FROM party_visibility_settings WHERE party_id = $1`,
		`UPDATE party_memberships SET invite_id = NULL WHERE party_id = $1`,
		`DELETE FROM party_invites WHERE party_id = $1`,
		`DELETE FROM party_memberships WHERE party_id = $1`,
		`DELETE FROM parties WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// allDay는 7일 모두 00:00~24:00 활동 시간이다.
const allDay = `[{"weekday":1,"start_minute":0,"end_minute":1440},{"weekday":2,"start_minute":0,"end_minute":1440},
{"weekday":3,"start_minute":0,"end_minute":1440},{"weekday":4,"start_minute":0,"end_minute":1440},
{"weekday":5,"start_minute":0,"end_minute":1440},{"weekday":6,"start_minute":0,"end_minute":1440},
{"weekday":7,"start_minute":0,"end_minute":1440}]`

// setPrefs는 활동 시간을 실제 API로 저장한다(create-only, 이미 있으면 현재 version으로 교체).
func (s *stack) setPrefs(t *testing.T, sess session, tz, intervals string) {
	t.Helper()
	ifMatch := ""
	if rec := s.get(t, prefPath, sess.AccessToken); rec.Header().Get("ETag") != "" {
		ifMatch = rec.Header().Get("ETag")
	}
	body := fmt.Sprintf(`{"time_zone":%q,"weekly_intervals":%s}`, tz, intervals)
	if rec := s.put(t, sess.AccessToken, uuid.NewString(), ifMatch, body); rec.Code != http.StatusOK {
		t.Fatalf("활동 시간 저장 = %d: %s", rec.Code, rec.Body.String())
	}
}

// 기본 window는 실제 시각 기준 어제부터 95일이다. 검색은 오늘+2일부터 하므로 lead time과 무관하다.
func defaultWindow() (time.Time, time.Time) {
	now := time.Now().UTC().Truncate(time.Hour)
	return now.Add(-24 * time.Hour), now.Add(95 * 24 * time.Hour)
}

// seedCalendar는 ready 연결, generation 1의 completed session(window), Busy facts를 직접 만든다.
// snapshot complete를 거친 상태와 같게 active_generation_usable도 true다. 상태 전이 자체를 보는 테스트는
// 실제 calendar API를 쓴다(search_calendar_test.go).
func (s *stack) seedCalendar(t *testing.T, userID string, lastSync, winStart, winEnd time.Time, busy ...engine.Interval) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	connID := uuid.New()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO calendar_connections (id, user_id, source_status, active_generation, active_generation_usable, last_completed_sync_at, time_zone, last_snapshot_revision)
		VALUES ($1, $2, 'ready', 1, true, $3, 'Asia/Seoul', 1)`, connID, userID, lastSync); err != nil {
		t.Fatal(err)
	}
	s.addGeneration(t, connID, userID, 1, winStart, winEnd, busy...)
	return connID
}

// addGeneration은 generation gen의 completed session과 facts를 만들고 연결의 active generation으로 올린다.
// 이전 generation의 facts는 남겨 둔다(활성 generation만 읽는지 확인하기 위해).
func (s *stack) addGeneration(t *testing.T, connID uuid.UUID, userID string, gen int64, winStart, winEnd time.Time, busy ...engine.Interval) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO calendar_snapshot_sessions
		       (id, connection_id, revision, window_start, window_end, expected_pages, status, fact_count, generation, completed_at)
		VALUES ($1, $2, $3, $4, $5, 0, 'completed', $6, $3, now())`, uuid.New(), connID, gen, winStart, winEnd, len(busy)); err != nil {
		t.Fatal(err)
	}
	for _, b := range busy {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO calendar_busy_facts
			       (connection_id, source_event_key, user_id, generation, start_at, end_at, all_day, time_zone, availability)
			VALUES ($1, $2, $3, $4, $5, $6, false, 'Asia/Seoul', 'busy')`,
			connID, []byte(uuid.NewString()), userID, gen, b.Start, b.End); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE calendar_connections SET active_generation = $2, active_generation_usable = true, last_snapshot_revision = $2 WHERE id = $1`, connID, gen); err != nil {
		t.Fatal(err)
	}
}

// readyMember는 로그인, 활동 시간(매일 종일), fresh 캘린더를 갖춘 사용자다.
func (s *stack) readyMember(t *testing.T, tz string, busy ...engine.Interval) session {
	t.Helper()
	sess := s.signIn(t)
	s.setPrefs(t, sess, tz, allDay)
	ws, we := defaultWindow()
	s.seedCalendar(t, sess.UserID, time.Now().Add(-time.Hour), ws, we, busy...)
	return sess
}

func testParams(t *testing.T, tz string) SearchParams {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	start := engine.DateOf(time.Now(), loc).AddDays(2)
	return SearchParams{StartDate: start, EndDate: start.AddDays(13), DurationMinutes: 60, TimeZone: tz, MaxResults: 20}
}

func envOf(t *testing.T, p SearchParams) engine.Interval {
	t.Helper()
	loc, err := time.LoadLocation(p.TimeZone)
	if err != nil {
		t.Fatal(err)
	}
	return engine.RequestEnvelope(p.StartDate, p.EndDate, loc)
}

// issue는 현재 snapshot으로 validUntil = now+5분인 version을 만든다.
func (s *stack) issue(t *testing.T, partyID uuid.UUID, caller session, p SearchParams) string {
	t.Helper()
	return s.issueUntil(t, partyID, caller, p, time.Now().Add(resultValidity))
}

func (s *stack) issueUntil(t *testing.T, partyID uuid.UUID, caller session, p SearchParams, until time.Time) string {
	t.Helper()
	snap, err := readSnapshotTx(context.Background(), s.pool, partyID, uuid.MustParse(caller.UserID), p, envOf(t, p), time.Now())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s.avail.versioner.token(snap.state.canonical, until)
}

func (s *stack) verify(t *testing.T, partyID uuid.UUID, caller session, p SearchParams, version string, now time.Time) error {
	t.Helper()
	return s.avail.versioner.VerifyCalculationVersion(context.Background(), s.pool, partyID, uuid.MustParse(caller.UserID), p, version, now)
}

// --- A3 테스트 ---

func TestSerializationRetryWrapper(t *testing.T) {
	serial := &pgconn.PgError{Code: "40001"}
	other := errors.New("다른 오류")
	cases := []struct {
		name    string
		results []error
		want    error
		calls   int
	}{
		{"첫 시도 성공", []error{nil}, nil, 1},
		{"한 번 실패 후 성공", []error{serial, nil}, nil, 2},
		{"두 번 실패", []error{serial, serial}, errChangedRetry, 2},
		{"다른 오류는 재시도하지 않음", []error{other}, other, 1},
		{"재시도 뒤 다른 오류", []error{serial, other}, other, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			err := withSerializationRetry(func() error {
				calls++
				return c.results[calls-1]
			})
			if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if calls != c.calls {
				t.Fatalf("calls = %d, want %d", calls, c.calls)
			}
		})
	}
}

func TestVersionerKeyDerivationAndTokenShape(t *testing.T) {
	if _, err := NewVersioner(nil); err == nil {
		t.Fatal("빈 secret을 받으면 안 된다")
	}
	a, _ := NewVersioner([]byte("secret-a-secret-a-secret-a-secret-a"))
	b, _ := NewVersioner([]byte("secret-b-secret-b-secret-b-secret-b"))
	canonical := []byte("tuple")
	until := time.Unix(1_900_000_000, 0)
	ta, tb := a.token(canonical, until), b.token(canonical, until)
	if ta == tb {
		t.Fatal("secret이 다르면 token이 달라야 한다")
	}
	if ta != a.token(canonical, until) {
		t.Fatal("같은 입력은 같은 token이어야 한다")
	}
	if ta == a.token([]byte("tuple2"), until) || ta == a.token(canonical, until.Add(time.Second)) {
		t.Fatal("tuple 또는 validUntil이 바뀌면 token이 바뀌어야 한다")
	}
	raw, err := base64.RawURLEncoding.DecodeString(ta)
	if err != nil || len(raw) != 41 || raw[0] != versionFormat {
		t.Fatalf("token 형식: len=%d err=%v", len(raw), err)
	}
}

func TestSnapshotVersionTupleAndVerify(t *testing.T) {
	s := newStack(t, false)
	a := s.readyMember(t, "Asia/Seoul")
	b := s.readyMember(t, "America/New_York")
	p := s.newParty(t, a)
	s.join(t, b, p)
	params := testParams(t, "Asia/Seoul")
	ctx := context.Background()

	v := s.issue(t, p.ID, a, params)
	if err := s.verify(t, p.ID, a, params, v, time.Now()); err != nil {
		t.Fatalf("변화 없음 = %v, want nil", err)
	}
	// 다른 활성 멤버도 같은 tuple로 검증된다(호출자는 tuple에 들어가지 않는다).
	if err := s.verify(t, p.ID, b, params, v, time.Now()); err != nil {
		t.Fatalf("다른 멤버 검증 = %v", err)
	}

	stale := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrResultStale) {
			t.Fatalf("%s: err = %v, want ErrResultStale", name, err)
		}
	}
	stale("validUntil 경과", s.verify(t, p.ID, a, params, v, time.Now().Add(resultValidity+time.Second)))
	other := params
	other.DurationMinutes = 90
	stale("파라미터 변경", s.verify(t, p.ID, a, other, v, time.Now()))
	stale("형식 오류", s.verify(t, p.ID, a, params, "not-a-version", time.Now()))
	tampered := []byte(v)
	tampered[len(tampered)-2] ^= 1
	stale("변조", s.verify(t, p.ID, a, params, string(tampered), time.Now()))

	// 활동 시간 변경 → stale
	s.setPrefs(t, b, "America/New_York", `[{"weekday":1,"start_minute":540,"end_minute":1320}]`)
	stale("활동 시간 변경", s.verify(t, p.ID, a, params, v, time.Now()))
	s.setPrefs(t, b, "America/New_York", allDay)
	v = s.issue(t, p.ID, a, params)

	// calendar generation 변경 → stale
	var connB uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT id FROM calendar_connections WHERE user_id = $1`, b.UserID).Scan(&connB); err != nil {
		t.Fatal(err)
	}
	ws, we := defaultWindow()
	s.addGeneration(t, connB, b.UserID, 2, ws, we)
	stale("generation 변경", s.verify(t, p.ID, a, params, v, time.Now()))
	v = s.issue(t, p.ID, a, params)

	// 가장 이른 마지막 sync 시각 변경(fresh_until = min + 6h) → stale
	if _, err := s.pool.Exec(ctx, `UPDATE calendar_connections SET last_completed_sync_at = now() - interval '2 hours' WHERE id = $1`, connB); err != nil {
		t.Fatal(err)
	}
	stale("fresh_until 변경", s.verify(t, p.ID, a, params, v, time.Now()))
	v = s.issue(t, p.ID, a, params)

	// Party version 변경 → stale
	if _, err := s.pool.Exec(ctx, `UPDATE parties SET version = version + 1 WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	stale("party version 변경", s.verify(t, p.ID, a, params, v, time.Now()))
	v = s.issue(t, p.ID, a, params)

	// 멤버십 변경(가입) → stale
	c := s.readyMember(t, "Europe/London")
	s.join(t, c, p)
	stale("멤버 가입", s.verify(t, p.ID, a, params, v, time.Now()))
	v = s.issue(t, p.ID, a, params)

	// 멤버십 변경(탈퇴) → stale, 떠난 멤버는 not found
	s.leave(t, c, p)
	stale("멤버 탈퇴", s.verify(t, p.ID, a, params, v, time.Now()))
	if err := s.verify(t, p.ID, c, params, v, time.Now()); !IsNotFound(err) {
		t.Fatalf("떠난 멤버 검증 = %v, want not found", err)
	}

	// 준비 미완료가 되면 validUntil 전이어도 stale이다. validUntil을 멀리 둔 token으로 gate만 확인한다.
	far := s.issueUntil(t, p.ID, a, params, time.Now().Add(24*time.Hour))
	if err := s.verify(t, p.ID, a, params, far, time.Now()); err != nil {
		t.Fatalf("far token = %v", err)
	}
	stale("calendar freshness 만료", s.verify(t, p.ID, a, params, far, time.Now().Add(6*time.Hour+time.Minute)))
}

func TestSnapshotReadsOnlyActiveGenerationFactsInEnvelope(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	env := envOf(t, params)
	inside := engine.Interval{Start: env.Start.Add(3 * time.Hour), End: env.Start.Add(4 * time.Hour)}
	outside := engine.Interval{Start: env.End.Add(time.Hour), End: env.End.Add(2 * time.Hour)}
	crossing := engine.Interval{Start: env.Start.Add(-time.Hour), End: env.Start.Add(time.Hour)}
	a := s.readyMember(t, "Asia/Seoul", inside, outside, crossing)
	p := s.newParty(t, a)

	snap, err := readSnapshotTx(context.Background(), s.pool, p.ID, uuid.MustParse(a.UserID), params, env, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snap.members[0].Busy); got != 2 {
		t.Fatalf("generation 1 facts = %d, want 2 (envelope 안 + 경계 횡단)", got)
	}

	var conn uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM calendar_connections WHERE user_id = $1`, a.UserID).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	ws, we := defaultWindow()
	newer := engine.Interval{Start: env.Start.Add(10 * time.Hour), End: env.Start.Add(11 * time.Hour)}
	s.addGeneration(t, conn, a.UserID, 2, ws, we, newer)
	snap, err = readSnapshotTx(context.Background(), s.pool, p.ID, uuid.MustParse(a.UserID), params, env, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.members[0].Busy; len(got) != 1 || !got[0].Start.Equal(newer.Start) {
		t.Fatalf("generation 2 facts = %v, want 새 generation 하나", got)
	}
}

func TestSnapshotNotFoundIsIndistinguishable(t *testing.T) {
	s := newStack(t, false)
	a := s.readyMember(t, "Asia/Seoul")
	outsider := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	params := testParams(t, "Asia/Seoul")
	for name, args := range map[string][2]uuid.UUID{
		"비멤버":      {p.ID, uuid.MustParse(outsider.UserID)},
		"없는 Party": {uuid.New(), uuid.MustParse(a.UserID)},
	} {
		_, err := readSnapshotTx(context.Background(), s.pool, args[0], args[1], params, envOf(t, params), time.Now())
		if !errors.Is(err, errSearchNotFound) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}
