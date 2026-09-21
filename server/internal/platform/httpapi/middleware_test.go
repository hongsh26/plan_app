package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// 응답 헤더의 request_id와 핸들러가 context에서 읽은 request_id가 같아야 한다.
// 다르면 클라이언트가 신고한 ID로 서버 로그를 찾을 수 없다.
func TestRequestIDIsIssuedAndPropagated(t *testing.T) {
	var seen uuid.UUID
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	header, err := uuid.Parse(rec.Header().Get(RequestIDHeader))
	if err != nil {
		t.Fatalf("응답 헤더 %s가 UUID가 아니다: %v", RequestIDHeader, err)
	}
	if seen == uuid.Nil {
		t.Fatal("핸들러 context에 request_id가 없다")
	}
	if header != seen {
		t.Errorf("헤더 request_id %s != context request_id %s", header, seen)
	}
}

// 클라이언트가 보낸 X-Request-ID를 그대로 쓰면 다른 요청의 ID를 재사용해
// 로그·audit 상관관계를 오염시킬 수 있다.
func TestRequestIDIgnoresClientSuppliedValue(t *testing.T) {
	forged := uuid.New()
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, forged.String())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get(RequestIDHeader) == forged.String() {
		t.Fatal("클라이언트가 보낸 request_id를 그대로 채택했다")
	}
}

func TestRequestIDDiffersPerRequest(t *testing.T) {
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ids := map[string]bool{}
	for range 5 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		ids[rec.Header().Get(RequestIDHeader)] = true
	}
	if len(ids) != 5 {
		t.Fatalf("request_id가 요청마다 다르지 않다: %d개만 서로 다름", len(ids))
	}
}

// §6: 오류 응답은 code, message, request_id를 반환한다. 본문의 request_id는
// 응답 헤더와 같아야 한다.
func TestWriteErrorCarriesRequestID(t *testing.T) {
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusUnauthorized, CodeInvalidSession, "세션이 유효하지 않다")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("오류 응답이 JSON이 아니다: %v", err)
	}
	if body["code"] != CodeInvalidSession {
		t.Errorf("code = %v, want %s", body["code"], CodeInvalidSession)
	}
	if body["request_id"] != rec.Header().Get(RequestIDHeader) {
		t.Errorf("본문 request_id %v != 헤더 %s", body["request_id"], rec.Header().Get(RequestIDHeader))
	}
	if _, ok := body["current_version"]; ok {
		t.Error("version_conflict가 아닌 오류에 current_version이 있다")
	}
}

// access log는 §11 허용 필드만 남긴다. 특히 원본 경로와 query를 남기지 않는다.
// query에는 sync cursor 같은 값이 들어간다.
func TestAccessLogUsesRoutePatternNotRawPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	secretID := uuid.New().String()
	rec := httptest.NewRecorder()
	Wrap(logger, mux).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/devices/"+secretID+"?cursor=opaque-cursor-value", nil))

	line := buf.String()
	if strings.Contains(line, secretID) || strings.Contains(line, "opaque-cursor-value") {
		t.Fatalf("access log에 원본 경로나 query가 남았다: %s", line)
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("access log가 JSON 한 줄이 아니다: %v (%s)", err, line)
	}
	if entry["action"] != "GET /v1/devices/{id}" {
		t.Errorf("action = %v, want route 패턴", entry["action"])
	}
	if entry["status"] != float64(http.StatusNoContent) {
		t.Errorf("status = %v, want 204", entry["status"])
	}
	if entry["request_id"] != rec.Header().Get(RequestIDHeader) {
		t.Errorf("로그 request_id %v != 응답 헤더 %s", entry["request_id"], rec.Header().Get(RequestIDHeader))
	}

	allowed := map[string]bool{
		"time": true, "level": true, "msg": true,
		"request_id": true, "action": true, "result": true, "status": true, "latency_ms": true,
	}
	for k := range entry {
		if !allowed[k] {
			t.Errorf("허용 목록 밖의 로그 필드 %q", k)
		}
	}
}

func TestAccessLogMarksUnmatchedRoutes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	rec := httptest.NewRecorder()
	Wrap(logger, http.NewServeMux()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/wp-admin/x.php", nil))

	if strings.Contains(buf.String(), "wp-admin") {
		t.Fatalf("매칭되지 않은 원본 경로가 로그에 남았다: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"action":"unmatched"`) || !strings.Contains(buf.String(), `"result":"failure"`) {
		t.Errorf("매칭 실패 요청이 unmatched/failure로 기록되지 않았다: %s", buf.String())
	}
}

func TestDecodeJSONRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	type payload struct {
		A string `json:"a"`
	}
	cases := map[string]string{
		"알 수 없는 필드": `{"a":"x","b":"y"}`,
		"두 번째 값":    `{"a":"x"}{"a":"y"}`,
		"문법 오류":     `{"a":`,
		"빈 본문":      ``,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var p payload
			req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
			if err := DecodeJSON(httptest.NewRecorder(), req, &p); err == nil {
				t.Fatalf("%q를 받아들였다", body)
			}
		})
	}

	var p payload
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":"ok"}`))
	if err := DecodeJSON(httptest.NewRecorder(), req, &p); err != nil || p.A != "ok" {
		t.Fatalf("정상 본문을 거부했다: err=%v, a=%q", err, p.A)
	}
}

func TestDecodeJSONRejectsOversizedBody(t *testing.T) {
	var p struct {
		A string `json:"a"`
	}
	big := `{"a":"` + strings.Repeat("x", maxRequestBody) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(big))
	if err := DecodeJSON(httptest.NewRecorder(), req, &p); err == nil {
		t.Fatal("상한을 넘는 본문을 받아들였다")
	}
}
