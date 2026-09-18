// Command worker는 비동기 작업 처리 프로세스다. account_backend_design.md §9의
// 역할 구분에서 APNs 발송, 계정 삭제, 명령 만료·재할당과 서버 측 재시도 상태
// 관리를 담당한다. EventKit 작업은 수행하지 않는다.
//
// P0에서는 설정 로드, DB 연결, graceful shutdown, no-op 루프까지만 만든다.
// 실제 job claim(FOR UPDATE SKIP LOCKED)과 lease 처리는 P7이다.
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

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// no-op 루프는 진행 중인 작업이 없으므로 즉시 끝난다. P7에서 실제
			// job이 생기면 여기서 lease 반납과 진행 중 작업 대기를 한다.
			logger.Info("worker 종료 완료",
				slog.String("action", "shutdown"),
				slog.String("result", "success"),
			)
			return nil
		case <-ticker.C:
			// P0은 job을 claim하지 않는다. DB 연결이 살아 있는지만 확인해
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
			logger.Debug("job 처리 없음",
				slog.String("action", "poll"),
				slog.String("result", "success"),
			)
		}
	}
}
