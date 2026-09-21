// Package appleid는 Sign in with Apple의 서버 쪽 경계다.
//
// account_backend_design.md §5.1의 세 가지 Apple 상호작용을 담는다.
//   - identity token 검증 (Verifier): 서명, iss, aud, exp, nonce
//   - authorization code 교환 (Client.ExchangeCode): Apple refresh token 획득
//   - token 폐기 (Client.Revoke): 계정 삭제 §10 3단계
//
// Apple 계약은 2026-09-21에 공식 문서로 다시 확인했다. 근거와 URL은
// docs/rec의 P1 인증 기록에 있다.
//   - JWKS: GET https://appleid.apple.com/auth/keys (RSA, RS256)
//   - iss: https://appleid.apple.com, aud: 앱의 client_id(bundle ID)
//   - authorization code는 1회용이고 5분간 유효하다
//   - Apple은 JWKS 캐시 TTL이나 키 교체 주기를 문서화하지 않는다
package appleid

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer는 Apple identity token의 iss 값이다.
const Issuer = "https://appleid.apple.com"

// DefaultKeysURL은 Apple 공개 키 JWKS endpoint다.
const DefaultKeysURL = "https://appleid.apple.com/auth/keys"

// Identity는 검증을 통과한 identity token에서 서버가 쓰는 값이다.
//
// email은 담지 않는다. §5.1은 이메일을 로그인 키로 쓰지 않고, 현재 스키마에는
// 이메일을 보관할 열도 없다. 보관하지 않을 값은 꺼내지도 않는다(§4 데이터 최소화).
type Identity struct {
	// Subject는 Apple의 안정적 사용자 식별자다. 같은 팀의 앱 사이에서 같고
	// 바뀌지 않는다. auth_identities.provider_subject에 들어간다.
	Subject string
}

var (
	// ErrNonceMismatch는 token의 nonce가 클라이언트가 보낸 raw nonce의 해시와
	// 다를 때다. 다른 로그인 시도에서 가로챈 token의 재사용을 막는다.
	ErrNonceMismatch = errors.New("appleid: nonce가 일치하지 않는다")

	// ErrMissingSubject는 서명은 맞지만 sub가 비어 있을 때다.
	ErrMissingSubject = errors.New("appleid: sub가 없다")

	// ErrUnknownKey는 token의 kid가 Apple JWKS에 없을 때다.
	ErrUnknownKey = errors.New("appleid: 알 수 없는 서명 키")

	// ErrKeysUnavailable은 공개 키를 가져올 수 없어 검증을 할 수 없을 때다.
	// 검증 실패가 아니라 의존성 장애이므로 호출자는 401이 아니라 503으로 답한다.
	ErrKeysUnavailable = errors.New("appleid: Apple 공개 키를 가져올 수 없다")
)

// 키 캐시 정책. Apple이 TTL을 문서화하지 않으므로 두 규칙으로 정한다.
//
//   - keysMaxAge: 이 시간이 지난 키 집합은 쓰기 전에 다시 받는다. 다시 받기에
//     실패하면 낡은 키로 계속 검증하지 않고 실패한다. 낡은 키를 무기한 쓰면
//     Apple이 폐기한 키로 서명된 token을 계속 받아들이게 된다.
//   - unknownKidRefetchInterval: 모르는 kid가 오면 키 교체로 보고 다시 받되,
//     이 간격 안에서는 한 번만 받는다. 임의 kid를 담은 token을 반복해 보내
//     Apple endpoint를 두드리게 만드는 경로를 막는다.
const (
	keysMaxAge                = 24 * time.Hour
	unknownKidRefetchInterval = time.Minute
)

// Verifier는 Apple identity token을 검증한다. 동시에 여러 요청이 써도 안전하다.
type Verifier struct {
	clientID   string
	keysURL    string
	httpClient *http.Client
	now        func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	// lastAttempt는 성공 여부와 상관없이 마지막으로 JWKS를 요청한 시각이다.
	// 실패한 요청도 간격 제한에 넣어야 장애 중 Apple을 두드리지 않는다.
	lastAttempt time.Time
}

// NewVerifier는 Verifier를 만든다. clientID는 identity token의 aud와 비교할
// 앱 bundle ID다.
func NewVerifier(clientID, keysURL string, httpClient *http.Client, now func() time.Time) (*Verifier, error) {
	if clientID == "" {
		return nil, errors.New("appleid: clientID가 비어 있다")
	}
	if keysURL == "" {
		keysURL = DefaultKeysURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{clientID: clientID, keysURL: keysURL, httpClient: httpClient, now: now}, nil
}

// Verify는 identity token을 검증하고 Identity를 돌려준다.
//
// rawNonce는 iOS가 로그인 요청 전에 만든 원문 nonce다. iOS는 그 SHA-256 hex를
// ASAuthorizationAppleIDRequest.nonce로 Apple에 보내고, Apple은 그 값을 token의
// nonce claim에 그대로 싣는다. 서버는 원문을 받아 해시해서 비교한다. 해시만
// token에 실리므로 token을 가로챈 쪽은 원문을 모른다.
//
// iss, aud, exp, 서명, nonce를 각각 따로 판정한다. 오류는 원인별로 구분되지만
// HTTP 응답으로는 구분하지 않는다(호출자가 전부 invalid_session으로 답한다).
func (v *Verifier) Verify(ctx context.Context, idToken, rawNonce string) (Identity, error) {
	if rawNonce == "" {
		// nonce 없는 로그인은 받지 않는다. nonce를 선택으로 두면 가로챈 token의
		// 재사용을 막는 장치가 조용히 꺼진다.
		return Identity{}, ErrNonceMismatch
	}

	var claims appleClaims
	_, err := jwt.ParseWithClaims(idToken, &claims,
		func(t *jwt.Token) (any, error) { return v.keyFor(ctx, t) },
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(v.clientID),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return Identity{}, err
	}

	if claims.Subject == "" {
		return Identity{}, ErrMissingSubject
	}
	sum := sha256.Sum256([]byte(rawNonce))
	if claims.Nonce == "" || claims.Nonce != hex.EncodeToString(sum[:]) {
		return Identity{}, ErrNonceMismatch
	}
	return Identity{Subject: claims.Subject}, nil
}

// appleClaims는 검증에 필요한 claim만 받는다.
//
// email_verified, is_private_email은 Apple이 문자열("true")과 bool을 모두 쓸 수
// 있다고 문서화했다. 쓰지 않는 claim이므로 필드로 두지 않는다. 두면 타입이
// 달라지는 순간 파싱이 실패해 로그인 전체가 막힌다.
type appleClaims struct {
	jwt.RegisteredClaims
	Nonce string `json:"nonce"`
}

func (v *Verifier) keyFor(ctx context.Context, t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return nil, ErrUnknownKey
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.now()
	if v.keys == nil || now.Sub(v.fetchedAt) > keysMaxAge {
		if err := v.refreshLocked(ctx, now); err != nil {
			return nil, err
		}
	}
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}

	// 모르는 kid다. Apple이 키를 교체했을 수 있으므로 간격 제한 안에서 한 번
	// 다시 받는다.
	if now.Sub(v.lastAttempt) < unknownKidRefetchInterval {
		return nil, ErrUnknownKey
	}
	if err := v.refreshLocked(ctx, now); err != nil {
		return nil, err
	}
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	return nil, ErrUnknownKey
}

// refreshLocked는 JWKS를 받아 캐시를 교체한다. v.mu를 쥔 채로 호출한다.
//
// 락을 쥔 채 네트워크를 기다리므로 동시 요청이 줄을 선다. 의도한 것이다.
// 캐시가 비었을 때 동시 요청마다 따로 Apple을 부르는 것보다 낫고, 대기는
// httpClient timeout으로 제한된다.
func (v *Verifier) refreshLocked(ctx context.Context, now time.Time) error {
	v.lastAttempt = now

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.keysURL, nil)
	if err != nil {
		return ErrKeysUnavailable
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ErrKeysUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ErrKeysUnavailable
	}

	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return ErrKeysUnavailable
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := rsaPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		// 빈 키 집합으로 캐시를 덮으면 이후 모든 로그인이 ErrUnknownKey가 된다.
		// 받기 실패로 취급하고 기존 캐시를 유지한다.
		return ErrKeysUnavailable
	}
	v.keys = keys
	v.fetchedAt = now
	return nil
}

func rsaPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
		return nil, fmt.Errorf("appleid: RSA 지수가 범위를 벗어났다")
	}
	n := new(big.Int).SetBytes(nBytes)
	if n.BitLen() < 2048 {
		return nil, fmt.Errorf("appleid: RSA 키가 2048비트보다 짧다")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}
