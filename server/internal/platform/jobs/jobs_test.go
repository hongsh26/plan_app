package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/platform/postgres/pgtest"
)

func TestBackoffGrowsWithinBoundsAndCaps(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	prevCeil := time.Duration(0)
	for attempt := 1; attempt <= 12; attempt++ {
		ceil := min(backoffBase<<min(attempt-1, 7), backoffCap)
		if ceil < prevCeil {
			t.Fatalf("attempt %d의 상한 %v가 앞의 %v보다 작다", attempt, ceil, prevCeil)
		}
		prevCeil = ceil
		for range 200 {
			d := Backoff(attempt, rnd)
			if d < ceil/2 || d > ceil {
				t.Fatalf("attempt %d: Backoff = %v, want [%v, %v]", attempt, d, ceil/2, ceil)
			}
		}
	}
	if d := Backoff(1000, rnd); d > backoffCap {
		t.Fatalf("큰 attempt에서 상한을 넘었다: %v", d)
	}
	if d := Backoff(0, rnd); d < backoffBase/2 || d > backoffBase {
		t.Fatalf("attempt 0은 1로 취급해야 한다: %v", d)
	}
}

func TestPermanentIsDetectedThroughWrapping(t *testing.T) {
	base := errors.New("잘못된 payload")
	err := errors.Join(errors.New("앞"), Permanent(base))
	if !IsPermanent(err) || !errors.Is(err, base) {
		t.Fatal("감싼 Permanent를 찾지 못했다")
	}
	if IsPermanent(base) || Permanent(nil) != nil {
		t.Fatal("일반 오류를 Permanent로 봤다")
	}
}

func TestErrorTextTruncatesAndFixesUTF8(t *testing.T) {
	long := errors.New(strings.Repeat("가", maxErrorRunes+10))
	if got := []rune(errorText(long)); len(got) != maxErrorRunes+1 {
		t.Fatalf("길이 = %d, want %d", len(got), maxErrorRunes+1)
	}
	if got := errorText(errors.New("a\xffb")); got != "a?b" {
		t.Fatalf("잘못된 UTF-8 처리 = %q", got)
	}
}

// ---- PostgreSQL 통합 테스트 (worker 역할) ----

// fx는 테스트 하나의 job 종류를 고유하게 만든다. 다른 패키지 테스트가 같은 DB에
// 동시에 job을 넣으므로, 종류로 격리하지 않으면 서로의 job을 점유한다.
type fx struct {
	pool *pgxpool.Pool
	typ  string
}

func newFx(t *testing.T) *fx {
	t.Helper()
	f := &fx{pool: pgtest.Pool(t, pgtest.WorkerRoleURLEnv), typ: "test_" + uuid.NewString()}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM outbox_jobs WHERE type = $1`, f.typ)
	})
	return f
}

func (f *fx) insert(t *testing.T, n int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
		if _, err := f.pool.Exec(context.Background(),
			`INSERT INTO outbox_jobs (id, type, payload) VALUES ($1, $2, '{}')`, ids[i], f.typ); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

type row struct {
	status      string
	attempts    int
	lockedUntil *time.Time
	nextRunIn   time.Duration
	lastError   *string
}

func (f *fx) row(t *testing.T, id uuid.UUID) row {
	t.Helper()
	var r row
	var nextSecs float64
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status, attempt_count, locked_until,
		       extract(epoch FROM next_run_at - now())::float8, last_error
		  FROM outbox_jobs WHERE id = $1`, id).Scan(
		&r.status, &r.attempts, &r.lockedUntil, &nextSecs, &r.lastError); err != nil {
		t.Fatal(err)
	}
	r.nextRunIn = time.Duration(nextSecs * float64(time.Second))
	return r
}

func (f *fx) runner(kind Kind) *Runner {
	return &Runner{
		Pool:            f.pool,
		Kinds:           map[string]Kind{f.typ: kind},
		Lease:           time.Minute,
		BatchSize:       10,
		PollInterval:    20 * time.Millisecond,
		ShutdownTimeout: 50 * time.Millisecond,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// §15 "SKIP LOCKED worker 경쟁": 여러 worker가 동시에 점유해도 같은 job을 두 번 받지 않는다.
func TestConcurrentClaimsNeverShareAJob(t *testing.T) {
	f := newFx(t)
	ids := f.insert(t, 60)
	ctx := context.Background()

	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for {
				got, err := Claim(ctx, f.pool, []string{f.typ}, 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				total := 0
				for _, j := range got {
					seen[j.ID]++
				}
				for _, n := range seen {
					total += n
				}
				mu.Unlock()
				// lease가 살아 있는 job을 다시 점유하면 끝나지 않는다. 멈추지 말고 실패한다.
				if total > len(ids) {
					t.Error("같은 job을 두 번 이상 점유했다")
					return
				}
			}
		})
	}
	wg.Wait()

	if len(seen) != len(ids) {
		t.Fatalf("점유한 job %d개, want %d", len(seen), len(ids))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %s를 %d번 점유했다", id, n)
		}
	}
}

func TestClaimSkipsOtherTypesAndFutureJobs(t *testing.T) {
	f := newFx(t)
	id := f.insert(t, 1)[0]
	ctx := context.Background()

	if got, err := Claim(ctx, f.pool, []string{"test_" + uuid.NewString()}, 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("다른 종류를 점유했다: %v, %v", got, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE outbox_jobs SET next_run_at = now() + interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got, err := Claim(ctx, f.pool, []string{f.typ}, 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("실행할 때가 안 된 job을 점유했다: %v, %v", got, err)
	}
}

// §15 "lease 복구": worker가 죽어 running으로 남은 job은 lease가 지나면 다시 점유되고,
// 앞 worker가 늦게 기록하려 하면 펜싱에 막힌다.
func TestExpiredLeaseIsReclaimedAndStaleWorkerIsFenced(t *testing.T) {
	f := newFx(t)
	id := f.insert(t, 1)[0]
	ctx := context.Background()

	first, err := Claim(ctx, f.pool, []string{f.typ}, 1, time.Minute)
	if err != nil || len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("첫 점유 = %v, %v", first, err)
	}
	// lease가 살아 있는 동안에는 다시 점유되지 않는다.
	if got, _ := Claim(ctx, f.pool, []string{f.typ}, 1, time.Minute); len(got) != 0 {
		t.Fatal("lease가 살아 있는 job을 다시 점유했다")
	}

	// worker가 죽었다. lease가 지났다.
	if _, err := f.pool.Exec(ctx, `UPDATE outbox_jobs SET locked_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	second, err := Claim(ctx, f.pool, []string{f.typ}, 1, time.Minute)
	if err != nil || len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("lease 만료 뒤 점유 = %v, %v", second, err)
	}

	if err := Succeed(ctx, f.pool, first[0]); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("앞 worker의 늦은 기록 err = %v, want ErrLeaseLost", err)
	}
	if err := Succeed(ctx, f.pool, second[0]); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.status != "succeeded" || r.lockedUntil != nil {
		t.Fatalf("상태 = %+v", r)
	}
}

func TestRunnerRecordsSuccessRetryAndDead(t *testing.T) {
	ctx := context.Background()

	t.Run("성공", func(t *testing.T) {
		f := newFx(t)
		id := f.insert(t, 1)[0]
		var payload string
		n, err := f.runner(Kind{Handler: func(_ context.Context, j Job) error {
			payload = string(j.Payload)
			return nil
		}}).RunOnce(ctx)
		if err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}
		if r := f.row(t, id); r.status != "succeeded" || r.attempts != 1 || payload != "{}" {
			t.Fatalf("상태 = %+v, payload = %q", r, payload)
		}
	})

	t.Run("재시도", func(t *testing.T) {
		f := newFx(t)
		id := f.insert(t, 1)[0]
		if _, err := f.runner(Kind{Handler: func(context.Context, Job) error {
			return errors.New("일시 실패")
		}}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		r := f.row(t, id)
		if r.status != "retryable_failed" || r.attempts != 1 || r.lockedUntil != nil ||
			r.lastError == nil || *r.lastError != "일시 실패" {
			t.Fatalf("상태 = %+v", r)
		}
		if r.nextRunIn < backoffBase/2-2*time.Second || r.nextRunIn > backoffBase+2*time.Second {
			t.Fatalf("다음 실행까지 %v, want 첫 backoff 범위", r.nextRunIn)
		}
	})

	t.Run("panic은 재시도", func(t *testing.T) {
		f := newFx(t)
		id := f.insert(t, 1)[0]
		if _, err := f.runner(Kind{Handler: func(context.Context, Job) error {
			panic("터졌다")
		}}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if r := f.row(t, id); r.status != "retryable_failed" || r.lastError == nil ||
			!strings.Contains(*r.lastError, "panic") {
			t.Fatalf("상태 = %+v", r)
		}
	})

	t.Run("영구 실패는 즉시 dead이고 OnDead가 같은 트랜잭션에서 돈다", func(t *testing.T) {
		f := newFx(t)
		id := f.insert(t, 1)[0]
		var seenStatus string
		if _, err := f.runner(Kind{
			Handler: func(context.Context, Job) error { return Permanent(errors.New("대상 없음")) },
			OnDead: func(ctx context.Context, tx pgx.Tx, j Job) error {
				return tx.QueryRow(ctx, `SELECT status FROM outbox_jobs WHERE id = $1`, j.ID).Scan(&seenStatus)
			},
		}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if r := f.row(t, id); r.status != "dead" || r.attempts != 1 {
			t.Fatalf("상태 = %+v", r)
		}
		if seenStatus != "dead" {
			t.Fatalf("OnDead가 본 상태 = %q, want dead(같은 트랜잭션)", seenStatus)
		}
	})

	t.Run("OnDead 실패는 dead 전이를 롤백한다", func(t *testing.T) {
		f := newFx(t)
		id := f.insert(t, 1)[0]
		if _, err := f.runner(Kind{
			Handler: func(context.Context, Job) error { return Permanent(errors.New("대상 없음")) },
			OnDead:  func(context.Context, pgx.Tx, Job) error { return errors.New("sync 기록 실패") },
		}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		// lease가 지나면 다시 점유되어 dead 처리를 다시 시도한다.
		if r := f.row(t, id); r.status != "running" || r.lockedUntil == nil {
			t.Fatalf("상태 = %+v, want running(lease 대기)", r)
		}
	})

	t.Run("최대 시도 횟수에서 dead", func(t *testing.T) {
		f := newFx(t)
		id := f.insert(t, 1)[0]
		r := f.runner(Kind{MaxAttempts: 2, Handler: func(context.Context, Job) error {
			return errors.New("일시 실패")
		}})
		for range 2 {
			if _, err := r.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE outbox_jobs SET next_run_at = now() WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.row(t, id); got.status != "dead" || got.attempts != 2 {
			t.Fatalf("상태 = %+v", got)
		}
		// dead는 다시 점유되지 않는다(§14 무한 재시도 금지).
		if n, _ := r.RunOnce(ctx); n != 0 {
			t.Fatalf("dead job을 점유했다")
		}
	})
}

// worker를 죽이는 job(poison pill)도 dead에 도달한다. 시도 횟수는 점유할 때 세므로
// lease 만료로 끝난 시도도 센다.
func TestLeaseExpiryAfterLastAttemptGoesDeadWithoutRunning(t *testing.T) {
	f := newFx(t)
	id := f.insert(t, 1)[0]
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		UPDATE outbox_jobs SET status = 'running', attempt_count = 3,
		       locked_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var ran atomic.Bool
	if _, err := f.runner(Kind{MaxAttempts: 3, Handler: func(context.Context, Job) error {
		ran.Store(true)
		return nil
	}}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("최대 시도를 넘은 job의 handler를 실행했다")
	}
	if r := f.row(t, id); r.status != "dead" || r.lastError == nil || !strings.Contains(*r.lastError, "lease") {
		t.Fatalf("상태 = %+v", r)
	}
}

// 종료 신호에 응한 handler의 job은 lease를 기다리지 않고 곧바로 반납되며 시도로 세지 않는다.
func TestShutdownReleasesLeaseWithoutCountingAttempt(t *testing.T) {
	f := newFx(t)
	id := f.insert(t, 1)[0]

	started := make(chan struct{})
	r := f.runner(Kind{Handler: func(ctx context.Context, _ Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler가 시작하지 않았다")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run이 종료 한도 안에 끝나지 않았다")
	}

	got := f.row(t, id)
	if got.status != "retryable_failed" || got.attempts != 0 || got.lockedUntil != nil || got.nextRunIn > time.Second {
		t.Fatalf("상태 = %+v, want 곧바로 다시 점유 가능", got)
	}
}

// 종료 신호 뒤에도 ShutdownTimeout 안에 끝난 handler의 결과는 성공으로 기록된다.
func TestShutdownLetsInFlightHandlerFinish(t *testing.T) {
	f := newFx(t)
	id := f.insert(t, 1)[0]

	started := make(chan struct{})
	finish := make(chan struct{})
	r := f.runner(Kind{Handler: func(context.Context, Job) error {
		close(started)
		<-finish
		return nil
	}})
	r.ShutdownTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	<-started
	cancel()
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, id); got.status != "succeeded" {
		t.Fatalf("상태 = %+v", got)
	}
}

func TestStalledCountAndPruneSucceeded(t *testing.T) {
	f := newFx(t)
	ids := f.insert(t, 3)
	ctx := context.Background()

	// 0: 오래 밀린 pending, 1: 오래전에 끝난 succeeded, 2: 방금 끝난 succeeded
	if _, err := f.pool.Exec(ctx, `UPDATE outbox_jobs SET next_run_at = now() - interval '1 hour' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE outbox_jobs SET status = 'succeeded', updated_at = now() - interval '30 days' WHERE id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE outbox_jobs SET status = 'succeeded' WHERE id = $1`, ids[2]); err != nil {
		t.Fatal(err)
	}

	n, err := StalledCount(ctx, f.pool, 15*time.Minute)
	if err != nil || n < 1 {
		t.Fatalf("StalledCount = %d, %v, want >= 1", n, err)
	}

	if _, err := PruneSucceeded(ctx, f.pool, time.Now().Add(-7*24*time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	var left []uuid.UUID
	rows, _ := f.pool.Query(ctx, `SELECT id FROM outbox_jobs WHERE type = $1`, f.typ)
	left, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("남은 job %v, want 오래된 succeeded만 지워짐", left)
	}
	for _, id := range left {
		if id == ids[1] {
			t.Fatal("오래된 succeeded가 남았다")
		}
	}
}
