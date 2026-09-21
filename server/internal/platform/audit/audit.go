// Package audit는 보안 audit 이벤트를 기록한다(account_backend_design.md §11).
//
// token, Apple credential, 캘린더 제목·장소·메모, 초대 원문을 넣지 않는다.
// 이 패키지의 Event에는 그런 값을 담을 필드가 없다.
package audit

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execer는 pgx.Tx와 pgxpool.Pool이 모두 만족한다. 도메인 변경과 같은
// 트랜잭션에 남길 때는 Tx를, 트랜잭션 밖 실패 기록에는 Pool을 넘긴다.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Event는 audit 한 줄이다.
type Event struct {
	// Actor는 행위자다. 인증 전 실패처럼 모를 때는 nil이다.
	Actor      *uuid.UUID
	Action     string
	TargetType string
	TargetID   *uuid.UUID
	// Result는 success 또는 failure다(audit_events_result_check).
	Result    string
	RequestID uuid.UUID
}

// Record는 이벤트를 남긴다.
func Record(ctx context.Context, db Execer, e Event) error {
	var target *string
	if e.TargetType != "" {
		target = &e.TargetType
	}
	var req *uuid.UUID
	if e.RequestID != uuid.Nil {
		req = &e.RequestID
	}
	_, err := db.Exec(ctx, `
		INSERT INTO audit_events (id, actor_user_id, action, target_type, target_id, result, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(), e.Actor, e.Action, target, e.TargetID, e.Result, req)
	return err
}
