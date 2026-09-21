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
	}
}

func TestLoadAuthMinimalLocalSkipsCodeExchange(t *testing.T) {
	s, err := LoadAuth(EnvLocal, FromMap(authEnv()))
	if err != nil {
		t.Fatalf("최소 로컬 설정을 거부했다: %v", err)
	}
	if s.AppleCodeExchange != nil || s.TokenEncryptionKey != nil {
		t.Error("Apple 자격이 없는데 code 교환이 켜졌다")
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
	for _, want := range []string{"ACCESS_TOKEN_SIGNING_KEY", "APPLE_CLIENT_ID"} {
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

func TestLoadAuthRequiresEncryptionKeyWithAppleCredentials(t *testing.T) {
	env := authEnv()
	env["APPLE_TEAM_ID"], env["APPLE_KEY_ID"], env["APPLE_PRIVATE_KEY"] = "TEAM", "KEY", "-----BEGIN PRIVATE KEY-----"
	if _, err := LoadAuth(EnvLocal, FromMap(env)); err == nil || !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY") {
		t.Fatalf("암호화 키 없이 code 교환이 켜졌다: %v", err)
	}

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
