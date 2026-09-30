package ratelimit

import (
	"context"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/httpapi"
)

// scope 이름과 한도는 여기 한 곳에 모은다. 운영 계측 후 조정할 때 이 파일만 바꾼다.
// 값의 근거는 docs/rate_limit_design.md §3.1이다. "잠정"은 계측 전 출발값이다.
const (
	ScopeAuthApple           = "auth.apple"
	ScopeAuthRefresh         = "auth.refresh"
	ScopeAccountDelete       = "account.delete"
	ScopeInvitePreview       = "invite.preview"
	ScopeInviteAccept        = "invite.accept"
	ScopeAuditAuthFailure    = "audit.auth_failure"
	ScopeAuditAuthFailureAll = "audit.auth_failure.global"
)

var (
	LimitAuthApple     = Limit{Count: 30, Window: time.Minute}       // IP당 (잠정)
	LimitAuthRefresh   = Limit{Count: 300, Window: time.Minute}      // IP당 (잠정)
	LimitAccountDelete = Limit{Count: 10, Window: time.Hour}         // 사용자당 (잠정)
	LimitPreviewUser   = Limit{Count: 10, Window: time.Minute}       // party_membership_design.md §4.2
	LimitPreviewIP     = Limit{Count: 30, Window: time.Minute}       //
	LimitAcceptUser    = Limit{Count: 5, Window: time.Minute}        //
	LimitAcceptIP      = Limit{Count: 30, Window: time.Minute}       //
	LimitAuditIP       = Limit{Count: 5, Window: 10 * time.Minute}   // 미인증 실패 audit, IP당
	LimitAuditGlobal   = Limit{Count: 300, Window: 10 * time.Minute} // 미인증 실패 audit, 전역 (잠정)
)

// UserRule은 인증된 사용자 규칙이다.
func UserRule(scope string, l Limit, userID uuid.UUID) Rule {
	return Rule{Scope: scope, Limit: l.Count, Window: l.Window, Subject: "user:" + userID.String()}
}

// IPRule은 context의 클라이언트 IP 규칙이다. IP를 해석하지 못했으면 ip:fallback 공유
// subject로 센다(설계 §5.3). 판정에서 빼지 않으므로 IP만 쓰는 scope가 무제한이 되지 않는다.
func IPRule(ctx context.Context, scope string, l Limit) Rule {
	ci := httpapi.ClientIPFrom(ctx)
	return Rule{Scope: scope, Limit: l.Count, Window: l.Window, Subject: ci.Subject(), Fallback: ci.Fallback || ci.Key == ""}
}

// GlobalRule은 전역 규칙이다.
func GlobalRule(scope string, l Limit) Rule {
	return Rule{Scope: scope, Limit: l.Count, Window: l.Window, Subject: "global"}
}
