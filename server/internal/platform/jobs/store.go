package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Claim은 실행할 때가 된 job을 limit개까지 점유한다. types에 있는 종류만 본다.
//
// 종류를 거르는 이유: 모르는 종류를 점유하면 할 수 있는 일이 재시도나 dead뿐이다.
// 새 api가 먼저 배포되어 옛 worker가 새 종류를 보는 순간(롤링 배포) dead로 보내면
// 작업이 사라진다. 점유하지 않고 두면 새 worker가 처리하고, 끝내 처리되지 않으면
// scheduler의 정체 감시가 경보를 낸다(StalledCount).
//
// 조건은 outbox_jobs_claim_idx의 partial index 조건을 그대로 적는다. 그래야 planner가
// 그 index를 쓴다.
func Claim(ctx context.Context, pool *pgxpool.Pool, types []string, limit int, lease time.Duration) ([]Job, error) {
	if len(types) == 0 || limit <= 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		WITH c AS (
			SELECT id FROM outbox_jobs
			 WHERE status IN ('pending', 'retryable_failed', 'running')
			   AND next_run_at <= now()
			   AND (status <> 'running' OR locked_until <= now())
			   AND type = ANY($1)
			 ORDER BY next_run_at
			 LIMIT $2
			   FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_jobs j
		   SET status = 'running',
		       locked_until = now() + make_interval(secs => $3),
		       attempt_count = j.attempt_count + 1,
		       updated_at = now()
		  FROM c
		 WHERE j.id = c.id
		RETURNING j.id, j.type, j.payload, j.attempt_count`,
		types, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) {
		var j Job
		err := r.Scan(&j.ID, &j.Type, &j.Payload, &j.Attempt)
		return j, err
	})
}

// ErrLeaseLost는 완료를 기록하려 했는데 job이 더 이상 이 시도의 것이 아닐 때다.
// lease가 지나 다른 worker가 다시 점유했거나 이미 끝난 경우다.
var ErrLeaseLost = errors.New("jobs: lease를 잃었다")

// fenced는 (id, running, attempt_count)가 이 시도와 같을 때만 row를 바꾼다.
const fenced = `id = $1 AND status = 'running' AND attempt_count = $2`

func expectOne(tag interface{ RowsAffected() int64 }, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Succeed는 job을 succeeded로 바꾼다.
func Succeed(ctx context.Context, pool *pgxpool.Pool, j Job) error {
	return expectOne(pool.Exec(ctx, `
		UPDATE outbox_jobs
		   SET status = 'succeeded', locked_until = NULL, last_error = NULL, updated_at = now()
		 WHERE `+fenced, j.ID, j.Attempt))
}

// Retry는 job을 retryable_failed로 바꾸고 after 뒤에 다시 점유되게 한다.
func Retry(ctx context.Context, pool *pgxpool.Pool, j Job, after time.Duration, cause error) error {
	return expectOne(pool.Exec(ctx, `
		UPDATE outbox_jobs
		   SET status = 'retryable_failed',
		       next_run_at = now() + make_interval(secs => $3),
		       locked_until = NULL,
		       last_error = $4,
		       updated_at = now()
		 WHERE `+fenced, j.ID, j.Attempt, after.Seconds(), errorText(cause)))
}

// Kill은 job을 dead로 바꾼다. onDead가 있으면 같은 트랜잭션에서 부른다.
func Kill(ctx context.Context, pool *pgxpool.Pool, j Job, cause error, onDead func(context.Context, pgx.Tx, Job) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := expectOne(tx.Exec(ctx, `
			UPDATE outbox_jobs
			   SET status = 'dead', locked_until = NULL, last_error = $3, updated_at = now()
			 WHERE `+fenced, j.ID, j.Attempt, errorText(cause))); err != nil {
			return err
		}
		if onDead == nil {
			return nil
		}
		if err := onDead(ctx, tx, j); err != nil {
			return fmt.Errorf("jobs: dead 처리 실패: %w", err)
		}
		return nil
	})
}

// Release는 끝내지 못한 시도의 lease를 반납한다. job은 곧바로 다시 점유될 수 있다.
//
// worker 종료로 중단된 시도는 job의 탓이 아니므로 attempt_count를 되돌린다. handler가
// 취소에 응하지 않으면 Runner.Run은 그 handler를 기다리고, 오케스트레이터가 프로세스를
// 강제 종료하면 Release가 불리지 않는다. 그 시도는 lease 만료로 끝나며 센다(poison pill이
// 무한히 되풀이되지 않게).
func Release(ctx context.Context, pool *pgxpool.Pool, j Job) error {
	return expectOne(pool.Exec(ctx, `
		UPDATE outbox_jobs
		   SET status = 'retryable_failed',
		       next_run_at = now(),
		       locked_until = NULL,
		       attempt_count = attempt_count - 1,
		       updated_at = now()
		 WHERE `+fenced, j.ID, j.Attempt))
}

// StalledCount는 실행할 때가 olderThan보다 더 지났는데 점유되지 않은 job 수다.
// worker가 멈췄거나 아무 worker도 모르는 종류가 쌓이는 것을 잡는다. scheduler가 경보로 쓴다.
func StalledCount(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_jobs
		 WHERE status IN ('pending', 'retryable_failed', 'running')
		   AND next_run_at <= now() - make_interval(secs => $1)
		   AND (status <> 'running' OR locked_until <= now())`,
		olderThan.Seconds()).Scan(&n)
	return n, err
}

// PruneSucceeded는 before보다 먼저 끝난 succeeded job을 batch개씩 지운다. 지운 수를 돌려준다.
// dead는 운영 확인 대상이므로 지우지 않는다.
func PruneSucceeded(ctx context.Context, pool *pgxpool.Pool, before time.Time, batch int) (int64, error) {
	var total int64
	for {
		tag, err := pool.Exec(ctx, `
			DELETE FROM outbox_jobs WHERE id IN (
				SELECT id FROM outbox_jobs
				 WHERE status = 'succeeded' AND updated_at < $1
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
