package httpapi

import (
	"log/slog"
	"os"
)

// NewLogger는 구조화 로거를 만든다.
//
// account_backend_design.md §11: 구조화 로그는 request_id, actor 내부 ID,
// action, result, latency만 기록한다. token, Apple credential, 캘린더
// 제목·장소·메모, 초대 원문을 남기지 않는다. 이 로거를 쓰는 모든 호출부가
// 그 필드 allowlist를 지킬 책임이 있다.
func NewLogger(level slog.Level, role string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler).With(slog.String("role", role))
}
