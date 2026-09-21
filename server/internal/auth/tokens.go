package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// §5.2 세션 수명.
const (
	AccessTokenTTL  = 15 * time.Minute
	RefreshTokenTTL = 30 * 24 * time.Hour
)

// access token의 iss·aud. 같은 서명 키를 다른 용도의 token에 쓰게 되더라도
// 이 token으로 오인되지 않게 고정 값을 검증한다.
const (
	accessTokenIssuer   = "plantogether-api"
	accessTokenAudience = "plantogether-ios"
)

// MinSigningKeySize는 access token HMAC 키의 최소 길이다. HS256의 출력 길이와 같다.
const MinSigningKeySize = 32

// Principal은 인증된 요청의 주체다. §5.2에 따라 device ID는 서버가 발급해
// access token에 결합한 값만 쓴다. 클라이언트가 path나 header로 보낸 device ID는
// 권한 근거가 아니다.
type Principal struct {
	UserID   uuid.UUID
	DeviceID uuid.UUID
}

// accessClaims는 access token의 claim이다. 사용자 이름, 이메일 같은 개인
// 정보를 넣지 않는다. token은 기기 로그나 프록시에 남을 수 있다.
type accessClaims struct {
	jwt.RegisteredClaims
	DeviceID string `json:"did"`
}

// AccessTokens는 access token을 발급·검증한다.
//
// HS256을 쓴다. 발급자와 검증자가 같은 api 프로세스이므로 비대칭 키가 줄
// 이점이 없다. 키 교체(kid)는 아직 없다. 키를 바꾸면 발급된 access token이
// 전부 무효가 되지만 수명이 15분이고 refresh로 다시 받으므로 사용자는 한 번의
// refresh 왕복만 겪는다.
type AccessTokens struct {
	key []byte
	now func() time.Time
}

// NewAccessTokens는 AccessTokens를 만든다.
func NewAccessTokens(key []byte, now func() time.Time) (*AccessTokens, error) {
	if len(key) < MinSigningKeySize {
		return nil, errors.New("auth: access token 서명 키는 32바이트 이상이어야 한다")
	}
	if now == nil {
		now = time.Now
	}
	return &AccessTokens{key: key, now: now}, nil
}

// Issue는 principal에 대한 access token과 만료 시각을 돌려준다.
func (a *AccessTokens) Issue(p Principal) (string, time.Time, error) {
	now := a.now()
	exp := now.Add(AccessTokenTTL)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    accessTokenIssuer,
			Audience:  jwt.ClaimStrings{accessTokenAudience},
			Subject:   p.UserID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		DeviceID: p.DeviceID.String(),
	})
	s, err := tok.SignedString(a.key)
	if err != nil {
		return "", time.Time{}, errors.New("auth: access token에 서명할 수 없다")
	}
	return s, exp, nil
}

// Verify는 access token을 검증하고 principal을 돌려준다. 서명과 만료만 본다.
// 기기 폐기와 계정 상태는 Service.Authenticate가 DB에서 확인한다.
func (a *AccessTokens) Verify(token string) (Principal, error) {
	var c accessClaims
	_, err := jwt.ParseWithClaims(token, &c,
		func(*jwt.Token) (any, error) { return a.key, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(accessTokenIssuer),
		jwt.WithAudience(accessTokenAudience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(a.now),
	)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	uid, err := uuid.Parse(c.Subject)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	did, err := uuid.Parse(c.DeviceID)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	return Principal{UserID: uid, DeviceID: did}, nil
}

// refreshTokenPrefix는 refresh token을 로그·저장소 스캔에서 알아보게 하는 표시다.
const refreshTokenPrefix = "rt_"

// newRefreshToken은 §5.2의 256-bit 난수 opaque token과 DB에 저장할 해시를 만든다.
func newRefreshToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, errors.New("auth: refresh token 난수를 만들 수 없다")
	}
	token = refreshTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashRefreshToken(token), nil
}

// hashRefreshToken은 저장·조회용 해시다. token이 256-bit 난수라 사전 공격이
// 불가능하므로 느린 해시(bcrypt 등)가 필요 없고, 조회 키로 쓰려면 결정적이어야 한다.
func hashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// wellFormedRefreshToken은 DB 조회 전에 형식이 틀린 값을 거른다.
func wellFormedRefreshToken(token string) bool {
	rest, ok := strings.CutPrefix(token, refreshTokenPrefix)
	if !ok {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}
