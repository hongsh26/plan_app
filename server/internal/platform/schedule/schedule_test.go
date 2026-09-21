package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/platform/postgres/pgtest"
)

func newRunner(t *testing.T, pool *pgxpool.Pool, task Task) *Runner {
	t.Helper()
	r := &Runner{
		Pool:         pool,
		Tasks:        []Task{task},
		PollInterval: time.Second,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func uniqueName(t *testing.T, pool *pgxpool.Pool) string {
	name := "test_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM scheduled_tasks WHERE name = $1`, name)
	})
	return name
}

type state struct {
	nextRunIn time.Duration
	locked    bool
	lastError *string
	ran       bool
}

func read(t *testing.T, pool *pgxpool.Pool, name string) state {
	t.Helper()
	var s state
	var secs float64
	if err := pool.QueryRow(context.Background(), `
		SELECT extract(epoch FROM next_run_at - now())::float8, locked_until IS NOT NULL,
		       last_error, last_run_at IS NOT NULL
		  FROM scheduled_tasks WHERE name = $1`, name).Scan(&secs, &s.locked, &s.lastError, &s.ran); err != nil {
		t.Fatal(err)
	}
	s.nextRunIn = time.Duration(secs * float64(time.Second))
	return s
}

// §12 "scheduler 작업 자체는 DB lease로 중복을 방지한다": 두 scheduler가 동시에 같은
// 작업을 보더라도 한쪽만 실행한다.
func TestTwoSchedulersRunATaskOnce(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.WorkerRoleURLEnv)
	name := uniqueName(t, pool)

	var runs atomic.Int32
	release := make(chan struct{})
	task := Task{Name: name, Interval: time.Hour, Timeout: time.Minute, Run: func(context.Context) error {
		runs.Add(1)
		<-release
		return nil
	}}
	a, b := newRunner(t, pool, task), newRunner(t, pool, task)

	var wg sync.WaitGroup
	for _, r := range []*Runner{a, b} {
		wg.Go(func() { r.RunDue(context.Background()) })
	}
	// 먼저 잡은 쪽이 실행 중인 동안 다른 쪽은 점유에 실패하고 돌아온다.
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := runs.Load(); n != 1 {
		t.Fatalf("실행 횟수 = %d, want 1", n)
	}
	s := read(t, pool, name)
	if s.locked || !s.ran || s.lastError != nil || s.nextRunIn < 59*time.Minute {
		t.Fatalf("상태 = %+v, want 한 시간 뒤 다음 실행", s)
	}
	// 때가 안 됐으므로 다시 돌지 않는다.
	if ran := a.RunDue(context.Background()); len(ran) != 0 {
		t.Fatalf("때가 안 된 작업을 돌렸다: %v", ran)
	}
}

func TestFailedTaskRetriesSoonerAndRecordsError(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.WorkerRoleURLEnv)
	name := uniqueName(t, pool)
	r := newRunner(t, pool, Task{Name: name, Interval: time.Hour, Timeout: time.Minute,
		Run: func(context.Context) error { return errors.New("정리 실패") }})

	if ran := r.RunDue(context.Background()); len(ran) != 1 {
		t.Fatalf("돌린 작업 = %v", ran)
	}
	s := read(t, pool, name)
	if s.locked || s.lastError == nil || *s.lastError != "정리 실패" {
		t.Fatalf("상태 = %+v", s)
	}
	if s.nextRunIn > retryAfterFailure+time.Second || s.nextRunIn < retryAfterFailure-5*time.Second {
		t.Fatalf("다음 실행까지 %v, want 약 %v", s.nextRunIn, retryAfterFailure)
	}
}

// scheduler가 작업 도중 죽으면 lease가 지난 뒤 다른 scheduler가 이어받는다. 죽은 쪽의
// 늦은 기록은 펜싱으로 무시된다.
func TestExpiredTaskLeaseIsTakenOver(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.WorkerRoleURLEnv)
	name := uniqueName(t, pool)
	var runs atomic.Int32
	r := newRunner(t, pool, Task{Name: name, Interval: time.Hour, Timeout: time.Minute,
		Run: func(context.Context) error { runs.Add(1); return nil }})
	ctx := context.Background()

	// 다른 scheduler가 잡고 있다.
	if _, err := pool.Exec(ctx, `UPDATE scheduled_tasks SET locked_until = now() + interval '1 minute' WHERE name = $1`, name); err != nil {
		t.Fatal(err)
	}
	if ran := r.RunDue(ctx); len(ran) != 0 {
		t.Fatal("다른 scheduler가 잡고 있는 작업을 돌렸다")
	}
	// 그 scheduler가 죽고 lease가 지났다.
	if _, err := pool.Exec(ctx, `UPDATE scheduled_tasks SET locked_until = now() - interval '1 second' WHERE name = $1`, name); err != nil {
		t.Fatal(err)
	}
	if ran := r.RunDue(ctx); len(ran) != 1 || runs.Load() != 1 {
		t.Fatalf("lease 만료 뒤 이어받지 못했다: %v", ran)
	}
}

func TestEnsureRejectsIncompleteTask(t *testing.T) {
	r := &Runner{Tasks: []Task{{Name: "x"}}}
	if err := r.Ensure(context.Background()); err == nil {
		t.Fatal("Interval·Timeout·Run 없는 작업이 허용됐다")
	}
}
