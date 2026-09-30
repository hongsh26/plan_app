package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/postgres/pgtest"
	"plantogether/server/internal/platform/ratelimit"
	"plantogether/server/internal/platform/secretbox"
)

// 계정 삭제 요청 제한은 사용자당 시간당 N회다. 성공한 삭제 뒤에는 계정이 잠겨 Require가 막으므로
// 여기서 세는 것은 400/409 실패 반복이다. 무작위 HMAC 키로 다른 실행의 bucket과 섞이지 않는다.
func TestDeleteMeLimitCountsFailedAttemptsAndAuditsOnce(t *testing.T) {
	old := ratelimit.LimitAccountDelete
	ratelimit.LimitAccountDelete = ratelimit.Limit{Count: 3, Window: old.Window}
	t.Cleanup(func() { ratelimit.LimitAccountDelete = old })

	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	limiter, err := ratelimit.New(pool, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewAccessTokens(bytes.Repeat([]byte{3}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(auth.Deps{Pool: pool, Apple: subjectApple{}, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	authHandler := auth.NewHandler(svc, logger)
	mux := http.NewServeMux()
	authHandler.Register(mux)
	box, err := secretbox.NewLocal("local", bytes.Repeat([]byte{9}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	NewHandler(Deps{Pool: pool, Logger: logger, Sealer: box, RefKeyBox: box, Limiter: limiter}).Register(mux, authHandler.Require)
	s := &stack{handler: httpapi.Wrap(logger, mux), pool: pool, box: box, logs: &logs}

	u := s.signIn(t, "rl.delete."+uuid.NewString())

	// 낡은 If-Match라 매번 409지만 시도는 센다.
	for i := 0; i < 3; i++ {
		assertError(t, s.mut(t, http.MethodDelete, "/v1/me", u.AccessToken, "del-"+strconv.Itoa(i), `"99"`, ``),
			http.StatusConflict, httpapi.CodeVersionConflict)
	}
	for i := 0; i < 2; i++ {
		rec := s.mut(t, http.MethodDelete, "/v1/me", u.AccessToken, "del-x"+strconv.Itoa(i), `"1"`, ``)
		assertError(t, rec, http.StatusTooManyRequests, httpapi.CodeRateLimited)
		if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 {
			t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
		}
	}
	// 거부된 삭제는 아무것도 바꾸지 않았다.
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id = $1`, u.UserID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("status = %s", status)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = 'rate_limit.exceeded' AND actor_user_id = $1`, u.UserID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rate_limit.exceeded audit = %d, want 1", n)
	}
}
