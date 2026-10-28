// Package availability는 설계 6(docs/availability_search_design.md)의 서버 쪽 공통 가능 시간이다.
// A1 사용자별 활동 시간 저장·조회(이 파일), A3 검색 snapshot·calculationVersion(search_snapshot.go),
// A4 Party 검색 API·캐시(search.go)가 있다. 계산 엔진은 engine 하위 패키지에 있다.
//
// 개인정보 규칙(§11, §12): 원시 활동 interval과 time zone은 본인 API 응답에만 싣는다. 다른 사용자
// sync에 발행하지 않고, 본인 sync 변경에도 interval·time zone을 넣지 않으며, 로그와 오류 메시지에
// 남기지 않는다.
package availability

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/availability/engine"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/ratelimit"

	// 시간대 검증에 IANA 데이터베이스가 필요하다. 배포 이미지에 zoneinfo가 없어도 동작하게 내장한다.
	_ "time/tzdata"
)

//go:generate go run ./tzgen

// ReplayedHeader는 저장된 결과를 돌려준 재전송 표시다.
const ReplayedHeader = "Idempotent-Replayed"

const (
	StatusUnconfigured = "unconfigured"
	StatusConfigured   = "configured"

	// CodeTimeZoneInvalid는 time_zone이 지역 IANA 시간대가 아닐 때의 400 code다. 클라이언트는 이 code를
	// 받으면 기기 시간대 대신 도시 선택 UI를 띄운다(예: 기기 identifier가 GMT인 경우).
	CodeTimeZoneInvalid = "time_zone_invalid"

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
	pool      *pgxpool.Pool
	logger    *slog.Logger
	now       func() time.Time
	limiter   ratelimit.Allower
	versioner *Versioner
	cache     *resultCache
}

type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Now    func() time.Time
	// Limiter는 docs/rate_limit_design.md의 요청 제한이다. nil이면 제한하지 않는다.
	Limiter ratelimit.Allower
	// VersionSecret은 calculationVersion 키를 파생할 비밀(RATE_LIMIT_KEY)이다. 검색에 필수다.
	VersionSecret []byte
}

// NewHandler는 VersionSecret이 비어 있으면 오류다. 인스턴스마다 다른 키가 생기면 다른 인스턴스가 만든
// calculationVersion을 검증하지 못하므로 임의 키로 대신하지 않는다.
func NewHandler(d Deps) (*Handler, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	v, err := NewVersioner(d.VersionSecret)
	if err != nil {
		return nil, err
	}
	return &Handler{pool: d.Pool, logger: d.Logger, now: d.Now, limiter: d.Limiter, versioner: v,
		cache: newResultCache(searchCacheSize)}, nil
}

// Versioner는 설계 7이 VerifyCalculationVersion을 부를 때 쓴다.
func (h *Handler) Versioner() *Versioner { return h.versioner }

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/availability/preferences/me", require(http.HandlerFunc(h.getPreferences)))
	mux.Handle("PUT /v1/availability/preferences/me", require(http.HandlerFunc(h.putPreferences)))
	mux.Handle("POST /v1/parties/{party_id}/availability/search", require(http.HandlerFunc(h.search)))
}

// Interval은 API 응답의 정규형 활동 구간이다(engine.WeeklyInterval과 같은 필드). 분은 사용자 time zone의
// 지역 시각이며 end_minute ≤ 1440이다.
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

// Preference는 한 사용자의 저장된 활동 시간 설정이다. row가 없으면 Status=unconfigured, Version=0,
// TimeZone="", Intervals는 빈 slice, UpdatedAt은 zero다.
//
// Intervals는 저장 정규형(요일·시작 순 정렬, 같은 요일 겹침·맞닿음 없음, 0 ≤ Start < End ≤ 1440)이라
// engine.Member.Weekly에 그대로 넘긴다.
type Preference struct {
	Status    string
	TimeZone  string
	Version   int64
	Intervals []engine.WeeklyInterval
	UpdatedAt time.Time
}

func (h *Handler) getPreferences(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	// 조회는 사용자 전역 설정 하나라 남용 표면이 작다. 제한 장치 장애로 설정 화면이 막히지 않게 open이다.
	if _, ok := ratelimit.Enforce(w, r, h.limiter, ratelimit.ModeOpen,
		ratelimit.UserRule(ratelimit.ScopeAvailabilityPrefRead, ratelimit.LimitAvailabilityPrefRead, p.UserID)); !ok {
		return
	}
	pref, err := loadOne(r.Context(), h.pool, p.UserID)
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
		httpapi.WriteError(w, r, http.StatusBadRequest, CodeTimeZoneInvalid, "time_zone은 Area/City 형식의 지역 IANA 시간대여야 한다")
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
		if _, err := tx.Exec(ctx, `SELECT 1 FROM availability_preferences WHERE user_id = $1 FOR NO KEY UPDATE`, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		// 잠금 뒤의 새 문장이라 마지막 커밋 상태를 본다. 다른 쓰기는 이 row 잠금을 기다린다.
		current, err := loadOne(ctx, tx, p.UserID)
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
		updatedAt := current.UpdatedAt
		if !created {
			if current.Status == StatusConfigured && current.TimeZone == req.TimeZone &&
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
	pref, err := loadOne(r.Context(), h.pool, p.UserID)
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
func normalizeIntervals(in []intervalInput) ([]engine.WeeklyInterval, error) {
	if len(in) == 0 || len(in) > maxInputIntervals {
		return nil, errInvalidInput
	}
	var days [8][]engine.WeeklyInterval
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
			days[w] = append(days[w], engine.WeeklyInterval{Weekday: w, StartMinute: s, EndMinute: e})
			continue
		}
		next := w%7 + 1
		days[w] = append(days[w], engine.WeeklyInterval{Weekday: w, StartMinute: s, EndMinute: minutesPerDay})
		days[next] = append(days[next], engine.WeeklyInterval{Weekday: next, StartMinute: 0, EndMinute: e - minutesPerDay})
	}
	var out []engine.WeeklyInterval
	for w := 1; w <= 7; w++ {
		segs := days[w]
		sort.Slice(segs, func(i, j int) bool { return segs[i].StartMinute < segs[j].StartMinute })
		var merged []engine.WeeklyInterval
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

// zoneNamePattern은 "Area/City" 또는 "Area/Sub/City" 모양이다. tzgen의 같은 이름 변수와 같아야 한다.
var zoneNamePattern = regexp.MustCompile(`^[A-Z][A-Za-z_]+(/[A-Za-z0-9_+\-]+){1,2}$`)

// ValidTimeZone은 지역 IANA 시간대만 받는다(§3.3 "고정 UTC offset은 허용하지 않는다").
// 결과가 서버 환경(OS zoneinfo 유무, 대소문자를 무시하는 파일시스템)에 따라 달라지지 않게 한다.
//
//   - zoneNamePattern에 맞아야 한다. "/"가 없는 UTC, GMT, EST, EST5EDT, Local과 "Asia//Seoul"을 거부한다
//   - Etc/·posix/·right/·SystemV/ 접두는 대소문자와 상관없이 거부한다(Etc/GMT-9, etc/UTC 등 고정 offset)
//   - 내장 tzdata에서 만든 목록(regionZones, zones_gen.go)에 정확히(대소문자 포함) 있어야 한다.
//     macOS에서 "asia/seoul"이 OS zoneinfo로 로딩되는 것을 막는다
//   - 마지막으로 time.LoadLocation이 성공해야 한다
func ValidTimeZone(name string) bool {
	if len(name) > maxTimeZoneLen || !zoneNamePattern.MatchString(name) {
		return false
	}
	lower := strings.ToLower(name)
	for _, prefix := range []string{"etc/", "posix/", "right/", "systemv/"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	if _, ok := regionZones[name]; !ok {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

func sameIntervals[T comparable](a, b []T) bool {
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

func insertIntervals(ctx context.Context, tx *mutation.Tx, userID uuid.UUID, intervals []engine.WeeklyInterval) error {
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

// Querier는 pool과 트랜잭션이 함께 만족하는 조회 interface다.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadPreferences는 여러 사용자의 설정을 한 문장으로 읽는다. 요청한 모든 사용자가 결과에 있고, row가
// 없는 사용자는 unconfigured·version 0이다.
//
// preference와 interval을 한 문장(같은 snapshot)에서 읽으므로 둘이 서로 다른 시점으로 섞이지 않는다.
// 검색(A3)은 REPEATABLE READ 트랜잭션을 q로 넘겨 다른 입력과 같은 시점으로 맞춘다.
func LoadPreferences(ctx context.Context, q Querier, userIDs []uuid.UUID) (map[uuid.UUID]Preference, error) {
	out := make(map[uuid.UUID]Preference, len(userIDs))
	for _, id := range userIDs {
		out[id] = Preference{Status: StatusUnconfigured, Intervals: []engine.WeeklyInterval{}}
	}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT p.user_id, p.status, p.time_zone, p.version, p.updated_at,
		       COALESCE((SELECT json_agg(json_build_array(i.weekday, i.start_minute, i.end_minute)
		                                 ORDER BY i.weekday, i.start_minute)
		                   FROM availability_weekly_intervals i
		                  WHERE i.user_id = p.user_id), '[]'::json)
		  FROM availability_preferences p
		 WHERE p.user_id = ANY($1)`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      uuid.UUID
			pref    Preference
			tz      *string
			rawJSON []byte
		)
		if err := rows.Scan(&id, &pref.Status, &tz, &pref.Version, &pref.UpdatedAt, &rawJSON); err != nil {
			return nil, err
		}
		if tz != nil {
			pref.TimeZone = *tz
		}
		var triples [][3]int
		if err := json.Unmarshal(rawJSON, &triples); err != nil {
			return nil, err
		}
		pref.Intervals = make([]engine.WeeklyInterval, len(triples))
		for i, t := range triples {
			pref.Intervals[i] = engine.WeeklyInterval{Weekday: t[0], StartMinute: t[1], EndMinute: t[2]}
		}
		out[id] = pref
	}
	return out, rows.Err()
}

func loadOne(ctx context.Context, q Querier, userID uuid.UUID) (Preference, error) {
	m, err := LoadPreferences(ctx, q, []uuid.UUID{userID})
	if err != nil {
		return Preference{}, err
	}
	return m[userID], nil
}

func writePreference(w http.ResponseWriter, r *http.Request, pref Preference, status int) {
	// 미설정(version 0)에는 ETag가 없다. 첫 저장은 If-Match 없이 create-only로 보낸다.
	if pref.Version > 0 {
		w.Header().Set("ETag", `"`+strconv.FormatInt(pref.Version, 10)+`"`)
	}
	w.Header().Set("Cache-Control", "no-store")
	resp := preferencesResponse{Status: pref.Status, Version: pref.Version, WeeklyIntervals: make([]Interval, len(pref.Intervals))}
	for i, iv := range pref.Intervals {
		resp.WeeklyIntervals[i] = Interval(iv)
	}
	if pref.TimeZone != "" {
		resp.TimeZone = &pref.TimeZone
	}
	if !pref.UpdatedAt.IsZero() {
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
