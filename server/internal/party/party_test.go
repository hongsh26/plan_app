package party

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/postgres/pgtest"
)

type subjectApple struct{}

func (subjectApple) Verify(_ context.Context, idToken, _ string) (appleid.Identity, error) {
	return appleid.Identity{Subject: idToken}, nil
}

type stack struct {
	handler http.Handler
	party   *Handler
	pool    *pgxpool.Pool
	logs    *bytes.Buffer
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	tokens, err := auth.NewAccessTokens(bytes.Repeat([]byte{4}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(auth.Deps{Pool: pool, Apple: subjectApple{}, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	authHandler := auth.NewHandler(svc, logger)
	mux := http.NewServeMux()
	authHandler.Register(mux)
	partyHandler := NewHandler(Deps{Pool: pool, Logger: logger})
	partyHandler.Register(mux, authHandler.Require)
	return &stack{handler: httpapi.Wrap(logger, mux), party: partyHandler, pool: pool, logs: &logs}
}

func (s *stack) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func (s *stack) mut(t *testing.T, method, path, token, key, ifMatch, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set(mutationKeyHeader, key)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

const mutationKeyHeader = "Idempotency-Key"

type sessionBody struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
}

func (s *stack) signIn(t *testing.T, subject string) sessionBody {
	t.Helper()
	rec := s.do(t, http.MethodPost, "/v1/auth/apple", "", map[string]string{
		"identity_token": subject, "authorization_code": "c", "raw_nonce": "n", "display_name": "Party Tester",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("로그인 status = %d: %s", rec.Code, rec.Body.String())
	}
	body := decode[sessionBody](t, rec)
	uid := uuid.MustParse(body.UserID)
	t.Cleanup(func() { cleanupUser(t, s.pool, uid) })
	return body
}

func cleanupUser(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("cleanup tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM sync_changes WHERE recipient_user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT party_id FROM party_memberships WHERE user_id = $1`, userID)
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
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	for _, partyID := range partyIDs {
		if _, err := tx.Exec(ctx, `DELETE FROM party_schedule_projections WHERE party_id = $1`, partyID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM party_visibility_settings WHERE party_id = $1`, partyID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE party_memberships SET invite_id = NULL WHERE party_id = $1`, partyID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM party_invites WHERE party_id = $1`, partyID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM party_memberships WHERE party_id = $1`, partyID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM parties WHERE id = $1`, partyID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("응답이 JSON이 아니다: %v (%s)", err, rec.Body.String())
	}
	return v
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	e := decode[httpapi.ErrorResponse](t, rec)
	if e.Code != code {
		t.Fatalf("code = %q, want %q", e.Code, code)
	}
}

func TestCreateGetPatchAndListParty(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "party."+uuid.NewString())

	rec := s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "create-1", "",
		`{"name":"  Café   Party  "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/parties status = %d: %s", rec.Code, rec.Body.String())
	}
	created := decode[partyEnvelope](t, rec)
	if created.Party.Name != "Café Party" {
		t.Fatalf("name = %q", created.Party.Name)
	}
	if rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("ETag = %q", rec.Header().Get("ETag"))
	}

	replay := s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "create-1", "",
		`{"name":"  Café   Party  "}`)
	if replay.Code != http.StatusCreated || replay.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("replay status/header = %d/%q: %s", replay.Code, replay.Header().Get(ReplayedHeader), replay.Body.String())
	}

	assertCount(t, s.pool, `SELECT count(*) FROM parties WHERE created_by_user_id = $1`, sess.UserID, 1)
	assertCount(t, s.pool, `SELECT count(*) FROM party_memberships WHERE party_id = $1 AND role = 'owner' AND status = 'active'`, created.Party.ID, 1)
	assertCount(t, s.pool, `SELECT count(*) FROM party_visibility_settings WHERE party_id = $1 AND visibility_level = 'busyOnly'`, created.Party.ID, 1)
	assertCount(t, s.pool, `SELECT count(*) FROM party_schedule_projections WHERE party_id = $1`, created.Party.ID, 0)
	assertCount(t, s.pool, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1`, sess.UserID, 3)

	get := s.do(t, http.MethodGet, "/v1/parties/"+created.Party.ID.String(), sess.AccessToken, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET party status = %d: %s", get.Code, get.Body.String())
	}

	list := s.do(t, http.MethodGet, "/v1/parties/"+created.Party.ID.String()+"/memberships", sess.AccessToken, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("GET memberships status = %d: %s", list.Code, list.Body.String())
	}
	members := decode[membershipsEnvelope](t, list)
	if len(members.Memberships) != 1 || members.Memberships[0].Role != "owner" {
		t.Fatalf("memberships = %+v", members.Memberships)
	}

	patch := s.mut(t, http.MethodPatch, "/v1/parties/"+created.Party.ID.String(), sess.AccessToken, "patch-1", `"1"`,
		`{"name":"Updated Party"}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH party status = %d: %s", patch.Code, patch.Body.String())
	}
	updated := decode[partyEnvelope](t, patch)
	if updated.Party.Name != "Updated Party" || updated.Party.Version != 2 {
		t.Fatalf("updated party = %+v", updated.Party)
	}
}

func TestPartyPermissionsAndValidation(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "party."+uuid.NewString())
	other := s.signIn(t, "party."+uuid.NewString())

	rec := s.mut(t, http.MethodPost, "/v1/parties", owner.AccessToken, "create-permission", "", `{"name":"Owners"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	partyID := decode[partyEnvelope](t, rec).Party.ID

	assertError(t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String(), other.AccessToken, nil),
		http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.mut(t, http.MethodPatch, "/v1/parties/"+partyID.String(), other.AccessToken, "other-patch", `"1"`, `{"name":"x"}`),
		http.StatusNotFound, httpapi.CodeNotFound)

	addMember(t, s.pool, partyID, uuid.MustParse(other.UserID))
	if rec := s.do(t, http.MethodGet, "/v1/parties/"+partyID.String(), other.AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("member GET status = %d: %s", rec.Code, rec.Body.String())
	}
	assertError(t, s.mut(t, http.MethodPatch, "/v1/parties/"+partyID.String(), other.AccessToken, "member-patch", `"1"`, `{"name":"x"}`),
		http.StatusForbidden, httpapi.CodeForbidden)

	assertError(t, s.mut(t, http.MethodPost, "/v1/parties", owner.AccessToken, "bad-name", "", `{"name":"line
break"}`),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.mut(t, http.MethodPost, "/v1/parties", owner.AccessToken, "bad-utf8", "", string([]byte{
		'{', '"', 'n', 'a', 'm', 'e', '"', ':', '"', 'B', 'a', 'd', ' ', 0xff, '"', '}',
	})), http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.mut(t, http.MethodPost, "/v1/parties", owner.AccessToken, "replacement-rune", "", `{"name":"Bad �"}`),
		http.StatusBadRequest, httpapi.CodeInvalidRequest)
	assertError(t, s.mut(t, http.MethodPatch, "/v1/parties/"+partyID.String(), owner.AccessToken, "stale-patch", `"99"`, `{"name":"New"}`),
		http.StatusConflict, httpapi.CodeVersionConflict)
}

func TestPatchReplayRequiresCurrentOwner(t *testing.T) {
	for i, tc := range []struct {
		name       string
		change     func(t *testing.T, pool *pgxpool.Pool, partyID uuid.UUID, ownerID uuid.UUID)
		wantStatus int
		wantCode   string
	}{
		{
			name: "owner role lost",
			change: func(t *testing.T, pool *pgxpool.Pool, partyID uuid.UUID, ownerID uuid.UUID) {
				other := insertUser(t, pool)
				newOwnerMembershipID := addMember(t, pool, partyID, other)
				transferOwner(t, pool, partyID, ownerID, newOwnerMembershipID)
			},
			wantStatus: http.StatusForbidden,
			wantCode:   httpapi.CodeForbidden,
		},
		{
			name: "membership removed",
			change: func(t *testing.T, pool *pgxpool.Pool, partyID uuid.UUID, ownerID uuid.UUID) {
				other := insertUser(t, pool)
				newOwnerMembershipID := addMember(t, pool, partyID, other)
				transferOwner(t, pool, partyID, ownerID, newOwnerMembershipID)
				removeMembership(t, pool, partyID, ownerID)
			},
			wantStatus: http.StatusForbidden,
			wantCode:   httpapi.CodeForbidden,
		},
		{
			name: "party disbanded",
			change: func(t *testing.T, pool *pgxpool.Pool, partyID uuid.UUID, _ uuid.UUID) {
				disbandParty(t, pool, partyID)
			},
			wantStatus: http.StatusGone,
			wantCode:   codePartyDisbanded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keySuffix := strconv.Itoa(i)
			s := newStack(t)
			sess := s.signIn(t, "party."+uuid.NewString())
			ownerID := uuid.MustParse(sess.UserID)

			create := s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "create-replay-"+keySuffix, "", `{"name":"Replay Guard"}`)
			if create.Code != http.StatusCreated {
				t.Fatalf("create status = %d: %s", create.Code, create.Body.String())
			}
			partyID := decode[partyEnvelope](t, create).Party.ID

			patch := s.mut(t, http.MethodPatch, "/v1/parties/"+partyID.String(), sess.AccessToken, "patch-replay-"+keySuffix, `"1"`, `{"name":"First Patch"}`)
			if patch.Code != http.StatusOK {
				t.Fatalf("first patch status = %d: %s", patch.Code, patch.Body.String())
			}

			tc.change(t, s.pool, partyID, ownerID)
			replay := s.mut(t, http.MethodPatch, "/v1/parties/"+partyID.String(), sess.AccessToken, "patch-replay-"+keySuffix, `"1"`, `{"name":"First Patch"}`)
			assertError(t, replay, tc.wantStatus, tc.wantCode)
			if replay.Header().Get(ReplayedHeader) != "" {
				t.Fatalf("권한 상실 후 replay header가 설정됐다: %q", replay.Header().Get(ReplayedHeader))
			}
		})
	}
}

func TestCreateReplayRequiresCurrentActiveMember(t *testing.T) {
	for i, tc := range []struct {
		name       string
		change     func(t *testing.T, pool *pgxpool.Pool, partyID uuid.UUID, creatorID uuid.UUID)
		wantStatus int
		wantCode   string
	}{
		{
			name: "creator removed",
			change: func(t *testing.T, pool *pgxpool.Pool, partyID uuid.UUID, creatorID uuid.UUID) {
				other := insertUser(t, pool)
				newOwnerMembershipID := addMember(t, pool, partyID, other)
				transferOwner(t, pool, partyID, creatorID, newOwnerMembershipID)
				removeMembership(t, pool, partyID, creatorID)
			},
			wantStatus: http.StatusNotFound,
			wantCode:   httpapi.CodeNotFound,
		},
		{
			name: "party disbanded",
			change: func(t *testing.T, pool *pgxpool.Pool, partyID uuid.UUID, _ uuid.UUID) {
				disbandParty(t, pool, partyID)
			},
			wantStatus: http.StatusNotFound,
			wantCode:   httpapi.CodeNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keySuffix := strconv.Itoa(i)
			s := newStack(t)
			sess := s.signIn(t, "party."+uuid.NewString())
			creatorID := uuid.MustParse(sess.UserID)

			create := s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "create-replay-member-"+keySuffix, "", `{"name":"Create Replay Guard"}`)
			if create.Code != http.StatusCreated {
				t.Fatalf("create status = %d: %s", create.Code, create.Body.String())
			}
			partyID := decode[partyEnvelope](t, create).Party.ID

			tc.change(t, s.pool, partyID, creatorID)
			replay := s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "create-replay-member-"+keySuffix, "", `{"name":"Create Replay Guard"}`)
			assertError(t, replay, tc.wantStatus, tc.wantCode)
			if replay.Header().Get(ReplayedHeader) != "true" {
				t.Fatalf("create replay marker missing: %q", replay.Header().Get(ReplayedHeader))
			}
			if replay.Header().Get("ETag") != "" {
				t.Fatalf("권한 상실 후 ETag가 설정됐다: %q", replay.Header().Get("ETag"))
			}
		})
	}
}

func TestPatchResponseRequiresOwnerAtWriteTime(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "party."+uuid.NewString())
	ownerID := uuid.MustParse(sess.UserID)

	create := s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "create-owner-response", "", `{"name":"Owner Response"}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", create.Code, create.Body.String())
	}
	partyID := decode[partyEnvelope](t, create).Party.ID
	other := insertUser(t, s.pool)
	newOwnerMembershipID := addMember(t, s.pool, partyID, other)

	response := httptest.NewRecorder()
	transferOwner(t, s.pool, partyID, ownerID, newOwnerMembershipID)
	s.party.writePartyForOwner(response, httptest.NewRequest(http.MethodPatch, "/v1/parties/"+partyID.String(), nil), partyID, ownerID, http.StatusOK)

	assertError(t, response, http.StatusForbidden, httpapi.CodeForbidden)
	if response.Header().Get("ETag") != "" {
		t.Fatalf("권한 상실 후 ETag가 설정됐다: %q", response.Header().Get("ETag"))
	}
}

func TestRemovedMemberCannotReadPartyOrMemberships(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "party."+uuid.NewString())
	member := s.signIn(t, "party."+uuid.NewString())
	memberID := uuid.MustParse(member.UserID)

	create := s.mut(t, http.MethodPost, "/v1/parties", owner.AccessToken, "create-read-auth", "", `{"name":"Read Auth"}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", create.Code, create.Body.String())
	}
	partyID := decode[partyEnvelope](t, create).Party.ID
	addMember(t, s.pool, partyID, memberID)

	if rec := s.do(t, http.MethodGet, "/v1/parties/"+partyID.String(), member.AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("member GET before removal status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := s.do(t, http.MethodGet, "/v1/parties/"+partyID.String()+"/memberships", member.AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("member list before removal status = %d: %s", rec.Code, rec.Body.String())
	}

	removeMembership(t, s.pool, partyID, memberID)
	assertError(t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String(), member.AccessToken, nil),
		http.StatusNotFound, httpapi.CodeNotFound)
	assertError(t, s.do(t, http.MethodGet, "/v1/parties/"+partyID.String()+"/memberships", member.AccessToken, nil),
		http.StatusNotFound, httpapi.CodeNotFound)
}

func TestConcurrentCreateLimitSerializesOnUserWithoutRetries(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "party."+uuid.NewString())
	userID := uuid.MustParse(sess.UserID)

	for i := 0; i < 9; i++ {
		rec := s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "seed-"+strconv.Itoa(i), "",
			`{"name":"Seed `+strconv.Itoa(i)+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed %d status = %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	before := mutation.Retries()
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	codes := make(chan string, 2)
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "race-"+strconv.Itoa(i), "",
				`{"name":"Race `+strconv.Itoa(i)+`"}`)
			statuses <- rec.Code
			if rec.Code != http.StatusCreated {
				codes <- decode[httpapi.ErrorResponse](t, rec).Code
			}
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	close(codes)

	gotCreated, gotLimit := 0, 0
	for status := range statuses {
		if status == http.StatusCreated {
			gotCreated++
		}
		if status == http.StatusConflict {
			gotLimit++
		}
	}
	if gotCreated != 1 || gotLimit != 1 {
		t.Fatalf("동시 생성 결과 created=%d limit=%d, want 1/1", gotCreated, gotLimit)
	}
	for code := range codes {
		if code != codePartyOwnedLimitReached {
			t.Fatalf("conflict code = %q, want %q", code, codePartyOwnedLimitReached)
		}
	}
	assertCount(t, s.pool, `
		SELECT count(*)
		  FROM party_memberships m
		  JOIN parties p ON p.id = m.party_id
		 WHERE m.user_id = $1 AND m.role = 'owner'
		   AND m.status = 'active' AND p.status = 'active'`, userID, 10)
	if after := mutation.Retries(); after != before {
		t.Fatalf("mutation.Retries changed from %d to %d", before, after)
	}
}

func insertUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO users (id, display_name) VALUES ($1, 'Party Helper')`, id); err != nil {
		t.Fatalf("사용자를 만들 수 없다: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, id) })
	return id
}

func addMember(t *testing.T, pool *pgxpool.Pool, partyID, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO party_memberships (id, party_id, user_id, role)
		VALUES ($1, $2, $3, 'member')`, id, partyID, userID)
	if err != nil {
		t.Fatalf("멤버를 추가할 수 없다: %v", err)
	}
	return id
}

func transferOwner(t *testing.T, pool *pgxpool.Pool, partyID, oldOwnerUserID, newOwnerMembershipID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE party_memberships
		   SET role = 'member', version = version + 1, updated_at = now()
		 WHERE party_id = $1 AND user_id = $2 AND status = 'active'`, partyID, oldOwnerUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE party_memberships
		   SET role = 'owner', version = version + 1, updated_at = now()
		 WHERE id = $1`, newOwnerMembershipID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE parties
		   SET owner_membership_id = $2, version = version + 1, updated_at = now()
		 WHERE id = $1`, partyID, newOwnerMembershipID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func removeMembership(t *testing.T, pool *pgxpool.Pool, partyID, userID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE party_memberships
		   SET status = 'removed', ended_at = now(), end_reason = 'removed_by_owner',
		       version = version + 1, updated_at = now()
		 WHERE party_id = $1 AND user_id = $2 AND status = 'active'`, partyID, userID)
	if err != nil {
		t.Fatalf("멤버십을 제거할 수 없다: %v", err)
	}
}

func disbandParty(t *testing.T, pool *pgxpool.Pool, partyID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE parties
		   SET status = 'disbanded', disbanded_at = now(), version = version + 1, updated_at = now()
		 WHERE id = $1`, partyID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE party_memberships
		   SET status = 'removed', ended_at = now(), end_reason = 'party_disbanded',
		       version = version + 1, updated_at = now()
		 WHERE party_id = $1 AND status = 'active'`, partyID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertCount(t *testing.T, pool *pgxpool.Pool, query string, arg any, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), query, arg).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count = %d, want %d for %s", got, want, query)
	}
}
