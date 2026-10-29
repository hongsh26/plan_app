package party

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/platform/mutation"
)

// 설계 4 §5.8 계정 삭제 연동(계획 P8)이다.

const (
	// jobOwnerTransferredByDeletion은 계정 삭제의 강제 위임 알림이다(§9.3, dedupe party_id:party_version).
	jobOwnerTransferredByDeletion = "notify_owner_transferred_by_deletion"

	endReasonAccountDeleted = "account_deleted"
)

// EndMembershipsForDeletedUser는 삭제 대상 사용자의 활성 멤버십을 모두 정리한다. Party마다(party_id 순)
//   - 방장이고 승계자가 있으면 강제 위임(T4 코어, 알림 notify_owner_transferred_by_deletion) 뒤 종료한다.
//   - 방장인데 승계자가 없으면 해산(T7 코어)한다. 해산이 그 멤버십을 removed/party_disbanded로 끝내므로 종료 단계는 없다.
//   - 그 밖에는 T5 코어로 left/account_deleted 종료한다(projection·공개 설정 삭제와 tombstone 포함).
//
// 승계자는 삭제 대상을 뺀 활성 멤버 중 users.status='active'인 사람을 joined_at, user_id 순으로 고른 첫 번째다.
// 삭제가 진행 중인 사용자를 빼야 그 사용자의 삭제가 곧바로 재위임을 일으키는 연쇄가 생기지 않는다.
//
// 잠금: 호출자는 삭제 대상 users row를 FOR NO KEY UPDATE로 잡은 트랜잭션에서 부른다. 그 뒤 parties(FOR UPDATE)
// → 멤버십(id 순 FOR UPDATE) → 초대 순이며 T3·T4·T5와 같다. 승계자 후보의 users row는 잠그지 않는다. parties 뒤에
// 다른 사용자의 users를 잡으면 users → parties 순인 T3와 교착한다. 그래서 후보가 이 판정 직후 삭제를 요청하면
// 삭제 중인 사용자에게 넘어갈 수 있는데, 그 사용자의 삭제 작업이 같은 규칙으로 다시 위임하거나 해산한다.
//
// 이미 정리된 멤버십은 건너뛰므로 재시도해도 안전하다.
func EndMembershipsForDeletedUser(ctx context.Context, tx *mutation.Tx, userID uuid.UUID, now time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT m.party_id
		  FROM party_memberships m
		  JOIN parties p ON p.id = m.party_id
		 WHERE m.user_id = $1 AND m.status = 'active' AND p.status = 'active'
		 ORDER BY m.party_id`, userID)
	if err != nil {
		return err
	}
	partyIDs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, partyID := range partyIDs {
		if err := endMembershipForDeletedUser(ctx, tx, partyID, userID, now); err != nil {
			return err
		}
	}
	return nil
}

func endMembershipForDeletedUser(ctx context.Context, tx *mutation.Tx, partyID, userID uuid.UUID, now time.Time) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&status); err != nil {
		return err
	}
	if status != "active" {
		return nil
	}
	members, err := lockActiveMemberships(ctx, tx, partyID)
	if err != nil {
		return err
	}
	var mine *memberRef
	for i := range members {
		if members[i].userID == userID {
			mine = &members[i]
		}
	}
	if mine == nil {
		return nil
	}
	if mine.role == "owner" {
		successor, err := deletionSuccessor(ctx, tx, partyID, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err := disbandPartyTx(ctx, tx, partyID, members, nil, now)
			return err
		}
		if err != nil {
			return err
		}
		if _, err := transferOwnerTx(ctx, tx, partyID, *mine, successor, jobOwnerTransferredByDeletion, now); err != nil {
			return err
		}
	}
	return endMembershipTx(ctx, tx, partyID, members, *mine, "left", endReasonAccountDeleted, now)
}

// deletionSuccessor는 §5.8 승계자다. 멤버십 행은 호출자가 이미 잠갔다. 없으면 pgx.ErrNoRows다.
func deletionSuccessor(ctx context.Context, tx *mutation.Tx, partyID, deletedUserID uuid.UUID) (memberRef, error) {
	var m memberRef
	err := tx.QueryRow(ctx, `
		SELECT m.id, m.user_id, m.role
		  FROM party_memberships m
		  JOIN users u ON u.id = m.user_id
		 WHERE m.party_id = $1 AND m.status = 'active'
		   AND m.user_id <> $2 AND u.status = 'active'
		 ORDER BY m.joined_at, m.user_id
		 LIMIT 1`, partyID, deletedUserID).Scan(&m.id, &m.userID, &m.role)
	return m, err
}
