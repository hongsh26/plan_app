// Package party implements the P2 Party creation, lookup, update, and
// membership list endpoints from party_membership_design.md.
package party

import (
	"context"
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
	"plantogether/server/internal/platform/mutation"
)

const ReplayedHeader = "Idempotent-Replayed"

const (
	codePartyOwnedLimitReached  = "party_owned_limit_reached"
	codePartyJoinedLimitReached = "party_joined_limit_reached"
	codePartyDisbanded          = "party_disbanded"
)

var (
	errNotFound           = errors.New("party: 대상이 없다")
	errForbidden          = errors.New("party: 권한이 없다")
	errOwnedLimitReached  = errors.New("party: 소유 Party 한도에 도달했다")
	errJoinedLimitReached = errors.New("party: 참여 Party 한도에 도달했다")
	errPartyDisbanded     = errors.New("party: 해산된 Party다")
)

type Handler struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	now    func() time.Time
}

type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Now    func() time.Time
}

func NewHandler(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{pool: d.Pool, logger: d.Logger, now: d.Now}
}

func (h *Handler) Register(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/parties", require(http.HandlerFunc(h.createParty)))
	mux.Handle("GET /v1/parties/{id}", require(http.HandlerFunc(h.getParty)))
	mux.Handle("PATCH /v1/parties/{id}", require(http.HandlerFunc(h.patchParty)))
	mux.Handle("GET /v1/parties/{id}/memberships", require(http.HandlerFunc(h.listMemberships)))
}

type partyResponse struct {
	ID                uuid.UUID `json:"id"`
	Name              string    `json:"name"`
	Status            string    `json:"status"`
	Version           int64     `json:"version"`
	MemberLimit       int16     `json:"member_limit"`
	OwnerMembershipID uuid.UUID `json:"owner_membership_id"`
	CreatedByUserID   uuid.UUID `json:"created_by_user_id"`
	CreatedAt         string    `json:"created_at"`
	UpdatedAt         string    `json:"updated_at"`
}

type partyEnvelope struct {
	RequestID string        `json:"request_id"`
	Party     partyResponse `json:"party"`
}

type createPartyRequest struct {
	Name string `json:"name"`
}

func (h *Handler) createParty(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	var req createPartyRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	name, ok := NormalizeName(req.Name)
	if !ok {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "Party 이름이 올바르지 않다")
		return
	}

	var createdID uuid.UUID
	out, ok := h.run(w, r, p, prep, "party.create", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		if err := lockUserForPartyCreate(ctx, tx, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if err := checkCreateLimits(ctx, tx, p.UserID); err != nil {
			return mutation.Result{}, err
		}

		partyID := uuid.New()
		membershipID := uuid.New()
		settingID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO parties (id, name, owner_membership_id, created_by_user_id)
			VALUES ($1, $2, $3, $4)`, partyID, name, membershipID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO party_memberships (id, party_id, user_id, role)
			VALUES ($1, $2, $3, 'owner')`, membershipID, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO party_visibility_settings (id, party_id, user_id, visibility_level)
			VALUES ($1, $2, $3, 'busyOnly')`, settingID, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if err := createBusyOnlyProjections(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		for _, id := range []uuid.UUID{partyID, membershipID, settingID} {
			if err := emitProjection(ctx, tx, p.UserID, id); err != nil {
				return mutation.Result{}, err
			}
		}
		createdID = partyID
		return mutation.Result{StatusCode: http.StatusCreated, ResourceType: entityParty, ResourceID: partyID, ResourceVersion: 1}, nil
	})
	if !ok {
		return
	}
	if out.Replayed {
		createdID = out.ResourceID
	}
	h.writePartyForMember(w, r, createdID, p.UserID, out.StatusCode)
}

func (h *Handler) getParty(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	h.writePartyForMember(w, r, partyID, p.UserID, http.StatusOK)
}

type patchPartyRequest struct {
	Name string `json:"name"`
}

func (h *Handler) patchParty(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	var req patchPartyRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	name, ok := NormalizeName(req.Name)
	if !ok {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "Party 이름이 올바르지 않다")
		return
	}
	if err := h.requireCurrentOwner(r.Context(), partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.update", err)
		return
	}

	out, ok := h.run(w, r, p, prep, "party.update", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		current, err := lockPartyForMutation(ctx, tx, partyID, p.UserID)
		if err != nil {
			return mutation.Result{}, err
		}
		if err := requireOwner(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if err := mutation.CheckVersion(prep.ExpectedVersion, current); err != nil {
			return mutation.Result{}, err
		}
		var version int64
		if err := tx.QueryRow(ctx, `
			UPDATE parties
			   SET name = $2, version = version + 1, updated_at = now()
			 WHERE id = $1
			RETURNING version`, partyID, name).Scan(&version); err != nil {
			return mutation.Result{}, err
		}
		recipients, err := activeMemberIDs(ctx, tx, partyID)
		if err != nil {
			return mutation.Result{}, err
		}
		for _, recipient := range recipients {
			if err := emitProjection(ctx, tx, recipient, partyID); err != nil {
				return mutation.Result{}, err
			}
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityParty, ResourceID: partyID, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	h.writePartyForOwner(w, r, out.ResourceID, p.UserID, out.StatusCode)
}

type membershipResponse struct {
	ID          uuid.UUID `json:"id"`
	PartyID     uuid.UUID `json:"party_id"`
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	Version     int64     `json:"version"`
	JoinedAt    string    `json:"joined_at"`
}

type membershipsEnvelope struct {
	RequestID   string               `json:"request_id"`
	Memberships []membershipResponse `json:"memberships"`
}

func (h *Handler) listMemberships(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		WITH visible_party AS (
			SELECT p.id
			  FROM parties p
			  JOIN party_memberships caller
			    ON caller.party_id = p.id
			   AND caller.user_id = $2
			   AND caller.status = 'active'
			 WHERE p.id = $1 AND p.status = 'active'
		)
		SELECT m.id, m.party_id, m.user_id, u.display_name, m.role, m.status, m.version, m.joined_at
		  FROM party_memberships m
		  JOIN users u ON u.id = m.user_id
		 WHERE m.party_id = (SELECT id FROM visible_party) AND m.status = 'active'
		 ORDER BY m.joined_at, m.id`, partyID, p.UserID)
	if err != nil {
		h.writeDomainError(w, r, "party.list_memberships", err)
		return
	}
	defer rows.Close()
	out := membershipsEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Memberships: []membershipResponse{}}
	for rows.Next() {
		var m membershipResponse
		var joinedAt time.Time
		if err := rows.Scan(&m.ID, &m.PartyID, &m.UserID, &m.DisplayName, &m.Role, &m.Status, &m.Version, &joinedAt); err != nil {
			h.writeDomainError(w, r, "party.list_memberships", err)
			return
		}
		m.JoinedAt = joinedAt.UTC().Format(time.RFC3339)
		out.Memberships = append(out.Memberships, m)
	}
	if err := rows.Err(); err != nil {
		h.writeDomainError(w, r, "party.list_memberships", err)
		return
	}
	if len(out.Memberships) == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
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
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "권한이 없다")
	case errors.Is(err, errOwnedLimitReached):
		httpapi.WriteError(w, r, http.StatusConflict, codePartyOwnedLimitReached, "소유한 Party 한도에 도달했다")
	case errors.Is(err, errJoinedLimitReached):
		httpapi.WriteError(w, r, http.StatusConflict, codePartyJoinedLimitReached, "참여한 Party 한도에 도달했다")
	case errors.Is(err, errPartyDisbanded):
		httpapi.WriteError(w, r, http.StatusGone, codePartyDisbanded, "이미 해산된 Party다")
	default:
		h.logger.ErrorContext(r.Context(), "Party 요청 처리 실패",
			slog.String("request_id", httpapi.RequestID(r.Context()).String()),
			slog.String("action", action),
			slog.String("result", "failure"),
			slog.String("error_type", httpapi.ErrorType(err)),
		)
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
	}
}

func (h *Handler) writePartyForMember(w http.ResponseWriter, r *http.Request, partyID, userID uuid.UUID, status int) {
	p, err := loadPartyForMember(r.Context(), h.pool, partyID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	if err != nil {
		h.writeDomainError(w, r, "party.get", err)
		return
	}
	writePartyResponse(w, r, p, status)
}

func (h *Handler) writePartyForOwner(w http.ResponseWriter, r *http.Request, partyID, userID uuid.UUID, status int) {
	p, err := loadPartyForOwner(r.Context(), h.pool, partyID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := h.requireCurrentOwner(r.Context(), partyID, userID); err != nil {
			h.writeDomainError(w, r, "party.get", err)
			return
		}
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	if err != nil {
		h.writeDomainError(w, r, "party.get", err)
		return
	}
	writePartyResponse(w, r, p, status)
}

func loadPartyForOwner(ctx context.Context, q Querier, partyID, userID uuid.UUID) (partyResponse, error) {
	var p partyResponse
	var createdAt, updatedAt time.Time
	err := q.QueryRow(ctx, `
		SELECT p.id, p.name, p.status, p.version, p.member_limit, p.owner_membership_id,
		       p.created_by_user_id, p.created_at, p.updated_at
		  FROM parties p
		  JOIN party_memberships m
		    ON m.party_id = p.id
		   AND m.user_id = $2
		   AND m.status = 'active'
		   AND m.role = 'owner'
		 WHERE p.id = $1 AND p.status = 'active'`, partyID, userID).Scan(
		&p.ID, &p.Name, &p.Status, &p.Version, &p.MemberLimit, &p.OwnerMembershipID,
		&p.CreatedByUserID, &createdAt, &updatedAt,
	)
	if err != nil {
		return partyResponse{}, err
	}
	p.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	p.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return p, nil
}

func loadPartyForMember(ctx context.Context, q Querier, partyID, userID uuid.UUID) (partyResponse, error) {
	var p partyResponse
	var createdAt, updatedAt time.Time
	err := q.QueryRow(ctx, `
		SELECT p.id, p.name, p.status, p.version, p.member_limit, p.owner_membership_id,
		       p.created_by_user_id, p.created_at, p.updated_at
		  FROM parties p
		  JOIN party_memberships m
		    ON m.party_id = p.id
		   AND m.user_id = $2
		   AND m.status = 'active'
		 WHERE p.id = $1 AND p.status = 'active'`, partyID, userID).Scan(
		&p.ID, &p.Name, &p.Status, &p.Version, &p.MemberLimit, &p.OwnerMembershipID,
		&p.CreatedByUserID, &createdAt, &updatedAt,
	)
	if err != nil {
		return partyResponse{}, err
	}
	p.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	p.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return p, nil
}

func writePartyResponse(w http.ResponseWriter, r *http.Request, p partyResponse, status int) {
	w.Header().Set("ETag", etag(p.Version))
	httpapi.WriteJSON(w, status, partyEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Party: p})
}

func (h *Handler) requireCurrentOwner(ctx context.Context, partyID, userID uuid.UUID) error {
	var (
		status  string
		history bool
		owner   bool
	)
	err := h.pool.QueryRow(ctx, `
		SELECT p.status,
		       EXISTS (
		           SELECT 1 FROM party_memberships
		            WHERE party_id = p.id AND user_id = $2
		       ),
		       EXISTS (
		           SELECT 1 FROM party_memberships
		            WHERE party_id = p.id AND user_id = $2
		              AND status = 'active' AND role = 'owner'
		       )
		  FROM parties p
		 WHERE p.id = $1`, partyID, userID).Scan(&status, &history, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if !history {
		return errNotFound
	}
	if status == "disbanded" {
		return errPartyDisbanded
	}
	if !owner {
		return errForbidden
	}
	return nil
}

func parsePartyID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return uuid.Nil, false
	}
	return id, true
}

func etag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

func lockUserForPartyCreate(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&status); err != nil {
		return err
	}
	if status != "active" {
		return auth.ErrAccountLocked
	}
	return nil
}

func checkCreateLimits(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	var owned int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM party_memberships m
		  JOIN parties p ON p.id = m.party_id
		 WHERE m.user_id = $1 AND m.role = 'owner' AND m.status = 'active'
		   AND p.status = 'active'`, userID).Scan(&owned); err != nil {
		return err
	}
	if owned >= 10 {
		return errOwnedLimitReached
	}
	var joined int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM party_memberships m
		  JOIN parties p ON p.id = m.party_id
		 WHERE m.user_id = $1 AND m.status = 'active' AND p.status = 'active'`, userID).Scan(&joined); err != nil {
		return err
	}
	if joined >= 20 {
		return errJoinedLimitReached
	}
	return nil
}

func createBusyOnlyProjections(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ uuid.UUID) error {
	// calendar_busy_facts is introduced by the calendar sync slice. Until that
	// table exists, P2 creates zero projections and later snapshot completion
	// will populate them.
	return nil
}

func emitProjection(ctx context.Context, tx *mutation.Tx, recipient, entityID uuid.UUID) error {
	var (
		p   Projection
		err error
	)
	if p, err = PartyProjection(ctx, tx, entityID); err == nil {
		return tx.EmitChange(ctx, mutation.Change{Recipient: recipient, EntityType: p.EntityType, EntityID: p.ID, Version: &p.Version, Payload: p.Payload})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if p, err = MembershipProjection(ctx, tx, entityID); err == nil {
		return tx.EmitChange(ctx, mutation.Change{Recipient: recipient, EntityType: p.EntityType, EntityID: p.ID, Version: &p.Version, Payload: p.Payload})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	p, err = VisibilitySettingProjection(ctx, tx, entityID)
	if err != nil {
		return err
	}
	return tx.EmitChange(ctx, mutation.Change{Recipient: recipient, EntityType: p.EntityType, EntityID: p.ID, Version: &p.Version, Payload: p.Payload})
}

func requireActiveMember(ctx context.Context, q Querier, partyID, userID uuid.UUID) error {
	var status string
	err := q.QueryRow(ctx, `
		SELECT p.status
		  FROM parties p
		  JOIN party_memberships m ON m.party_id = p.id
		 WHERE p.id = $1 AND m.user_id = $2 AND m.status = 'active'`, partyID, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return errNotFound
	}
	return nil
}

func lockPartyForMutation(ctx context.Context, tx pgx.Tx, partyID, userID uuid.UUID) (int64, error) {
	var status string
	var version int64
	err := tx.QueryRow(ctx, `SELECT status, version FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound
	}
	if err != nil {
		return 0, err
	}
	var seen bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM party_memberships WHERE party_id = $1 AND user_id = $2
		)`, partyID, userID).Scan(&seen); err != nil {
		return 0, err
	}
	if !seen {
		return 0, errNotFound
	}
	if status == "disbanded" {
		return 0, errPartyDisbanded
	}
	return version, nil
}

func requireOwner(ctx context.Context, q Querier, partyID, userID uuid.UUID) error {
	var ok bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM party_memberships
			 WHERE party_id = $1 AND user_id = $2
			   AND status = 'active' AND role = 'owner'
		)`, partyID, userID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errForbidden
	}
	return nil
}

func activeMemberIDs(ctx context.Context, q Querier, partyID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT user_id FROM party_memberships
		 WHERE party_id = $1 AND status = 'active'
		 ORDER BY joined_at, id`, partyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
