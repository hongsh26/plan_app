// Package account는 사용자 자신의 계정과 기기 조회를 제공한다.
//
// 이 단계(P1)는 조회만 담는다. PATCH /v1/me, DELETE /v1/me,
// DELETE /v1/devices/{id}, PUT /v1/devices/{id}/push-token은 §6에 따라
// Idempotency-Key와 expected_version이 필요한 mutation이므로, 그 규칙을 한 곳에서
// 강제하는 mutation transaction helper와 함께 구현한다. 먼저 만들면 helper가
// 들어올 때 다시 써야 한다.
package account

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/httpapi"
)

// Handler는 계정 조회 endpoint다.
type Handler struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewHandler는 Handler를 만든다.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger) *Handler {
	return &Handler{pool: pool, logger: logger}
}

// Register는 endpoint를 등록한다. require는 인증 미들웨어다.
func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/me", require(http.HandlerFunc(h.getMe)))
	mux.Handle("GET /v1/devices", require(http.HandlerFunc(h.listDevices)))
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

	var u userResponse
	var id uuid.UUID
	var createdAt time.Time
	err := h.pool.QueryRow(r.Context(), `
		SELECT id, display_name, status, version, created_at
		  FROM users WHERE id = $1`, p.UserID).Scan(&id, &u.DisplayName, &u.Status, &u.Version, &createdAt)
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
	httpapi.WriteJSON(w, http.StatusOK, meResponse{
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

	rows, err := h.pool.Query(r.Context(), `
		SELECT id, platform, push_authorization, time_sensitive_setting,
		       last_seen_at, version, created_at
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
		var d deviceResponse
		var id uuid.UUID
		var lastSeen *time.Time
		var createdAt time.Time
		if err := rows.Scan(&id, &d.Platform, &d.PushAuthorization, &d.TimeSensitiveSetting,
			&lastSeen, &d.Version, &createdAt); err != nil {
			h.internalError(w, r, "account.list_devices")
			return
		}
		d.ID = id.String()
		d.Current = id == p.DeviceID
		if lastSeen != nil {
			s := lastSeen.UTC().Format(time.RFC3339)
			d.LastSeenAt = &s
		}
		d.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		out.Devices = append(out.Devices, d)
	}
	if rows.Err() != nil {
		h.internalError(w, r, "account.list_devices")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, action string) {
	h.logger.ErrorContext(r.Context(), "계정 조회 실패",
		slog.String("request_id", httpapi.RequestID(r.Context()).String()),
		slog.String("action", action),
		slog.String("result", "failure"),
	)
	httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
}
