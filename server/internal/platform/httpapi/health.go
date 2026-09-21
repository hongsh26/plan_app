// Package httpapi는 HTTP 서버 골격과 health/readiness 핸들러를 제공한다.
// account_backend_design.md §3.2에 따라 net/http와 http.ServeMux만 쓴다.
// 외부 웹 프레임워크를 도입하지 않는다.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"plantogether/server/internal/platform/postgres"
)

// healthResponse는 liveness와 readiness가 모두 쓰는 응답이다.
//
// §11에 따라 token, 자격, 내부 상세를 노출하지 않는다. 특히 DB 오류 원문은
// 호스트명과 사용자명을 담을 수 있으므로 응답에 넣지 않는다. 진단에 필요한
// 내용은 서버 로그에만 남긴다.
type healthResponse struct {
	Status string `json:"status"`
}

// NewLivenessHandler는 프로세스 생존만 보고하는 핸들러를 만든다.
//
// DB를 비롯한 어떤 외부 의존성도 확인하지 않는다. liveness가 DB에 의존하면
// DB 장애가 모든 API 인스턴스의 재시작 루프로 번진다. 프로세스가 요청을
// 처리할 수 있으면 200이다.
func NewLivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	})
}

// NewReadinessHandler는 트래픽을 받을 준비가 됐는지 보고하는 핸들러를 만든다.
//
// DB에 의존한다. ping이 실패하면 503을 반환해 load balancer가 이 인스턴스를
// 제외하게 한다. pinger는 interface이므로 단위 테스트에 실제 DB가 필요 없다.
func NewReadinessHandler(pinger postgres.Pinger, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DB가 멈춰 있을 때 dial timeout 전체를 기다리지 않도록 한도를 둔다.
		ctx, cancel := context.WithTimeout(r.Context(), postgres.DefaultPingTimeout)
		defer cancel()

		if err := pinger.Ping(ctx); err != nil {
			// 오류 원문은 로그에만 남긴다. 응답에는 넣지 않는다 (§11).
			logger.WarnContext(ctx, "readiness 검사 실패",
				slog.String("request_id", RequestID(ctx).String()),
				slog.String("action", "readiness_check"),
				slog.String("result", "failure"),
				slog.String("error", err.Error()),
			)
			WriteJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "unavailable"})
			return
		}
		WriteJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	})
}

// NewMux는 P0 범위의 라우트를 등록한다. Go 1.22+ ServeMux의 메서드 포함
// 패턴을 쓰므로 별도 라우터가 필요 없다.
//
// 인증이 필요한 /v1 endpoint는 P0 범위가 아니다.
func NewMux(pinger postgres.Pinger, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /livez", NewLivenessHandler())
	mux.Handle("GET /readyz", NewReadinessHandler(pinger, logger))
	return mux
}

// NewServer는 기본 timeout이 설정된 http.Server를 만든다. net/http의 기본값은
// timeout이 없어 느린 클라이언트가 연결을 무한정 붙잡을 수 있다.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
