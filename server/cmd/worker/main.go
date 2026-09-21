// Command worker는 비동기 작업 처리 프로세스다. account_backend_design.md §9의
// 역할 구분에서 APNs 발송, 계정 삭제, 명령 만료·재할당과 서버 측 재시도 상태
// 관리를 담당한다. EventKit 작업은 수행하지 않는다.
//
// job 점유·재시도·dead 처리는 internal/platform/jobs가 한다. 여기서는 job 종류를
// 등록하고 루프를 돌린다. 아직 등록된 종류가 없으므로 아무 job도 점유하지 않는다.
// 계정 삭제·APNs 발송 종류가 생기면 kinds()에 넣는다.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"plantogether/server/internal/platform/config"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/jobs"
	"plantogether/server/internal/platform/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker 종료: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.RoleWorker, config.FromEnv())
	if err != nil {
		return err
	}

	logger := httpapi.NewLogger(cfg.LogLevel, string(cfg.Role))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	logger.Info("worker 기동",
		slog.String("action", "startup"),
		slog.String("env", cfg.Env),
		slog.String("poll_interval", cfg.PollInterval.String()),
	)

	kinds := kinds()
	runner := &jobs.Runner{
		Pool:            pool.Pool(),
		Kinds:           kinds,
		Lease:           cfg.WorkerLease,
		BatchSize:       cfg.WorkerBatchSize,
		PollInterval:    cfg.PollInterval,
		ShutdownTimeout: cfg.ShutdownTimeout,
		Logger:          logger,
	}
	logger.Info("job 종류 등록",
		slog.String("action", "startup"),
		slog.Int("kinds", len(kinds)),
		slog.String("lease", cfg.WorkerLease.String()),
		slog.Int("batch_size", cfg.WorkerBatchSize),
	)

	if err := runner.Run(ctx); err != nil {
		return err
	}
	logger.Info("worker 종료 완료",
		slog.String("action", "shutdown"),
		slog.String("result", "success"),
	)
	return nil
}

// kinds는 이 worker가 처리하는 job 종류다. 여기 없는 종류는 점유하지 않는다
// (jobs.Claim 문서의 롤링 배포 이유).
func kinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{}
}
