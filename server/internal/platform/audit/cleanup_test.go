package audit

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/postgres/pgtest"
)

func TestPruneTombstonesOnlyDeletesExpiredDeletedActors(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.WorkerRoleURLEnv)
	ctx := context.Background()
	cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	oldID, recentID, anonymousID := uuid.New(), uuid.New(), uuid.New()
	for _, row := range []struct {
		id        uuid.UUID
		tombstone *string
		created   time.Time
	}{
		{oldID, ptr("deleted_user:old"), cutoff.Add(-time.Hour)},
		{recentID, ptr("deleted_user:recent"), cutoff.Add(time.Hour)},
		{anonymousID, nil, cutoff.Add(-time.Hour)},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO audit_events (id, actor_tombstone, action, result, created_at)
			VALUES ($1, $2, 'test', 'success', $3)`, row.id, row.tombstone, row.created); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_events WHERE id = ANY($1)`, []uuid.UUID{oldID, recentID, anonymousID})
	})

	n, err := PruneTombstones(ctx, pool, cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}
	for id, want := range map[uuid.UUID]bool{oldID: false, recentID: true, anonymousID: true} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events WHERE id = $1)`, id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != want {
			t.Errorf("id=%s exists=%v want=%v", id, exists, want)
		}
	}
}

func ptr(s string) *string { return &s }
