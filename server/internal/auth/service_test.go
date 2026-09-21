package auth

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/postgres/pgtest"
)

// 이 파일은 account_backend_design.md §14 "인증·세션" 인수 조건을 실제
// PostgreSQL에서 검증한다. api 런타임 역할로 접속한다(pgtest.APIRoleURLEnv).

// fakeApple은 identity token 문자열을 subject로 바꿔 주는 가짜 검증기다.
// "bad"로 시작하면 서명 오류, "down"이면 Apple 장애를 흉내 낸다.
type fakeApple struct{}

func (fakeApple) Verify(_ context.Context, idToken, rawNonce string) (appleid.Identity, error) {
	switch {
	case idToken == "down":
		return appleid.Identity{}, appleid.ErrKeysUnavailable
	case len(idToken) >= 3 && idToken[:3] == "bad":
		return appleid.Identity{}, errors.New("서명 오류")
	case rawNonce == "":
		return appleid.Identity{}, appleid.ErrNonceMismatch
	}
	return appleid.Identity{Subject: idToken}, nil
}

type fakeExchanger struct {
	err   error
	calls int
	mu    sync.Mutex
}

// ExchangeCode는 code를 그 code를 받은 Apple 사용자의 subject로 취급한다.
// 테스트는 보통 code에 identity token과 같은 subject를 넣고, 남의 code를 흉내 낼
// 때만 다른 값을 넣는다.
func (f *fakeExchanger) ExchangeCode(_ context.Context, code string) (appleid.Exchange, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return appleid.Exchange{}, f.err
	}
	return appleid.Exchange{RefreshToken: "apple-refresh-for-" + code, Subject: code}, nil
}

// fakeSealer는 평문이 그대로 저장되지 않았는지 확인할 수 있게 뒤집어 저장한다.
type fakeSealer struct{}

func (fakeSealer) Seal(p []byte, purpose string) ([]byte, error) {
	out := make([]byte, 0, len(purpose)+len(p)+1)
	out = append(out, purpose...)
	out = append(out, '|')
	for i := len(p) - 1; i >= 0; i-- {
		out = append(out, p[i])
	}
	return out, nil
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	svc   *Service
	pool  *pgxpool.Pool
	clock *testClock
	ex    *fakeExchanger
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	c := &testClock{t: time.Now()}
	tokens, err := NewAccessTokens(bytes.Repeat([]byte{1}, 32), c.now)
	if err != nil {
		t.Fatal(err)
	}
	ex := &fakeExchanger{}
	svc, err := NewService(Deps{Pool: pool, Apple: fakeApple{}, CodeExchanger: ex, Sealer: fakeSealer{}, Tokens: tokens, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, pool: pool, clock: c, ex: ex}
}

// subject는 테스트마다 겹치지 않는 Apple subject를 만들고, 끝나면 그 subject로
// 만들어진 사용자를 지운다. 서비스가 자체 트랜잭션을 커밋하므로 rollback으로
// 격리할 수 없다.
func (e *env) subject(t *testing.T) string {
	t.Helper()
	s := "test." + uuid.NewString()
	t.Cleanup(func() { e.deleteUsersBySubject(t, s) })
	return s
}

func (e *env) deleteUsersBySubject(t *testing.T, subject string) {
	ctx := context.Background()
	_, _ = e.pool.Exec(ctx, `
		DELETE FROM audit_events WHERE actor_user_id IN (
			SELECT user_id FROM auth_identities WHERE provider_subject = $1)`, subject)
	_, _ = e.pool.Exec(ctx, `
		DELETE FROM users WHERE id IN (
			SELECT user_id FROM auth_identities WHERE provider_subject = $1)`, subject)
}

func (e *env) trackUser(t *testing.T, id uuid.UUID) {
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.pool.Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id = $1`, id)
		_, _ = e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	})
}

func (e *env) signIn(t *testing.T, subject string) Session {
	t.Helper()
	s, err := e.svc.SignInWithApple(context.Background(), SignInInput{
		IdentityToken: subject, AuthorizationCode: subject, RawNonce: "nonce", DisplayName: "홍길동",
	})
	if err != nil {
		t.Fatalf("로그인 실패: %v", err)
	}
	return s
}

func (e *env) setStatus(t *testing.T, userID uuid.UUID, status string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE users SET status = $2 WHERE id = $1`, userID, status); err != nil {
		t.Fatal(err)
	}
}

// §14: 같은 Apple subject로 다시 로그인하면 기존 user_id를 반환하고 중복
// 사용자를 만들지 않는다.
func TestSignInSameSubjectReturnsSameUser(t *testing.T) {
	e := newEnv(t)
	sub := e.subject(t)

	first := e.signIn(t, sub)
	second := e.signIn(t, sub)

	if !first.NewUser || second.NewUser {
		t.Errorf("NewUser = %v, %v, want true, false", first.NewUser, second.NewUser)
	}
	if first.UserID != second.UserID {
		t.Fatalf("같은 subject에 다른 user_id: %s, %s", first.UserID, second.UserID)
	}
	if first.DeviceID == second.DeviceID {
		t.Error("로그인마다 새 device ID를 발급해야 한다")
	}
	var n int
	_ = e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM auth_identities WHERE provider_subject = $1`, sub).Scan(&n)
	if n != 1 {
		t.Errorf("identity row %d개, want 1", n)
	}
}

// 최초 로그인이 동시에 여러 번 와도 계정은 하나다.
func TestSignInConcurrentFirstLoginCreatesOneUser(t *testing.T) {
	e := newEnv(t)
	sub := e.subject(t)

	const n = 8
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := e.svc.SignInWithApple(context.Background(), SignInInput{
				IdentityToken: sub, AuthorizationCode: sub, RawNonce: "n"})
			ids[i], errs[i] = s.UserID, err
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("동시 로그인 %d 실패: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("동시 로그인이 서로 다른 계정을 만들었다: %s, %s", ids[0], ids[i])
		}
	}
	var users int
	_ = e.pool.QueryRow(context.Background(), `
		SELECT count(DISTINCT u.id) FROM users u
		  JOIN auth_identities i ON i.user_id = u.id
		 WHERE i.provider_subject = $1`, sub).Scan(&users)
	if users != 1 {
		t.Errorf("사용자 %d명, want 1", users)
	}
}

// §5.3·§14: active가 아닌 계정은 로그인할 수 없다.
func TestSignInRejectedForLockedAccounts(t *testing.T) {
	for _, status := range []string{"disabled", "deletion_requested", "deleting"} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			sub := e.subject(t)
			s := e.signIn(t, sub)
			e.setStatus(t, s.UserID, status)

			_, err := e.svc.SignInWithApple(context.Background(), SignInInput{
				IdentityToken: sub, AuthorizationCode: sub, RawNonce: "n"})
			if !errors.Is(err, ErrAccountLocked) {
				t.Fatalf("err = %v, want ErrAccountLocked", err)
			}
		})
	}
}

// §14: 삭제가 deleted까지 끝난 뒤 같은 subject로 로그인하면 새 user_id가
// 발급된다. §10 7단계가 identity row를 물리 삭제하는 것을 흉내 낸다.
func TestSignInAfterDeletionCreatesNewUser(t *testing.T) {
	e := newEnv(t)
	sub := e.subject(t)
	old := e.signIn(t, sub)
	e.trackUser(t, old.UserID)

	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `DELETE FROM auth_identities WHERE user_id = $1`, old.UserID); err != nil {
		t.Fatal(err)
	}
	e.setStatus(t, old.UserID, "deleted")

	fresh := e.signIn(t, sub)
	if !fresh.NewUser || fresh.UserID == old.UserID {
		t.Fatalf("삭제 후 재가입이 새 계정을 만들지 않았다: new=%v old=%s fresh=%s",
			fresh.NewUser, old.UserID, fresh.UserID)
	}
}

func TestSignInRejectsInvalidAppleCredential(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.SignInWithApple(context.Background(), SignInInput{
		IdentityToken: "bad-token", AuthorizationCode: "c", RawNonce: "n"})
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
	if e.ex.calls != 0 {
		t.Error("identity token 검증 실패 뒤에 Apple code 교환을 호출했다")
	}
}

// Apple 장애는 사용자 자격 문제가 아니다. 401로 답하면 클라이언트가 사용자를
// 로그아웃시킨다.
func TestSignInReportsAppleOutageAsUnavailable(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.SignInWithApple(context.Background(), SignInInput{
		IdentityToken: "down", AuthorizationCode: "c", RawNonce: "n"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}

	e.ex.err = appleid.ErrUnavailable
	sub := e.subject(t)
	_, err = e.svc.SignInWithApple(context.Background(), SignInInput{
		IdentityToken: sub, AuthorizationCode: sub, RawNonce: "n"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("code 교환 장애 err = %v, want ErrUnavailable", err)
	}
}

// code 교환이 거부되면 계정을 만들지 않는다. 만들면 §10 3단계에서 revoke할
// token이 없는 계정이 남는다.
func TestSignInDoesNotCreateUserWhenCodeExchangeRejected(t *testing.T) {
	e := newEnv(t)
	sub := e.subject(t)
	e.ex.err = &appleid.APIError{Status: 400, Code: "invalid_grant"}

	_, err := e.svc.SignInWithApple(context.Background(), SignInInput{
		IdentityToken: sub, AuthorizationCode: sub, RawNonce: "n"})
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM auth_identities WHERE provider_subject = $1`, sub).Scan(&n)
	if n != 0 {
		t.Fatalf("code 교환 실패인데 identity %d개가 만들어졌다", n)
	}
}

// Apple refresh token은 암호화해서만 저장한다(§11).
func TestSignInStoresOnlySealedAppleToken(t *testing.T) {
	e := newEnv(t)
	sub := e.subject(t)
	e.signIn(t, sub)

	var stored []byte
	_ = e.pool.QueryRow(context.Background(), `
		SELECT provider_refresh_token_ciphertext FROM auth_identities
		 WHERE provider_subject = $1`, sub).Scan(&stored)
	if len(stored) == 0 {
		t.Fatal("Apple refresh token 암호문이 저장되지 않았다")
	}
	if bytes.Contains(stored, []byte("apple-refresh-for-"+sub)) {
		t.Fatal("Apple refresh token이 평문으로 저장됐다")
	}
	if !bytes.HasPrefix(stored, []byte(SealPurposeAppleRefreshToken+"|")) {
		t.Errorf("봉인 purpose가 %q가 아니다", SealPurposeAppleRefreshToken)
	}
}

// DB에는 refresh token의 해시만 있다(§5.2, §11).
func TestRefreshTokenIsStoredOnlyAsHash(t *testing.T) {
	e := newEnv(t)
	s := e.signIn(t, e.subject(t))

	var hash []byte
	_ = e.pool.QueryRow(context.Background(),
		`SELECT refresh_token_hash FROM sessions WHERE device_id = $1`, s.DeviceID).Scan(&hash)
	if bytes.Contains(hash, []byte(s.RefreshToken)) || !bytes.Equal(hash, hashRefreshToken(s.RefreshToken)) {
		t.Fatal("refresh token이 해시가 아닌 형태로 저장됐다")
	}
}

func TestRefreshRotatesTokens(t *testing.T) {
	e := newEnv(t)
	s := e.signIn(t, e.subject(t))

	r, err := e.svc.Refresh(context.Background(), s.RefreshToken, uuid.Nil)
	if err != nil {
		t.Fatalf("refresh 실패: %v", err)
	}
	if r.RefreshToken == s.RefreshToken {
		t.Fatal("refresh token이 회전하지 않았다")
	}
	if r.UserID != s.UserID || r.DeviceID != s.DeviceID {
		t.Error("refresh가 다른 사용자·기기의 세션을 돌려줬다")
	}
	if _, err := e.svc.Authenticate(context.Background(), r.AccessToken); err != nil {
		t.Errorf("회전 후 access token이 인증되지 않는다: %v", err)
	}
	if _, err := e.svc.Refresh(context.Background(), r.RefreshToken, uuid.Nil); err != nil {
		t.Errorf("회전된 새 token으로 다시 refresh할 수 없다: %v", err)
	}
}

// §14: 사용된 refresh token이 재사용되면 해당 device token family가 전부
// 폐기된다.
func TestRefreshReuseRevokesWholeFamily(t *testing.T) {
	e := newEnv(t)
	s := e.signIn(t, e.subject(t))
	ctx := context.Background()

	rotated, err := e.svc.Refresh(ctx, s.RefreshToken, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	// 탈취자가 이미 쓰인 원래 token을 제시한다.
	if _, err := e.svc.Refresh(ctx, s.RefreshToken, uuid.Nil); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("재사용 err = %v, want ErrInvalidCredential", err)
	}
	// 정상 사용자가 가진 최신 token도 이제 쓸 수 없다.
	if _, err := e.svc.Refresh(ctx, rotated.RefreshToken, uuid.Nil); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("재사용 탐지 후 같은 family의 최신 token이 살아 있다: err = %v", err)
	}
	// access token도 즉시 막힌다(기기 폐기).
	if _, err := e.svc.Authenticate(ctx, rotated.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("재사용 탐지 후 access token이 살아 있다: err = %v", err)
	}

	var live int
	_ = e.pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE device_id = $1 AND revoked_at IS NULL`, s.DeviceID).Scan(&live)
	if live != 0 {
		t.Errorf("폐기되지 않은 세션 %d개", live)
	}
	var audited int
	_ = e.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		 WHERE actor_user_id = $1 AND action = 'auth.refresh_reuse_detected'`, s.UserID).Scan(&audited)
	if audited != 1 {
		t.Errorf("재사용 탐지 audit %d건, want 1", audited)
	}
}

// 같은 token으로 동시에 refresh하면 많아야 하나만 성공한다. 둘 다 성공하면
// 한 token에서 두 갈래 세션이 생긴다.
func TestRefreshConcurrentUseYieldsAtMostOneSuccess(t *testing.T) {
	e := newEnv(t)
	s := e.signIn(t, e.subject(t))

	const n = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.Refresh(context.Background(), s.RefreshToken, uuid.Nil); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// 정확히 하나다. 0이면 정상 회전까지 막힌 것이고, 2 이상이면 한 token에서
	// 두 갈래 세션이 생긴 것이다.
	if ok != 1 {
		t.Fatalf("같은 refresh token으로 %d번 성공했다, want 1", ok)
	}
}

func TestRefreshRejectsExpiredToken(t *testing.T) {
	e := newEnv(t)
	s := e.signIn(t, e.subject(t))
	e.clock.advance(RefreshTokenTTL + time.Minute)

	if _, err := e.svc.Refresh(context.Background(), s.RefreshToken, uuid.Nil); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("만료된 refresh token err = %v, want ErrInvalidCredential", err)
	}
}

func TestRefreshRejectsMalformedAndUnknownTokens(t *testing.T) {
	e := newEnv(t)
	for _, tok := range []string{"", "garbage", "rt_short", refreshTokenPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := e.svc.Refresh(context.Background(), tok, uuid.Nil); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("%q: err = %v, want ErrInvalidCredential", tok, err)
		}
	}
}

// §14: device A 로그아웃 후 A의 refresh는 실패하지만 device B 세션은 유지된다.
func TestLogoutRevokesOnlyCurrentDevice(t *testing.T) {
	e := newEnv(t)
	sub := e.subject(t)
	a := e.signIn(t, sub)
	b := e.signIn(t, sub)
	ctx := context.Background()

	if err := e.svc.Logout(ctx, Principal{UserID: a.UserID, DeviceID: a.DeviceID}, uuid.Nil); err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.Refresh(ctx, a.RefreshToken, uuid.Nil); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("로그아웃한 기기 A의 refresh err = %v, want ErrInvalidCredential", err)
	}
	if _, err := e.svc.Authenticate(ctx, a.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("로그아웃한 기기 A의 access token err = %v, want ErrInvalidCredential", err)
	}
	if _, err := e.svc.Authenticate(ctx, b.AccessToken); err != nil {
		t.Errorf("기기 B의 access token이 막혔다: %v", err)
	}
	if _, err := e.svc.Refresh(ctx, b.RefreshToken, uuid.Nil); err != nil {
		t.Errorf("기기 B의 refresh가 막혔다: %v", err)
	}

	// 이미 로그아웃한 기기에 다시 호출해도 성공한다.
	if err := e.svc.Logout(ctx, Principal{UserID: a.UserID, DeviceID: a.DeviceID}, uuid.Nil); err != nil {
		t.Errorf("두 번째 로그아웃 실패: %v", err)
	}
}

// §14: disabled, deletion_requested, deleting, deleted 계정은 보호 API를
// 사용할 수 없다. 발급된 access token이 아직 유효해도 막는다.
func TestAuthenticateRejectsLockedAccounts(t *testing.T) {
	for _, status := range []string{"disabled", "deletion_requested", "deleting", "deleted"} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			s := e.signIn(t, e.subject(t))
			e.setStatus(t, s.UserID, status)

			if _, err := e.svc.Authenticate(context.Background(), s.AccessToken); !errors.Is(err, ErrAccountLocked) {
				t.Fatalf("err = %v, want ErrAccountLocked", err)
			}
			if _, err := e.svc.Refresh(context.Background(), s.RefreshToken, uuid.Nil); !errors.Is(err, ErrAccountLocked) {
				t.Fatalf("refresh err = %v, want ErrAccountLocked", err)
			}
		})
	}
}

func TestAuthenticateRejectsExpiredAndForeignTokens(t *testing.T) {
	e := newEnv(t)
	s := e.signIn(t, e.subject(t))
	ctx := context.Background()

	// 다른 키로 서명한 token.
	other, _ := NewAccessTokens(bytes.Repeat([]byte{2}, 32), e.clock.now)
	forged, _, _ := other.Issue(Principal{UserID: s.UserID, DeviceID: s.DeviceID})
	if _, err := e.svc.Authenticate(ctx, forged); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("다른 키로 서명한 token err = %v", err)
	}

	// 다른 사용자의 기기 ID를 담은 token. 서명은 맞다.
	mixed, _, _ := e.svc.tokens.Issue(Principal{UserID: uuid.New(), DeviceID: s.DeviceID})
	if _, err := e.svc.Authenticate(ctx, mixed); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("사용자와 기기가 어긋난 token err = %v", err)
	}

	e.clock.advance(AccessTokenTTL + time.Second)
	if _, err := e.svc.Authenticate(ctx, s.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("만료된 access token err = %v", err)
	}
}

// 남의 authorization code를 자기 identity token과 함께 내면 거부한다. 받아들이면
// 남의 Apple grant가 내 계정에 저장되고 §10 삭제 때 엉뚱한 grant를 revoke한다.
func TestSignInRejectsCodeOfAnotherAppleUser(t *testing.T) {
	e := newEnv(t)
	mine := e.subject(t)
	_, err := e.svc.SignInWithApple(context.Background(), SignInInput{
		IdentityToken: mine, AuthorizationCode: "someone-else", RawNonce: "n"})
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM auth_identities WHERE provider_subject = $1`, mine).Scan(&n)
	if n != 0 {
		t.Fatalf("subject가 다른 code로 계정 %d개가 만들어졌다", n)
	}
}

// invalid_client 같은 Apple 오류는 서버 자격 문제다. 사용자 자격 오류(401)로
// 바꾸면 모든 로그인이 "다시 로그인하라"로 보이고 운영자는 원인을 모른다.
func TestSignInDistinguishesServerSideAppleErrors(t *testing.T) {
	for code, wantUserError := range map[string]bool{
		"invalid_grant": true, "invalid_client": false, "unauthorized_client": false, "unsupported_grant_type": false,
	} {
		t.Run(code, func(t *testing.T) {
			e := newEnv(t)
			sub := e.subject(t)
			e.ex.err = &appleid.APIError{Status: 400, Code: code}
			_, err := e.svc.SignInWithApple(context.Background(), SignInInput{
				IdentityToken: sub, AuthorizationCode: sub, RawNonce: "n"})
			if got := errors.Is(err, ErrInvalidCredential); got != wantUserError {
				t.Fatalf("err = %v, 사용자 오류로 취급 = %v, want %v", err, got, wantUserError)
			}
			var apiErr *appleid.APIError
			if !wantUserError && (!errors.As(err, &apiErr) || apiErr.Code != code) {
				t.Errorf("서버 쪽 오류가 Apple code를 잃었다: %v", err)
			}
		})
	}
}

// 로그아웃과 refresh가 같은 기기에서 동시에 일어나도 교착(40P01)으로 500이
// 나지 않는다. 두 경로 모두 users → devices → sessions 순으로 잠근다.
func TestConcurrentRefreshAndLogoutDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	sub := e.subject(t)
	for range 10 {
		s := e.signIn(t, sub)
		r, err := e.svc.Refresh(context.Background(), s.RefreshToken, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 3)
		wg.Add(3)
		go func() { defer wg.Done(); _, errs[0] = e.svc.Refresh(context.Background(), s.RefreshToken, uuid.Nil) }()
		go func() { defer wg.Done(); _, errs[1] = e.svc.Refresh(context.Background(), r.RefreshToken, uuid.Nil) }()
		go func() {
			defer wg.Done()
			errs[2] = e.svc.Logout(context.Background(), Principal{UserID: s.UserID, DeviceID: s.DeviceID}, uuid.Nil)
		}()
		wg.Wait()
		for i, err := range errs {
			if err != nil && !errors.Is(err, ErrInvalidCredential) {
				t.Fatalf("동시 실행 %d에서 예상 밖 오류: %v", i, err)
			}
		}
	}
}
