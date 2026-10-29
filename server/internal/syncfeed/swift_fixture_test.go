package syncfeed

// iOS sync 적용기 계약 테스트용 fixture 기록기.
//
// DUMP_SWIFT_FIXTURES=<디렉터리>이면 party_sync_test의 인수 시나리오와 순서 역전 재현이 실제로 받은 bootstrap 응답과
// 피드 페이지를 세션별로 기록하고, 테스트가 통과한 뒤에만 sync_<시나리오>.json으로 쓴다. Swift 테스트
// (src/PlanTogetherTests/SyncFixtureTests.swift)는 같은 응답을 적용기에 넣어 최종 모델이 새 bootstrap과 같은지 본다.
//
// 변수가 없으면 아무것도 하지 않는다. 요청·쿼리를 더하지 않으므로 CI의 테스트 시간과 순서가 그대로다. 별도 테스트
// 함수가 아니라 기존 테스트에 붙어 있으므로 건너뛰는(skip) 테스트도 생기지 않는다.
//
// 생성: cd server && DUMP_SWIFT_FIXTURES=$PWD/../src/PlanTogetherTests/Fixtures go test -count=1 \
//   -run 'TestPartySyncIncrementalMatchesBootstrap|TestLeaveRacing|TestTransferRacing|TestRejoinRacing|TestRenameRacingJoin|TestBootstrapCarries|TestBootstrapFloorKeeps|TestBootstrapOmits' ./internal/syncfeed/

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/uuid"
)

const fixtureDirEnv = "DUMP_SWIFT_FIXTURES"

type fixtureEntity struct {
	EntityType string         `json:"entity_type"`
	EntityID   string         `json:"entity_id"`
	Version    int64          `json:"version"`
	Payload    map[string]any `json:"payload"`
}

type fixtureCheckpoint struct {
	// AfterPages는 bootstrap 뒤 적용한 페이지 수다.
	AfterPages int `json:"after_pages"`
	// Entities는 그 시점 새 bootstrap의 Party 범위 entity다(converge가 대조한 기준).
	Entities []fixtureEntity `json:"entities"`
}

// fixtureSession은 기기(세션) 하나가 받은 응답 열이다.
type fixtureSession struct {
	Name        string              `json:"name"`
	SelfUserID  string              `json:"self_user_id"`
	Bootstrap   json.RawMessage     `json:"bootstrap"`
	Pages       []json.RawMessage   `json:"pages"`
	Checkpoints []fixtureCheckpoint `json:"checkpoints"`
	// Expected는 모든 페이지를 받은 뒤 새 bootstrap을 §7.2대로 적용한 모델이다(하한 항목 먼저, 하한보다 큰 upsert).
	Expected []fixtureEntity `json:"expected"`

	finish func(t *testing.T) []byte // 마지막까지 받고 새 bootstrap 원문을 돌려준다.
}

func (fs *fixtureSession) addPage(body []byte) {
	fs.Pages = append(fs.Pages, sanitizeFixture(body))
}

func (fs *fixtureSession) addCheckpoint(fresh map[entityKey]entity) {
	cp := fixtureCheckpoint{AfterPages: len(fs.Pages), Entities: fixtureEntities(fresh)}
	if n := len(fs.Checkpoints); n > 0 && fs.Checkpoints[n-1].AfterPages == cp.AfterPages {
		fs.Checkpoints[n-1] = cp
		return
	}
	fs.Checkpoints = append(fs.Checkpoints, cp)
}

type fixtureRecorder struct {
	s        *partyStack
	dir      string
	Scenario string            `json:"scenario"`
	Desc     string            `json:"description"`
	Diverges []string          `json:"diverges_without"`
	Sessions []*fixtureSession `json:"sessions"`
}

// fixture는 시나리오 기록기를 만든다. diverges는 Swift 대조군 중 이 시나리오에서 bootstrap과 달라져야 하는 것이다
// (rules: 규칙 (a)(b) 끔, floors: version 하한 끔, orphans: 부모 없는 entity 버림).
func (s *partyStack) fixture(t *testing.T, scenario, desc string, diverges ...string) *fixtureRecorder {
	t.Helper()
	dir := os.Getenv(fixtureDirEnv)
	if dir == "" {
		return nil
	}
	if diverges == nil {
		diverges = []string{}
	}
	r := &fixtureRecorder{s: s, dir: dir, Scenario: scenario, Desc: desc, Diverges: diverges}
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		for _, fs := range r.Sessions {
			fs.Bootstrap = sanitizeFixture(fs.Bootstrap)
			fs.Expected = expectedFromBootstrap(t, fs.finish(t))
		}
		b, err := json.MarshalIndent(r, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sync_"+scenario+".json"), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	return r
}

// client는 Party 모델 세션을 기록한다. 이후 pull·converge가 페이지와 checkpoint를 쌓는다.
func (r *fixtureRecorder) client(c *client) {
	if r == nil {
		return
	}
	fs := &fixtureSession{Name: c.name, SelfUserID: c.self.String(), Bootstrap: c.bootstrapBody, Pages: []json.RawMessage{}, Checkpoints: []fixtureCheckpoint{}}
	fs.finish = func(t *testing.T) []byte {
		r.s.drain(t, c)
		r.s.converge(t, "fixture "+r.Scenario, c)
		return r.s.bootstrapBody(t, c.token)
	}
	c.rec = fs
	r.Sessions = append(r.Sessions, fs)
}

// devices는 기기·사용자 모델 세션을 기록한다.
func (r *fixtureRecorder) devices(m *deviceModel, token string, userID uuid.UUID) {
	if r == nil {
		return
	}
	fs := &fixtureSession{Name: m.typ, SelfUserID: userID.String(), Bootstrap: m.bootstrapBody, Pages: []json.RawMessage{}, Checkpoints: []fixtureCheckpoint{}}
	fs.finish = func(t *testing.T) []byte {
		r.s.drainDevices(t, m, token, userID)
		return r.s.bootstrapBody(t, token)
	}
	m.rec = fs
	r.Sessions = append(r.Sessions, fs)
}

func (s *partyStack) bootstrapBody(t *testing.T, token string) []byte {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/v1/sync/bootstrap", token, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// sanitizeFixture는 실행마다 달라 계약과 무관하거나 비밀인 필드(request_id, server_time, notification_ref_key)를 지운다.
func sanitizeFixture(body []byte) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		panic(err)
	}
	delete(m, "request_id")
	delete(m, "server_time")
	delete(m, "notification_ref_key")
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// expectedFromBootstrap은 모든 entity type을 §7.2대로 한 저장소에 적용한 결과다.
func expectedFromBootstrap(t *testing.T, body []byte) []fixtureEntity {
	t.Helper()
	var resp struct {
		Snapshot []wireChange `json:"snapshot"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	floors := map[entityKey]int64{}
	for _, c := range resp.Snapshot {
		if k := (entityKey{c.EntityType, c.EntityID}); c.Operation == "tombstone" && c.Version != nil && *c.Version > floors[k] {
			floors[k] = *c.Version
		}
	}
	model := map[entityKey]entity{}
	for _, c := range resp.Snapshot {
		k := entityKey{c.EntityType, c.EntityID}
		if c.Operation != "upsert" || c.Version == nil {
			continue
		}
		if f, ok := floors[k]; ok && *c.Version <= f {
			continue
		}
		model[k] = entity{version: *c.Version, payload: decodePayload(t, c.Payload)}
	}
	return fixtureEntities(model)
}

func fixtureEntities(m map[entityKey]entity) []fixtureEntity {
	out := make([]fixtureEntity, 0, len(m))
	for k, e := range m {
		out = append(out, fixtureEntity{EntityType: k.typ, EntityID: k.id.String(), Version: e.version, Payload: e.payload})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EntityType != out[j].EntityType {
			return out[i].EntityType < out[j].EntityType
		}
		return out[i].EntityID < out[j].EntityID
	})
	return out
}
