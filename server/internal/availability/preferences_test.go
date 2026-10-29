package availability

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/availability/engine"
	"plantogether/server/internal/calendar"
	"plantogether/server/internal/party"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/postgres/pgtest"
	"plantogether/server/internal/platform/ratelimit"
	"plantogether/server/internal/platform/secretbox"
	"plantogether/server/internal/syncfeed"
)

const prefPath = "/v1/availability/preferences/me"

type subjectApple struct{}

func (subjectApple) Verify(_ context.Context, idToken, _ string) (appleid.Identity, error) {
	return appleid.Identity{Subject: idToken}, nil
}

type stack struct {
	handler http.Handler
	pool    *pgxpool.Pool
	logs    *bytes.Buffer
	avail   *Handler
	clock   *testClock
}

// testClock은 검색 handler의 시각이다. 기본은 실제 시각이고 shift로 미래로 옮긴다.
type testClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *testClock) shift(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// newStack은 인증·활동 시간·sync endpoint를 묶은 HTTP 스택이다. limited면 실제 요청 제한을 켠다.
func newStack(t *testing.T, limited bool) *stack {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	// 일부 테스트가 sync bootstrap·증분을 읽으므로 sync 정리 테스트와 겹치지 않게 한다(pgtest.SyncPruneLockKey).
	pgtest.HoldSyncReader(t, pool)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	tokens, err := auth.NewAccessTokens(bytes.Repeat([]byte{4}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(auth.Deps{Pool: pool, Apple: subjectApple{}, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.NewLocal("local", bytes.Repeat([]byte{5}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{}
	deps := Deps{Pool: pool, Logger: logger, Now: clock.now, VersionSecret: bytes.Repeat([]byte{7}, 32)}
	if limited {
		// 테스트마다 무작위 HMAC 키를 써서 다른 실행의 bucket과 섞이지 않는다.
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		limiter, err := ratelimit.New(pool, key, logger)
		if err != nil {
			t.Fatal(err)
		}
		deps.Limiter = limiter
	}
	authHandler := auth.NewHandler(svc, logger)
	mux := http.NewServeMux()
	authHandler.Register(mux)
	avail, err := NewHandler(deps)
	if err != nil {
		t.Fatal(err)
	}
	avail.Register(mux, authHandler.Require)
	party.NewHandler(party.Deps{Pool: pool, Logger: logger}).Register(mux, authHandler.Require)
	calendar.NewHandler(calendar.Deps{Pool: pool, Logger: logger, SecretBox: box}).Register(mux, authHandler.Require)
	syncfeed.NewHandler(syncfeed.Deps{Pool: pool, Logger: logger, RefKeyBox: box}).Register(mux, authHandler.Require)
	return &stack{handler: httpapi.Wrap(logger, mux), pool: pool, logs: &logs, avail: avail, clock: clock}
}

type session struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
}

func (s *stack) signIn(t *testing.T) session {
	t.Helper()
	b, _ := json.Marshal(map[string]string{
		"identity_token": "availability." + uuid.NewString(), "authorization_code": "c", "raw_nonce": "n", "display_name": "활동 시간 테스트",
	})
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/apple", bytes.NewReader(b)))
	if rec.Code != http.StatusOK {
		t.Fatalf("로그인 status = %d: %s", rec.Code, rec.Body.String())
	}
	sess := decode[session](t, rec)
	uid := uuid.MustParse(sess.UserID)
	t.Cleanup(func() { cleanupUser(t, s.pool, uid) })
	return sess
}

func cleanupUser(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	// 검색 테스트의 Party를 먼저 지운다. 멤버십 FK가 users 삭제를 막는다.
	rows, err := pool.Query(ctx, `SELECT DISTINCT party_id FROM party_memberships WHERE user_id = $1`, userID)
	if err != nil {
		t.Fatal(err)
	}
	partyIDs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range partyIDs {
		cleanupParty(t, pool, id)
	}
	for _, sql := range []string{
		`DELETE FROM sync_changes WHERE recipient_user_id = $1`,
		`DELETE FROM idempotency_keys WHERE user_id = $1`,
		`DELETE FROM audit_events WHERE actor_user_id = $1`,
		// availability_preferences·intervals는 users cascade로 함께 지워진다.
		`DELETE FROM users WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, userID); err != nil {
			t.Fatal(err)
		}
	}
}

func (s *stack) get(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func (s *stack) put(t *testing.T, token, key, ifMatch, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, prefPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

// weekly는 (요일, 시작, 종료) 묶음으로 engine 구간을 만든다.
func weekly(triples ...[3]int) []engine.WeeklyInterval {
	out := make([]engine.WeeklyInterval, len(triples))
	for i, t := range triples {
		out[i] = engine.WeeklyInterval{Weekday: t[0], StartMinute: t[1], EndMinute: t[2]}
	}
	return out
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("응답이 JSON이 아니다: %v (%s)", err, rec.Body.String())
	}
	return v
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	if e := decode[httpapi.ErrorResponse](t, rec); e.Code != code {
		t.Fatalf("code = %q, want %q", e.Code, code)
	}
}

func prefOf(t *testing.T, rec *httptest.ResponseRecorder) preferencesResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	return decode[preferencesEnvelope](t, rec).Preferences
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func prefChanges(t *testing.T, s *stack, userID string) int {
	t.Helper()
	return countRows(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = 'availability_preference'`, userID)
}

const weekdayBody = `{"time_zone":"Asia/Seoul","weekly_intervals":[
	{"weekday":1,"start_minute":540,"end_minute":1320},
	{"weekday":2,"start_minute":540,"end_minute":1320}]}`

func TestGetUnconfiguredThenCreateAndRead(t *testing.T) {
	s := newStack(t, false)
	u := s.signIn(t)

	rec := s.get(t, prefPath, u.AccessToken)
	pref := prefOf(t, rec)
	if pref.Status != StatusUnconfigured || pref.Version != 0 || pref.TimeZone != nil || len(pref.WeeklyIntervals) != 0 || pref.UpdatedAt != nil {
		t.Fatalf("미설정 응답 = %+v", pref)
	}
	if rec.Header().Get("ETag") != "" {
		t.Fatalf("미설정에 ETag가 있다: %q", rec.Header().Get("ETag"))
	}
	if !strings.Contains(rec.Body.String(), `"weekly_intervals":[]`) {
		t.Fatalf("미설정 interval은 빈 배열이어야 한다: %s", rec.Body.String())
	}
	// 조회는 row를 만들지 않는다.
	if n := countRows(t, s.pool, `SELECT count(*) FROM availability_preferences WHERE user_id = $1`, u.UserID); n != 0 {
		t.Fatalf("GET이 row %d개를 만들었다", n)
	}

	rec = s.put(t, u.AccessToken, "create", "", weekdayBody)
	created := prefOf(t, rec)
	if rec.Header().Get("ETag") != `"1"` || created.Version != 1 || created.Status != StatusConfigured || created.TimeZone == nil || *created.TimeZone != "Asia/Seoul" {
		t.Fatalf("생성 응답 = %+v, ETag %q", created, rec.Header().Get("ETag"))
	}
	want := []Interval{{1, 540, 1320}, {2, 540, 1320}}
	if !sameIntervals(created.WeeklyIntervals, want) {
		t.Fatalf("저장 interval = %+v", created.WeeklyIntervals)
	}
	got := prefOf(t, s.get(t, prefPath, u.AccessToken))
	if got.Version != 1 || !sameIntervals(got.WeeklyIntervals, want) || got.UpdatedAt == nil {
		t.Fatalf("다시 읽은 설정 = %+v", got)
	}
	if n := prefChanges(t, s, u.UserID); n != 1 {
		t.Fatalf("본인 sync 변경 %d건, want 1", n)
	}
}

func TestPutReplacesAllIntervalsAndChecksVersion(t *testing.T) {
	s := newStack(t, false)
	u := s.signIn(t)
	other := s.signIn(t)

	// 설정이 없는데 If-Match를 보내면 낡은 version이다.
	assertError(t, s.put(t, other.AccessToken, "stale-none", `"1"`, weekdayBody), http.StatusConflict, httpapi.CodeVersionConflict)

	if rec := s.put(t, u.AccessToken, "c1", "", weekdayBody); rec.Code != http.StatusOK {
		t.Fatalf("생성 = %d: %s", rec.Code, rec.Body.String())
	}
	// create-only로 기존 설정을 덮어쓰지 않는다.
	assertError(t, s.put(t, u.AccessToken, "c2", "", weekdayBody), http.StatusConflict, httpapi.CodeVersionConflict)

	replaced := prefOf(t, s.put(t, u.AccessToken, "r1", `"1"`,
		`{"time_zone":"America/New_York","weekly_intervals":[{"weekday":5,"start_minute":600,"end_minute":660}]}`))
	if replaced.Version != 2 || *replaced.TimeZone != "America/New_York" || !sameIntervals(replaced.WeeklyIntervals, []Interval{{5, 600, 660}}) {
		t.Fatalf("전체 교체 결과 = %+v", replaced)
	}
	if n := countRows(t, s.pool, `SELECT count(*) FROM availability_weekly_intervals WHERE user_id = $1`, u.UserID); n != 1 {
		t.Fatalf("이전 interval이 남았다: %d개", n)
	}
	assertError(t, s.put(t, u.AccessToken, "r-stale", `"1"`, weekdayBody), http.StatusConflict, httpapi.CodeVersionConflict)

	// 같은 내용은 version을 올리지 않고 sync 변경도 남기지 않는다.
	before := prefChanges(t, s, u.UserID)
	same := prefOf(t, s.put(t, u.AccessToken, "r-same", `"2"`,
		`{"time_zone":"America/New_York","weekly_intervals":[{"weekday":5,"start_minute":600,"end_minute":630},{"weekday":5,"start_minute":630,"end_minute":660}]}`))
	if same.Version != 2 || prefChanges(t, s, u.UserID) != before {
		t.Fatalf("같은 정규형 재저장이 version을 올렸다: %+v", same)
	}
	// time zone만 바꿔도 새 version이다.
	if tz := prefOf(t, s.put(t, u.AccessToken, "r-tz", `"2"`,
		`{"time_zone":"Europe/Berlin","weekly_intervals":[{"weekday":5,"start_minute":600,"end_minute":660}]}`)); tz.Version != 3 {
		t.Fatalf("time zone 변경 version = %d, want 3", tz.Version)
	}
	// 다른 사용자의 미설정 상태는 그대로다.
	if p := prefOf(t, s.get(t, prefPath, other.AccessToken)); p.Status != StatusUnconfigured {
		t.Fatalf("다른 사용자 설정이 바뀌었다: %+v", p)
	}
}

func TestPutIdempotentReplay(t *testing.T) {
	s := newStack(t, false)
	u := s.signIn(t)

	first := s.put(t, u.AccessToken, "same-key", "", weekdayBody)
	if first.Code != http.StatusOK {
		t.Fatalf("첫 요청 = %d: %s", first.Code, first.Body.String())
	}
	replay := s.put(t, u.AccessToken, "same-key", "", weekdayBody)
	if replay.Code != http.StatusOK || replay.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("재전송 = %d, replayed=%q: %s", replay.Code, replay.Header().Get(ReplayedHeader), replay.Body.String())
	}
	if p := prefOf(t, replay); p.Version != 1 {
		t.Fatalf("재전송이 다시 실행됐다: version %d", p.Version)
	}
	if n := prefChanges(t, s, u.UserID); n != 1 {
		t.Fatalf("재전송이 sync 변경을 더 남겼다: %d건", n)
	}
	assertError(t, s.put(t, u.AccessToken, "same-key", "", `{"time_zone":"Asia/Seoul","weekly_intervals":[{"weekday":3,"start_minute":0,"end_minute":60}]}`),
		http.StatusConflict, httpapi.CodeIdempotencyMismatch)
}

func TestPutRejectsInvalidInput(t *testing.T) {
	s := newStack(t, false)
	u := s.signIn(t)
	iv := func(w, start, end int) string {
		return `{"weekday":` + strconv.Itoa(w) + `,"start_minute":` + strconv.Itoa(start) + `,"end_minute":` + strconv.Itoa(end) + `}`
	}
	body := func(tz string, ivs ...string) string {
		return `{"time_zone":"` + tz + `","weekly_intervals":[` + strings.Join(ivs, ",") + `]}`
	}
	cases := map[string]string{
		"30분 경계 아님 시작": body("Asia/Seoul", iv(1, 545, 600)),
		"30분 경계 아님 종료": body("Asia/Seoul", iv(1, 540, 610)),
		"30분 미만":       body("Asia/Seoul", iv(1, 540, 540)),
		"start > end":  body("Asia/Seoul", iv(1, 600, 540)),
		"음수 시작":        body("Asia/Seoul", iv(1, -30, 60)),
		"시작이 1440":     body("Asia/Seoul", iv(1, 1440, 1500)),
		"24시간 초과":      body("Asia/Seoul", iv(1, 60, 1530)),
		"요일 0":         body("Asia/Seoul", iv(0, 540, 600)),
		"요일 8":         body("Asia/Seoul", iv(8, 540, 600)),
		"같은 요일 겹침":     body("Asia/Seoul", iv(1, 540, 720), iv(1, 660, 780)),
		"중복":           body("Asia/Seoul", iv(1, 540, 720), iv(1, 540, 720)),
		"자정 꼬리가 다음 요일과 겹침": body("Asia/Seoul", iv(1, 1320, 1560), iv(2, 60, 180)),
		"병합 후 4개":          body("Asia/Seoul", iv(3, 0, 60), iv(3, 120, 180), iv(3, 240, 300), iv(3, 360, 420)),
		"자정 머리로 4개":        body("Asia/Seoul", iv(3, 120, 180), iv(3, 240, 300), iv(3, 360, 420), iv(2, 1410, 1470)),
		"빈 주":              body("Asia/Seoul"),
		"필드 누락":            `{"time_zone":"Asia/Seoul","weekly_intervals":[{"weekday":1,"start_minute":540}]}`,
		"모르는 필드":           `{"time_zone":"Asia/Seoul","weekly_intervals":[` + iv(1, 540, 600) + `],"extra":1}`,
	}
	// time zone 오류는 전용 code다. 클라이언트가 도시 선택 UI로 갈 수 있게 다른 입력 오류와 구분한다.
	tzCases := map[string]string{
		"time zone 없음": `{"weekly_intervals":[` + iv(1, 540, 600) + `]}`,
		"고정 offset":    body("UTC+09:00", iv(1, 540, 600)),
		"Etc/GMT-9":    body("Etc/GMT-9", iv(1, 540, 600)),
		"etc/UTC":      body("etc/UTC", iv(1, 540, 600)),
		"UTC":          body("UTC", iv(1, 540, 600)),
		"GMT":          body("GMT", iv(1, 540, 600)),
		"Local":        body("Local", iv(1, 540, 600)),
		"소문자":          body("asia/seoul", iv(1, 540, 600)),
		"대문자":          body("ASIA/SEOUL", iv(1, 540, 600)),
		"빈 segment":    body("Asia//Seoul", iv(1, 540, 600)),
		"없는 시간대":       body("Mars/Olympus", iv(1, 540, 600)),
	}
	i := 0
	for name, b := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			assertError(t, s.put(t, u.AccessToken, "invalid-"+strconv.Itoa(i), "", b), http.StatusBadRequest, httpapi.CodeInvalidRequest)
		})
	}
	for name, b := range tzCases {
		i++
		t.Run(name, func(t *testing.T) {
			assertError(t, s.put(t, u.AccessToken, "invalid-"+strconv.Itoa(i), "", b), http.StatusBadRequest, CodeTimeZoneInvalid)
		})
	}
	if n := countRows(t, s.pool, `SELECT count(*) FROM availability_preferences WHERE user_id = $1`, u.UserID); n != 0 {
		t.Fatalf("거부된 요청이 설정을 만들었다")
	}
	// 경계값: 하루 전체(0→1440)와 가장 늦은 시작의 최대 횡단(1410→2850)은 허용된다.
	b := prefOf(t, s.put(t, u.AccessToken, "bounds", "", body("Asia/Seoul", iv(1, 0, 1440), iv(3, 1410, 2850))))
	if want := []Interval{{1, 0, 1440}, {3, 1410, 1440}, {4, 0, 1410}}; !sameIntervals(b.WeeklyIntervals, want) {
		t.Fatalf("경계값 저장 = %+v, want %+v", b.WeeklyIntervals, want)
	}
	// 같은 요일 3개는 허용된다.
	if rec := s.put(t, u.AccessToken, "three", `"1"`, body("Asia/Seoul", iv(3, 0, 60), iv(3, 120, 180), iv(3, 240, 300))); rec.Code != http.StatusOK {
		t.Fatalf("요일당 3개 = %d: %s", rec.Code, rec.Body.String())
	}
	// 헤더 검증
	assertError(t, s.put(t, u.AccessToken, "", "", weekdayBody), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.put(t, u.AccessToken, "weak", `W/"1"`, weekdayBody), http.StatusBadRequest, httpapi.CodeInvalidRequest)
}

func TestPutNormalizesMidnightAndMerges(t *testing.T) {
	s := newStack(t, false)
	u := s.signIn(t)
	// 월 22:00~02:00(자정 횡단), 일 23:00~01:00(일요일 → 월요일), 수 09:00~10:00 + 10:00~11:00(맞닿음 병합),
	// 화 02:00~03:00은 월 꼬리(화 00:00~02:00)와 맞닿아 병합된다.
	body := `{"time_zone":"Europe/London","weekly_intervals":[
		{"weekday":1,"start_minute":1320,"end_minute":1560},
		{"weekday":7,"start_minute":1380,"end_minute":1500},
		{"weekday":3,"start_minute":540,"end_minute":600},
		{"weekday":3,"start_minute":600,"end_minute":660},
		{"weekday":2,"start_minute":120,"end_minute":180}]}`
	pref := prefOf(t, s.put(t, u.AccessToken, "normalize", "", body))
	want := []Interval{
		{1, 0, 60}, {1, 1320, 1440},
		{2, 0, 180},
		{3, 540, 660},
		{7, 1380, 1440},
	}
	if !sameIntervals(pref.WeeklyIntervals, want) {
		t.Fatalf("정규화 결과 = %+v, want %+v", pref.WeeklyIntervals, want)
	}
	// position은 요일 안 시작 순서다.
	rows, err := s.pool.Query(context.Background(), `
		SELECT weekday, position, start_minute FROM availability_weekly_intervals WHERE user_id = $1 ORDER BY weekday, position`, u.UserID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var w, p, start int
		if err := rows.Scan(&w, &p, &start); err != nil {
			t.Fatal(err)
		}
		got = append(got, strconv.Itoa(w)+"/"+strconv.Itoa(p)+"/"+strconv.Itoa(start))
	}
	if strings.Join(got, ",") != "1/0/0,1/1/1320,2/0/0,3/0/540,7/0/1380" {
		t.Fatalf("position = %v", got)
	}
}

func TestNormalizeIntervalsUnit(t *testing.T) {
	p := func(v int) *int { return &v }
	in := func(w, s, e int) intervalInput {
		return intervalInput{Weekday: p(w), StartMinute: p(s), EndMinute: p(e)}
	}
	got, err := normalizeIntervals([]intervalInput{in(7, 1410, 1470), in(1, 30, 90)})
	if err != nil {
		t.Fatal(err)
	}
	// 일요일 꼬리가 월요일 00:00~00:30이 되고 월 00:30~01:30과 맞닿아 병합된다.
	if want := weekly([3]int{1, 0, 90}, [3]int{7, 1410, 1440}); !sameIntervals(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// 정확히 24시간 횡단 구간: 월 12:00 ~ 화 12:00
	got, err = normalizeIntervals([]intervalInput{in(1, 720, 2160)})
	if err != nil || !sameIntervals(got, weekly([3]int{1, 720, 1440}, [3]int{2, 0, 720})) {
		t.Fatalf("24시간 횡단 = %+v, %v", got, err)
	}
	// 입력 상한
	many := make([]intervalInput, maxInputIntervals+1)
	for i := range many {
		many[i] = in(1, 0, 30)
	}
	if _, err := normalizeIntervals(many); err == nil {
		t.Fatal("입력 상한을 넘었는데 통과했다")
	}
	// 30분 칸 전부(7×48)는 상한 안이고 요일당 하나로 병합된다.
	grid := make([]intervalInput, 0, maxInputIntervals)
	for w := 1; w <= 7; w++ {
		for m := 0; m < minutesPerDay; m += slotMinutes {
			grid = append(grid, in(w, m, m+slotMinutes))
		}
	}
	got, err = normalizeIntervals(grid)
	if err != nil || len(got) != 7 || got[0] != weekly([3]int{1, 0, 1440})[0] {
		t.Fatalf("30분 칸 전체 = %+v, %v", got, err)
	}
}

func TestValidTimeZone(t *testing.T) {
	for _, ok := range []string{"Asia/Seoul", "America/New_York", "Europe/London", "America/Argentina/Buenos_Aires", "Pacific/Chatham"} {
		if !ValidTimeZone(ok) {
			t.Errorf("%q를 거부했다", ok)
		}
	}
	for _, bad := range []string{"", "Local", "UTC", "GMT", "EST", "EST5EDT", "Etc/UTC", "Etc/GMT-9", "Etc/GMT+5", "UTC+09:00", "+09:00",
		"etc/UTC", "ETC/GMT-9", "asia/seoul", "ASIA/SEOUL", "Asia/seoul", "Asia//Seoul", "Asia/Seoul/", "/Asia/Seoul", " Asia/Seoul",
		"posix/Asia/Seoul", "right/Asia/Seoul", "../etc/passwd", "/usr/share/zoneinfo/Asia/Seoul", "Asia/Nowhere", strings.Repeat("A/", 40)} {
		if ValidTimeZone(bad) {
			t.Errorf("%q를 받아들였다", bad)
		}
	}
}

// DB 불변식: service를 거치지 않은 쓰기도 같은 요일의 겹침·맞닿음을 커밋 시점에 거부한다.
func TestDBTriggerRejectsOverlapAndAdjacency(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, display_name) VALUES ($1, '트리거 테스트')`, userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })
	if _, err := pool.Exec(ctx, `INSERT INTO availability_preferences (user_id, status, time_zone) VALUES ($1, 'configured', 'Asia/Seoul')`, userID); err != nil {
		t.Fatal(err)
	}
	try := func(rows ...[4]int) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `DELETE FROM availability_weekly_intervals WHERE user_id = $1`, userID); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if _, err := tx.Exec(ctx, `
				INSERT INTO availability_weekly_intervals (user_id, weekday, position, start_minute, end_minute)
				VALUES ($1, $2, $3, $4, $5)`, userID, r[0], r[1], r[2], r[3]); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	assertTrigger := func(name string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "availability_weekly_intervals_no_overlap_check" {
			t.Fatalf("%s: err = %v, want 겹침 트리거 위반", name, err)
		}
	}
	assertTrigger("겹침", try([4]int{1, 0, 540, 720}, [4]int{1, 1, 600, 780}))
	assertTrigger("맞닿음", try([4]int{1, 0, 540, 600}, [4]int{1, 1, 600, 660}))
	if err := try([4]int{1, 0, 540, 600}, [4]int{1, 1, 630, 660}, [4]int{2, 0, 540, 600}); err != nil {
		t.Fatalf("떨어진 구간과 다른 요일 같은 구간이 거부됐다: %v", err)
	}
	// position을 바꿔도(UPDATE) 겹침을 만들 수 없다.
	_, err := pool.Exec(ctx, `UPDATE availability_weekly_intervals SET start_minute = 570 WHERE user_id = $1 AND weekday = 1 AND position = 1`, userID)
	assertTrigger("UPDATE 겹침", err)
	// row CHECK: 30분 배수, 1440 이하
	var pgErr *pgconn.PgError
	if err := try([4]int{1, 0, 545, 600}); !errors.As(err, &pgErr) || pgErr.ConstraintName != "availability_weekly_intervals_minute_check" {
		t.Fatalf("30분 배수 CHECK: %v", err)
	}
	if err := try([4]int{1, 3, 540, 600}); !errors.As(err, &pgErr) || pgErr.ConstraintName != "availability_weekly_intervals_position_check" {
		t.Fatalf("position CHECK: %v", err)
	}
	// configured는 time_zone이 필수다.
	if _, err := pool.Exec(ctx, `UPDATE availability_preferences SET time_zone = NULL WHERE user_id = $1`, userID); !errors.As(err, &pgErr) || pgErr.ConstraintName != "availability_preferences_time_zone_check" {
		t.Fatalf("time_zone CHECK: %v", err)
	}
	// 사용자 삭제는 cascade로 지운다(DELETE에는 트리거가 없다).
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("사용자 삭제 cascade 실패: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM availability_weekly_intervals WHERE user_id = $1`, userID); n != 0 {
		t.Fatalf("cascade 뒤 interval %d개", n)
	}
}

// 같은 expected version의 동시 PUT은 정확히 하나만 이기고, 잠금 규약 위반(교착 재시도)이 없다.
func TestConcurrentPutsSerializeWithoutRetries(t *testing.T) {
	s := newStack(t, false)
	u := s.signIn(t)
	before := mutation.Retries()

	race := func(ifMatch string, round int) {
		t.Helper()
		start := make(chan struct{})
		var wg sync.WaitGroup
		// 여러 요청을 한꺼번에 풀어 실제로 잠금에서 겹치게 한다.
		const n = 8
		codes := make([]int, n)
		bodies := make([]string, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				b := `{"time_zone":"Asia/Seoul","weekly_intervals":[{"weekday":` + strconv.Itoa(i%7+1) + `,"start_minute":` + strconv.Itoa(60*round) + `,"end_minute":1440}]}`
				rec := s.put(t, u.AccessToken, "race-"+ifMatch+"-"+strconv.Itoa(i), ifMatch, b)
				codes[i], bodies[i] = rec.Code, rec.Body.String()
			}()
		}
		close(start)
		wg.Wait()
		ok, conflict := 0, 0
		for i, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
				if !strings.Contains(bodies[i], httpapi.CodeVersionConflict) {
					t.Fatalf("409 code: %s", bodies[i])
				}
			default:
				t.Fatalf("예상 밖 status %d: %s", c, bodies[i])
			}
		}
		if ok != 1 || conflict != n-1 {
			t.Fatalf("If-Match %q 경쟁 결과 ok=%d conflict=%d, want 1/%d (%v)", ifMatch, ok, conflict, n-1, bodies)
		}
	}
	race("", 1)    // 둘 다 create-only
	race(`"1"`, 2) // 둘 다 version 1 기준 교체

	pref := prefOf(t, s.get(t, prefPath, u.AccessToken))
	if pref.Version != 2 || len(pref.WeeklyIntervals) != 1 {
		t.Fatalf("최종 상태 = %+v", pref)
	}
	if n := prefChanges(t, s, u.UserID); n != 2 {
		t.Fatalf("sync 변경 %d건, want 2(이긴 요청마다 하나)", n)
	}
	if after := mutation.Retries(); after != before {
		t.Fatalf("mutation.Retries가 %d에서 %d로 늘었다. 잠금 규약이 깨졌다", before, after)
	}
}

// 다른 사용자는 API·sync·bootstrap·로그 어디서도 원시 활동 시간을 얻지 못한다.
func TestRawIntervalsStayPrivate(t *testing.T) {
	s := newStack(t, false)
	owner := s.signIn(t)
	other := s.signIn(t)
	const tz = "America/Argentina/Buenos_Aires"
	body := `{"time_zone":"` + tz + `","weekly_intervals":[{"weekday":4,"start_minute":750,"end_minute":1170}]}`
	if rec := s.put(t, owner.AccessToken, "private", "", body); rec.Code != http.StatusOK {
		t.Fatalf("저장 = %d: %s", rec.Code, rec.Body.String())
	}
	// 실패 경로 로그도 본다.
	_ = s.put(t, owner.AccessToken, "private-bad", `"1"`, `{"time_zone":"`+tz+`","weekly_intervals":[{"weekday":4,"start_minute":750,"end_minute":755}]}`)

	// 다른 사용자의 GET은 자기 미설정 상태다. 다른 사용자 규칙을 조회하는 endpoint는 없다.
	if p := prefOf(t, s.get(t, prefPath, other.AccessToken)); p.Status != StatusUnconfigured || len(p.WeeklyIntervals) != 0 {
		t.Fatalf("다른 사용자 GET = %+v", p)
	}
	if rec := s.get(t, "/v1/availability/preferences/"+owner.UserID, other.AccessToken); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("다른 사용자 규칙 경로가 응답했다: %d", rec.Code)
	}
	if n := countRows(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1 AND entity_type LIKE 'availability%'`, other.UserID); n != 0 {
		t.Fatalf("다른 사용자 sync에 활동 시간 변경 %d건", n)
	}
	boot := s.get(t, "/v1/sync/bootstrap", other.AccessToken)
	if boot.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d: %s", boot.Code, boot.Body.String())
	}
	var snap struct {
		Snapshot []struct {
			EntityType string `json:"entity_type"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &snap); err != nil || len(snap.Snapshot) == 0 {
		t.Fatalf("bootstrap 해석 실패(%v): %s", err, boot.Body.String())
	}
	for _, e := range snap.Snapshot {
		if strings.HasPrefix(e.EntityType, "availability") {
			t.Fatalf("다른 사용자 bootstrap에 %s entity가 있다", e.EntityType)
		}
	}
	// 우연히 나올 수 없는 값만 문자열로 찾는다(숫자는 UUID·cursor에 우연히 섞일 수 있다).
	for _, leak := range []string{tz, `"start_minute"`} {
		if strings.Contains(boot.Body.String(), leak) {
			t.Fatalf("다른 사용자 bootstrap에 %q가 있다: %s", leak, boot.Body.String())
		}
	}
	// 본인 sync 변경도 무효화 신호뿐이다. interval과 time zone을 싣지 않는다.
	var payload string
	if err := s.pool.QueryRow(context.Background(), `
		SELECT payload::text FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = 'availability_preference'`, owner.UserID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, tz) || strings.Contains(payload, "minute") || strings.Contains(payload, "interval") {
		t.Fatalf("본인 sync payload에 원시 값이 있다: %s", payload)
	}
	for _, leak := range []string{tz, `"start_minute"`} {
		if strings.Contains(s.logs.String(), leak) {
			t.Fatalf("로그에 %q가 있다: %s", leak, s.logs.String())
		}
	}
}

// 새 소비자는 Require 다음, 입력 검증 앞에서 센다. 한도를 넘으면 잘못된 본문도 400이 아니라 429다.
// awaitSafeWindow는 고정 창 경계까지 margin보다 적게 남았으면 경계를 지날 때까지 기다린다. 창은 epoch 기준
// date_bin이라 경계에 걸리면 한 테스트의 요청이 두 창에 나뉘어 한도 단언이 어긋난다. margin은 세는 요청 전체가
// 끝나는 데 걸리는 시간보다 넉넉해야 하고, 호출은 첫 번째로 세는 요청 바로 앞에 둔다.
func awaitSafeWindow(window, margin time.Duration) {
	if remaining := window - time.Duration(time.Now().UnixNano())%window; remaining < margin {
		time.Sleep(remaining + 100*time.Millisecond)
	}
}

func TestRateLimitBeforeValidation(t *testing.T) {
	s := newStack(t, true)
	u := s.signIn(t)
	awaitSafeWindow(time.Minute, 10*time.Second)
	for i := range ratelimit.LimitAvailabilityPrefWrite.Count {
		if rec := s.put(t, u.AccessToken, "", "", `{}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("%d번째 = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := s.put(t, u.AccessToken, "", "", `{}`)
	assertError(t, rec, http.StatusTooManyRequests, httpapi.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After가 없다")
	}
	// 조회 한도는 별도 scope이고, 인증 직후·조회 전에 센다.
	for i := range ratelimit.LimitAvailabilityPrefRead.Count {
		if got := s.get(t, prefPath, u.AccessToken); got.Code != http.StatusOK {
			t.Fatalf("%d번째 조회 = %d", i, got.Code)
		}
	}
	assertError(t, s.get(t, prefPath, u.AccessToken), http.StatusTooManyRequests, httpapi.CodeRateLimited)
}

// 내장 목록의 모든 이름은 규칙을 통과하고 실제로 로딩된다. Go를 올린 뒤 목록을 다시 만들지 않으면 여기서 드러난다.
func TestRegionZonesAreLoadable(t *testing.T) {
	if len(regionZones) < 300 {
		t.Fatalf("지역 시간대가 %d개뿐이다", len(regionZones))
	}
	for name := range regionZones {
		if !ValidTimeZone(name) {
			t.Errorf("%q가 규칙을 통과하지 못한다", name)
		}
	}
}

// LoadPreferences는 여러 사용자를 한 문장으로 읽고, 미설정 사용자도 version 0으로 돌려준다.
func TestLoadPreferencesInRepeatableRead(t *testing.T) {
	s := newStack(t, false)
	a, b, c := s.signIn(t), s.signIn(t), s.signIn(t)
	if rec := s.put(t, a.AccessToken, "a", "", `{"time_zone":"Asia/Seoul","weekly_intervals":[{"weekday":2,"start_minute":600,"end_minute":660},{"weekday":1,"start_minute":1320,"end_minute":1500}]}`); rec.Code != http.StatusOK {
		t.Fatalf("a 저장 = %d", rec.Code)
	}
	if rec := s.put(t, b.AccessToken, "b", "", weekdayBody); rec.Code != http.StatusOK {
		t.Fatalf("b 저장 = %d", rec.Code)
	}
	ctx := context.Background()
	ids := []uuid.UUID{uuid.MustParse(a.UserID), uuid.MustParse(b.UserID), uuid.MustParse(c.UserID)}
	var got map[uuid.UUID]Preference
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		got, err = LoadPreferences(ctx, tx, ids)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("결과 %d명, want 3", len(got))
	}
	pa := got[ids[0]]
	if pa.Status != StatusConfigured || pa.TimeZone != "Asia/Seoul" || pa.Version != 1 || pa.UpdatedAt.IsZero() ||
		!sameIntervals(pa.Intervals, weekly([3]int{1, 1320, 1440}, [3]int{2, 0, 60}, [3]int{2, 600, 660})) {
		t.Fatalf("a = %+v", pa)
	}
	if pb := got[ids[1]]; pb.Version != 1 || len(pb.Intervals) != 2 {
		t.Fatalf("b = %+v", pb)
	}
	if pc := got[ids[2]]; pc.Status != StatusUnconfigured || pc.Version != 0 || pc.TimeZone != "" || pc.Intervals == nil || len(pc.Intervals) != 0 {
		t.Fatalf("미설정 c = %+v", pc)
	}
	if empty, err := LoadPreferences(ctx, s.pool, nil); err != nil || len(empty) != 0 {
		t.Fatalf("빈 입력 = %v, %v", empty, err)
	}
}

// create-only와 If-Match 요청이 섞여 경쟁해도 각 version에 이기는 요청은 하나뿐이고 교착 재시도가 없다.
func TestMixedCreateAndIfMatchRace(t *testing.T) {
	s := newStack(t, false)
	u := s.signIn(t)
	before := mutation.Retries()
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ifMatch := ""
			if i%2 == 1 {
				ifMatch = `"1"`
			}
			b := `{"time_zone":"Asia/Seoul","weekly_intervals":[{"weekday":` + strconv.Itoa(i%7+1) + `,"start_minute":0,"end_minute":` + strconv.Itoa(60+30*i) + `}]}`
			codes[i] = s.put(t, u.AccessToken, "mixed-"+strconv.Itoa(i), ifMatch, b).Code
		}()
	}
	close(start)
	wg.Wait()
	createWins, matchWins := 0, 0
	for i, c := range codes {
		switch {
		case c == http.StatusOK && i%2 == 0:
			createWins++
		case c == http.StatusOK:
			matchWins++
		case c != http.StatusConflict:
			t.Fatalf("예상 밖 status %d", c)
		}
	}
	// create-only는 정확히 하나가 이긴다. If-Match "1"은 생성 뒤에 도착한 경우에만, 많아야 하나가 이긴다.
	if createWins != 1 || matchWins > 1 {
		t.Fatalf("create 승 %d, If-Match 승 %d (%v)", createWins, matchWins, codes)
	}
	if p := prefOf(t, s.get(t, prefPath, u.AccessToken)); p.Version != int64(1+matchWins) {
		t.Fatalf("최종 version %d, 성공 %d건", p.Version, 1+matchWins)
	}
	if after := mutation.Retries(); after != before {
		t.Fatalf("mutation.Retries가 %d에서 %d로 늘었다", before, after)
	}
}

// 본인 다른 기기는 GET /v1/sync로 availability_preference 무효화 변경을 받는다.
func TestOwnSyncDeliversPreferenceChange(t *testing.T) {
	s := newStack(t, false)
	u := s.signIn(t)
	boot := s.get(t, "/v1/sync/bootstrap", u.AccessToken)
	if boot.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d", boot.Code)
	}
	cursor := decode[struct {
		Watermark string `json:"cursor_watermark"`
	}](t, boot).Watermark
	if rec := s.put(t, u.AccessToken, "sync", "", weekdayBody); rec.Code != http.StatusOK {
		t.Fatalf("저장 = %d", rec.Code)
	}
	// settled horizon 읽기라 방금 커밋한 변경이 곧바로 보이지 않을 수 있다. 잠시 다시 읽는다.
	type change struct {
		EntityType string          `json:"entity_type"`
		EntityID   string          `json:"entity_id"`
		Operation  string          `json:"operation"`
		Version    *int64          `json:"version"`
		Payload    json.RawMessage `json:"payload"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := s.get(t, "/v1/sync?cursor="+cursor, u.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("sync = %d: %s", rec.Code, rec.Body.String())
		}
		page := decode[struct {
			Changes []change `json:"changes"`
		}](t, rec)
		for _, c := range page.Changes {
			if c.EntityType != entityPreference {
				continue
			}
			if c.EntityID != u.UserID || c.Operation != "upsert" || c.Version == nil || *c.Version != 1 {
				t.Fatalf("변경 = %+v", c)
			}
			var payload map[string]any
			if err := json.Unmarshal(c.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload) != 3 || payload["status"] != StatusConfigured || payload["version"] != float64(1) || payload["updated_at"] == nil {
				t.Fatalf("payload = %s", c.Payload)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("availability_preference 변경이 오지 않았다: %s", rec.Body.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// trigger는 소유자 preference row를 잠근 뒤 검사한다. service 밖 두 트랜잭션이 동시에 겹치는 row를
// 넣어도 둘 다 커밋되지 않는다. 잠금이 없으면 tx2의 검사가 tx1 커밋 전에 끝나 겹침이 저장된다.
func TestDBTriggerSerializesConcurrentDirectWrites(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, display_name) VALUES ($1, '트리거 경쟁')`, userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })
	if _, err := pool.Exec(ctx, `INSERT INTO availability_preferences (user_id, status, time_zone) VALUES ($1, 'configured', 'Asia/Seoul')`, userID); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO availability_weekly_intervals (user_id, weekday, position, start_minute, end_minute) VALUES ($1, 1, $2, $3, $4)`
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	if _, err := tx1.Exec(ctx, insert, userID, 0, 540, 720); err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, insert, userID, 1, 600, 780); err != nil {
		t.Fatal(err)
	}
	// tx1의 검사를 지금 실행해 preference row 잠금을 쥔 채로 둔다(이 트랜잭션의 마지막 SET CONSTRAINTS).
	if _, err := tx1.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tx2.Commit(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("tx2가 tx1의 잠금을 기다리지 않고 끝났다: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 커밋: %v", err)
	}
	err = <-done
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "availability_weekly_intervals_no_overlap_check" {
		t.Fatalf("tx2 커밋 err = %v, want 겹침 트리거 위반", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM availability_weekly_intervals WHERE user_id = $1`, userID); n != 1 {
		t.Fatalf("저장된 구간 %d개, want 1", n)
	}
}
