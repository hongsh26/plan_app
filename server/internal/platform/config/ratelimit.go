package config

import (
	"errors"
	"strings"
	"time"
)

// 요청 제한 설정 환경 변수. api 역할이 읽는다. docs/rate_limit_design.md §5, §8.
const (
	envRateLimitKey              = "RATE_LIMIT_KEY"
	envClientIPSource            = "CLIENT_IP_SOURCE"
	envTrustedProxyHops          = "TRUSTED_PROXY_HOPS"
	envRateLimitPoolMaxConns     = "RATE_LIMIT_POOL_MAX_CONNS"
	envRateLimitStatementTimeout = "RATE_LIMIT_STATEMENT_TIMEOUT"
)

// CLIENT_IP_SOURCE의 허용 값이다.
const (
	// ClientIPSourceRemoteAddr는 연결 상대(RemoteAddr)를 클라이언트로 본다. 로컬 개발과
	// 클라이언트 IP를 보존하는 NLB 구성에 쓴다.
	ClientIPSourceRemoteAddr = "remote_addr"
	// ClientIPSourceXFF는 X-Forwarded-For의 오른쪽에서 TRUSTED_PROXY_HOPS번째를 쓴다.
	ClientIPSourceXFF = "xff"
)

// localRateLimitKey는 APP_ENV=local의 고정 기본 키다. "언제든 기존 코드를 돌릴 수
// 있어야 한다"는 규칙 때문에 로컬은 키 없이도 기동한다. 비로컬에서는 쓰이지 않는다.
var localRateLimitKey = []byte("local-rate-limit-key-not-for-production-use")

// RateLimitSettings는 요청 제한 설정이다.
type RateLimitSettings struct {
	// Key는 bucket key_hash의 HMAC 키다. 32바이트 이상.
	Key []byte

	// ClientIPSource는 클라이언트 IP를 읽는 방식이다.
	ClientIPSource string

	// TrustedProxyHops는 xff일 때 우리 인프라의 프록시 개수다(1 이상).
	TrustedProxyHops int

	// PoolMaxConns는 제한 장치 전용 pool의 최대 연결 수다. 도메인 pool과 분리한다(§4.5).
	PoolMaxConns int32

	// StatementTimeout은 제한 문장의 서버 측 timeout이다(연결 파라미터).
	StatementTimeout time.Duration
}

// LoadRateLimit는 api 역할의 요청 제한 설정을 읽고 검증한다. env는 Load가 검증한 APP_ENV다.
//
// 규칙:
//   - RATE_LIMIT_KEY는 base64, 디코딩해서 32바이트 이상. local이 아니면 필수다.
//   - CLIENT_IP_SOURCE는 local이 아니면 명시 필수다(기본값 없음). 설정 누락이 전역
//     단일 bucket이라는 조용한 결함이 되기 때문이다. local의 기본은 remote_addr다.
//   - xff이면 TRUSTED_PROXY_HOPS가 1 이상이어야 한다.
func LoadRateLimit(env string, lookup Lookup) (RateLimitSettings, error) {
	if lookup == nil {
		return RateLimitSettings{}, errors.New("config: lookup이 nil이다")
	}
	v := &validator{lookup: lookup}
	s := RateLimitSettings{
		PoolMaxConns:     int32(v.optionalInt(envRateLimitPoolMaxConns, 4, 1, 100)),
		StatementTimeout: v.optionalDuration(envRateLimitStatementTimeout, 150*time.Millisecond),
	}

	if raw, ok := lookup(envRateLimitKey); env == EnvLocal && (!ok || strings.TrimSpace(raw) == "") {
		s.Key = localRateLimitKey
	} else {
		s.Key = v.requiredBase64Key(envRateLimitKey, 32, 0)
	}

	raw, ok := lookup(envClientIPSource)
	raw = strings.TrimSpace(raw)
	switch {
	case (!ok || raw == "") && env == EnvLocal:
		s.ClientIPSource = ClientIPSourceRemoteAddr
	case !ok || raw == "":
		v.addf("%s가 설정되지 않았다 (%s 또는 %s)", envClientIPSource, ClientIPSourceRemoteAddr, ClientIPSourceXFF)
	case raw == ClientIPSourceRemoteAddr || raw == ClientIPSourceXFF:
		s.ClientIPSource = raw
	default:
		v.addf("%s가 허용 값 중 하나가 아니다 (허용: %s, %s)", envClientIPSource, ClientIPSourceRemoteAddr, ClientIPSourceXFF)
	}

	if s.ClientIPSource == ClientIPSourceXFF {
		s.TrustedProxyHops = v.optionalInt(envTrustedProxyHops, 0, 1, 10)
		if s.TrustedProxyHops == 0 {
			v.addf("%s는 %s=%s이면 1 이상이어야 한다", envTrustedProxyHops, envClientIPSource, ClientIPSourceXFF)
		}
	}

	if err := v.err(); err != nil {
		return RateLimitSettings{}, err
	}
	return s, nil
}
