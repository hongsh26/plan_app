package mutation

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PruneIdempotencyKeys는 before에 이미 만료된 idempotency 기록을 batch개씩 지운다.
// 지운 수를 돌려준다. scheduler가 주기적으로 부른다.
//
// 만료된 기록은 claimOrLoad가 어차피 무시하고 지운 뒤 다시 점유하므로, 여기서 먼저
// 지워도 동작이 달라지지 않는다. 동시에 같은 row를 지우면 claimOrLoad는 row가 없는
// 것을 보고 점유부터 다시 한다.
func PruneIdempotencyKeys(ctx context.Context, pool *pgxpool.Pool, before time.Time, batch int) (int64, error) {
	var total int64
	for {
		tag, err := pool.Exec(ctx, `
			DELETE FROM idempotency_keys WHERE id IN (
				SELECT id FROM idempotency_keys WHERE expires_at < $1 LIMIT $2)`, before, batch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
	}
}
