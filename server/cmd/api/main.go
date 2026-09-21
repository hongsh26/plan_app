// Command api는 HTTP API 프로세스다. account_backend_design.md §9의 역할
// 구분에서 인증, 권한, 상태 전이, sync를 담당한다.
//
// P1부터 인증(§5)과 계정 조회 endpoint를 제공한다. 인증 설정 규칙은
// config.LoadAuth에 있다.
//
// -migrate 플래그는 MIGRATION_DATABASE_URL 자격으로 마이그레이션만 실행하고
// 종료한다. 런타임 DATABASE_URL(api 역할)은 DDL 권한이 없어야 하므로 두 자격을
// 섞지 않는다 (§11). 파괴적 방향(down, reset)은 postgres.Migrate가 APP_ENV를
// 보고 로컬 밖에서 거부한다.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"plantogether/server/internal/account"
	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/config"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/postgres"
	"plantogether/server/internal/platform/secretbox"
)

func main() {
	migrateDirection := flag.String("migrate", "",
		"마이그레이션만 실행하고 종료한다 (up, down, reset, status). "+
			"MIGRATION_DATABASE_URL과 APP_ENV가 필요하다. "+
			"파괴적 방향(down, reset)은 APP_ENV=local에서만 허용된다.")
	flag.Parse()

	if *migrateDirection != "" {
		if err := runMigrate(*migrateDirection); err != nil {
			fmt.Fprintf(os.Stderr, "마이그레이션 실패: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// 설정 오류는 로거를 만들기 전에도 발생할 수 있으므로 stderr로 낸다.
		fmt.Fprintf(os.Stderr, "api 종료: %v\n", err)
		os.Exit(1)
	}
}

func runMigrate(direction string) error {
	settings, err := config.LoadMigrationSettings(config.FromEnv())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return postgres.Migrate(ctx, settings.DatabaseURL, postgres.MigrateDirection(direction), settings.Env)
}

func run() error {
	cfg, err := config.Load(config.RoleAPI, config.FromEnv())
	if err != nil {
		return err
	}

	authSettings, err := config.LoadAuth(cfg.Env, config.FromEnv())
	if err != nil {
		return err
	}

	logger := httpapi.NewLogger(cfg.LogLevel, string(cfg.Role))

	// SIGINT/SIGTERM에 취소되는 context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// pgxpool.New는 lazy connect다. DB가 아직 없어도 기동에 실패하지 않으므로
	// liveness는 200을 유지하고 readiness만 503이 된다.
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	// secretbox.NewLocal은 APP_ENV=local이 아니면 실패한다. config.LoadAuth가
	// 이미 거부했으므로 여기 오는 것은 local뿐이다.
	box, err := secretbox.NewLocal(cfg.Env, authSettings.TokenEncryptionKey)
	if err != nil {
		return err
	}
	authHandler, err := newAuthHandler(authSettings, pool, box, logger)
	if err != nil {
		return err
	}
	mux := httpapi.NewMux(pool, logger)
	authHandler.Register(mux)
	account.NewHandler(account.Deps{Pool: pool.Pool(), Logger: logger, Sealer: box}).Register(mux, authHandler.Require)

	server := httpapi.NewServer(cfg.HTTPAddr, httpapi.Wrap(logger, mux))

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("api 기동",
			slog.String("action", "startup"),
			slog.String("addr", cfg.HTTPAddr),
			slog.String("env", cfg.Env),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("HTTP 서버가 중단됐다: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("종료 신호 수신, graceful shutdown 시작",
			slog.String("action", "shutdown"),
			slog.String("timeout", cfg.ShutdownTimeout.String()),
		)
	}

	// 진행 중인 요청이 끝날 때까지 기다린다. 신호 context는 이미 취소됐으므로
	// shutdown에는 별도 context를 쓴다.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown이 한도 안에 끝나지 않았다: %w", err)
	}

	logger.Info("api 종료 완료",
		slog.String("action", "shutdown"),
		slog.String("result", "success"),
	)
	return <-serveErr
}

// newAuthHandler는 인증 서비스를 조립한다.
//
// Apple code 교환 자격이 없으면 교환을 건너뛴다. config.LoadAuth가 이 조합을
// APP_ENV=local에서만 허용하므로 여기서 다시 확인하지 않는다. 대신 기동 로그에
// 남겨 개발자가 모르고 지나가지 않게 한다.
func newAuthHandler(s config.AuthSettings, pool *postgres.Pool, box *secretbox.LocalAESGCM, logger *slog.Logger) (*auth.Handler, error) {
	tokens, err := auth.NewAccessTokens(s.AccessTokenSigningKey, nil)
	if err != nil {
		return nil, err
	}
	verifier, err := appleid.NewVerifier(s.AppleClientID, "", nil, nil)
	if err != nil {
		return nil, err
	}
	deps := auth.Deps{Pool: pool.Pool(), Apple: verifier, Tokens: tokens}

	if s.AppleCodeExchange != nil {
		key, err := appleid.ParsePrivateKey(s.AppleCodeExchange.PrivateKeyPEM)
		if err != nil {
			return nil, err
		}
		client, err := appleid.NewClient(appleid.Credentials{
			TeamID:     s.AppleCodeExchange.TeamID,
			KeyID:      s.AppleCodeExchange.KeyID,
			ClientID:   s.AppleClientID,
			PrivateKey: key,
		}, "", "", nil, nil)
		if err != nil {
			return nil, err
		}
		deps.CodeExchanger = client
		deps.Sealer = box
	} else {
		logger.Warn("Apple code 교환이 꺼져 있다. Apple refresh token을 저장하지 않으므로 계정 삭제 시 revoke할 수 없다 (로컬 전용)",
			slog.String("action", "startup"),
		)
	}

	svc, err := auth.NewService(deps)
	if err != nil {
		return nil, err
	}
	return auth.NewHandler(svc, logger), nil
}
