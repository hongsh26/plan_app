package config

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func b64(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

func authEnv() map[string]string {
	return map[string]string{
		"ACCESS_TOKEN_SIGNING_KEY": b64(32),
		"APPLE_CLIENT_ID":          "com.example.plantogether",
		"TOKEN_ENCRYPTION_KEY":     b64(32),
	}
}

func TestLoadAuthMinimalLocalSkipsCodeExchange(t *testing.T) {
	s, err := LoadAuth(EnvLocal, FromMap(authEnv()))
	if err != nil {
		t.Fatalf("최소 로컬 설정을 거부했다: %v", err)
	}
	if s.AppleCodeExchange != nil {
		t.Error("Apple 자격이 없는데 code 교환이 켜졌다")
	}
	if len(s.TokenEncryptionKey) != 32 {
		t.Error("암호화 키가 읽히지 않았다")
	}
	if len(s.AccessTokenSigningKey) != 32 || s.AppleClientID != "com.example.plantogether" {
		t.Errorf("설정 값이 잘못 읽혔다: %+v", s)
	}
}

// 배포 환경에서는 KMS 없이 기동하지 않는다. 로컬 키 암호화로 조용히 대체되면
// 평문 키로 "암호화됐다"고 믿게 된다.
func TestLoadAuthRefusesNonLocal(t *testing.T) {
	env := authEnv()
	env["APPLE_TEAM_ID"], env["APPLE_KEY_ID"], env["APPLE_PRIVATE_KEY"] = "T", "K", "PEM"
	env["TOKEN_ENCRYPTION_KEY"] = b64(32)
	for _, e := range []string{EnvStaging, EnvProduction} {
		if _, err := LoadAuth(e, FromMap(env)); !errors.Is(err, ErrKMSNotImplemented) {
			t.Errorf("%s: err = %v, want ErrKMSNotImplemented", e, err)
		}
	}
}

func TestLoadAuthRequiresSigningKeyAndClientID(t *testing.T) {
	_, err := LoadAuth(EnvLocal, FromMap(map[string]string{}))
	if err == nil {
		t.Fatal("필수 값 없이 통과했다")
	}
	for _, want := range []string{"ACCESS_TOKEN_SIGNING_KEY", "APPLE_CLIENT_ID", "TOKEN_ENCRYPTION_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("오류가 %s를 지목하지 않는다: %v", want, err)
		}
	}
}

func TestLoadAuthRejectsShortOrMalformedSigningKey(t *testing.T) {
	for name, val := range map[string]string{"짧은 키": b64(16), "base64 아님": "not base64!!"} {
		t.Run(name, func(t *testing.T) {
			env := authEnv()
			env["ACCESS_TOKEN_SIGNING_KEY"] = val
			_, err := LoadAuth(EnvLocal, FromMap(env))
			if err == nil {
				t.Fatal("받아들였다")
			}
			if strings.Contains(err.Error(), val) {
				t.Errorf("오류에 키 값이 들어갔다: %v", err)
			}
		})
	}
}

// Apple 자격이 일부만 있으면 오타일 가능성이 높다. 조용히 교환을 끄면 revoke할
// token 없는 계정이 만들어진다.
func TestLoadAuthRejectsPartialAppleCredentials(t *testing.T) {
	env := authEnv()
	env["APPLE_TEAM_ID"] = "TEAM"
	env["APPLE_KEY_ID"] = "KEY"
	if _, err := LoadAuth(EnvLocal, FromMap(env)); err == nil {
		t.Fatal("APPLE_PRIVATE_KEY 없이 통과했다")
	}
}

// push token과 Apple refresh token은 암호화해서만 저장한다. 키는 Apple 자격
// 유무와 무관하게 필수다.
func TestLoadAuthRequiresEncryptionKey(t *testing.T) {
	env := authEnv()
	delete(env, "TOKEN_ENCRYPTION_KEY")
	if _, err := LoadAuth(EnvLocal, FromMap(env)); err == nil || !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY") {
		t.Fatalf("암호화 키 없이 통과했다: %v", err)
	}

	env["APPLE_TEAM_ID"], env["APPLE_KEY_ID"], env["APPLE_PRIVATE_KEY"] = "TEAM", "KEY", "-----BEGIN PRIVATE KEY-----"

	env["TOKEN_ENCRYPTION_KEY"] = b64(16)
	if _, err := LoadAuth(EnvLocal, FromMap(env)); err == nil {
		t.Fatal("16바이트 암호화 키를 받아들였다")
	}

	env["TOKEN_ENCRYPTION_KEY"] = b64(32)
	s, err := LoadAuth(EnvLocal, FromMap(env))
	if err != nil {
		t.Fatalf("완전한 설정을 거부했다: %v", err)
	}
	if s.AppleCodeExchange == nil || s.AppleCodeExchange.TeamID != "TEAM" || len(s.TokenEncryptionKey) != 32 {
		t.Errorf("code 교환 설정이 잘못 읽혔다: %+v", s)
	}
}

func TestLoadWorkerAuthDoesNotRequireAccessSigningKey(t *testing.T) {
	env := authEnv()
	delete(env, "ACCESS_TOKEN_SIGNING_KEY")
	env["APPLE_TEAM_ID"], env["APPLE_KEY_ID"], env["APPLE_PRIVATE_KEY"] = "TEAM", "KEY", "-----BEGIN PRIVATE KEY-----"

	s, err := LoadWorkerAuth(EnvLocal, FromMap(env))
	if err != nil {
		t.Fatalf("worker auth 설정을 거부했다: %v", err)
	}
	if s.AppleClientID != "com.example.plantogether" || len(s.TokenEncryptionKey) != 32 {
		t.Fatalf("worker auth 설정 값이 잘못 읽혔다: %+v", s)
	}
	if s.AppleCodeExchange == nil || s.AppleCodeExchange.TeamID != "TEAM" {
		t.Fatalf("Apple revoke 자격이 읽히지 않았다: %+v", s.AppleCodeExchange)
	}
}

func TestLoadWorkerAuthRefusesNonLocalAndPartialAppleCredentials(t *testing.T) {
	if _, err := LoadWorkerAuth(EnvProduction, FromMap(authEnv())); !errors.Is(err, ErrKMSNotImplemented) {
		t.Fatalf("production worker auth err = %v, want ErrKMSNotImplemented", err)
	}

	env := authEnv()
	env["APPLE_TEAM_ID"] = "TEAM"
	if _, err := LoadWorkerAuth(EnvLocal, FromMap(env)); err == nil ||
		!strings.Contains(err.Error(), "APPLE_TEAM_ID") ||
		!strings.Contains(err.Error(), "APPLE_KEY_ID") ||
		!strings.Contains(err.Error(), "APPLE_PRIVATE_KEY") {
		t.Fatalf("partial Apple 자격 오류가 올바르지 않다: %v", err)
	}
}

func TestLoadAuthDevLoginFlag(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			"ACCESS_TOKEN_SIGNING_KEY": b64(32),
			"APPLE_CLIENT_ID":          "com.example.app",
			"TOKEN_ENCRYPTION_KEY":     b64(32),
		}
	}
	s, err := LoadAuth(EnvLocal, FromMap(base()))
	if err != nil || s.DevLogin {
		t.Fatalf("기본값은 꺼져 있어야 한다: %+v %v", s, err)
	}
	m := base()
	m["DEV_LOGIN_ENABLED"] = "true"
	if s, err = LoadAuth(EnvLocal, FromMap(m)); err != nil || !s.DevLogin {
		t.Fatalf("켜지지 않았다: %+v %v", s, err)
	}
	m["DEV_LOGIN_ENABLED"] = "maybe"
	if _, err = LoadAuth(EnvLocal, FromMap(m)); err == nil {
		t.Fatal("잘못된 값을 받았다")
	}
	// 배포 환경에서는 KMS 가드와 별개로 개발용 로그인 전용 오류가 나야 한다. KMS를 구현해
	// 배포 환경을 허용하는 날에도 이 가드가 남아 있어야 하므로 오류 메시지까지 확인한다.
	for _, env := range []string{"production", "staging"} {
		m["DEV_LOGIN_ENABLED"] = "true"
		_, err = LoadAuth(env, FromMap(m))
		if err == nil || errors.Is(err, ErrKMSNotImplemented) || !strings.Contains(err.Error(), "DEV_LOGIN_ENABLED") {
			t.Fatalf("%s: 개발용 로그인 전용 거부가 아니다: %v", env, err)
		}
		// 꺼져 있으면(또는 값이 없으면) 기존 KMS 거부 그대로다.
		m["DEV_LOGIN_ENABLED"] = "false"
		if _, err = LoadAuth(env, FromMap(m)); !errors.Is(err, ErrKMSNotImplemented) {
			t.Fatalf("%s: %v", env, err)
		}
	}
}
