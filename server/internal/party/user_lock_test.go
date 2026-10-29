package party

import (
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/mutation"
)

// Party 생성은 lockUserForPartyCreate가 users row를 FOR NO KEY UPDATE로 잡는다. mutation helper가 먼저 FOR SHARE로
// 잡으면 같은 사용자의 두 생성이 서로의 업그레이드를 기다리며 교착한다. runLockingUser가 처음부터 같은 모드로 잡는지
// 재시도 횟수로 본다(교착은 재시도로 가려져 응답만으로는 보이지 않는다).
func TestSameUserConcurrentCreatesDoNotRetry(t *testing.T) {
	s := newStack(t)
	sub := "lock." + uuid.NewString()
	a, b := s.signIn(t, sub), s.signIn(t, sub)
	before := mutation.Retries()
	for i := range 5 {
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make([]int, 2)
		for j, sess := range []sessionBody{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes[j] = s.mut(t, http.MethodPost, "/v1/parties", sess.AccessToken, "lock-"+strconv.Itoa(i)+"-"+strconv.Itoa(j), "", `{"name":"L"}`).Code
			}()
		}
		close(start)
		wg.Wait()
		for j, code := range codes {
			if code != http.StatusCreated && code != http.StatusConflict {
				t.Fatalf("반복 %d 요청 %d = %d", i, j, code)
			}
		}
	}
	if got := mutation.Retries() - before; got != 0 {
		t.Fatalf("교착으로 %d번 재시도했다. Party 생성은 LockUserForUpdate가 필요하다", got)
	}
}
