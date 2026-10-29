package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// page 업로드는 mutation.Run을 거치지 않는다. 인증을 통과한 page 요청이 진행 중인 계정 삭제 요청 트랜잭션을 기다렸다가
// 그 커밋 뒤에 staging을 쓰면 삭제 정리 뒤에 데이터가 남는다. 삭제 요청처럼 users row를 FOR NO KEY UPDATE로 잡고
// 상태를 바꾼 트랜잭션을 열어 둔 채 요청을 보내고, 커밋 뒤 요청이 423이고 아무것도 쓰지 않았는지 본다.
func TestSnapshotPageWaitingOnDeletionRequestIsRejected(t *testing.T) {
	s := newStack(t)
	a := s.signIn(t, "cal.delrace."+uuid.NewString())
	s.enableCalendar(t, a)
	rec := s.createSnapshot(t, a, 1, 1)
	if rec.Code != http.StatusCreated {
		t.Fatalf("snapshot 생성 = %d: %s", rec.Code, rec.Body.String())
	}
	snap := decode[snapEnv](t, rec).Snapshot

	ctx := context.Background()
	del, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = del.Rollback(ctx) })
	var pid int32
	if err := del.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := del.Exec(ctx, `
		UPDATE users SET status = 'deletion_requested', deletion_requested_at = now(), version = version + 1
		 WHERE id = $1`, a.UserID); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- s.putPage(t, a, snap.ID, 0, []fact{mkFact(1, 1, 9)}) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("page 요청이 users 잠금에서 기다리지 않았다")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := del.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertError(t, <-done, http.StatusLocked, "account_locked")
	var staged int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM calendar_snapshot_facts_staging WHERE session_id = $1`, snap.ID).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if staged != 0 {
		t.Fatalf("삭제 요청 뒤 staging %d행이 쓰였다", staged)
	}
}
