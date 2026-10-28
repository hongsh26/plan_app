package party

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BootstrapProjections returns only the Party state currently visible to userID.
// It runs inside the caller's snapshot transaction so its contents and cursor agree.
func BootstrapProjections(ctx context.Context, q Querier, userID uuid.UUID) ([]Projection, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id FROM parties p
		JOIN party_memberships mine ON mine.party_id = p.id
		WHERE mine.user_id = $1 AND mine.status = 'active' AND p.status = 'active'
		ORDER BY p.id`, userID)
	if err != nil {
		return nil, err
	}
	var partyIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		partyIDs = append(partyIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]Projection, 0)
	for _, partyID := range partyIDs {
		pr, err := PartyProjection(ctx, q, partyID)
		if err != nil {
			return nil, err
		}
		result = append(result, pr)

		members, err := q.Query(ctx, `SELECT id FROM party_memberships WHERE party_id = $1 AND status = 'active' ORDER BY id`, partyID)
		if err != nil {
			return nil, err
		}
		var memberIDs []uuid.UUID
		for members.Next() {
			var id uuid.UUID
			if err := members.Scan(&id); err != nil {
				members.Close()
				return nil, err
			}
			memberIDs = append(memberIDs, id)
		}
		members.Close()
		if err := members.Err(); err != nil {
			return nil, err
		}
		for _, id := range memberIDs {
			pr, err := MembershipProjection(ctx, q, id)
			if err != nil {
				return nil, err
			}
			result = append(result, pr)
		}

		var settingID uuid.UUID
		if err := q.QueryRow(ctx, `SELECT id FROM party_visibility_settings WHERE party_id = $1 AND user_id = $2`, partyID, userID).Scan(&settingID); err != nil {
			return nil, err
		}
		pr, err = VisibilitySettingProjection(ctx, q, settingID)
		if err != nil {
			return nil, err
		}
		result = append(result, pr)

		invites, err := q.Query(ctx, `SELECT i.id FROM party_invites i
			JOIN parties p ON p.id = i.party_id
			JOIN party_memberships owner ON owner.id = p.owner_membership_id
			WHERE i.party_id = $1 AND owner.user_id = $2 AND i.status = 'active'
			ORDER BY i.id`, partyID, userID)
		if err != nil {
			return nil, err
		}
		var inviteIDs []uuid.UUID
		for invites.Next() {
			var id uuid.UUID
			if err := invites.Scan(&id); err != nil {
				invites.Close()
				return nil, err
			}
			inviteIDs = append(inviteIDs, id)
		}
		invites.Close()
		if err := invites.Err(); err != nil {
			return nil, err
		}
		for _, id := range inviteIDs {
			pr, err := InviteProjection(ctx, q, id)
			if err != nil {
				return nil, err
			}
			result = append(result, pr)
		}

		projections, err := q.Query(ctx, `SELECT id, owner_user_id, visibility_level, start_at, end_at, all_day, time_zone, version
			FROM party_schedule_projections WHERE party_id = $1 ORDER BY id`, partyID)
		if err != nil {
			return nil, err
		}
		for projections.Next() {
			var id, owner uuid.UUID
			var level, tz string
			var start, end time.Time
			var allDay bool
			var version int64
			if err := projections.Scan(&id, &owner, &level, &start, &end, &allDay, &tz, &version); err != nil {
				projections.Close()
				return nil, err
			}
			result = append(result, Projection{EntityType: entityScheduleProjection, ID: id, Version: version, Payload: map[string]any{
				"party_id": partyID.String(), "owner_user_id": owner.String(), "visibility_level": level,
				"start_at": start.UTC().Format(time.RFC3339), "end_at": end.UTC().Format(time.RFC3339),
				"all_day": allDay, "time_zone": tz,
			}})
		}
		projections.Close()
		if err := projections.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}
