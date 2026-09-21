// Package jobs는 outbox_jobs를 소비하는 worker의 골격이다. account_backend_design.md §9.
//
// 상태 전이:
//
//	pending ─┐
//	         ├─ claim → running ─┬─ succeeded
//	retryable_failed ─┘          ├─ retryable_failed (backoff 뒤 다시 claim)
//	                             └─ dead
//
// 점유(claim)는 FOR UPDATE SKIP LOCKED와 locked_until lease다. worker가 죽어 status가
// running인 채 lease가 지난 row도 다시 점유한다(§9 "worker crash 후 locked_until이
// 지나면 다른 worker가 안전하게 재개한다").
//
// attempt_count는 점유할 때 올린다. 실패할 때 올리면 worker를 죽이는 job(poison pill)은
// 시도가 기록되지 않아 dead에 도달하지 못하고 영원히 재시도된다(§14 "무한 재시도하지
// 않는다" 위반). 점유 시점에 올리면 lease 만료로 끝난 시도도 센다.
//
// 완료 기록은 (id, status='running', attempt_count)로 펜싱한다. lease가 지나 다른
// worker가 다시 점유했다면 attempt_count가 달라져 늦게 끝난 앞 worker의 기록은 무시된다.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Job은 점유한 outbox job 하나다.
type Job struct {
	ID      uuid.UUID
	Type    string
	Payload json.RawMessage
	// Attempt는 이번 시도의 번호다. 첫 시도가 1이다.
	Attempt int
}

// Handler는 job 하나를 처리한다. nil이면 succeeded, Permanent로 감싼 오류면 즉시
// dead, 그 밖의 오류면 재시도한다.
//
// handler는 외부 부작용이 두 번 일어나도 안전해야 한다. lease가 지난 뒤 늦게 끝난
// 시도와 다른 worker의 재시도가 겹칠 수 있다(최소 한 번 실행).
//
// 오류 메시지는 outbox_jobs.last_error와 로그에 남는다. token, Apple credential,
// 캘린더 내용, 외부 응답 본문을 넣지 않는다(§11).
type Handler func(ctx context.Context, j Job) error

// Kind는 job 종류 하나의 처리 규칙이다.
type Kind struct {
	Handler Handler
	// MaxAttempts는 이 종류의 최대 시도 횟수다(§9 "작업 종류별 최대 횟수"). 0이면
	// DefaultMaxAttempts다.
	MaxAttempts int
	// OnDead는 job이 dead로 바뀌는 트랜잭션 안에서 불린다. 선택이다.
	//
	// §9 "영구 실패는 dead로 이동하고 사용자 조치가 필요한 상태를 sync feed에 기록한다"의
	// 자리다. 사용자 조치 상태를 만드는 종류(캘린더 명령 등)가 여기서 도메인 상태와 sync
	// 변경을 dead 전이와 원자적으로 쓴다. 오류를 돌려주면 dead 전이도 롤백되고 job은
	// lease가 지난 뒤 다시 점유된다.
	OnDead func(ctx context.Context, tx pgx.Tx, j Job) error
}

// DefaultMaxAttempts는 Kind.MaxAttempts가 0일 때의 최대 시도 횟수다. 기본 backoff로
// 첫 시도부터 dead까지 약 1.5~3시간에 걸친다.
const DefaultMaxAttempts = 10

func (k Kind) maxAttempts() int {
	if k.MaxAttempts > 0 {
		return k.MaxAttempts
	}
	return DefaultMaxAttempts
}

// permanentError는 재시도해도 성공할 수 없는 실패다.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent는 err를 영구 실패로 표시한다. handler가 돌려주면 job은 재시도 없이 dead가 된다.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent는 err가 Permanent로 표시됐는지다.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// backoff 상수. §9 "재시도는 지수 backoff와 jitter를 적용한다".
const (
	backoffBase = 30 * time.Second
	backoffCap  = time.Hour
)

// Backoff는 attempt번째 시도가 실패한 뒤 다음 시도까지 기다릴 시간이다.
//
// 상한 d = min(base·2^(attempt-1), cap)을 두고 [d/2, d]에서 고른다. 절반을 바닥으로
// 두는 이유는 시도가 쌓일수록 대기가 실제로 늘어나게 하기 위해서다. 완전 jitter([0, d])는
// 여러 번 연속 0 근처를 뽑아 외부 서비스를 연달아 두드릴 수 있다.
func Backoff(attempt int, rnd *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := backoffCap
	// 2^7·30s = 64분 > 1시간이므로 그 뒤는 계산하지 않는다(시프트 넘침 방지).
	if attempt <= 7 {
		d = min(backoffBase<<(attempt-1), backoffCap)
	}
	half := d / 2
	var jitter time.Duration
	if rnd != nil {
		jitter = time.Duration(rnd.Int64N(int64(half) + 1))
	} else {
		jitter = time.Duration(rand.Int64N(int64(half) + 1))
	}
	return half + jitter
}

// maxErrorRunes는 last_error에 남기는 최대 길이다.
const maxErrorRunes = 500

// errorText는 last_error에 넣을 문자열이다. 길이를 자르고 잘못된 UTF-8을 고친다.
// 내용 검열은 하지 않는다. 민감정보를 넣지 않는 것은 handler의 책임이다(Handler 문서).
func errorText(err error) string {
	s := strings.ToValidUTF8(err.Error(), "?")
	if utf8.RuneCountInString(s) <= maxErrorRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxErrorRunes]) + "…"
}
