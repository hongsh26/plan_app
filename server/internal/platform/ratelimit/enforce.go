package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/audit"
	"plantogether/server/internal/platform/httpapi"
)

// Allower는 소비자가 쓰는 판정 interface다. Limiter가 구현한다. 소비자 패키지가 자체
// interface를 두어도 된다.
type Allower interface {
	Allow(ctx context.Context, mode Mode, rules ...Rule) (Decision, error)
}

// Enforce는 판정하고, 통과하지 못하면 응답까지 쓴다. false이면 호출자는 즉시 반환한다.
//
//   - a가 nil이면 제한이 꺼진 것이다(통과).
//   - 거부: 429 rate_limited + Retry-After. 본문에 키, 한도, 남은 횟수를 담지 않는다.
//   - closed 모드의 장치 오류: 503 + Retry-After.
func Enforce(w http.ResponseWriter, r *http.Request, a Allower, mode Mode, rules ...Rule) (Decision, bool) {
	if a == nil {
		return Decision{Allowed: true}, true
	}
	d, err := a.Allow(r.Context(), mode, rules...)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		httpapi.WriteError(w, r, http.StatusServiceUnavailable, httpapi.CodeInternal, "요청 제한을 확인할 수 없다. 잠시 후 다시 시도해야 한다")
		return Decision{}, false
	}
	if !d.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(d.RetryAfter)))
		httpapi.WriteError(w, r, http.StatusTooManyRequests, httpapi.CodeRateLimited, "요청이 너무 많다. 잠시 후 다시 시도해야 한다")
		return d, false
	}
	return d, true
}

// retryAfterSeconds는 헤더용 올림 초다(최소 1). 캐시 만료는 내림이고 헤더만 올림이다.
func retryAfterSeconds(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

// RecordExceeded는 사용자가 인증된 scope가 처음 한도를 넘긴 요청에 audit 한 줄을 남긴다.
// 요청당 최대 1줄이고 target_type에 첫 scope를 담는다. IP·key_hash·token은 담지 않는다.
// 미인증 scope(auth.*, invite.web)는 IP를 순환하면 행이 요청량에 비례해 늘어나므로 호출하지 않는다.
// 실패해도 요청 처리에 영향을 주지 않는다(쓰기 증폭 방지가 audit 보존보다 우선).
func RecordExceeded(ctx context.Context, db audit.Execer, actor uuid.UUID, d Decision) {
	if len(d.Exceeded) == 0 {
		return
	}
	_ = audit.Record(ctx, db, audit.Event{
		Actor: &actor, Action: "rate_limit.exceeded", TargetType: d.Exceeded[0],
		Result: "failure", RequestID: httpapi.RequestID(ctx),
	})
}
