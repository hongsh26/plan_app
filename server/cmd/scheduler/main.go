// Command scheduler는 주기 작업 프로세스다. account_backend_design.md §9의
// 역할 구분에서 만료 초대, 알림 예약, 장기 실행 작업 복구, 삭제 작업 시작을
// 담당한다. §12에 따라 단일 leader로 돌되 작업 중복은 DB lease로 막는다.
//
// 작업 점유와 주기 관리는 internal/platform/schedule이 한다. 작업 목록은 tasks.go에 있다.
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
	"plantogether/server/internal/platform/postgres"
	"plantogether/server/internal/platform/schedule"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "scheduler 종료: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.RoleScheduler, config.FromEnv())
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

	logger.Info("scheduler 기동",
		slog.String("action", "startup"),
		slog.String("env", cfg.Env),
		slog.String("poll_interval", cfg.PollInterval.String()),
	)

	runner := &schedule.Runner{
		Pool:         pool.Pool(),
		Tasks:        tasks(pool.Pool(), logger),
		PollInterval: cfg.PollInterval,
		Logger:       logger,
	}
	if err := runner.Run(ctx); err != nil {
		return err
	}
	logger.Info("scheduler 종료 완료",
		slog.String("action", "shutdown"),
		slog.String("result", "success"),
	)
	return nil
}
