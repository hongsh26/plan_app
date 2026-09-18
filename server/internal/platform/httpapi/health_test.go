package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakePinger는 실제 DB 없이 readiness 분기를 확인한다.
type fakePinger struct {
	err   error
	delay time.Duration
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// liveness는 DB에 의존하면 안 된다. DB가 완전히 죽어 있어도 프로세스가 살아
// 있으면 200이다.
func TestLivenessIgnoresDatabase(t *testing.T) {
	mux := NewMux(fakePinger{err: errors.New("connection refused")}, discardLogger())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("DB 장애 중 liveness = %d, want 200", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("응답이 JSON이 아니다: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
}

func TestReadinessOKWhenDatabaseReachable(t *testing.T) {
	mux := NewMux(fakePinger{}, discardLogger())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("readiness = %d, want 200", rec.Code)
	}
}

// readiness는 DB에 의존한다. ping이 실패하면 503이어야 load balancer가 이
// 인스턴스를 제외한다.
func TestReadinessUnavailableWhenDatabaseDown(t *testing.T) {
	mux := NewMux(fakePinger{err: errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")},
		discardLogger())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("DB 장애 중 readiness = %d, want 503", rec.Code)
	}
}

// §11: 응답에 내부 상세를 노출하지 않는다. DB 오류 원문은 호스트, 포트,
// 사용자명을 담을 수 있다.
func TestReadinessResponseHidesInternalDetail(t *testing.T) {
	const detail = "dial tcp 10.0.3.14:5432: user=api_runtime password authentication failed"
	mux := NewMux(fakePinger{err: errors.New(detail)}, discardLogger())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	body := rec.Body.String()
	for _, leaked := range []string{"10.0.3.14", "api_runtime", "password", "5432"} {
		if strings.Contains(body, leaked) {
			t.Errorf("readiness 응답이 내부 상세 %q를 노출했다: %s", leaked, body)
		}
	}

	var parsed healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("응답이 JSON이 아니다: %v", err)
	}
	if parsed.Status != "unavailable" {
		t.Errorf("status = %q, want unavailable", parsed.Status)
	}
}

// DB가 멈춰 응답하지 않을 때 readiness가 무한정 붙잡히면 load balancer의
// health check까지 함께 막힌다. ping에 한도를 둔다.
func TestReadinessTimesOutOnHangingDatabase(t *testing.T) {
	mux := NewMux(fakePinger{delay: 10 * time.Second}, discardLogger())

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		done <- rec.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("멈춘 DB에 대한 readiness = %d, want 503", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readiness가 ping timeout 안에 반환되지 않았다")
	}
}

// health endpoint는 캐시되면 의미가 없다.
func TestHealthResponsesAreNotCacheable(t *testing.T) {
	mux := NewMux(fakePinger{}, discardLogger())

	for _, path := range []string{"/livez", "/readyz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestHealthRejectsNonGET(t *testing.T) {
	mux := NewMux(fakePinger{}, discardLogger())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/livez", nil))

	if rec.Code == http.StatusOK {
		t.Errorf("POST /livez가 200을 반환했다")
	}
}
