package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Runner는 worker 루프다. 점유한 batch를 동시에 처리하고, batch가 끝난 뒤 다음
// batch를 점유한다. 연속 점유는 첫 job 종류를 등록하기 전에 보완한다.
type Runner struct {
	Pool  *pgxpool.Pool
	Kinds map[string]Kind
	// Lease는 점유 한 번의 유효 기간이다. handler 제한 시간은 이보다 짧다(handlerBudget).
	Lease time.Duration
	// BatchSize는 한 번에 점유하는 최대 job 수이자 동시 처리 수다.
	BatchSize int
	// PollInterval은 batch가 비었을 때 다음 점유까지 기다리는 시간이다.
	PollInterval time.Duration
	// ShutdownTimeout은 종료 신호 뒤 처리 중인 handler를 기다리는 한도다.
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
}

// finishTimeout은 완료 기록 한 번의 한도다. 종료 중에도 쓰므로 신호 context와 무관하다.
const finishTimeout = 5 * time.Second

// leaseMargin은 handler 기한과 완료 기록을 마친 뒤에도 lease가 남도록 두는 여유다.
const leaseMargin = 2 * time.Second

// MinLease는 Runner.Lease의 최솟값이다. handler에 최소 10초를 남긴다.
const MinLease = finishTimeout + leaseMargin + 10*time.Second

// handlerBudget은 점유 요청 직전부터 handler가 쓸 수 있는 시간이다.
//
// locked_until은 DB가 점유 트랜잭션을 시작한 시각 + Lease다. 그 시각은 worker가 점유를
// 요청한 뒤이므로, 요청 직전 시각 + Lease는 locked_until보다 늦지 않다. 거기서 완료 기록
// 한도와 여유를 빼면 handler가 기한까지 가도 기록이 lease 안에 끝난다. lease가 지난 뒤
// 기록하면 다른 worker가 이미 다시 점유해 두 시도가 겹치고, 앞 시도의 재시도 기록은
// 펜싱에 버려져 backoff 없이 곧바로 재실행된 셈이 된다.
func (r *Runner) handlerBudget() time.Duration {
	return r.Lease - finishTimeout - leaseMargin
}

func (r *Runner) types() []string {
	ts := make([]string, 0, len(r.Kinds))
	for t := range r.Kinds {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	return ts
}

// Run은 ctx가 끝날 때까지 돈다. ctx가 끝나면 새로 점유하지 않고, 처리 중인 handler를
// 기다린 뒤 끝내지 못한 시도의 lease를 반납한다. 반납 기록까지 ShutdownTimeout 안에
// 끝나도록 handler는 ShutdownTimeout - finishTimeout에 끊는다.
//
// handler가 취소에 응하지 않으면 Run은 그 handler가 끝날 때까지 돌아오지 않는다. 그때는
// 오케스트레이터의 강제 종료가 끝을 내고, 그 시도는 lease 만료로 끝나 시도로 센다.
func (r *Runner) Run(ctx context.Context) error {
	if r.Lease < MinLease || r.BatchSize <= 0 || r.PollInterval <= 0 {
		return fmt.Errorf("jobs: Lease는 %s 이상, BatchSize와 PollInterval은 0보다 커야 한다", MinLease)
	}
	// handler는 신호 context가 아니라 이 context를 받는다. 종료 신호 즉시 handler를
	// 끊지 않고 끝낼 기회를 준다.
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	stop := context.AfterFunc(ctx, func() {
		grace := r.ShutdownTimeout - finishTimeout
		if grace <= 0 {
			cancelWork()
			return
		}
		time.AfterFunc(grace, cancelWork)
	})
	defer stop()

	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := r.runBatch(ctx, work)
		if err != nil && ctx.Err() == nil {
			r.Logger.Warn("job 점유 실패",
				slog.String("action", "job_claim"),
				slog.String("result", "failure"),
				slog.String("error", err.Error()))
		}
		if n > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.PollInterval):
		}
	}
}

// RunOnce는 batch 하나를 점유해 처리하고 처리한 수를 돌려준다. 테스트와 수동 실행용이다.
func (r *Runner) RunOnce(ctx context.Context) (int, error) {
	return r.runBatch(ctx, ctx)
}

// runBatch는 claimCtx로 점유하고 work로 handler를 돌린다.
func (r *Runner) runBatch(claimCtx, work context.Context) (int, error) {
	deadline := time.Now().Add(r.handlerBudget())
	jobs, err := Claim(claimCtx, r.Pool, r.types(), r.BatchSize, r.Lease)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Go(func() { r.process(work, deadline, j) })
	}
	wg.Wait()
	return len(jobs), nil
}

func (r *Runner) process(work context.Context, deadline time.Time, j Job) {
	kind := r.Kinds[j.Type]
	log := r.Logger.With(
		slog.String("job_id", j.ID.String()),
		slog.String("job_type", j.Type),
		slog.Int("attempt", j.Attempt))

	// finish는 결과를 기록하고 기록이 커밋됐는지 돌려준다. 결과 로그(success, retry,
	// dead)는 기록이 커밋됐을 때만 남긴다. 경보 규칙이 그 로그를 잡으므로, 펜싱에 막혔거나
	// 롤백된 기록을 결과로 알리면 오탐이 된다.
	finish := func(what string, f func(context.Context) error) bool {
		fctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
		defer cancel()
		err := f(fctx)
		switch {
		case err == nil:
			return true
		case errors.Is(err, ErrLeaseLost):
			// 다른 worker가 이미 다시 점유했다. 그 시도의 결과가 이긴다.
			log.Warn("job lease를 잃어 결과를 버린다",
				slog.String("action", "job_"+what), slog.String("result", "lease_lost"))
		case what == "dead":
			// dead 전이가 롤백됐다(OnDead 실패 포함). job은 lease가 지난 뒤 다시 점유된다.
			// 조용히 넘어가면 dead여야 할 job이 경보 없이 되풀이되므로 따로 경보한다.
			log.Error("job dead 전이 실패",
				slog.String("action", "job_dead"), slog.String("result", "dead_failed"),
				slog.String("error", err.Error()))
		default:
			// 기록하지 못하면 lease가 지난 뒤 다시 점유된다(최소 한 번 실행).
			log.Error("job 결과 기록 실패",
				slog.String("action", "job_"+what), slog.String("result", "failure"),
				slog.String("error", err.Error()))
		}
		return false
	}
	kill := func(cause error) {
		if !finish("dead", func(c context.Context) error { return Kill(c, r.Pool, j, cause, kind.OnDead) }) {
			return
		}
		// §14: dead job은 운영 경보를 만든다. 경보 규칙은 이 로그(level=ERROR, result=dead)를 잡는다.
		log.Error("job이 dead가 됐다",
			slog.String("action", "job_run"), slog.String("result", "dead"),
			slog.String("error", errorText(cause)))
	}

	// 앞 시도가 lease 만료로 끝났고(worker crash나 handler 멈춤) 그것이 마지막 시도였다.
	if j.Attempt > kind.maxAttempts() {
		kill(fmt.Errorf("lease 만료로 최대 시도 횟수 %d를 넘었다", kind.maxAttempts()))
		return
	}

	hctx, cancel := context.WithDeadline(work, deadline)
	start := time.Now()
	herr := runHandler(hctx, kind.Handler, j)
	cancel()
	latency := slog.Int64("latency_ms", time.Since(start).Milliseconds())

	// 순서가 중요하다. Permanent는 종료 중에 나와도 job 자신의 판정이므로 반납보다
	// 먼저 본다. 반납하면 시도가 되돌려져 handler가 다시 돈다.
	switch {
	case herr == nil:
		if finish("succeed", func(c context.Context) error { return Succeed(c, r.Pool, j) }) {
			log.Info("job 완료", slog.String("action", "job_run"), slog.String("result", "success"), latency)
		}
	case IsPermanent(herr):
		kill(herr)
	case work.Err() != nil:
		// 종료로 끊겼다. job 탓이 아니므로 시도를 세지 않고 곧바로 반납한다.
		if finish("release", func(c context.Context) error { return Release(c, r.Pool, j) }) {
			log.Info("종료로 job lease를 반납했다", slog.String("action", "job_run"), slog.String("result", "released"), latency)
		}
	case j.Attempt >= kind.maxAttempts():
		kill(herr)
	default:
		after := Backoff(j.Attempt, nil)
		if !finish("retry", func(c context.Context) error { return Retry(c, r.Pool, j, after, herr) }) {
			return
		}
		log.Warn("job 실패, 재시도 예정",
			slog.String("action", "job_run"), slog.String("result", "retry"),
			slog.String("retry_after", after.String()), latency,
			slog.String("error", errorText(herr)))
	}
}

// runHandler는 handler의 panic을 오류로 바꾼다. panic 하나가 worker 전체와 같은
// batch의 다른 job을 끌고 내려가지 않게 한다.
func runHandler(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	if h == nil {
		return Permanent(errors.New("handler가 없다"))
	}
	return h(ctx, j)
}
