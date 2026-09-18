// Package config는 세 진입점(api, worker, scheduler)의 환경 변수 설정을 읽고
// 기동 시 검증한다. 필수 값이 없으면 기동을 거부한다.
//
// 이 패키지는 오류 메시지에 설정 "값"을 절대 넣지 않는다. DSN에 비밀번호가 들어
// 있으므로 값을 에코하는 순간 account_backend_design.md §11의 로그 금지 규칙을
// 기동 경로에서 위반한다. 오류는 변수 "이름"만 말한다.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Lookup은 os.LookupEnv와 같은 형태다. 테스트가 실제 환경 변수를 건드리지 않고
// 설정 검증을 확인할 수 있도록 주입 가능하게 둔다.
type Lookup func(key string) (string, bool)

// FromEnv는 실제 프로세스 환경을 읽는 Lookup이다.
func FromEnv() Lookup { return os.LookupEnv }

// FromMap은 맵을 읽는 Lookup이다. 테스트용이다.
func FromMap(m map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// Role은 실행 역할이다. account_backend_design.md §9가 정의한 셋이다.
type Role string

const (
	RoleAPI       Role = "api"
	RoleWorker    Role = "worker"
	RoleScheduler Role = "scheduler"
)

// Config는 모든 역할이 공유하는 설정과 역할별 설정을 담는다.
type Config struct {
	Role Role

	// Env는 배포 환경 이름이다. 스테이징과 프로덕션은 §12에 따라 자격을 분리한다.
	Env string

	// DatabaseURL은 런타임 DB 연결 문자열이다. §11의 역할 분리에 따라 api와
	// worker는 서로 다른 DB 역할의 자격을 받는다. 마이그레이션 자격과도 다르다.
	DatabaseURL string

	// DatabaseMaxConns는 pgx pool의 최대 연결 수다.
	DatabaseMaxConns int32

	// HTTPAddr은 api가 listen하는 주소다. worker와 scheduler는 사용하지 않는다.
	HTTPAddr string

	// ShutdownTimeout은 SIGINT/SIGTERM 수신 후 진행 중 작업을 기다리는 한도다.
	ShutdownTimeout time.Duration

	// PollInterval은 worker와 scheduler의 루프 주기다. P0에서는 no-op 루프가 쓴다.
	PollInterval time.Duration

	// LogLevel은 구조화 로그 수준이다.
	LogLevel slog.Level
}

const (
	envEnv              = "APP_ENV"
	envDatabaseURL      = "DATABASE_URL"
	envDatabaseMaxConns = "DATABASE_MAX_CONNS"
	envHTTPAddr         = "HTTP_ADDR"
	envShutdownTimeout  = "SHUTDOWN_TIMEOUT"
	envPollInterval     = "POLL_INTERVAL"
	envLogLevel         = "LOG_LEVEL"

	// MigrationDatabaseURLEnv는 마이그레이션 전용 DB 자격이다. §11이 요구하는
	// 역할 분리를 지키려면 마이그레이션은 DDL 권한이 있는 별도 역할로 실행해야
	// 하고, 런타임 api/worker 자격으로 실행해서는 안 된다.
	MigrationDatabaseURLEnv = "MIGRATION_DATABASE_URL"
)

// Load는 역할에 맞는 설정을 읽고 검증한다. 문제가 하나라도 있으면 Config를
// 반환하지 않는다. 누락·형식 오류는 모두 모아 한 번에 보고한다. 운영자가 한
// 변수를 고치고 다시 기동했다가 다음 변수에서 또 실패하는 왕복을 없앤다.
func Load(role Role, lookup Lookup) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("config: lookup이 nil이다")
	}

	v := &validator{lookup: lookup}

	cfg := Config{
		Role:             role,
		Env:              v.requiredOneOf(envEnv, "local", "staging", "production"),
		DatabaseURL:      v.requiredNonEmpty(envDatabaseURL),
		DatabaseMaxConns: int32(v.optionalInt(envDatabaseMaxConns, 10, 1, 1000)),
		ShutdownTimeout:  v.optionalDuration(envShutdownTimeout, 15*time.Second),
		LogLevel:         v.optionalLogLevel(envLogLevel, slog.LevelInfo),
	}

	switch role {
	case RoleAPI:
		cfg.HTTPAddr = v.optionalNonEmpty(envHTTPAddr, ":8080")
	case RoleWorker, RoleScheduler:
		cfg.PollInterval = v.optionalDuration(envPollInterval, 5*time.Second)
	default:
		v.addf("알 수 없는 실행 역할 %q", role)
	}

	if err := v.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadMigrationDatabaseURL은 마이그레이션 전용 자격을 읽는다. 런타임 설정과
// 분리해 두어 api 자격으로 DDL을 실행하는 경로를 만들지 않는다.
func LoadMigrationDatabaseURL(lookup Lookup) (string, error) {
	v := &validator{lookup: lookup}
	url := v.requiredNonEmpty(MigrationDatabaseURLEnv)
	if err := v.err(); err != nil {
		return "", err
	}
	return url, nil
}

// validator는 누락과 형식 오류를 모아 둔다.
type validator struct {
	lookup   Lookup
	problems []string
}

func (v *validator) addf(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	sort.Strings(v.problems)
	return fmt.Errorf("설정이 올바르지 않아 기동할 수 없다:\n  - %s",
		strings.Join(v.problems, "\n  - "))
}

func (v *validator) requiredNonEmpty(key string) string {
	raw, ok := v.lookup(key)
	if !ok {
		v.addf("%s가 설정되지 않았다", key)
		return ""
	}
	if strings.TrimSpace(raw) == "" {
		v.addf("%s가 비어 있다", key)
		return ""
	}
	return raw
}

func (v *validator) optionalNonEmpty(key, fallback string) string {
	raw, ok := v.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	return raw
}

func (v *validator) requiredOneOf(key string, allowed ...string) string {
	raw := v.requiredNonEmpty(key)
	if raw == "" {
		return ""
	}
	for _, a := range allowed {
		if raw == a {
			return raw
		}
	}
	// 허용 값 목록은 비밀이 아니므로 보여주되, 받은 값은 에코하지 않는다.
	v.addf("%s가 허용 값 중 하나가 아니다 (허용: %s)", key, strings.Join(allowed, ", "))
	return ""
}

func (v *validator) optionalInt(key string, fallback, min, max int) int {
	raw, ok := v.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		v.addf("%s가 정수가 아니다", key)
		return fallback
	}
	if n < min || n > max {
		v.addf("%s가 허용 범위 %d~%d를 벗어났다", key, min, max)
		return fallback
	}
	return n
}

func (v *validator) optionalDuration(key string, fallback time.Duration) time.Duration {
	raw, ok := v.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		v.addf("%s가 기간 형식이 아니다 (예: 15s, 1m)", key)
		return fallback
	}
	if d <= 0 {
		v.addf("%s는 0보다 커야 한다", key)
		return fallback
	}
	return d
}

func (v *validator) optionalLogLevel(key string, fallback slog.Level) slog.Level {
	raw, ok := v.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		v.addf("%s가 알 수 없는 로그 수준이다 (허용: debug, info, warn, error)", key)
		return fallback
	}
}
