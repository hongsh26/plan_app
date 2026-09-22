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

// APP_ENV의 허용 값이다. 문자열 리터럴을 여러 패키지에 흩어 두면 파괴적
// 마이그레이션 가드가 오타 하나로 조용히 무력화되므로 여기서만 정의한다.
const (
	EnvLocal      = "local"
	EnvStaging    = "staging"
	EnvProduction = "production"
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

	// PollInterval은 worker와 scheduler의 루프 주기다. worker는 점유할 job이 없을 때만
	// 이만큼 기다린다.
	PollInterval time.Duration

	// WorkerLease는 worker가 job 하나를 점유하는 기간이자 handler 제한 시간이다.
	// worker가 죽으면 이 시간이 지난 뒤 다른 worker가 job을 다시 점유한다(§9).
	WorkerLease time.Duration

	// WorkerBatchSize는 worker가 한 번에 점유하는 최대 job 수이자 동시 처리 수다.
	WorkerBatchSize int

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
	envWorkerLease      = "WORKER_LEASE_DURATION"
	envWorkerBatchSize  = "WORKER_BATCH_SIZE"

	// MigrationDatabaseURLEnv는 마이그레이션 전용 DB 자격이다. §11이 요구하는
	// 역할 분리를 지키려면 마이그레이션은 DDL 권한이 있는 별도 역할로 실행해야
	// 하고, 런타임 api/worker 자격으로 실행해서는 안 된다.
	MigrationDatabaseURLEnv = "MIGRATION_DATABASE_URL"
)

// MinWorkerLease는 WORKER_LEASE_DURATION의 최솟값이다. jobs.MinLease와 같아야 한다.
const MinWorkerLease = 17 * time.Second

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
		Env:              v.requiredOneOf(envEnv, EnvLocal, EnvStaging, EnvProduction),
		DatabaseURL:      v.requiredNonEmpty(envDatabaseURL),
		DatabaseMaxConns: int32(v.optionalInt(envDatabaseMaxConns, 10, 1, 1000)),
		ShutdownTimeout:  v.optionalDuration(envShutdownTimeout, 15*time.Second),
		LogLevel:         v.optionalLogLevel(envLogLevel, slog.LevelInfo),
	}

	switch role {
	case RoleAPI:
		cfg.HTTPAddr = v.optionalNonEmpty(envHTTPAddr, ":8080")
	case RoleWorker:
		cfg.PollInterval = v.optionalDuration(envPollInterval, 5*time.Second)
		cfg.WorkerLease = v.optionalDuration(envWorkerLease, time.Minute)
		// jobs.MinLease와 같다. config가 jobs에 의존하지 않도록 값을 적는다(jobs 테스트가 대조한다).
		if cfg.WorkerLease > 0 && cfg.WorkerLease < MinWorkerLease {
			v.addf("%s는 %s 이상이어야 한다(handler 기한과 결과 기록이 lease 안에 끝나야 한다)", envWorkerLease, MinWorkerLease)
		}
		cfg.WorkerBatchSize = v.optionalInt(envWorkerBatchSize, 10, 1, 100)
	case RoleScheduler:
		cfg.PollInterval = v.optionalDuration(envPollInterval, 5*time.Second)
	default:
		v.addf("알 수 없는 실행 역할 %q", role)
	}

	if err := v.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// MigrationSettings는 마이그레이션 실행에 필요한 설정이다. 런타임 Config와
// 분리해 두어 api 자격으로 DDL을 실행하는 경로를 만들지 않는다.
type MigrationSettings struct {
	// Env는 APP_ENV다. 파괴적 마이그레이션(down, reset)을 배포 환경에서
	// 거부하는 판단 근거이므로 마이그레이션 경로에서도 필수다.
	Env string

	// DatabaseURL은 MIGRATION_DATABASE_URL이다. DDL 권한이 있는 유일한 역할의
	// 자격이다 (§11).
	DatabaseURL string
}

// LoadMigrationSettings는 마이그레이션 전용 설정을 읽는다.
//
// APP_ENV를 여기서도 필수로 요구한다. 환경을 모르면 파괴적 연산을 거부할지
// 판단할 수 없고, "모르면 허용"은 프로덕션에서 스키마를 통째로 날리는 쪽으로
// 실패한다.
func LoadMigrationSettings(lookup Lookup) (MigrationSettings, error) {
	if lookup == nil {
		return MigrationSettings{}, errors.New("config: lookup이 nil이다")
	}

	v := &validator{lookup: lookup}
	s := MigrationSettings{
		Env:         v.requiredOneOf(envEnv, EnvLocal, EnvStaging, EnvProduction),
		DatabaseURL: v.requiredNonEmpty(MigrationDatabaseURLEnv),
	}
	if err := v.err(); err != nil {
		return MigrationSettings{}, err
	}
	return s, nil
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
