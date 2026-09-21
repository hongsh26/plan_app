package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// 오류 코드. account_backend_design.md §6 오류 코드 표를 그대로 옮긴다.
// 클라이언트는 HTTP 상태가 아니라 이 code로 분기한다(409가 두 가지 뜻을 가진다).
const (
	CodeInvalidRequest      = "invalid_request"
	CodeInvalidSession      = "invalid_session"
	CodeForbidden           = "forbidden"
	CodeNotFound            = "not_found"
	CodeVersionConflict     = "version_conflict"
	CodeIdempotencyMismatch = "idempotency_mismatch"
	CodeSyncCursorExpired   = "sync_cursor_expired"
	CodeAccountLocked       = "account_locked"
	CodeRateLimited         = "rate_limited"
	CodeInternal            = "internal"
)

// ErrorResponse는 §6의 오류 응답이다. code, message, request_id를 반환한다.
// current_version은 version_conflict에서만 채운다.
//
// message는 사람이 읽는 짧은 설명이다. 내부 오류 원문, SQL, token을 넣지
// 않는다(§11). 진단 정보는 서버 로그에만 있다.
type ErrorResponse struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	RequestID      string `json:"request_id"`
	CurrentVersion *int64 `json:"current_version,omitempty"`
}

// WriteError는 §6 형식의 오류 응답을 쓴다.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, status, ErrorResponse{
		Code:      code,
		Message:   message,
		RequestID: RequestID(r.Context()).String(),
	})
}

// WriteJSON은 JSON 응답을 쓴다. API 응답은 사용자별 데이터이므로 공유
// 캐시에 남지 않게 no-store를 건다.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// maxRequestBody는 JSON 요청 본문의 상한이다. 인증 요청의 가장 큰 필드인
// Apple identity token도 수 KB이므로 64KB면 충분하다.
const maxRequestBody = 64 << 10

// errBadJSON은 DecodeJSON이 돌려주는 단일 오류다. 원인(크기 초과, 문법 오류,
// 알 수 없는 필드)을 구분해 응답하지 않는다. 핸들러는 400 invalid_request 하나로
// 답한다.
var errBadJSON = errors.New("요청 본문이 올바른 JSON이 아니다")

// DecodeJSON은 요청 본문을 dst로 읽는다. 알 수 없는 필드와 두 번째 JSON 값을
// 거부한다. 필드 이름 오타가 조용히 무시되면 클라이언트는 값이 전달된 줄 안다.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errBadJSON
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errBadJSON
	}
	return nil
}
