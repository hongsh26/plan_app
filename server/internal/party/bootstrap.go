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
		SELECT p.id, mine.id FROM parties p
		JOIN party_memberships mine ON mine.party_id = p.id
		WHERE mine.user_id = $1 AND mine.status = 'active' AND p.status = 'active'
		ORDER BY p.id`, userID)
	if err != nil {
		return nil, err
	}
	var partyIDs, scopes []uuid.UUID
	for rows.Next() {
		var id, scope uuid.UUID
		if err := rows.Scan(&id, &scope); err != nil {
			rows.Close()
			return nil, err
		}
		partyIDs = append(partyIDs, id)
		scopes = append(scopes, scope)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]Projection, 0)
	for i, partyID := range partyIDs {
		start := len(result)
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
		// 이 Party의 모든 entity는 호출자의 활성 멤버십을 scope로 갖는다.
		for j := start; j < len(result); j++ {
			result[j].Scope = scopes[i]
		}
	}
	return result, nil
}

// EndedMembershipFloors는 호출자가 since 이후 끝낸 멤버십을 bootstrap의 version 있는 tombstone 항목으로 준다
// (account_backend_design.md §7.2 2026-10-28 개정). snapshot을 교체하면 하한 기록이 비워지는데, 탈퇴는 snapshot에
// 반영됐지만 그보다 먼저 직렬화되고 txid가 watermark 뒤인 변경이 다시 오면 규칙 (a)가 그 멤버십이 끝났음을 알아야
// 한다. window는 변경 피드 보존 기간이다. 기준 시각은 DB now()다. horizon이 window 이상 묶이지 않는다고 가정한다
// (account_backend_design.md §12 horizon lag 경보가 30초에서 울린다).
func EndedMembershipFloors(ctx context.Context, q Querier, userID uuid.UUID, window time.Duration) ([]Projection, error) {
	rows, err := q.Query(ctx, `
		SELECT id, version FROM party_memberships
		 WHERE user_id = $1 AND status <> 'active' AND ended_at >= now() - make_interval(secs => $2)
		 ORDER BY id`, userID, window.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Projection
	for rows.Next() {
		var p Projection
		if err := rows.Scan(&p.ID, &p.Version); err != nil {
			return nil, err
		}
		p.EntityType, p.Scope, p.Tombstone = entityPartyMembership, p.ID, true
		out = append(out, p)
	}
	return out, rows.Err()
}
