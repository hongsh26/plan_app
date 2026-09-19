package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	// pgx의 database/sql 드라이버. goose는 *sql.DB를 요구하므로 pgxpool이
	// 아니라 이 bridge로 연다. 런타임 쿼리는 계속 pgxpool을 쓴다.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"plantogether/server/internal/platform/config"
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

// ErrDestructiveMigrationNotLocal은 파괴적 마이그레이션이 로컬 밖에서
// 요청됐을 때 반환된다.
var ErrDestructiveMigrationNotLocal = errors.New(
	"파괴적 마이그레이션은 APP_ENV=local에서만 허용된다")

// isDestructive는 방향이 데이터를 잃는지 판정한다.
//
// reset뿐 아니라 down도 포함한다. 현재 마이그레이션은 00001 하나뿐이라
// down 한 번이 reset과 똑같이 8개 테이블을 전부 DROP한다. reset만 막으면
// 가드가 이름만 남고 실제로는 뚫려 있다. 마이그레이션이 늘어나 down의 폭이
// 줄어든 뒤에도, 되돌리기는 운영 DB에서 사람이 판단할 일이지 프로세스 인자
// 하나로 실행될 일이 아니므로 목록에 남긴다.
func isDestructive(direction MigrateDirection) bool {
	return direction == MigrateReset || direction == MigrateDownOne
}

// Migrate는 embed된 SQL 마이그레이션을 실행한다.
//
// databaseURL은 MIGRATION_DATABASE_URL이어야 한다. §11이 DB 역할을 migration,
// api, worker, read-only로 분리하라고 요구하므로 런타임 api/worker 자격으로
// DDL을 실행하지 않는다. 호출자가 이 규칙을 지킬 책임이 있다.
//
// goose를 라이브러리로 쓰고 embed.FS를 넘긴다. 별도 goose 바이너리 설치를
// 요구하지 않는다.
//
// env는 APP_ENV다. 파괴적 방향(down, reset)은 config.EnvLocal에서만 허용한다.
// 이 판정을 호출자가 아니라 여기에 두는 이유는 두 가지다. 프로덕션 api task에
// MIGRATION_DATABASE_URL이 주입된 상태에서 -migrate reset이 한 번 실행되면
// 스키마 전체가 사라지는데, 호출자 쪽 CLI 인자 검사는 다음에 추가될 진입점이
// 그대로 빠뜨릴 수 있다. DROP을 실제로 수행하는 함수가 스스로 거부해야
// 이 모듈의 어느 진입점을 통해서도 우회되지 않는다.
//
// 이 가드의 범위는 거기까지다. 두 가지는 막지 못한다.
//   - MIGRATION_DATABASE_URL을 쥔 사람이 psql이나 goose 바이너리를 직접 쓰는 것.
//     DB 수준 방어(배포 시에만 migration 역할에 DDL 부여)는 별개 과제다.
//   - 미래의 호출자가 APP_ENV를 읽지 않고 config.EnvLocal을 하드코딩하는 것.
//     env를 named type으로 바꿔도 막히지 않는다. Go는 untyped 문자열 상수를
//     named string type에 암묵 변환하므로 Migrate(..., "local")이 그대로
//     컴파일된다. 이 경로는 타입이 아니라 리뷰가 막아야 한다.
func Migrate(ctx context.Context, databaseURL string, direction MigrateDirection, env string) error {
	// 연결을 열기 전에 판정한다. 거부는 DB 왕복 없이 성립해야 하고, 그래야
	// 실제 PostgreSQL 없이도 가드를 테스트할 수 있다.
	if isDestructive(direction) && env != config.EnvLocal {
		// 환경 이름은 비밀이 아니고 진단에 필요하므로 그대로 싣는다.
		// DSN은 싣지 않는다 (§11).
		return fmt.Errorf("%w: 방향 %s, APP_ENV %s", ErrDestructiveMigrationNotLocal, direction, env)
	}

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
