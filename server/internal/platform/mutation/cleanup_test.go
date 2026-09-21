package mutation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/postgres/pgtest"
)

// 정리는 worker 역할로 돈다. 시각은 먼 과거로 잡아 다른 테스트의 기록을 건드리지 않는다.
func TestPruneIdempotencyKeysRemovesOnlyExpired(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.WorkerRoleURLEnv)
	ctx := context.Background()
	user, device := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, display_name) VALUES ($1, '정리 테스트')`, user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })
	if _, err := pool.Exec(ctx, `INSERT INTO devices (id, user_id, platform) VALUES ($1, $2, 'ios')`, device, user); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	insert := func(key string, expires time.Time) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO idempotency_keys (id, user_id, device_id, key, request_hash, expires_at)
			VALUES ($1, $2, $3, $4, '\x00', $5)`, uuid.New(), user, device, key, expires); err != nil {
			t.Fatal(err)
		}
	}
	insert("old-1", cutoff.Add(-time.Hour))
	insert("old-2", cutoff.Add(-time.Minute))
	insert("live", cutoff.Add(time.Hour))

	n, err := PruneIdempotencyKeys(ctx, pool, cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("지운 수 = %d, want >= 2", n)
	}
	var left []string
	rows, err := pool.Query(ctx, `SELECT key FROM idempotency_keys WHERE user_id = $1`, user)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		left = append(left, k)
	}
	if len(left) != 1 || left[0] != "live" {
		t.Fatalf("남은 키 = %v, want [live]", left)
	}
}
