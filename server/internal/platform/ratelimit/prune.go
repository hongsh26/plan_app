package ratelimit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Retention은 bucket을 남겨 두는 기간이다. 가장 긴 창이 1시간이므로 2시간이면 살아 있는
// 창을 지우지 않는다(설계 §8).
const Retention = 2 * time.Hour

// Execer는 pgxpool.Pool이 만족한다.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// maxPruneBatches는 한 번의 실행에서 도는 배치 수의 상한이다. 정리가 밀려도 한 실행이
// 무한히 길어지지 않는다.
const maxPruneBatches = 50

// Prune은 window_start가 retention보다 오래된 bucket을 배치로 지운다. 기준 시각은 DB
// 시각이다. 긴 트랜잭션은 sync의 settled horizon을 붙잡으므로 작게 나눠 커밋한다.
func Prune(ctx context.Context, db Execer, retention time.Duration, batch int) (int64, error) {
	var total int64
	for i := 0; i < maxPruneBatches; i++ {
		tag, err := db.Exec(ctx, `
			DELETE FROM rate_limit_buckets
			 WHERE ctid IN (
			       SELECT ctid FROM rate_limit_buckets
			        WHERE window_start < now() - make_interval(secs => $1::double precision)
			        LIMIT $2)`, retention.Seconds(), batch)
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
