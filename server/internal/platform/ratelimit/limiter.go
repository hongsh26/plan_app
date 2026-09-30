// Package ratelimit는 docs/rate_limit_design.md의 공용 요청 제한 장치다.
//
// PostgreSQL 고정 창 카운터(rate_limit_buckets)를 도메인 트랜잭션 밖의 자동 커밋
// upsert로 증가시킨다. 지켜야 할 불변식(설계 §4.2):
//
//  1. 판정은 도메인 트랜잭션 밖이다. 도메인 처리가 롤백돼도 카운트는 남는다.
//  2. 판정은 Require 다음, 다른 모든 입력 검증과 자원 조회보다 먼저다.
//  3. 판정은 멱등성 replay 확인보다 먼저다.
//  4. DB에 도달한 시도는 거부돼도 센다.
//
// 이 패키지는 사용자 ID, IP, token 원문을 저장하지도 로그에 남기지도 않는다.
// bucket 키는 HMAC-SHA256(key, scope || 0x00 || subject)다.
package ratelimit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/platform/httpapi"
)

// Mode는 제한 장치 자체의 DB 문장이 실패했을 때의 동작이다(설계 §4.5).
type Mode int

const (
	// ModeClosed는 요청을 거부한다(503). 남용 표면이라 제한 없이 통과시키지 않는다.
	ModeClosed Mode = iota
	// ModeOpen은 요청을 통과시키고 limiter_error를 기록한다.
	ModeOpen
)

// ErrUnavailable은 제한 장치가 판정하지 못했을 때(closed)의 오류다.
var ErrUnavailable = errors.New("ratelimit: 요청 제한을 확인할 수 없다")

// ErrInvalidRule은 호출자의 규칙이 잘못됐을 때(프로그래밍 오류)다.
var ErrInvalidRule = errors.New("ratelimit: 규칙이 올바르지 않다")

// Limit은 한 scope의 한도와 창이다. 값은 limits.go 한 곳에 모은다.
type Limit struct {
	Count  int
	Window time.Duration
}

// Rule은 한 번의 판정에 참여하는 규칙 하나다.
type Rule struct {
	Scope  string
	Limit  int
	Window time.Duration
	// Subject는 "user:<uuid>", "ip:<addr>", "ip:fallback", "global" 같은 문자열이다.
	// 같은 scope 안에서 user/ip가 같은 row를 가리키지 않도록 접두사를 붙인다.
	Subject string
	// Fallback은 클라이언트 IP를 해석하지 못해 ip:fallback으로 센 규칙이다. 설정 오류를
	// 경보하기 위한 표시이며 판정에는 영향이 없다.
	Fallback bool
}

// Decision은 판정 결과다.
type Decision struct {
	Allowed bool
	// RetryAfter는 거부됐을 때 창 종료까지 남은 시간이다(DB 시각 기준).
	RetryAfter time.Duration
	// Exceeded는 이 요청에서 처음 한도를 넘긴(count == limit+1) scope다. 창당 bucket당
	// 정확히 한 요청만 채운다. 인증된 scope의 audit 한 줄을 남기는 근거다.
	Exceeded []string
	// Degraded는 open 모드에서 장치 오류를 무시하고 통과시켰다는 표시다.
	Degraded bool
}

// Querier는 pgxpool.Pool이 만족한다. 테스트가 실패 pool을 주입할 수 있게 interface로 둔다.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const (
	defaultTimeout      = 250 * time.Millisecond
	defaultCacheEntries = 10000
	fallbackLogEvery    = 10 * time.Second
)

// Limiter는 요청 제한 장치다. 여러 goroutine이 함께 써도 안전하다.
type Limiter struct {
	db      Querier
	key     []byte
	logger  *slog.Logger
	now     func() time.Time
	timeout time.Duration
	maxKeys int

	mu    sync.Mutex
	block map[string]time.Time // 캐시: DB가 초과를 확인한 키 -> 로컬 만료 시각

	statsMu   sync.Mutex
	limited   map[string]int64
	errors    int64
	fallbacks int64

	lastFallbackLog atomic.Int64
}

// Option은 Limiter 설정이다.
type Option func(*Limiter)

// WithClock은 캐시 만료 판단에 쓰는 시계를 바꾼다. 테스트용이다. DB 시각과 무관하다.
func WithClock(now func() time.Time) Option { return func(l *Limiter) { l.now = now } }

// WithTimeout은 판정 문장의 ctx timeout을 바꾼다.
func WithTimeout(d time.Duration) Option { return func(l *Limiter) { l.timeout = d } }

// WithMaxCacheEntries는 사전 차단 캐시의 항목 상한을 바꾼다.
func WithMaxCacheEntries(n int) Option { return func(l *Limiter) { l.maxKeys = n } }

// New는 Limiter를 만든다. key는 32바이트 이상이어야 한다.
func New(db Querier, key []byte, logger *slog.Logger, opts ...Option) (*Limiter, error) {
	if db == nil || logger == nil {
		return nil, errors.New("ratelimit: db와 logger는 필수다")
	}
	if len(key) < 32 {
		return nil, errors.New("ratelimit: 키는 32바이트 이상이어야 한다")
	}
	l := &Limiter{
		db: db, key: append([]byte(nil), key...), logger: logger,
		now: time.Now, timeout: defaultTimeout, maxKeys: defaultCacheEntries,
		block: make(map[string]time.Time), limited: make(map[string]int64),
	}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

// keyHash는 bucket 키다. 원문 subject는 이 함수 밖으로 나가지 않는다.
func (l *Limiter) keyHash(scope, subject string) []byte {
	m := hmac.New(sha256.New, l.key)
	m.Write([]byte(scope))
	m.Write([]byte{0})
	m.Write([]byte(subject))
	return m.Sum(nil)
}

type prepared struct {
	rule Rule
	hash []byte
	ck   string // 캐시 키: scope + 0x00 + hash
}

// Allow는 규칙을 한 호출로 판정한다. 여러 규칙은 한 SQL 문으로 함께 증가시키고, 하나라도
// 한도를 넘으면 거부한다. 거부된 시도도 DB에 도달했다면 센다.
func (l *Limiter) Allow(ctx context.Context, mode Mode, rules ...Rule) (Decision, error) {
	if len(rules) == 0 {
		return Decision{}, ErrInvalidRule
	}
	ps := make([]prepared, len(rules))
	seen := make(map[string]struct{}, len(rules))
	for i, r := range rules {
		if r.Scope == "" || r.Subject == "" || r.Limit < 1 || r.Window < time.Second || r.Window%time.Second != 0 {
			return Decision{}, ErrInvalidRule
		}
		h := l.keyHash(r.Scope, r.Subject)
		ck := r.Scope + "\x00" + string(h)
		if _, dup := seen[ck]; dup {
			return Decision{}, ErrInvalidRule
		}
		seen[ck] = struct{}{}
		ps[i] = prepared{rule: r, hash: h, ck: ck}
		if r.Fallback {
			l.noteFallback(ctx, r.Scope)
		}
	}
	// 교착을 막기 위해 항상 같은 순서로 row를 건드린다.
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].rule.Scope != ps[j].rule.Scope {
			return ps[i].rule.Scope < ps[j].rule.Scope
		}
		return string(ps[i].hash) < string(ps[j].hash)
	})

	// 사전 차단: DB가 이미 초과를 확인한 키만 막는다. count == limit인 요청 다음 요청은
	// 캐시가 없으므로 DB로 가서 limit+1(초과 audit의 순간)을 받는다.
	if retry, blocked := l.cached(ps); blocked {
		l.countLimited(ps[0].rule.Scope)
		return Decision{Allowed: false, RetryAfter: retry}, nil
	}

	rows, err := l.upsert(ctx, ps)
	if err != nil {
		return l.fail(ctx, mode, ps, err)
	}

	d := Decision{Allowed: true}
	byKey := make(map[string]prepared, len(ps))
	for _, p := range ps {
		byKey[p.ck] = p
	}
	for _, row := range rows {
		p, ok := byKey[row.scope+"\x00"+string(row.keyHash)]
		if !ok {
			return l.fail(ctx, mode, ps, errors.New("ratelimit: 예상하지 못한 결과 row"))
		}
		if row.count <= p.rule.Limit {
			continue
		}
		d.Allowed = false
		// 창 종료까지 남은 시간은 DB 시각(window_start, now())로만 계산한다.
		remaining := row.windowStart.Add(p.rule.Window).Sub(row.dbNow)
		if remaining < 0 {
			remaining = 0
		}
		if remaining > d.RetryAfter {
			d.RetryAfter = remaining
		}
		l.remember(p.ck, remaining)
		if row.count == p.rule.Limit+1 {
			d.Exceeded = append(d.Exceeded, p.rule.Scope)
			l.logger.WarnContext(ctx, "요청 제한 초과",
				slog.String("request_id", httpapi.RequestID(ctx).String()),
				slog.String("action", "rate_limit"),
				slog.String("scope", p.rule.Scope),
				slog.String("result", "limited"),
			)
		}
	}
	if !d.Allowed {
		l.countLimited(ps[0].rule.Scope)
		sort.Strings(d.Exceeded)
	}
	return d, nil
}

type bucketRow struct {
	scope       string
	keyHash     []byte
	count       int
	windowStart time.Time
	dbNow       time.Time
}

const upsertSQL = `
WITH input AS (
	SELECT scope, key_hash, w
	  FROM unnest($1::text[], $2::bytea[], $3::bigint[]) AS t(scope, key_hash, w)
	 ORDER BY scope, key_hash
)
INSERT INTO rate_limit_buckets AS b (scope, key_hash, window_start, count)
SELECT scope, key_hash, date_bin(make_interval(secs => w::double precision), now(), 'epoch'::timestamptz), 1
  FROM input
 ORDER BY scope, key_hash
ON CONFLICT (scope, key_hash, window_start) DO UPDATE SET count = b.count + 1
RETURNING b.scope, b.key_hash, b.count, b.window_start, now()`

func (l *Limiter) upsert(ctx context.Context, ps []prepared) ([]bucketRow, error) {
	scopes := make([]string, len(ps))
	hashes := make([][]byte, len(ps))
	windows := make([]int64, len(ps))
	for i, p := range ps {
		scopes[i], hashes[i], windows[i] = p.rule.Scope, p.hash, int64(p.rule.Window/time.Second)
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	rows, err := l.db.Query(ctx, upsertSQL, scopes, hashes, windows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bucketRow
	for rows.Next() {
		var r bucketRow
		var count int32
		if err := rows.Scan(&r.scope, &r.keyHash, &count, &r.windowStart, &r.dbNow); err != nil {
			return nil, err
		}
		r.count = int(count)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(ps) {
		return nil, fmt.Errorf("ratelimit: 결과 row 수 %d, 기대 %d", len(out), len(ps))
	}
	return out, nil
}

// fail은 장치 오류를 mode에 따라 처리한다. 카운트는 반영됐을 수도 있다(timeout).
func (l *Limiter) fail(ctx context.Context, mode Mode, ps []prepared, err error) (Decision, error) {
	l.statsMu.Lock()
	l.errors++
	l.statsMu.Unlock()
	l.logger.ErrorContext(ctx, "요청 제한 장치 오류",
		slog.String("request_id", httpapi.RequestID(ctx).String()),
		slog.String("action", "rate_limit"),
		slog.String("scope", ps[0].rule.Scope),
		slog.String("result", "limiter_error"),
		slog.String("error_type", httpapi.ErrorType(err)),
	)
	if mode == ModeOpen {
		return Decision{Allowed: true, Degraded: true}, nil
	}
	return Decision{}, ErrUnavailable
}

// cached는 규칙 중 하나라도 캐시로 막혀 있으면 남은 시간을 돌려준다. 캐시가 막은 요청은
// 이미 초과 상태이므로 DB로 보내지 않고 세지도 않는다(설계 §4.2 불변식 4).
func (l *Limiter) cached(ps []prepared) (time.Duration, bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var retry time.Duration
	blocked := false
	for _, p := range ps {
		exp, ok := l.block[p.ck]
		if !ok {
			continue
		}
		if !now.Before(exp) {
			delete(l.block, p.ck)
			continue
		}
		blocked = true
		if d := exp.Sub(now); d > retry {
			retry = d
		}
	}
	return retry, blocked
}

// remember는 DB가 초과를 확인한 키를 캐시에 둔다. 만료는 DB가 준 남은 시간을 밀리초로
// 내림한 상대 시간이다. 절대 시각을 비교하지 않아 task와 DB의 시계 오차가 창 경계
// 오거부를 만들지 못한다.
func (l *Limiter) remember(ck string, remaining time.Duration) {
	remaining = remaining.Truncate(time.Millisecond)
	if remaining <= 0 {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.block[ck]; !ok && len(l.block) >= l.maxKeys {
		for k, exp := range l.block {
			if !now.Before(exp) {
				delete(l.block, k)
			}
		}
		if len(l.block) >= l.maxKeys {
			return
		}
	}
	l.block[ck] = now.Add(remaining)
}

func (l *Limiter) countLimited(scope string) {
	l.statsMu.Lock()
	l.limited[scope]++
	l.statsMu.Unlock()
}

// noteFallback은 클라이언트 IP 폴백을 센다. 설정 오류라 경보 대상이지만 요청마다 로그를
// 쓰면 로그 폭주가 되므로 로그는 10초에 한 번만 남긴다.
func (l *Limiter) noteFallback(ctx context.Context, scope string) {
	l.statsMu.Lock()
	l.fallbacks++
	l.statsMu.Unlock()
	now := l.now().UnixNano()
	last := l.lastFallbackLog.Load()
	if now-last < int64(fallbackLogEvery) || !l.lastFallbackLog.CompareAndSwap(last, now) {
		return
	}
	l.logger.ErrorContext(ctx, "클라이언트 IP를 해석하지 못했다. CLIENT_IP_SOURCE와 TRUSTED_PROXY_HOPS를 확인해야 한다",
		slog.String("action", "rate_limit"),
		slog.String("scope", scope),
		slog.String("result", "client_ip_fallback"),
	)
}

// Limited는 scope의 거부 횟수다(캐시로 막은 것 포함).
func (l *Limiter) Limited(scope string) int64 {
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	return l.limited[scope]
}

// Errors는 장치 오류 횟수다.
func (l *Limiter) Errors() int64 {
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	return l.errors
}

// Fallbacks는 클라이언트 IP 폴백 규칙 수다.
func (l *Limiter) Fallbacks() int64 {
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	return l.fallbacks
}
