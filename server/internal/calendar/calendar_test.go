package calendar

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/party"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/postgres/pgtest"
)

type subjectApple struct{}

func (subjectApple) Verify(_ context.Context, idToken, _ string) (appleid.Identity, error) {
	return appleid.Identity{Subject: idToken}, nil
}

type stack struct {
	handler http.Handler
	pool    *pgxpool.Pool
	logs    *bytes.Buffer
	cal     *Handler
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	tokens, err := auth.NewAccessTokens(bytes.Repeat([]byte{6}, 32), nil)
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
	party.NewHandler(party.Deps{Pool: pool, Logger: logger}).Register(mux, authHandler.Require)
	cal := NewHandler(Deps{Pool: pool, Logger: logger})
	cal.Register(mux, authHandler.Require)
	return &stack{handler: httpapi.Wrap(logger, mux), pool: pool, logs: &logs, cal: cal}
}

type session struct {
	UserID       string `json:"user_id"`
	DeviceID     string `json:"device_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (s *stack) req(t *testing.T, method, path, token string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func (s *stack) get(t *testing.T, path, token string) *httptest.ResponseRecorder {
	return s.req(t, http.MethodGet, path, token, nil, "")
}

func (s *stack) mut(t *testing.T, method, path, token, body string, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	h := map[string]string{"Idempotency-Key": uuid.NewString()}
	if ifMatch != "" {
		h["If-Match"] = `"` + ifMatch + `"`
	}
	return s.req(t, method, path, token, h, body)
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
	if e := decode[httpapi.ErrorResponse](t, rec); e.Code != code {
		t.Fatalf("code = %q, want %q", e.Code, code)
	}
}

func (s *stack) signIn(t *testing.T, subject string) session {
	t.Helper()
	rec := s.req(t, http.MethodPost, "/v1/auth/apple", "", nil,
		fmt.Sprintf(`{"identity_token":%q,"authorization_code":"c","raw_nonce":"n","display_name":"Cal Tester"}`, subject))
	if rec.Code != http.StatusOK {
		t.Fatalf("로그인 status = %d: %s", rec.Code, rec.Body.String())
	}
	sess := decode[session](t, rec)
	uid := uuid.MustParse(sess.UserID)
	t.Cleanup(func() { cleanupUser(t, s.pool, uid) })
	return sess
}

func cleanupUser(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("cleanup tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	exec := func(q string, args ...any) {
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`DELETE FROM sync_changes WHERE recipient_user_id = $1`, userID)
	exec(`DELETE FROM idempotency_keys WHERE user_id = $1`, userID)
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
	for _, id := range partyIDs {
		exec(`DELETE FROM party_schedule_projections WHERE party_id = $1`, id)
		exec(`DELETE FROM party_visibility_settings WHERE party_id = $1`, id)
		exec(`UPDATE party_memberships SET invite_id = NULL WHERE party_id = $1`, id)
		exec(`DELETE FROM party_invites WHERE party_id = $1`, id)
		exec(`DELETE FROM party_memberships WHERE party_id = $1`, id)
		exec(`DELETE FROM parties WHERE id = $1`, id)
	}
	exec(`DELETE FROM audit_events WHERE actor_user_id = $1`, userID)
	exec(`DELETE FROM users WHERE id = $1`, userID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// --- 캘린더 시나리오 헬퍼 ---

type fact struct {
	Key          string `json:"source_event_key"`
	Start        string `json:"start_at"`
	End          string `json:"end_at"`
	AllDay       bool   `json:"all_day"`
	TimeZone     string `json:"time_zone"`
	Availability string `json:"availability"`
}

// 테스트 window는 오늘 0시(UTC)부터 30일이다. 시각은 항상 window 안에서 만든다.
var (
	winStart = time.Now().UTC().Truncate(24 * time.Hour)
	winEnd   = winStart.Add(30 * 24 * time.Hour)
)

func key(n byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{n}, 16))
}

func mkFact(n byte, dayOffset int, hour int) fact {
	st := winStart.Add(time.Duration(dayOffset)*24*time.Hour + time.Duration(hour)*time.Hour)
	return fact{Key: key(n), Start: st.Format(time.RFC3339), End: st.Add(time.Hour).Format(time.RFC3339),
		TimeZone: "Asia/Seoul", Availability: "busy"}
}

type connEnv struct {
	Connection connectionResponse `json:"connection"`
}
type snapEnv struct {
	Snapshot   snapshotResponse   `json:"snapshot"`
	Connection connectionResponse `json:"connection"`
}

func (s *stack) connection(t *testing.T, sess session) connectionResponse {
	t.Helper()
	rec := s.get(t, "/v1/calendar/connection", sess.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET connection = %d: %s", rec.Code, rec.Body.String())
	}
	return decode[connEnv](t, rec).Connection
}

// enableCalendar는 연결을 만들고 읽기 source 하나와 sync 기기(이 기기)를 지정한다.
func (s *stack) enableCalendar(t *testing.T, sess session) {
	t.Helper()
	rec := s.mut(t, http.MethodPut, "/v1/calendar/connection", sess.AccessToken,
		`{"status":"selecting","sources":[{"key":"cal-1","enabled":true}],"time_zone":"Asia/Seoul","claim_sync_device":true}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT connection = %d: %s", rec.Code, rec.Body.String())
	}
}

func (s *stack) createSnapshot(t *testing.T, sess session, revision int64, pages int) *httptest.ResponseRecorder {
	t.Helper()
	return s.mut(t, http.MethodPost, "/v1/calendar/snapshots", sess.AccessToken,
		fmt.Sprintf(`{"revision":%d,"window_start":%q,"window_end":%q,"expected_pages":%d}`,
			revision, winStart.Format(time.RFC3339), winEnd.Format(time.RFC3339), pages), "")
}

func (s *stack) putPage(t *testing.T, sess session, snapshotID uuid.UUID, page int, facts []fact) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"facts": facts})
	return s.req(t, http.MethodPut, fmt.Sprintf("/v1/calendar/snapshots/%s/pages/%d", snapshotID, page), sess.AccessToken, nil, string(b))
}

func (s *stack) complete(t *testing.T, sess session, snapshotID uuid.UUID, count int) *httptest.ResponseRecorder {
	t.Helper()
	return s.mut(t, http.MethodPost, fmt.Sprintf("/v1/calendar/snapshots/%s/complete", snapshotID), sess.AccessToken,
		fmt.Sprintf(`{"fact_count":%d}`, count), "")
}

// sync는 snapshot 전체 흐름(create → pages → complete)이다. 페이지는 pageSize개씩 나눈다.
func (s *stack) sync(t *testing.T, sess session, revision int64, facts []fact, pageSize int) snapshotResponse {
	t.Helper()
	pages := (len(facts) + pageSize - 1) / pageSize
	rec := s.createSnapshot(t, sess, revision, pages)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("snapshot 생성 = %d: %s", rec.Code, rec.Body.String())
	}
	id := decode[snapEnv](t, rec).Snapshot.ID
	for p := 0; p < pages; p++ {
		end := (p + 1) * pageSize
		if end > len(facts) {
			end = len(facts)
		}
		if rec := s.putPage(t, sess, id, p, facts[p*pageSize:end]); rec.Code != http.StatusOK {
			t.Fatalf("page %d = %d: %s", p, rec.Code, rec.Body.String())
		}
	}
	rec = s.complete(t, sess, id, len(facts))
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s\n%s", rec.Code, rec.Body.String(), s.logs.String())
	}
	return decode[snapEnv](t, rec).Snapshot
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func factCount(t *testing.T, s *stack, userID string) int {
	return count(t, s.pool, `SELECT count(*) FROM calendar_busy_facts WHERE user_id = $1`, userID)
}
