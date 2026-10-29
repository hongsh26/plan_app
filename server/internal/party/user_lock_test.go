package party

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/mutation"
)

// Party 생성은 lockUserForPartyCreate가 users row를 FOR NO KEY UPDATE로 잡는다. mutation helper가 먼저 FOR SHARE로
// 잡으면 같은 사용자의 두 생성이 서로의 업그레이드를 기다리며 교착한다. runLockingUser가 처음부터 같은 모드로 잡는지
// 재시도 횟수로 본다(교착은 재시도로 가려져 응답만으로는 보이지 않는다).
//
// 겹침을 결정적으로 만든다. 외부 트랜잭션이 users row를 FOR SHARE로 쥔 채 요청 A, B를 차례로 줄 세운 뒤 놓는다.
//   - 올바른 경우: A·B가 helper에서 NO KEY UPDATE를 기다리다 차례로 끝난다.
//   - LockUserForUpdate가 빠지면: A·B가 모두 SHARE를 얻고 각자 NO KEY UPDATE로 올리려다 서로를 기다린다(40P01).
func TestSameUserConcurrentCreatesDoNotRetry(t *testing.T) {
	s := newStack(t)
	sub := "lock." + uuid.NewString()
	a, b := s.signIn(t, sub), s.signIn(t, sub)
	before := mutation.Retries()

	ctx := context.Background()
	hold, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Rollback(ctx) })
	var pid int32
	if err := hold.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR SHARE`, a.UserID); err != nil {
		t.Fatal(err)
	}

	done := make([]chan *httptest.ResponseRecorder, 2)
	for i, sess := range []sessionBody{a, b} {
		done[i] = make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done[i] <- s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, uuid.NewString(), "", `{"name":"L"}`)
		}()
		waitUserLockWaiters(t, s, pid, i+1)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range done {
		if rec := <-done[i]; rec.Code != http.StatusCreated {
			t.Fatalf("요청 %d = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if got := mutation.Retries() - before; got != 0 {
		t.Fatalf("교착으로 %d번 재시도했다. Party 생성은 LockUserForUpdate가 필요하다", got)
	}
}

// waitUserLockWaiters는 pid가 쥔 잠금을 직접 또는 앞선 대기자 뒤에서 기다리는 backend가 n개 이상이 될 때까지 기다린다.
func waitUserLockWaiters(t *testing.T, s *stack, pid int32, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got int
		if err := s.pool.QueryRow(context.Background(), `
			WITH direct AS (SELECT pid FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))
			SELECT count(*) FROM pg_stat_activity a
			 WHERE $1 = ANY(pg_blocking_pids(a.pid))
			    OR EXISTS (SELECT 1 FROM direct d WHERE d.pid = ANY(pg_blocking_pids(a.pid)))`, pid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("users 잠금 대기자가 %d개가 되지 않았다", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
