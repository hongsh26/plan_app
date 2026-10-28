// Package availability는 설계 6(docs/availability_search_design.md)의 서버 쪽 공통 가능 시간이다.
// 지금은 A1 사용자별 활동 시간 저장·조회만 있다. 계산 엔진은 engine 하위 패키지에 있다.
//
// 개인정보 규칙(§11, §12): 원시 활동 interval과 time zone은 본인 API 응답에만 싣는다. 다른 사용자
// sync에 발행하지 않고, 본인 sync 변경에도 interval·time zone을 넣지 않으며, 로그와 오류 메시지에
// 남기지 않는다.
package availability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/ratelimit"

	// 시간대 검증에 IANA 데이터베이스가 필요하다. 배포 이미지에 zoneinfo가 없어도 동작하게 내장한다.
	_ "time/tzdata"
)

// ReplayedHeader는 저장된 결과를 돌려준 재전송 표시다.
const ReplayedHeader = "Idempotent-Replayed"

const (
	StatusUnconfigured = "unconfigured"
	StatusConfigured   = "configured"

	// entityPreference는 본인에게만 보내는 sync 무효화 entity다. payload에 interval이 없다.
	entityPreference = "availability_preference"

	minutesPerDay = 1440
	slotMinutes   = 30
	// maxIntervalsPerDay는 정규화·병합 뒤 요일별 최대 interval 수다(§3.2).
	maxIntervalsPerDay = 3
	// maxInputIntervals는 요청 하나의 입력 interval 상한이다. 30분 칸 UI가 칸마다 보내도
	// 한 주(7일 × 48칸)를 넘지 않는다. 병합 전 입력이므로 요일별 3개보다 넉넉하다.
	maxInputIntervals = 7 * minutesPerDay / slotMinutes
	maxTimeZoneLen    = 64
)

var errInvalidInput = errors.New("availability: 입력이 올바르지 않다")

type Handler struct {
	pool    *pgxpool.Pool
	logger  *slog.Logger
	now     func() time.Time
	limiter ratelimit.Allower
}

type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Now    func() time.Time
	// Limiter는 docs/rate_limit_design.md의 요청 제한이다. nil이면 제한하지 않는다.
	Limiter ratelimit.Allower
}

func NewHandler(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{pool: d.Pool, logger: d.Logger, now: d.Now, limiter: d.Limiter}
}

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/availability/preferences/me", require(http.HandlerFunc(h.getPreferences)))
	mux.Handle("PUT /v1/availability/preferences/me", require(http.HandlerFunc(h.putPreferences)))
}

// Interval은 저장된 정규형 활동 구간이다. 분은 사용자 time zone의 지역 시각이며 end_minute ≤ 1440이다.
type Interval struct {
	Weekday     int `json:"weekday"`
	StartMinute int `json:"start_minute"`
	EndMinute   int `json:"end_minute"`
}

// intervalInput은 PUT 입력이다. 0이 유효한 값(자정)이라 누락과 구분하려고 포인터를 쓴다.
// 자정을 넘는 구간은 end_minute를 1440보다 크게(최대 2880) 보낸다. 예: 월 22:00~02:00 =
// {weekday:1, start_minute:1320, end_minute:1560}.
type intervalInput struct {
	Weekday     *int `json:"weekday"`
	StartMinute *int `json:"start_minute"`
	EndMinute   *int `json:"end_minute"`
}

type putPreferencesRequest struct {
	TimeZone        string          `json:"time_zone"`
	WeeklyIntervals []intervalInput `json:"weekly_intervals"`
}

type preferencesResponse struct {
	Status          string     `json:"status"`
	TimeZone        *string    `json:"time_zone"`
	Version         int64      `json:"version"`
	WeeklyIntervals []Interval `json:"weekly_intervals"`
	UpdatedAt       *string    `json:"updated_at"`
}

type preferencesEnvelope struct {
	RequestID   string              `json:"request_id"`
	Preferences preferencesResponse `json:"preferences"`
}

// preference는 저장 상태다. row가 없으면 Status=unconfigured, Version=0이다.
type preference struct {
	Status    string
	TimeZone  *string
	Version   int64
	Intervals []Interval
	UpdatedAt *time.Time
}

func (h *Handler) getPreferences(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	// 조회는 사용자 전역 설정 하나라 남용 표면이 작다. 제한 장치 장애로 설정 화면이 막히지 않게 open이다.
	if _, ok := ratelimit.Enforce(w, r, h.limiter, ratelimit.ModeOpen,
		ratelimit.UserRule(ratelimit.ScopeAvailabilityPrefRead, ratelimit.LimitAvailabilityPrefRead, p.UserID)); !ok {
		return
	}
	pref, err := loadPreference(r.Context(), h.pool, p.UserID, false)
	if err != nil {
		h.writeDomainError(w, r, "availability.preferences.get", err)
		return
	}
	writePreference(w, r, pref, http.StatusOK)
}

func (h *Handler) putPreferences(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if _, ok := ratelimit.Enforce(w, r, h.limiter, ratelimit.ModeClosed,
		ratelimit.UserRule(ratelimit.ScopeAvailabilityPrefWrite, ratelimit.LimitAvailabilityPrefWrite, p.UserID)); !ok {
		return
	}
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	// If-Match가 없으면 create-only다(calendar PUT /v1/calendar/connection과 같은 규약). 미설정
	// 사용자는 version 0이고 mutation.ExpectedVersion은 1 이상만 받기 때문이다.
	createOnly := r.Header.Get("If-Match") == ""
	if !createOnly {
		var err error
		if prep.ExpectedVersion, err = mutation.ExpectedVersion(r); err != nil {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error())
			return
		}
	}
	var req putPreferencesRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	if !ValidTimeZone(req.TimeZone) {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "time_zone은 지역 IANA 시간대여야 한다")
		return
	}
	intervals, err := normalizeIntervals(req.WeeklyIntervals)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "활동 시간 구간이 올바르지 않다")
		return
	}

	out, ok := h.run(w, r, p, prep, "availability.preferences.put", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		created := false
		if createOnly {
			tag, err := tx.Exec(ctx, `
				INSERT INTO availability_preferences (user_id, status, time_zone)
				VALUES ($1, 'configured', $2)
				ON CONFLICT (user_id) DO NOTHING`, p.UserID, req.TimeZone)
			if err != nil {
				return mutation.Result{}, err
			}
			created = tag.RowsAffected() == 1
		}
		// 같은 사용자의 PUT은 이 row 잠금으로 직렬화된다. 동시 create-only는 위 INSERT의 unique
		// 대기 뒤 새 문장 snapshot에서 상대가 만든 row를 본다.
		current, err := loadPreference(ctx, tx, p.UserID, true)
		if err != nil {
			return mutation.Result{}, err
		}
		if current.Version == 0 {
			// If-Match가 있는데 설정이 없다. 낡은 version이다(현재 0).
			return mutation.Result{}, &mutation.VersionConflictError{Current: 0}
		}
		if createOnly && !created {
			// 이미 있는 설정을 create-only로 덮어쓰지 않는다.
			return mutation.Result{}, &mutation.VersionConflictError{Current: current.Version}
		}
		if !createOnly {
			if err := mutation.CheckVersion(prep.ExpectedVersion, current.Version); err != nil {
				return mutation.Result{}, err
			}
		}

		version := current.Version
		updatedAt := *current.UpdatedAt
		if !created {
			if current.Status == StatusConfigured && current.TimeZone != nil && *current.TimeZone == req.TimeZone &&
				sameIntervals(current.Intervals, intervals) {
				// 내용이 같으면 version을 올리지 않는다. 검색 cache key와 결과를 불필요하게 무효화하지 않는다.
				return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityPreference, ResourceID: p.UserID, ResourceVersion: version}, nil
			}
			if err := tx.QueryRow(ctx, `
				UPDATE availability_preferences
				   SET status = 'configured', time_zone = $2, version = version + 1, updated_at = now()
				 WHERE user_id = $1
				RETURNING version, updated_at`, p.UserID, req.TimeZone).Scan(&version, &updatedAt); err != nil {
				return mutation.Result{}, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM availability_weekly_intervals WHERE user_id = $1`, p.UserID); err != nil {
				return mutation.Result{}, err
			}
		}
		if err := insertIntervals(ctx, tx, p.UserID, intervals); err != nil {
			return mutation.Result{}, err
		}
		// 본인 기기들에 대한 무효화 신호만 보낸다. interval·time zone은 GET으로 다시 받는다.
		if err := tx.EmitChange(ctx, mutation.Change{
			Recipient: p.UserID, EntityType: entityPreference, EntityID: p.UserID, Version: &version,
			Payload: map[string]any{
				"status":     StatusConfigured,
				"version":    version,
				"updated_at": updatedAt.UTC().Format(time.RFC3339),
			},
		}); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityPreference, ResourceID: p.UserID, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	pref, err := loadPreference(r.Context(), h.pool, p.UserID, false)
	if err != nil {
		h.writeDomainError(w, r, "availability.preferences.put", err)
		return
	}
	writePreference(w, r, pref, out.StatusCode)
}

// normalizeIntervals는 §3.2 규칙으로 입력을 저장 정규형으로 바꾼다.
//
//  1. 입력마다: weekday 1~7, 시작 0~1410, 시작·종료 30분 배수, 길이 30분 이상 1440분 이하
//  2. end_minute > 1440이면 시작 요일의 자정 전 구간과 다음 요일(일요일 7 → 월요일 1)의 자정 후 구간으로 나눈다
//  3. 나눈 뒤 같은 요일에서 겹치면(중복 포함) 거부한다. 나뉜 꼬리가 다음 요일의 다른 구간과 겹칠 수 있다
//  4. 맞닿은 구간은 병합한다
//  5. 요일별 3개 초과, 한 주 전체 0개는 거부한다
//
// 결과는 요일, 시작 순으로 정렬돼 있고, 같은 요일 안의 순서가 저장 position이다.
func normalizeIntervals(in []intervalInput) ([]Interval, error) {
	if len(in) == 0 || len(in) > maxInputIntervals {
		return nil, errInvalidInput
	}
	var days [8][]Interval
	for _, iv := range in {
		if iv.Weekday == nil || iv.StartMinute == nil || iv.EndMinute == nil {
			return nil, errInvalidInput
		}
		w, s, e := *iv.Weekday, *iv.StartMinute, *iv.EndMinute
		if w < 1 || w > 7 || s < 0 || s >= minutesPerDay || s%slotMinutes != 0 || e%slotMinutes != 0 ||
			e-s < slotMinutes || e-s > minutesPerDay {
			return nil, errInvalidInput
		}
		if e <= minutesPerDay {
			days[w] = append(days[w], Interval{Weekday: w, StartMinute: s, EndMinute: e})
			continue
		}
		next := w%7 + 1
		days[w] = append(days[w], Interval{Weekday: w, StartMinute: s, EndMinute: minutesPerDay})
		days[next] = append(days[next], Interval{Weekday: next, StartMinute: 0, EndMinute: e - minutesPerDay})
	}
	var out []Interval
	for w := 1; w <= 7; w++ {
		segs := days[w]
		sort.Slice(segs, func(i, j int) bool { return segs[i].StartMinute < segs[j].StartMinute })
		var merged []Interval
		for _, seg := range segs {
			if n := len(merged); n > 0 {
				last := &merged[n-1]
				if seg.StartMinute < last.EndMinute {
					return nil, errInvalidInput
				}
				if seg.StartMinute == last.EndMinute {
					last.EndMinute = seg.EndMinute
					continue
				}
			}
			merged = append(merged, seg)
		}
		if len(merged) > maxIntervalsPerDay {
			return nil, errInvalidInput
		}
		out = append(out, merged...)
	}
	if len(out) == 0 {
		return nil, errInvalidInput
	}
	return out, nil
}

// ValidTimeZone은 지역 IANA 시간대만 받는다(§3.3 "고정 UTC offset은 허용하지 않는다").
//
//   - "Area/City" 형식이어야 한다. "/"가 없는 UTC, GMT, EST, EST5EDT 같은 고정 offset·레거시 별칭을 거부한다
//   - Etc/ 아래(Etc/GMT-9, Etc/UTC 등)는 모두 고정 offset이라 거부한다
//   - host zoneinfo에만 있는 posix/, right/, SystemV/ 변형은 내장 tzdata와 뜻이 갈릴 수 있어 거부한다
//   - "Local"은 서버의 지역 시간대로 해석되므로 거부한다
func ValidTimeZone(name string) bool {
	if name == "" || len(name) > maxTimeZoneLen || name == "Local" || !strings.Contains(name, "/") {
		return false
	}
	for _, prefix := range []string{"Etc/", "posix/", "right/", "SystemV/"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

func sameIntervals(a, b []Interval) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func insertIntervals(ctx context.Context, tx *mutation.Tx, userID uuid.UUID, intervals []Interval) error {
	weekdays := make([]int32, len(intervals))
	positions := make([]int32, len(intervals))
	starts := make([]int32, len(intervals))
	ends := make([]int32, len(intervals))
	for i, iv := range intervals {
		pos := 0
		if i > 0 && intervals[i-1].Weekday == iv.Weekday {
			pos = int(positions[i-1]) + 1
		}
		weekdays[i], positions[i], starts[i], ends[i] = int32(iv.Weekday), int32(pos), int32(iv.StartMinute), int32(iv.EndMinute)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO availability_weekly_intervals (user_id, weekday, position, start_minute, end_minute)
		SELECT $1, w, p, s, e
		  FROM unnest($2::smallint[], $3::smallint[], $4::int[], $5::int[]) AS x(w, p, s, e)`,
		userID, weekdays, positions, starts, ends)
	return err
}

// querier는 pool과 트랜잭션이 함께 만족하는 조회 interface다.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// loadPreference는 사용자의 설정을 읽는다. row가 없으면 unconfigured·version 0이다.
// lock이면 preference row를 FOR NO KEY UPDATE로 잡는다(트랜잭션 안에서만).
func loadPreference(ctx context.Context, q querier, userID uuid.UUID, lock bool) (preference, error) {
	sql := `SELECT status, time_zone, version, updated_at FROM availability_preferences WHERE user_id = $1`
	if lock {
		sql += ` FOR NO KEY UPDATE`
	}
	var pref preference
	var updatedAt time.Time
	err := q.QueryRow(ctx, sql, userID).Scan(&pref.Status, &pref.TimeZone, &pref.Version, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return preference{Status: StatusUnconfigured, Intervals: []Interval{}}, nil
	}
	if err != nil {
		return preference{}, err
	}
	pref.UpdatedAt = &updatedAt
	rows, err := q.Query(ctx, `
		SELECT weekday, start_minute, end_minute
		  FROM availability_weekly_intervals
		 WHERE user_id = $1
		 ORDER BY weekday, start_minute`, userID)
	if err != nil {
		return preference{}, err
	}
	defer rows.Close()
	pref.Intervals = []Interval{}
	for rows.Next() {
		var iv Interval
		if err := rows.Scan(&iv.Weekday, &iv.StartMinute, &iv.EndMinute); err != nil {
			return preference{}, err
		}
		pref.Intervals = append(pref.Intervals, iv)
	}
	return pref, rows.Err()
}

func writePreference(w http.ResponseWriter, r *http.Request, pref preference, status int) {
	// 미설정(version 0)에는 ETag가 없다. 첫 저장은 If-Match 없이 create-only로 보낸다.
	if pref.Version > 0 {
		w.Header().Set("ETag", `"`+strconv.FormatInt(pref.Version, 10)+`"`)
	}
	w.Header().Set("Cache-Control", "no-store")
	resp := preferencesResponse{
		Status: pref.Status, TimeZone: pref.TimeZone, Version: pref.Version, WeeklyIntervals: pref.Intervals,
	}
	if pref.UpdatedAt != nil {
		s := pref.UpdatedAt.UTC().Format(time.RFC3339)
		resp.UpdatedAt = &s
	}
	httpapi.WriteJSON(w, status, preferencesEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Preferences: resp})
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request, p auth.Principal, prep mutation.Prepared, action string, fn mutation.Func) (mutation.Outcome, bool) {
	out, err := mutation.Run(r.Context(), h.pool, mutation.Request{
		UserID: p.UserID, DeviceID: p.DeviceID, Key: prep.Key,
		Hash: mutation.RequestHash(r.Method, r.URL.EscapedPath(), prep.Body),
	}, h.now(), fn)
	if err == nil {
		if out.Replayed {
			w.Header().Set(ReplayedHeader, "true")
		}
		return out, true
	}
	h.writeDomainError(w, r, action, err)
	return mutation.Outcome{}, false
}

func (h *Handler) writeDomainError(w http.ResponseWriter, r *http.Request, action string, err error) {
	switch {
	case mutation.WriteError(w, r, err):
	case errors.Is(err, auth.ErrAccountLocked):
		httpapi.WriteError(w, r, http.StatusLocked, httpapi.CodeAccountLocked, "계정이 비활성화됐거나 삭제 중이다")
	case errors.Is(err, errInvalidInput):
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 값이 올바르지 않다")
	default:
		// 오류 원문에는 SQL과 값(interval, time zone)이 섞일 수 있다. 분류만 남긴다.
		h.logger.ErrorContext(r.Context(), "활동 시간 요청 처리 실패",
			slog.String("request_id", httpapi.RequestID(r.Context()).String()),
			slog.String("action", action),
			slog.String("result", "failure"),
			slog.String("error_type", httpapi.ErrorType(err)),
		)
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
	}
}
