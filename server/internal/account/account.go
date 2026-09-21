// Package account는 사용자 자신의 계정과 기기 조회를 제공한다.
//
// 조회: GET /v1/me, GET /v1/devices
// 변경: PATCH /v1/me, DELETE /v1/devices/{id}, PUT /v1/devices/{id}/push-token
//
// 변경은 모두 internal/platform/mutation을 거친다. Idempotency-Key와 If-Match가
// 필수이고, 도메인 변경과 sync 변경이 한 트랜잭션에서 커밋된다.
//
// DELETE /v1/me(계정 삭제 요청)는 아직 없다. §10 파이프라인을 진행시킬 worker가
// 없는 상태에서 만들면 요청한 계정이 deletion_requested에 영구히 갇힌다.
// §5.3은 유예 기간 없이 즉시 차단하라고 하므로 되돌릴 방법도 없다.
package account

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/httpapi"
)

// Sealer는 push token 암호화다. secretbox가 구현한다.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
}

// SealPurposePushToken은 push token 암호문의 associated data다. 발송 worker가
// 같은 값으로 연다.
const SealPurposePushToken = "devices.push_token"

// Handler는 계정 endpoint다.
type Handler struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	sealer Sealer
	now    func() time.Time
}

// Deps는 Handler의 의존성이다.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Sealer Sealer
	Now    func() time.Time
}

// NewHandler는 Handler를 만든다.
func NewHandler(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{pool: d.Pool, logger: d.Logger, sealer: d.Sealer, now: d.Now}
}

// Register는 endpoint를 등록한다. require는 인증 미들웨어다.
func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/me", require(http.HandlerFunc(h.getMe)))
	mux.Handle("PATCH /v1/me", require(http.HandlerFunc(h.patchMe)))
	mux.Handle("GET /v1/devices", require(http.HandlerFunc(h.listDevices)))
	mux.Handle("DELETE /v1/devices/{id}", require(http.HandlerFunc(h.deleteDevice)))
	mux.Handle("PUT /v1/devices/{id}/push-token", require(http.HandlerFunc(h.putPushToken)))
}

type userResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Version     int64  `json:"version"`
	CreatedAt   string `json:"created_at"`
}

type meResponse struct {
	RequestID string       `json:"request_id"`
	User      userResponse `json:"user"`
}

func (h *Handler) getMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	h.writeMe(w, r, p.UserID, http.StatusOK)
}

// writeMe는 사용자의 현재 상태를 응답한다. 조회와 PATCH, PATCH의 재전송이 같이 쓴다.
// ETag에 version을 실어 클라이언트가 다음 If-Match에 그대로 쓰게 한다.
func (h *Handler) writeMe(w http.ResponseWriter, r *http.Request, userID uuid.UUID, status int) {
	var u userResponse
	var id uuid.UUID
	var createdAt time.Time
	err := h.pool.QueryRow(r.Context(), `
		SELECT id, display_name, status, version, created_at
		  FROM users WHERE id = $1`, userID).Scan(&id, &u.DisplayName, &u.Status, &u.Version, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// 인증 미들웨어가 방금 확인한 사용자다. 그 사이 물리 삭제된 경우뿐이다.
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeInvalidSession, "세션이 유효하지 않다")
		return
	}
	if err != nil {
		h.internalError(w, r, "account.get_me")
		return
	}
	u.ID = id.String()
	u.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	w.Header().Set("ETag", etag(u.Version))
	httpapi.WriteJSON(w, status, meResponse{
		RequestID: httpapi.RequestID(r.Context()).String(),
		User:      u,
	})
}

type deviceResponse struct {
	ID                   string  `json:"id"`
	Platform             string  `json:"platform"`
	Current              bool    `json:"current"`
	PushAuthorization    string  `json:"push_authorization"`
	TimeSensitiveSetting string  `json:"time_sensitive_setting"`
	LastSeenAt           *string `json:"last_seen_at"`
	Version              int64   `json:"version"`
	CreatedAt            string  `json:"created_at"`
}

type devicesResponse struct {
	RequestID string           `json:"request_id"`
	Devices   []deviceResponse `json:"devices"`
}

// listDevices는 §5.2 "사용자는 설정에서 다른 기기 세션을 조회"하는 목록이다.
// 폐기된 기기는 보여주지 않는다. push token은 암호문이라도 내보내지 않는다.
func (h *Handler) listDevices(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())

	rows, err := h.pool.Query(r.Context(), deviceColumns+`
		  FROM devices
		 WHERE user_id = $1 AND revoked_at IS NULL
		 ORDER BY created_at, id`, p.UserID)
	if err != nil {
		h.internalError(w, r, "account.list_devices")
		return
	}
	defer rows.Close()

	out := devicesResponse{RequestID: httpapi.RequestID(r.Context()).String(), Devices: []deviceResponse{}}
	for rows.Next() {
		d, err := scanDevice(rows, p.DeviceID)
		if err != nil {
			h.internalError(w, r, "account.list_devices")
			return
		}
		out.Devices = append(out.Devices, d)
	}
	if rows.Err() != nil {
		h.internalError(w, r, "account.list_devices")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

func etag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, action string) {
	h.logger.ErrorContext(r.Context(), "계정 요청 처리 실패",
		slog.String("request_id", httpapi.RequestID(r.Context()).String()),
		slog.String("action", action),
		slog.String("result", "failure"),
	)
	httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
}
