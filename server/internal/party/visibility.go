package party

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/ratelimit"
)

const (
	visibilityDetails  = "details"
	visibilityBusyOnly = "busyOnly"
	visibilityHidden   = "hidden"

	detailsRefreshNotRequired = "not_required"
	detailsRefreshPending     = "pending_details_refresh"
)

type visibilitySettingResponse struct {
	PartyID             uuid.UUID `json:"party_id"`
	UserID              uuid.UUID `json:"user_id"`
	VisibilityLevel     string    `json:"visibility_level"`
	ShareLocation       bool      `json:"share_location"`
	Version             int64     `json:"version"`
	DetailsRefreshState string    `json:"details_refresh_state"`
	UpdatedAt           string    `json:"updated_at"`
}

type visibilityEnvelope struct {
	RequestID  string                    `json:"request_id"`
	Visibility visibilitySettingResponse `json:"visibility"`
}

type patchVisibilityRequest struct {
	VisibilityLevel string `json:"visibility_level"`
	ShareLocation   *bool  `json:"share_location"`
}

type visibilitySettingRow struct {
	id            uuid.UUID
	partyID       uuid.UUID
	userID        uuid.UUID
	level         string
	shareLocation bool
	version       int64
	readyVersion  *int64
	updatedAt     time.Time
}

func (h *Handler) registerVisibility(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/parties/{id}/visibility/me", require(http.HandlerFunc(h.getVisibilityMe)))
	mux.Handle("PATCH /v1/parties/{id}/visibility/me", require(http.HandlerFunc(h.patchVisibilityMe)))
}

func (h *Handler) getVisibilityMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	setting, err := loadVisibilityForMember(r.Context(), h.pool, partyID, p.UserID)
	if err != nil {
		h.writeDomainError(w, r, "party.visibility.get", err)
		return
	}
	writeVisibilityResponse(w, r, setting, http.StatusOK)
}

func (h *Handler) patchVisibilityMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	rawPartyID := r.PathValue("id")
	if !h.enforceVisibilityLimit(w, r, p.UserID, rawPartyID) {
		return
	}
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	if err := requireVisibilityMember(r.Context(), h.pool, partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.visibility.patch", err)
		return
	}
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	var req patchVisibilityRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	if !validVisibilityRequest(req) {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "공개 수준 설정이 올바르지 않다")
		return
	}

	var settingID uuid.UUID
	status := http.StatusOK
	out, ok := h.run(w, r, p, prep, "party.visibility.patch", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		current, err := lockVisibilityForMutation(ctx, tx, partyID, p.UserID)
		if err != nil {
			return mutation.Result{}, err
		}
		settingID = current.id
		if err := mutation.CheckVersion(prep.ExpectedVersion, current.version); err != nil {
			return mutation.Result{}, err
		}
		if current.level == req.VisibilityLevel && current.shareLocation == *req.ShareLocation {
			status = visibilityStatusCode(req.VisibilityLevel)
			if detailsRefreshState(current) == detailsRefreshNotRequired {
				status = http.StatusOK
			}
			return mutation.Result{StatusCode: status, ResourceType: entityVisibilitySetting, ResourceID: current.id, ResourceVersion: current.version}, nil
		}

		var version int64
		preserveReady := current.level == visibilityDetails && req.VisibilityLevel == visibilityDetails && !*req.ShareLocation && current.readyVersion != nil && *current.readyVersion == current.version
		if err := tx.QueryRow(ctx, `
			UPDATE party_visibility_settings
			   SET visibility_level = $3, share_location = $4,
			       details_ready_version = CASE WHEN $5::boolean THEN version + 1 ELSE NULL END,
			       version = version + 1, updated_at = now()
			 WHERE party_id = $1 AND user_id = $2
			RETURNING id, version`, partyID, p.UserID, req.VisibilityLevel, *req.ShareLocation, preserveReady).Scan(&settingID, &version); err != nil {
			return mutation.Result{}, err
		}
		if err := RebuildBusyProjections(ctx, tx, p.UserID, &partyID, false); err != nil {
			return mutation.Result{}, err
		}
		if err := emitProjection(ctx, tx, p.UserID, settingID); err != nil {
			return mutation.Result{}, err
		}
		status = visibilityStatusCode(req.VisibilityLevel)
		if preserveReady {
			status = http.StatusOK
		}
		return mutation.Result{StatusCode: status, ResourceType: entityVisibilitySetting, ResourceID: settingID, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	if out.Replayed {
		settingID = out.ResourceID
		status = out.StatusCode
	}
	setting, err := loadVisibilityByID(r.Context(), h.pool, settingID, p.UserID)
	if err != nil {
		h.writeDomainError(w, r, "party.visibility.patch", err)
		return
	}
	writeVisibilityResponse(w, r, setting, status)
}

func validVisibilityRequest(req patchVisibilityRequest) bool {
	if req.ShareLocation == nil {
		return false
	}
	switch req.VisibilityLevel {
	case visibilityDetails:
		return true
	case visibilityBusyOnly, visibilityHidden:
		return !*req.ShareLocation
	default:
		return false
	}
}

func visibilityStatusCode(level string) int {
	if level == visibilityDetails {
		return http.StatusAccepted
	}
	return http.StatusOK
}

func writeVisibilityResponse(w http.ResponseWriter, r *http.Request, s visibilitySettingRow, status int) {
	w.Header().Set("ETag", etag(s.version))
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, status, visibilityEnvelope{
		RequestID: httpapi.RequestID(r.Context()).String(),
		Visibility: visibilitySettingResponse{
			PartyID:             s.partyID,
			UserID:              s.userID,
			VisibilityLevel:     s.level,
			ShareLocation:       s.shareLocation,
			Version:             s.version,
			DetailsRefreshState: detailsRefreshState(s),
			UpdatedAt:           s.updatedAt.UTC().Format(time.RFC3339),
		},
	})
}

func detailsRefreshState(s visibilitySettingRow) string {
	if s.level == visibilityDetails && (s.readyVersion == nil || *s.readyVersion != s.version) {
		return detailsRefreshPending
	}
	return detailsRefreshNotRequired
}

func loadVisibilityForMember(ctx context.Context, q Querier, partyID, userID uuid.UUID) (visibilitySettingRow, error) {
	if err := requireVisibilityMember(ctx, q, partyID, userID); err != nil {
		return visibilitySettingRow{}, err
	}
	return queryVisibility(ctx, q, `
		SELECT id, party_id, user_id, visibility_level, share_location, version, details_ready_version, updated_at
		  FROM party_visibility_settings
		 WHERE party_id = $1 AND user_id = $2`, partyID, userID)
}

func loadVisibilityByID(ctx context.Context, q Querier, settingID, userID uuid.UUID) (visibilitySettingRow, error) {
	return queryVisibility(ctx, q, `
		SELECT id, party_id, user_id, visibility_level, share_location, version, details_ready_version, updated_at
		  FROM party_visibility_settings
		 WHERE id = $1 AND user_id = $2`, settingID, userID)
}

func queryVisibility(ctx context.Context, q Querier, sql string, args ...any) (visibilitySettingRow, error) {
	var s visibilitySettingRow
	err := q.QueryRow(ctx, sql, args...).Scan(
		&s.id, &s.partyID, &s.userID, &s.level, &s.shareLocation, &s.version, &s.readyVersion, &s.updatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return visibilitySettingRow{}, errNotFound
	}
	if err != nil {
		return visibilitySettingRow{}, err
	}
	return s, nil
}

func requireVisibilityMember(ctx context.Context, q Querier, partyID, userID uuid.UUID) error {
	var status string
	var hasHistory, active bool
	err := q.QueryRow(ctx, `
		SELECT p.status,
		       EXISTS (SELECT 1 FROM party_memberships WHERE party_id = p.id AND user_id = $2),
		       EXISTS (SELECT 1 FROM party_memberships WHERE party_id = p.id AND user_id = $2 AND status = 'active')
		  FROM parties p
		 WHERE p.id = $1`, partyID, userID).Scan(&status, &hasHistory, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if !hasHistory {
		return errNotFound
	}
	if status == "disbanded" {
		return errPartyDisbanded
	}
	if !active {
		return errNotFound
	}
	return nil
}

func lockVisibilityForMutation(ctx context.Context, tx *mutation.Tx, partyID, userID uuid.UUID) (visibilitySettingRow, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM parties WHERE id = $1 FOR NO KEY UPDATE`, partyID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return visibilitySettingRow{}, errNotFound
	}
	if err != nil {
		return visibilitySettingRow{}, err
	}
	if status == "disbanded" {
		return visibilitySettingRow{}, errPartyDisbanded
	}
	var membershipID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM party_memberships
		 WHERE party_id = $1 AND user_id = $2 AND status = 'active'
		   FOR KEY SHARE`, partyID, userID).Scan(&membershipID)
	if errors.Is(err, pgx.ErrNoRows) {
		return visibilitySettingRow{}, errNotFound
	}
	if err != nil {
		return visibilitySettingRow{}, err
	}
	return queryVisibility(ctx, tx, `
		SELECT id, party_id, user_id, visibility_level, share_location, version, details_ready_version, updated_at
		  FROM party_visibility_settings
		 WHERE party_id = $1 AND user_id = $2
		   FOR NO KEY UPDATE`, partyID, userID)
}

func (h *Handler) enforceVisibilityLimit(w http.ResponseWriter, r *http.Request, userID uuid.UUID, rawPartyID string) bool {
	subject := "user:" + userID.String() + ":party:" + rawPartyID
	d, ok := ratelimit.Enforce(w, r, h.limiter, ratelimit.ModeClosed, ratelimit.Rule{
		Scope:   ratelimit.ScopePartyVisibilityUpdate,
		Limit:   ratelimit.LimitPartyVisibilityUpdate.Count,
		Window:  ratelimit.LimitPartyVisibilityUpdate.Window,
		Subject: subject,
	})
	if !ok {
		if len(d.Exceeded) > 0 {
			ratelimit.RecordExceeded(r.Context(), h.pool, userID, d)
		}
		return false
	}
	return true
}
