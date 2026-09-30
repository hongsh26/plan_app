package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientIPRemoteAddrIgnoresForwardedHeader(t *testing.T) {
	c := ClientIPResolver{Source: "remote_addr"}
	got := c.Resolve(req("203.0.113.9:4444", "198.51.100.1"))
	if got.Fallback || got.Key != "203.0.113.9" {
		t.Fatalf("got %+v", got)
	}
}

func TestClientIPXFFRules(t *testing.T) {
	cases := []struct {
		name         string
		hops         int
		xff          []string
		want         string
		wantFallback bool
	}{
		{"오른쪽 첫 항목", 1, []string{"198.51.100.7"}, "198.51.100.7", false},
		{"왼쪽 위조 항목 무시", 1, []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7", false},
		{"두 홉이면 오른쪽에서 두 번째", 2, []string{"6.6.6.6, 198.51.100.7, 10.0.0.1"}, "198.51.100.7", false},
		{"여러 줄 헤더는 모두 합친다", 1, []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7", false},
		{"ip:port", 1, []string{"198.51.100.7:5555"}, "198.51.100.7", false},
		{"[v6]:port는 /64로 묶는다", 1, []string{"[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:443"}, "2001:db8:1:2::", false},
		{"같은 /64의 다른 주소", 1, []string{"2001:db8:1:2:1::9"}, "2001:db8:1:2::", false},
		{"IPv4-mapped IPv6는 IPv4로 푼다", 1, []string{"::ffff:198.51.100.7"}, "198.51.100.7", false},
		{"항목 부족은 폴백", 2, []string{"198.51.100.7"}, "", true},
		{"헤더 없음은 폴백", 1, nil, "", true},
		{"파싱 실패는 폴백", 1, []string{"not-an-ip"}, "", true},
		{"빈 항목도 위치를 차지한다", 1, []string{"198.51.100.7,"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClientIPResolver{Source: "xff", Hops: tc.hops}.Resolve(req("10.0.0.1:1", tc.xff...))
			if got.Fallback != tc.wantFallback || got.Key != tc.want {
				t.Fatalf("got %+v, want key=%q fallback=%v", got, tc.want, tc.wantFallback)
			}
		})
	}
}

func TestClientIPFallbackUsesSharedSubject(t *testing.T) {
	fb := ClientIPResolver{Source: "xff", Hops: 1}.Resolve(req("10.0.0.1:1"))
	if fb.Subject() != "ip:fallback" {
		t.Fatalf("subject = %q", fb.Subject())
	}
	ok := ClientIPResolver{Source: "xff", Hops: 1}.Resolve(req("10.0.0.1:1", "198.51.100.7"))
	if ok.Subject() != "ip:198.51.100.7" {
		t.Fatalf("subject = %q", ok.Subject())
	}
}

func TestClientIPFromWithoutMiddlewareIsFallback(t *testing.T) {
	if got := ClientIPFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context()); !got.Fallback {
		t.Fatalf("got %+v", got)
	}
}
