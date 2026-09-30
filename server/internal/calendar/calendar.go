// Package calendar는 설계 3(docs/calendar_privacy_sync_design.md)의 서버 쪽 캘린더 동기화다.
// 기기가 올린 snapshot을 검증해 Busy facts로 바꾸고, 사용자가 속한 Party의 busy projection을
// 맞춘다. 제목·장소 같은 상세와 쓰기 명령은 공개 수준·캘린더 쓰기 단계에서 추가한다.
//
// 개인정보 규칙(§3, §13): source_event_key 원문, 시간 구간, 시간대는 로그와 오류 메시지에 남기지
// 않는다. facts는 소유자 본인과 서버의 projection·availability service만 읽는다.
package calendar

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

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
	codeStateInvalid       = "calendar_state_invalid"
	codeSourceRequired     = "calendar_source_required"
	codeSnapshotIncomplete = "calendar_snapshot_incomplete"
)

var (
	errNotFound       = errors.New("calendar: 대상이 없다")
	errForbidden      = errors.New("calendar: 권한이 없다")
	errStateInvalid   = errors.New("calendar: 연결 상태가 이 동작을 허용하지 않는다")
	errSourceRequired = errors.New("calendar: 읽을 캘린더가 선택되지 않았다")
	errInvalidInput   = errors.New("calendar: 입력이 올바르지 않다")
)

// FreshnessWindow는 마지막 정상 sync가 이 시간 안이어야 fresh다(설계 3 §5.2, §9).
const FreshnessWindow = 6 * time.Hour

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
	// Limiter는 snapshot 생성·업로드 요청 제한이다. nil이면 제한하지 않는다.
	Limiter ratelimit.Allower
}

func NewHandler(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{pool: d.Pool, logger: d.Logger, now: d.Now, limiter: d.Limiter}
}

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/calendar/connection", require(http.HandlerFunc(h.getConnection)))
	mux.Handle("PUT /v1/calendar/connection", require(http.HandlerFunc(h.putConnection)))
	mux.Handle("POST /v1/calendar/snapshots", require(http.HandlerFunc(h.createSnapshot)))
	mux.Handle("PUT /v1/calendar/snapshots/{id}/pages/{n}", require(http.HandlerFunc(h.putPage)))
	mux.Handle("POST /v1/calendar/snapshots/{id}/complete", require(http.HandlerFunc(h.completeSnapshot)))
	mux.Handle("POST /v1/calendar/snapshots/{id}/abort", require(http.HandlerFunc(h.abortSnapshot)))
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
	case errors.Is(err, errNotFound):
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
	case errors.Is(err, errForbidden):
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "이 기기는 캘린더 sync 기기가 아니다")
	case errors.Is(err, errStateInvalid):
		httpapi.WriteError(w, r, http.StatusConflict, codeStateInvalid, "현재 연결 상태에서는 할 수 없는 동작이다")
	case errors.Is(err, errSourceRequired):
		httpapi.WriteError(w, r, http.StatusConflict, codeSourceRequired, "읽을 캘린더를 하나 이상 선택해야 한다")
	case errors.Is(err, errSnapshotState):
		httpapi.WriteError(w, r, http.StatusConflict, codeSnapshotState, "이미 끝났거나 중단된 snapshot이다")
	case errors.Is(err, errSnapshotIncomplete):
		httpapi.WriteError(w, r, http.StatusConflict, codeSnapshotIncomplete, "모든 page가 올라오지 않았거나 fact 개수가 맞지 않는다")
	case errors.Is(err, errInvalidInput):
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 값이 올바르지 않다")
	default:
		// 오류 원문에는 SQL과 값이 섞일 수 있다. 분류만 남긴다.
		h.logger.ErrorContext(r.Context(), "캘린더 요청 처리 실패",
			slog.String("request_id", httpapi.RequestID(r.Context()).String()),
			slog.String("action", action),
			slog.String("result", "failure"),
			slog.String("error_type", httpapi.ErrorType(err)),
		)
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
	}
}

// enforce는 사용자 단위 요청 제한이다. false이면 응답을 이미 썼다.
func (h *Handler) enforce(w http.ResponseWriter, r *http.Request, scope string, l ratelimit.Limit, p auth.Principal) bool {
	_, ok := ratelimit.Enforce(w, r, h.limiter, ratelimit.ModeClosed, ratelimit.UserRule(scope, l, p.UserID))
	return ok
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// validTimeZone은 IANA 시간대 이름만 받는다. time.LoadLocation은 "Local"을 서버의 지역 시간대로
// 해석하므로 거부한다. 서버 환경에 따라 값의 뜻이 달라지면 안 된다.
func validTimeZone(name string) bool {
	if name == "" || len(name) > 64 || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
