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
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/account"
	"plantogether/server/internal/auth"
	"plantogether/server/internal/party"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
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
	return &partyStack{h: httpapi.Wrap(logger, mux), pool: pool}
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

// client는 기기 하나(세션 하나)의 로컬 저장소다. iOS 계약을 그대로 따른다.
//   - upsert는 version이 로컬 version과 versioned tombstone이 남긴 하한보다 클 때만 적용한다.
//   - version 있는 tombstone은 지우고 하한을 기록한다.
//   - version 없는 tombstone은 지우기만 한다(재가입으로 같은 ID·version이 다시 올 수 있다).
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
}

type wireChange struct {
	EntityType string          `json:"entity_type"`
	EntityID   uuid.UUID       `json:"entity_id"`
	Operation  string          `json:"operation"`
	Version    *int64          `json:"version"`
	Payload    json.RawMessage `json:"payload"`
}

func (s *partyStack) fetchBootstrap(t *testing.T, token string) (map[entityKey]entity, string) {
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
	for _, c := range body.Snapshot {
		if !partyEntityTypes[c.EntityType] {
			continue
		}
		if c.Operation != "upsert" || c.Version == nil {
			t.Fatalf("bootstrap entity가 upsert가 아니다: %+v", c)
		}
		out[entityKey{c.EntityType, c.EntityID}] = entity{version: *c.Version, payload: decodePayload(t, c.Payload)}
	}
	return out, body.Watermark
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
	c := &client{name: name, token: token, limit: limit, strictOrder: true}
	c.model, c.cursor = s.fetchBootstrap(t, token)
	c.floors = map[entityKey]int64{}
	return c
}

func (c *client) apply(t *testing.T, ch wireChange) {
	t.Helper()
	if !partyEntityTypes[ch.EntityType] {
		return
	}
	k := entityKey{ch.EntityType, ch.EntityID}
	switch ch.Operation {
	case "upsert":
		if ch.Version == nil {
			t.Fatalf("%s: version 없는 upsert %+v", c.name, ch)
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
		if ch.Version != nil {
			if cur, ok := c.model[k]; ok && cur.version > *ch.Version {
				return // 더 새로운 upsert가 이미 적용됐다.
			}
			if *ch.Version > c.floors[k] {
				c.floors[k] = *ch.Version
			}
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
