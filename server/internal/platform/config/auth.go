package config

import (
	"encoding/base64"
	"errors"
	"strings"
)

// 인증 설정 환경 변수. api와 worker 역할이 읽는다.
const (
	envAccessTokenSigningKey = "ACCESS_TOKEN_SIGNING_KEY"
	envAppleClientID         = "APPLE_CLIENT_ID"
	envAppleTeamID           = "APPLE_TEAM_ID"
	envAppleKeyID            = "APPLE_KEY_ID"
	envApplePrivateKey       = "APPLE_PRIVATE_KEY"
	envTokenEncryptionKey    = "TOKEN_ENCRYPTION_KEY"
	envDevLoginEnabled       = "DEV_LOGIN_ENABLED"
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

	// DevLogin이 true이면 identity_token "dev:<이름>"으로 Apple 없이 로그인한다. 시연과 로컬
	// 개발 전용이다. LoadAuth가 KMS 가드와 별개로 APP_ENV=local이 아니면 이 플래그를 거부한다.
	// 켜면 실제 Apple token은 받지 않는다(모든 로그인이 Apple refresh token 없이 만들어진다).
	DevLogin bool
}

// WorkerAuthSettings는 worker가 Apple refresh token을 열고 revoke하는 데 필요한 설정이다.
type WorkerAuthSettings struct {
	AppleClientID      string
	AppleCodeExchange  *AppleCodeExchangeSettings
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
	// 개발용 로그인 가드는 KMS 가드와 독립이다. 나중에 KMS를 구현해 배포 환경을 허용하더라도
	// 이 검사가 남아 있어야 "dev:<이름>"으로 누구나 로그인하는 우회가 배포 환경에서 켜지지 않는다.
	if env != EnvLocal {
		if raw, ok := nonEmpty(lookup, envDevLoginEnabled); ok && raw != "0" && strings.ToLower(raw) != "false" {
			return AuthSettings{}, errors.New(envDevLoginEnabled + "는 APP_ENV=local에서만 켤 수 있다")
		}
		return AuthSettings{}, ErrKMSNotImplemented
	}

	v := &validator{lookup: lookup}
	s := AuthSettings{
		AccessTokenSigningKey: v.requiredBase64Key(envAccessTokenSigningKey, 32, 0),
		AppleClientID:         v.requiredNonEmpty(envAppleClientID),
		TokenEncryptionKey:    v.requiredBase64Key(envTokenEncryptionKey, 32, 32),
	}

	s.AppleCodeExchange = optionalAppleCodeExchange(v, lookup)
	if raw, ok := nonEmpty(lookup, envDevLoginEnabled); ok {
		switch strings.ToLower(raw) {
		case "1", "true":
			s.DevLogin = true
		case "0", "false":
		default:
			v.addf("%s는 true 또는 false여야 한다", envDevLoginEnabled)
		}
	}

	if err := v.err(); err != nil {
		return AuthSettings{}, err
	}
	return s, nil
}

// LoadWorkerAuth는 worker 역할의 삭제 pipeline 설정을 읽고 검증한다.
//
// worker는 access token을 발급하지 않으므로 ACCESS_TOKEN_SIGNING_KEY를 읽지 않는다.
// Apple revoke는 로그인 code 교환과 같은 client secret 자격을 사용한다. 로컬에서
// 자격이 없으면 token이 없는 개발 계정만 삭제할 수 있고, token이 있는 job은 dead가 된다.
func LoadWorkerAuth(env string, lookup Lookup) (WorkerAuthSettings, error) {
	if lookup == nil {
		return WorkerAuthSettings{}, errors.New("config: lookup이 nil이다")
	}
	if env != EnvLocal {
		return WorkerAuthSettings{}, ErrKMSNotImplemented
	}
	v := &validator{lookup: lookup}
	s := WorkerAuthSettings{
		AppleClientID:      v.requiredNonEmpty(envAppleClientID),
		TokenEncryptionKey: v.requiredBase64Key(envTokenEncryptionKey, 32, 32),
	}
	s.AppleCodeExchange = optionalAppleCodeExchange(v, lookup)
	if err := v.err(); err != nil {
		return WorkerAuthSettings{}, err
	}
	return s, nil
}

func optionalAppleCodeExchange(v *validator, lookup Lookup) *AppleCodeExchangeSettings {
	teamID, hasTeam := nonEmpty(lookup, envAppleTeamID)
	keyID, hasKey := nonEmpty(lookup, envAppleKeyID)
	pemRaw, hasPEM := nonEmpty(lookup, envApplePrivateKey)
	switch {
	case hasTeam && hasKey && hasPEM:
		return &AppleCodeExchangeSettings{TeamID: teamID, KeyID: keyID, PrivateKeyPEM: []byte(pemRaw)}
	case hasTeam || hasKey || hasPEM:
		v.addf("%s, %s, %s는 모두 설정하거나 모두 비워야 한다", envAppleTeamID, envAppleKeyID, envApplePrivateKey)
	}
	return nil
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
