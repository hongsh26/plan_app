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

// Runner는 worker 루프다. 점유한 batch를 동시에 처리하고, batch가 비지 않았으면
// 기다리지 않고 곧바로 다음 batch를 점유한다(§12 "도메인 이벤트 commit 후 5초 이내
// worker 대상화").
type Runner struct {
	Pool  *pgxpool.Pool
	Kinds map[string]Kind
	// Lease는 점유 한 번의 유효 기간이다. handler 제한 시간이기도 하다.
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

func (r *Runner) types() []string {
	ts := make([]string, 0, len(r.Kinds))
	for t := range r.Kinds {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	return ts
}

// Run은 ctx가 끝날 때까지 돈다. ctx가 끝나면 새로 점유하지 않고, 처리 중인 handler를
// ShutdownTimeout까지 기다린 뒤 끝내지 못한 시도의 lease를 반납한다.
func (r *Runner) Run(ctx context.Context) error {
	if r.Lease <= 0 || r.BatchSize <= 0 || r.PollInterval <= 0 {
		return errors.New("jobs: Lease, BatchSize, PollInterval은 0보다 커야 한다")
	}
	// handler는 신호 context가 아니라 이 context를 받는다. 종료 신호 즉시 handler를
	// 끊지 않고 ShutdownTimeout 동안 끝낼 기회를 준다.
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	stop := context.AfterFunc(ctx, func() {
		if r.ShutdownTimeout <= 0 {
			cancelWork()
			return
		}
		time.AfterFunc(r.ShutdownTimeout, cancelWork)
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
	jobs, err := Claim(claimCtx, r.Pool, r.types(), r.BatchSize, r.Lease)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Go(func() { r.process(work, j) })
	}
	wg.Wait()
	return len(jobs), nil
}

func (r *Runner) process(work context.Context, j Job) {
	kind := r.Kinds[j.Type]
	log := r.Logger.With(
		slog.String("job_id", j.ID.String()),
		slog.String("job_type", j.Type),
		slog.Int("attempt", j.Attempt))

	finish := func(what string, f func(context.Context) error) {
		fctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
		defer cancel()
		err := f(fctx)
		switch {
		case errors.Is(err, ErrLeaseLost):
			// 다른 worker가 이미 다시 점유했다. 그 시도의 결과가 이긴다.
			log.Warn("job lease를 잃어 결과를 버린다",
				slog.String("action", "job_"+what), slog.String("result", "lease_lost"))
		case err != nil:
			// 기록하지 못하면 lease가 지난 뒤 다시 점유된다(최소 한 번 실행).
			log.Error("job 결과 기록 실패",
				slog.String("action", "job_"+what), slog.String("result", "failure"),
				slog.String("error", err.Error()))
		}
	}
	kill := func(cause error) {
		finish("dead", func(c context.Context) error { return Kill(c, r.Pool, j, cause, kind.OnDead) })
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

	hctx, cancel := context.WithTimeout(work, r.Lease)
	start := time.Now()
	herr := runHandler(hctx, kind.Handler, j)
	cancel()
	latency := slog.Int64("latency_ms", time.Since(start).Milliseconds())

	switch {
	case herr == nil:
		finish("succeed", func(c context.Context) error { return Succeed(c, r.Pool, j) })
		log.Info("job 완료", slog.String("action", "job_run"), slog.String("result", "success"), latency)
	case work.Err() != nil:
		// 종료로 끊겼다. job 탓이 아니므로 시도를 세지 않고 곧바로 반납한다.
		finish("release", func(c context.Context) error { return Release(c, r.Pool, j) })
		log.Info("종료로 job lease를 반납했다", slog.String("action", "job_run"), slog.String("result", "released"), latency)
	case IsPermanent(herr) || j.Attempt >= kind.maxAttempts():
		kill(herr)
	default:
		after := Backoff(j.Attempt, nil)
		finish("retry", func(c context.Context) error { return Retry(c, r.Pool, j, after, herr) })
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
