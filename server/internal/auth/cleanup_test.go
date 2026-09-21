package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/postgres/pgtest"
)

// 정리는 worker 역할로 돈다. 시각은 먼 과거로 잡아 다른 테스트의 세션을 건드리지 않는다.
func TestPruneSessionsKeepsFamiliesWithALiveSession(t *testing.T) {
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
	past := cutoff.Add(-24 * time.Hour)
	future := cutoff.Add(24 * time.Hour)
	insert := func(family uuid.UUID, expires time.Time, used, revoked bool) uuid.UUID {
		id := uuid.New()
		var usedAt, revokedAt *time.Time
		if used {
			usedAt = &past
		}
		if revoked {
			revokedAt = &past
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO sessions (id, user_id, device_id, token_family_id, refresh_token_hash, expires_at, used_at, revoked_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, user, device, family, []byte(id.String()), expires, usedAt, revokedAt); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// 살아 있는 family: 쓴 옛 세션이 만료됐어도 재사용 탐지를 위해 남는다.
	liveFamily := uuid.New()
	usedOld := insert(liveFamily, past, true, false)
	current := insert(liveFamily, future, false, false)
	// 끝난 family: 모두 만료됐거나 폐기됐다.
	deadFamily := uuid.New()
	deadUsed := insert(deadFamily, past, true, false)
	deadRevoked := insert(deadFamily, future, false, true)
	// 폐기됐지만 아직 만료 전인 세션은 만료 전이므로 남는다.

	n, err := PruneSessions(ctx, pool, cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("지운 수 = %d", n)
	}
	exists := func(id uuid.UUID) bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1)`, id).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for _, c := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"살아 있는 family의 만료된 쓴 세션", usedOld, true},
		{"살아 있는 세션", current, true},
		{"끝난 family의 만료된 세션", deadUsed, false},
		{"폐기됐지만 만료 전 세션", deadRevoked, true},
	} {
		if got := exists(c.id); got != c.want {
			t.Errorf("%s: 남았는가 = %v, want %v", c.name, got, c.want)
		}
	}
}
