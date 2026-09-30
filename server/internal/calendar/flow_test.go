package calendar

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/httpapi"
)

func TestConnectionDefaultsCreateAndTransitions(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal."+uuid.NewString())
	b := s.signIn(t, "cal."+uuid.NewString())

	c := s.connection(t, a)
	if c.Status != "disconnected" || c.Version != 0 || len(c.Sources) != 0 {
		t.Fatalf("연결 전 상태 = %+v", c)
	}

	// "Local"은 서버 환경에 따라 뜻이 달라지므로 시간대로 받지 않는다.
	assertError(t, s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"time_zone":"Local"}`, ""),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
	// 서버가 정하는 상태(ready, syncing)와 모르는 상태는 클라이언트가 정할 수 없다.
	for _, bad := range []string{"ready", "syncing", "stale", "nonsense"} {
		assertError(t, s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"`+bad+`"}`, ""),
			http.StatusBadRequest, httpapi.CodeInvalidRequest)
	}
	// If-Match 형식이 잘못되면 400. version 0은 받지 않는다(생성은 If-Match 없이).
	assertError(t, s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"selecting"}`, "0"),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
	// 연결이 없는데 If-Match를 주면 낡은 version이다.
	assertError(t, s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"selecting"}`, "3"),
		http.StatusConflict, httpapi.CodeVersionConflict)

	s.enableCalendar(t, a)
	c = s.connection(t, a)
	if c.Status != "selecting" || c.Version < 1 || !c.IsSyncDevice || len(c.Sources) != 1 || c.Sources[0].Key != "cal-1" {
		t.Fatalf("연결 뒤 상태 = %+v", c)
	}
	// 이미 있는 연결을 create-only로 덮어쓸 수 없고, 낡은 version도 거부된다.
	assertError(t, s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"denied"}`, ""),
		http.StatusConflict, httpapi.CodeVersionConflict)
	assertError(t, s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"denied"}`, "1"),
		http.StatusConflict, httpapi.CodeVersionConflict)

	// 허용되지 않는 전이: selecting에서 곧바로 error 같은 것은 없다.
	assertError(t, s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"error"}`, itoa(c.Version)),
		http.StatusConflict, codeStateInvalid)

	// 다른 사용자는 자기 연결만 본다.
	if other := s.connection(t, b); other.Status != "disconnected" || other.SyncDeviceID != nil {
		t.Fatalf("다른 사용자의 연결이 보인다: %+v", other)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestSnapshotReplacesFactsAtomicallyAndIsIdempotent(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal."+uuid.NewString())
	s.enableCalendar(t, a)

	first := []fact{mkFact(1, 1, 10), mkFact(2, 2, 10), mkFact(3, 3, 10)}
	snap := s.sync(t, a, 1, first, 2)
	if snap.Status != "completed" || snap.Generation == nil || *snap.Generation != 1 || *snap.FactCount != 3 {
		t.Fatalf("snapshot = %+v", snap)
	}
	c := s.connection(t, a)
	if c.Status != "ready" || c.ActiveGeneration != 1 || c.LastCompletedSyncAt == nil {
		t.Fatalf("connection = %+v", c)
	}
	if n := factCount(t, s, a.UserID); n != 3 {
		t.Fatalf("facts = %d", n)
	}
	// staging은 complete가 정리한다.
	if n := count(t, s.pool, `SELECT count(*) FROM calendar_snapshot_facts_staging WHERE session_id = $1`, snap.ID); n != 0 {
		t.Fatalf("staging 잔존 = %d", n)
	}

	// 같은 session을 다시 complete해도 같은 결과다(다른 Idempotency-Key).
	rec := s.complete(t, a, snap.ID, 3)
	if rec.Code != http.StatusOK || *decode[snapEnv](t, rec).Snapshot.Generation != 1 {
		t.Fatalf("재호출 = %d %s", rec.Code, rec.Body.String())
	}
	if n := factCount(t, s, a.UserID); n != 3 || s.connection(t, a).ActiveGeneration != 1 {
		t.Fatalf("재호출이 상태를 바꿨다")
	}
	// 같은 revision·같은 값의 create는 같은 session이다. 값이 다르면 409.
	rec = s.createSnapshot(t, a, 1, 2)
	if rec.Code != http.StatusOK || decode[snapEnv](t, rec).Snapshot.ID != snap.ID {
		t.Fatalf("같은 revision create = %d %s", rec.Code, rec.Body.String())
	}
	assertError(t, s.createSnapshot(t, a, 1, 5), http.StatusConflict, httpapi.CodeVersionConflict)

	// 새 snapshot은 통째로 교체한다: 2번은 유지, 1·3번은 사라지고 4번이 생긴다.
	snap2 := s.sync(t, a, 2, []fact{mkFact(2, 2, 10), mkFact(4, 4, 10)}, 10)
	if *snap2.Generation != 2 {
		t.Fatalf("generation = %d", *snap2.Generation)
	}
	if n := factCount(t, s, a.UserID); n != 2 {
		t.Fatalf("교체 뒤 facts = %d", n)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM calendar_busy_facts WHERE user_id = $1 AND generation <> 2`, a.UserID); n != 0 {
		t.Fatalf("옛 generation row = %d", n)
	}
	// 빈 snapshot도 유효하다(일정이 하나도 없는 사용자). 그러면 facts가 0이고 여전히 ready다.
	s.sync(t, a, 3, nil, 10)
	if c := s.connection(t, a); c.Status != "ready" || c.ActiveGeneration != 3 || factCount(t, s, a.UserID) != 0 {
		t.Fatalf("빈 snapshot 뒤 = %+v facts=%d", c, factCount(t, s, a.UserID))
	}
}

func TestIncompleteOrAbortedSnapshotKeepsExistingGeneration(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal."+uuid.NewString())
	s.enableCalendar(t, a)
	s.sync(t, a, 1, []fact{mkFact(1, 1, 10), mkFact(2, 2, 10)}, 10)

	// page 하나를 안 올림 → complete 거부, 기존 generation과 facts 유지.
	rec := s.createSnapshot(t, a, 2, 2)
	id := decode[snapEnv](t, rec).Snapshot.ID
	if rec := s.putPage(t, a, id, 0, []fact{mkFact(9, 1, 10)}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	assertError(t, s.complete(t, a, id, 1), http.StatusConflict, codeSnapshotIncomplete)
	// 개수 선언이 틀려도 거부.
	if rec := s.putPage(t, a, id, 1, nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	assertError(t, s.complete(t, a, id, 5), http.StatusConflict, codeSnapshotIncomplete)
	if c := s.connection(t, a); c.ActiveGeneration != 1 || factCount(t, s, a.UserID) != 2 {
		t.Fatalf("실패한 complete가 상태를 바꿨다: %+v", c)
	}
	// 연결은 syncing이다(마지막 정상 데이터는 유지).
	if c := s.connection(t, a); c.Status != "syncing" {
		t.Fatalf("status = %s", c.Status)
	}

	// abort하면 ready로 돌아가고 staging이 지워진다. 그 session은 더 못 쓴다.
	rec = s.mut(t, http.MethodPost, "/v1/calendar/snapshots/"+id.String()+"/abort", a.AccessToken, "", "")
	if rec.Code != http.StatusOK || decode[snapEnv](t, rec).Snapshot.Status != "aborted" {
		t.Fatalf("abort = %d %s", rec.Code, rec.Body.String())
	}
	if c := s.connection(t, a); c.Status != "ready" || c.ActiveGeneration != 1 {
		t.Fatalf("abort 뒤 = %+v", c)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM calendar_snapshot_facts_staging WHERE session_id = $1`, id); n != 0 {
		t.Fatalf("staging 잔존 = %d", n)
	}
	assertError(t, s.putPage(t, a, id, 0, nil), http.StatusConflict, codeSnapshotState)
	assertError(t, s.complete(t, a, id, 0), http.StatusConflict, codeSnapshotState)

	// 새 session을 만들면 이전 열린 session은 자동으로 abort된다(열린 session은 하나뿐).
	first := decode[snapEnv](t, s.createSnapshot(t, a, 3, 1)).Snapshot.ID
	second := decode[snapEnv](t, s.createSnapshot(t, a, 4, 1)).Snapshot.ID
	if st := count(t, s.pool, `SELECT count(*) FROM calendar_snapshot_sessions WHERE id = $1 AND status = 'aborted'`, first); st != 1 {
		t.Fatal("이전 열린 session이 abort되지 않았다")
	}
	_ = second
}

func TestSnapshotValidation(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal."+uuid.NewString())
	s.enableCalendar(t, a)
	id := decode[snapEnv](t, s.createSnapshot(t, a, 1, 2)).Snapshot.ID

	ok := mkFact(1, 1, 10)
	mod := func(f func(*fact)) fact { x := ok; f(&x); return x }
	bad := map[string]fact{
		"짧은 key":     mod(func(f *fact) { f.Key = "AAAA" }),
		"base64 아님":  mod(func(f *fact) { f.Key = "!!!!" }),
		"end<=start": mod(func(f *fact) { f.End = f.Start }),
		"window 밖": mod(func(f *fact) {
			f.Start = winEnd.Add(48 * 60 * 60 * 1e9).Format("2006-01-02T15:04:05Z")
			f.End = winEnd.Add(50 * 60 * 60 * 1e9).Format("2006-01-02T15:04:05Z")
		}),
		"모르는 시간대":       mod(func(f *fact) { f.TimeZone = "Mars/Base" }),
		"free는 올리지 않는다": mod(func(f *fact) { f.Availability = "free" }),
		"시간 형식":         mod(func(f *fact) { f.Start = "yesterday" }),
		"Local 시간대":     mod(func(f *fact) { f.TimeZone = "Local" }),
	}
	for name, f := range bad {
		assertError(t, s.putPage(t, a, id, 0, []fact{f}), http.StatusBadRequest, httpapi.CodeInvalidRequest)
		_ = name
	}
	// 같은 page 안 중복 key, 다른 page와 겹치는 key.
	assertError(t, s.putPage(t, a, id, 0, []fact{ok, ok}), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	if rec := s.putPage(t, a, id, 0, []fact{ok}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	assertError(t, s.putPage(t, a, id, 1, []fact{ok}), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	// page 번호 범위, 알 수 없는 필드(allowlist), 너무 많은 fact.
	assertError(t, s.putPage(t, a, id, 2, nil), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.req(t, http.MethodPut, "/v1/calendar/snapshots/"+id.String()+"/pages/0", a.AccessToken, nil,
		`{"facts":[{"source_event_key":"`+key(1)+`","start_at":"`+ok.Start+`","end_at":"`+ok.End+`","all_day":false,"time_zone":"Asia/Seoul","availability":"busy","title":"비밀 회의"}]}`),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
	many := make([]fact, maxFactsPerPage+1)
	for i := range many {
		many[i] = mkFact(byte(i), 1, 10)
		many[i].Key = base64Key(i)
	}
	assertError(t, s.putPage(t, a, id, 0, many), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	// 실패한 page 요청은 기존 page 내용을 바꾸지 않았다.
	if n := count(t, s.pool, `SELECT count(*) FROM calendar_snapshot_facts_staging WHERE session_id = $1`, id); n != 1 {
		t.Fatalf("staging = %d, want 1", n)
	}
	// window 검증: 뒤집힌 window, 너무 긴 window, revision 0.
	assertError(t, s.mut(t, http.MethodPost, "/v1/calendar/snapshots", a.AccessToken,
		`{"revision":9,"window_start":"`+winEnd.Format("2006-01-02T15:04:05Z")+`","window_end":"`+winStart.Format("2006-01-02T15:04:05Z")+`","expected_pages":1}`, ""),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.mut(t, http.MethodPost, "/v1/calendar/snapshots", a.AccessToken,
		`{"revision":0,"window_start":"`+winStart.Format("2006-01-02T15:04:05Z")+`","window_end":"`+winEnd.Format("2006-01-02T15:04:05Z")+`","expected_pages":1}`, ""),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
}

func base64Key(i int) string {
	b := make([]byte, 16)
	b[0], b[1] = byte(i), byte(i>>8)
	b[15] = 0x7f
	return encodeKey(b)
}

func TestSnapshotRequiresSourceStateAndMonotonicRevision(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal."+uuid.NewString())
	// 연결이 없으면 시작할 수 없다.
	assertError(t, s.createSnapshot(t, a, 1, 1), http.StatusConflict, codeStateInvalid)
	// 권한만 있고 source를 고르지 않았으면 시작할 수 없다(사용자가 하나 이상 명시적으로 선택해야 한다).
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"status":"selecting","claim_sync_device":true}`, ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	assertError(t, s.createSnapshot(t, a, 1, 1), http.StatusConflict, codeSourceRequired)
	// 모두 꺼진 source도 마찬가지다.
	c := s.connection(t, a)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"sources":[{"key":"cal-1","enabled":false}]}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	assertError(t, s.createSnapshot(t, a, 1, 1), http.StatusConflict, codeSourceRequired)
	c = s.connection(t, a)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", a.AccessToken, `{"sources":[{"key":"cal-1","enabled":true}]}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}

	s.sync(t, a, 10, []fact{mkFact(1, 1, 10)}, 10)
	// 이미 받아들인 revision보다 작거나 같은 새 revision은 거부한다. current_version에 마지막 revision.
	rec := s.createSnapshot(t, a, 5, 1)
	assertError(t, rec, http.StatusConflict, httpapi.CodeVersionConflict)
	if e := decode[httpapi.ErrorResponse](t, rec); e.CurrentVersion == nil || *e.CurrentVersion != 10 {
		t.Fatalf("current_version = %v", e.CurrentVersion)
	}
	if rec := s.createSnapshot(t, a, 11, 1); rec.Code != http.StatusCreated {
		t.Fatalf("다음 revision = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotIsBoundToOwnerAndSyncDevice(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal."+uuid.NewString())
	b := s.signIn(t, "cal."+uuid.NewString())
	s.enableCalendar(t, a)
	s.enableCalendar(t, b)
	id := decode[snapEnv](t, s.createSnapshot(t, a, 1, 1)).Snapshot.ID

	// 다른 사용자는 존재를 알 수 없다.
	assertError(t, s.putPage(t, b, id, 0, nil), http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.complete(t, b, id, 0), http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.mut(t, http.MethodPost, "/v1/calendar/snapshots/"+id.String()+"/abort", b.AccessToken, "", ""), http.StatusNotFound, httpapi.CodeNotFound)

	// 같은 사용자의 다른 기기는 sync 기기가 아니면 올릴 수 없다.
	a2 := s.signInSameUser(t, a)
	assertError(t, s.putPage(t, a2, id, 0, nil), http.StatusForbidden, httpapi.CodeForbidden)
	assertError(t, s.createSnapshot(t, a2, 2, 1), http.StatusForbidden, httpapi.CodeForbidden)
	// 기기를 넘기면(claim) 새 기기가 sync 기기가 되고 이전 기기는 막힌다. 새 기기의 snapshot이 끝날
	// 때까지 상태는 syncing이다.
	c := s.connection(t, a2)
	if rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", a2.AccessToken, `{"claim_sync_device":true}`, itoa(c.Version)); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if c := s.connection(t, a2); !c.IsSyncDevice || s.connection(t, a).IsSyncDevice {
		t.Fatal("sync 기기가 바뀌지 않았다")
	}
	assertError(t, s.putPage(t, a, id, 0, nil), http.StatusForbidden, httpapi.CodeForbidden)
}

func (s *stack) signInSameUser(t *testing.T, first session) session {
	t.Helper()
	var subject string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT provider_subject FROM auth_identities WHERE user_id = $1`, first.UserID).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	rec := s.req(t, http.MethodPost, "/v1/auth/apple", "", nil,
		`{"identity_token":"`+subject+`","authorization_code":"c","raw_nonce":"n"}`)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	second := decode[session](t, rec)
	if second.UserID != first.UserID || second.DeviceID == first.DeviceID {
		t.Fatalf("같은 사용자의 새 기기가 아니다: %+v", second)
	}
	return second
}

func encodeKey(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
