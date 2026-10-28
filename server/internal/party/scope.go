package party

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/mutation"
)

// emitScoped는 Party 범위 sync 변경을 수신자의 멤버십 ID(scope)와 함께 기록한다(account_backend_design.md
// §7.1 2026-10-28 개정). Party 범위 변경은 모두 이 함수를 지난다.
//
// scope는 수신자의 이 Party 멤버십 중 활성 멤버십, 없으면 가장 최근에 끝난 멤버십이다. 활성 멤버에게는 활성
// 멤버십이고, 방금 떠나거나 해산으로 끝난 수신자에게는 이 트랜잭션에서 끝낸 그 멤버십이다. joined_at은 생성
// 경로마다 DB 시계와 앱 시계가 섞여 쓰지 않는다. sync_changes 행이 수신자별이라 값도 수신자별로 정확하다.
func emitScoped(ctx context.Context, tx *mutation.Tx, partyID uuid.UUID, changes ...mutation.Change) error {
	if len(changes) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (user_id) user_id, id
		  FROM party_memberships
		 WHERE party_id = $1
		 ORDER BY user_id, (status = 'active') DESC, ended_at DESC NULLS LAST, id DESC`, partyID)
	if err != nil {
		return err
	}
	scopes := map[uuid.UUID]uuid.UUID{}
	for rows.Next() {
		var userID, id uuid.UUID
		if err := rows.Scan(&userID, &id); err != nil {
			rows.Close()
			return err
		}
		scopes[userID] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range changes {
		s, ok := scopes[changes[i].Recipient]
		if !ok {
			return fmt.Errorf("party: sync 수신자 %s에게 Party %s 멤버십이 없다", changes[i].Recipient, partyID)
		}
		changes[i].Scope = &s
	}
	return tx.EmitChanges(ctx, changes)
}
