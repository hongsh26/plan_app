package test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// account_backend_design.md §11: "DB 역할은 migration, api, worker, read-only
// 운영 계정으로 분리한다."
//
// 역할 분리는 docker/postgres-init/01-roles.sql의 GRANT와 ALTER DEFAULT
// PRIVILEGES가 만든다. 다른 통합 테스트는 모두 DDL 권한이 있는 migration
// 자격으로 접속하므로, 이 파일이 없으면 역할 권한이 회귀해도 CI가 초록이다.
//
// 거부 쪽만 보지 않고 허용 쪽도 본다. ALTER DEFAULT PRIVILEGES의 FOR ROLE이
// 빠지면 api 역할이 DDL을 못 하는 것은 그대로지만 테이블 SELECT도 못 하게 되어
// 런타임이 통째로 멈춘다. 거부만 보는 테스트는 그 회귀를 통과시킨다.

const pgInsufficientPrivilege = "42501"

// 역할별 DSN 환경 변수. connect()의 TEST_DATABASE_URL과 같은 skip/fail 규칙을 따른다.
const (
	envAPIRoleURL      = "TEST_API_DATABASE_URL"
	envWorkerRoleURL   = "TEST_WORKER_DATABASE_URL"
	envReadonlyRoleURL = "TEST_READONLY_DATABASE_URL"
)

// connectAs는 지정한 환경 변수의 자격으로 접속한다. 값이 없으면 skip하되
// REQUIRE_DB_TESTS=1이면 실패한다.
func connectAs(t *testing.T, envName string) *pgx.Conn {
	t.Helper()

	url := os.Getenv(envName)
	required := os.Getenv("REQUIRE_DB_TESTS") == "1"
	if url == "" {
		msg := envName + "가 설정되지 않아 DB 역할 분리 테스트를 실행할 수 없다"
		if required {
			t.Fatal(msg + " (REQUIRE_DB_TESTS=1이므로 skip하지 않는다)")
		}
		t.Skip(msg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		// 연결 오류 원문에는 사용자명과 host가 들어 있다. 테스트 로그라도 그대로
		// 남기지 않고 어떤 변수였는지만 말한다.
		msg := envName + "로 PostgreSQL에 연결할 수 없다"
		if required {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// assertInsufficientPrivilege는 권한 부족(42501)으로 거부됐는지 확인한다.
// 다른 이유(문법 오류, 없는 테이블)의 실패를 "막혔다"로 오해하지 않게 한다.
func assertInsufficientPrivilege(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s가 허용됐다. 역할 권한이 회귀했다", what)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: PostgreSQL 오류가 아니다: %v", what, err)
	}
	if pgErr.Code != pgInsufficientPrivilege {
		t.Fatalf("%s: SQLSTATE = %s, want %s (insufficient_privilege). "+
			"권한이 아니라 다른 이유로 실패했다: %v", what, pgErr.Code, pgInsufficientPrivilege, err)
	}
}

// 런타임 역할(api, worker)은 DDL을 할 수 없다. api 자격이 유출되거나 SQL
// injection이 생겨도 스키마를 바꾸거나 테이블을 지울 수 없어야 한다.
func TestRuntimeRolesCannotRunDDL(t *testing.T) {
	for _, envName := range []string{envAPIRoleURL, envWorkerRoleURL} {
		t.Run(envName, func(t *testing.T) {
			conn := connectAs(t, envName)
			ctx := context.Background()

			_, err := conn.Exec(ctx, `CREATE TABLE role_separation_probe (id int)`)
			assertInsufficientPrivilege(t, err, "CREATE TABLE")

			_, err = conn.Exec(ctx, `DROP TABLE users`)
			assertInsufficientPrivilege(t, err, "DROP TABLE users")

			_, err = conn.Exec(ctx, `ALTER TABLE users ADD COLUMN probe int`)
			assertInsufficientPrivilege(t, err, "ALTER TABLE users")

			_, err = conn.Exec(ctx, `TRUNCATE users`)
			assertInsufficientPrivilege(t, err, "TRUNCATE users")
		})
	}
}

// 런타임 역할은 마이그레이션이 만든 테이블에 읽기·쓰기·삭제를 할 수 있어야 한다.
// 01-roles.sql의 ALTER DEFAULT PRIVILEGES가 FOR ROLE plantogether_migration 없이
// 쓰이면 이 테스트가 실패한다.
func TestRuntimeRolesCanReadAndWriteDomainTables(t *testing.T) {
	for _, envName := range []string{envAPIRoleURL, envWorkerRoleURL} {
		t.Run(envName, func(t *testing.T) {
			conn := connectAs(t, envName)
			ctx := context.Background()
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("트랜잭션을 시작할 수 없다: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			id := uuid.New()
			if _, err := tx.Exec(ctx,
				`INSERT INTO users (id, display_name) VALUES ($1, '역할 테스트')`, id); err != nil {
				t.Fatalf("INSERT users가 거부됐다: %v", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE users SET display_name = '변경' WHERE id = $1`, id); err != nil {
				t.Fatalf("UPDATE users가 거부됐다: %v", err)
			}
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, id).Scan(&n); err != nil {
				t.Fatalf("SELECT users가 거부됐다: %v", err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
				t.Fatalf("DELETE users가 거부됐다: %v", err)
			}
		})
	}
}

// 운영 조회 전용 역할은 읽기만 한다. 사람이 운영 중에 쓰는 자격이므로 실수로
// 쓰기가 나가지 않아야 한다.
func TestReadonlyRoleCannotWrite(t *testing.T) {
	conn := connectAs(t, envReadonlyRoleURL)
	ctx := context.Background()

	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("readonly 역할의 SELECT가 거부됐다: %v", err)
	}

	_, err := conn.Exec(ctx, `DELETE FROM users WHERE id = $1`, uuid.New())
	assertInsufficientPrivilege(t, err, "readonly DELETE")

	_, err = conn.Exec(ctx, `INSERT INTO users (id, display_name) VALUES ($1, 'x')`, uuid.New())
	assertInsufficientPrivilege(t, err, "readonly INSERT")

	_, err = conn.Exec(ctx, `UPDATE users SET display_name = 'x' WHERE id = $1`, uuid.New())
	assertInsufficientPrivilege(t, err, "readonly UPDATE")

	_, err = conn.Exec(ctx, `CREATE TABLE role_separation_probe (id int)`)
	assertInsufficientPrivilege(t, err, "readonly CREATE TABLE")
}
