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

// SyncPruneLockKey는 sync 정리 워터마크를 다루는 테스트 사이의 advisory lock 키다. 키는 여기 한 곳에만 둔다.
//
// 정리 워터마크(sync_prune_state)는 DB 전체에 하나다. 정리 테스트는 방금 만든 row를 40일 전으로 바꿔 지우므로
// 워터마크가 "지금"에 가까운 txid로 오른다. 그동안 다른 열린 트랜잭션이 horizon을 붙잡고 있으면 bootstrap
// watermark(= horizon)가 정리 워터마크 이하가 되어 cursor를 읽는 테스트가 410 sync_cursor_expired를 받는다.
// 운영에서는 워터마크가 30일 넘은 row로만 오르므로 이 상태가 생기지 않는다.
//
// 그래서 sync cursor를 읽는 테스트는 HoldSyncReader(공유)를, 정리 테스트는 HoldSyncPruner(배타)를 잡는다. 패키지가
// 달라도 같은 DB에서 겹치지 않는다. 한 테스트가 두 잠금을 함께 잡으면 교착하므로 정리 테스트는 공유 잠금을 잡는
// 헬퍼를 쓰지 않는다.
const SyncPruneLockKey int64 = 0x53594e4350524e45 // "SYNCPRNE"

// HoldSyncReader는 테스트가 끝날 때까지 정리 잠금을 공유로 잡는다. sync bootstrap·증분을 읽는 테스트 스택이 부른다.
func HoldSyncReader(t *testing.T, pool *pgxpool.Pool) { holdAdvisory(t, pool, false) }

// HoldSyncPruner는 테스트가 끝날 때까지 정리 잠금을 배타로 잡는다. sync 정리를 실행하는 테스트가 부른다.
func HoldSyncPruner(t *testing.T, pool *pgxpool.Pool) { holdAdvisory(t, pool, true) }

// holdAdvisory는 세션 잠금이라 전용 연결을 잡고, 끝나면 풀고 반납한다(잠금을 쥔 채 pool로 돌아가지 않게).
func holdAdvisory(t *testing.T, pool *pgxpool.Pool, exclusive bool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lock, unlock := "pg_advisory_lock_shared", "pg_advisory_unlock_shared"
	if exclusive {
		lock, unlock = "pg_advisory_lock", "pg_advisory_unlock"
	}
	if _, err := conn.Exec(ctx, `SELECT `+lock+`($1)`, SyncPruneLockKey); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT `+unlock+`($1)`, SyncPruneLockKey)
		conn.Release()
	})
}
