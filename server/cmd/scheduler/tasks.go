package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/jobs"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/schedule"
	"plantogether/server/internal/syncfeed"
)

const (
	// cleanupBatch는 정리 DELETE 한 문장이 지우는 최대 row 수다. 긴 트랜잭션은 sync의
	// settled horizon을 붙잡으므로(§12 sync horizon lag) 작게 나눠 커밋한다.
	cleanupBatch = 1000

	// succeededJobRetention은 끝난 job을 남겨 두는 기간이다. 조사용이다.
	succeededJobRetention = 7 * 24 * time.Hour

	// stallThreshold보다 오래 점유되지 않은 job이 있으면 경보한다. worker가 멈췄거나
	// 아무 worker도 모르는 종류가 쌓이고 있다.
	stallThreshold = 15 * time.Minute
)

// tasks는 scheduler가 도는 주기 작업이다. 이름은 scheduled_tasks의 키이므로 바꾸지 않는다.
func tasks(pool *pgxpool.Pool, logger *slog.Logger) []schedule.Task {
	cleanup := func(name string, f func(ctx context.Context) (int64, error)) func(context.Context) error {
		return func(ctx context.Context) error {
			n, err := f(ctx)
			logger.Info("정리",
				slog.String("action", "task_"+name),
				slog.Int64("deleted", n))
			return err
		}
	}
	return []schedule.Task{
		{
			Name: "sync_prune", Interval: time.Hour, Timeout: 10 * time.Minute,
			Run: cleanup("sync_prune", func(ctx context.Context) (int64, error) {
				return syncfeed.Prune(ctx, pool, time.Now().Add(-syncfeed.Retention))
			}),
		},
		{
			Name: "session_prune", Interval: time.Hour, Timeout: 5 * time.Minute,
			Run: cleanup("session_prune", func(ctx context.Context) (int64, error) {
				return auth.PruneSessions(ctx, pool, time.Now(), cleanupBatch)
			}),
		},
		{
			Name: "idempotency_prune", Interval: time.Hour, Timeout: 5 * time.Minute,
			Run: cleanup("idempotency_prune", func(ctx context.Context) (int64, error) {
				return mutation.PruneIdempotencyKeys(ctx, pool, time.Now(), cleanupBatch)
			}),
		},
		{
			Name: "outbox_prune", Interval: time.Hour, Timeout: 5 * time.Minute,
			Run: cleanup("outbox_prune", func(ctx context.Context) (int64, error) {
				return jobs.PruneSucceeded(ctx, pool, time.Now().Add(-succeededJobRetention), cleanupBatch)
			}),
		},
		{
			Name: "outbox_stall_check", Interval: time.Minute, Timeout: 30 * time.Second,
			Run: func(ctx context.Context) error {
				n, err := jobs.StalledCount(ctx, pool, stallThreshold)
				if err != nil {
					return err
				}
				if n > 0 {
					// 경보 규칙은 이 로그(level=ERROR, result=stalled)를 잡는다.
					logger.Error("점유되지 않고 밀린 job이 있다",
						slog.String("action", "task_outbox_stall_check"),
						slog.String("result", "stalled"),
						slog.Int64("jobs", n))
				}
				return nil
			},
		},
	}
}
