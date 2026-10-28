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
)

// 설계 4 §5.5 T5(탈퇴)·T6(강퇴)·T7(해산)과 설계 5 §9의 cleanup이다.
//
// sync 규칙: 증분 sync 결과는 같은 시점의 bootstrap과 같아야 한다. bootstrap은 활성 Party와
// 활성 멤버십만 담으므로 종료된 멤버십과 해산된 Party는 upsert가 아니라 tombstone으로 보낸다.
//   - 실제로 끝나거나 삭제된 entity(종료 멤버십, 해산 Party, 삭제한 projection·설정)는 version이
//     있는 tombstone이다. 같은 ID가 되살아나지 않으므로 클라이언트가 version 하한을 기억해도 된다.
//   - 서버에는 그대로 있고 수신자의 scope에서만 빠지는 entity(떠난 사람이 보던 Party와 다른 멤버의
//     멤버십·projection)는 version 없는 tombstone이다. 재가입하면 같은 ID·같은 version으로 다시
//     내려오므로 version 하한을 남기면 그 upsert가 무시된다.

const (
	jobMemberLeft     = "notify_member_left"
	jobMemberRemoved  = "notify_member_removed"
	jobPartyDisbanded = "notify_party_disbanded"

	codeOwnerTransferRequired = "owner_transfer_required"

	endReasonLeftVoluntarily = "left_voluntarily"
	endReasonRemovedByOwner  = "removed_by_owner"
)

var (
	errOwnerTransferRequired = errors.New("party: 다른 멤버가 있는 방장은 위임 없이 탈퇴할 수 없다")
	errRemoveSelf            = errors.New("party: 방장은 자기 자신을 강퇴할 수 없다")
)

func (h *Handler) registerMembershipEnd(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("DELETE /v1/parties/{id}/memberships/me", require(http.HandlerFunc(h.leaveParty)))
	mux.Handle("DELETE /v1/parties/{id}/memberships/{user_id}", require(http.HandlerFunc(h.removeMemberByOwner)))
	mux.Handle("POST /v1/parties/{id}/disband", require(http.HandlerFunc(h.disbandByOwner)))
}

type membershipEnvelope struct {
	RequestID  string             `json:"request_id"`
	Membership membershipResponse `json:"membership"`
}

// leaveParty는 T5다. 오프라인 큐에 쌓일 수 있는 유일한 종료 요청이므로 §9.4에 따라 이미
// 비활성이면 현재 상태와 200을 돌려준다. Party가 해산됐으면(단독 방장 탈퇴의 재전송 포함) 410이며,
// 클라이언트는 둘 다 탈퇴 완료로 처리한다.
func (h *Handler) leaveParty(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	if err := requireMembershipHistory(r.Context(), h.pool, partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.leave", err)
		return
	}

	out, ok := h.run(w, r, p, prep, "party.leave", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		if _, err := lockPartyForMutation(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		members, err := lockActiveMemberships(ctx, tx, partyID)
		if err != nil {
			return mutation.Result{}, err
		}
		var mine *memberRef
		for i := range members {
			if members[i].userID == p.UserID {
				mine = &members[i]
			}
		}
		if mine == nil {
			// 이미 떠났다. 상태를 바꾸지 않고 마지막 멤버십을 돌려준다.
			id, version, err := latestMembership(ctx, tx, partyID, p.UserID)
			if err != nil {
				return mutation.Result{}, err
			}
			return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityPartyMembership, ResourceID: id, ResourceVersion: version}, nil
		}
		if mine.role == "owner" {
			if len(members) > 1 {
				return mutation.Result{}, errOwnerTransferRequired
			}
			// 단독 방장의 탈퇴는 해산이다. 그 멤버십만 left_voluntarily로 남기고 알림은 보내지 않는다.
			if _, err := disbandPartyTx(ctx, tx, partyID, members, &p.UserID, h.now()); err != nil {
				return mutation.Result{}, err
			}
		} else if err := endMembershipTx(ctx, tx, partyID, members, *mine, "left", endReasonLeftVoluntarily, h.now()); err != nil {
			return mutation.Result{}, err
		}
		var version int64
		if err := tx.QueryRow(ctx, `SELECT version FROM party_memberships WHERE id = $1`, mine.id).Scan(&version); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityPartyMembership, ResourceID: mine.id, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	h.writeMembership(w, r, "party.leave", out.ResourceID, out.StatusCode)
}

// removeMemberByOwner는 T6이다. 방장만 호출하며 If-Match가 parties.version과 같아야 한다.
func (h *Handler) removeMemberByOwner(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	// 재전송에서도 현재 권한을 먼저 평가한다(§9.4). 대상 파싱보다 앞서야 해산 Party의 과거 방장이
	// 잘못된 user_id로도 410을 받는다.
	if err := h.requireCurrentOwner(r.Context(), partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.remove_member", err)
		return
	}
	targetUserID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	if targetUserID == p.UserID {
		h.writeDomainError(w, r, "party.remove_member", errRemoveSelf)
		return
	}

	out, ok := h.run(w, r, p, prep, "party.remove_member", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
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
		members, err := lockActiveMemberships(ctx, tx, partyID)
		if err != nil {
			return mutation.Result{}, err
		}
		var target *memberRef
		for i := range members {
			if members[i].userID == targetUserID {
				target = &members[i]
			}
		}
		if target == nil {
			return mutation.Result{}, errNotFound
		}
		if err := endMembershipTx(ctx, tx, partyID, members, *target, "removed", endReasonRemovedByOwner, h.now()); err != nil {
			return mutation.Result{}, err
		}
		var version int64
		if err := tx.QueryRow(ctx, `SELECT version FROM party_memberships WHERE id = $1`, target.id).Scan(&version); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityPartyMembership, ResourceID: target.id, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	h.writeMembership(w, r, "party.remove_member", out.ResourceID, out.StatusCode)
}

// disbandByOwner는 T7이다. 응답은 해산된 Party다. 재전송은 현재 권한 평가에서 410을 받는다.
func (h *Handler) disbandByOwner(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	if err := h.requireCurrentOwner(r.Context(), partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.disband", err)
		return
	}

	out, ok := h.run(w, r, p, prep, "party.disband", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
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
		members, err := lockActiveMemberships(ctx, tx, partyID)
		if err != nil {
			return mutation.Result{}, err
		}
		version, err := disbandPartyTx(ctx, tx, partyID, members, nil, h.now())
		if err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityParty, ResourceID: partyID, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	party, err := loadPartyByID(r.Context(), h.pool, out.ResourceID)
	if err != nil {
		h.writeDomainError(w, r, "party.disband", err)
		return
	}
	writePartyResponse(w, r, party, out.StatusCode)
}

// endMembershipTx는 T5 step 4~9다(T6도 재사용). members는 종료 전 활성 멤버 전원이며 호출자가
// 잠근 상태여야 한다. 수신자 집합을 전이 전에 받아야 떠나는 사람도 tombstone을 받는다.
func endMembershipTx(ctx context.Context, tx *mutation.Tx, partyID uuid.UUID, members []memberRef, leaving memberRef, status, endReason string, now time.Time) error {
	var membershipVersion int64
	if err := tx.QueryRow(ctx, `
		UPDATE party_memberships
		   SET status = $2, ended_at = $3, end_reason = $4, version = version + 1, updated_at = $3
		 WHERE id = $1
		RETURNING version`, leaving.id, status, now, endReason).Scan(&membershipVersion); err != nil {
		return err
	}
	deletedProjections, err := deleteProjections(ctx, tx, partyID, &leaving.userID)
	if err != nil {
		return err
	}
	deletedSettings, err := deleteVisibilitySettings(ctx, tx, partyID, &leaving.userID)
	if err != nil {
		return err
	}
	var partyVersion int64
	if err := tx.QueryRow(ctx, `
		UPDATE parties SET version = version + 1, updated_at = $2
		 WHERE id = $1 RETURNING version`, partyID, now).Scan(&partyVersion); err != nil {
		return err
	}
	// 떠나는 사람이 아직 캐시에 둔 다른 멤버의 projection. 서버에는 남으므로 version 없이 지운다.
	others, err := projectionIDs(ctx, tx, partyID, leaving.userID)
	if err != nil {
		return err
	}

	var changes []mutation.Change
	for _, m := range members {
		if m.userID == leaving.userID {
			continue
		}
		v := membershipVersion
		changes = append(changes, mutation.Change{Recipient: m.userID, EntityType: entityPartyMembership, EntityID: leaving.id, Version: &v, Tombstone: true})
	}
	for _, d := range deletedProjections {
		for _, m := range members {
			changes = append(changes, versionedTombstone(m.userID, entityScheduleProjection, d))
		}
	}
	for _, d := range deletedSettings {
		changes = append(changes, versionedTombstone(d.owner, entityVisibilitySetting, d))
	}
	// 떠나는 사람의 Party scope 정리. 본인 멤버십은 끝났으므로 version을 붙인다.
	v := membershipVersion
	changes = append(changes,
		mutation.Change{Recipient: leaving.userID, EntityType: entityPartyMembership, EntityID: leaving.id, Version: &v, Tombstone: true},
		mutation.Change{Recipient: leaving.userID, EntityType: entityParty, EntityID: partyID},
	)
	for _, m := range members {
		if m.userID != leaving.userID {
			changes = append(changes, mutation.Change{Recipient: leaving.userID, EntityType: entityPartyMembership, EntityID: m.id})
		}
	}
	for _, id := range others {
		changes = append(changes, mutation.Change{Recipient: leaving.userID, EntityType: entityScheduleProjection, EntityID: id})
	}
	if err := emitScoped(ctx, tx, partyID, changes...); err != nil {
		return err
	}
	// 남은 멤버는 version이 오른 Party를 받는다.
	for _, m := range members {
		if m.userID == leaving.userID {
			continue
		}
		if err := emitProjection(ctx, tx, partyID, m.userID, partyID); err != nil {
			return err
		}
	}

	jobType := jobMemberLeft
	if endReason == endReasonRemovedByOwner {
		jobType = jobMemberRemoved
	}
	dedupe := leaving.id.String()
	// TODO(설계 7): 진행 중 제안에 membership_ended 신호를 발행한다. proposal 테이블이 아직 없다.
	return tx.Enqueue(ctx, mutation.Job{
		Type:      jobType,
		Payload:   map[string]string{"party_id": partyID.String(), "membership_id": leaving.id.String()},
		DedupeKey: &dedupe,
	})
}

// disbandPartyTx는 T7 step 2~8이다. members는 해산 직전 활성 멤버 전원이며 호출자가 잠근 상태여야
// 한다. leaverUserID가 있으면 단독 방장 탈퇴 경로다: 그 멤버십은 left_voluntarily로 남고 알림을
// 보내지 않는다(T5 step 3, T7 step 8). 반환값은 새 Party version이다.
func disbandPartyTx(ctx context.Context, tx *mutation.Tx, partyID uuid.UUID, members []memberRef, leaverUserID *uuid.UUID, now time.Time) (int64, error) {
	var partyVersion int64
	if err := tx.QueryRow(ctx, `
		UPDATE parties
		   SET status = 'disbanded', disbanded_at = $2, version = version + 1, updated_at = $2
		 WHERE id = $1
		RETURNING version`, partyID, now).Scan(&partyVersion); err != nil {
		return 0, err
	}
	mrows, err := tx.Query(ctx, `
		UPDATE party_memberships
		   SET status = CASE WHEN user_id = $3 THEN 'left' ELSE 'removed' END,
		       end_reason = CASE WHEN user_id = $3 THEN 'left_voluntarily' ELSE 'party_disbanded' END,
		       ended_at = $2, version = version + 1, updated_at = $2
		 WHERE party_id = $1 AND status = 'active'
		RETURNING id, version`, partyID, now, leaverUserID)
	if err != nil {
		return 0, err
	}
	// 멤버십은 삭제가 아니라 종료라 이미 올린 version을 그대로 tombstone에 쓴다.
	var endedMemberships []deletedEntity
	for mrows.Next() {
		var d deletedEntity
		if err := mrows.Scan(&d.id, &d.version); err != nil {
			mrows.Close()
			return 0, err
		}
		endedMemberships = append(endedMemberships, d)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return 0, err
	}
	deletedProjections, err := deleteProjections(ctx, tx, partyID, nil)
	if err != nil {
		return 0, err
	}
	deletedSettings, err := deleteVisibilitySettings(ctx, tx, partyID, nil)
	if err != nil {
		return 0, err
	}
	revoked, err := revokeActiveInvites(ctx, tx, partyID, now)
	if err != nil {
		return 0, err
	}

	// 수신자별로 끊어 발행한다. 멤버 10명 x projection 수천 건이 한 슬라이스에 쌓이지 않게 한다.
	for _, m := range members {
		var changes []mutation.Change
		v := partyVersion
		changes = append(changes, mutation.Change{Recipient: m.userID, EntityType: entityParty, EntityID: partyID, Version: &v, Tombstone: true})
		for _, d := range endedMemberships {
			v := d.version
			changes = append(changes, mutation.Change{Recipient: m.userID, EntityType: entityPartyMembership, EntityID: d.id, Version: &v, Tombstone: true})
		}
		for _, d := range deletedProjections {
			changes = append(changes, versionedTombstone(m.userID, entityScheduleProjection, d))
		}
		// 초대는 방장만 본다(§9.2).
		if m.role == "owner" {
			for _, inv := range revoked {
				changes = append(changes, inviteTombstone(m.userID, inv.id, inv.used))
			}
		}
		for _, d := range deletedSettings {
			if d.owner == m.userID {
				changes = append(changes, versionedTombstone(d.owner, entityVisibilitySetting, d))
			}
		}
		if err := emitScoped(ctx, tx, partyID, changes...); err != nil {
			return 0, err
		}
	}

	if leaverUserID == nil {
		// §9.3 표의 dedupe key는 party_id다. 해산은 종단 전이라 한 번만 일어난다.
		dedupe := partyID.String()
		if err := tx.Enqueue(ctx, mutation.Job{
			Type:      jobPartyDisbanded,
			Payload:   map[string]string{"party_id": partyID.String()},
			DedupeKey: &dedupe,
		}); err != nil {
			return 0, err
		}
	}
	// TODO(설계 7·8): party_disbanded 제안 신호, calendar_cleanup_suggestions, calendar_write_commands
	// 취소는 해당 테이블이 생길 때 이 트랜잭션에 추가한다.
	return partyVersion, nil
}

type deletedEntity struct {
	id      uuid.UUID
	owner   uuid.UUID
	version int64
}

// versionedTombstone은 삭제·종료된 entity의 tombstone이다. 마지막 upsert보다 1 큰 version을 붙여
// 늦게 도착한 낮은 version upsert를 이긴다(syncPartyProjections의 하향 삭제와 같은 규칙).
func versionedTombstone(recipient uuid.UUID, entityType string, d deletedEntity) mutation.Change {
	v := d.version + 1
	return mutation.Change{Recipient: recipient, EntityType: entityType, EntityID: d.id, Version: &v, Tombstone: true}
}

// lockActiveMemberships는 Party의 활성 멤버십을 id 순으로 FOR UPDATE 잠근다. parties 행을 잡은
// 뒤에 부른다(§5.5 잠금 순서). snapshot complete의 FOR KEY SHARE OF m과 충돌하므로 rebuild가
// 끝나는 멤버십에 projection을 다시 쓰지 못한다.
func lockActiveMemberships(ctx context.Context, tx *mutation.Tx, partyID uuid.UUID) ([]memberRef, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, user_id, role FROM party_memberships
		 WHERE party_id = $1 AND status = 'active'
		 ORDER BY id
		   FOR UPDATE`, partyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memberRef
	for rows.Next() {
		var m memberRef
		if err := rows.Scan(&m.id, &m.userID, &m.role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func deleteProjections(ctx context.Context, tx *mutation.Tx, partyID uuid.UUID, ownerUserID *uuid.UUID) ([]deletedEntity, error) {
	return scanDeleted(tx.Query(ctx, `
		DELETE FROM party_schedule_projections
		 WHERE party_id = $1 AND ($2::uuid IS NULL OR owner_user_id = $2)
		RETURNING id, owner_user_id, version`, partyID, ownerUserID))
}

func deleteVisibilitySettings(ctx context.Context, tx *mutation.Tx, partyID uuid.UUID, userID *uuid.UUID) ([]deletedEntity, error) {
	return scanDeleted(tx.Query(ctx, `
		DELETE FROM party_visibility_settings
		 WHERE party_id = $1 AND ($2::uuid IS NULL OR user_id = $2)
		RETURNING id, user_id, version`, partyID, userID))
}

func scanDeleted(rows pgx.Rows, err error) ([]deletedEntity, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deletedEntity
	for rows.Next() {
		var d deletedEntity
		if err := rows.Scan(&d.id, &d.owner, &d.version); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func projectionIDs(ctx context.Context, tx *mutation.Tx, partyID, excludeOwner uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT id FROM party_schedule_projections
		 WHERE party_id = $1 AND owner_user_id <> $2
		 ORDER BY id`, partyID, excludeOwner)
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

func latestMembership(ctx context.Context, q Querier, partyID, userID uuid.UUID) (uuid.UUID, int64, error) {
	var id uuid.UUID
	var version int64
	err := q.QueryRow(ctx, `
		SELECT id, version FROM party_memberships
		 WHERE party_id = $1 AND user_id = $2
		 ORDER BY joined_at DESC, id DESC
		 LIMIT 1`, partyID, userID).Scan(&id, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, errNotFound
	}
	return id, version, err
}

func loadPartyByID(ctx context.Context, q Querier, partyID uuid.UUID) (partyResponse, error) {
	var p partyResponse
	var createdAt, updatedAt time.Time
	err := q.QueryRow(ctx, `
		SELECT id, name, status, version, member_limit, owner_membership_id,
		       created_by_user_id, created_at, updated_at
		  FROM parties WHERE id = $1`, partyID).Scan(
		&p.ID, &p.Name, &p.Status, &p.Version, &p.MemberLimit, &p.OwnerMembershipID,
		&p.CreatedByUserID, &createdAt, &updatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return partyResponse{}, errNotFound
	}
	if err != nil {
		return partyResponse{}, err
	}
	p.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	p.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return p, nil
}

// requireMembershipHistory는 §5.5 "해산 확인" 순서다. 이력이 없으면 404, 해산이면 410이다.
func requireMembershipHistory(ctx context.Context, q Querier, partyID, userID uuid.UUID) error {
	var status string
	var history bool
	err := q.QueryRow(ctx, `
		SELECT p.status,
		       EXISTS (SELECT 1 FROM party_memberships WHERE party_id = p.id AND user_id = $2)
		  FROM parties p
		 WHERE p.id = $1`, partyID, userID).Scan(&status, &history)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !history) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if status == "disbanded" {
		return errPartyDisbanded
	}
	return nil
}

// writeMembership은 종료된 멤버십을 돌려준다. 호출자가 방금 종료했거나(탈퇴·강퇴) 이미 종료된
// 본인 멤버십이므로 활성 여부로 거르지 않는다.
func (h *Handler) writeMembership(w http.ResponseWriter, r *http.Request, action string, membershipID uuid.UUID, status int) {
	var m membershipResponse
	var joinedAt time.Time
	err := h.pool.QueryRow(r.Context(), `
		SELECT m.id, m.party_id, m.user_id, u.display_name, m.role, m.status, m.version, m.joined_at
		  FROM party_memberships m
		  JOIN users u ON u.id = m.user_id
		 WHERE m.id = $1`, membershipID).Scan(&m.ID, &m.PartyID, &m.UserID, &m.DisplayName, &m.Role, &m.Status, &m.Version, &joinedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
	}
	if err != nil {
		h.writeDomainError(w, r, action, err)
		return
	}
	m.JoinedAt = joinedAt.UTC().Format(time.RFC3339)
	httpapi.WriteJSON(w, status, membershipEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Membership: m})
}
