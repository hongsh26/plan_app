package party

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/postgres/pgtest"
	"plantogether/server/internal/platform/ratelimit"
)

// limitedStack은 요청 제한이 켜진 HTTP 스택이다. 테스트마다 무작위 HMAC 키를 써서 이전 실행이나
// 다른 패키지가 남긴 bucket과 섞이지 않는다.
type limitedStack struct {
	*stack
	limiter *ratelimit.Limiter
	apple   *countingApple
	ip      string
}

type countingApple struct {
	calls atomic.Int64
	fail  atomic.Bool
}

func (a *countingApple) Verify(_ context.Context, idToken, _ string) (appleid.Identity, error) {
	a.calls.Add(1)
	if a.fail.Load() {
		return appleid.Identity{}, errors.New("잘못된 identity token")
	}
	return appleid.Identity{Subject: idToken}, nil
}

type failingLimiterDB struct{}

func (failingLimiterDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("연결 실패")
}

func uniqueIP() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("10.%d.%d.%d", b[0], b[1], b[2])
}

func newLimitedStack(t *testing.T, failLimiter bool) *limitedStack {
	t.Helper()
	awaitSafeWindow(time.Minute, 3*time.Second)
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	var db ratelimit.Querier = pool
	if failLimiter {
		db = failingLimiterDB{}
	}
	limiter, err := ratelimit.New(db, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewAccessTokens(bytes.Repeat([]byte{4}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	apple := &countingApple{}
	svc, err := auth.NewService(auth.Deps{Pool: pool, Apple: apple, Tokens: tokens, Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	authHandler := auth.NewHandler(svc, logger)
	mux := http.NewServeMux()
	authHandler.Register(mux)
	partyHandler := NewHandler(Deps{Pool: pool, Logger: logger, Limiter: limiter})
	partyHandler.Register(mux, authHandler.Require)

	ls := &limitedStack{limiter: limiter, apple: apple, ip: uniqueIP()}
	inner := httpapi.WithClientIP(httpapi.ClientIPResolver{Source: "remote_addr"}, httpapi.Wrap(logger, mux))
	ls.stack = &stack{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = ls.ip + ":4000"
			inner.ServeHTTP(w, r)
		}),
		party: partyHandler, pool: pool, logs: &logs,
	}
	return ls
}

// awaitSafeWindow는 고정 창 경계 직전이면 경계를 지날 때까지 기다린다. 창은 epoch 기준
// date_bin이라 경계에 걸리면 한 테스트 안의 요청이 두 창에 나뉘어 한도 단언이 어긋난다.
func awaitSafeWindow(window, margin time.Duration) {
	if remaining := window - time.Duration(time.Now().UnixNano())%window; remaining < margin {
		time.Sleep(remaining + 100*time.Millisecond)
	}
}

func bogusToken() string {
	return strings.ReplaceAll(uuid.NewString()+uuid.NewString()[:7], "-", "x")[:43]
}

func assertRateLimited(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assertError(t, rec, http.StatusTooManyRequests, httpapi.CodeRateLimited)
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	body := rec.Body.String()
	// 오류 code(rate_limited)에 "limit"이 들어 있으므로 JSON 키와 subject 형태로 검사한다.
	for _, leak := range []string{`"limit"`, `"remaining"`, `"retry_after"`, "user:", "ip:"} {
		if strings.Contains(body, leak) {
			t.Fatalf("429 본문이 %q를 담는다: %s", leak, body)
		}
	}
}

func auditCount(t *testing.T, s *limitedStack, action string, actor string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = $1 AND actor_user_id = $2`, action, actor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 인수 조건: 잘못된 token으로 미리보기를 반복하면 모두 404지만 그 시도가 세어져 429가 된다.
// 도메인 처리가 없거나 롤백돼도 카운트가 남아야 한다(불변식 1).
func TestPreviewWithBogusTokensEventuallyGets429AndAuditsOnce(t *testing.T) {
	s := newLimitedStack(t, false)
	u := s.signIn(t, "rl.preview."+uuid.NewString())

	for i := 0; i < 10; i++ {
		assertError(t, s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", u.AccessToken, nil),
			http.StatusNotFound, httpapi.CodeNotFound)
	}
	for i := 0; i < 3; i++ {
		assertRateLimited(t, s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", u.AccessToken, nil))
	}
	// 초과 audit는 창당 한 줄이다. 이후 거부는 audit를 늘리지 않는다.
	if n := auditCount(t, s, "rate_limit.exceeded", u.UserID); n != 1 {
		t.Fatalf("rate_limit.exceeded audit = %d, want 1", n)
	}
	// 원문 IP와 token은 로그에 없다.
	if strings.Contains(s.logs.String(), s.ip) {
		t.Fatal("로그에 원문 IP가 있다")
	}
}

// 형식 검사 404(너무 긴 token)도 한도를 소비해야 한다. 판정이 형식 검사보다 먼저다.
func TestMalformedTokenAttemptsAreCounted(t *testing.T) {
	s := newLimitedStack(t, false)
	u := s.signIn(t, "rl.malformed."+uuid.NewString())
	long := strings.Repeat("a", 500)
	for i := 0; i < 10; i++ {
		assertError(t, s.do(t, http.MethodGet, "/v1/invites/"+long+"/preview", u.AccessToken, nil),
			http.StatusNotFound, httpapi.CodeNotFound)
	}
	assertRateLimited(t, s.do(t, http.MethodGet, "/v1/invites/"+long+"/preview", u.AccessToken, nil))
}

// 수락은 사용자당 분당 5회다. 같은 Idempotency-Key 재전송도 한도를 소비한다.
func TestAcceptLimitCountsIdempotentReplays(t *testing.T) {
	s := newLimitedStack(t, false)
	u := s.signIn(t, "rl.accept."+uuid.NewString())
	tok := bogusToken()
	for i := 0; i < 5; i++ {
		assertError(t, s.accept(t, u.AccessToken, tok, "same-key"), http.StatusNotFound, httpapi.CodeNotFound)
	}
	assertRateLimited(t, s.accept(t, u.AccessToken, tok, "same-key"))
	if n := auditCount(t, s, "rate_limit.exceeded", u.UserID); n != 1 {
		t.Fatalf("audit = %d", n)
	}
}

// 한 사용자가 한도를 소진해도 다른 사용자는 영향이 없다. IP 한도(30)는 사용자 한도와 별개다.
func TestUserLimitIsIsolatedAndIPLimitIsShared(t *testing.T) {
	s := newLimitedStack(t, false)
	a := s.signIn(t, "rl.iso.a."+uuid.NewString())
	b := s.signIn(t, "rl.iso.b."+uuid.NewString())
	c := s.signIn(t, "rl.iso.c."+uuid.NewString())
	d := s.signIn(t, "rl.iso.d."+uuid.NewString())

	for i := 0; i < 10; i++ {
		s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", a.AccessToken, nil)
	}
	assertRateLimited(t, s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", a.AccessToken, nil))
	// b는 사용자 한도가 남아 있다. 여기까지 IP 카운트는 a 11 + b 1.
	assertError(t, s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", b.AccessToken, nil),
		http.StatusNotFound, httpapi.CodeNotFound)

	// 같은 IP에서 c가 10회를 더하면 IP 카운트는 22, d가 8회를 더하면 30을 채운다.
	for i := 0; i < 10; i++ {
		s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", c.AccessToken, nil)
	}
	for i := 0; i < 8; i++ {
		s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", d.AccessToken, nil)
	}
	// d의 사용자 한도(10)는 남았지만 IP 한도(30)가 찼다.
	assertRateLimited(t, s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", d.AccessToken, nil))
}

func withLimit(t *testing.T, target *ratelimit.Limit, l ratelimit.Limit) {
	t.Helper()
	old := *target
	*target = l
	t.Cleanup(func() { *target = old })
}

// 인수 조건: 한도를 넘긴 로그인은 Apple 서버를 호출하기 전에 거부된다.
func TestAuthAppleLimitRejectsBeforeCallingApple(t *testing.T) {
	withLimit(t, &ratelimit.LimitAuthApple, ratelimit.Limit{Count: 3, Window: ratelimit.LimitAuthApple.Window})
	s := newLimitedStack(t, false)
	for i := 0; i < 3; i++ {
		s.signIn(t, "rl.auth."+uuid.NewString())
	}
	calls := s.apple.calls.Load()
	rec := s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{
		"identity_token": "rl.auth." + uuid.NewString(), "authorization_code": "c", "raw_nonce": "n",
	})
	assertRateLimited(t, rec)
	if s.apple.calls.Load() != calls {
		t.Fatal("한도 초과 요청이 Apple 검증까지 도달했다")
	}
	// 미인증 scope의 초과는 audit를 늘리지 않는다.
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = 'rate_limit.exceeded' AND target_type = $1 AND created_at > now() - interval '1 minute'`,
		ratelimit.ScopeAuthApple).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("미인증 scope 초과 audit = %d, want 0", n)
	}
}

// refresh는 장치 오류에도 통과(open)하고, 429여도 세션을 지우지 않는다.
func TestRefreshLimitReturns429WithoutRevokingSession(t *testing.T) {
	withLimit(t, &ratelimit.LimitAuthRefresh, ratelimit.Limit{Count: 2, Window: ratelimit.LimitAuthRefresh.Window})
	s := newLimitedStack(t, false)
	u := s.signInFull(t, "rl.refresh."+uuid.NewString())
	for i := 0; i < 2; i++ {
		assertError(t, s.do(t, http.MethodPost, "/v1/auth/refresh", "", map[string]string{"refresh_token": "wrong-" + uuid.NewString()}),
			http.StatusUnauthorized, httpapi.CodeInvalidSession)
	}
	assertRateLimited(t, s.do(t, http.MethodPost, "/v1/auth/refresh", "", map[string]string{"refresh_token": u.RefreshToken}))
	// 거부된 refresh token은 소비되지 않았으므로 access token도 살아 있다.
	if rec := s.do(t, http.MethodGet, "/v1/parties/"+uuid.NewString(), u.AccessToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("세션이 살아 있어야 한다: %d %s", rec.Code, rec.Body.String())
	}
}

type fullSession struct {
	UserID       string `json:"user_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (s *limitedStack) signInFull(t *testing.T, subject string) fullSession {
	t.Helper()
	rec := s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{
		"identity_token": subject, "authorization_code": "c", "raw_nonce": "n",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("로그인 status = %d: %s", rec.Code, rec.Body.String())
	}
	body := decode[fullSession](t, rec)
	uid := uuid.MustParse(body.UserID)
	t.Cleanup(func() { cleanupUser(t, s.pool, uid) })
	return body
}

func failedSignInRequestIDs(t *testing.T, s *limitedStack, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rec := s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{
			"identity_token": "bad", "authorization_code": "c", "raw_nonce": "n",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("실패 로그인 status = %d: %s", rec.Code, rec.Body.String())
		}
		ids = append(ids, rec.Header().Get(httpapi.RequestIDHeader))
	}
	return ids
}

func auditRowsForRequests(t *testing.T, s *limitedStack, ids []string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE request_id = ANY($1::uuid[])`, ids).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 인수 조건: 같은 IP에서 로그인 실패를 반복해도 미인증 실패 audit 행은 10분에 5개 이하다.
func TestUnauthenticatedFailureAuditIsCappedPerIP(t *testing.T) {
	awaitSafeWindow(10*time.Minute, 15*time.Second)
	withLimit(t, &ratelimit.LimitAuthApple, ratelimit.Limit{Count: 1000, Window: ratelimit.LimitAuthApple.Window})
	s := newLimitedStack(t, false)
	s.apple.fail.Store(true)
	ids := failedSignInRequestIDs(t, s, 40)
	if n := auditRowsForRequests(t, s, ids); n != 5 {
		t.Fatalf("실패 audit 행 = %d, want 5", n)
	}
}

// IP를 순환해도 전역 상한이 전체 행 수를 막는다.
func TestUnauthenticatedFailureAuditIsCappedGlobally(t *testing.T) {
	awaitSafeWindow(10*time.Minute, 15*time.Second)
	withLimit(t, &ratelimit.LimitAuthApple, ratelimit.Limit{Count: 1000, Window: ratelimit.LimitAuthApple.Window})
	withLimit(t, &ratelimit.LimitAuditIP, ratelimit.Limit{Count: 2, Window: ratelimit.LimitAuditIP.Window})
	withLimit(t, &ratelimit.LimitAuditGlobal, ratelimit.Limit{Count: 7, Window: ratelimit.LimitAuditGlobal.Window})
	s := newLimitedStack(t, false)
	s.apple.fail.Store(true)
	var ids []string
	for i := 0; i < 8; i++ {
		s.ip = uniqueIP()
		ids = append(ids, failedSignInRequestIDs(t, s, 3)...)
	}
	// IP당 2행이 상한이라 8개 IP면 16행까지 가능하지만 전역 상한 7이 막는다.
	if n := auditRowsForRequests(t, s, ids); n != 7 {
		t.Fatalf("실패 audit 행 = %d, want 7", n)
	}
}

// 제한 장치가 죽으면 closed scope는 503 + Retry-After, open scope(refresh)는 통과한다.
func TestLimiterOutageFollowsScopeMode(t *testing.T) {
	s := newLimitedStack(t, true)
	stable := newStack(t)
	u := stable.signIn(t, "rl.outage."+uuid.NewString())

	rec := s.do(t, http.MethodGet, "/v1/invites/"+bogusToken()+"/preview", u.AccessToken, nil)
	assertError(t, rec, http.StatusServiceUnavailable, httpapi.CodeInternal)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503에 Retry-After가 없다")
	}
	assertError(t, s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{
		"identity_token": "x", "authorization_code": "c", "raw_nonce": "n",
	}), http.StatusServiceUnavailable, httpapi.CodeInternal)
	// open: 장치 오류를 무시하고 정상 처리(잘못된 token이므로 401)까지 간다.
	assertError(t, s.do(t, http.MethodPost, "/v1/auth/refresh", "", map[string]string{"refresh_token": "wrong"}),
		http.StatusUnauthorized, httpapi.CodeInvalidSession)
}
