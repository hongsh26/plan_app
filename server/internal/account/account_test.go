package account

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/postgres/pgtest"
)

// HTTP 경계 전체(request_id → access log → 라우팅 → 인증 → 핸들러)를 실제
// PostgreSQL 위에서 돌린다. 서비스 단위 테스트가 잡지 못하는 것, 즉 상태 코드와
// §6 오류 코드 매핑, 라우트 등록, 응답 형식을 본다.

type subjectApple struct{}

func (subjectApple) Verify(_ context.Context, idToken, _ string) (appleid.Identity, error) {
	if idToken == "bad" {
		return appleid.Identity{}, appleid.ErrNonceMismatch
	}
	return appleid.Identity{Subject: idToken}, nil
}

type stack struct {
	handler http.Handler
	pool    *pgxpool.Pool
	logs    *bytes.Buffer
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

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
	NewHandler(pool, logger).Register(mux, authHandler.Require)
	return &stack{handler: httpapi.Wrap(logger, mux), pool: pool, logs: &logs}
}

func (s *stack) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("응답이 JSON이 아니다: %v (%s)", err, rec.Body.String())
	}
	return v
}

type sessionBody struct {
	RequestID    string `json:"request_id"`
	UserID       string `json:"user_id"`
	DeviceID     string `json:"device_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	NewUser      bool   `json:"new_user"`
}

func (s *stack) signIn(t *testing.T, subject string) sessionBody {
	t.Helper()
	rec := s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{
		"identity_token": subject, "authorization_code": "c", "raw_nonce": "n", "display_name": "  홍길동\u0000 ",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("로그인 status = %d: %s", rec.Code, rec.Body.String())
	}
	body := decode[sessionBody](t, rec)
	uid := uuid.MustParse(body.UserID)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.pool.Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id = $1`, uid)
		_, _ = s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, uid)
	})
	return body
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	e := decode[httpapi.ErrorResponse](t, rec)
	if e.Code != code {
		t.Errorf("code = %q, want %q", e.Code, code)
	}
	if e.RequestID == "" || e.RequestID != rec.Header().Get(httpapi.RequestIDHeader) {
		t.Errorf("오류 본문 request_id %q가 헤더 %q와 다르다", e.RequestID, rec.Header().Get(httpapi.RequestIDHeader))
	}
}

func TestSignInMeLogoutFlow(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "http."+uuid.NewString())

	if !sess.NewUser || sess.RequestID == "" {
		t.Errorf("new_user=%v request_id=%q", sess.NewUser, sess.RequestID)
	}

	rec := s.do(t, http.MethodGet, "/v1/me", sess.AccessToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/me status = %d: %s", rec.Code, rec.Body.String())
	}
	me := decode[meResponse](t, rec)
	if me.User.ID != sess.UserID || me.User.Status != "active" || me.User.Version != 1 {
		t.Errorf("me = %+v", me.User)
	}
	if me.User.DisplayName != "홍길동" {
		t.Errorf("표시 이름이 정규화되지 않았다: %q", me.User.DisplayName)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("사용자 데이터 응답에 no-store가 없다")
	}

	if rec := s.do(t, http.MethodPost, "/v1/auth/logout", sess.AccessToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("로그아웃 status = %d: %s", rec.Code, rec.Body.String())
	}
	assertError(t, s.do(t, http.MethodGet, "/v1/me", sess.AccessToken, nil),
		http.StatusUnauthorized, httpapi.CodeInvalidSession)
	assertError(t, s.do(t, http.MethodPost, "/v1/auth/refresh", "", map[string]string{"refresh_token": sess.RefreshToken}),
		http.StatusUnauthorized, httpapi.CodeInvalidSession)
}

func TestProtectedRoutesRequireBearerToken(t *testing.T) {
	s := newStack(t)
	for _, tc := range []struct{ method, path, token string }{
		{http.MethodGet, "/v1/me", ""},
		{http.MethodGet, "/v1/devices", ""},
		{http.MethodPost, "/v1/auth/logout", ""},
		{http.MethodGet, "/v1/me", "not-a-jwt"},
	} {
		rec := s.do(t, tc.method, tc.path, tc.token, nil)
		assertError(t, rec, http.StatusUnauthorized, httpapi.CodeInvalidSession)
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: 401에 WWW-Authenticate가 없다", tc.method, tc.path)
		}
	}
}

func TestLockedAccountGets423(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "http."+uuid.NewString())
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE users SET status = 'disabled' WHERE id = $1`, sess.UserID); err != nil {
		t.Fatal(err)
	}
	assertError(t, s.do(t, http.MethodGet, "/v1/me", sess.AccessToken, nil),
		http.StatusLocked, httpapi.CodeAccountLocked)
}

func TestSignInValidation(t *testing.T) {
	s := newStack(t)
	assertError(t, s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{"identity_token": "x"}),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{
		"identity_token": "x", "authorization_code": "c", "raw_nonce": "n", "unknown": "y"}),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{
		"identity_token": "bad", "authorization_code": "c", "raw_nonce": "n"}),
		http.StatusUnauthorized, httpapi.CodeInvalidSession)
}

// §5.2: 사용자는 다른 기기 세션을 조회할 수 있다. 로그아웃한 기기는 빠진다.
func TestListDevicesShowsLiveDevicesAndMarksCurrent(t *testing.T) {
	s := newStack(t)
	sub := "http." + uuid.NewString()
	a := s.signIn(t, sub)
	b := s.signIn(t, sub)
	c := s.signIn(t, sub)

	if rec := s.do(t, http.MethodPost, "/v1/auth/logout", c.AccessToken, nil); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body.String())
	}

	rec := s.do(t, http.MethodGet, "/v1/devices", b.AccessToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[devicesResponse](t, rec)
	if len(got.Devices) != 2 {
		t.Fatalf("기기 %d개, want 2 (로그아웃한 기기 제외): %+v", len(got.Devices), got.Devices)
	}
	seenA := false
	for _, d := range got.Devices {
		seenA = seenA || d.ID == a.DeviceID
		if d.ID == c.DeviceID {
			t.Error("로그아웃한 기기가 목록에 있다")
		}
		if (d.ID == b.DeviceID) != d.Current {
			t.Errorf("기기 %s current=%v", d.ID, d.Current)
		}
	}
	if !seenA {
		t.Error("다른 기기 A가 목록에 없다")
	}
	if strings.Contains(rec.Body.String(), "push_token") {
		t.Error("응답에 push token 필드가 있다")
	}
}

// 인증 요청의 로그에 token이 남지 않는다(§11).
func TestLogsNeverContainTokens(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "http."+uuid.NewString())
	s.do(t, http.MethodGet, "/v1/me", sess.AccessToken, nil)
	s.do(t, http.MethodPost, "/v1/auth/refresh", "", map[string]string{"refresh_token": sess.RefreshToken})
	s.do(t, http.MethodPost, "/v1/auth/refresh", "", map[string]string{"refresh_token": sess.RefreshToken})

	logs := s.logs.String()
	for name, secret := range map[string]string{"access token": sess.AccessToken, "refresh token": sess.RefreshToken} {
		if strings.Contains(logs, secret) {
			t.Errorf("로그에 %s가 남았다", name)
		}
	}
}
