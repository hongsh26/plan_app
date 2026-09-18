// Package postgres는 PostgreSQL 연결 pool과 마이그레이션 러너를 제공한다.
// account_backend_design.md §3.2에 따라 PostgreSQL 전용 드라이버(pgx)와 명시적
// SQL만 사용하고 ORM을 두지 않는다.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pinger는 readiness 판정에 필요한 최소 동작만 노출한다. health 핸들러가 이
// interface에만 의존하므로 단위 테스트에 실제 DB가 필요하지 않다.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Pool은 pgxpool.Pool을 감싼다.
type Pool struct {
	pool *pgxpool.Pool
}

// NewPool은 연결 pool을 만든다. pgxpool.New는 lazy connect이므로 DB가 아직
// 떠 있지 않아도 실패하지 않는다. api가 DB 없이도 기동해 liveness 200을
// 응답하고 readiness만 503을 내는 동작이 여기에 달려 있다.
func NewPool(ctx context.Context, databaseURL string, maxConns int32) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx의 파싱 오류 메시지는 DSN 일부를 담을 수 있다. §11에 따라
		// 자격이 로그로 새지 않도록 원문을 감싸지 않고 버린다.
		return nil, errors.New("DATABASE_URL을 연결 문자열로 해석할 수 없다")
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("연결 pool을 만들지 못했다: %w", err)
	}
	return &Pool{pool: pool}, nil
}

// Ping은 DB 왕복을 확인한다. readiness 핸들러가 호출한다.
func (p *Pool) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// Pool은 도메인 패키지가 쓸 실제 pool을 돌려준다. P0에서는 통합 테스트만 쓴다.
func (p *Pool) Pool() *pgxpool.Pool { return p.pool }

// Close는 pool을 닫는다. graceful shutdown 마지막 단계에서 호출한다.
func (p *Pool) Close() { p.pool.Close() }

// DefaultPingTimeout은 readiness 검사 한 번의 한도다. DB가 멈춰 있을 때
// readiness가 dial timeout 전체를 기다리지 않고 즉시 503을 내도록 짧게 잡는다.
const DefaultPingTimeout = time.Second
