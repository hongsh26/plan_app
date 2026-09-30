package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/platform/ratelimit"
)

type downLimiterDB struct{}

func (downLimiterDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("연결 실패")
}

func failureAuditRows(t *testing.T, e *env, requestID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 인수 조건: 제한 장치가 오류일 때 미인증 실패 audit를 쓰지 않고 로그인 실패 결과는 그대로다.
// 대조군으로 Limiter 없이 같은 실패가 audit 행을 남기는 것도 확인한다.
func TestAuditFailureIsSkippedWhenLimiterErrors(t *testing.T) {
	e := newEnv(t)
	tokens, err := NewAccessTokens(bytes.Repeat([]byte{1}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := ratelimit.New(downLimiterDB{}, bytes.Repeat([]byte{5}, 32), slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	limited, err := NewService(Deps{Pool: e.pool, Apple: fakeApple{}, Tokens: tokens, Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}

	control := uuid.New()
	if _, err := e.svc.SignInWithApple(context.Background(), SignInInput{IdentityToken: "bad-1", RawNonce: "n", RequestID: control}); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("대조군 err = %v", err)
	}
	if n := failureAuditRows(t, e, control); n != 1 {
		t.Fatalf("Limiter 없는 실패 audit = %d, want 1", n)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM audit_events WHERE request_id = $1`, control)
	})

	down := uuid.New()
	if _, err := limited.SignInWithApple(context.Background(), SignInInput{IdentityToken: "bad-2", RawNonce: "n", RequestID: down}); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("장치 오류 중 로그인 실패 결과가 바뀌었다: %v", err)
	}
	if n := failureAuditRows(t, e, down); n != 0 {
		t.Fatalf("제한 장치 오류 중 실패 audit = %d, want 0", n)
	}
}
