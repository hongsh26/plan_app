package config

import (
	"encoding/base64"
	"errors"
	"strings"
)

// 인증 설정 환경 변수. api 역할만 읽는다.
const (
	envAccessTokenSigningKey = "ACCESS_TOKEN_SIGNING_KEY"
	envAppleClientID         = "APPLE_CLIENT_ID"
	envAppleTeamID           = "APPLE_TEAM_ID"
	envAppleKeyID            = "APPLE_KEY_ID"
	envApplePrivateKey       = "APPLE_PRIVATE_KEY"
	envTokenEncryptionKey    = "TOKEN_ENCRYPTION_KEY"
)

// AuthSettings는 api의 인증 설정이다.
type AuthSettings struct {
	// AccessTokenSigningKey는 access token HMAC 키다. 32바이트 이상.
	AccessTokenSigningKey []byte

	// AppleClientID는 앱 bundle ID다. identity token의 aud와 비교한다.
	AppleClientID string

	// AppleCodeExchange는 Apple code 교환 자격이다. nil이면 교환을 건너뛴다.
	// nil은 APP_ENV=local에서만 가능하다.
	AppleCodeExchange *AppleCodeExchangeSettings

	// TokenEncryptionKey는 로컬 암호화 키다. Apple refresh token과 push token을
	// 봉인한다(§11). 항상 필수다.
	TokenEncryptionKey []byte
}

// AppleCodeExchangeSettings는 client secret 서명에 필요한 자격이다.
type AppleCodeExchangeSettings struct {
	TeamID        string
	KeyID         string
	PrivateKeyPEM []byte
}

// ErrKMSNotImplemented는 배포 환경에서 api를 기동하려 할 때다.
//
// §11은 Apple refresh token을 KMS envelope encryption으로 저장하라고 한다. KMS
// 구현이 아직 없으므로, 로컬 키 암호화로 조용히 대체하지 않고 기동을 거부한다.
var ErrKMSNotImplemented = errors.New(
	"APP_ENV가 local이 아니다. 배포 환경의 Apple refresh token 암호화(KMS)가 아직 구현되지 않아 api를 기동할 수 없다")

// LoadAuth는 api 역할의 인증 설정을 읽고 검증한다. env는 Load가 검증한 APP_ENV다.
//
// 규칙:
//   - ACCESS_TOKEN_SIGNING_KEY, APPLE_CLIENT_ID는 항상 필수다.
//   - APPLE_TEAM_ID, APPLE_KEY_ID, APPLE_PRIVATE_KEY는 모두 있거나 모두 없어야 한다.
//     일부만 있으면 오타일 가능성이 높으므로 조용히 교환을 끄지 않고 거부한다.
//   - 셋이 없으면 code 교환을 건너뛴다. local에서만 허용한다.
//   - TOKEN_ENCRYPTION_KEY는 항상 필수다. push token도 암호화해서만 저장한다.
//   - local이 아니면 거부한다(ErrKMSNotImplemented).
func LoadAuth(env string, lookup Lookup) (AuthSettings, error) {
	if lookup == nil {
		return AuthSettings{}, errors.New("config: lookup이 nil이다")
	}
	if env != EnvLocal {
		return AuthSettings{}, ErrKMSNotImplemented
	}

	v := &validator{lookup: lookup}
	s := AuthSettings{
		AccessTokenSigningKey: v.requiredBase64Key(envAccessTokenSigningKey, 32, 0),
		AppleClientID:         v.requiredNonEmpty(envAppleClientID),
		TokenEncryptionKey:    v.requiredBase64Key(envTokenEncryptionKey, 32, 32),
	}

	teamID, hasTeam := nonEmpty(lookup, envAppleTeamID)
	keyID, hasKey := nonEmpty(lookup, envAppleKeyID)
	pemRaw, hasPEM := nonEmpty(lookup, envApplePrivateKey)
	switch {
	case hasTeam && hasKey && hasPEM:
		s.AppleCodeExchange = &AppleCodeExchangeSettings{TeamID: teamID, KeyID: keyID, PrivateKeyPEM: []byte(pemRaw)}
	case hasTeam || hasKey || hasPEM:
		v.addf("%s, %s, %s는 모두 설정하거나 모두 비워야 한다", envAppleTeamID, envAppleKeyID, envApplePrivateKey)
	}

	if err := v.err(); err != nil {
		return AuthSettings{}, err
	}
	return s, nil
}

func nonEmpty(lookup Lookup, key string) (string, bool) {
	raw, ok := lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", false
	}
	return raw, true
}

// requiredBase64Key는 base64(표준) 키를 읽는다. min 이상, max가 0이 아니면 max 이하.
// 오류에 값을 넣지 않는다.
func (v *validator) requiredBase64Key(key string, min, max int) []byte {
	raw := v.requiredNonEmpty(key)
	if raw == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		v.addf("%s가 base64가 아니다", key)
		return nil
	}
	if len(b) < min || (max > 0 && len(b) > max) {
		if max == min {
			v.addf("%s는 디코딩해서 %d바이트여야 한다", key, min)
		} else {
			v.addf("%s는 디코딩해서 %d바이트 이상이어야 한다", key, min)
		}
		return nil
	}
	return b
}
