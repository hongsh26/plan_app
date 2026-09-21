package account

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/httpapi"
)

func (s *stack) syncCount(t *testing.T, userID string, entityType string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1 AND entity_type = $2`,
		userID, entityType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPatchMeUpdatesNameAndVersion(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "mut."+uuid.NewString())

	rec := s.mut(t, http.MethodPatch, "/v1/me", sess.AccessToken, uuid.NewString(), `"1"`, `{"display_name":"새 이름"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	me := decode[meResponse](t, rec)
	if me.User.DisplayName != "새 이름" || me.User.Version != 2 {
		t.Errorf("user = %+v", me.User)
	}
	if rec.Header().Get("ETag") != `"2"` {
		t.Errorf("ETag = %q, want \"2\"", rec.Header().Get("ETag"))
	}
	if n := s.syncCount(t, sess.UserID, entityUser); n != 1 {
		t.Errorf("user sync 변경 %d개, want 1", n)
	}
}

// §6: 모든 mutation은 Idempotency-Key를, 수정·삭제는 If-Match를 요구한다.
func TestMutationsRequireKeyAndVersion(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "mut."+uuid.NewString())

	cases := []struct {
		name, method, path, key, ifMatch, body string
	}{
		{"PATCH 키 없음", http.MethodPatch, "/v1/me", "", `"1"`, `{"display_name":"x"}`},
		{"PATCH version 없음", http.MethodPatch, "/v1/me", "k", "", `{"display_name":"x"}`},
		{"PATCH 약한 ETag", http.MethodPatch, "/v1/me", "k", `W/"1"`, `{"display_name":"x"}`},
		{"PATCH 빈 이름", http.MethodPatch, "/v1/me", "k", `"1"`, `{"display_name":" ‮ "}`},
		{"PATCH 키에 공백", http.MethodPatch, "/v1/me", "a b", `"1"`, `{"display_name":"x"}`},
		{"DELETE 키 없음", http.MethodDelete, "/v1/devices/" + sess.DeviceID, "", `"1"`, ``},
		{"DELETE version 없음", http.MethodDelete, "/v1/devices/" + sess.DeviceID, "k", "", ``},
		{"PUT 키 없음", http.MethodPut, "/v1/devices/" + sess.DeviceID + "/push-token", "", `"1"`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertError(t, s.mut(t, tc.method, tc.path, sess.AccessToken, tc.key, tc.ifMatch, tc.body),
				http.StatusBadRequest, httpapi.CodeInvalidRequest)
		})
	}
}

// §7.4: 낡은 version의 수정은 409와 현재 version을 돌려준다.
func TestStaleVersionGetsConflictWithCurrentVersion(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "mut."+uuid.NewString())
	if rec := s.mut(t, http.MethodPatch, "/v1/me", sess.AccessToken, "a", `"1"`, `{"display_name":"첫째"}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}

	rec := s.mut(t, http.MethodPatch, "/v1/me", sess.AccessToken, "b", `"1"`, `{"display_name":"낡은 수정"}`)
	assertError(t, rec, http.StatusConflict, httpapi.CodeVersionConflict)
	e := decode[httpapi.ErrorResponse](t, rec)
	if e.CurrentVersion == nil || *e.CurrentVersion != 2 {
		t.Fatalf("current_version = %v, want 2", e.CurrentVersion)
	}
	if n := s.syncCount(t, sess.UserID, entityUser); n != 1 {
		t.Errorf("충돌한 수정이 sync 변경을 남겼다: %d개", n)
	}
}

// §7.4: 같은 키 재전송은 원래 결과를 돌려준다. 본문이 다르면 409 idempotency_mismatch.
func TestReplayAndIdempotencyMismatch(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "mut."+uuid.NewString())
	key := uuid.NewString()
	body := `{"display_name":"한 번만"}`

	first := s.mut(t, http.MethodPatch, "/v1/me", sess.AccessToken, key, `"1"`, body)
	if first.Code != 200 || first.Header().Get(ReplayedHeader) != "" {
		t.Fatalf("첫 요청 status=%d replayed=%q", first.Code, first.Header().Get(ReplayedHeader))
	}
	// 응답을 잃은 클라이언트가 같은 키, 같은 If-Match, 같은 본문으로 다시 보낸다.
	// version은 이미 2가 됐지만 재전송이므로 충돌이 아니다.
	again := s.mut(t, http.MethodPatch, "/v1/me", sess.AccessToken, key, `"1"`, body)
	if again.Code != 200 || again.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("재전송 status=%d replayed=%q: %s", again.Code, again.Header().Get(ReplayedHeader), again.Body.String())
	}
	if decode[meResponse](t, again).User.Version != 2 {
		t.Error("재전송이 다시 실행돼 version이 올랐다")
	}
	if n := s.syncCount(t, sess.UserID, entityUser); n != 1 {
		t.Errorf("재전송이 sync 변경을 또 남겼다: %d개", n)
	}

	assertError(t, s.mut(t, http.MethodPatch, "/v1/me", sess.AccessToken, key, `"1"`, `{"display_name":"다른 내용"}`),
		http.StatusConflict, httpapi.CodeIdempotencyMismatch)
}

// §5.2: 사용자는 다른 기기를 폐기할 수 있다. 남의 기기는 404다.
func TestDeleteDevice(t *testing.T) {
	s := newStack(t)
	sub := "mut." + uuid.NewString()
	a := s.signIn(t, sub)
	b := s.signIn(t, sub)
	stranger := s.signIn(t, "mut."+uuid.NewString())

	assertError(t, s.mut(t, http.MethodDelete, "/v1/devices/"+stranger.DeviceID, a.AccessToken, "x", `"1"`, ``),
		http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.mut(t, http.MethodDelete, "/v1/devices/not-a-uuid", a.AccessToken, "y", `"1"`, ``),
		http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.mut(t, http.MethodDelete, "/v1/devices/"+b.DeviceID, a.AccessToken, "z", `"7"`, ``),
		http.StatusConflict, httpapi.CodeVersionConflict)

	if rec := s.mut(t, http.MethodDelete, "/v1/devices/"+b.DeviceID, a.AccessToken, "ok", `"1"`, ``); rec.Code != http.StatusNoContent {
		t.Fatalf("기기 폐기 status = %d: %s", rec.Code, rec.Body.String())
	}
	// 폐기된 기기 B는 즉시 인증되지 않는다.
	assertError(t, s.do(t, http.MethodGet, "/v1/me", b.AccessToken, nil), http.StatusUnauthorized, httpapi.CodeInvalidSession)
	// 재전송은 같은 결과(204)다.
	if rec := s.mut(t, http.MethodDelete, "/v1/devices/"+b.DeviceID, a.AccessToken, "ok", `"1"`, ``); rec.Code != http.StatusNoContent || rec.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("재전송 status = %d replayed=%q", rec.Code, rec.Header().Get(ReplayedHeader))
	}
	// 이미 폐기된 기기를 새 키로 지우면 404다.
	assertError(t, s.mut(t, http.MethodDelete, "/v1/devices/"+b.DeviceID, a.AccessToken, "again", `"2"`, ``),
		http.StatusNotFound, httpapi.CodeNotFound)

	var payload string
	if err := s.pool.QueryRow(context.Background(), `
		SELECT payload::text FROM sync_changes
		 WHERE recipient_user_id = $1 AND entity_type = 'device' AND entity_id = $2 AND operation = 'upsert'`,
		a.UserID, b.DeviceID).Scan(&payload); err != nil {
		t.Fatalf("기기 폐기 sync 변경이 없다: %v", err)
	}
	if !strings.Contains(payload, `"revoked": true`) {
		t.Errorf("payload = %s", payload)
	}
}

func pushBody(token, env *string, authz, ts string) string {
	q := func(p *string) string {
		if p == nil {
			return "null"
		}
		return `"` + *p + `"`
	}
	return `{"push_token":` + q(token) + `,"push_environment":` + q(env) +
		`,"push_authorization":"` + authz + `","time_sensitive_setting":"` + ts + `"}`
}

func TestPutPushToken(t *testing.T) {
	s := newStack(t)
	sub := "mut." + uuid.NewString()
	a := s.signIn(t, sub)
	b := s.signIn(t, sub)
	token := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	sandbox := "sandbox"
	path := "/v1/devices/" + a.DeviceID + "/push-token"

	// 다른 기기의 token은 대신 등록할 수 없다.
	assertError(t, s.mut(t, http.MethodPut, "/v1/devices/"+b.DeviceID+"/push-token", a.AccessToken, "k0", `"1"`,
		pushBody(&token, &sandbox, "authorized", "enabled")), http.StatusForbidden, httpapi.CodeForbidden)
	// token과 environment는 함께다.
	assertError(t, s.mut(t, http.MethodPut, path, a.AccessToken, "k1", `"1"`,
		pushBody(&token, nil, "authorized", "enabled")), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	// hex가 아닌 token, 허용 값 밖의 상태.
	bad := "zz"
	assertError(t, s.mut(t, http.MethodPut, path, a.AccessToken, "k2", `"1"`,
		pushBody(&bad, &sandbox, "authorized", "enabled")), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.mut(t, http.MethodPut, path, a.AccessToken, "k3", `"1"`,
		pushBody(&token, &sandbox, "maybe", "enabled")), http.StatusBadRequest, httpapi.CodeInvalidRequest)

	rec := s.mut(t, http.MethodPut, path, a.AccessToken, "k4", `"1"`, pushBody(&token, &sandbox, "authorized", "enabled"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	d := decode[deviceBody](t, rec).Device
	if d.PushAuthorization != "authorized" || d.TimeSensitiveSetting != "enabled" || d.Version != 2 || !d.Current {
		t.Errorf("device = %+v", d)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Error("응답에 push token이 들어 있다")
	}

	// DB에는 암호문만 있다.
	var stored []byte
	var env *string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT push_token_ciphertext, push_environment FROM devices WHERE id = $1`, a.DeviceID).Scan(&stored, &env); err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(token)
	if len(stored) == 0 || bytes.Contains(stored, raw) || bytes.Contains(stored, []byte(token)) {
		t.Fatal("push token이 암호화되지 않고 저장됐다")
	}
	if env == nil || *env != "sandbox" {
		t.Errorf("push_environment = %v", env)
	}

	// 알림을 끄면 token과 environment를 함께 지운다.
	rec = s.mut(t, http.MethodPut, path, a.AccessToken, "k5", `"2"`, pushBody(nil, nil, "denied", "not_supported"))
	if rec.Code != http.StatusOK {
		t.Fatalf("token 해제 status = %d: %s", rec.Code, rec.Body.String())
	}
	if err := s.pool.QueryRow(context.Background(),
		`SELECT push_token_ciphertext, push_environment FROM devices WHERE id = $1`, a.DeviceID).Scan(&stored, &env); err != nil {
		t.Fatal(err)
	}
	if stored != nil || env != nil {
		t.Errorf("해제 후에도 token=%v env=%v", stored, env)
	}
}
