package availability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/availability/engine"
	"plantogether/server/internal/platform/httpapi"
)

func searchPath(id uuid.UUID) string { return "/v1/parties/" + id.String() + "/availability/search" }

func (s *stack) search(t *testing.T, sess session, partyID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return s.post(t, searchPath(partyID), sess.AccessToken, "", body)
}

type searchResult struct {
	RequestID          string         `json:"request_id"`
	Slots              []slotResponse `json:"slots"`
	CalculationVersion string         `json:"calculation_version"`
	GeneratedAt        string         `json:"generated_at"`
	ValidUntil         string         `json:"valid_until"`
}

func paramsBody(p SearchParams) string {
	return fmt.Sprintf(`{"start_date":%q,"end_date":%q,"duration_minutes":%d,"time_zone":%q,"max_results":%d}`,
		dateString(p.StartDate), dateString(p.EndDate), p.DurationMinutes, p.TimeZone, p.MaxResults)
}

func (s *stack) mustSearch(t *testing.T, sess session, partyID uuid.UUID, body string) searchResult {
	t.Helper()
	rec := s.search(t, sess, partyID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("검색 = %d: %s", rec.Code, rec.Body.String())
	}
	return decode[searchResult](t, rec)
}

// assertBodyKeys는 JSON 최상위 key가 정확히 want인지 본다.
func assertBodyKeys(t *testing.T, body []byte, want ...string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != len(want) {
		t.Fatalf("key = %v, want %v", keys(m), want)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("key = %v, want %v", keys(m), want)
		}
	}
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func assertPending(t *testing.T, rec *httptest.ResponseRecorder, code string, self SelfState) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	assertBodyKeys(t, rec.Body.Bytes(), "code", "message", "request_id", "self")
	got := decode[pendingResponse](t, rec)
	if got.Code != code || got.Self != self {
		t.Fatalf("pending = %s %+v, want %s %+v", got.Code, got.Self, code, self)
	}
	var selfKeys map[string]json.RawMessage
	_ = json.Unmarshal(mustRaw(t, rec.Body.Bytes(), "self"), &selfKeys)
	if len(selfKeys) != 2 {
		t.Fatalf("self key = %v", keys(selfKeys))
	}
}

func mustRaw(t *testing.T, body []byte, key string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m[key]
}

func TestSearchReadyAcrossTimeZones(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	env := envOf(t, params)
	busyC := engine.Interval{Start: env.Start.Add(26 * time.Hour), End: env.Start.Add(30 * time.Hour)}

	a := s.readyMember(t, "Asia/Seoul")
	b := s.signIn(t)
	// 뉴욕 평일 08:00~12:00만 활동
	s.setPrefs(t, b, "America/New_York", `[{"weekday":1,"start_minute":480,"end_minute":720},{"weekday":2,"start_minute":480,"end_minute":720},
		{"weekday":3,"start_minute":480,"end_minute":720},{"weekday":4,"start_minute":480,"end_minute":720},{"weekday":5,"start_minute":480,"end_minute":720}]`)
	ws, we := defaultWindow()
	s.seedCalendar(t, b.UserID, time.Now().Add(-2*time.Hour), ws, we)
	c := s.readyMember(t, "Europe/London", busyC)
	p := s.newParty(t, a)
	s.join(t, b, p)
	s.join(t, c, p)

	rec := s.search(t, b, p.ID, paramsBody(params))
	if rec.Code != http.StatusOK {
		t.Fatalf("검색 = %d: %s", rec.Code, rec.Body.String())
	}
	assertBodyKeys(t, rec.Body.Bytes(), "request_id", "slots", "calculation_version", "generated_at", "valid_until")
	res := decode[searchResult](t, rec)
	if len(res.Slots) == 0 || len(res.Slots) > 20 {
		t.Fatalf("slots = %d", len(res.Slots))
	}

	// 같은 입력을 엔진에 직접 넣은 결과와 정확히 같아야 한다(DB → 엔진 조립 확인).
	seoul, _ := time.LoadLocation("Asia/Seoul")
	ny, _ := time.LoadLocation("America/New_York")
	london, _ := time.LoadLocation("Europe/London")
	all := []engine.WeeklyInterval{}
	for d := 1; d <= 7; d++ {
		all = append(all, engine.WeeklyInterval{Weekday: d, StartMinute: 0, EndMinute: 1440})
	}
	weekday := []engine.WeeklyInterval{}
	for d := 1; d <= 5; d++ {
		weekday = append(weekday, engine.WeeklyInterval{Weekday: d, StartMinute: 480, EndMinute: 720})
	}
	want := engine.Search([]engine.Member{
		{Location: seoul, Weekly: all},
		{Location: ny, Weekly: weekday},
		{Location: london, Weekly: all, Busy: []engine.Interval{busyC}},
	}, engine.Params{StartDate: params.StartDate, EndDate: params.EndDate, Duration: time.Hour, Location: seoul, MaxResults: 20, Now: time.Now()})
	if len(want) != len(res.Slots) {
		t.Fatalf("slots = %d, 엔진 = %d", len(res.Slots), len(want))
	}
	perDay := map[string]int{}
	for i, sl := range res.Slots {
		st, err1 := time.Parse(time.RFC3339, sl.StartAt)
		en, err2 := time.Parse(time.RFC3339, sl.EndAt)
		if err1 != nil || err2 != nil || !strings.HasSuffix(sl.StartAt, "Z") || !strings.HasSuffix(sl.EndAt, "Z") {
			t.Fatalf("slot 형식: %+v", sl)
		}
		if !st.Equal(want[i].Start) || !en.Equal(want[i].End) {
			t.Fatalf("slot %d = %v~%v, 엔진 %v~%v", i, st, en, want[i].Start, want[i].End)
		}
		local := st.In(ny)
		if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday || local.Hour() < 8 || en.In(ny).After(time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, ny)) {
			t.Fatalf("뉴욕 활동 시간 밖 slot: %v", local)
		}
		if st.Before(busyC.End) && en.After(busyC.Start) {
			t.Fatalf("Busy와 겹치는 slot: %v", st)
		}
		perDay[st.In(seoul).Format(time.DateOnly)]++
	}
	for d, n := range perDay {
		if n > 3 {
			t.Fatalf("%s에 %d개", d, n)
		}
	}
	gen, _ := time.Parse(time.RFC3339, res.GeneratedAt)
	until, _ := time.Parse(time.RFC3339, res.ValidUntil)
	if d := until.Sub(gen); d <= 0 || d > resultValidity {
		t.Fatalf("validUntil - generatedAt = %v", d)
	}
	if err := s.avail.versioner.VerifyCalculationVersion(context.Background(), s.pool, p.ID, uuid.MustParse(a.UserID), params, res.CalculationVersion, time.Now()); err != nil {
		t.Fatalf("응답 version 검증 = %v", err)
	}
}

func TestSearchEmptyResultIs200EmptyArray(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	env := envOf(t, params)
	a := s.readyMember(t, "Asia/Seoul", engine.Interval{Start: env.Start.Add(-time.Hour), End: env.End.Add(time.Hour)})
	b := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	s.join(t, b, p)
	rec := s.search(t, b, p.ID, paramsBody(params))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"slots":[]`) {
		t.Fatalf("빈 결과 = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSearchPendingStatesExposeOnlySelf(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	body := paramsBody(params)
	ws, we := defaultWindow()

	a := s.readyMember(t, "Asia/Seoul")
	b := s.signIn(t) // 활동 시간 미설정, 캘린더 ready
	s.seedCalendar(t, b.UserID, time.Now().Add(-time.Hour), ws, we)
	p := s.newParty(t, a)
	s.join(t, b, p)

	assertPending(t, s.search(t, a, p.ID, body), CodeSetupPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarReady})
	assertPending(t, s.search(t, b, p.ID, body), CodeSetupPending, SelfState{Activity: StatusUnconfigured, Calendar: SelfCalendarReady})

	s.setPrefs(t, b, "Asia/Seoul", allDay)
	s.mustSearch(t, a, p.ID, body)

	// C: 캘린더 연결 없음
	c := s.signIn(t)
	s.setPrefs(t, c, "Asia/Seoul", allDay)
	s.join(t, c, p)
	assertPending(t, s.search(t, a, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarReady})
	assertPending(t, s.search(t, c, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarNotConnected})

	// C 연결 후 revoked
	connC := s.seedCalendar(t, c.UserID, time.Now().Add(-time.Hour), ws, we)
	s.mustSearch(t, a, p.ID, body)
	for status, want := range map[string]string{
		"revoked": SelfCalendarRevoked, "denied": SelfCalendarDenied, "restricted": SelfCalendarDenied,
		"permission_required": SelfCalendarDenied, "error": SelfCalendarError, "selecting": SelfCalendarSyncing,
		"needs_source_reselection": SelfCalendarSourceNeeded, "disconnected": SelfCalendarNotConnected,
	} {
		if _, err := s.pool.Exec(context.Background(), `UPDATE calendar_connections SET source_status = $2 WHERE id = $1`, connC, status); err != nil {
			t.Fatal(err)
		}
		assertPending(t, s.search(t, c, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: want})
		assertPending(t, s.search(t, a, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarReady})
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE calendar_connections SET source_status = 'ready' WHERE id = $1`, connC); err != nil {
		t.Fatal(err)
	}
	s.mustSearch(t, a, p.ID, body)

	// active generation의 window row가 정리됐으면(24시간 prune) stale로 본다.
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM calendar_snapshot_sessions WHERE connection_id = $1`, connC); err != nil {
		t.Fatal(err)
	}
	assertPending(t, s.search(t, c, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarStale})
	s.addGeneration(t, connC, c.UserID, 2, ws, we)
	s.mustSearch(t, a, p.ID, body)

	// 시간 경과로 stale(6시간): 마지막 sync가 1시간 전이므로 5시간 뒤부터 stale이다.
	s.clock.shift(5*time.Hour + time.Minute)
	assertPending(t, s.search(t, a, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarStale})

	// 활동 시간 미설정과 캘린더 stale이 함께면 setup이 우선한다.
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM availability_preferences WHERE user_id = $1`, b.UserID); err != nil {
		t.Fatal(err)
	}
	rec := s.search(t, b, p.ID, body)
	assertPending(t, rec, CodeSetupPending, SelfState{Activity: StatusUnconfigured, Calendar: SelfCalendarStale})
	for _, other := range []string{a.UserID, c.UserID} {
		if strings.Contains(rec.Body.String(), other) {
			t.Fatal("오류 본문에 다른 멤버 ID가 있다")
		}
	}
}

func TestSearchEnvelopeMustBeCoveredByAllWindows(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	env := envOf(t, params)
	a := s.readyMember(t, "Asia/Seoul")
	b := s.signIn(t)
	s.setPrefs(t, b, "Asia/Seoul", allDay)
	ws, _ := defaultWindow()
	// b의 window는 envelope 끝 1시간 전에 끝난다.
	s.seedCalendar(t, b.UserID, time.Now().Add(-time.Hour), ws, env.End.Add(-time.Hour))
	p := s.newParty(t, a)
	s.join(t, b, p)

	rec := s.search(t, a, p.ID, paramsBody(params))
	assertError(t, rec, http.StatusBadRequest, CodeRangeInvalid)
	assertBodyKeys(t, rec.Body.Bytes(), "code", "message", "request_id")

	// 끝이 정확히 envelope 끝이면(반개구간) 포함이다.
	if _, err := s.pool.Exec(context.Background(), `
		UPDATE calendar_snapshot_sessions SET window_end = $2
		 WHERE connection_id = (SELECT id FROM calendar_connections WHERE user_id = $1)`, b.UserID, env.End); err != nil {
		t.Fatal(err)
	}
	s.mustSearch(t, a, p.ID, paramsBody(params))
	// 시작이 envelope 시작보다 늦으면 미포함이다.
	if _, err := s.pool.Exec(context.Background(), `
		UPDATE calendar_snapshot_sessions SET window_start = $2
		 WHERE connection_id = (SELECT id FROM calendar_connections WHERE user_id = $1)`, b.UserID, env.Start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertError(t, s.search(t, a, p.ID, paramsBody(params)), http.StatusBadRequest, CodeRangeInvalid)
}

func TestSearchNotFoundIsIdentical(t *testing.T) {
	s := newStack(t, false)
	a := s.readyMember(t, "Asia/Seoul")
	outsider := s.readyMember(t, "Asia/Seoul")
	former := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	s.join(t, former, p)
	s.leave(t, former, p)
	body := paramsBody(testParams(t, "Asia/Seoul"))
	// 해산된 Party: 방장이 해산한 뒤 전 방장이 검색한다.
	dp := s.newParty(t, outsider)
	var ver int64
	if err := s.pool.QueryRow(context.Background(), `SELECT version FROM parties WHERE id = $1`, dp.ID).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/parties/"+dp.ID.String()+"/disband", nil)
	req.Header.Set("Authorization", "Bearer "+outsider.AccessToken)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set("If-Match", fmt.Sprintf(`"%d"`, ver))
	drec := httptest.NewRecorder()
	s.handler.ServeHTTP(drec, req)
	if drec.Code != http.StatusOK {
		t.Fatalf("해산 = %d: %s", drec.Code, drec.Body.String())
	}

	var bodies []string
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"비멤버":       s.search(t, outsider, p.ID, body),
		"없는 Party":  s.search(t, a, uuid.New(), body),
		"떠난 멤버":     s.search(t, former, p.ID, body),
		"잘못된 UUID":  s.post(t, "/v1/parties/not-a-uuid/availability/search", a.AccessToken, "", body),
		"해산된 Party": s.search(t, outsider, dp.ID, body),
	} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d: %s", name, rec.Code, rec.Body.String())
		}
		e := decode[httpapi.ErrorResponse](t, rec)
		bodies = append(bodies, e.Code+"|"+e.Message)
		assertBodyKeys(t, rec.Body.Bytes(), "code", "message", "request_id")
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("404 본문이 다르다: %v", bodies)
		}
	}
}

func TestSearchInputValidation(t *testing.T) {
	s := newStack(t, false)
	a := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	today := engine.DateOf(time.Now(), seoul)
	ds := func(n int) string { return dateString(today.AddDays(n)) }

	ok := []string{
		`{"time_zone":"Asia/Seoul"}`,
		fmt.Sprintf(`{"start_date":%q,"time_zone":"Asia/Seoul"}`, ds(2)),
		fmt.Sprintf(`{"start_date":%q,"end_date":%q,"time_zone":"Asia/Seoul"}`, ds(1), ds(31)),
		fmt.Sprintf(`{"start_date":%q,"end_date":%q,"time_zone":"Asia/Seoul"}`, ds(79), ds(89)),
		`{"time_zone":"Asia/Seoul","duration_minutes":30,"max_results":1}`,
		`{"time_zone":"Asia/Seoul","duration_minutes":480,"max_results":20}`,
	}
	for _, body := range ok {
		if rec := s.search(t, a, p.ID, body); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", body, rec.Code, rec.Body.String())
		}
	}
	bad := []struct {
		body string
		code string
	}{
		{`{}`, CodeTimeZoneInvalid},
		{`{"time_zone":"UTC"}`, CodeTimeZoneInvalid},
		{`{"time_zone":"asia/seoul"}`, CodeTimeZoneInvalid},
		{`{"time_zone":"Etc/GMT-9"}`, CodeTimeZoneInvalid},
		{fmt.Sprintf(`{"start_date":%q,"time_zone":"Asia/Seoul"}`, ds(-1)), httpapi.CodeInvalidRequest},
		{fmt.Sprintf(`{"start_date":%q,"end_date":%q,"time_zone":"Asia/Seoul"}`, ds(3), ds(2)), httpapi.CodeInvalidRequest},
		{`{"start_date":"2026/10/30","time_zone":"Asia/Seoul"}`, httpapi.CodeInvalidRequest},
		{`{"start_date":"2026-02-30","time_zone":"Asia/Seoul"}`, httpapi.CodeInvalidRequest},
		{`{"time_zone":"Asia/Seoul","duration_minutes":0}`, httpapi.CodeInvalidRequest},
		{`{"time_zone":"Asia/Seoul","duration_minutes":20}`, httpapi.CodeInvalidRequest},
		{`{"time_zone":"Asia/Seoul","duration_minutes":45}`, httpapi.CodeInvalidRequest},
		{`{"time_zone":"Asia/Seoul","duration_minutes":510}`, httpapi.CodeInvalidRequest},
		{`{"time_zone":"Asia/Seoul","max_results":0}`, httpapi.CodeInvalidRequest},
		{`{"time_zone":"Asia/Seoul","max_results":21}`, httpapi.CodeInvalidRequest},
		{`{"time_zone":"Asia/Seoul","members":["x"]}`, httpapi.CodeInvalidRequest},
		{`{"time_zone":"Asia/Seoul","visibility":"details"}`, httpapi.CodeInvalidRequest},
		{`{"timeZone":"Asia/Seoul"}`, httpapi.CodeInvalidRequest},
		{`not json`, httpapi.CodeInvalidRequest},
		{``, httpapi.CodeInvalidRequest},
		{fmt.Sprintf(`{"start_date":%q,"end_date":%q,"time_zone":"Asia/Seoul"}`, ds(1), ds(32)), CodeRangeInvalid},
		{fmt.Sprintf(`{"start_date":%q,"end_date":%q,"time_zone":"Asia/Seoul"}`, ds(85), ds(90)), CodeRangeInvalid},
		{fmt.Sprintf(`{"start_date":%q,"time_zone":"Asia/Seoul"}`, ds(90)), CodeRangeInvalid},
	}
	for _, c := range bad {
		assertError(t, s.search(t, a, p.ID, c.body), http.StatusBadRequest, c.code)
	}
}

func TestSearchDefaults(t *testing.T) {
	now := time.Date(2026, 10, 28, 23, 0, 0, 0, time.UTC) // 서울 10/29 08:00
	p, _, _, err := parseSearchRequest(searchRequest{TimeZone: "Asia/Seoul"}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := SearchParams{StartDate: engine.Date{Year: 2026, Month: 10, Day: 29}, EndDate: engine.Date{Year: 2026, Month: 11, Day: 11},
		DurationMinutes: 60, TimeZone: "Asia/Seoul", MaxResults: 20}
	if p != want {
		t.Fatalf("기본값 = %+v, want %+v", p, want)
	}
	start := "2026-11-20"
	p, _, _, err = parseSearchRequest(searchRequest{TimeZone: "Asia/Seoul", StartDate: &start}, now)
	if err != nil || dateString(p.EndDate) != "2026-12-03" {
		t.Fatalf("시작일 지정 기본 종료일 = %v %v", p.EndDate, err)
	}
}

func TestSearchRateLimitBeforeValidation(t *testing.T) {
	s := newStack(t, true)
	a := s.signIn(t)
	partyID := uuid.New()
	for i := 0; i < 20; i++ {
		assertError(t, s.post(t, searchPath(partyID), a.AccessToken, "", `not json`), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	}
	rec := s.post(t, searchPath(partyID), a.AccessToken, "", `not json`)
	assertError(t, rec, http.StatusTooManyRequests, httpapi.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After가 없다")
	}

	// 사용자 전체 60회: 새 사용자는 Party id가 매번 달라도 60회 뒤 사용자 전체 한도로 429다.
	b := s.signIn(t)
	for i := 0; i < 60; i++ {
		rec := s.post(t, searchPath(uuid.New()), b.AccessToken, "", `{}`)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("%d번째에서 이미 429", i+1)
		}
	}
	assertError(t, s.post(t, searchPath(uuid.New()), b.AccessToken, "", `{}`), http.StatusTooManyRequests, httpapi.CodeRateLimited)
}

func TestSearchCacheInvalidation(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	body := paramsBody(params)
	a := s.readyMember(t, "Asia/Seoul")
	b := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	s.join(t, b, p)

	hits := func() int64 { return s.avail.cache.hitCount() }
	first := s.mustSearch(t, a, p.ID, body)
	second := s.mustSearch(t, b, p.ID, body)
	if hits() != 1 || second.CalculationVersion != first.CalculationVersion || second.GeneratedAt != first.GeneratedAt {
		t.Fatalf("두 번째 검색은 cache hit여야 한다: hits=%d", hits())
	}
	expectMiss := func(name string) searchResult {
		t.Helper()
		before := hits()
		r := s.mustSearch(t, a, p.ID, body)
		if hits() != before {
			t.Fatalf("%s 뒤 cache hit", name)
		}
		if r.CalculationVersion == first.CalculationVersion {
			t.Fatalf("%s 뒤 같은 version", name)
		}
		first = r
		return r
	}
	// 다른 파라미터는 다른 key다.
	other := params
	other.DurationMinutes = 90
	before := hits()
	s.mustSearch(t, a, p.ID, paramsBody(other))
	if hits() != before {
		t.Fatal("다른 파라미터가 cache hit")
	}

	s.setPrefs(t, b, "Asia/Seoul", `[{"weekday":1,"start_minute":540,"end_minute":1320},{"weekday":3,"start_minute":540,"end_minute":1320}]`)
	expectMiss("활동 시간 변경")

	var connB uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM calendar_connections WHERE user_id = $1`, b.UserID).Scan(&connB); err != nil {
		t.Fatal(err)
	}
	ws, we := defaultWindow()
	s.addGeneration(t, connB, b.UserID, 2, ws, we)
	expectMiss("generation 변경")

	c := s.readyMember(t, "Asia/Seoul")
	s.join(t, c, p)
	expectMiss("멤버 가입")

	// 변화 없으면 hit, validUntil이 지나면 miss
	before = hits()
	s.mustSearch(t, a, p.ID, body)
	if hits() != before+1 {
		t.Fatal("변화 없는 재검색이 miss")
	}
	s.clock.shift(resultValidity + time.Second)
	expectMiss("validUntil 경과")
}

func TestResultCacheBoundAndExpiry(t *testing.T) {
	c := newResultCache(2)
	now := time.Now()
	k := func(n byte) cacheKey { return cacheKey{tuple: [32]byte{n}} }
	res := cachedResult{validUntil: now.Add(time.Minute)}
	c.put(k(1), res)
	c.put(k(2), res)
	if _, ok := c.get(k(1), now); !ok {
		t.Fatal("k1 miss")
	}
	c.put(k(3), res) // k2가 가장 오래 안 쓰였다
	if _, ok := c.get(k(2), now); ok {
		t.Fatal("LRU 제거 실패")
	}
	if _, ok := c.get(k(1), now.Add(time.Minute)); ok {
		t.Fatal("validUntil에 도달한 항목을 돌려줬다")
	}
	if len(c.items) != 1 || c.ll.Len() != 1 {
		t.Fatalf("항목 수 = %d", len(c.items))
	}
}

// nextWeekday는 [from, from+13] 안의 첫 weekday 날짜다.
func nextWeekday(from engine.Date, wd int) engine.Date {
	for i := 0; i < 14; i++ {
		if d := from.AddDays(i); d.ISOWeekday() == wd {
			return d
		}
	}
	panic("unreachable")
}

func TestSearchMidnightCrossingThroughDB(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	seoul, _ := time.LoadLocation("Asia/Seoul")
	mon := nextWeekday(params.StartDate, 1)
	monAt := func(h, m int) time.Time { return time.Date(mon.Year, mon.Month, mon.Day, h, m, 0, 0, seoul) }

	// A: 월 22:00 ~ 화 02:00 (입력은 자정을 넘는 하나, 저장은 월 [1320,1440] + 화 [0,120])
	a := s.signIn(t)
	s.setPrefs(t, a, "Asia/Seoul", `[{"weekday":1,"start_minute":1320,"end_minute":1560}]`)
	var rows int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM availability_weekly_intervals WHERE user_id = $1`, a.UserID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("저장 구간 = %d (%v), want 2", rows, err)
	}
	ws, we := defaultWindow()
	s.seedCalendar(t, a.UserID, time.Now().Add(-time.Hour), ws, we)
	// B: 매일 종일, 월 22:00~23:30 Busy
	b := s.readyMember(t, "Asia/Seoul", engine.Interval{Start: monAt(22, 0).UTC(), End: monAt(23, 30).UTC()})
	p := s.newParty(t, a)
	s.join(t, b, p)

	params.EndDate = mon.AddDays(1)
	if params.EndDate.Before(params.StartDate) {
		t.Fatal("범위 계산 오류")
	}
	res := s.mustSearch(t, a, p.ID, paramsBody(params))
	want := []string{
		monAt(23, 30).UTC().Format(time.RFC3339) + "/" + monAt(24, 30).UTC().Format(time.RFC3339),
		monAt(24, 0).UTC().Format(time.RFC3339) + "/" + monAt(25, 0).UTC().Format(time.RFC3339),
		monAt(24, 30).UTC().Format(time.RFC3339) + "/" + monAt(25, 30).UTC().Format(time.RFC3339),
		monAt(25, 0).UTC().Format(time.RFC3339) + "/" + monAt(26, 0).UTC().Format(time.RFC3339),
	}
	var got []string
	for _, sl := range res.Slots {
		st, _ := time.Parse(time.RFC3339, sl.StartAt)
		if st.Before(monAt(0, 0)) {
			continue // 앞선 주의 월요일 결과는 보지 않는다
		}
		got = append(got, sl.StartAt+"/"+sl.EndAt)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("slots =\n%v\nwant\n%v", got, want)
	}
}

func TestSearchVisibilityDoesNotChangeResults(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	env := envOf(t, params)
	busy := []engine.Interval{
		{Start: env.Start.Add(2 * time.Hour), End: env.Start.Add(5 * time.Hour)},
		{Start: env.Start.Add(30 * time.Hour), End: env.Start.Add(31 * time.Hour)},
	}
	a := s.readyMember(t, "Asia/Seoul")
	b := s.readyMember(t, "Asia/Seoul", busy...)
	p := s.newParty(t, a)
	s.join(t, b, p)

	var baseline []slotResponse
	for i, level := range []string{"details", "busyOnly", "hidden"} {
		if _, err := s.pool.Exec(context.Background(), `
			UPDATE party_visibility_settings SET visibility_level = $3, share_location = false, version = version + 1
			 WHERE party_id = $1 AND user_id = $2`, p.ID, b.UserID, level); err != nil {
			t.Fatal(err)
		}
		// 공개 수준은 tuple에 없으므로 캐시가 결과를 덮지 않게 매번 비운다.
		s.avail.cache = newResultCache(searchCacheSize)
		res := s.mustSearch(t, a, p.ID, paramsBody(params))
		if s.avail.cache.hitCount() != 0 {
			t.Fatal("cache를 거쳤다")
		}
		if i == 0 {
			baseline = res.Slots
			if len(baseline) == 0 {
				t.Fatal("결과가 비었다")
			}
			continue
		}
		if fmt.Sprint(res.Slots) != fmt.Sprint(baseline) {
			t.Fatalf("%s 결과가 다르다", level)
		}
	}
}

func TestSearchPrivacyOfBodiesAndLogs(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	a := s.readyMember(t, "Asia/Seoul")
	b := s.readyMember(t, "Asia/Seoul")
	c := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	s.join(t, b, p)
	s.join(t, c, p)

	// 준비 과정(가입 등)의 로그는 이 검사 대상이 아니다. 검색 요청의 로그만 본다.
	s.logs.Reset()
	rec := s.search(t, a, p.ID, paramsBody(params))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	res := decode[searchResult](t, rec)
	body := rec.Body.String()
	for _, banned := range []string{b.UserID, c.UserID, "member", "count", "visibility", "busy", "calendar", "cause"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(banned)) {
			t.Fatalf("응답에 %q가 있다: %s", banned, body)
		}
	}
	// 오류 경로도 남긴다.
	s.clock.shift(6 * time.Hour)
	s.search(t, a, p.ID, paramsBody(params))
	logs := s.logs.String()
	if !strings.Contains(logs, `"action":"availability.search"`) || !strings.Contains(logs, `"cache_hit":false`) {
		t.Fatalf("검색 지표 로그가 없다: %s", logs)
	}
	for _, sl := range res.Slots {
		if strings.Contains(logs, sl.StartAt) || strings.Contains(logs, sl.EndAt) {
			t.Fatal("로그에 slot 시각이 있다")
		}
	}
	for _, banned := range []string{dateString(params.StartDate), dateString(params.EndDate), "Asia/Seoul", p.ID.String(), b.UserID, c.UserID, res.CalculationVersion} {
		if strings.Contains(logs, banned) {
			t.Fatalf("로그에 %q가 있다", banned)
		}
	}
}

func TestSearchTimeoutMapsTo503(t *testing.T) {
	s := newStack(t, false)
	a := s.readyMember(t, "Asia/Seoul")
	p := s.newParty(t, a)
	params := testParams(t, "Asia/Seoul")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := readSnapshotTx(ctx, s.pool, p.ID, uuid.MustParse(a.UserID), params, envOf(t, params), time.Now())
	if err == nil {
		t.Fatal("기한이 지난 context에서 snapshot이 성공했다")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if code := s.avail.writeSearchError(rec, req, err); code != CodeUnavailable || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("timeout = %d %s: %s", rec.Code, code, rec.Body.String())
	}
	assertBodyKeys(t, rec.Body.Bytes(), "code", "message", "request_id")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After가 없다")
	}
}

// 오늘 검색: 멤버 window는 그 멤버 시간대의 오늘 00:00부터라 요청 시간대 오늘 00:00보다 늦을 수 있다.
// 이미 지난 [envelope 시작, now)는 후보가 없으므로 포함 검사의 하한은 max(envelope 시작, now)다.
func TestSearchTodayAcceptsWindowStartingBeforeNow(t *testing.T) {
	s := newStack(t, false)
	params := testParams(t, "Asia/Seoul")
	params.StartDate = params.StartDate.AddDays(-2) // 오늘
	params.EndDate = params.StartDate.AddDays(13)
	env := envOf(t, params)
	now := time.Now()
	a := s.readyMember(t, "Asia/Seoul")
	b := s.signIn(t)
	s.setPrefs(t, b, "Asia/Seoul", allDay)
	_, we := defaultWindow()
	mid := env.Start.Add(now.Sub(env.Start) / 2)
	s.seedCalendar(t, b.UserID, now.Add(-time.Hour), mid, we)
	p := s.newParty(t, a)
	s.join(t, b, p)

	s.mustSearch(t, a, p.ID, `{"time_zone":"Asia/Seoul"}`)
	s.mustSearch(t, a, p.ID, paramsBody(params))

	// window가 now보다 늦게 시작하면 앞으로 올 구간이 빠졌으므로 여전히 400이다.
	if _, err := s.pool.Exec(context.Background(), `
		UPDATE calendar_snapshot_sessions SET window_start = $2
		 WHERE connection_id = (SELECT id FROM calendar_connections WHERE user_id = $1)`, b.UserID, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertError(t, s.search(t, a, p.ID, `{"time_zone":"Asia/Seoul"}`), http.StatusBadRequest, CodeRangeInvalid)
}

// 재동기화 중(syncing)이어도 이전 generation이 fresh면 그 generation으로 계산한다. 진행 중 업로드의
// staging facts는 섞이지 않고, 업로드가 complete되어 generation이 바뀌면 이전 version은 stale이다.
func TestSearchDuringResyncUsesActiveGeneration(t *testing.T) {
	s := newStack(t, false)
	ctx := context.Background()
	params := testParams(t, "Asia/Seoul")
	env := envOf(t, params)
	body := paramsBody(params)
	a := s.readyMember(t, "Asia/Seoul")
	b := s.readyMember(t, "Asia/Seoul", engine.Interval{Start: env.Start.Add(10 * time.Hour), End: env.Start.Add(12 * time.Hour)})
	p := s.newParty(t, a)
	s.join(t, b, p)
	before := s.mustSearch(t, a, p.ID, body)
	if len(before.Slots) == 0 {
		t.Fatal("결과가 비었다")
	}

	// b가 재동기화를 시작한다: 연결 syncing, 열린 session revision 2와 envelope 전체를 덮는 staging fact.
	var connB uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT id FROM calendar_connections WHERE user_id = $1`, b.UserID).Scan(&connB); err != nil {
		t.Fatal(err)
	}
	ws, we := defaultWindow()
	sessionID := uuid.New()
	key := uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE calendar_connections SET source_status = 'syncing' WHERE id = $1`, []any{connB}},
		{`INSERT INTO calendar_snapshot_sessions (id, connection_id, revision, window_start, window_end, expected_pages)
		  VALUES ($1, $2, 2, $3, $4, 1)`, []any{sessionID, connB, ws, we}},
		{`INSERT INTO calendar_snapshot_pages (session_id, page, fact_count) VALUES ($1, 0, 1)`, []any{sessionID}},
		{`INSERT INTO calendar_snapshot_facts_staging (session_id, page, source_event_key, start_at, end_at, all_day, time_zone, availability)
		  VALUES ($1, 0, $2, $3, $4, false, 'Asia/Seoul', 'busy')`, []any{sessionID, key[:], env.Start.Add(-time.Hour), env.End.Add(time.Hour)}},
	} {
		if _, err := s.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	s.avail.cache = newResultCache(searchCacheSize) // 캐시가 아니라 계산으로 같은 결과인지 본다
	during := s.mustSearch(t, a, p.ID, body)
	if fmt.Sprint(during.Slots) != fmt.Sprint(before.Slots) {
		t.Fatal("업로드 중 staging facts가 결과에 섞였다")
	}
	if s.avail.cache.hitCount() != 0 {
		t.Fatal("cache를 거쳤다")
	}
	// syncing 전환만으로는 version이 바뀌지 않는다(generation 기준).
	if err := s.verify(t, p.ID, a, params, before.CalculationVersion, time.Now()); err != nil {
		t.Fatalf("syncing 중 이전 version 검증 = %v", err)
	}

	// complete: staging을 generation 2 facts로 승격하고 연결을 ready·generation 2로 바꾼다(snapshot.go와 같은 변화).
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM calendar_busy_facts WHERE connection_id = $1`, []any{connB}},
		{`INSERT INTO calendar_busy_facts (connection_id, source_event_key, user_id, generation, start_at, end_at, all_day, time_zone, availability)
		  SELECT $1, source_event_key, $3, 2, start_at, end_at, all_day, time_zone, availability
		    FROM calendar_snapshot_facts_staging WHERE session_id = $2`, []any{connB, sessionID, b.UserID}},
		{`UPDATE calendar_snapshot_sessions SET status = 'completed', generation = 2, fact_count = 1, completed_at = now() WHERE id = $1`, []any{sessionID}},
		{`UPDATE calendar_connections SET source_status = 'ready', active_generation = 2, active_generation_usable = true, last_completed_sync_at = now() WHERE id = $1`, []any{connB}},
	} {
		if _, err := s.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.verify(t, p.ID, a, params, before.CalculationVersion, time.Now()); !errors.Is(err, ErrResultStale) {
		t.Fatalf("complete 뒤 이전 version = %v, want stale", err)
	}
	after := s.mustSearch(t, a, p.ID, body)
	if len(after.Slots) != 0 || after.CalculationVersion == before.CalculationVersion {
		t.Fatalf("새 generation 결과 = %d개", len(after.Slots))
	}

	// 다시 syncing인데 이전 generation이 stale이면 pending이고 본인은 syncing이다.
	if _, err := s.pool.Exec(ctx, `UPDATE calendar_connections SET source_status = 'syncing', last_completed_sync_at = now() - interval '7 hours' WHERE id = $1`, connB); err != nil {
		t.Fatal(err)
	}
	assertPending(t, s.search(t, b, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarSyncing})
	assertPending(t, s.search(t, a, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarReady})
}

// 첫 연결의 syncing(이전 정상 generation 없음)은 계속 pending이다.
func TestSearchFirstSyncingIsPending(t *testing.T) {
	s := newStack(t, false)
	body := paramsBody(testParams(t, "Asia/Seoul"))
	a := s.readyMember(t, "Asia/Seoul")
	c := s.signIn(t)
	s.setPrefs(t, c, "Asia/Seoul", allDay)
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO calendar_connections (id, user_id, source_status, active_generation, time_zone)
		VALUES ($1, $2, 'syncing', 0, 'Asia/Seoul')`, uuid.New(), c.UserID); err != nil {
		t.Fatal(err)
	}
	p := s.newParty(t, a)
	s.join(t, c, p)
	assertPending(t, s.search(t, c, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarSyncing})
	assertPending(t, s.search(t, a, p.ID, body), CodeSyncPending, SelfState{Activity: StatusConfigured, Calendar: SelfCalendarReady})
}
