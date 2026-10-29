package syncfeed

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// 같은 사용자의 기존 세션은 새 기기 로그인과 로그아웃을 증분으로 받는다(증분 == bootstrap). 로그인이 device 변경을
// 내보내지 않으면 A의 기기 목록이 bootstrap을 다시 받을 때까지 B를 모른다.
//
// 로그인·로그아웃·refresh는 mutation.Run이 아니라 pgx.BeginFunc라 재시도가 없다. 잠금 규약이 깨져 교착하면 요청이
// 오류로 끝나 아래 status 단언에서 드러난다.
func TestDeviceLoginAndLogoutReachOtherSessions(t *testing.T) {
	s := newPartyStack(t)
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
}

// refresh 재사용 탐지가 기기를 폐기하면 같은 사용자의 다른 세션도 revoked=true를 증분으로 받는다.
func TestRefreshReuseRevocationReachesOtherSessions(t *testing.T) {
	s := newPartyStack(t)
	u := s.newUser(t, "reuse.u")
	aTok, aDev := s.signInDevice(t, u.subject)
	bRefresh, bDev := s.signInRefresh(t, u.subject)
	a := s.bootstrapDevices(t, aTok, true)
	if _, ok := a.present[bDev]; !ok {
		t.Fatalf("bootstrap에 B 기기가 없다: %v", a.present)
	}

	body := `{"refresh_token":"` + bRefresh + `"}`
	if rec := s.req(t, http.MethodPost, "/v1/auth/refresh", "", "", "", body); rec.Code != http.StatusOK {
		t.Fatalf("첫 refresh = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := s.req(t, http.MethodPost, "/v1/auth/refresh", "", "", "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("재사용 refresh = %d, want 401: %s", rec.Code, rec.Body.String())
	}

	s.drainDevices(t, a, aTok, u.id)
	if _, ok := a.present[bDev]; ok {
		t.Fatal("A의 기기 목록에 재사용 탐지로 폐기된 B가 남았다")
	}
	if _, ok := a.present[aDev]; !ok {
		t.Fatal("A 자신의 기기가 빠졌다")
	}
	if fresh := s.bootstrapDevices(t, aTok, true); !reflect.DeepEqual(keys(a.present), keys(fresh.raw)) {
		t.Fatalf("재사용 폐기 뒤 증분 기기 %v != bootstrap %v", keys(a.present), keys(fresh.raw))
	}
}

// signInRefresh는 새 기기로 로그인하고 refresh token과 기기 ID를 돌려준다.
func (s *partyStack) signInRefresh(t *testing.T, subject string) (string, uuid.UUID) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"identity_token": subject, "authorization_code": "c", "raw_nonce": "n", "display_name": "P6"})
	rec := s.req(t, http.MethodPost, "/v1/auth/apple", "", "", "", string(b))
	if rec.Code != http.StatusOK {
		t.Fatalf("로그인 = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		DeviceID     string `json:"device_id"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.RefreshToken, uuid.MustParse(out.DeviceID)
}
