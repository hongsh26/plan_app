// Package apns는 APNs 발송 경계다. account_backend_design.md §9, §13.
//
// 지금은 interface만 있다. 실제 발송(HTTP/2, provider token 서명)과 알림 job 종류는
// 알림 구현(notification_design.md)에서 넣는다. worker의 알림 handler는 이 interface에만
// 의존하므로 테스트에 APNs가 필요 없다.
package apns

import (
	"context"
	"errors"
)

// Environment는 device token이 속한 APNs 환경이다. devices.push_environment와 같다.
type Environment string

const (
	Sandbox    Environment = "sandbox"
	Production Environment = "production"
)

// Notification은 발송 한 건이다.
//
// DeviceToken은 복호화된 원문이다. 로그, 오류 메시지, outbox payload 어디에도 남기지
// 않는다(§11). payload에는 device ID만 두고 handler가 발송 직전에 복호화한다.
type Notification struct {
	DeviceToken []byte
	Environment Environment
	// Payload는 aps JSON이다. 캘린더 제목·장소·메모를 넣지 않는다(§11).
	Payload []byte
	// CollapseID가 있으면 같은 값의 이전 알림을 기기에서 대체한다.
	CollapseID string
}

// ErrTokenInvalid는 APNs가 token이 더 이상 유효하지 않다고 답한 경우다(410, BadDeviceToken 등).
// handler는 이 오류를 받으면 해당 token을 비활성화하고 다시 발송하지 않는다(§14).
var ErrTokenInvalid = errors.New("apns: device token이 유효하지 않다")

// Sender는 APNs에 알림을 보낸다. 재시도할 수 있는 실패는 일반 오류로, 재시도해도 소용없는
// 실패는 ErrTokenInvalid 또는 jobs.Permanent로 감싼 오류로 돌려준다.
type Sender interface {
	Send(ctx context.Context, n Notification) error
}
