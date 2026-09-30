package calendar

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/party"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
)

const (
	maxSources      = 100
	maxSourceKeyLen = 128
)

type sourceJSON struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

type connectionResponse struct {
	// Status는 클라이언트가 보는 유효 상태다. ready인데 마지막 정상 sync가 6시간을 넘었으면 stale이다.
	Status              string       `json:"status"`
	SyncDeviceID        *uuid.UUID   `json:"sync_device_id"`
	IsSyncDevice        bool         `json:"is_sync_device"`
	ActiveGeneration    int64        `json:"active_generation"`
	LastCompletedSyncAt *string      `json:"last_completed_sync_at"`
	TimeZone            *string      `json:"time_zone"`
	Sources             []sourceJSON `json:"sources"`
	Version             int64        `json:"version"`
}

type connectionEnvelope struct {
	RequestID  string             `json:"request_id"`
	Connection connectionResponse `json:"connection"`
}

func effectiveStatus(status string, last *time.Time, now time.Time) string {
	if status == "ready" && (last == nil || now.Sub(*last) > FreshnessWindow) {
		return "stale"
	}
	return status
}

func (h *Handler) loadConnection(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, userID, deviceID uuid.UUID) (connectionResponse, error) {
	var (
		id     uuid.UUID
		c      connectionResponse
		status string
		last   *time.Time
	)
	err := q.QueryRow(ctx, `
		SELECT id, source_status, sync_device_id, active_generation, last_completed_sync_at, time_zone, version
		  FROM calendar_connections WHERE user_id = $1`, userID).Scan(
		&id, &status, &c.SyncDeviceID, &c.ActiveGeneration, &last, &c.TimeZone, &c.Version)
	if isNoRows(err) {
		return connectionResponse{Status: "disconnected", Sources: []sourceJSON{}}, nil
	}
	if err != nil {
		return connectionResponse{}, err
	}
	c.Status = effectiveStatus(status, last, h.now())
	if last != nil {
		s := last.UTC().Format(time.RFC3339)
		c.LastCompletedSyncAt = &s
	}
	c.IsSyncDevice = c.SyncDeviceID != nil && *c.SyncDeviceID == deviceID
	c.Sources = []sourceJSON{}
	rows, err := q.Query(ctx, `
		SELECT source_key, enabled FROM calendar_source_selections
		 WHERE connection_id = $1 ORDER BY source_key`, id)
	if err != nil {
		return connectionResponse{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var s sourceJSON
		if err := rows.Scan(&s.Key, &s.Enabled); err != nil {
			return connectionResponse{}, err
		}
		c.Sources = append(c.Sources, s)
	}
	return c, rows.Err()
}

// getConnection은 본인 연결만 돌려준다. 연결이 없으면 disconnected와 version 0이다.
func (h *Handler) getConnection(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	c, err := h.loadConnection(r.Context(), h.pool, p.UserID, p.DeviceID)
	if err != nil {
		h.writeDomainError(w, r, "calendar.connection.get", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusOK, connectionEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Connection: c})
}

type putConnectionRequest struct {
	Status          *string       `json:"status"`
	TimeZone        *string       `json:"time_zone"`
	Sources         *[]sourceJSON `json:"sources"`
	ClaimSyncDevice bool          `json:"claim_sync_device"`
}

// clientSettable은 클라이언트가 PUT으로 정할 수 있는 상태다. syncing과 ready는 서버가 snapshot
// 생성·완료로만 정한다(설계 3 §5.2 "파랑").
var clientSettable = map[string]bool{
	"disconnected": true, "permission_required": true, "selecting": true, "denied": true,
	"restricted": true, "revoked": true, "needs_source_reselection": true, "error": true,
}

// transitionAllowed는 설계 3 §5.2 상태 그림을 그대로 옮긴 것이다.
func transitionAllowed(from, to string) bool {
	if from == to {
		return true
	}
	if to == "disconnected" {
		return true // 사용자가 연결을 끊는다.
	}
	switch from {
	case "disconnected":
		return to == "permission_required" || to == "selecting" || to == "denied" || to == "restricted"
	case "permission_required":
		return to == "selecting" || to == "denied" || to == "restricted"
	case "selecting":
		return to == "denied" || to == "restricted" || to == "revoked" || to == "permission_required"
	case "syncing", "ready":
		return to == "revoked" || to == "needs_source_reselection" || to == "error" || to == "denied" || to == "restricted"
	case "denied", "restricted", "revoked", "needs_source_reselection", "error":
		return to == "selecting" || to == "permission_required" || to == "revoked" || to == "denied" || to == "restricted" || to == "needs_source_reselection" || to == "error"
	}
	return false
}

// hidesProjections는 다른 멤버에게 projection을 보이면 안 되는 상태다(설계 3 §12).
func hidesProjections(status string) bool {
	switch status {
	case "denied", "restricted", "revoked", "needs_source_reselection", "disconnected", "permission_required", "selecting":
		return true
	}
	return false
}

// putConnection은 상태 전이, 시간대, 읽기 source 목록, sync 기기 지정을 갱신한다.
//
//   - If-Match가 없으면 create-only다. 연결이 없으면 만들고, 이미 있으면 409 version_conflict
//     (current_version에 현재 version)다. 공용 파서가 version 0을 받지 않으므로 생성은 If-Match 없이 한다.
//   - If-Match가 있으면 그 version과 현재 version이 같을 때만 갱신한다.
func (h *Handler) putConnection(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	createOnly := r.Header.Get("If-Match") == ""
	if !createOnly {
		var err error
		if prep.ExpectedVersion, err = mutation.ExpectedVersion(r); err != nil {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error())
			return
		}
	}
	var req putConnectionRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	if err := validateConnectionRequest(req); err != nil {
		h.writeDomainError(w, r, "calendar.connection.put", err)
		return
	}

	out, ok := h.run(w, r, p, prep, "calendar.connection.put", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		// 잠금 순서: 사용자의 활성 Party(FOR SHARE) -> connection(FOR UPDATE) -> 멤버십(KEY SHARE).
		// migration 00012의 잠금 순서를 따른다.
		if err := party.LockUserPartiesShared(ctx, tx, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		created := false
		if createOnly {
			tag, err := tx.Exec(ctx, `
				INSERT INTO calendar_connections (id, user_id) VALUES ($1, $2)
				ON CONFLICT (user_id) DO NOTHING`, uuid.New(), p.UserID)
			if err != nil {
				return mutation.Result{}, err
			}
			created = tag.RowsAffected() == 1
		}
		var (
			id      uuid.UUID
			status  string
			gen     int64
			version int64
			syncDev *uuid.UUID
		)
		err := tx.QueryRow(ctx, `
			SELECT id, source_status, active_generation, version, sync_device_id
			  FROM calendar_connections WHERE user_id = $1 FOR UPDATE`, p.UserID).Scan(&id, &status, &gen, &version, &syncDev)
		if isNoRows(err) {
			// If-Match가 있는데 연결이 없다. 낡은 version이다(현재 0).
			return mutation.Result{}, &mutation.VersionConflictError{Current: 0}
		}
		if err != nil {
			return mutation.Result{}, err
		}
		if createOnly && !created {
			// 이미 있는 연결을 create-only로 덮어쓰지 않는다.
			return mutation.Result{}, &mutation.VersionConflictError{Current: version}
		}
		if !createOnly {
			if err := mutation.CheckVersion(prep.ExpectedVersion, version); err != nil {
				return mutation.Result{}, err
			}
		}

		newStatus := status
		if req.Status != nil {
			if !transitionAllowed(status, *req.Status) {
				return mutation.Result{}, errStateInvalid
			}
			newStatus = *req.Status
		}
		if req.ClaimSyncDevice && (syncDev == nil || *syncDev != p.DeviceID) {
			// 기기를 바꾸면 새 기기의 full snapshot이 끝날 때까지 syncing이다(설계 3 §7.1). 마지막
			// 정상 데이터는 유지한다.
			if err := tx.QueryRow(ctx, `SELECT true FROM devices WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, p.DeviceID, p.UserID).Scan(new(bool)); err != nil {
				if isNoRows(err) {
					return mutation.Result{}, errForbidden
				}
				return mutation.Result{}, err
			}
			syncDev = &p.DeviceID
			if newStatus == "ready" {
				newStatus = "syncing"
			}
		}

		oldVisible := visible(status, gen)
		newVisible := visible(newStatus, gen)

		// 진행 중이던 snapshot은 상태가 syncing을 벗어나면 이어갈 수 없다. 열린 session을 폐기해 늦은
		// complete가 철회·거부된 연결을 되살리지 못하게 한다(complete도 상태를 다시 확인한다).
		if status == "syncing" && newStatus != "syncing" {
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_snapshot_facts_staging WHERE session_id IN (SELECT id FROM calendar_snapshot_sessions WHERE connection_id = $1 AND status = 'open')`, id); err != nil {
				return mutation.Result{}, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_snapshot_pages WHERE session_id IN (SELECT id FROM calendar_snapshot_sessions WHERE connection_id = $1 AND status = 'open')`, id); err != nil {
				return mutation.Result{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE calendar_snapshot_sessions SET status = 'aborted' WHERE connection_id = $1 AND status = 'open'`, id); err != nil {
				return mutation.Result{}, err
			}
		}

		if req.Sources != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_source_selections WHERE connection_id = $1`, id); err != nil {
				return mutation.Result{}, err
			}
			for _, s := range *req.Sources {
				if _, err := tx.Exec(ctx, `
					INSERT INTO calendar_source_selections (connection_id, source_key, enabled)
					VALUES ($1, $2, $3)`, id, s.Key, s.Enabled); err != nil {
					return mutation.Result{}, err
				}
			}
		}

		var revokedAt any
		if hidesProjections(newStatus) && newStatus != "disconnected" && newStatus != "selecting" && newStatus != "permission_required" {
			revokedAt = h.now()
		}
		if newStatus == "disconnected" {
			// 연결 해제는 facts도 지운다. 남기면 사용자가 끊었다고 믿는 데이터가 서버에 남는다.
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_busy_facts WHERE connection_id = $1`, id); err != nil {
				return mutation.Result{}, err
			}
			// 열려 있던 snapshot도 폐기한다(staging은 cascade로 함께 지워진다).
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_snapshot_sessions WHERE connection_id = $1`, id); err != nil {
				return mutation.Result{}, err
			}
		}
		tz := req.TimeZone
		var newVersion int64
		if err := tx.QueryRow(ctx, `
			UPDATE calendar_connections
			   SET source_status = $2, sync_device_id = $3,
			       time_zone = COALESCE($4, time_zone),
			       last_completed_sync_at = CASE WHEN $2 = 'disconnected' THEN NULL ELSE last_completed_sync_at END,
			       revoked_at = CASE WHEN $5::timestamptz IS NOT NULL THEN COALESCE(revoked_at, $5) ELSE NULL END,
			       version = version + 1, updated_at = now()
			 WHERE id = $1
			RETURNING version`, id, newStatus, syncDev, tz, revokedAt).Scan(&newVersion); err != nil {
			return mutation.Result{}, err
		}

		// 보이던 projection이 보이면 안 되는 상태가 되면 같은 트랜잭션에서 즉시 지우고 tombstone을
		// 보낸다(설계 3 §12). facts는 7일 유예 동안 남는다.
		if oldVisible != newVisible {
			if err := party.RebuildBusyProjections(ctx, tx, p.UserID, nil, true); err != nil {
				return mutation.Result{}, err
			}
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: "calendar_connection", ResourceID: id, ResourceVersion: newVersion}, nil
	})
	if !ok {
		return
	}
	c, err := h.loadConnection(r.Context(), h.pool, p.UserID, p.DeviceID)
	if err != nil {
		h.writeDomainError(w, r, "calendar.connection.put", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, out.StatusCode, connectionEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Connection: c})
}

func visible(status string, gen int64) bool {
	return gen > 0 && (status == "ready" || status == "syncing" || status == "error")
}

func validateConnectionRequest(req putConnectionRequest) error {
	if req.Status != nil && !clientSettable[*req.Status] {
		return errInvalidInput
	}
	if req.TimeZone != nil {
		if len(*req.TimeZone) == 0 || len(*req.TimeZone) > 64 {
			return errInvalidInput
		}
		if !validTimeZone(*req.TimeZone) {
			return errInvalidInput
		}
	}
	if req.Sources != nil {
		if len(*req.Sources) > maxSources {
			return errInvalidInput
		}
		seen := map[string]struct{}{}
		for _, s := range *req.Sources {
			if len(s.Key) == 0 || len(s.Key) > maxSourceKeyLen {
				return errInvalidInput
			}
			if _, dup := seen[s.Key]; dup {
				return errInvalidInput
			}
			seen[s.Key] = struct{}{}
		}
	}
	return nil
}
