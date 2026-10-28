package syncfeed

// P6 완료 기준: "두 세션에서 멤버 변경이 cursor 증분으로 정확히 반영된다".
//
// 실제 HTTP 경로(auth Require → party handler → sync handler)로 Party 전이를 차례로 실행하고, 각 단계
// 뒤에 세션마다 자기 cursor에서 GET /v1/sync를 끝까지 읽어 테스트 안의 클라이언트 모델에 적용한다.
// 그 모델이 같은 사용자의 새 bootstrap과 같아야 한다(증분 == bootstrap). 같은 방식으로 틀려도
// 통과하지 않게 단계마다 절대 개수도 확인한다.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/account"
	"plantogether/server/internal/auth"
	"plantogether/server/internal/party"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/jobs"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/postgres/pgtest"
	"plantogether/server/internal/platform/secretbox"
)

// partyEntityTypes는 비교 대상이다. user·device는 last_seen_at처럼 sync 변경 없이 바뀌는 값이 있어 뺀다.
var partyEntityTypes = map[string]bool{
	"party": true, "party_membership": true, "party_invite": true,
	"party_visibility_setting": true, "party_schedule_projection": true,
}

type subjectApple struct{}

func (subjectApple) Verify(_ context.Context, idToken, _ string) (appleid.Identity, error) {
	return appleid.Identity{Subject: idToken}, nil
}

type partyStack struct {
	h    http.Handler
	pool *pgxpool.Pool
	box  *secretbox.LocalAESGCM
}

func newPartyStack(t *testing.T) *partyStack {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	tokens, err := auth.NewAccessTokens(bytes.Repeat([]byte{4}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(auth.Deps{Pool: pool, Apple: subjectApple{}, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.NewLocal("local", bytes.Repeat([]byte{5}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	authHandler := auth.NewHandler(svc, logger)
	mux := http.NewServeMux()
	authHandler.Register(mux)
	account.NewHandler(account.Deps{Pool: pool, Logger: logger, Sealer: box, RefKeyBox: box}).Register(mux, authHandler.Require)
	party.NewHandler(party.Deps{Pool: pool, Logger: logger}).Register(mux, authHandler.Require)
	NewHandler(Deps{Pool: pool, Logger: logger, RefKeyBox: box}).Register(mux, authHandler.Require)
	return &partyStack{h: httpapi.Wrap(logger, mux), pool: pool, box: box}
}

func (s *partyStack) req(t *testing.T, method, path, token, key, ifMatch, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec
}

// must는 기대 status가 아니면 실패한다. 성공 본문을 map으로 돌려준다.
func (s *partyStack) must(t *testing.T, want int, method, path, token, ifMatch, body string) map[string]any {
	t.Helper()
	rec := s.req(t, method, path, token, uuid.NewString(), ifMatch, body)
	if rec.Code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s 응답이 JSON이 아니다: %v", method, path, err)
	}
	return out
}

type user struct {
	id      uuid.UUID
	subject string
}

func (s *partyStack) newUser(t *testing.T, name string) user {
	t.Helper()
	u := user{subject: "p6." + name + "." + uuid.NewString()}
	sess := s.signIn(t, u.subject)
	u.id = sess.userID
	t.Cleanup(func() { cleanupPartyUser(t, s.pool, u.id) })
	return u
}

type signedIn struct {
	userID uuid.UUID
	token  string
}

// signIn은 로그인마다 새 기기와 세션을 만든다. 같은 사용자의 두 번째 기기가 된다.
func (s *partyStack) signIn(t *testing.T, subject string) signedIn {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"identity_token": subject, "authorization_code": "c", "raw_nonce": "n", "display_name": "P6"})
	rec := s.req(t, http.MethodPost, "/v1/auth/apple", "", "", "", string(b))
	if rec.Code != http.StatusOK {
		t.Fatalf("로그인 = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return signedIn{userID: uuid.MustParse(body.UserID), token: body.AccessToken}
}

func cleanupPartyUser(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("cleanup tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`DELETE FROM sync_changes WHERE recipient_user_id = $1`,
		`DELETE FROM idempotency_keys WHERE user_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, userID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT party_id FROM party_memberships WHERE user_id = $1`, userID)
	if err != nil {
		t.Fatal(err)
	}
	var partyIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		partyIDs = append(partyIDs, id)
	}
	rows.Close()
	for _, id := range partyIDs {
		for _, q := range []string{
			`DELETE FROM party_schedule_projections WHERE party_id = $1`,
			`DELETE FROM party_visibility_settings WHERE party_id = $1`,
			`UPDATE party_memberships SET invite_id = NULL WHERE party_id = $1`,
			`DELETE FROM party_invites WHERE party_id = $1`,
			`DELETE FROM party_memberships WHERE party_id = $1`,
			`DELETE FROM parties WHERE id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, q := range []string{
		`DELETE FROM audit_events WHERE actor_user_id = $1`,
		`DELETE FROM users WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, userID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// seedReadyCalendar는 snapshot complete를 거친 것과 같은 ready 연결과 Busy fact 하나를 만든다.
// 이후 Party 생성·가입·공개 수준 변경이 실제 경로로 busyOnly projection을 만든다.
func (s *partyStack) seedReadyCalendar(t *testing.T, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	connID := uuid.New()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO calendar_connections (id, user_id, source_status, active_generation, active_generation_usable, last_completed_sync_at, time_zone)
		VALUES ($1, $2, 'ready', 1, true, now(), 'Asia/Seoul')`, connID, userID); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO calendar_busy_facts
		(connection_id, source_event_key, user_id, generation, start_at, end_at, all_day, time_zone, availability)
		VALUES ($1, $2, $3, 1, $4, $5, false, 'Asia/Seoul', 'busy')`,
		connID, []byte(uuid.NewString()), userID, start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// ---- 테스트 안의 클라이언트 모델 ----

type entityKey struct {
	typ string
	id  uuid.UUID
}

type entity struct {
	version int64
	payload map[string]any
}

// client는 기기 하나(세션 하나)의 로컬 저장소다. iOS 계약(account_backend_design.md §7.1·§7.2)을 그대로 따른다.
//   - upsert는 version이 로컬 version과 versioned tombstone이 남긴 하한보다 클 때만 적용한다.
//   - version 있는 tombstone은 지우고 하한을 기록한다.
//   - version 없는 tombstone은 지우기만 한다(재가입으로 같은 ID·version이 다시 올 수 있다).
//   - 규칙 (a): scope_membership_id가 하한이 기록된(끝난) 멤버십인 upsert는 버린다.
//   - 규칙 (b): version 없는 tombstone은 그 entity의 Party에 scope가 아닌 본인 활성 멤버십이 로컬에 있으면 무시한다
//     (재가입한 새 멤버십으로 이미 다시 받은 사본이다). 탈퇴 트랜잭션은 본인 멤버십 tombstone을 scope tombstone보다
//     먼저 보내므로 정상 탈퇴에서는 남은 본인 활성 멤버십이 없다.
//
// rules가 false이면 (a)(b)를 끈다. 규칙이 실제로 필요한지 보이는 대조군이다.
type client struct {
	name   string
	token  string
	cursor string
	limit  int
	model  map[entityKey]entity
	floors map[entityKey]int64
	pulled int
	// strictOrder이면 부모(party)보다 먼저 온 하위 entity upsert를 실패로 본다.
	strictOrder bool
	rules       bool
	// floorless이면 version 있는 tombstone도 하한 없이 지우기만 한다(초대 version 하한의 대조군).
	floorless bool
	self      uuid.UUID
	// shadow는 같은 변경을 규칙 없이 적용하는 대조군이다(nil이면 없음).
	shadow *client
}

type wireChange struct {
	EntityType string          `json:"entity_type"`
	EntityID   uuid.UUID       `json:"entity_id"`
	Operation  string          `json:"operation"`
	Version    *int64          `json:"version"`
	Payload    json.RawMessage `json:"payload"`
	Scope      *uuid.UUID      `json:"scope_membership_id"`
}

func (s *partyStack) fetchBootstrap(t *testing.T, token string) (map[entityKey]entity, string) {
	t.Helper()
	m, _, w := s.fetchBootstrapFloors(t, token)
	return m, w
}

// fetchBootstrapFloors는 snapshot의 entity와 하한 전용 tombstone 항목(최근 끝난 본인 멤버십)을 함께 준다.
func (s *partyStack) fetchBootstrapFloors(t *testing.T, token string) (map[entityKey]entity, map[entityKey]int64, string) {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/v1/sync/bootstrap", token, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Snapshot  []wireChange `json:"snapshot"`
		Watermark string       `json:"cursor_watermark"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	out := map[entityKey]entity{}
	floors := map[entityKey]int64{}
	for _, c := range body.Snapshot {
		if !partyEntityTypes[c.EntityType] {
			continue
		}
		if c.Operation == "tombstone" && c.Version != nil && c.Scope != nil {
			if k := (entityKey{c.EntityType, c.EntityID}); *c.Version > floors[k] {
				floors[k] = *c.Version
			}
			continue
		}
		if c.Operation != "upsert" || c.Version == nil || c.Scope == nil {
			t.Fatalf("bootstrap entity가 scope 있는 upsert가 아니다: %+v", c)
		}
		out[entityKey{c.EntityType, c.EntityID}] = entity{version: *c.Version, payload: decodePayload(t, c.Payload)}
	}
	return out, floors, body.Watermark
}

func decodePayload(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload: %v (%s)", err, raw)
	}
	return m
}

func (s *partyStack) newClient(t *testing.T, name, token string, limit int) *client {
	t.Helper()
	c := &client{name: name, token: token, limit: limit, strictOrder: true, rules: true, self: s.userOf(t, token)}
	c.model, c.floors, c.cursor = s.fetchBootstrapFloors(t, token)
	return c
}

// withShadow는 같은 bootstrap에서 시작해 규칙 (a)(b) 없이 적용하는 대조군을 붙인다.
func (c *client) withShadow() *client {
	return c.attachShadow("(규칙 없음)", false)
}

// withFloorlessShadow는 규칙 (a)(b)도 하한도 없는 대조군을 붙인다.
func (c *client) withFloorlessShadow() *client {
	return c.attachShadow("(하한 없음)", true)
}

func (c *client) attachShadow(suffix string, floorless bool) *client {
	sh := &client{name: c.name + suffix, self: c.self, floorless: floorless, model: map[entityKey]entity{}, floors: map[entityKey]int64{}}
	for k, e := range c.model {
		sh.model[k] = e
	}
	for k, f := range c.floors {
		if !floorless {
			sh.floors[k] = f
		}
	}
	c.shadow = sh
	return c
}

func (c *client) apply(t *testing.T, ch wireChange) {
	t.Helper()
	if c.shadow != nil {
		c.shadow.apply(t, ch)
	}
	if !partyEntityTypes[ch.EntityType] {
		return
	}
	if ch.Scope == nil {
		t.Fatalf("%s: Party 범위 변경에 scope_membership_id가 없다: %+v", c.name, ch)
	}
	k := entityKey{ch.EntityType, ch.EntityID}
	switch ch.Operation {
	case "upsert":
		if ch.Version == nil {
			t.Fatalf("%s: version 없는 upsert %+v", c.name, ch)
		}
		if _, ended := c.floors[entityKey{"party_membership", *ch.Scope}]; c.rules && ended {
			return // 규칙 (a): 끝난 멤버십으로 발행된 upsert다.
		}
		if f, ok := c.floors[k]; ok && *ch.Version <= f {
			return
		}
		if cur, ok := c.model[k]; ok && *ch.Version <= cur.version {
			return
		}
		payload := decodePayload(t, ch.Payload)
		// 하위 entity upsert는 그 Party가 먼저 와 있어야 한다(한 트랜잭션 안에서 party가 앞선다). 동시 실행에서는
		// 먼저 txid를 받고 나중에 직렬화된 트랜잭션의 변경이 앞에 올 수 있어 순차 시나리오에서만 본다.
		if pid, ok := payload["party_id"].(string); ok && c.strictOrder {
			if _, ok := c.model[entityKey{"party", uuid.MustParse(pid)}]; !ok {
				t.Fatalf("%s: Party %s보다 하위 entity %s %s가 먼저 왔다", c.name, pid, ch.EntityType, ch.EntityID)
			}
		}
		c.model[k] = entity{version: *ch.Version, payload: payload}
	case "tombstone":
		if ch.Version != nil && !c.floorless {
			// 서버는 마지막 upsert보다 큰 version으로 tombstone을 보낸다. 로컬이 더 새로우면 서버 결함이다.
			if cur, ok := c.model[k]; ok && cur.version >= *ch.Version {
				t.Fatalf("%s: tombstone version %d가 로컬 version %d 이하다: %+v", c.name, *ch.Version, cur.version, ch)
			}
			if *ch.Version > c.floors[k] {
				c.floors[k] = *ch.Version
			}
			delete(c.model, k)
			return
		}
		if cur, ok := c.model[k]; ok && c.rules && c.hasOtherOwnMembership(partyOf(k, cur), *ch.Scope) {
			return // 규칙 (b): 다른(새) 본인 멤버십으로 다시 받은 사본이다.
		}
		delete(c.model, k)
	default:
		t.Fatalf("%s: 모르는 operation %q", c.name, ch.Operation)
	}
}

// pull은 cursor에서 has_more가 false가 될 때까지 읽어 적용한다.
func (s *partyStack) pull(t *testing.T, c *client) {
	t.Helper()
	for {
		path := "/v1/sync?cursor=" + url.QueryEscape(c.cursor)
		if c.limit > 0 {
			path += "&limit=" + strconv.Itoa(c.limit)
		}
		rec := s.req(t, http.MethodGet, path, c.token, "", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: sync = %d: %s", c.name, rec.Code, rec.Body.String())
		}
		var page struct {
			Changes []wireChange `json:"changes"`
			Cursor  string       `json:"cursor"`
			HasMore bool         `json:"has_more"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, ch := range page.Changes {
			c.apply(t, ch)
		}
		c.pulled += len(page.Changes)
		c.cursor = page.Cursor
		if !page.HasMore {
			return
		}
	}
}

// converge는 증분 적용 결과가 새 bootstrap과 같아질 때까지 읽는다. 다른 패키지의 열린 트랜잭션이
// settled horizon을 잠시 붙잡을 수 있으므로 최대 10초 기다린다(4f3d4e7과 같은 이유).
func (s *partyStack) converge(t *testing.T, step string, cs ...*client) {
	t.Helper()
	for _, c := range cs {
		deadline := time.Now().Add(10 * time.Second)
		for {
			s.pull(t, c)
			fresh, _ := s.fetchBootstrap(t, c.token)
			diff := diffModels(c.model, fresh)
			if diff == "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("[%s] %s: 증분 결과가 bootstrap과 다르다\n%s", step, c.name, diff)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
}

func diffModels(got, want map[entityKey]entity) string {
	var lines []string
	for k, w := range want {
		g, ok := got[k]
		switch {
		case !ok:
			lines = append(lines, fmt.Sprintf("  누락 %s %s v%d %v", k.typ, k.id, w.version, w.payload))
		case g.version != w.version || !reflect.DeepEqual(g.payload, w.payload):
			lines = append(lines, fmt.Sprintf("  다름 %s %s 증분 v%d %v / bootstrap v%d %v", k.typ, k.id, g.version, g.payload, w.version, w.payload))
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			lines = append(lines, fmt.Sprintf("  남음 %s %s v%d %v", k.typ, k.id, g.version, g.payload))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// count는 모델에서 Party에 속한 entity를 type별로 센다. party entity는 ID로, 나머지는 payload의
// party_id로 판정한다.
func (c *client) count(partyID uuid.UUID) map[string]int {
	out := map[string]int{}
	for k, e := range c.model {
		if (k.typ == "party" && k.id == partyID) || e.payload["party_id"] == partyID.String() {
			out[k.typ]++
		}
	}
	return out
}

func (c *client) expect(t *testing.T, step string, partyID uuid.UUID, want map[string]int) {
	t.Helper()
	got := c.count(partyID)
	for typ := range partyEntityTypes {
		if got[typ] != want[typ] {
			t.Fatalf("[%s] %s: Party entity 개수 %v, want %v", step, c.name, got, want)
		}
	}
}

func partyOf(k entityKey, e entity) string {
	if k.typ == "party" {
		return k.id.String()
	}
	pid, _ := e.payload["party_id"].(string)
	return pid
}

func (c *client) hasOtherOwnMembership(partyID string, scope uuid.UUID) bool {
	for k, e := range c.model {
		if k.typ == "party_membership" && k.id != scope && e.payload["party_id"] == partyID &&
			e.payload["user_id"] == c.self.String() && e.payload["status"] == "active" {
			return true
		}
	}
	return false
}

func (s *partyStack) userOf(t *testing.T, token string) uuid.UUID {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/v1/me", token, "", "", "")
	var body struct {
		User struct {
			ID uuid.UUID `json:"id"`
		} `json:"user"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.User.ID == uuid.Nil {
		t.Fatalf("GET /v1/me = %d: %s", rec.Code, rec.Body.String())
	}
	return body.User.ID
}

func memberName(c *client, userID uuid.UUID) string {
	for k, e := range c.model {
		if k.typ == "party_membership" && e.payload["user_id"] == userID.String() {
			name, _ := e.payload["display_name"].(string)
			return name
		}
	}
	return ""
}

// ---- Party API 도우미 ----

func etagOf(rec *httptest.ResponseRecorder) string { return rec.Header().Get("ETag") }

func (s *partyStack) partyETag(t *testing.T, token string, partyID uuid.UUID) string {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/v1/parties/"+partyID.String(), token, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET party = %d: %s", rec.Code, rec.Body.String())
	}
	return etagOf(rec)
}

func (s *partyStack) visibilityETag(t *testing.T, token string, partyID uuid.UUID) string {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/v1/parties/"+partyID.String()+"/visibility/me", token, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET visibility = %d: %s", rec.Code, rec.Body.String())
	}
	return etagOf(rec)
}

func (s *partyStack) invite(t *testing.T, token string, partyID uuid.UUID, body string) (id, tok string) {
	t.Helper()
	out := s.must(t, http.StatusCreated, http.MethodPost, "/v1/parties/"+partyID.String()+"/invites", token, "", body)
	inv := out["invite"].(map[string]any)
	return inv["id"].(string), inv["token"].(string)
}

func (s *partyStack) setVisibility(t *testing.T, token string, partyID uuid.UUID, level string) {
	t.Helper()
	rec := s.req(t, http.MethodPatch, "/v1/parties/"+partyID.String()+"/visibility/me", token, uuid.NewString(),
		s.visibilityETag(t, token, partyID), `{"visibility_level":"`+level+`","share_location":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("visibility %s = %d: %s", level, rec.Code, rec.Body.String())
	}
}

func TestPartySyncIncrementalMatchesBootstrap(t *testing.T) {
	s := newPartyStack(t)
	retriesBefore := mutation.Retries()

	a := s.newUser(t, "a")
	b := s.newUser(t, "b")
	c := s.newUser(t, "c")
	for _, u := range []user{a, b, c} {
		s.seedReadyCalendar(t, u.id)
	}
	a1Sess := s.signIn(t, a.subject)
	a2Sess := s.signIn(t, a.subject)
	b1Sess := s.signIn(t, b.subject)
	c1Sess := s.signIn(t, c.subject)
	aTok, bTok, cTok := a1Sess.token, b1Sess.token, c1Sess.token

	a1 := s.newClient(t, "A1", aTok, 0)
	// A2는 같은 사용자의 두 번째 기기다. 작은 페이지(limit=2)로 읽어 트랜잭션 중간의 페이지 경계를 지나고,
	// 몇 단계씩 건너뛰며 읽어 여러 전이를 한꺼번에 받는다.
	a2 := s.newClient(t, "A2", a2Sess.token, 2)
	b1 := s.newClient(t, "B1", bTok, 0)
	c1 := s.newClient(t, "C1", cTok, 0)
	all := []*client{a1, b1, c1}

	// C의 다른 Party. C가 P를 떠나도 남아 있어야 한다.
	q := uuid.MustParse(s.must(t, http.StatusCreated, http.MethodPost, "/v1/parties", cTok, "", `{"name":"Q"}`)["party"].(map[string]any)["id"].(string))

	// 1. A가 P를 만든다.
	p := uuid.MustParse(s.must(t, http.StatusCreated, http.MethodPost, "/v1/parties", aTok, "", `{"name":"P"}`)["party"].(map[string]any)["id"].(string))
	s.converge(t, "create", all...)
	a1.expect(t, "create", p, map[string]int{"party": 1, "party_membership": 1, "party_visibility_setting": 1, "party_schedule_projection": 1})
	b1.expect(t, "create", p, nil)

	// 2. 1회용 초대 → B 수락. 초대가 소진되므로 방장 모델에서 사라져야 한다.
	_, tok1 := s.invite(t, aTok, p, `{"max_uses":1}`)
	s.converge(t, "invite", a1)
	a1.expect(t, "invite", p, map[string]int{"party": 1, "party_membership": 1, "party_visibility_setting": 1, "party_schedule_projection": 1, "party_invite": 1})
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+tok1+"/accept", bTok, "", "")
	s.converge(t, "accept B", all...)
	// 신규 멤버: Party 단위 부트스트랩(party, 모든 멤버십, 본인 설정, 허용된 projection).
	b1.expect(t, "accept B", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})
	a1.expect(t, "accept B", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})
	s.converge(t, "accept B (A2)", a2)

	// 3. B의 공개 수준 busyOnly → hidden → busyOnly. versioned tombstone 뒤 새 ID projection이 온다.
	s.setVisibility(t, bTok, p, "hidden")
	s.converge(t, "hidden", all...)
	a1.expect(t, "hidden", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 1})
	s.setVisibility(t, bTok, p, "busyOnly")
	s.converge(t, "busyOnly", all...)
	a1.expect(t, "busyOnly", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})

	// 3-1. B의 표시 이름 변경. 같은 Party 멤버는 멤버십 투영의 display_name으로 받는다.
	me := s.req(t, http.MethodGet, "/v1/me", bTok, "", "", "")
	s.must(t, http.StatusOK, http.MethodPatch, "/v1/me", bTok, etagOf(me), `{"display_name":"B 새 이름"}`)
	s.converge(t, "rename B", all...)
	if got := memberName(a1, b.id); got != "B 새 이름" {
		t.Fatalf("A1이 본 B 이름 = %q", got)
	}

	// 4. 이름 변경, 초대 2개 생성 후 1개 무효화. 초대는 방장(A)만 본다.
	s.must(t, http.StatusOK, http.MethodPatch, "/v1/parties/"+p.String(), aTok, s.partyETag(t, aTok, p), `{"name":"P2"}`)
	_, _ = s.invite(t, aTok, p, "")
	inv3, _ := s.invite(t, aTok, p, "")
	s.must(t, http.StatusOK, http.MethodDelete, "/v1/parties/"+p.String()+"/invites/"+inv3, aTok, "", "")
	s.converge(t, "rename+invites", all...)
	a1.expect(t, "rename+invites", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2, "party_invite": 1})
	b1.expect(t, "rename+invites", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})

	// B의 두 번째 기기는 여기서 처음 bootstrap한다(늦게 들어온 세션).
	b2 := s.newClient(t, "B2", s.signIn(t, b.subject).token, 0)

	// 5. 방장 위임 A → B. 활성 초대는 무효화되고 tombstone은 이전·새 방장에게만 간다.
	s.must(t, http.StatusOK, http.MethodPost, "/v1/parties/"+p.String()+"/owner", aTok, s.partyETag(t, aTok, p), `{"user_id":"`+b.id.String()+`"}`)
	s.converge(t, "transfer", all...)
	a1.expect(t, "transfer", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})

	// 6. 새 방장 B가 초대 → C 가입. 초대 upsert는 B만 받는다.
	_, tok4 := s.invite(t, bTok, p, `{"max_uses":5}`)
	s.converge(t, "invite by B", a1, b1, b2)
	b1.expect(t, "invite by B", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2, "party_invite": 1})
	a1.expect(t, "invite by B", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+tok4+"/accept", cTok, "", "")
	s.converge(t, "accept C", all...)
	c1.expect(t, "accept C", p, map[string]int{"party": 1, "party_membership": 3, "party_visibility_setting": 1, "party_schedule_projection": 3})
	s.converge(t, "accept C (A2)", a2)

	// 7. C 탈퇴 → C 모델에서 P가 완전히 사라진다. Q는 남는다.
	s.must(t, http.StatusOK, http.MethodDelete, "/v1/parties/"+p.String()+"/memberships/me", cTok, "", "")
	s.converge(t, "leave C", all...)
	c1.expect(t, "leave C", p, nil)
	c1.expect(t, "leave C", q, map[string]int{"party": 1, "party_membership": 1, "party_visibility_setting": 1, "party_schedule_projection": 1})
	a1.expect(t, "leave C", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 2})

	// 8. C 재가입. 탈퇴 때 받은 version 없는 tombstone이 하한을 남기지 않아 같은 Party가 다시 들어온다.
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+tok4+"/accept", cTok, "", "")
	s.converge(t, "rejoin C", all...)
	c1.expect(t, "rejoin C", p, map[string]int{"party": 1, "party_membership": 3, "party_visibility_setting": 1, "party_schedule_projection": 3})

	// 9. 방장 B가 C 강퇴.
	s.must(t, http.StatusOK, http.MethodDelete, "/v1/parties/"+p.String()+"/memberships/"+c.id.String(), bTok, s.partyETag(t, bTok, p), "")
	s.converge(t, "remove C", all...)
	c1.expect(t, "remove C", p, nil)
	s.converge(t, "remove C (late)", a2, b2)

	// 10. 해산. 남은 멤버 모두에게서 P가 사라진다. 방장의 초대도 사라진다.
	s.must(t, http.StatusOK, http.MethodPost, "/v1/parties/"+p.String()+"/disband", bTok, s.partyETag(t, bTok, p), "")
	s.converge(t, "disband", append(all, a2, b2)...)
	for _, cl := range append(all, a2, b2) {
		cl.expect(t, "disband", p, nil)
	}
	c1.expect(t, "disband", q, map[string]int{"party": 1, "party_membership": 1, "party_visibility_setting": 1, "party_schedule_projection": 1})

	if a2.pulled == 0 || b2.pulled == 0 {
		t.Fatalf("늦은 세션이 증분을 받지 않았다: A2 %d, B2 %d", a2.pulled, b2.pulled)
	}
	if d := mutation.Retries() - retriesBefore; d != 0 {
		t.Fatalf("mutation 재시도 %d회: 잠금 규약 위반", d)
	}
}

// 이름 변경(users → parties FOR SHARE)과 같은 Party 가입(users → parties FOR UPDATE)이 동시에 일어나도 교착·재시도가
// 없고, 신규 멤버가 결국 새 이름을 본다.
func TestDisplayNameRacingAcceptConverges(t *testing.T) {
	s := newPartyStack(t)
	retriesBefore := mutation.Retries()
	a := s.newUser(t, "race.a")
	aTok := s.signIn(t, a.subject).token
	p := uuid.MustParse(s.must(t, http.StatusCreated, http.MethodPost, "/v1/parties", aTok, "", `{"name":"R"}`)["party"].(map[string]any)["id"].(string))
	_, tok := s.invite(t, aTok, p, `{"max_uses":10}`)

	const n = 5
	joiners := make([]string, n)
	clients := make([]*client, n)
	for i := range joiners {
		joiners[i] = s.signIn(t, s.newUser(t, "race.j"+strconv.Itoa(i)).subject).token
		// 가입 전에 bootstrap해 둔다. 이름은 증분으로만 받는다.
		clients[i] = s.newClient(t, "J"+strconv.Itoa(i), joiners[i], 0)
		clients[i].strictOrder = false
	}
	final := ""
	for i := 0; i < n; i++ {
		me := s.req(t, http.MethodGet, "/v1/me", aTok, "", "", "")
		final = "A" + strconv.Itoa(i)
		done := make(chan *httptest.ResponseRecorder, 2)
		go func() {
			done <- s.req(t, http.MethodPatch, "/v1/me", aTok, uuid.NewString(), etagOf(me), `{"display_name":"`+final+`"}`)
		}()
		go func(tk string) {
			done <- s.req(t, http.MethodPost, "/v1/invites/"+tok+"/accept", tk, uuid.NewString(), "", "")
		}(joiners[i])
		for range 2 {
			if rec := <-done; rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
				t.Fatalf("동시 요청 = %d: %s", rec.Code, rec.Body.String())
			}
		}
	}
	for i, cl := range clients {
		s.converge(t, "race", cl)
		if got := memberName(cl, a.id); got != final {
			t.Fatalf("J%d가 본 A 이름 = %q, want %q", i, got, final)
		}
	}
	if d := mutation.Retries() - retriesBefore; d != 0 {
		t.Fatalf("mutation 재시도 %d회", d)
	}
}

// ---- 동시 실행 순서 역전 재현 ----
//
// txid는 멱등성 INSERT(트랜잭션 첫 쓰기)에서 배정되고 Party 잠금은 그 뒤다. 요청 기기의 devices row를 FOR UPDATE로
// 잡아 두면 그 INSERT는 heap에 쓴 뒤(txid 배정) FK 검사(KEY SHARE)에서 멈춘다. 그 사이 다른 요청을 끝까지 실행하면
// "txid는 작은데 나중에 직렬화된" 트랜잭션을 결정적으로 만든다. 운영의 스케줄링 지연과 같은 순서다.

type blocker struct {
	tx   pgx.Tx
	pool *pgxpool.Pool
	pid  int32
}

func (s *partyStack) blockDevice(t *testing.T, deviceID uuid.UUID) *blocker {
	t.Helper()
	tx, err := s.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `SELECT id FROM devices WHERE id = $1 FOR UPDATE`, deviceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return &blocker{tx: tx, pool: s.pool, pid: pid}
}

// waitBlocked는 이 blocker를 기다리는 멱등성 INSERT가 생길 때까지 기다린다. 다른 패키지의 잠금 대기와 섞이지
// 않게 pg_blocking_pids로 거른다.
func (b *blocker) waitBlocked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := b.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND query LIKE '%INSERT INTO idempotency_keys%'
			   AND $1 = ANY(pg_blocking_pids(pid))`, b.pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("요청이 devices 잠금에서 멈추지 않았다")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (b *blocker) release(t *testing.T) {
	t.Helper()
	if err := b.tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// async는 요청을 고루틴에서 보내고 결과를 채널로 준다.
func (s *partyStack) async(t *testing.T, method, path, token, ifMatch, body string) <-chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	key := uuid.NewString()
	go func() { ch <- s.req(t, method, path, token, key, ifMatch, body) }()
	return ch
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
}

// maxTxid는 수신자에게 간 entity 변경 중 가장 큰 txid다. 순서 역전이 실제로 일어났는지 확인한다.
func (s *partyStack) txidOf(t *testing.T, recipient uuid.UUID, entityID uuid.UUID, op string) uint64 {
	t.Helper()
	var txid string
	if err := s.pool.QueryRow(context.Background(), `
		SELECT max(txid)::text FROM sync_changes
		 WHERE recipient_user_id = $1 AND entity_id = $2 AND operation = $3`, recipient, entityID, op).Scan(&txid); err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseUint(txid, 10, 64)
	if err != nil {
		t.Fatalf("txid %q: %v", txid, err)
	}
	return v
}

func (s *partyStack) signInDevice(t *testing.T, subject string) (string, uuid.UUID) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"identity_token": subject, "authorization_code": "c", "raw_nonce": "n", "display_name": "P6"})
	rec := s.req(t, http.MethodPost, "/v1/auth/apple", "", "", "", string(b))
	if rec.Code != http.StatusOK {
		t.Fatalf("로그인 = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		DeviceID    string `json:"device_id"`
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.AccessToken, uuid.MustParse(body.DeviceID)
}

func (s *partyStack) createParty(t *testing.T, token, name string) uuid.UUID {
	t.Helper()
	return uuid.MustParse(s.must(t, http.StatusCreated, http.MethodPost, "/v1/parties", token, "", `{"name":"`+name+`"}`)["party"].(map[string]any)["id"].(string))
}

// drain은 그 수신자에게 기록된 변경을 모두 받을 때까지 읽는다. converge는 bootstrap과 같아지는 즉시 끝나므로
// 다른 패키지의 열린 트랜잭션이 horizon을 붙잡아 일부만 받은 상태에서 대조군 단언을 하면 흔들린다.
func (s *partyStack) drain(t *testing.T, c *client) {
	t.Helper()
	recipient := c.self
	var maxTx string
	var maxOrd int32
	if err := s.pool.QueryRow(context.Background(), `
		SELECT txid::text, ordinal FROM sync_changes WHERE recipient_user_id = $1
		 ORDER BY txid DESC, ordinal DESC LIMIT 1`, recipient).Scan(&maxTx, &maxOrd); err != nil {
		t.Fatal(err)
	}
	last, err := strconv.ParseUint(maxTx, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.pull(t, c)
		cur, err := DecodeCursor(c.cursor)
		if err != nil {
			t.Fatal(err)
		}
		if cur.TxID > last || (cur.TxID == last && cur.Ordinal >= maxOrd) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: 10초 안에 모든 변경을 받지 못했다", c.name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// convergeShadowDiverges는 모든 변경을 받은 뒤 규칙을 켠 모델이 bootstrap과 같고 대조군은 다르다는 것을 확인한다.
func (s *partyStack) convergeShadowDiverges(t *testing.T, step string, c *client) {
	t.Helper()
	s.drain(t, c)
	s.converge(t, step, c)
	fresh, _ := s.fetchBootstrap(t, c.token)
	if diffModels(c.shadow.model, fresh) == "" {
		t.Fatalf("[%s] 규칙 (a)(b) 없이도 bootstrap과 같다. 재현이 순서 역전을 만들지 못했다", step)
	}
}

// 재현 1: 탈퇴 L이 먼저 txid를 받고, 방장의 Party 이름 변경 E가 먼저 잠금·커밋한다. 탈퇴자 피드는 L의 version 없는
// party tombstone 뒤에 E의 party upsert가 온다. 규칙 (a)가 없으면 탈퇴자 모델에 Party가 되살아난다.
func TestLeaveRacingPartyChangeDoesNotResurrect(t *testing.T) {
	s := newPartyStack(t)
	retriesBefore := mutation.Retries()
	a, u := s.newUser(t, "r1.a"), s.newUser(t, "r1.u")
	s.seedReadyCalendar(t, a.id)
	aTok := s.signIn(t, a.subject).token
	uTok, uDev := s.signInDevice(t, u.subject)
	p := s.createParty(t, aTok, "R1")
	_, inv := s.invite(t, aTok, p, "")
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", uTok, "", "")
	uc := s.newClient(t, "U", uTok, 0).withShadow()
	uc.strictOrder = false

	b := s.blockDevice(t, uDev)
	leave := s.async(t, http.MethodDelete, "/v1/parties/"+p.String()+"/memberships/me", uTok, "", "")
	b.waitBlocked(t)
	s.must(t, http.StatusOK, http.MethodPatch, "/v1/parties/"+p.String(), aTok, s.partyETag(t, aTok, p), `{"name":"R1b"}`)
	// 남은 멤버의 표시 이름 변경도 탈퇴자에게 멤버십 upsert를 보낸다(리뷰 Low: §4 범위 확대).
	me := s.req(t, http.MethodGet, "/v1/me", aTok, "", "", "")
	s.must(t, http.StatusOK, http.MethodPatch, "/v1/me", aTok, etagOf(me), `{"display_name":"A 새 이름"}`)
	b.release(t)
	expectStatus(t, <-leave, http.StatusOK)

	if l, e := s.txidOf(t, u.id, p, "tombstone"), s.txidOf(t, u.id, p, "upsert"); l >= e {
		t.Fatalf("순서 역전이 일어나지 않았다: tombstone txid %d, upsert txid %d", l, e)
	}
	s.convergeShadowDiverges(t, "leave vs rename", uc)
	uc.expect(t, "leave vs rename", p, nil)
	if d := mutation.Retries() - retriesBefore; d != 0 {
		t.Fatalf("mutation 재시도 %d회", d)
	}
}

// 재현 2: 위임 T가 먼저 txid를 받고, 동시 수락 E가 먼저 잠금·커밋한다. 옛 방장 피드는 T의 초대 tombstone 뒤에 E의
// 초대 upsert가 온다. 초대 tombstone의 version(마지막 upsert + 1)이 하한으로 그 upsert를 막는다.
func TestTransferRacingAcceptDoesNotResurrectInvite(t *testing.T) {
	s := newPartyStack(t)
	retriesBefore := mutation.Retries()
	a, b2, j := s.newUser(t, "r2.a"), s.newUser(t, "r2.b"), s.newUser(t, "r2.j")
	aTok, aDev := s.signInDevice(t, a.subject)
	bTok := s.signIn(t, b2.subject).token
	jTok := s.signIn(t, j.subject).token
	p := s.createParty(t, aTok, "R2")
	_, inv1 := s.invite(t, aTok, p, "")
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv1+"/accept", bTok, "", "")
	invID, inv2 := s.invite(t, aTok, p, `{"max_uses":5}`)
	ac := s.newClient(t, "A", aTok, 0).withFloorlessShadow()
	ac.strictOrder = false
	bc := s.newClient(t, "B", bTok, 0)
	bc.strictOrder = false

	// 수락이 Party version을 1 올리므로 위임은 그 다음 version을 If-Match로 보낸다.
	v, _ := strconv.ParseInt(strings.Trim(s.partyETag(t, aTok, p), `"`), 10, 64)
	blk := s.blockDevice(t, aDev)
	transfer := s.async(t, http.MethodPost, "/v1/parties/"+p.String()+"/owner", aTok, `"`+strconv.FormatInt(v+1, 10)+`"`, `{"user_id":"`+b2.id.String()+`"}`)
	blk.waitBlocked(t)
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv2+"/accept", jTok, "", "")
	blk.release(t)
	expectStatus(t, <-transfer, http.StatusOK)

	id := uuid.MustParse(invID)
	if tomb, up := s.txidOf(t, a.id, id, "tombstone"), s.txidOf(t, a.id, id, "upsert"); tomb >= up {
		t.Fatalf("순서 역전이 일어나지 않았다: tombstone txid %d, upsert txid %d", tomb, up)
	}
	// 옛 방장은 여전히 활성 멤버라 규칙 (a)(b)는 관여하지 않는다. 막는 것은 초대 tombstone의 version 하한이다.
	s.convergeShadowDiverges(t, "transfer vs accept", ac)
	s.converge(t, "transfer vs accept", bc)
	ac.expect(t, "transfer vs accept", p, map[string]int{"party": 1, "party_membership": 3, "party_visibility_setting": 1})
	if d := mutation.Retries() - retriesBefore; d != 0 {
		t.Fatalf("mutation 재시도 %d회", d)
	}
}

// 재현 3: 같은 사용자의 재가입 T3가 먼저 txid를 받고, 다른 기기의 탈퇴 L이 먼저 잠금·커밋한다. 피드는 T3의 Party 단위
// 부트스트랩 뒤에 L의 version 없는 scope tombstone이 온다. 규칙 (b)가 없으면 재가입했는데 모델이 비어 버린다.
func TestRejoinRacingLeaveKeepsNewScope(t *testing.T) {
	s := newPartyStack(t)
	retriesBefore := mutation.Retries()
	a, u := s.newUser(t, "r3.a"), s.newUser(t, "r3.u")
	s.seedReadyCalendar(t, a.id)
	aTok := s.signIn(t, a.subject).token
	u1Tok, u1Dev := s.signInDevice(t, u.subject)
	u2Tok := s.signIn(t, u.subject).token
	p := s.createParty(t, aTok, "R3")
	_, inv := s.invite(t, aTok, p, `{"max_uses":5}`)
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", u1Tok, "", "")
	uc := s.newClient(t, "U", u2Tok, 0).withShadow()
	uc.strictOrder = false

	blk := s.blockDevice(t, u1Dev)
	rejoin := s.async(t, http.MethodPost, "/v1/invites/"+inv+"/accept", u1Tok, "", "")
	blk.waitBlocked(t)
	s.must(t, http.StatusOK, http.MethodDelete, "/v1/parties/"+p.String()+"/memberships/me", u2Tok, "", "")
	blk.release(t)
	expectStatus(t, <-rejoin, http.StatusCreated)

	if up, tomb := s.txidOf(t, u.id, p, "upsert"), s.txidOf(t, u.id, p, "tombstone"); up >= tomb {
		t.Fatalf("순서 역전이 일어나지 않았다: 재가입 upsert txid %d, 탈퇴 tombstone txid %d", up, tomb)
	}
	s.convergeShadowDiverges(t, "rejoin vs leave", uc)
	uc.expect(t, "rejoin vs leave", p, map[string]int{"party": 1, "party_membership": 2, "party_visibility_setting": 1, "party_schedule_projection": 1})
	if d := mutation.Retries() - retriesBefore; d != 0 {
		t.Fatalf("mutation 재시도 %d회", d)
	}
}

// 이름 변경 R이 먼저 txid를 받고 가입 T3가 먼저 커밋한다. 가입자 피드는 R의 멤버십 upsert(새 이름, 부모 Party보다
// 먼저)가 T3 부트스트랩보다 앞에 온다. 부모 없는 사본을 버리지 않고 version으로 적용하면 새 이름으로 수렴한다.
func TestRenameRacingJoinDeliversNewName(t *testing.T) {
	s := newPartyStack(t)
	retriesBefore := mutation.Retries()
	a, j := s.newUser(t, "r4.a"), s.newUser(t, "r4.j")
	aTok, aDev := s.signInDevice(t, a.subject)
	jTok := s.signIn(t, j.subject).token
	p := s.createParty(t, aTok, "R4")
	_, inv := s.invite(t, aTok, p, "")
	jc := s.newClient(t, "J", jTok, 0)
	jc.strictOrder = false

	me := s.req(t, http.MethodGet, "/v1/me", aTok, "", "", "")
	blk := s.blockDevice(t, aDev)
	rename := s.async(t, http.MethodPatch, "/v1/me", aTok, etagOf(me), `{"display_name":"A 늦은 이름"}`)
	blk.waitBlocked(t)
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", jTok, "", "")
	blk.release(t)
	expectStatus(t, <-rename, http.StatusOK)

	var aMembership uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM party_memberships WHERE party_id = $1 AND user_id = $2`, p, a.id).Scan(&aMembership); err != nil {
		t.Fatal(err)
	}
	// 가입자는 A 멤버십 upsert를 두 번 받는다(가입 부트스트랩, 이름 변경). 높은 version 쪽이 이름 변경이다.
	var renameTxid string
	if err := s.pool.QueryRow(context.Background(), `
		SELECT txid::text FROM sync_changes WHERE recipient_user_id = $1 AND entity_id = $2 AND operation = 'upsert'
		 ORDER BY entity_version DESC LIMIT 1`, j.id, aMembership).Scan(&renameTxid); err != nil {
		t.Fatal(err)
	}
	r, _ := strconv.ParseUint(renameTxid, 10, 64)
	if join := s.txidOf(t, j.id, p, "upsert"); r >= join {
		t.Fatalf("순서 역전이 일어나지 않았다: 이름 변경 txid %d, 가입 txid %d", r, join)
	}
	s.converge(t, "rename vs join", jc)
	if got := memberName(jc, a.id); got != "A 늦은 이름" {
		t.Fatalf("J가 본 A 이름 = %q", got)
	}
	if d := mutation.Retries() - retriesBefore; d != 0 {
		t.Fatalf("mutation 재시도 %d회", d)
	}
}

// 이름이 그대로인 PATCH /v1/me는 version을 올리지 않고 다른 멤버에게 아무것도 보내지 않는다.
func TestSameDisplayNameIsNoop(t *testing.T) {
	s := newPartyStack(t)
	a, b := s.newUser(t, "same.a"), s.newUser(t, "same.b")
	aTok := s.signIn(t, a.subject).token
	bTok := s.signIn(t, b.subject).token
	p := s.createParty(t, aTok, "S")
	_, inv := s.invite(t, aTok, p, "")
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", bTok, "", "")

	count := func() int {
		var n int
		if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1`, b.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	me := s.req(t, http.MethodGet, "/v1/me", aTok, "", "", "")
	out := s.must(t, http.StatusOK, http.MethodPatch, "/v1/me", aTok, etagOf(me), `{"display_name":"P6"}`)
	if v := out["user"].(map[string]any)["version"].(float64); etagOf(me) != `"`+strconv.Itoa(int(v))+`"` {
		t.Fatalf("같은 이름인데 version이 바뀌었다: %s -> %v", etagOf(me), v)
	}
	if after := count(); after != before {
		t.Fatalf("같은 이름 PATCH가 다른 멤버에게 변경 %d건을 보냈다", after-before)
	}
}

type noRevoke struct{}

func (noRevoke) Revoke(context.Context, string) error { return nil }

// 계정 삭제 완료가 표시 이름을 익명화하면 같은 Party 멤버의 모델도 bootstrap과 같게 익명화된다.
// 멤버십 자체의 정리(강제 위임·해산)는 P8이다.
func TestAccountDeletionAnonymizesNameForCoMembers(t *testing.T) {
	s := newPartyStack(t)
	a, d := s.newUser(t, "del.a"), s.newUser(t, "del.d")
	aTok := s.signIn(t, a.subject).token
	dTok := s.signIn(t, d.subject).token
	p := s.createParty(t, aTok, "D")
	_, inv := s.invite(t, aTok, p, "")
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", dTok, "", "")
	ac := s.newClient(t, "A", aTok, 0)

	me := s.req(t, http.MethodGet, "/v1/me", dTok, "", "", "")
	if rec := s.req(t, http.MethodDelete, "/v1/me", dTok, uuid.NewString(), etagOf(me), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("삭제 요청 = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE users SET deletion_requested_at = now() - interval '25 hours' WHERE id = $1`, d.id); err != nil {
		t.Fatal(err)
	}
	svc, err := account.NewDeletionService(account.DeletionDeps{Pool: s.pool, Opener: s.box, Apple: noRevoke{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleJob(context.Background(), jobs.Job{Type: account.AccountDeletionJobType,
		Payload: []byte(`{"user_id":"` + d.id.String() + `"}`), Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	s.converge(t, "deletion", ac)
	if got := memberName(ac, d.id); got != "탈퇴한 사용자" {
		t.Fatalf("A가 본 삭제 사용자 이름 = %q", got)
	}
}

// 재현 5: 탈퇴 L이 먼저 txid를 받고 방장 이름 변경 E가 먼저 커밋한 뒤, 그 사이 txid를 받은 열린 트랜잭션이 horizon을
// 붙잡은 채 탈퇴자가 새로 bootstrap한다. snapshot은 탈퇴 뒤 상태(Party 없음)인데 E는 watermark 뒤라 다시 온다.
// snapshot이 끝난 본인 멤버십 하한을 실어야 규칙 (a)가 E를 버린다.
func TestBootstrapCarriesEndedMembershipFloors(t *testing.T) {
	s := newPartyStack(t)
	a, u := s.newUser(t, "r5.a"), s.newUser(t, "r5.u")
	aTok := s.signIn(t, a.subject).token
	uTok, uDev := s.signInDevice(t, u.subject)
	p := s.createParty(t, aTok, "R5")
	_, inv := s.invite(t, aTok, p, "")
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv+"/accept", uTok, "", "")

	blk := s.blockDevice(t, uDev)
	leave := s.async(t, http.MethodDelete, "/v1/parties/"+p.String()+"/memberships/me", uTok, "", "")
	blk.waitBlocked(t)
	// L 다음 txid를 받아 열어 두는 트랜잭션. E는 그 뒤 txid다.
	hold, err := s.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err := hold.Exec(context.Background(), `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	s.must(t, http.StatusOK, http.MethodPatch, "/v1/parties/"+p.String(), aTok, s.partyETag(t, aTok, p), `{"name":"R5b"}`)
	blk.release(t)
	expectStatus(t, <-leave, http.StatusOK)

	// 다른 패키지의 더 오래된 트랜잭션이 열려 있으면 watermark가 L 아래로 내려가 L이 다시 오고 스스로 하한을
	// 남긴다. 그러면 재현이 되지 않으므로 watermark가 L 뒤일 때까지 다시 bootstrap한다.
	leaveTx := s.txidOf(t, u.id, p, "tombstone")
	var uc, noFloor *client
	deadline := time.Now().Add(10 * time.Second)
	for {
		uc = s.newClient(t, "U", uTok, 0)
		noFloor = s.newClient(t, "U(snapshot 하한 없음)", uTok, 0)
		cu, _ := DecodeCursor(uc.cursor)
		cn, _ := DecodeCursor(noFloor.cursor)
		if cu.TxID > leaveTx && cn.TxID > leaveTx {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watermark가 탈퇴 txid를 넘지 않았다")
		}
		time.Sleep(20 * time.Millisecond)
	}
	uc.strictOrder = false
	noFloor.strictOrder = false
	noFloor.floors = map[entityKey]int64{}
	if len(uc.floors) == 0 || uc.count(p)["party"] != 0 {
		t.Fatalf("snapshot이 탈퇴 뒤 상태와 하한을 싣지 않았다: floors %v, entity %v", uc.floors, uc.count(p))
	}
	if err := hold.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.drain(t, uc)
	s.converge(t, "bootstrap floors", uc)
	uc.expect(t, "bootstrap floors", p, nil)
	s.drain(t, noFloor)
	if noFloor.count(p)["party"] != 1 {
		t.Fatalf("재현 실패: 하한 없는 모델에도 Party가 되살아나지 않았다 %v", noFloor.count(p))
	}
}

// 재현 6: 위임 T가 먼저 txid를 받고 수락 E가 먼저 커밋한 뒤, 그 사이 txid를 받은 열린 트랜잭션이 horizon을 붙잡은
// 채 옛 방장이 새로 bootstrap한다. T의 초대 tombstone은 watermark 아래라 다시 오지 않고 E의 초대 upsert는 다시 온다.
// 옛 방장은 활성 멤버라 규칙 (a)가 관여하지 않는다. snapshot이 그 초대의 하한을 실어야 되살아나지 않는다.
func TestBootstrapCarriesTombstoneFloorsForRedeliveredUpserts(t *testing.T) {
	s := newPartyStack(t)
	a, b2, j := s.newUser(t, "r6.a"), s.newUser(t, "r6.b"), s.newUser(t, "r6.j")
	aTok, aDev := s.signInDevice(t, a.subject)
	bTok := s.signIn(t, b2.subject).token
	jTok := s.signIn(t, j.subject).token
	p := s.createParty(t, aTok, "R6")
	_, inv1 := s.invite(t, aTok, p, "")
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv1+"/accept", bTok, "", "")
	invID, inv2 := s.invite(t, aTok, p, `{"max_uses":5}`)

	v, _ := strconv.ParseInt(strings.Trim(s.partyETag(t, aTok, p), `"`), 10, 64)
	blk := s.blockDevice(t, aDev)
	transfer := s.async(t, http.MethodPost, "/v1/parties/"+p.String()+"/owner", aTok, `"`+strconv.FormatInt(v+1, 10)+`"`, `{"user_id":"`+b2.id.String()+`"}`)
	blk.waitBlocked(t)
	hold, err := s.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err := hold.Exec(context.Background(), `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	s.must(t, http.StatusCreated, http.MethodPost, "/v1/invites/"+inv2+"/accept", jTok, "", "")
	blk.release(t)
	expectStatus(t, <-transfer, http.StatusOK)

	id := uuid.MustParse(invID)
	tombTx := s.txidOf(t, a.id, id, "tombstone")
	var ac, noFloor *client
	deadline := time.Now().Add(10 * time.Second)
	for {
		ac = s.newClient(t, "A", aTok, 0)
		noFloor = s.newClient(t, "A(snapshot 하한 없음)", aTok, 0)
		ca, _ := DecodeCursor(ac.cursor)
		cn, _ := DecodeCursor(noFloor.cursor)
		if ca.TxID > tombTx && cn.TxID > tombTx {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watermark가 위임 txid를 넘지 않았다")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ac.strictOrder, noFloor.strictOrder = false, false
	noFloor.floors = map[entityKey]int64{}
	if _, ok := ac.floors[entityKey{"party_invite", id}]; !ok {
		t.Fatalf("snapshot에 초대 하한이 없다: %v", ac.floors)
	}
	if err := hold.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.drain(t, ac)
	s.converge(t, "bootstrap tombstone floors", ac)
	ac.expect(t, "bootstrap tombstone floors", p, map[string]int{"party": 1, "party_membership": 3, "party_visibility_setting": 1})
	s.drain(t, noFloor)
	if noFloor.count(p)["party_invite"] != 1 {
		t.Fatalf("재현 실패: 하한 없는 모델에도 초대가 되살아나지 않았다 %v", noFloor.count(p))
	}
}
