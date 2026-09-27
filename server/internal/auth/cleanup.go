package auth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionReuseRetention은 사용된 refresh token 해시를 재사용 탐지에 남기는 기간이다.
// §5.2에 따라 90일이 지난 token은 일반 무효 token으로 취급한다.
const SessionReuseRetention = 90 * 24 * time.Hour

// PruneSessions는 before에 이미 만료된 세션 중, 같은 token family에 살아 있는 세션이
// 없거나 재사용 탐지 보존 기간이 지난 것을 batch개씩 지운다. 지운 수를 돌려준다.
// scheduler가 주기적으로 부른다.
//
// 만료만 보고 지우지 않는 이유: §5.2는 이미 쓴 refresh token이 다시 오면 family 전체를
// 폐기하라고 요구한다(Refresh의 used_at 분기). 쓴 row를 만료 즉시 지우면 그 token의
// 재사용이 "없는 세션"으로 보여 401만 나가고, 같은 family의 살아 있는 후속 세션은
// 폐기되지 않는다. 다만 used_at부터 90일이 지나면 활성 family에 속해도 삭제해 row가
// 무한히 늘지 않게 한다. 그보다 오래된 token의 재사용은 일반 무효 token으로 처리한다.
// 살아 있는 세션이 없으면 폐기할 것도 없으므로 보존 기간 전에도 지워도 잃는 것이 없다.
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
				   AND (s.used_at < $2 OR NOT EXISTS (
					SELECT 1 FROM sessions live
					 WHERE live.token_family_id = s.token_family_id
					   AND live.revoked_at IS NULL
					   AND live.expires_at >= $1))
				 ORDER BY s.expires_at, s.id
				 LIMIT $3)`, before, before.Add(-SessionReuseRetention), batch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
	}
}
