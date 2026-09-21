package account

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/audit"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
)

// sync 변경 피드의 entity 이름. 클라이언트가 분기하는 계약 값이다.
const (
	entityUser   = "user"
	entityDevice = "device"
)

// ReplayedHeader는 저장된 결과를 돌려준 응답에 붙는다. 클라이언트가 재전송이
// 처리됐음을 알 수 있게 한다. 본문은 현재 리소스 상태다.
const ReplayedHeader = "Idempotent-Replayed"

// errNotFound는 대상이 없거나 이 사용자가 볼 수 없는 경우다. 둘을 구분하지 않는다(§6 404).
var errNotFound = errors.New("account: 대상이 없다")

// run은 mutation.Run을 호출하고 공통 오류를 응답한다. 응답을 썼으면 ok가 false다.
func (h *Handler) run(w http.ResponseWriter, r *http.Request, p auth.Principal, prep mutation.Prepared,
	action string, fn mutation.Func) (mutation.Outcome, bool) {
	out, err := mutation.Run(r.Context(), h.pool, mutation.Request{
		UserID:   p.UserID,
		DeviceID: p.DeviceID,
		Key:      prep.Key,
		Hash:     mutation.RequestHash(r.Method, r.URL.EscapedPath(), prep.Body),
	}, h.now(), fn)
	if err == nil {
		if out.Replayed {
			w.Header().Set(ReplayedHeader, "true")
		}
		return out, true
	}
	switch {
	case mutation.WriteError(w, r, err):
	case errors.Is(err, errNotFound):
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
	default:
		h.logger.ErrorContext(r.Context(), "계정 변경 실패",
			slog.String("request_id", httpapi.RequestID(r.Context()).String()),
			slog.String("actor", p.UserID.String()),
			slog.String("action", action),
			slog.String("result", "failure"),
			slog.String("error_type", httpapi.ErrorType(err)),
		)
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
	}
	return mutation.Outcome{}, false
}

type patchMeRequest struct {
	DisplayName *string `json:"display_name"`
}

// patchMe는 프로필을 수정한다. 현재 바꿀 수 있는 것은 표시 이름뿐이다.
func (h *Handler) patchMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	var req patchMeRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil || req.DisplayName == nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "display_name이 필요하다")
		return
	}
	name, ok := auth.NormalizeDisplayName(*req.DisplayName)
	if !ok {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "표시 이름이 비어 있다")
		return
	}

	out, ok := h.run(w, r, p, prep, "account.update_profile", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		var current int64
		if err := tx.QueryRow(ctx,
			`SELECT version FROM users WHERE id = $1 FOR NO KEY UPDATE`, p.UserID).Scan(&current); err != nil {
			return mutation.Result{}, err
		}
		if err := mutation.CheckVersion(prep.ExpectedVersion, current); err != nil {
			return mutation.Result{}, err
		}
		var version int64
		if err := tx.QueryRow(ctx, `
			UPDATE users SET display_name = $2, version = version + 1, updated_at = now()
			 WHERE id = $1 RETURNING version`, p.UserID, name).Scan(&version); err != nil {
			return mutation.Result{}, err
		}
		// 표시 이름은 앞으로 Party 멤버에게도 전파된다. 지금은 Party가 없으므로
		// 본인의 다른 기기만 받는다.
		if err := tx.EmitChange(ctx, mutation.Change{
			Recipient: p.UserID, EntityType: entityUser, EntityID: p.UserID, Version: &version,
			Payload: map[string]any{"display_name": name},
		}); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityUser, ResourceID: p.UserID, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	h.writeMe(w, r, out.ResourceID, out.StatusCode)
}

// deleteDevice는 기기와 그 세션을 폐기한다(§5.2 "다른 기기 세션을 조회하고 폐기").
//
// 같은 사용자의 어느 기기든 지정할 수 있다. 남의 기기는 존재 여부를 드러내지
// 않도록 404다. 현재 기기를 지정하면 로그아웃과 같다.
//
// sync에는 tombstone이 아니라 revoked를 담은 upsert로 보낸다. row는 남고 version이
// 오르며, 스키마 CHECK상 tombstone은 version을 가질 수 없다. 클라이언트는
// revoked=true를 보고 목록에서 뺀다.
func (h *Handler) deleteDevice(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	target, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	if len(prep.Body) != 0 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "본문을 보내지 않는다")
		return
	}

	_, ok = h.run(w, r, p, prep, "device.revoke", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		current, err := lockOwnLiveDevice(ctx, tx, p.UserID, target)
		if err != nil {
			return mutation.Result{}, err
		}
		if err := mutation.CheckVersion(prep.ExpectedVersion, current); err != nil {
			return mutation.Result{}, err
		}
		version, _, err := auth.RevokeDevice(ctx, tx, target)
		if err != nil {
			return mutation.Result{}, err
		}
		if err := tx.EmitChange(ctx, mutation.Change{
			Recipient: p.UserID, EntityType: entityDevice, EntityID: target, Version: &version,
			Payload: map[string]any{"revoked": true},
		}); err != nil {
			return mutation.Result{}, err
		}
		if err := audit.Record(ctx, tx, audit.Event{Actor: &p.UserID, Action: "device.revoke",
			TargetType: entityDevice, TargetID: &target, Result: "success",
			RequestID: httpapi.RequestID(ctx)}); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusNoContent, ResourceType: entityDevice, ResourceID: target, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lockOwnLiveDevice는 auth의 잠금 순서(users → devices)대로 잠그고, 기기가 이 사용자의
// 폐기되지 않은 기기인지 확인한 뒤 현재 version을 돌려준다.
func lockOwnLiveDevice(ctx context.Context, tx pgx.Tx, userID, deviceID uuid.UUID) (int64, error) {
	if err := auth.LockUserAndDevice(ctx, tx, userID, deviceID); err != nil {
		return 0, err
	}
	var version int64
	err := tx.QueryRow(ctx, `
		SELECT version FROM devices
		 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, deviceID, userID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound
	}
	return version, err
}

type pushTokenRequest struct {
	// PushToken은 APNs device token의 hex 문자열이다. null이면 알림을 끈 것이므로
	// 저장된 token과 environment를 지운다.
	PushToken            *string `json:"push_token"`
	PushEnvironment      *string `json:"push_environment"`
	PushAuthorization    string  `json:"push_authorization"`
	TimeSensitiveSetting string  `json:"time_sensitive_setting"`
}

// APNs device token 길이 범위(바이트). 현재 32바이트지만 Apple은 길이를 계약으로
// 고정하지 않았으므로 여유를 둔다.
const (
	minPushTokenBytes = 32
	maxPushTokenBytes = 100
)

// putPushToken은 기기의 APNs token과 알림 권한 상태를 등록·교체한다.
//
// {id}는 access token의 기기와 같아야 한다(§6 "path 값이 있으면 claim과 일치").
// 다른 기기의 token을 대신 등록할 이유가 없고, 허용하면 남의 알림을 가로챌 수
// 있다. 존재 여부와 무관하게 403이다.
func (h *Handler) putPushToken(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	target, err := uuid.Parse(r.PathValue("id"))
	if err != nil || target != p.DeviceID {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "자기 기기에만 등록할 수 있다")
		return
	}
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	var req pushTokenRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	token, msg := validatePushToken(req)
	if msg != "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, msg)
		return
	}
	var sealed []byte
	if token != nil {
		if sealed, err = h.sealer.Seal(token, SealPurposePushToken); err != nil {
			httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
			return
		}
	}

	out, ok := h.run(w, r, p, prep, "device.update_push", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		current, err := lockOwnLiveDevice(ctx, tx, p.UserID, target)
		if err != nil {
			return mutation.Result{}, err
		}
		if err := mutation.CheckVersion(prep.ExpectedVersion, current); err != nil {
			return mutation.Result{}, err
		}
		var version int64
		if err := tx.QueryRow(ctx, `
			UPDATE devices
			   SET push_token_ciphertext = $2, push_environment = $3, push_authorization = $4,
			       time_sensitive_setting = $5, last_seen_at = now(),
			       version = version + 1, updated_at = now()
			 WHERE id = $1
			RETURNING version`,
			target, sealed, req.PushEnvironment, req.PushAuthorization, req.TimeSensitiveSetting,
		).Scan(&version); err != nil {
			return mutation.Result{}, err
		}
		// token 자체는 암호문이라도 sync로 보내지 않는다. 다른 기기가 알아야 할 것은
		// 알림을 받을 수 있는 상태인지뿐이다.
		if err := tx.EmitChange(ctx, mutation.Change{
			Recipient: p.UserID, EntityType: entityDevice, EntityID: target, Version: &version,
			Payload: map[string]any{
				"push_authorization":     req.PushAuthorization,
				"time_sensitive_setting": req.TimeSensitiveSetting,
				"has_push_token":         token != nil,
			},
		}); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityDevice, ResourceID: target, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	h.writeDevice(w, r, p, out.ResourceID, out.StatusCode)
}

// validatePushToken은 입력을 검사하고 token 바이트를 돌려준다. 문제가 있으면
// msg가 비어 있지 않다. token과 environment는 함께 있거나 함께 없어야 한다
// (devices_push_token_environment_pairing_check).
func validatePushToken(req pushTokenRequest) (token []byte, msg string) {
	switch req.PushAuthorization {
	case "unknown", "authorized", "provisional", "denied":
	default:
		return nil, "push_authorization은 unknown, authorized, provisional, denied 중 하나다"
	}
	switch req.TimeSensitiveSetting {
	case "enabled", "disabled", "not_supported":
	default:
		return nil, "time_sensitive_setting은 enabled, disabled, not_supported 중 하나다"
	}
	if (req.PushToken == nil) != (req.PushEnvironment == nil) {
		return nil, "push_token과 push_environment는 함께 보내거나 함께 null이어야 한다"
	}
	if req.PushToken == nil {
		return nil, ""
	}
	if *req.PushEnvironment != "sandbox" && *req.PushEnvironment != "production" {
		return nil, "push_environment는 sandbox 또는 production이다"
	}
	b, err := hex.DecodeString(*req.PushToken)
	if err != nil || len(b) < minPushTokenBytes || len(b) > maxPushTokenBytes {
		return nil, "push_token은 APNs device token의 hex 문자열이어야 한다"
	}
	return b, ""
}

// writeDevice는 기기 하나의 현재 상태를 응답한다.
func (h *Handler) writeDevice(w http.ResponseWriter, r *http.Request, p auth.Principal, deviceID uuid.UUID, status int) {
	d, err := scanDevice(h.pool.QueryRow(r.Context(), deviceColumns+`
		  FROM devices WHERE id = $1 AND user_id = $2`, deviceID, p.UserID), p.DeviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	if err != nil {
		h.internalError(w, r, "account.get_device")
		return
	}
	w.Header().Set("ETag", etag(d.Version))
	httpapi.WriteJSON(w, status, deviceBody{RequestID: httpapi.RequestID(r.Context()).String(), Device: d})
}

type deviceBody struct {
	RequestID string         `json:"request_id"`
	Device    deviceResponse `json:"device"`
}

// deviceColumns와 scanDevice는 목록과 단건 조회가 같은 형식을 쓰게 한다.
const deviceColumns = `
		SELECT id, platform, push_authorization, time_sensitive_setting,
		       last_seen_at, version, created_at`

func scanDevice(row pgx.Row, current uuid.UUID) (deviceResponse, error) {
	var d deviceResponse
	var id uuid.UUID
	var lastSeen *time.Time
	var createdAt time.Time
	if err := row.Scan(&id, &d.Platform, &d.PushAuthorization, &d.TimeSensitiveSetting,
		&lastSeen, &d.Version, &createdAt); err != nil {
		return deviceResponse{}, err
	}
	d.ID = id.String()
	d.Current = id == current
	if lastSeen != nil {
		s := lastSeen.UTC().Format(time.RFC3339)
		d.LastSeenAt = &s
	}
	d.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return d, nil
}
