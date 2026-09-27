package account

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/jobs"
	"plantogether/server/internal/platform/mutation"
)

func TestConcurrentDeleteRequestsLeaveNoLiveSessionWithoutDeadlockRetry(t *testing.T) {
	s := newStack(t)
	sub := "delete.concurrent." + uuid.NewString()
	a := s.signIn(t, sub)
	b := s.signIn(t, sub)
	before := mutation.Retries()

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i, sess := range []sessionBody{a, b} {
		wg.Add(1)
		go func(i int, sess sessionBody) {
			defer wg.Done()
			<-start
			responses <- s.mut(t, http.MethodDelete, "/v1/me", sess.AccessToken,
				fmt.Sprintf("delete-concurrent-%d", i), `"1"`, ``)
		}(i, sess)
	}
	close(start)
	wg.Wait()
	close(responses)

	accepted := 0
	for rec := range responses {
		switch rec.Code {
		case http.StatusAccepted:
			accepted++
		case http.StatusUnauthorized, http.StatusLocked, http.StatusConflict:
		default:
			t.Fatalf("동시 삭제 status = %d: %s", rec.Code, rec.Body.String())
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted = %d, want 1", accepted)
	}
	if got := mutation.Retries(); got != before {
		t.Fatalf("mutation retries = %d -> %d; 잠금 순서가 깨졌다", before, got)
	}
	var live int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, a.UserID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("live sessions = %d", live)
	}
}

func TestDeleteMeRequestsDeletionAndBlocksReplayAtAuth(t *testing.T) {
	s := newStack(t)
	sub := "delete." + uuid.NewString()
	a := s.signIn(t, sub)
	b := s.signIn(t, sub)

	rec := s.mut(t, http.MethodDelete, "/v1/me", a.AccessToken, "delete-key", `"1"`, ``)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE /v1/me status = %d: %s", rec.Code, rec.Body.String())
	}
	me := decode[meResponse](t, rec)
	if me.User.Status != "deletion_requested" || me.User.Version != 2 {
		t.Fatalf("삭제 요청 후 user = %+v", me.User)
	}
	if rec.Header().Get("ETag") != `"2"` {
		t.Errorf("ETag = %q, want \"2\"", rec.Header().Get("ETag"))
	}

	assertError(t, s.do(t, http.MethodGet, "/v1/me", a.AccessToken, nil),
		http.StatusUnauthorized, httpapi.CodeInvalidSession)
	assertError(t, s.do(t, http.MethodGet, "/v1/me", b.AccessToken, nil),
		http.StatusUnauthorized, httpapi.CodeInvalidSession)
	assertError(t, s.mut(t, http.MethodDelete, "/v1/me", a.AccessToken, "delete-key", `"1"`, ``),
		http.StatusUnauthorized, httpapi.CodeInvalidSession)

	var liveSessions, liveDevices, jobsN int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, a.UserID).Scan(&liveSessions); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM devices WHERE user_id = $1 AND revoked_at IS NULL`, a.UserID).Scan(&liveDevices); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM outbox_jobs
		 WHERE type = $1 AND payload->>'user_id' = $2 AND status = 'pending'`,
		AccountDeletionJobType, a.UserID).Scan(&jobsN); err != nil {
		t.Fatal(err)
	}
	if liveSessions != 0 || liveDevices != 0 || jobsN != 1 {
		t.Fatalf("liveSessions=%d liveDevices=%d jobs=%d, want 0/0/1", liveSessions, liveDevices, jobsN)
	}
	if n := s.syncCount(t, a.UserID, entityUser); n != 1 {
		t.Errorf("user sync 변경 %d개, want 1", n)
	}
	if n := s.syncCount(t, a.UserID, entityDevice); n != 2 {
		t.Errorf("device sync 변경 %d개, want 2", n)
	}
}

func TestAccountDeletionWorkerRevokesAppleTokenAndDeletesIdentity(t *testing.T) {
	s := newStack(t)
	sub := "delete.worker." + uuid.NewString()
	sess := s.signIn(t, sub)
	storeAppleRefresh(t, s, sess.UserID, "apple-refresh-token")

	if rec := s.mut(t, http.MethodDelete, "/v1/me", sess.AccessToken, "delete-key", `"1"`, ``); rec.Code != http.StatusAccepted {
		t.Fatalf("삭제 요청 실패: %d %s", rec.Code, rec.Body.String())
	}
	revoker := &fakeRevoker{}
	runDeletionJob(t, s, revoker, sess.UserID)
	if len(revoker.tokens) != 1 || revoker.tokens[0] != "apple-refresh-token" {
		t.Fatalf("revoke tokens = %#v", revoker.tokens)
	}

	var status, displayName string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT status, display_name FROM users WHERE id = $1`, sess.UserID).Scan(&status, &displayName); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" || displayName != "탈퇴한 사용자" {
		t.Fatalf("user status=%s display_name=%q", status, displayName)
	}
	assertNoRows(t, s, `SELECT count(*) FROM auth_identities WHERE user_id = $1`, sess.UserID)
	assertNoRows(t, s, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, sess.UserID)
	assertNoRows(t, s, `SELECT count(*) FROM devices WHERE user_id = $1 AND (revoked_at IS NULL OR push_token_ciphertext IS NOT NULL)`, sess.UserID)

	var tombstones int
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		 WHERE actor_user_id IS NULL AND actor_tombstone LIKE 'deleted_user:%'`).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if tombstones == 0 {
		t.Fatal("audit actor tombstone이 남지 않았다")
	}

	again := s.signIn(t, sub)
	if again.UserID == sess.UserID {
		t.Fatal("deleted 뒤 같은 Apple subject 재가입이 새 user_id를 발급하지 않았다")
	}
}

func TestAccountDeletionWorkerTreatsInvalidGrantAsAlreadyRevoked(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "delete.invalid_grant."+uuid.NewString())
	storeAppleRefresh(t, s, sess.UserID, "already-revoked")
	if rec := s.mut(t, http.MethodDelete, "/v1/me", sess.AccessToken, "delete-key", `"1"`, ``); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body.String())
	}
	runDeletionJob(t, s, &fakeRevoker{err: &appleid.APIError{Status: http.StatusBadRequest, Code: "invalid_grant"}}, sess.UserID)

	var status string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id = $1`, sess.UserID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" {
		t.Fatalf("status = %s, want deleted", status)
	}
}

func TestAccountDeletionWorkerRetriesAppleCredentialError(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "delete.invalid_client."+uuid.NewString())
	storeAppleRefresh(t, s, sess.UserID, "needs-revoke")
	if rec := s.mut(t, http.MethodDelete, "/v1/me", sess.AccessToken, "delete-key", `"1"`, ``); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body.String())
	}

	svc, err := NewDeletionService(DeletionDeps{
		Pool: s.pool, Opener: s.box,
		Apple: &fakeRevoker{err: &appleid.APIError{Status: http.StatusBadRequest, Code: "invalid_client"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = svc.HandleJob(context.Background(), deletionJob(sess.UserID))
	if err == nil || jobs.IsPermanent(err) {
		t.Fatalf("err = %v, want retryable", err)
	}
	var status string
	if qerr := s.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id = $1`, sess.UserID).Scan(&status); qerr != nil {
		t.Fatal(qerr)
	}
	if status != "deleting" {
		t.Fatalf("revoke 실패 후 status = %s, want deleting", status)
	}
	svc.apple = &fakeRevoker{}
	if err := svc.HandleJob(context.Background(), deletionJob(sess.UserID)); err != nil {
		t.Fatalf("revoker 복구 후 재시도: %v", err)
	}
	if qerr := s.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id = $1`, sess.UserID).Scan(&status); qerr != nil {
		t.Fatal(qerr)
	}
	if status != "deleted" {
		t.Fatalf("revoker 복구 후 status = %s, want deleted", status)
	}
}

func TestAccountDeletionWorkerCompletesAfterDeadline(t *testing.T) {
	s := newStack(t)
	sess := s.signIn(t, "delete.overdue."+uuid.NewString())
	if rec := s.mut(t, http.MethodDelete, "/v1/me", sess.AccessToken, "delete-key", `"1"`, ``); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body.String())
	}
	if _, err := s.pool.Exec(context.Background(), `
		UPDATE users SET deletion_requested_at = now() - interval '25 hours' WHERE id = $1`, sess.UserID); err != nil {
		t.Fatal(err)
	}
	runDeletionJob(t, s, &fakeRevoker{}, sess.UserID)
	var status string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id = $1`, sess.UserID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" {
		t.Fatalf("24시간 초과 후 status = %s, want deleted", status)
	}
	assertNoRows(t, s, `SELECT count(*) FROM auth_identities WHERE user_id = $1`, sess.UserID)
}

type fakeRevoker struct {
	tokens []string
	err    error
}

func (f *fakeRevoker) Revoke(_ context.Context, refreshToken string) error {
	f.tokens = append(f.tokens, refreshToken)
	return f.err
}

func storeAppleRefresh(t *testing.T, s *stack, userID, token string) {
	t.Helper()
	sealed, err := s.box.Seal([]byte(token), auth.SealPurposeAppleRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `
		UPDATE auth_identities
		   SET provider_refresh_token_ciphertext = $2
		 WHERE user_id = $1`, userID, sealed); err != nil {
		t.Fatal(err)
	}
}

func runDeletionJob(t *testing.T, s *stack, revoker *fakeRevoker, userID string) {
	t.Helper()
	svc, err := NewDeletionService(DeletionDeps{Pool: s.pool, Opener: s.box, Apple: revoker})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleJob(context.Background(), deletionJob(userID)); err != nil {
		t.Fatal(err)
	}
}

func deletionJob(userID string) jobs.Job {
	return jobs.Job{Type: AccountDeletionJobType, Payload: []byte(`{"user_id":"` + userID + `"}`), Attempt: 1}
}

func assertNoRows(t *testing.T, s *stack, query, userID string) {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), query, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%s => %d, want 0", query, n)
	}
}

func TestParseDeletionPayloadRejectsBadInput(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"user_id":"not-a-uuid"}`,
		`{"user_id":"` + uuid.NewString() + `","extra":true}`,
		`{"user_id":"` + uuid.NewString() + `"} {}`,
	} {
		if _, err := parseDeletionPayload([]byte(raw)); err == nil {
			t.Fatalf("payload %s accepted", raw)
		}
	}
	if _, err := parseDeletionPayload([]byte(`{"user_id":"` + uuid.NewString() + `"}`)); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
}
