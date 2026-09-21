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
)

// Cursor는 클라이언트에 주는 opaque cursor의 내부 표현이다(§7.1).
//
// 클라이언트는 이 값을 파싱하거나 만들지 않는다. 형식은 계약이 아니지만, 한번
// 발급한 cursor는 클라이언트가 계속 들고 있으므로 형식을 바꿀 때는 이전 버전도
// 계속 읽어야 한다. 그래서 첫 필드에 버전을 둔다.
//
// 담는 것:
//   - Epoch: 발급한 DB 클러스터의 세대. 장애 전환이나 시점 복구로 트랜잭션 ID
//     이력이 되감기면 세대가 바뀐다. 세대가 다른 cursor는 410이다(epoch.go).
//   - (TxID, Ordinal): 마지막으로 전달한 위치. 다음 읽기는 이보다 뒤만 반환한다.
//
// 발급 시각은 담지 않는다. 보존 기간 판정은 시각이 아니라 정리 워터마크로 한다(prune.go).
type Cursor struct {
	Epoch   Epoch
	TxID    uint64
	Ordinal int32
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
		strconv.FormatUint(c.Epoch.SystemID, 10),
		strconv.FormatUint(uint64(c.Epoch.Timeline), 10),
		strconv.FormatUint(c.TxID, 10),
		strconv.FormatInt(int64(c.Ordinal), 10),
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
	if len(parts) != 5 || parts[0] != cursorVersion {
		return Cursor{}, ErrBadCursor
	}
	sysid, err1 := strconv.ParseUint(parts[1], 10, 64)
	tli, err2 := strconv.ParseUint(parts[2], 10, 32)
	txid, err3 := strconv.ParseUint(parts[3], 10, 64)
	ord, err4 := strconv.ParseInt(parts[4], 10, 32)
	// ordinal은 -1(해당 txid의 처음 앞)부터 가능하다.
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || ord < -1 || sysid == 0 || tli == 0 {
		return Cursor{}, ErrBadCursor
	}
	return Cursor{Epoch: Epoch{SystemID: sysid, Timeline: uint32(tli)}, TxID: txid, Ordinal: int32(ord)}, nil
}

// after는 (TxID, Ordinal) 위치가 o보다 뒤인지다.
func (c Cursor) after(o Cursor) bool {
	return c.TxID > o.TxID || (c.TxID == o.TxID && c.Ordinal > o.Ordinal)
}
