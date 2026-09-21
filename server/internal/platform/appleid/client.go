package appleid

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Apple REST endpoint.
const (
	DefaultTokenURL  = "https://appleid.apple.com/auth/token"
	DefaultRevokeURL = "https://appleid.apple.com/auth/revoke"
)

// clientSecretTTL은 요청마다 새로 만드는 client secret의 수명이다. Apple은
// 최대 6개월(15777000초)을 허용하지만 길게 둘 이유가 없다. 요청마다 서명하므로
// 유출돼도 몇 분 뒤에는 쓸 수 없다.
const clientSecretTTL = 5 * time.Minute

// Credentials는 client secret 서명에 필요한 Apple 자격이다. §5.1에 따라
// private key는 secret manager에서 주입하고 저장소와 로그에 남기지 않는다.
type Credentials struct {
	TeamID     string
	KeyID      string
	ClientID   string
	PrivateKey *ecdsa.PrivateKey
}

// ParsePrivateKey는 Apple이 발급한 .p8(PKCS#8 PEM) ECDSA 키를 읽는다.
// 오류에 키 내용을 넣지 않는다.
func ParsePrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("appleid: private key가 PEM 형식이 아니다")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("appleid: private key를 PKCS#8로 해석할 수 없다")
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("appleid: private key가 ECDSA 키가 아니다")
	}
	return ec, nil
}

// Client는 Apple token endpoint를 호출한다.
type Client struct {
	creds      Credentials
	tokenURL   string
	revokeURL  string
	httpClient *http.Client
	now        func() time.Time
}

// NewClient는 Client를 만든다. URL이 비어 있으면 Apple 운영 endpoint를 쓴다.
func NewClient(creds Credentials, tokenURL, revokeURL string, httpClient *http.Client, now func() time.Time) (*Client, error) {
	if creds.TeamID == "" || creds.KeyID == "" || creds.ClientID == "" || creds.PrivateKey == nil {
		return nil, errors.New("appleid: Apple 자격(team ID, key ID, client ID, private key)이 모두 필요하다")
	}
	if tokenURL == "" {
		tokenURL = DefaultTokenURL
	}
	if revokeURL == "" {
		revokeURL = DefaultRevokeURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &Client{creds: creds, tokenURL: tokenURL, revokeURL: revokeURL, httpClient: httpClient, now: now}, nil
}

// APIError는 Apple이 400으로 돌려준 오류다. Code는 Apple 문서의 고정 값 중
// 하나이므로 로그에 남겨도 된다. 그 밖의 응답 본문은 담지 않는다.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("appleid: Apple이 요청을 거부했다 (HTTP %d, %s)", e.Status, e.Code)
}

// knownErrorCodes는 Apple ErrorResponse의 문서화된 error 값이다. 목록 밖의 값은
// 그대로 옮기지 않고 unknown으로 바꾼다. 응답 본문의 임의 문자열이 로그로 새지
// 않게 한다.
var knownErrorCodes = map[string]bool{
	"invalid_request": true, "invalid_client": true, "invalid_grant": true,
	"unauthorized_client": true, "unsupported_grant_type": true, "invalid_scope": true,
}

// IsUserError는 Apple 오류가 사용자가 제출한 code 때문인지다. invalid_grant만
// 그렇다(만료, 재사용, 다른 앱의 code). 나머지(invalid_client, unauthorized_client
// 등)는 서버 자격이나 설정 문제이므로 사용자에게 "다시 로그인하라"고 답하면 안 된다.
func (e *APIError) IsUserError() bool { return e.Code == "invalid_grant" }

// ErrUnavailable은 네트워크 오류나 5xx처럼 Apple 쪽 장애로 결과를 모를 때다.
var ErrUnavailable = errors.New("appleid: Apple token endpoint를 사용할 수 없다")

// Exchange는 code 교환 결과다.
type Exchange struct {
	RefreshToken string
	// Subject는 교환 응답의 id_token에 담긴 sub다. 호출자는 이 값이 검증한
	// identity token의 sub와 같은지 확인해야 한다. 같지 않으면 남의 code를
	// 자기 identity token과 함께 제출한 것이다.
	Subject string
}

// ExchangeCode는 iOS가 받은 authorization code를 Apple refresh token으로
// 교환한다. code는 1회용이고 5분간 유효하므로 로그인 요청 안에서 즉시 호출한다.
//
// 네이티브 iOS 로그인은 redirect_uri를 쓰지 않으므로 보내지 않는다.
//
// 응답의 id_token은 서명을 다시 검증하지 않고 sub만 읽는다. 이 token은 클라이언트가
// 아니라 Apple token endpoint가 TLS로 직접 준 것이므로 출처가 이미 보장된다.
func (c *Client) ExchangeCode(ctx context.Context, code string) (Exchange, error) {
	if code == "" {
		return Exchange{}, &APIError{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	secret, err := c.clientSecret()
	if err != nil {
		return Exchange{}, err
	}
	form := url.Values{
		"client_id":     {c.creds.ClientID},
		"client_secret": {secret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
	}

	body, err := c.post(ctx, c.tokenURL, form)
	if err != nil {
		return Exchange{}, err
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.RefreshToken == "" || tok.IDToken == "" {
		return Exchange{}, ErrUnavailable
	}
	var claims jwt.RegisteredClaims
	if _, _, err := jwt.NewParser().ParseUnverified(tok.IDToken, &claims); err != nil || claims.Subject == "" {
		return Exchange{}, ErrUnavailable
	}
	return Exchange{RefreshToken: tok.RefreshToken, Subject: claims.Subject}, nil
}

// Revoke는 Apple refresh token을 폐기한다. §10 3단계에서 삭제 worker가 부른다.
func (c *Client) Revoke(ctx context.Context, refreshToken string) error {
	secret, err := c.clientSecret()
	if err != nil {
		return err
	}
	form := url.Values{
		"client_id":       {c.creds.ClientID},
		"client_secret":   {secret},
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
	}
	_, err = c.post(ctx, c.revokeURL, form)
	return err
}

func (c *Client) post(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, ErrUnavailable
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		return body, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		code := e.Error
		if !knownErrorCodes[code] {
			code = "unknown"
		}
		return nil, &APIError{Status: resp.StatusCode, Code: code}
	default:
		return nil, ErrUnavailable
	}
}

// clientSecret은 Apple이 요구하는 ES256 JWT를 만든다.
// header: alg=ES256, kid=key ID. claim: iss=team ID, iat, exp,
// aud=https://appleid.apple.com, sub=client ID.
func (c *Client) clientSecret() (string, error) {
	now := c.now()
	// RegisteredClaims를 쓰지 않는다. jwt v5는 aud 하나짜리를 기본으로 배열
	// ["..."]로 직렬화하는데, Apple 문서는 aud를 문자열로 정의한다.
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": c.creds.TeamID,
		"iat": now.Unix(),
		"exp": now.Add(clientSecretTTL).Unix(),
		"aud": Issuer,
		"sub": c.creds.ClientID,
	})
	tok.Header["kid"] = c.creds.KeyID
	s, err := tok.SignedString(c.creds.PrivateKey)
	if err != nil {
		return "", errors.New("appleid: client secret에 서명할 수 없다")
	}
	return s, nil
}
