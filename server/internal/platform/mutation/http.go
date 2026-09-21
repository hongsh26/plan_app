package mutation

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"plantogether/server/internal/platform/httpapi"
)

// IdempotencyKeyHeader는 §6이 모든 mutation에 요구하는 헤더다.
const IdempotencyKeyHeader = "Idempotency-Key"

// maxKeyLength는 Idempotency-Key 길이 상한이다. 클라이언트는 UUID를 쓴다(§7.3).
const maxKeyLength = 128

var (
	errMissingKey     = errors.New("Idempotency-Key 헤더가 필요하다")
	errBadKey         = errors.New("Idempotency-Key는 1~128자의 출력 가능한 ASCII여야 한다")
	errMissingVersion = errors.New(`If-Match 헤더에 현재 version이 필요하다 (예: If-Match: "3")`)
)

// IdempotencyKey는 헤더에서 키를 읽는다.
func IdempotencyKey(r *http.Request) (string, error) {
	k := r.Header.Get(IdempotencyKeyHeader)
	if k == "" {
		return "", errMissingKey
	}
	if len(k) > maxKeyLength {
		return "", errBadKey
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return "", errBadKey
		}
	}
	return k, nil
}

// ExpectedVersion은 If-Match에서 expected version을 읽는다. §6은 If-Match 또는
// 본문의 expected_version을 허용하지만 한 가지만 받는다. 둘을 모두 받으면 둘이
// 다를 때의 규칙이 또 필요하다. 형식은 강한 ETag `"3"`이고, 따옴표 없는 3도 받는다.
// 약한 ETag(W/)와 *는 받지 않는다. 낙관적 동시성에는 정확한 version이 필요하다.
func ExpectedVersion(r *http.Request) (int64, error) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" {
		return 0, errMissingVersion
	}
	v = strings.TrimSuffix(strings.TrimPrefix(v, `"`), `"`)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return 0, errMissingVersion
	}
	return n, nil
}

// Prepared는 mutation handler가 트랜잭션 전에 모은 입력이다.
type Prepared struct {
	Key             string
	ExpectedVersion int64
	Body            []byte
}

// Prepare는 키, If-Match, 본문을 읽는다. 실패하면 400 응답을 이미 쓰고 false다.
// needVersion이 false면 If-Match를 읽지 않는다(생성 요청).
func Prepare(w http.ResponseWriter, r *http.Request, needVersion bool) (Prepared, bool) {
	var p Prepared
	var err error
	if p.Key, err = IdempotencyKey(r); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error())
		return p, false
	}
	if needVersion {
		if p.ExpectedVersion, err = ExpectedVersion(r); err != nil {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error())
			return p, false
		}
	}
	if p.Body, err = httpapi.ReadBody(w, r); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문을 읽을 수 없다")
		return p, false
	}
	return p, true
}

// WriteError는 Run이 돌려준 멱등성·동시성 오류를 §6 응답으로 쓴다. 해당하지
// 않는 오류면 false를 돌려주고 아무것도 쓰지 않는다.
func WriteError(w http.ResponseWriter, r *http.Request, err error) bool {
	var vc *VersionConflictError
	switch {
	case errors.As(err, &vc):
		httpapi.WriteVersionConflict(w, r, vc.Current)
	case errors.Is(err, ErrIdempotencyMismatch):
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeIdempotencyMismatch,
			"같은 Idempotency-Key가 다른 요청에 쓰였다")
	default:
		return false
	}
	return true
}
