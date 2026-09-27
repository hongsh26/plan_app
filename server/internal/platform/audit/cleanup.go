package audit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TombstoneRetention은 §10 6단계의 삭제 계정 audit 보존 기간이다.
const TombstoneRetention = 90 * 24 * time.Hour

// PruneTombstones는 익명 tombstone으로 바뀐 audit 중 보존 기간이 지난 row를
// batch개씩 삭제한다. 익명화되지 않은 운영·보안 audit은 이 정책의 대상이 아니다.
func PruneTombstones(ctx context.Context, pool *pgxpool.Pool, before time.Time, batch int) (int64, error) {
	var total int64
	for {
		tag, err := pool.Exec(ctx, `
			DELETE FROM audit_events WHERE id IN (
				SELECT id FROM audit_events
				 WHERE actor_tombstone IS NOT NULL AND created_at < $1
				 ORDER BY created_at, id
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
