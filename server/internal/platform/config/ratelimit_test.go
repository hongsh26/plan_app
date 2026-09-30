package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func rlKey() string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) }

func TestLoadRateLimitLocalDefaults(t *testing.T) {
	s, err := LoadRateLimit(EnvLocal, FromMap(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if s.ClientIPSource != ClientIPSourceRemoteAddr || len(s.Key) < 32 || s.PoolMaxConns != 4 {
		t.Fatalf("settings = %+v", s)
	}
}

func TestLoadRateLimitNonLocalRequiresExplicitSettings(t *testing.T) {
	// CLIENT_IP_SOURCE 누락: 조용한 전역 단일 bucket이 되므로 기동을 거부한다.
	_, err := LoadRateLimit("production", FromMap(map[string]string{"RATE_LIMIT_KEY": rlKey()}))
	if err == nil || !strings.Contains(err.Error(), "CLIENT_IP_SOURCE") {
		t.Fatalf("err = %v", err)
	}
	// 키 누락.
	_, err = LoadRateLimit("production", FromMap(map[string]string{"CLIENT_IP_SOURCE": "remote_addr"}))
	if err == nil || !strings.Contains(err.Error(), "RATE_LIMIT_KEY") {
		t.Fatalf("err = %v", err)
	}
	s, err := LoadRateLimit("production", FromMap(map[string]string{"RATE_LIMIT_KEY": rlKey(), "CLIENT_IP_SOURCE": "remote_addr"}))
	if err != nil || s.ClientIPSource != ClientIPSourceRemoteAddr {
		t.Fatalf("s=%+v err=%v", s, err)
	}
}

func TestLoadRateLimitXFFNeedsHops(t *testing.T) {
	base := map[string]string{"RATE_LIMIT_KEY": rlKey(), "CLIENT_IP_SOURCE": "xff"}
	if _, err := LoadRateLimit("production", FromMap(base)); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_HOPS") {
		t.Fatalf("err = %v", err)
	}
	base["TRUSTED_PROXY_HOPS"] = "2"
	s, err := LoadRateLimit("production", FromMap(base))
	if err != nil || s.TrustedProxyHops != 2 {
		t.Fatalf("s=%+v err=%v", s, err)
	}
}

func TestLoadRateLimitRejectsUnknownSourceAndShortKey(t *testing.T) {
	if _, err := LoadRateLimit(EnvLocal, FromMap(map[string]string{"CLIENT_IP_SOURCE": "header"})); err == nil {
		t.Fatal("알 수 없는 CLIENT_IP_SOURCE를 받았다")
	}
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := LoadRateLimit(EnvLocal, FromMap(map[string]string{"RATE_LIMIT_KEY": short})); err == nil {
		t.Fatal("짧은 키를 받았다")
	}
}
