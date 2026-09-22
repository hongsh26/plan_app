// Package schedule은 scheduler의 주기 작업 골격이다. account_backend_design.md §9, §12.
//
// 작업마다 scheduled_tasks에 한 줄을 두고 DB lease로 점유한다. scheduler가 여럿 떠도
// 한 작업은 한 번에 하나만 돈다. leader 선출은 따로 없다(migrations/00005 주석).
package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Task는 주기 작업 하나다.
type Task struct {
	// Name은 scheduled_tasks의 키다. 바꾸면 새 작업으로 취급된다.
	Name string
	// Interval은 성공한 실행 끝에서 다음 실행까지의 간격이다.
	Interval time.Duration
	// Timeout은 lease 기간이다. 실행 한도는 결과 기록을 lease 안에 끝내도록 이보다
	// finishTimeout + leaseMargin만큼 짧다.
	Timeout time.Duration
	Run     func(ctx context.Context) error
}

// finishTimeout은 결과 기록 한 번의 한도다.
const finishTimeout = 5 * time.Second

// leaseMargin은 실행 한도와 결과 기록을 마친 뒤에도 lease가 남도록 두는 여유다.
const leaseMargin = 2 * time.Second

// MinTimeout은 Task.Timeout의 최솟값이다. 실행에 최소 10초를 남긴다.
const MinTimeout = finishTimeout + leaseMargin + 10*time.Second

// retryAfterFailure는 실패한 작업을 다시 시도하기까지의 최대 간격이다.
const retryAfterFailure = 5 * time.Minute

// Runner는 scheduler 루프다.
type Runner struct {
	Pool         *pgxpool.Pool
	Tasks        []Task
	PollInterval time.Duration
	Logger       *slog.Logger
}

// Ensure는 작업마다 scheduled_tasks 줄이 있게 한다. 새 작업은 곧바로 실행 대상이다.
func (r *Runner) Ensure(ctx context.Context) error {
	for _, t := range r.Tasks {
		if t.Interval <= 0 || t.Timeout <= 0 || t.Run == nil {
			return fmt.Errorf("schedule: 작업 %q의 Interval, Timeout, Run이 필요하다", t.Name)
		}
		if t.Timeout < MinTimeout {
			return fmt.Errorf("schedule: 작업 %q의 Timeout은 %s 이상이어야 한다", t.Name, MinTimeout)
		}
		if _, err := r.Pool.Exec(ctx,
			`INSERT INTO scheduled_tasks (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`, t.Name); err != nil {
			return err
		}
	}
	return nil
}

// Run은 ctx가 끝날 때까지 PollInterval마다 때가 된 작업을 돌린다.
func (r *Runner) Run(ctx context.Context) error {
	if r.PollInterval <= 0 {
		return errors.New("schedule: PollInterval은 0보다 커야 한다")
	}
	if err := r.Ensure(ctx); err != nil {
		return err
	}
	for {
		r.RunDue(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.PollInterval):
		}
	}
}

// RunDue는 때가 됐고 다른 scheduler가 잡고 있지 않은 작업을 차례로 돌린다. 돌린 작업
// 이름을 돌려준다.
func (r *Runner) RunDue(ctx context.Context) []string {
	var ran []string
	for _, t := range r.Tasks {
		if ctx.Err() != nil {
			break
		}
		ok, err := r.runTask(ctx, t)
		if err != nil {
			r.Logger.Error("예약 작업 실패",
				slog.String("action", "task_"+t.Name),
				slog.String("result", "failure"),
				slog.String("error", err.Error()))
		}
		if ok {
			ran = append(ran, t.Name)
		}
	}
	return ran
}

// runTask는 t를 점유하면 돌리고 true를 돌려준다.
func (r *Runner) runTask(ctx context.Context, t Task) (bool, error) {
	// 한 줄 UPDATE다. 두 scheduler가 동시에 오면 뒤의 것은 앞의 커밋을 기다렸다가
	// WHERE를 다시 평가해 locked_until이 채워진 것을 보고 0줄이 된다.
	// 실행 기한은 점유 요청 직전 시각에서 잰다. locked_until은 DB가 이보다 늦게 잰
	// now() + Timeout이므로, 기한 + 결과 기록 한도는 lease 안에 들어간다(jobs.handlerBudget와 같다).
	deadline := time.Now().Add(t.Timeout - finishTimeout - leaseMargin)
	var lockedUntil time.Time
	err := r.Pool.QueryRow(ctx, `
		UPDATE scheduled_tasks
		   SET locked_until = now() + make_interval(secs => $2), updated_at = now()
		 WHERE name = $1
		   AND next_run_at <= now()
		   AND (locked_until IS NULL OR locked_until <= now())
		RETURNING locked_until`, t.Name, t.Timeout.Seconds()).Scan(&lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	tctx, cancel := context.WithDeadline(ctx, deadline)
	start := time.Now()
	runErr := t.Run(tctx)
	cancel()

	next := t.Interval
	var lastError *string
	if runErr != nil {
		next = min(t.Interval, retryAfterFailure)
		s := runErr.Error()
		lastError = &s
	}
	// 종료 신호로 ctx가 끝났어도 결과는 기록한다.
	fctx, fcancel := context.WithTimeout(context.Background(), finishTimeout)
	defer fcancel()
	// locked_until로 펜싱한다. lease가 지나 다른 scheduler가 다시 잡았으면 그쪽이 기록한다.
	tag, err := r.Pool.Exec(fctx, `
		UPDATE scheduled_tasks
		   SET next_run_at = now() + make_interval(secs => $3),
		       locked_until = NULL,
		       last_run_at = now(),
		       last_error = $4,
		       updated_at = now()
		 WHERE name = $1 AND locked_until = $2`,
		t.Name, lockedUntil, next.Seconds(), lastError)
	if err != nil {
		return true, errors.Join(runErr, err)
	}
	if tag.RowsAffected() == 0 {
		// lease가 지나 다른 scheduler가 다시 잡았다. 이 실행의 결과는 버려진다.
		r.Logger.Warn("예약 작업 lease를 잃어 결과를 버린다",
			slog.String("action", "task_"+t.Name),
			slog.String("result", "lease_lost"),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()))
		return true, runErr
	}
	if runErr == nil {
		r.Logger.Info("예약 작업 완료",
			slog.String("action", "task_"+t.Name),
			slog.String("result", "success"),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()))
	}
	return true, runErr
}
