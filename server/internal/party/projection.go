package party

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	entityParty              = "party"
	entityPartyMembership    = "party_membership"
	entityVisibilitySetting  = "party_visibility_setting"
	entityScheduleProjection = "party_schedule_projection"
)

type Projection struct {
	EntityType string
	ID         uuid.UUID
	Version    int64
	Payload    map[string]any
}

type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func PartyProjection(ctx context.Context, q Querier, partyID uuid.UUID) (Projection, error) {
	var p partyResponse
	var createdAt, updatedAt time.Time
	if err := q.QueryRow(ctx, `
		SELECT id, name, status, version, member_limit, owner_membership_id,
		       created_by_user_id, created_at, updated_at
		  FROM parties WHERE id = $1`, partyID).Scan(
		&p.ID, &p.Name, &p.Status, &p.Version, &p.MemberLimit, &p.OwnerMembershipID,
		&p.CreatedByUserID, &createdAt, &updatedAt,
	); err != nil {
		return Projection{}, err
	}
	p.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	p.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return Projection{EntityType: entityParty, ID: p.ID, Version: p.Version, Payload: map[string]any{
		"name": p.Name, "status": p.Status, "member_limit": p.MemberLimit,
		"owner_membership_id": p.OwnerMembershipID.String(),
		"created_by_user_id":  p.CreatedByUserID.String(),
		"created_at":          p.CreatedAt,
		"updated_at":          p.UpdatedAt,
	}}, nil
}

func MembershipProjection(ctx context.Context, q Querier, membershipID uuid.UUID) (Projection, error) {
	var (
		id, partyID, userID uuid.UUID
		role, status        string
		version             int64
		joinedAt            time.Time
	)
	if err := q.QueryRow(ctx, `
		SELECT id, party_id, user_id, role, status, version, joined_at
		  FROM party_memberships WHERE id = $1`, membershipID).Scan(
		&id, &partyID, &userID, &role, &status, &version, &joinedAt,
	); err != nil {
		return Projection{}, err
	}
	return Projection{EntityType: entityPartyMembership, ID: id, Version: version, Payload: map[string]any{
		"party_id": partyID.String(), "user_id": userID.String(), "role": role,
		"status": status, "joined_at": joinedAt.UTC().Format(time.RFC3339),
	}}, nil
}

func VisibilitySettingProjection(ctx context.Context, q Querier, settingID uuid.UUID) (Projection, error) {
	var (
		id, partyID, userID uuid.UUID
		level               string
		shareLocation       bool
		version             int64
	)
	if err := q.QueryRow(ctx, `
		SELECT id, party_id, user_id, visibility_level, share_location, version
		  FROM party_visibility_settings WHERE id = $1`, settingID).Scan(
		&id, &partyID, &userID, &level, &shareLocation, &version,
	); err != nil {
		return Projection{}, err
	}
	return Projection{EntityType: entityVisibilitySetting, ID: id, Version: version, Payload: map[string]any{
		"party_id": partyID.String(), "user_id": userID.String(),
		"visibility_level": level, "share_location": shareLocation,
	}}, nil
}
