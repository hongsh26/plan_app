package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func validEnv() map[string]string {
	return map[string]string{
		"APP_ENV":      "local",
		"DATABASE_URL": "postgres://api:secret-password@localhost:5432/plantogether",
	}
}

func TestLoadAcceptsMinimalValidEnv(t *testing.T) {
	cfg, err := Load(RoleAPI, FromMap(validEnv()))
	if err != nil {
		t.Fatalf("유효한 설정이 거부됐다: %v", err)
	}
	if cfg.Env != "local" {
		t.Errorf("Env = %q, want %q", cfg.Env, "local")
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr 기본값 = %q, want %q", cfg.HTTPAddr, ":8080")
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout 기본값 = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel 기본값 = %v, want info", cfg.LogLevel)
	}
	if cfg.DatabaseMaxConns != 10 {
		t.Errorf("DatabaseMaxConns 기본값 = %d, want 10", cfg.DatabaseMaxConns)
	}
}

// 필수 값이 없으면 기동을 거부해야 한다. 기본값으로 조용히 채우면 운영자가
// 잘못된 DB를 향해 기동한 것을 알아채지 못한다.
func TestLoadRejectsMissingRequired(t *testing.T) {
	tests := []struct {
		name    string
		remove  string
		wantMsg string
	}{
		{"APP_ENV 누락", "APP_ENV", "APP_ENV"},
		{"DATABASE_URL 누락", "DATABASE_URL", "DATABASE_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			delete(env, tt.remove)

			_, err := Load(RoleAPI, FromMap(env))
			if err == nil {
				t.Fatalf("%s인데도 설정이 통과했다", tt.remove)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("오류가 %q를 지목하지 않는다: %v", tt.wantMsg, err)
			}
		})
	}
}

func TestLoadRejectsEmptyRequired(t *testing.T) {
	env := validEnv()
	env["DATABASE_URL"] = "   "

	_, err := Load(RoleAPI, FromMap(env))
	if err == nil {
		t.Fatal("공백뿐인 DATABASE_URL이 통과했다")
	}
}

// 누락이 여럿이면 한 번에 모두 보고해야 한다. 하나씩 고치며 재기동하는
// 왕복을 없앤다.
func TestLoadAggregatesAllProblems(t *testing.T) {
	_, err := Load(RoleAPI, FromMap(map[string]string{
		"DATABASE_MAX_CONNS": "정수아님",
		"SHUTDOWN_TIMEOUT":   "15초",
	}))
	if err == nil {
		t.Fatal("설정이 모두 잘못됐는데 통과했다")
	}
	msg := err.Error()
	for _, want := range []string{"APP_ENV", "DATABASE_URL", "DATABASE_MAX_CONNS", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("오류가 %s를 보고하지 않는다:\n%s", want, msg)
		}
	}
}

// §11: 로그에 자격을 남기지 않는다. 기동 실패 메시지는 로그로 흘러가므로
// 설정 오류가 값을 에코하면 그 자체가 위반이다.
func TestLoadErrorNeverEchoesValues(t *testing.T) {
	const password = "super-secret-password"
	env := map[string]string{
		"APP_ENV":            "값이틀림",
		"DATABASE_URL":       "postgres://api:" + password + "@localhost:5432/db",
		"DATABASE_MAX_CONNS": "99999",
		"LOG_LEVEL":          "이상한수준",
	}

	_, err := Load(RoleAPI, FromMap(env))
	if err == nil {
		t.Fatal("잘못된 설정이 통과했다")
	}
	msg := err.Error()
	if strings.Contains(msg, password) {
		t.Errorf("오류 메시지에 비밀번호가 들어 있다:\n%s", msg)
	}
	for _, leaked := range []string{"값이틀림", "이상한수준", "99999"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("오류 메시지가 설정 값 %q를 에코했다:\n%s", leaked, msg)
		}
	}
}

func TestLoadRejectsUnknownEnvName(t *testing.T) {
	env := validEnv()
	env["APP_ENV"] = "dev"

	_, err := Load(RoleAPI, FromMap(env))
	if err == nil {
		t.Fatal("허용되지 않은 APP_ENV가 통과했다")
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("오류가 허용 값 목록을 알려주지 않는다: %v", err)
	}
}

// worker와 scheduler는 HTTP를 listen하지 않고 대신 poll 주기를 갖는다.
func TestLoadRoleSpecificFields(t *testing.T) {
	for _, role := range []Role{RoleWorker, RoleScheduler} {
		t.Run(string(role), func(t *testing.T) {
			cfg, err := Load(role, FromMap(validEnv()))
			if err != nil {
				t.Fatalf("%s 설정이 거부됐다: %v", role, err)
			}
			if cfg.PollInterval != 5*time.Second {
				t.Errorf("PollInterval 기본값 = %v, want 5s", cfg.PollInterval)
			}
			if cfg.HTTPAddr != "" {
				t.Errorf("%s가 HTTPAddr을 가졌다: %q", role, cfg.HTTPAddr)
			}
		})
	}
}

func TestLoadRejectsUnknownRole(t *testing.T) {
	_, err := Load(Role("gateway"), FromMap(validEnv()))
	if err == nil {
		t.Fatal("알 수 없는 역할이 통과했다")
	}
}

func TestLoadRejectsOutOfRangeMaxConns(t *testing.T) {
	env := validEnv()
	env["DATABASE_MAX_CONNS"] = "0"

	if _, err := Load(RoleAPI, FromMap(env)); err == nil {
		t.Fatal("DATABASE_MAX_CONNS=0이 통과했다")
	}
}

func TestLoadParsesOptionalOverrides(t *testing.T) {
	env := validEnv()
	env["HTTP_ADDR"] = "127.0.0.1:9090"
	env["SHUTDOWN_TIMEOUT"] = "30s"
	env["DATABASE_MAX_CONNS"] = "25"
	env["LOG_LEVEL"] = "debug"

	cfg, err := Load(RoleAPI, FromMap(env))
	if err != nil {
		t.Fatalf("설정이 거부됐다: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.DatabaseMaxConns != 25 {
		t.Errorf("DatabaseMaxConns = %d", cfg.DatabaseMaxConns)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
}

// 마이그레이션 자격은 런타임 자격과 분리돼 있어야 한다 (§11의 DB 역할 분리).
func TestLoadMigrationDatabaseURL(t *testing.T) {
	if _, err := LoadMigrationDatabaseURL(FromMap(validEnv())); err == nil {
		t.Fatal("MIGRATION_DATABASE_URL 없이 통과했다. DATABASE_URL로 대체되면 안 된다")
	}

	got, err := LoadMigrationDatabaseURL(FromMap(map[string]string{
		MigrationDatabaseURLEnv: "postgres://migration@localhost:5432/db",
	}))
	if err != nil {
		t.Fatalf("유효한 마이그레이션 자격이 거부됐다: %v", err)
	}
	if got != "postgres://migration@localhost:5432/db" {
		t.Errorf("반환값 = %q", got)
	}
}

func TestLoadRejectsNilLookup(t *testing.T) {
	if _, err := Load(RoleAPI, nil); err == nil {
		t.Fatal("nil lookup이 통과했다")
	}
}
