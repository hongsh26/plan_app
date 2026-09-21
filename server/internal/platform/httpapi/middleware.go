package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// RequestIDHeader는 응답에 request_id를 싣는 헤더다. 클라이언트가 오류를
// 신고할 때 서버 로그와 대조하는 열쇠다.
const RequestIDHeader = "X-Request-ID"

type requestIDKey struct{}

// RequestID는 context에 담긴 request_id를 돌려준다. 미들웨어를 거치지 않은
// context(단위 테스트, 백그라운드 작업)에서는 uuid.Nil이다.
func RequestID(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(requestIDKey{}).(uuid.UUID)
	return id
}

// WithRequestID는 request_id를 context에 담는다. 미들웨어와 테스트가 쓴다.
func WithRequestID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDMiddleware는 요청마다 서버가 request_id를 발급한다.
//
// 클라이언트가 보낸 X-Request-ID를 받아 쓰지 않는다. audit_events.request_id와
// 구조화 로그가 이 값으로 요청을 묶는데, 클라이언트 값을 신뢰하면 다른 요청의
// ID를 재사용해 로그 상관관계를 오염시킬 수 있다. §6은 성공·오류 응답 모두에
// request_id를 요구하므로 헤더로 항상 돌려준다.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.New()
		w.Header().Set(RequestIDHeader, id.String())
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// statusRecorder는 핸들러가 쓴 상태 코드를 access log에 남기려고 가로챈다.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// AccessLogMiddleware는 요청 하나당 로그 한 줄을 남긴다.
//
// §11의 허용 필드(request_id, actor 내부 ID, action, result, latency)만 기록한다.
// action에는 원본 URL이 아니라 ServeMux가 매칭한 route 패턴을 쓴다. 원본 경로와
// query에는 sync cursor, 기기 ID처럼 로그에 남길 이유가 없는 값이 섞인다.
// actor는 인증 미들웨어가 context에 넣은 뒤에야 알 수 있으므로 여기서 남기지
// 않고, 인증이 필요한 도메인 로그가 따로 남긴다.
//
// RequestIDMiddleware 안쪽에서 감싸야 request_id를 읽을 수 있다.
func AccessLogMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		result := "success"
		if status >= 400 {
			result = "failure"
		}
		// 매칭된 route가 없으면(404, 405) 패턴이 비어 있다. 원본 경로로 대체하지
		// 않는다. 스캐너가 보내는 임의 경로가 로그를 채우는 것도 막는다.
		action := r.Pattern
		if action == "" {
			action = "unmatched"
		}
		logger.InfoContext(r.Context(), "request",
			slog.String("request_id", RequestID(r.Context()).String()),
			slog.String("action", action),
			slog.String("result", result),
			slog.Int("status", status),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
		)
	})
}

// Wrap은 모든 라우트에 공통 미들웨어를 적용한다. 바깥에서 안쪽 순서로
// request_id 발급 → access log → 라우팅이다.
func Wrap(logger *slog.Logger, h http.Handler) http.Handler {
	return RequestIDMiddleware(AccessLogMiddleware(logger, h))
}
