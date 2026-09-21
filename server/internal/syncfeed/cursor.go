// Package syncfeed는 account_backend_design.md §7.1·§7.2의 변경 피드 읽기 경로다.
//
//   - GET /v1/sync?cursor=  : 증분 변경 (settled horizon 아래만)
//   - GET /v1/sync/bootstrap: 전체 snapshot과 cursor watermark
//
// 쓰기는 internal/platform/mutation이 한다(ordinal 발급). 이 패키지는 읽기만 한다.
//
// 디렉터리 이름이 §13의 internal/sync가 아닌 이유: 표준 라이브러리 sync와 패키지
// 이름이 같아지면 이 패키지 안에서 sync.Mutex 같은 이름을 쓸 때마다 별칭이 필요하다.
package syncfeed

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Cursor는 클라이언트에 주는 opaque cursor의 내부 표현이다(§7.1).
//
// 클라이언트는 이 값을 파싱하거나 만들지 않는다. 형식은 계약이 아니지만, 한번
// 발급한 cursor는 클라이언트가 계속 들고 있으므로 형식을 바꿀 때는 이전 버전도
// 계속 읽어야 한다. 그래서 첫 필드에 버전을 둔다.
//
// 담는 것:
//   - (TxID, Ordinal): 마지막으로 전달한 위치. 다음 읽기는 이보다 뒤만 반환한다.
//   - IssuedAt: 서버가 이 cursor를 발급한 시각. 보존 기간 판정(410)에 쓴다.
type Cursor struct {
	TxID     uint64
	Ordinal  int32
	IssuedAt time.Time
}

const cursorVersion = "1"

// ErrBadCursor는 해석할 수 없는 cursor다. 400 invalid_request.
var ErrBadCursor = errors.New("syncfeed: cursor를 해석할 수 없다")

// Encode는 cursor를 opaque 문자열로 만든다.
//
// 서명하지 않는다. cursor를 위조해도 조회 범위는 recipient_user_id로 제한되므로
// 남의 변경을 볼 수 없다. 위조로 얻을 수 있는 것은 자기 변경을 건너뛰는 것뿐이다.
func (c Cursor) Encode() string {
	raw := strings.Join([]string{
		cursorVersion,
		strconv.FormatUint(c.TxID, 10),
		strconv.FormatInt(int64(c.Ordinal), 10),
		strconv.FormatInt(c.IssuedAt.Unix(), 10),
	}, ".")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor는 Encode의 역이다.
func DecodeCursor(s string) (Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	parts := strings.Split(string(b), ".")
	if len(parts) != 4 || parts[0] != cursorVersion {
		return Cursor{}, ErrBadCursor
	}
	txid, err1 := strconv.ParseUint(parts[1], 10, 64)
	ord, err2 := strconv.ParseInt(parts[2], 10, 32)
	issued, err3 := strconv.ParseInt(parts[3], 10, 64)
	// ordinal은 -1(해당 txid의 처음 앞)부터 가능하다.
	if err1 != nil || err2 != nil || err3 != nil || ord < -1 || issued <= 0 {
		return Cursor{}, ErrBadCursor
	}
	return Cursor{TxID: txid, Ordinal: int32(ord), IssuedAt: time.Unix(issued, 0).UTC()}, nil
}

// after는 (TxID, Ordinal) 위치가 o보다 뒤인지다.
func (c Cursor) after(o Cursor) bool {
	return c.TxID > o.TxID || (c.TxID == o.TxID && c.Ordinal > o.Ordinal)
}
