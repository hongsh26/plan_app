package ratelimit

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/platform/postgres/pgtest"
)

// 테스트마다 무작위 HMAC 키를 쓴다. 키가 다르면 같은 scope·subject라도 key_hash가 달라
// 이전 실행이나 다른 패키지가 남긴 bucket과 섞이지 않는다.
func newLimiter(t *testing.T, db Querier, opts ...Option) (*Limiter, *bytes.Buffer) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return newLimiterWithKey(t, db, key, opts...)
}

func newLimiterWithKey(t *testing.T, db Querier, key []byte, opts ...Option) (*Limiter, *bytes.Buffer) {
	t.Helper()
	awaitSafeWindow(time.Hour, 5*time.Second)
	var logs bytes.Buffer
	l, err := New(db, key, slog.New(slog.NewJSONHandler(&logs, nil)), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return l, &logs
}

// awaitSafeWindow는 고정 창 경계 직전이면 경계를 지날 때까지 기다린다. 창은 epoch 기준
// date_bin이라 경계에 걸리면 한 테스트 안의 요청이 두 창에 나뉘어 한도 단언이 어긋난다.
func awaitSafeWindow(window, margin time.Duration) {
	if remaining := window - time.Duration(time.Now().UnixNano())%window; remaining < margin {
		time.Sleep(remaining + 100*time.Millisecond)
	}
}

func rule(scope, subject string, limit int, window time.Duration) Rule {
	return Rule{Scope: scope, Subject: subject, Limit: limit, Window: window}
}

func bucketCount(t *testing.T, pool *pgxpool.Pool, l *Limiter, scope, subject string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT coalesce(sum(count), 0) FROM rate_limit_buckets WHERE scope = $1 AND key_hash = $2`,
		scope, l.keyHash(scope, subject)).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAllowRejectsOverLimitWithRetryAfter(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	l, _ := newLimiter(t, pool)
	scope := "test." + uuid.NewString()
	r := rule(scope, "user:a", 3, time.Hour)
	for i := 1; i <= 3; i++ {
		d, err := l.Allow(context.Background(), ModeClosed, r)
		if err != nil || !d.Allowed {
			t.Fatalf("%d번째 = %+v, %v", i, d, err)
		}
	}
	d, err := l.Allow(context.Background(), ModeClosed, r)
	if err != nil || d.Allowed {
		t.Fatalf("4번째 = %+v, %v", d, err)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Hour {
		t.Fatalf("RetryAfter = %v", d.RetryAfter)
	}
	// 다른 subject는 영향이 없다.
	if d, err := l.Allow(context.Background(), ModeClosed, rule(scope, "user:b", 3, time.Hour)); err != nil || !d.Allowed {
		t.Fatalf("다른 사용자 = %+v, %v", d, err)
	}
}

// 인수 조건: 거부되기 직전까지의 시도와 초과 시도가 모두 DB 카운트에 남고, 초과 audit의
// 근거(Exceeded)는 한도를 처음 넘기는 요청 하나에만 있다. 캐시가 그 순간을 지우면 안 된다.
func TestExceededIsReportedOnceAndCacheDoesNotHideIt(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	l, _ := newLimiter(t, pool)
	scope := "test." + uuid.NewString()
	r := rule(scope, "user:a", 2, time.Hour)
	var exceeded int
	for i := 0; i < 6; i++ {
		d, err := l.Allow(context.Background(), ModeClosed, r)
		if err != nil {
			t.Fatal(err)
		}
		exceeded += len(d.Exceeded)
		if i == 2 && len(d.Exceeded) != 1 {
			t.Fatalf("limit+1번째 요청이 DB를 거치지 않았다: %+v", d)
		}
	}
	if exceeded != 1 {
		t.Fatalf("Exceeded 보고 횟수 = %d, want 1", exceeded)
	}
	// limit+1까지만 DB에 닿았고 이후는 캐시가 막아 세지 않는다(불변식 4).
	if n := bucketCount(t, pool, l, scope, "user:a"); n != 3 {
		t.Fatalf("DB 카운트 = %d, want 3", n)
	}
}

// 캐시를 가진 Limiter 두 개가 같은 DB를 써도 합산 한도가 지켜지고 초과 보고는 한 번이다.
func TestTwoLimitersShareCountsAndReportExceededOnce(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	key := bytes.Repeat([]byte{7}, 32)
	a, _ := newLimiterWithKey(t, pool, key)
	b, _ := newLimiterWithKey(t, pool, key)
	scope := "test." + uuid.NewString()
	r := rule(scope, "ip:198.51.100.1", 4, time.Hour)

	var allowed, exceeded int
	for i := 0; i < 8; i++ {
		l := a
		if i%2 == 1 {
			l = b
		}
		d, err := l.Allow(context.Background(), ModeClosed, r)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			allowed++
		}
		exceeded += len(d.Exceeded)
	}
	if allowed != 4 || exceeded != 1 {
		t.Fatalf("allowed=%d exceeded=%d, want 4 and 1", allowed, exceeded)
	}
}

func TestConcurrentAllowNeverAdmitsMoreThanLimit(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	key := bytes.Repeat([]byte{9}, 32)
	scope := "test." + uuid.NewString()
	r := rule(scope, "user:a", 5, time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed, exceeded := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _ := newLimiterWithKey(t, pool, key)
			d, err := l.Allow(context.Background(), ModeClosed, r)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if d.Allowed {
				allowed++
			}
			exceeded += len(d.Exceeded)
		}()
	}
	wg.Wait()
	if allowed != 5 || exceeded != 1 {
		t.Fatalf("allowed=%d exceeded=%d, want 5 and 1", allowed, exceeded)
	}
}

// 캐시는 DB가 초과를 확인한 창에서만 막는다. 창이 지나면 다시 통과해야 한다.
func TestCacheDoesNotRejectAfterWindowRolls(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	l, _ := newLimiter(t, pool)
	scope := "test." + uuid.NewString()
	r := rule(scope, "user:a", 1, time.Second)
	// 창 경계에 걸리면 첫 두 요청이 서로 다른 창일 수 있으므로 초과가 확인될 때까지 시도한다.
	var d Decision
	for i := 0; i < 5; i++ {
		var err error
		if d, err = l.Allow(context.Background(), ModeClosed, r); err != nil {
			t.Fatal(err)
		}
		if !d.Allowed {
			break
		}
		if i == 4 {
			t.Fatal("초과를 만들지 못했다")
		}
	}
	if d.RetryAfter > time.Second {
		t.Fatalf("RetryAfter = %v", d.RetryAfter)
	}
	time.Sleep(1200 * time.Millisecond)
	d, err := l.Allow(context.Background(), ModeClosed, r)
	if err != nil || !d.Allowed {
		t.Fatalf("창이 지난 뒤 = %+v, %v", d, err)
	}
}

func TestMultiRuleCountsBothAndDeniesOnEither(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	l, _ := newLimiter(t, pool)
	scope := "test." + uuid.NewString()
	user := rule(scope, "user:a", 2, time.Hour)
	ip := rule(scope, "ip:198.51.100.1", 100, time.Hour)
	for i := 0; i < 2; i++ {
		if d, err := l.Allow(context.Background(), ModeClosed, user, ip); err != nil || !d.Allowed {
			t.Fatalf("%d = %+v, %v", i, d, err)
		}
	}
	d, err := l.Allow(context.Background(), ModeClosed, user, ip)
	if err != nil || d.Allowed {
		t.Fatalf("사용자 한도 초과 = %+v, %v", d, err)
	}
	if len(d.Exceeded) != 1 || d.Exceeded[0] != scope {
		t.Fatalf("Exceeded = %v", d.Exceeded)
	}
	// 거부된 시도도 IP 카운트에 남는다.
	if n := bucketCount(t, pool, l, scope, "ip:198.51.100.1"); n != 3 {
		t.Fatalf("IP 카운트 = %d, want 3", n)
	}
}

func TestInvalidRulesAreRejected(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	l, _ := newLimiter(t, pool)
	ctx := context.Background()
	for name, rules := range map[string][]Rule{
		"규칙 없음":      nil,
		"빈 scope":    {rule("", "user:a", 1, time.Minute)},
		"빈 subject":  {rule("s", "", 1, time.Minute)},
		"한도 0":       {rule("s", "user:a", 0, time.Minute)},
		"창이 초 단위 아님": {rule("s", "user:a", 1, 1500*time.Millisecond)},
		"같은 키 중복":    {rule("s", "user:a", 1, time.Minute), rule("s", "user:a", 1, time.Minute)},
	} {
		if _, err := l.Allow(ctx, ModeClosed, rules...); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// bucket 표에는 keyed hash만 있고 원문 subject(IP, 사용자 ID)가 없다. 로그에도 없다.
func TestNoRawSubjectInBucketsOrLogs(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	l, logs := newLimiter(t, pool)
	scope := "test." + uuid.NewString()
	subject := "ip:203.0.113.77"
	for i := 0; i < 3; i++ {
		if _, err := l.Allow(context.Background(), ModeClosed, rule(scope, subject, 1, time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	var text string
	var hashLen int
	if err := pool.QueryRow(context.Background(),
		`SELECT row_to_json(b)::text, length(key_hash) FROM rate_limit_buckets b WHERE scope = $1`, scope).Scan(&text, &hashLen); err != nil {
		t.Fatal(err)
	}
	if hashLen != 32 || strings.Contains(text, "203.0.113.77") {
		t.Fatalf("bucket row = %s (hash %d바이트)", text, hashLen)
	}
	if strings.Contains(logs.String(), "203.0.113.77") {
		t.Fatalf("로그에 원문 IP가 있다: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"result":"limited"`) {
		t.Fatalf("초과 로그가 없다: %s", logs.String())
	}
}

type failingDB struct{}

func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("연결 실패")
}

func TestDeviceFailureFollowsMode(t *testing.T) {
	l, logs := newLimiter(t, failingDB{})
	r := rule("test.fail", "ip:198.51.100.1", 1, time.Minute)
	if _, err := l.Allow(context.Background(), ModeClosed, r); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed err = %v", err)
	}
	d, err := l.Allow(context.Background(), ModeOpen, r)
	if err != nil || !d.Allowed || !d.Degraded {
		t.Fatalf("open = %+v, %v", d, err)
	}
	if l.Errors() != 2 || !strings.Contains(logs.String(), `"result":"limiter_error"`) {
		t.Fatalf("errors=%d logs=%s", l.Errors(), logs.String())
	}
}

func TestFallbackRuleIsCountedAndSurfaced(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	l, logs := newLimiter(t, pool)
	scope := "test." + uuid.NewString()
	r := rule(scope, "ip:fallback", 2, time.Hour)
	r.Fallback = true
	var denied bool
	for i := 0; i < 3; i++ {
		d, err := l.Allow(context.Background(), ModeClosed, r)
		if err != nil {
			t.Fatal(err)
		}
		denied = !d.Allowed
	}
	if !denied {
		t.Fatal("폴백 요청이 IP만 쓰는 scope에서 무제한이 됐다")
	}
	if l.Fallbacks() != 3 || !strings.Contains(logs.String(), "client_ip_fallback") {
		t.Fatalf("fallbacks=%d logs=%s", l.Fallbacks(), logs.String())
	}
}

func TestPruneRemovesOnlyOldBucketsAsWorkerRole(t *testing.T) {
	worker := pgtest.Pool(t, pgtest.WorkerRoleURLEnv)
	ctx := context.Background()
	scope := "test." + uuid.NewString()
	hash := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	for _, row := range []struct {
		h   byte
		age string
	}{{1, "3 hours"}, {2, "119 minutes"}, {3, "0 seconds"}} {
		if _, err := worker.Exec(ctx, `
			INSERT INTO rate_limit_buckets (scope, key_hash, window_start, count)
			VALUES ($1, $2, now() - $3::interval, 1)`, scope, hash(row.h), row.age); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Prune(ctx, worker, Retention, 2); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := worker.QueryRow(ctx, `SELECT count(*) FROM rate_limit_buckets WHERE scope = $1`, scope).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Fatalf("남은 row = %d, want 2 (2시간 안쪽은 살아 있어야 한다)", left)
	}
	if _, err := worker.Exec(ctx, `DELETE FROM rate_limit_buckets WHERE scope = $1`, scope); err != nil {
		t.Fatal(err)
	}
}

func TestPruneBatchLoopDrainsBacklog(t *testing.T) {
	worker := pgtest.Pool(t, pgtest.WorkerRoleURLEnv)
	ctx := context.Background()
	scope := "test." + uuid.NewString()
	if _, err := worker.Exec(ctx, `
		INSERT INTO rate_limit_buckets (scope, key_hash, window_start, count)
		SELECT $1, decode(lpad(to_hex(g), 64, '0'), 'hex'), now() - interval '5 hours', 1
		  FROM generate_series(1, 25) g`, scope); err != nil {
		t.Fatal(err)
	}
	n, err := Prune(ctx, worker, Retention, 10)
	if err != nil || n < 25 {
		t.Fatalf("삭제 수 = %d, err = %v", n, err)
	}
}
