package party

import (
	"context"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/mutation"
)

// PropagateDisplayName은 표시 이름 변경을 사용자가 속한 활성 Party의 멤버 전원에게 보낸다. 호출자는
// users row를 FOR NO KEY UPDATE로 잡고 이름을 바꾼 같은 트랜잭션에서 부른다.
//
// party_membership 투영이 display_name을 실으므로 그 version을 올려 upsert로 다시 보낸다. 멤버십
// version은 sync 투영용이라 요청 검증에 쓰지 않는다(설계 4 §5.5).
//
// 잠금: users → parties(FOR SHARE, id 순) → 본인 멤버십. T3도 users → parties 순이고 탈퇴·강퇴·해산은
// parties FOR UPDATE를 먼저 잡으므로 순서가 어긋나지 않는다. parties를 잡아 두면 동시 가입이 끝난 뒤에
// 수신자를 고르므로 신규 멤버가 옛 이름만 받는 일이 없다.
func PropagateDisplayName(ctx context.Context, tx *mutation.Tx, userID uuid.UUID) error {
	if err := LockUserPartiesShared(ctx, tx, userID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		UPDATE party_memberships m
		   SET version = m.version + 1, updated_at = now()
		  FROM parties p
		 WHERE p.id = m.party_id AND p.status = 'active'
		   AND m.user_id = $1 AND m.status = 'active'
		RETURNING m.id, m.party_id`, userID)
	if err != nil {
		return err
	}
	type bumped struct{ id, partyID uuid.UUID }
	var all []bumped
	for rows.Next() {
		var b bumped
		if err := rows.Scan(&b.id, &b.partyID); err != nil {
			rows.Close()
			return err
		}
		all = append(all, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range all {
		pr, err := MembershipProjection(ctx, tx, b.id)
		if err != nil {
			return err
		}
		recipients, err := activeMemberIDs(ctx, tx, b.partyID)
		if err != nil {
			return err
		}
		changes := make([]mutation.Change, 0, len(recipients))
		for _, r := range recipients {
			v := pr.Version
			changes = append(changes, mutation.Change{Recipient: r, EntityType: pr.EntityType, EntityID: pr.ID, Version: &v, Payload: pr.Payload})
		}
		if err := emitScoped(ctx, tx, b.partyID, changes...); err != nil {
			return err
		}
	}
	return nil
}
