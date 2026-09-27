package jobs

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunnerReclaimsVacantSlotsBeforeSlowJobFinishes(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()

	for i, name := range []string{"slow", "fast-1", "fast-2", "fast-3"} {
		id := uuid.New()
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO outbox_jobs (id, type, payload, next_run_at)
			VALUES ($1, $2, $3, now() - make_interval(secs => $4))`,
			id, f.typ, `{"name":"`+name+`"}`, 10-i); err != nil {
			t.Fatal(err)
		}
	}

	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	fastStarted := make(chan string, 3)
	var active atomic.Int32
	var maxActive atomic.Int32

	r := f.runner(Kind{Handler: func(ctx context.Context, j Job) error {
		cur := active.Add(1)
		defer active.Add(-1)
		for {
			prev := maxActive.Load()
			if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
				break
			}
		}

		payload := string(j.Payload)
		if strings.Contains(payload, `"slow"`) {
			close(slowStarted)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-releaseSlow:
				return nil
			}
		}
		fastStarted <- payload
		return nil
	}})
	r.BatchSize = 2
	r.PollInterval = 5 * time.Second

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()
	defer cancel()

	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("느린 job이 시작하지 않았다")
	}
	for i := 0; i < 3; i++ {
		select {
		case <-fastStarted:
		case <-time.After(time.Second):
			t.Fatalf("느린 job 완료 전에 빠른 job %d개만 시작했다", i)
		}
	}
	if got := maxActive.Load(); got > int32(r.BatchSize) {
		t.Fatalf("동시 실행 수 = %d, want <= %d", got, r.BatchSize)
	}

	close(releaseSlow)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var succeeded int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM outbox_jobs WHERE type = $1 AND status = 'succeeded'`, f.typ).Scan(&succeeded); err != nil {
			t.Fatal(err)
		}
		if succeeded == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("모든 job이 성공하지 않았다: succeeded=%d", succeeded)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run이 종료되지 않았다")
	}
}
