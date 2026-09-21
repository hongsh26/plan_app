// Package pgtest는 도메인 패키지의 PostgreSQL 통합 테스트가 쓰는 연결 헬퍼다.
//
// server/test의 connect()와 같은 규칙을 따른다. 환경 변수가 없으면 skip하고,
// REQUIRE_DB_TESTS=1이면 skip 대신 실패한다. CI는 REQUIRE_DB_TESTS=1로 돌므로
// skip이 통과로 위장되지 않는다.
package pgtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// APIRoleURLEnv는 api 런타임 역할의 DSN이다. 도메인 서비스 테스트는 이 역할로
// 접속한다. migration 역할로 접속하면 서비스가 DDL이나 TRUNCATE 같은 권한 밖의
// 동작에 기대도 테스트가 통과하고 운영에서야 실패한다.
const APIRoleURLEnv = "TEST_API_DATABASE_URL"

// WorkerRoleURLEnv는 worker 런타임 역할의 DSN이다. worker와 scheduler 코드의 테스트는
// 이 역할로 접속한다. api 역할로 접속하면 worker 역할에 빠진 권한이 운영에서야 드러난다.
const WorkerRoleURLEnv = "TEST_WORKER_DATABASE_URL"

// Pool은 envName의 DSN으로 pool을 연다. 테스트가 끝나면 닫는다.
func Pool(t *testing.T, envName string) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv(envName)
	required := os.Getenv("REQUIRE_DB_TESTS") == "1"
	if url == "" {
		msg := envName + "가 설정되지 않아 PostgreSQL 통합 테스트를 실행할 수 없다"
		if required {
			t.Fatal(msg + " (REQUIRE_DB_TESTS=1이므로 skip하지 않는다)")
		}
		t.Skip(msg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err == nil {
		if err = pool.Ping(ctx); err != nil {
			pool.Close()
		}
	}
	if err != nil {
		// 연결 오류 원문에는 사용자명과 host가 들어 있다.
		msg := envName + "로 PostgreSQL에 연결할 수 없다"
		if required {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	t.Cleanup(pool.Close)
	return pool
}
