package postgres

import (
	"context"
	"testing"
	"time"

	"plantogether/server/internal/platform/postgres/pgtest"
)

// 제한 장치 전용 pool은 서버 측 statement_timeout을 연결 파라미터로 갖는다. ctx timeout만
// 쓰면 pgx가 초과 시 연결을 끊어 재연결이 폭증한다(docs/rate_limit_design.md §4.5).
func TestLimiterPoolAppliesStatementTimeoutAndIsSeparate(t *testing.T) {
	domain := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	ctx := context.Background()
	p, err := NewLimiterPool(ctx, domain.Config().ConnString(), 2, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var got string
	if err := p.Pool().QueryRow(ctx, `SHOW statement_timeout`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "150ms" {
		t.Fatalf("statement_timeout = %q, want 150ms", got)
	}
	if err := domain.QueryRow(ctx, `SHOW statement_timeout`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == "150ms" {
		t.Fatal("도메인 pool에 제한 장치의 statement_timeout이 번졌다")
	}
	if p.Pool() == domain || p.Pool().Config().MaxConns != 2 {
		t.Fatalf("전용 pool이 아니다: max=%d", p.Pool().Config().MaxConns)
	}
}
