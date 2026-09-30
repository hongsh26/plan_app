package calendar

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// StagingRetention은 snapshot session과 staging을 남겨 두는 기간이다(설계 3 §11 "24시간").
// 열린 채 방치된 session도 같은 기준으로 지운다.
const StagingRetention = 24 * time.Hour

// Execer는 pgxpool.Pool이 만족한다.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const maxPruneBatches = 50

// Prune은 cutoff보다 오래된 snapshot session을 배치로 지운다. staging과 page row는 FK cascade로
// 함께 지워진다. 연결의 revision 단조성은 calendar_connections.last_snapshot_revision이 지키므로
// session row가 사라져도 지난 revision이 되살아나지 않는다.
func Prune(ctx context.Context, db Execer, cutoff time.Time, batch int) (int64, error) {
	var total int64
	for i := 0; i < maxPruneBatches; i++ {
		tag, err := db.Exec(ctx, `
			DELETE FROM calendar_snapshot_sessions
			 WHERE id IN (SELECT id FROM calendar_snapshot_sessions WHERE created_at < $1 LIMIT $2)`, cutoff, batch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			break
		}
	}
	return total, nil
}
