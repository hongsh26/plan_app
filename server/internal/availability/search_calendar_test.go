package availability

// 캘린더 상태 전이와 검색의 결합을 실제 calendar API 경로로 확인한다. SQL로 연결 상태를 세팅하지 않는다.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/availability/engine"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/postgres/pgtest"
)

func (s *stack) do(t *testing.T, method, path, token, ifMatch, body string, key bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if key {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

type calConn struct {
	Status           string `json:"status"`
	ActiveGeneration int64  `json:"active_generation"`
	Version          int64  `json:"version"`
}

func (s *stack) calConn(t *testing.T, sess session) calConn {
	t.Helper()
	rec := s.get(t, "/v1/calendar/connection", sess.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET connection = %d: %s", rec.Code, rec.Body.String())
	}
	return decode[struct {
		Connection calConn `json:"connection"`
	}](t, rec).Connection
}

// calPut은 현재 version을 If-Match로 실어 연결을 바꾼다(연결이 없으면 create-only).
func (s *stack) calPut(t *testing.T, sess session, body string) {
	t.Helper()
	ifMatch := ""
	if v := s.calConn(t, sess).Version; v > 0 {
		ifMatch = `"` + strconv.FormatInt(v, 10) + `"`
	}
	if rec := s.do(t, http.MethodPut, "/v1/calendar/connection", sess.AccessToken, ifMatch, body, true); rec.Code != http.StatusOK {
		t.Fatalf("PUT connection %s = %d: %s", body, rec.Code, rec.Body.String())
	}
}

const calEnableBody = `{"status":"selecting","sources":[{"key":"cal-1","enabled":true}],"time_zone":"Asia/Seoul","claim_sync_device":true}`

func eventKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// calCreate는 snapshot session을 만들고 facts를 한 page로 올린다. complete는 하지 않는다.
func (s *stack) calCreate(t *testing.T, sess session, revision int64, busy ...engine.Interval) uuid.UUID {
	t.Helper()
	ws, we := defaultWindow()
	pages := 0
	if len(busy) > 0 {
		pages = 1
	}
	rec := s.do(t, http.MethodPost, "/v1/calendar/snapshots", sess.AccessToken, "",
		fmt.Sprintf(`{"revision":%d,"window_start":%q,"window_end":%q,"expected_pages":%d}`,
			revision, ws.Format(time.RFC3339), we.Format(time.RFC3339), pages), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("snapshot 생성 = %d: %s", rec.Code, rec.Body.String())
	}
	id := decode[struct {
		Snapshot struct {
			ID uuid.UUID `json:"id"`
		} `json:"snapshot"`
	}](t, rec).Snapshot.ID
	if pages == 1 {
		facts := make([]map[string]any, len(busy))
		for i, b := range busy {
			facts[i] = map[string]any{"source_event_key": eventKey(t), "start_at": b.Start.UTC().Format(time.RFC3339),
				"end_at": b.End.UTC().Format(time.RFC3339), "all_day": false, "time_zone": "Asia/Seoul", "availability": "busy"}
		}
		body, _ := json.Marshal(map[string]any{"facts": facts})
		if rec := s.do(t, http.MethodPut, fmt.Sprintf("/v1/calendar/snapshots/%s/pages/0", id), sess.AccessToken, "", string(body), false); rec.Code != http.StatusOK {
			t.Fatalf("page = %d: %s", rec.Code, rec.Body.String())
		}
	}
	return id
}

func (s *stack) calComplete(t *testing.T, sess session, id uuid.UUID, n int) {
	t.Helper()
	if rec := s.do(t, http.MethodPost, "/v1/calendar/snapshots/"+id.String()+"/complete", sess.AccessToken, "", fmt.Sprintf(`{"fact_count":%d}`, n), true); rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body.String())
	}
}

func (s *stack) calAbort(t *testing.T, sess session, id uuid.UUID) {
	t.Helper()
	if rec := s.do(t, http.MethodPost, "/v1/calendar/snapshots/"+id.String()+"/abort", sess.AccessToken, "", "", true); rec.Code != http.StatusOK {
		t.Fatalf("abort = %d: %s", rec.Code, rec.Body.String())
	}
}

// apiReadyMember는 활동 시간과 캘린더(연결 → snapshot → complete)를 모두 실제 API로 준비한다.
func (s *stack) apiReadyMember(t *testing.T, busy ...engine.Interval) session {
	t.Helper()
	sess := s.signIn(t)
	s.setPrefs(t, sess, "Asia/Seoul", allDay)
	s.calPut(t, sess, calEnableBody)
	s.calComplete(t, sess, s.calCreate(t, sess, 1, busy...), len(busy))
	if c := s.calConn(t, sess); c.Status != "ready" {
		t.Fatalf("준비 뒤 상태 = %s", c.Status)
	}
	return sess
}

// 철회·오류·source 변경 뒤 이전 generation이 "재동기화 중"으로 되살아나지 않는다. 정상 재동기화는 계속 검색된다.
func TestSearchCalendarTransitionsThroughAPI(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	body := paramsBody(params)
	a := s.apiReadyMember(t)
	b := s.apiReadyMember(t)
	p := s.newParty(t, a)
	s.join(t, b, p)
	ok := func(name string) searchResult {
		t.Helper()
		rec := s.search(t, a, p.ID, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 검색 = %d: %s", name, rec.Code, rec.Body.String())
		}
		return decode[searchResult](t, rec)
	}
	pending := func(name, selfCal string) {
		t.Helper()
		assertPending(t, s.search(t, b, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: selfCal})
		assertPending(t, s.search(t, a, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarReady})
	}
	stale := func(name, version string) {
		t.Helper()
		if err := s.verify(t, p.ID, a, params, version, time.Now()); !errors.Is(err, ErrResultStale) {
			t.Fatalf("%s: 이전 version = %v, want stale", name, err)
		}
	}
	rev := int64(1)
	next := func() int64 { rev++; return rev }

	// 정상 재동기화(ready → syncing): 계속 200, 이전 version 유효.
	v := ok("처음").CalculationVersion
	resync := s.calCreate(t, b, next())
	if c := s.calConn(t, b); c.Status != "syncing" {
		t.Fatalf("재동기화 상태 = %s", c.Status)
	}
	ok("정상 재동기화 중")
	if err := s.verify(t, p.ID, a, params, v, time.Now()); err != nil {
		t.Fatalf("정상 재동기화 중 이전 version = %v", err)
	}
	s.calComplete(t, b, resync, 0)
	v = ok("재동기화 완료").CalculationVersion

	// 철회 → selecting → snapshot 생성(syncing): 철회 전 generation으로 계산하지 않는다.
	s.calPut(t, b, `{"status":"revoked"}`)
	pending("철회", SelfCalendarRevoked)
	stale("철회", v)
	s.calPut(t, b, `{"status":"selecting"}`)
	pending("철회 뒤 selecting", SelfCalendarSyncing)
	revived := s.calCreate(t, b, next())
	if c := s.calConn(t, b); c.Status != "syncing" || c.ActiveGeneration == 0 {
		t.Fatalf("철회 뒤 snapshot 생성 상태 = %+v", c)
	}
	pending("철회 뒤 재동기화 중", SelfCalendarSyncing)
	stale("철회 뒤 재동기화 중", v)
	// 그 업로드를 abort하면 철회 전 generation은 쓸 수 없으므로 selecting으로 돌아간다.
	s.calAbort(t, b, revived)
	if c := s.calConn(t, b); c.Status != "selecting" {
		t.Fatalf("abort 뒤 상태 = %s", c.Status)
	}
	pending("철회 뒤 abort", SelfCalendarSyncing)
	s.calComplete(t, b, s.calCreate(t, b, next()), 0)
	v = ok("철회 뒤 새 complete").CalculationVersion

	// ready → error: error 자체는 계산하지 않는다(409). error → snapshot 생성(syncing)은 마지막 정상 generation을
	// 유지하므로(설계 3 §5.2, 철회 이력 없음) 검색 200이고 이전 version도 그대로다.
	s.calPut(t, b, `{"status":"error"}`)
	pending("오류", SelfCalendarError)
	stale("오류 중", v)
	errSync := s.calCreate(t, b, next())
	ok("오류 뒤 재동기화 중")
	if err := s.verify(t, p.ID, a, params, v, time.Now()); err != nil {
		t.Fatalf("오류 뒤 재동기화 중 이전 version = %v", err)
	}
	s.calComplete(t, b, errSync, 0)
	v = ok("오류 뒤 새 complete").CalculationVersion

	// 철회 → error: 철회에서 플래그가 내려갔으므로 error → syncing이어도 계산하지 않는다.
	s.calPut(t, b, `{"status":"revoked"}`)
	s.calPut(t, b, `{"status":"error"}`)
	pending("철회 뒤 오류", SelfCalendarError)
	revokedErr := s.calCreate(t, b, next())
	pending("철회 뒤 오류 뒤 재동기화 중", SelfCalendarSyncing)
	stale("철회 뒤 오류 뒤 재동기화 중", v)
	s.calComplete(t, b, revokedErr, 0)
	v = ok("철회 뒤 오류 뒤 새 complete").CalculationVersion

	// ready에서 status 없이 source만 바꾸면 새 complete 전까지 계산하지 않는다.
	s.calPut(t, b, `{"sources":[{"key":"cal-2","enabled":true}]}`)
	if c := s.calConn(t, b); c.Status != "ready" {
		t.Fatalf("source 변경 뒤 상태 = %s", c.Status)
	}
	pending("source 변경", SelfCalendarStale)
	stale("source 변경", v)
	s.calComplete(t, b, s.calCreate(t, b, next()), 0)
	v = ok("source 변경 뒤 새 complete").CalculationVersion

	// 같은 source 목록을 다시 보내는 것은 변경이 아니다.
	s.calPut(t, b, `{"sources":[{"key":"cal-2","enabled":true}]}`)
	if got := ok("같은 source 재전송").CalculationVersion; got == "" {
		t.Fatal("version이 없다")
	}
	if err := s.verify(t, p.ID, a, params, v, time.Now()); err != nil {
		t.Fatalf("같은 source 재전송 뒤 이전 version = %v", err)
	}
}

// 캐시 key는 첫 grid 시작이다. 같은 grid 칸 안에서 시계가 움직여도 hit이다.
func TestSearchCacheKeyUsesFirstGridStart(t *testing.T) {
	s := newStack(t, false)
	// now+30분이 grid 칸의 10분 지점에 오게 시계를 맞춘다(Asia/Seoul grid는 UTC 30분 경계와 같다).
	real := time.Now()
	target := real.Truncate(30 * time.Minute).Add(10 * time.Minute)
	if target.Before(real) {
		target = target.Add(30 * time.Minute)
	}
	s.clock.shift(target.Sub(real))
	a := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	first := s.mustSearch(t, a, p.ID, `{"time_zone":"Asia/Seoul"}`)
	s.clock.shift(time.Second)
	second := s.mustSearch(t, a, p.ID, `{"time_zone":"Asia/Seoul"}`)
	if s.avail.cache.hitCount() != 1 || second.CalculationVersion != first.CalculationVersion {
		t.Fatalf("1초 뒤 재검색이 miss다(hits=%d)", s.avail.cache.hitCount())
	}
	// 다음 grid 칸으로 넘어가면 miss다.
	s.clock.shift(30 * time.Minute)
	s.mustSearch(t, a, p.ID, `{"time_zone":"Asia/Seoul"}`)
	if s.avail.cache.hitCount() != 1 {
		t.Fatal("grid 칸이 바뀌었는데 hit")
	}
}

// party_id는 정규화한 UUID로 센다. 파싱할 수 없는 값은 고정 subject 하나로 모은다.
func TestSearchRateLimitSubjectNormalization(t *testing.T) {
	s := newStack(t, true)
	a := s.signIn(t)
	b := s.signIn(t)
	id := uuid.New()
	awaitSafeWindow(time.Minute, 10*time.Second)
	for i := 0; i < 20; i++ {
		path := searchPath(id)
		if i%2 == 1 {
			path = "/v1/parties/" + strings.ToUpper(id.String()) + "/availability/search"
		}
		s.post(t, path, a.AccessToken, "", `{}`)
	}
	assertError(t, s.post(t, searchPath(id), a.AccessToken, "", `{}`), http.StatusTooManyRequests, httpapi.CodeRateLimited)

	for i := 0; i < 20; i++ {
		s.post(t, fmt.Sprintf("/v1/parties/bad-%d/availability/search", i), b.AccessToken, "", `{}`)
	}
	assertError(t, s.post(t, "/v1/parties/another-bad/availability/search", b.AccessToken, "", `{}`), http.StatusTooManyRequests, httpapi.CodeRateLimited)
}

// 쿼리 도중 기한이 지나면 부분 결과 없이 503이다. 다른 트랜잭션이 테이블 잠금을 잡아 snapshot 쿼리를 멈춘다.
func TestSearchTimeoutDuringQuery(t *testing.T) {
	s := newStack(t, false)
	a := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	ctx := context.Background()
	locker := pgtest.Pool(t, "TEST_DATABASE_URL")
	tx, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE calendar_connections IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	s.avail.searchTimeout = 300 * time.Millisecond
	s.logs.Reset()
	start := time.Now()
	rec := s.search(t, a, p.ID, paramsBody(testParams(t, "Asia/Seoul")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	assertError(t, rec, http.StatusServiceUnavailable, CodeUnavailable)
	if rec.Header().Get("Retry-After") == "" || strings.Contains(rec.Body.String(), "slots") {
		t.Fatalf("503 본문·헤더: %s", rec.Body.String())
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("기한을 지키지 못했다: %v", d)
	}
	if !strings.Contains(s.logs.String(), `"result_code":"availability_temporarily_unavailable"`) {
		t.Fatalf("지표 로그가 없다: %s", s.logs.String())
	}
}

// 클라이언트가 snapshot 쿼리 도중 취소하면 오류 로그 없이 499(result_code=canceled)로 남는다.
// 테이블 잠금으로 snapshot 쿼리를 멈춘 뒤 요청 context를 취소한다(인증은 잠긴 테이블을 읽지 않는다).
func TestSearchClientCancelIsNotAnError(t *testing.T) {
	s := newStack(t, false)
	a := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	bg := context.Background()
	locker := pgtest.Pool(t, "TEST_DATABASE_URL")
	tx, err := locker.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(bg) }()
	if _, err := tx.Exec(bg, `LOCK TABLE calendar_connections IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	req := httptest.NewRequest(http.MethodPost, searchPath(p.ID), strings.NewReader(paramsBody(testParams(t, "Asia/Seoul")))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	s.logs.Reset()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.handler.ServeHTTP(rec, req)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("취소 뒤에도 끝나지 않았다")
	}
	logs := s.logs.String()
	if rec.Code != statusClientClosed || !strings.Contains(logs, `"result_code":"canceled"`) {
		t.Fatalf("취소 = %d: %s", rec.Code, logs)
	}
	if strings.Contains(logs, `"level":"ERROR"`) {
		t.Fatalf("취소가 오류 로그를 남겼다: %s", logs)
	}
}
