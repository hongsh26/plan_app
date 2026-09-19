package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"plantogether/server/internal/platform/config"
)

// 연결 불가능한 DSN이다. 가드가 통과하면 sql.Open 이후 단계로 내려가 이
// 주소에서 연결이 실패하므로, 테스트가 "거부됐다"와 "거부되지 않았다"를
// 확실히 구분할 수 있다.
const unreachableDSN = "postgres://guard-test@127.0.0.1:1/guard-test?sslmode=disable&connect_timeout=1"

// 파괴적 마이그레이션은 로컬 밖에서 거부돼야 한다.
//
// 이 가드가 없으면 MIGRATION_DATABASE_URL이 주입된 프로덕션 api task에서
// -migrate reset 한 번으로 8개 테이블이 전부 DROP된다.
func TestMigrateRejectsDestructiveDirectionOutsideLocal(t *testing.T) {
	for _, direction := range []MigrateDirection{MigrateReset, MigrateDownOne} {
		for _, env := range []string{config.EnvStaging, config.EnvProduction} {
			t.Run(string(direction)+"_"+env, func(t *testing.T) {
				err := Migrate(context.Background(), unreachableDSN, direction, env)
				if err == nil {
					t.Fatalf("APP_ENV=%s에서 %s가 허용됐다", env, direction)
				}
				if !errors.Is(err, ErrDestructiveMigrationNotLocal) {
					t.Fatalf("다른 이유로 실패했다. 가드가 아니라 연결 실패일 수 있다: %v", err)
				}
			})
		}
	}
}

// APP_ENV가 비어 있거나 알 수 없는 값이어도 파괴적 방향은 거부한다.
// "모르면 허용"은 프로덕션 스키마를 날리는 쪽으로 실패한다.
func TestMigrateRejectsDestructiveDirectionOnUnknownEnv(t *testing.T) {
	for _, env := range []string{"", "prod", "LOCAL", "local "} {
		t.Run("env="+env, func(t *testing.T) {
			err := Migrate(context.Background(), unreachableDSN, MigrateReset, env)
			if !errors.Is(err, ErrDestructiveMigrationNotLocal) {
				t.Fatalf("APP_ENV=%q에서 reset이 가드에 걸리지 않았다: %v", env, err)
			}
		})
	}
}

// 가드는 DB 연결을 열기 전에 판정해야 한다. 연결 이후에 판정하면 DB가 닿는
// 순간에만 거부가 성립하고, 닿지 않으면 다른 오류에 가려진다.
func TestMigrateGuardRunsBeforeOpeningConnection(t *testing.T) {
	err := Migrate(context.Background(), unreachableDSN, MigrateReset, config.EnvProduction)
	if err == nil {
		t.Fatal("reset이 허용됐다")
	}
	// 연결 단계까지 내려갔다면 오류 문구가 연결 실패를 말한다.
	if strings.Contains(err.Error(), "연결") {
		t.Fatalf("가드보다 연결 시도가 먼저 일어났다: %v", err)
	}
}

// 비파괴적 방향은 가드에 걸리지 않는다. 가드가 너무 넓으면 프로덕션 배포의
// -migrate up이 막힌다.
func TestMigrateAllowsNonDestructiveDirectionsInProduction(t *testing.T) {
	for _, direction := range []MigrateDirection{MigrateUp, MigrateStatus} {
		t.Run(string(direction), func(t *testing.T) {
			err := Migrate(context.Background(), unreachableDSN, direction, config.EnvProduction)
			if errors.Is(err, ErrDestructiveMigrationNotLocal) {
				t.Fatalf("비파괴적 방향 %s가 가드에 걸렸다", direction)
			}
			// 연결이 불가능한 DSN이므로 여기까지 온 이상 실패하는 것이 정상이다.
			// 확인할 것은 "가드가 아닌 이유로 실패했다"는 사실뿐이다.
			if err == nil {
				t.Fatalf("연결 불가 DSN인데 %s가 성공했다", direction)
			}
		})
	}
}

// 로컬에서는 파괴적 방향이 가드를 통과한다.
func TestMigrateAllowsDestructiveDirectionInLocal(t *testing.T) {
	err := Migrate(context.Background(), unreachableDSN, MigrateReset, config.EnvLocal)
	if errors.Is(err, ErrDestructiveMigrationNotLocal) {
		t.Fatal("APP_ENV=local에서 reset이 거부됐다. 로컬 개발이 막힌다")
	}
}
