package party

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/platform/mutation"
)

const (
	entityParty              = "party"
	entityPartyMembership    = "party_membership"
	entityVisibilitySetting  = "party_visibility_setting"
	entityScheduleProjection = "party_schedule_projection"
	entityPartyInvite        = "party_invite"
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
		id, partyID, userID       uuid.UUID
		role, status, displayName string
		version                   int64
		joinedAt                  time.Time
	)
	if err := q.QueryRow(ctx, `
		SELECT m.id, m.party_id, m.user_id, m.role, m.status, m.version, m.joined_at, u.display_name
		  FROM party_memberships m
		  JOIN users u ON u.id = m.user_id
		 WHERE m.id = $1`, membershipID).Scan(
		&id, &partyID, &userID, &role, &status, &version, &joinedAt, &displayName,
	); err != nil {
		return Projection{}, err
	}
	// display_name은 멤버 목록 화면이 sync만으로 그려지도록 싣는다(설계 4 §6 "표시 이름 공개"). 이름이 바뀌면
	// PropagateDisplayName이 멤버십 version을 올려 다시 보낸다.
	return Projection{EntityType: entityPartyMembership, ID: id, Version: version, Payload: map[string]any{
		"party_id": partyID.String(), "user_id": userID.String(), "display_name": displayName, "role": role,
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

// inviteTombstone은 초대 무효화·소진 tombstone이다. version은 마지막 upsert(used_count + 1)보다 1 크다
// (projection·설정의 "마지막 version + 1" 규칙과 같다). 초대는 다시 active가 되지 않으므로 하한을 남겨도 된다.
// 피드는 txid 순이라 먼저 직렬화된 수락의 upsert가 무효화 tombstone 뒤에 올 수 있는데, 그 upsert의 version은
// 이 값보다 낮아 하한에 막힌다.
func inviteTombstone(recipient, inviteID uuid.UUID, usedCount int16) mutation.Change {
	v := int64(usedCount) + 2
	return mutation.Change{Recipient: recipient, EntityType: entityPartyInvite, EntityID: inviteID, Version: &v, Tombstone: true}
}

// InviteProjection은 방장에게만 보내는 초대 투영이다. token과 token_hash는 넣지 않는다.
//
// party_invites에는 version 열이 없다(migration 00010). sync upsert는 entity_version이
// 필요하므로 used_count + 1을 쓴다. 초대의 upsert는 생성(0회 사용)과 수락(사용 횟수 증가)
// 뿐이고 각 수락은 used_count를 정확히 1 올리므로 단조 증가한다. 무효화·소진 tombstone은 이보다 1 큰
// version을 싣는다(inviteTombstone).
func InviteProjection(ctx context.Context, q Querier, inviteID uuid.UUID) (Projection, error) {
	var (
		id, partyID, createdBy uuid.UUID
		status                 string
		maxUses, usedCount     int16
		expiresAt, createdAt   time.Time
	)
	if err := q.QueryRow(ctx, `
		SELECT id, party_id, status, expires_at, max_uses, used_count, created_by_membership_id, created_at
		  FROM party_invites WHERE id = $1`, inviteID).Scan(
		&id, &partyID, &status, &expiresAt, &maxUses, &usedCount, &createdBy, &createdAt,
	); err != nil {
		return Projection{}, err
	}
	return Projection{EntityType: entityPartyInvite, ID: id, Version: int64(usedCount) + 1, Payload: map[string]any{
		"party_id": partyID.String(), "status": status,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"max_uses":   maxUses, "used_count": usedCount,
		"created_by_membership_id": createdBy.String(),
		"created_at":               createdAt.UTC().Format(time.RFC3339),
	}}, nil
}
