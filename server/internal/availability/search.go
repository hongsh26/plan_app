package availability

// A4: POST /v1/parties/{party_id}/availability/search (설계 6 §4, §7.2, §9, §10, §12).

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/availability/engine"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/ratelimit"
)

const (
	defaultSearchDays   = 14
	maxSearchDays       = 31
	snapshotHorizonDays = 90
	defaultDuration     = 60
	minDuration         = 30
	maxDuration         = 480
	defaultMaxResults   = 20
	maxMaxResults       = 20
	// searchTimeout은 snapshot 읽기와 계산 전체의 기한이다. 31일·20명 계산은 수 ms이고 snapshot은
	// 쿼리 다섯 개라 3초면 정상 경로에 충분하다. 넘기면 부분 결과 없이 503이다(§10).
	searchTimeout = 3 * time.Second
	// searchCacheSize는 프로세스당 캐시 항목 상한이다. 항목은 슬롯 20개 이하라 작다.
	searchCacheSize = 1024
)

type searchRequest struct {
	StartDate       *string `json:"start_date"`
	EndDate         *string `json:"end_date"`
	DurationMinutes *int    `json:"duration_minutes"`
	TimeZone        string  `json:"time_zone"`
	MaxResults      *int    `json:"max_results"`
}

type slotResponse struct {
	StartAt string `json:"start_at"`
	EndAt   string `json:"end_at"`
}

type searchResponse struct {
	RequestID          string         `json:"request_id"`
	Slots              []slotResponse `json:"slots"`
	CalculationVersion string         `json:"calculation_version"`
	GeneratedAt        string         `json:"generated_at"`
	ValidUntil         string         `json:"valid_until"`
}

// pendingResponse는 409 준비 미완료 응답이다. self에는 호출자 본인 상태만 있다.
type pendingResponse struct {
	httpapi.ErrorResponse
	Self SelfState `json:"self"`
}

var errBadSearchInput = errors.New("availability: 검색 입력이 올바르지 않다")

// parseSearchRequest는 기본값을 채우고 §4 한도를 검증한다. code는 실패 시 응답 code다.
func parseSearchRequest(req searchRequest, now time.Time) (SearchParams, *time.Location, string, error) {
	if !ValidTimeZone(req.TimeZone) {
		return SearchParams{}, nil, CodeTimeZoneInvalid, errBadSearchInput
	}
	loc, err := time.LoadLocation(req.TimeZone)
	if err != nil {
		return SearchParams{}, nil, CodeTimeZoneInvalid, errBadSearchInput
	}
	today := engine.DateOf(now, loc)
	p := SearchParams{StartDate: today, DurationMinutes: defaultDuration, TimeZone: req.TimeZone, MaxResults: defaultMaxResults}
	if req.StartDate != nil {
		if p.StartDate, err = parseDate(*req.StartDate); err != nil {
			return SearchParams{}, nil, httpapi.CodeInvalidRequest, err
		}
	}
	p.EndDate = p.StartDate.AddDays(defaultSearchDays - 1)
	if req.EndDate != nil {
		if p.EndDate, err = parseDate(*req.EndDate); err != nil {
			return SearchParams{}, nil, httpapi.CodeInvalidRequest, err
		}
	}
	if req.DurationMinutes != nil {
		p.DurationMinutes = *req.DurationMinutes
	}
	if req.MaxResults != nil {
		p.MaxResults = *req.MaxResults
	}
	switch {
	case p.StartDate.Before(today), p.EndDate.Before(p.StartDate),
		p.DurationMinutes < minDuration, p.DurationMinutes > maxDuration, p.DurationMinutes%slotMinutes != 0,
		p.MaxResults < 1, p.MaxResults > maxMaxResults:
		return SearchParams{}, nil, httpapi.CodeInvalidRequest, errBadSearchInput
	case p.StartDate.AddDays(maxSearchDays - 1).Before(p.EndDate), today.AddDays(snapshotHorizonDays).Before(p.EndDate):
		return SearchParams{}, nil, CodeRangeInvalid, errBadSearchInput
	}
	return p, loc, "", nil
}

func parseDate(s string) (engine.Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return engine.Date{}, errBadSearchInput
	}
	return engine.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if !h.enforceSearchLimit(w, r, p.UserID, r.PathValue("party_id")) {
		return
	}
	outcome := "error"
	var (
		params   SearchParams
		slots    = -1
		cacheHit = false
	)
	defer func() { h.logSearch(r, outcome, params, slots, cacheHit) }()

	partyID, err := uuid.Parse(r.PathValue("party_id"))
	if err != nil {
		outcome = httpapi.CodeNotFound
		writeSearchNotFound(w, r)
		return
	}
	var req searchRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		outcome = httpapi.CodeInvalidRequest
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	now := h.now()
	params, loc, code, err := parseSearchRequest(req, now)
	if err != nil {
		outcome = code
		httpapi.WriteError(w, r, http.StatusBadRequest, code, searchInputMessage(code))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	ep := engine.Params{
		StartDate: params.StartDate, EndDate: params.EndDate, Duration: time.Duration(params.DurationMinutes) * time.Minute,
		Location: loc, MaxResults: params.MaxResults, Now: now,
	}
	env := engine.RequestEnvelope(params.StartDate, params.EndDate, loc)
	snap, err := readSnapshotTx(ctx, h.pool, partyID, p.UserID, params, env, now)
	if err != nil {
		outcome = h.writeSearchError(w, r, err)
		return
	}

	// 캐시 key는 매 요청 새로 읽은 snapshot에서 만든다. snapshot을 건너뛰지 않으므로 correctness는 캐시에
	// 의존하지 않는다. 캐시는 엔진 계산만 아낀다. 가장 이른 시작 grid가 다르면(lead time 경계를 지남) 다른 key다.
	ck := cacheKey{party: partyID, tuple: sha256.Sum256(snap.state.canonical), earliest: engine.EarliestStart(ep).UnixNano()}
	res, ok := h.cache.get(ck, now)
	if ok {
		cacheHit = true
	} else {
		computed, err := computeWithDeadline(ctx, snap.members, ep)
		if err != nil {
			outcome = h.writeSearchError(w, r, err)
			return
		}
		generated := now.UTC().Truncate(time.Second)
		validUntil := generated.Add(resultValidity)
		if fu := snap.state.freshUntil.UTC().Truncate(time.Second); fu.Before(validUntil) {
			validUntil = fu
		}
		if !generated.Before(validUntil) {
			// freshness가 이 1초 안에 끝났다. 쓸 수 없는 결과를 주지 않고 재검색하게 한다(다음 검색은 sync pending).
			outcome = h.writeSearchError(w, r, errChangedRetry)
			return
		}
		res = cachedResult{slots: computed, version: h.versioner.token(snap.state.canonical, validUntil), generatedAt: generated, validUntil: validUntil}
		h.cache.put(ck, res)
	}

	out := searchResponse{
		RequestID:          httpapi.RequestID(r.Context()).String(),
		Slots:              make([]slotResponse, len(res.slots)),
		CalculationVersion: res.version,
		GeneratedAt:        res.generatedAt.Format(time.RFC3339),
		ValidUntil:         res.validUntil.Format(time.RFC3339),
	}
	for i, s := range res.slots {
		out.Slots[i] = slotResponse{StartAt: s.Start.UTC().Format(time.RFC3339), EndAt: s.End.UTC().Format(time.RFC3339)}
	}
	outcome, slots = "ok", len(res.slots)
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// computeWithDeadline은 엔진을 기한 안에서 실행한다. 기한을 넘기면 결과를 버린다(부분 결과 없음).
func computeWithDeadline(ctx context.Context, members []engine.Member, p engine.Params) ([]engine.Interval, error) {
	done := make(chan []engine.Interval, 1)
	go func() { done <- engine.Search(members, p) }()
	select {
	case out := <-done:
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *Handler) enforceSearchLimit(w http.ResponseWriter, r *http.Request, userID uuid.UUID, rawPartyID string) bool {
	d, ok := ratelimit.Enforce(w, r, h.limiter, ratelimit.ModeClosed,
		ratelimit.Rule{
			Scope:   ratelimit.ScopeAvailabilitySearch,
			Limit:   ratelimit.LimitAvailabilitySearch.Count,
			Window:  ratelimit.LimitAvailabilitySearch.Window,
			Subject: "user:" + userID.String() + ":party:" + rawPartyID,
		},
		ratelimit.UserRule(ratelimit.ScopeAvailabilitySearchAll, ratelimit.LimitAvailabilitySearchAll, userID))
	if !ok {
		if len(d.Exceeded) > 0 {
			ratelimit.RecordExceeded(r.Context(), h.pool, userID, d)
		}
		return false
	}
	return true
}

func searchInputMessage(code string) string {
	switch code {
	case CodeTimeZoneInvalid:
		return "time_zone은 Area/City 형식의 지역 IANA 시간대여야 한다"
	case CodeRangeInvalid:
		return "검색 기간이 허용 범위를 벗어난다"
	default:
		return "검색 조건이 올바르지 않다"
	}
}

func writeSearchNotFound(w http.ResponseWriter, r *http.Request) {
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
}

// writeSearchError는 오류를 응답하고 로그용 결과 code를 돌려준다. 다른 멤버의 수·ID·원인을 싣지 않는다.
func (h *Handler) writeSearchError(w http.ResponseWriter, r *http.Request, err error) string {
	var pending *PendingError
	switch {
	case errors.Is(err, errSearchNotFound):
		writeSearchNotFound(w, r)
		return httpapi.CodeNotFound
	case errors.As(err, &pending):
		msg := "그룹 준비를 기다리는 중이다"
		httpapi.WriteJSON(w, http.StatusConflict, pendingResponse{
			ErrorResponse: httpapi.ErrorResponse{Code: pending.Code, Message: msg, RequestID: httpapi.RequestID(r.Context()).String()},
			Self:          pending.Self,
		})
		return pending.Code
	case errors.Is(err, errRangeNotCovered):
		httpapi.WriteError(w, r, http.StatusBadRequest, CodeRangeInvalid, searchInputMessage(CodeRangeInvalid))
		return CodeRangeInvalid
	case errors.Is(err, errChangedRetry):
		httpapi.WriteError(w, r, http.StatusConflict, CodeChangedRetry, "검색 중 그룹 상태가 바뀌었다. 다시 검색해야 한다")
		return CodeChangedRetry
	case errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "1")
		httpapi.WriteError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "가능 시간을 지금 계산할 수 없다. 잠시 후 다시 시도해야 한다")
		return CodeUnavailable
	default:
		// 오류 원문에는 SQL과 값이 섞일 수 있다. 분류만 남긴다.
		h.logger.ErrorContext(r.Context(), "가능 시간 검색 처리 실패",
			slog.String("request_id", httpapi.RequestID(r.Context()).String()),
			slog.String("action", "availability.search"),
			slog.String("result", "failure"),
			slog.String("error_type", httpapi.ErrorType(err)),
		)
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
		return httpapi.CodeInternal
	}
}

// logSearch는 §12 허용 지표만 남긴다: 결과 code, 기간·길이 bucket, 슬롯 수, 캐시 hit.
// 검색 날짜·슬롯 시각·Party·멤버 ID·시간대는 남기지 않는다.
func (h *Handler) logSearch(r *http.Request, outcome string, p SearchParams, slots int, cacheHit bool) {
	attrs := []any{
		slog.String("request_id", httpapi.RequestID(r.Context()).String()),
		slog.String("action", "availability.search"),
		slog.String("result_code", outcome),
	}
	if p.DurationMinutes > 0 {
		attrs = append(attrs,
			slog.String("duration_bucket", durationBucket(p.DurationMinutes)),
			slog.String("range_days_bucket", rangeBucket(p.StartDate, p.EndDate)))
	}
	if slots >= 0 {
		attrs = append(attrs, slog.Int("slot_count", slots), slog.Bool("cache_hit", cacheHit))
	}
	h.logger.InfoContext(r.Context(), "availability search", attrs...)
}

func durationBucket(m int) string {
	switch {
	case m <= 30:
		return "30"
	case m <= 60:
		return "60"
	case m <= 120:
		return "90-120"
	default:
		return "150+"
	}
}

func rangeBucket(start, end engine.Date) string {
	days := int(time.Date(end.Year, end.Month, end.Day, 0, 0, 0, 0, time.UTC).Sub(
		time.Date(start.Year, start.Month, start.Day, 0, 0, 0, 0, time.UTC)).Hours()/24) + 1
	switch {
	case days <= 7:
		return "1-7"
	case days <= 14:
		return "8-14"
	default:
		return "15-31"
	}
}

// --- 캐시 ---

type cacheKey struct {
	party    uuid.UUID
	tuple    [sha256.Size]byte
	earliest int64
}

type cachedResult struct {
	slots       []engine.Interval
	version     string
	generatedAt time.Time
	validUntil  time.Time
}

// resultCache는 프로세스 메모리 LRU다. 항목은 자기 validUntil까지만 쓴다(최대 5분, §9).
type resultCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[cacheKey]*list.Element
	hits  int64
}

type cacheEntry struct {
	key cacheKey
	res cachedResult
}

func newResultCache(max int) *resultCache {
	return &resultCache{max: max, ll: list.New(), items: map[cacheKey]*list.Element{}}
}

func (c *resultCache) get(k cacheKey, now time.Time) (cachedResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return cachedResult{}, false
	}
	e := el.Value.(*cacheEntry)
	if !now.Before(e.res.validUntil) {
		c.ll.Remove(el)
		delete(c.items, k)
		return cachedResult{}, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return e.res, true
}

func (c *resultCache) put(k cacheKey, res cachedResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		el.Value.(*cacheEntry).res = res
		c.ll.MoveToFront(el)
		return
	}
	c.items[k] = c.ll.PushFront(&cacheEntry{key: k, res: res})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*cacheEntry).key)
	}
}

func (c *resultCache) hitCount() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}
