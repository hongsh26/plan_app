package syncfeed

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/notification"
	"plantogether/server/internal/platform/httpapi"
)

// Handler는 sync endpoint다.
type Handler struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	box    notification.Box
	now    func() time.Time
}

// Deps는 Handler의 의존성이다.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	// RefKeyBox는 notification_ref_key를 봉인하고 연다.
	RefKeyBox notification.Box
	Now       func() time.Time
}

// NewHandler는 Handler를 만든다.
func NewHandler(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{pool: d.Pool, logger: d.Logger, box: d.RefKeyBox, now: d.Now}
}

// Register는 endpoint를 등록한다.
func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/sync", require(http.HandlerFunc(h.read)))
	mux.Handle("GET /v1/sync/bootstrap", require(http.HandlerFunc(h.bootstrap)))
}

type readResponse struct {
	RequestID  string   `json:"request_id"`
	Changes    []Change `json:"changes"`
	Cursor     string   `json:"cursor"`
	HasMore    bool     `json:"has_more"`
	ServerTime string   `json:"server_time"`
}

// read는 GET /v1/sync?cursor=&limit= 이다. cursor 없이 부르면 400이다. 처음에는
// bootstrap으로 watermark를 받아야 한다.
func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	q := r.URL.Query()
	raw := q.Get("cursor")
	if raw == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"cursor가 필요하다. 처음에는 /v1/sync/bootstrap을 호출한다")
		return
	}
	cur, err := DecodeCursor(raw)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "cursor를 해석할 수 없다")
		return
	}
	limit := 0
	if l := q.Get("limit"); l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit < 1 || limit > maxLimit {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "limit은 1~1000이다")
			return
		}
	}

	now := h.now()
	page, err := Read(r.Context(), h.pool, p.UserID, cur, limit)
	if errors.Is(err, ErrCursorExpired) {
		httpapi.WriteError(w, r, http.StatusGone, httpapi.CodeSyncCursorExpired,
			"cursor를 이어 쓸 수 없다. /v1/sync/bootstrap으로 다시 받아야 한다")
		return
	}
	if err != nil {
		h.internalError(w, r, "sync.read", err)
		return
	}
	if page.Changes == nil {
		page.Changes = []Change{}
	}
	httpapi.WriteJSON(w, http.StatusOK, readResponse{
		RequestID:  httpapi.RequestID(r.Context()).String(),
		Changes:    page.Changes,
		Cursor:     page.Next.Encode(),
		HasMore:    page.HasMore,
		ServerTime: now.UTC().Format(time.RFC3339),
	})
}

type bootstrapResponse struct {
	RequestID       string   `json:"request_id"`
	SchemaVersion   int      `json:"schema_version"`
	Snapshot        []Change `json:"snapshot"`
	CursorWatermark string   `json:"cursor_watermark"`
	ServerTime      string   `json:"server_time"`
	// NotificationRefKey는 base64url(패딩 없음) 32바이트다(설계 9 §4.3, §17.2).
	NotificationRefKey string `json:"notification_ref_key"`
}

func (h *Handler) bootstrap(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	now := h.now()
	snap, err := Bootstrap(r.Context(), h.pool, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		// 인증 직후 사용자 row가 사라졌다. 삭제 파이프라인의 물리 삭제뿐이다.
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeInvalidSession, "세션이 유효하지 않다")
		return
	}
	if err != nil {
		h.internalError(w, r, "sync.bootstrap", err)
		return
	}
	// snapshot 트랜잭션 밖에서 발급한다. EnsureRefKey는 REPEATABLE READ 안에서
	// 동시 생성 경쟁을 해소하지 못한다.
	key, err := notification.EnsureRefKey(r.Context(), h.pool, h.box, p.UserID)
	if err != nil {
		h.internalError(w, r, "sync.bootstrap", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, bootstrapResponse{
		RequestID:          httpapi.RequestID(r.Context()).String(),
		SchemaVersion:      SchemaVersion,
		Snapshot:           snap.Entities,
		CursorWatermark:    snap.Watermark.Encode(),
		ServerTime:         now.UTC().Format(time.RFC3339),
		NotificationRefKey: notification.EncodeRefKey(key),
	})
}

func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, action string, err error) {
	p, _ := auth.PrincipalFrom(r.Context())
	h.logger.ErrorContext(r.Context(), "sync 처리 실패",
		slog.String("request_id", httpapi.RequestID(r.Context()).String()),
		slog.String("actor", p.UserID.String()),
		slog.String("action", action),
		slog.String("result", "failure"),
		slog.String("error_type", httpapi.ErrorType(err)),
	)
	httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
}
