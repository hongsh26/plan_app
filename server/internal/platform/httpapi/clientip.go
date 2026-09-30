package httpapi

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
)

// 클라이언트 IP 해석. docs/rate_limit_design.md §5.
//
// load balancer 뒤에서 RemoteAddr는 balancer 주소라 IP 한도가 전역 하나로 뭉친다.
// 그래서 xff 모드는 X-Forwarded-For의 오른쪽에서 N번째 항목을 쓴다. 왼쪽 항목은
// 클라이언트가 위조할 수 있다.

// ClientIPSource 값은 config.ClientIPSource*와 같다. httpapi가 config에 의존하지
// 않도록 문자열로 받는다.
const (
	sourceRemoteAddr = "remote_addr"
	sourceXFF        = "xff"
)

// ClientIP는 해석된 클라이언트 IP다. 원문 IP는 저장하지 않고 요청 제한 키의
// keyed hash 입력으로만 쓴다.
type ClientIP struct {
	// Key는 정규화된 주소다. IPv4는 그대로, IPv6는 /64로 묶은 주소다.
	Key string
	// Fallback이 true이면 IP를 해석하지 못한 요청이다. 설정 오류이며 Key는 비어 있다.
	Fallback bool
}

// Subject는 요청 제한 subject 문자열이다. 폴백 요청은 모두 "ip:fallback" 하나로 센다.
// IP 키를 빼면 IP만 쓰는 scope가 무제한이 되고, RemoteAddr(balancer)로 세면 한 명이
// 전체를 막을 수 있기 때문이다.
func (c ClientIP) Subject() string {
	if c.Fallback || c.Key == "" {
		return "ip:fallback"
	}
	return "ip:" + c.Key
}

// ClientIPResolver는 요청에서 클라이언트 IP를 해석한다.
type ClientIPResolver struct {
	// Source는 "remote_addr" 또는 "xff"다.
	Source string
	// Hops는 xff일 때 신뢰하는 프록시 개수(1 이상)다.
	Hops int
}

// Resolve는 클라이언트 IP를 해석한다. 해석하지 못하면 Fallback이다.
func (c ClientIPResolver) Resolve(r *http.Request) ClientIP {
	var addr netip.Addr
	var ok bool
	if c.Source == sourceXFF {
		addr, ok = c.fromXFF(r)
	} else {
		addr, ok = parseAddr(r.RemoteAddr)
	}
	if !ok {
		return ClientIP{Fallback: true}
	}
	return ClientIP{Key: normalizeAddr(addr)}
}

func (c ClientIPResolver) fromXFF(r *http.Request) (netip.Addr, bool) {
	if c.Hops < 1 {
		return netip.Addr{}, false
	}
	// Header.Get은 첫 줄만 돌려준다. 헤더가 여러 줄이면 클라이언트가 보낸 줄만 읽게 되므로
	// 모든 줄을 순서대로 합쳐 쉼표로 나눈다. 빈 항목도 위치를 유지해 세지 않으면
	// 오른쪽 N번째가 밀린다.
	var entries []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		entries = append(entries, strings.Split(line, ",")...)
	}
	if len(entries) < c.Hops {
		return netip.Addr{}, false
	}
	return parseAddr(strings.TrimSpace(entries[len(entries)-c.Hops]))
}

// parseAddr는 "ip", "ip:port", "[v6]:port"를 모두 받는다.
func parseAddr(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.WithZone(""), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().WithZone(""), true
	}
	return netip.Addr{}, false
}

// normalizeAddr는 IPv4-mapped IPv6를 IPv4로 풀고 IPv6를 /64로 묶는다. Unmap하지
// 않고 /64로 자르면 모든 IPv4 클라이언트가 한 bucket으로 뭉친다.
func normalizeAddr(a netip.Addr) string {
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, err := a.Prefix(64)
	if err != nil {
		return a.String()
	}
	return p.Addr().String()
}

type clientIPKey struct{}

// WithClientIP는 요청마다 클라이언트 IP를 해석해 context에 담는다.
func WithClientIP(resolver ClientIPResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, resolver.Resolve(r))))
	})
}

// ClientIPFrom은 WithClientIP를 거친 요청의 클라이언트 IP를 돌려준다. 미들웨어를
// 거치지 않았으면 Fallback이다.
func ClientIPFrom(ctx context.Context) ClientIP {
	if c, ok := ctx.Value(clientIPKey{}).(ClientIP); ok {
		return c
	}
	return ClientIP{Fallback: true}
}

// WithTestClientIP는 테스트가 특정 클라이언트 IP를 context에 심는다.
func WithTestClientIP(ctx context.Context, c ClientIP) context.Context {
	return context.WithValue(ctx, clientIPKey{}, c)
}
