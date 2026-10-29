package syncfeed

import (
	"net/http"
	"reflect"
	"testing"

	"plantogether/server/internal/platform/mutation"
)

// 같은 사용자의 기존 세션은 새 기기 로그인과 로그아웃을 증분으로 받는다(증분 == bootstrap). 로그인이 device 변경을
// 내보내지 않으면 A의 기기 목록이 bootstrap을 다시 받을 때까지 B를 모른다.
func TestDeviceLoginAndLogoutReachOtherSessions(t *testing.T) {
	s := newPartyStack(t)
	retriesBefore := mutation.Retries()
	u := s.newUser(t, "dev.u")
	aTok, aDev := s.signInDevice(t, u.subject)
	a := s.bootstrapDevices(t, aTok, true)

	bTok, bDev := s.signInDevice(t, u.subject)
	s.drainDevices(t, a, aTok, u.id)
	if _, ok := a.present[bDev]; !ok {
		t.Fatalf("A가 B 기기 로그인을 증분으로 받지 못했다: %v", a.present)
	}
	if fresh := s.bootstrapDevices(t, aTok, true); !reflect.DeepEqual(keys(a.present), keys(fresh.raw)) {
		t.Fatalf("로그인 뒤 증분 기기 %v != bootstrap %v", keys(a.present), keys(fresh.raw))
	}

	if rec := s.req(t, http.MethodPost, "/v1/auth/logout", bTok, "", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("로그아웃 = %d: %s", rec.Code, rec.Body.String())
	}
	s.drainDevices(t, a, aTok, u.id)
	if _, ok := a.present[bDev]; ok {
		t.Fatal("A의 기기 목록에 로그아웃한 B가 남았다")
	}
	if _, ok := a.present[aDev]; !ok {
		t.Fatal("A 자신의 기기가 빠졌다")
	}
	if fresh := s.bootstrapDevices(t, aTok, true); !reflect.DeepEqual(keys(a.present), keys(fresh.raw)) {
		t.Fatalf("로그아웃 뒤 증분 기기 %v != bootstrap %v", keys(a.present), keys(fresh.raw))
	}
	if d := mutation.Retries() - retriesBefore; d != 0 {
		t.Fatalf("mutation 재시도 %d회", d)
	}
}
