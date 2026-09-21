package appleid

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testClientID = "com.example.plantogether"

// fakeApple은 JWKS endpoint를 흉내 낸다. 키 교체와 장애를 시험할 수 있게
// 응답을 바꿀 수 있다.
type fakeApple struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PrivateKey
	down    bool
	fetches atomic.Int32
	server  *httptest.Server
}

func newFakeApple(t *testing.T) *fakeApple {
	t.Helper()
	f := &fakeApple{keys: map[string]*rsa.PrivateKey{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.fetches.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var set struct {
			Keys []map[string]string `json:"keys"`
		}
		for kid, k := range f.keys {
			set.Keys = append(set.Keys, map[string]string{
				"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeApple) addKey(t *testing.T, kid string) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.keys[kid] = k
	f.mu.Unlock()
	return k
}

func (f *fakeApple) setDown(down bool) {
	f.mu.Lock()
	f.down = down
	f.mu.Unlock()
}

// clock은 테스트가 시간을 옮길 수 있는 시계다.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func hashNonce(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// validClaims는 모든 검사를 통과하는 claim 집합이다. 각 테스트는 여기서 하나만
// 바꿔서 그 검사가 단독으로 거부하는지 본다.
func validClaims(now time.Time, rawNonce string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   Issuer,
		"aud":   testClientID,
		"sub":   "001234.abcdef.0001",
		"iat":   now.Unix(),
		"exp":   now.Add(10 * time.Minute).Unix(),
		"nonce": hashNonce(rawNonce),
	}
}

func sign(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestVerifier(t *testing.T, f *fakeApple, c *clock) *Verifier {
	t.Helper()
	v, err := NewVerifier(testClientID, f.server.URL, f.server.Client(), c.now)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyAcceptsValidToken(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	id, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw-nonce")), "raw-nonce")
	if err != nil {
		t.Fatalf("정상 token을 거부했다: %v", err)
	}
	if id.Subject != "001234.abcdef.0001" {
		t.Errorf("subject = %q", id.Subject)
	}
}

// 각 claim 검사가 단독으로 거부하는지 본다. 한 검사가 빠져도 다른 검사 덕에
// 초록불이 나는 일을 막으려고, 나머지는 모두 유효하게 두고 하나만 바꾼다.
func TestVerifyRejectsEachInvalidClaimIndependently(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	now := c.now()

	cases := []struct {
		name    string
		mutate  func(jwt.MapClaims)
		nonce   string
		wantErr error
	}{
		{"다른 issuer", func(m jwt.MapClaims) { m["iss"] = "https://evil.example.com" }, "raw", jwt.ErrTokenInvalidIssuer},
		{"다른 audience", func(m jwt.MapClaims) { m["aud"] = "com.other.app" }, "raw", jwt.ErrTokenInvalidAudience},
		{"만료", func(m jwt.MapClaims) { m["exp"] = now.Add(-time.Minute).Unix() }, "raw", jwt.ErrTokenExpired},
		{"exp 없음", func(m jwt.MapClaims) { delete(m, "exp") }, "raw", jwt.ErrTokenRequiredClaimMissing},
		{"미래의 iat", func(m jwt.MapClaims) { m["iat"] = now.Add(time.Hour).Unix() }, "raw", jwt.ErrTokenUsedBeforeIssued},
		{"nonce 불일치", func(m jwt.MapClaims) { m["nonce"] = hashNonce("other") }, "raw", ErrNonceMismatch},
		{"token에 nonce 없음", func(m jwt.MapClaims) { delete(m, "nonce") }, "raw", ErrNonceMismatch},
		{"원문 nonce를 해시 없이 실음", func(m jwt.MapClaims) { m["nonce"] = "raw" }, "raw", ErrNonceMismatch},
		{"클라이언트가 nonce를 안 보냄", func(m jwt.MapClaims) {}, "", ErrNonceMismatch},
		{"sub 없음", func(m jwt.MapClaims) { delete(m, "sub") }, "raw", ErrMissingSubject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newTestVerifier(t, f, c)
			claims := validClaims(now, "raw")
			tc.mutate(claims)
			_, err := v.Verify(context.Background(), sign(t, key, "k1", claims), tc.nonce)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// 다른 키로 서명한 token은 kid가 같아도 거부된다.
func TestVerifyRejectsWrongSignature(t *testing.T) {
	f := newFakeApple(t)
	f.addKey(t, "k1")
	attacker, _ := rsa.GenerateKey(rand.Reader, 2048)
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	_, err := v.Verify(context.Background(), sign(t, attacker, "k1", validClaims(c.now(), "raw")), "raw")
	if !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("err = %v, want 서명 오류", err)
	}
}

// 알고리즘 혼동 공격. RSA 공개 키를 HMAC 비밀로 쓴 HS256 token을 받아들이면
// 공개 키만으로 token을 위조할 수 있다.
//
// 방어는 두 겹이다. WithValidMethods가 RS256만 허용하고, golang-jwt의 HS256
// 검증기는 *rsa.PublicKey를 키로 받지 않는다. 한 겹을 빼도 이 테스트는 통과한다
// (2026-09-21 WithValidMethods 제거로 확인). 둘 다 유지한다.
func TestVerifyRejectsAlgorithmConfusion(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	pubDER := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims(c.now(), "raw"))
	tok.Header["kid"] = "k1"
	forged, err := tok.SignedString(pubDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), forged, "raw"); !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("HS256 token의 err = %v, want 서명 오류(허용되지 않은 알고리즘)", err)
	}
}

// Apple이 키를 교체하면 모르는 kid가 온다. 다시 받아서 검증에 성공해야 한다.
func TestVerifyRefetchesKeysOnRotation(t *testing.T) {
	f := newFakeApple(t)
	k1 := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	if _, err := v.Verify(context.Background(), sign(t, k1, "k1", validClaims(c.now(), "raw")), "raw"); err != nil {
		t.Fatal(err)
	}
	// 간격 제한을 넘긴 뒤 새 키가 추가된다.
	c.advance(2 * unknownKidRefetchInterval)
	k2 := f.addKey(t, "k2")

	if _, err := v.Verify(context.Background(), sign(t, k2, "k2", validClaims(c.now(), "raw")), "raw"); err != nil {
		t.Fatalf("키 교체 후 새 kid를 거부했다: %v", err)
	}
	if got := f.fetches.Load(); got != 2 {
		t.Errorf("JWKS 요청 %d회, want 2", got)
	}
}

// 임의 kid로 Apple endpoint를 두드리게 만들 수 없어야 한다.
func TestVerifyRateLimitsUnknownKidRefetch(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	if _, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw")), "raw"); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		kid := "bogus-" + string(rune('a'+i))
		_, err := v.Verify(context.Background(), sign(t, key, kid, validClaims(c.now(), "raw")), "raw")
		if !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("모르는 kid의 err = %v, want ErrUnknownKey", err)
		}
	}
	if got := f.fetches.Load(); got != 1 {
		t.Fatalf("모르는 kid 20건에 JWKS를 %d회 요청했다. 간격 제한 안에서는 추가 요청이 없어야 한다", got)
	}
}

// 키 집합이 최대 수명을 넘겼는데 다시 받기에 실패하면 낡은 키로 검증하지
// 않는다. 폐기된 키로 서명된 token을 무기한 받아들이는 경로를 막는다.
func TestVerifyFailsClosedWhenStaleKeysCannotBeRefreshed(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	if _, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw")), "raw"); err != nil {
		t.Fatal(err)
	}
	c.advance(keysMaxAge + time.Minute)
	f.setDown(true)

	_, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw")), "raw")
	if !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("낡은 키로 검증을 계속했다: err = %v, want ErrKeysUnavailable", err)
	}
}

func TestVerifyReportsUnavailableWhenAppleIsDown(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	f.setDown(true)
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	_, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw")), "raw")
	if !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("err = %v, want ErrKeysUnavailable", err)
	}
}

// ---- Client (code 교환, revoke) ----

func testCredentials(t *testing.T) (Credentials, *ecdsa.PrivateKey) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{TeamID: "TEAM123456", KeyID: "KEY1234567", ClientID: testClientID, PrivateKey: k}, k
}

func TestParsePrivateKeyReadsP8(t *testing.T) {
	_, k := testCredentials(t)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	got, err := ParsePrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("p8 키를 읽지 못했다: %v", err)
	}
	if !got.Equal(k) {
		t.Error("읽은 키가 원래 키와 다르다")
	}
	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Error("PEM이 아닌 입력을 받아들였다")
	}
}

// code 교환 요청이 Apple 계약대로 만들어지는지 본다. client secret은 Apple이
// 검증할 방식(ES256, 공개 키) 그대로 확인한다.
func TestExchangeCodeSendsAppleContract(t *testing.T) {
	creds, key := testCredentials(t)
	now := time.Now()

	var form map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("method=%s content-type=%s", r.Method, r.Header.Get("Content-Type"))
		}
		_ = r.ParseForm()
		form = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "a", "expires_in": 3600, "id_token": fakeIDToken(t, "apple-sub-1"),
			"refresh_token": "apple-refresh", "token_type": "Bearer",
		})
	}))
	defer srv.Close()

	c, err := NewClient(creds, srv.URL, srv.URL, srv.Client(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ex, err := c.ExchangeCode(context.Background(), "auth-code")
	if err != nil {
		t.Fatalf("교환 실패: %v", err)
	}
	if ex.RefreshToken != "apple-refresh" || ex.Subject != "apple-sub-1" {
		t.Errorf("교환 결과 = %+v", ex)
	}

	if got := form["grant_type"]; len(got) != 1 || got[0] != "authorization_code" {
		t.Errorf("grant_type = %v", got)
	}
	if form["code"][0] != "auth-code" || form["client_id"][0] != testClientID {
		t.Errorf("code/client_id = %v/%v", form["code"], form["client_id"])
	}
	if _, ok := form["redirect_uri"]; ok {
		t.Error("네이티브 로그인에 redirect_uri를 보냈다")
	}

	secret := form["client_secret"][0]
	parsed, err := jwt.Parse(secret, func(tok *jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"ES256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("client secret이 ES256으로 검증되지 않는다: %v", err)
	}
	if parsed.Header["kid"] != creds.KeyID {
		t.Errorf("kid = %v", parsed.Header["kid"])
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["iss"] != creds.TeamID || claims["sub"] != testClientID {
		t.Errorf("iss/sub = %v/%v", claims["iss"], claims["sub"])
	}
	// Apple 문서는 aud를 문자열로 정의한다. 배열이면 거부될 수 있다.
	if aud, ok := claims["aud"].(string); !ok || aud != Issuer {
		t.Errorf("aud = %#v, want 문자열 %q", claims["aud"], Issuer)
	}
	exp, _ := claims.GetExpirationTime()
	if exp == nil || exp.Sub(now) > 15777000*time.Second || exp.Before(now) {
		t.Errorf("exp = %v, 6개월 이내의 미래여야 한다", exp)
	}
}

func TestExchangeCodeClassifiesAppleErrors(t *testing.T) {
	creds, _ := testCredentials(t)
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string
		wantErr  error
	}{
		{"만료·재사용된 code", 400, `{"error":"invalid_grant"}`, "invalid_grant", nil},
		{"문서에 없는 error 값은 옮기지 않는다", 400, `{"error":"Bearer secret-looking-thing"}`, "unknown", nil},
		{"Apple 장애", 503, `oops`, "", ErrUnavailable},
		{"200인데 refresh token 없음", 200, `{"access_token":"a"}`, "", ErrUnavailable},
		{"200인데 id_token 없음", 200, `{"refresh_token":"r"}`, "", ErrUnavailable},
		{"200인데 id_token이 JWT가 아님", 200, `{"refresh_token":"r","id_token":"garbage"}`, "", ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, _ := NewClient(creds, srv.URL, srv.URL, srv.Client(), nil)

			_, err := c.ExchangeCode(context.Background(), "code")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != tc.wantCode {
				t.Fatalf("err = %v, want APIError(%s)", err, tc.wantCode)
			}
			if strings.Contains(err.Error(), "secret-looking-thing") {
				t.Error("Apple 응답 본문의 임의 문자열이 오류 메시지에 들어갔다")
			}
		})
	}
}

func TestRevokeSendsRefreshTokenHint(t *testing.T) {
	creds, _ := testCredentials(t)
	var form map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
	}))
	defer srv.Close()
	c, _ := NewClient(creds, srv.URL, srv.URL, srv.Client(), nil)

	if err := c.Revoke(context.Background(), "apple-refresh"); err != nil {
		t.Fatalf("revoke 실패: %v", err)
	}
	if form["token"][0] != "apple-refresh" || form["token_type_hint"][0] != "refresh_token" {
		t.Errorf("token/hint = %v/%v", form["token"], form["token_type_hint"])
	}
}

func TestNewClientRequiresAllCredentials(t *testing.T) {
	creds, _ := testCredentials(t)
	creds.KeyID = ""
	if _, err := NewClient(creds, "", "", nil, nil); err == nil {
		t.Fatal("key ID 없이 Client가 만들어졌다")
	}
}

// fakeIDToken은 교환 응답의 id_token을 흉내 낸다. 서버는 이 token의 서명을
// 검증하지 않으므로(TLS로 Apple에서 직접 받는다) 임의 키로 서명한다.
func fakeIDToken(t *testing.T, sub string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": sub, "iss": Issuer}).
		SignedString([]byte("irrelevant"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAPIErrorIsUserErrorOnlyForInvalidGrant(t *testing.T) {
	for code, want := range map[string]bool{
		"invalid_grant": true, "invalid_client": false, "unauthorized_client": false,
		"invalid_request": false, "unsupported_grant_type": false, "unknown": false,
	} {
		if got := (&APIError{Code: code}).IsUserError(); got != want {
			t.Errorf("%s: IsUserError = %v, want %v", code, got, want)
		}
	}
}

// 키가 최대 수명을 넘겼는데 Apple이 죽어 있으면, 모든 로그인이 락 뒤에서 각자
// Apple timeout을 기다리지 않고 즉시 실패해야 한다.
func TestVerifyDoesNotHammerAppleWhenStaleKeysCannotBeRefreshed(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	if _, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw")), "raw"); err != nil {
		t.Fatal(err)
	}
	c.advance(keysMaxAge + time.Minute)
	f.setDown(true)
	before := f.fetches.Load()

	for range 20 {
		_, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw")), "raw")
		if !errors.Is(err, ErrKeysUnavailable) {
			t.Fatalf("err = %v, want ErrKeysUnavailable", err)
		}
	}
	if got := f.fetches.Load() - before; got != 1 {
		t.Fatalf("장애 중 로그인 20건에 JWKS를 %d회 요청했다, want 1", got)
	}

	// 간격이 지나고 Apple이 돌아오면 회복한다.
	c.advance(2 * unknownKidRefetchInterval)
	f.setDown(false)
	if _, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw")), "raw"); err != nil {
		t.Fatalf("Apple 복구 후에도 실패한다: %v", err)
	}
}

// 클라이언트가 연결을 끊어 요청 context가 취소돼도 JWKS 받기는 끝까지 간다.
// 그러지 않으면 그 실패가 재시도 간격에 걸려 다른 사용자의 로그인까지 막힌다.
func TestVerifyFetchSurvivesCallerCancellation(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = v.Verify(ctx, sign(t, key, "k1", validClaims(c.now(), "raw")), "raw")

	if _, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims(c.now(), "raw")), "raw"); err != nil {
		t.Fatalf("취소된 요청 뒤의 정상 요청이 실패했다: %v", err)
	}
}

// 서버 시계가 Apple보다 조금 늦어도 방금 발급된 token을 받아들인다.
func TestVerifyToleratesSmallClockSkew(t *testing.T) {
	f := newFakeApple(t)
	key := f.addKey(t, "k1")
	c := &clock{t: time.Now()}
	v := newTestVerifier(t, f, c)

	claims := validClaims(c.now(), "raw")
	claims["iat"] = c.now().Add(10 * time.Second).Unix()
	if _, err := v.Verify(context.Background(), sign(t, key, "k1", claims), "raw"); err != nil {
		t.Fatalf("10초 앞선 iat를 거부했다: %v", err)
	}
}
