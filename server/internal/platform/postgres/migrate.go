package postgres

import (
	"context"
	"database/sql"
	"fmt"

	// pgx의 database/sql 드라이버. goose는 *sql.DB를 요구하므로 pgxpool이
	// 아니라 이 bridge로 연다. 런타임 쿼리는 계속 pgxpool을 쓴다.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"plantogether/server/migrations"
)

// MigrateDirection은 마이그레이션 방향이다.
type MigrateDirection string

const (
	// MigrateUp은 미적용 마이그레이션을 모두 적용한다.
	MigrateUp MigrateDirection = "up"
	// MigrateDownOne은 가장 최근 마이그레이션 하나를 되돌린다.
	MigrateDownOne MigrateDirection = "down"
	// MigrateReset은 전부 되돌린다. 로컬과 테스트에서만 쓴다.
	MigrateReset MigrateDirection = "reset"
	// MigrateStatus는 적용 상태를 출력한다.
	MigrateStatus MigrateDirection = "status"
)

// Migrate는 embed된 SQL 마이그레이션을 실행한다.
//
// databaseURL은 MIGRATION_DATABASE_URL이어야 한다. §11이 DB 역할을 migration,
// api, worker, read-only로 분리하라고 요구하므로 런타임 api/worker 자격으로
// DDL을 실행하지 않는다. 호출자가 이 규칙을 지킬 책임이 있다.
//
// goose를 라이브러리로 쓰고 embed.FS를 넘긴다. 별도 goose 바이너리 설치를
// 요구하지 않는다.
func Migrate(ctx context.Context, databaseURL string, direction MigrateDirection) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		// DSN 원문이 오류에 실리지 않게 감싸지 않는다.
		return fmt.Errorf("마이그레이션 DB 연결을 열 수 없다 (방향 %s)", direction)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("마이그레이션 DB에 연결할 수 없다: %w", err)
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect를 설정할 수 없다: %w", err)
	}

	switch direction {
	case MigrateUp:
		return goose.UpContext(ctx, db, ".")
	case MigrateDownOne:
		return goose.DownContext(ctx, db, ".")
	case MigrateReset:
		return goose.DownToContext(ctx, db, ".", 0)
	case MigrateStatus:
		return goose.StatusContext(ctx, db, ".")
	default:
		return fmt.Errorf("알 수 없는 마이그레이션 방향 %q", direction)
	}
}
