// Command scheduler는 주기 작업 프로세스다. account_backend_design.md §9의
// 역할 구분에서 만료 초대, 알림 예약, 장기 실행 작업 복구, 삭제 작업 시작을
// 담당한다. §12에 따라 단일 leader로 돌되 작업 중복은 DB lease로 막는다.
//
// P0에서는 설정 로드, DB 연결, graceful shutdown, no-op 루프까지만 만든다.
// 실제 예약 작업 생성과 leader lease는 P7이다.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"plantogether/server/internal/platform/config"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/postgres"
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

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// no-op 루프는 진행 중인 작업이 없으므로 즉시 끝난다. P7에서 실제
			// 예약 작업이 생기면 여기서 leader lease 반납을 한다.
			logger.Info("scheduler 종료 완료",
				slog.String("action", "shutdown"),
				slog.String("result", "success"),
			)
			return nil
		case <-ticker.C:
			// P0은 작업을 예약하지 않는다. DB 연결이 살아 있는지만 확인해
			// 설정과 자격이 실제로 동작하는지 기동 직후에 드러나게 한다.
			pingCtx, cancel := context.WithTimeout(ctx, postgres.DefaultPingTimeout)
			err := pool.Ping(pingCtx)
			cancel()
			if err != nil {
				logger.Warn("DB 연결 확인 실패",
					slog.String("action", "db_probe"),
					slog.String("result", "failure"),
					slog.String("error", err.Error()),
				)
				continue
			}
			logger.Debug("예약 작업 없음",
				slog.String("action", "poll"),
				slog.String("result", "success"),
			)
		}
	}
}
