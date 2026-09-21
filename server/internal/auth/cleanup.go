package auth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PruneSessions는 before에 이미 만료된 세션 중, 같은 token family에 살아 있는 세션이
// 없는 것을 batch개씩 지운다. 지운 수를 돌려준다. scheduler가 주기적으로 부른다.
//
// 만료만 보고 지우지 않는 이유: §5.2는 이미 쓴 refresh token이 다시 오면 family 전체를
// 폐기하라고 요구한다(Refresh의 used_at 분기). 쓴 row를 만료 즉시 지우면 그 token의
// 재사용이 "없는 세션"으로 보여 401만 나가고, 같은 family의 살아 있는 후속 세션은
// 폐기되지 않는다. family에 살아 있는 세션이 하나라도 있는 동안은 그 family의 옛 row를
// 모두 남긴다. 살아 있는 세션이 없으면 폐기할 것도 없으므로 지워도 잃는 것이 없다.
//
// users·devices를 잠그지 않는다. sessions row만 지우므로 잠금 규약
// (users → devices → sessions)과 충돌하지 않는다.
func PruneSessions(ctx context.Context, pool *pgxpool.Pool, before time.Time, batch int) (int64, error) {
	var total int64
	for {
		tag, err := pool.Exec(ctx, `
			DELETE FROM sessions WHERE id IN (
				SELECT s.id FROM sessions s
				 WHERE s.expires_at < $1
				   AND NOT EXISTS (
					SELECT 1 FROM sessions live
					 WHERE live.token_family_id = s.token_family_id
					   AND live.revoked_at IS NULL
					   AND live.expires_at >= $1)
				 LIMIT $2)`, before, batch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
	}
}
