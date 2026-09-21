package syncfeed

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultLimit = 500
	maxLimit     = 1000
)

// ErrCursorExpired는 §7.1의 410 sync_cursor_expired다. 두 경우다.
//   - cursor가 정리 워터마크 이하다. 그 뒤의 변경 일부가 이미 지워졌을 수 있다.
//   - cursor를 발급한 클러스터 세대가 지금과 다르다(epoch.go).
//
// 어느 경우든 이어 받으면 무언가 빠졌다는 사실조차 모르게 되므로 전체 재동기화를 요구한다.
var ErrCursorExpired = errors.New("syncfeed: cursor를 이어 쓸 수 없다")

// Change는 클라이언트에 전달하는 변경 하나다. 스냅샷 entity도 같은 모양이다.
type Change struct {
	EntityType string          `json:"entity_type"`
	EntityID   uuid.UUID       `json:"entity_id"`
	Operation  string          `json:"operation"`
	Version    *int64          `json:"version"`
	Payload    json.RawMessage `json:"payload"`
}

// Page는 증분 읽기 결과다.
type Page struct {
	Changes []Change
	Next    Cursor
	HasMore bool
}

// Read는 cursor 뒤의 변경을 settled horizon 아래에서만 읽는다(§7.1).
//
// horizon = pg_snapshot_xmin(pg_current_snapshot())은 이 문장 snapshot에서 아직
// 진행 중인 가장 오래된 트랜잭션 ID다. 그보다 작은 txid의 트랜잭션은 전부 끝났으므로
// 그 row들 사이에 나중에 끼어들 row가 없다. horizon 이상은 진행 중인 트랜잭션이
// 있을 수 있으므로 돌려주지 않는다. 먼저 시작해 나중에 커밋한 트랜잭션의 변경이
// cursor 뒤로 밀려 영구히 유실되는 것을 이 조건이 막는다.
//
// horizon을 같은 문장 안에서 구한다. 따로 구하면 두 문장 사이에 snapshot이 달라진다.
//
// recipient_user_id 조건이 권한의 전부다. handler에서 거르지 않는다.
func Read(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, from Cursor, limit int) (Page, error) {
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}

	// 한 문장에서 horizon, 세대, 정리 워터마크 판정, 변경을 함께 읽는다. 정리는
	// 삭제와 워터마크 갱신을 한 트랜잭션에서 하므로, 한 snapshot 안에서는 "워터마크는
	// 옛날인데 row는 지워진" 상태가 보이지 않는다.
	//
	// LEFT JOIN LATERAL: 변경이 하나도 없어도 첫 줄(horizon 등)은 돌려받는다.
	// LIMIT은 한 줄 더 읽어 다음 페이지 유무를 판단한다.
	rows, err := pool.Query(ctx, `
		WITH h AS (SELECT pg_snapshot_xmin(pg_current_snapshot()) AS x),
		     p AS (SELECT pruned_through_txid AS t, pruned_through_ordinal AS o FROM sync_prune_state)
		SELECT h.x::text, `+epochSQL+`,
		       ($2::text::xid8, $3::int) <= (p.t, p.o),
		       c.txid::text, c.ordinal, c.entity_type, c.entity_id,
		       c.operation, c.entity_version, c.payload
		  FROM h CROSS JOIN p
		  LEFT JOIN LATERAL (
		        SELECT txid, ordinal, entity_type, entity_id, operation, entity_version, payload
		          FROM sync_changes
		         WHERE recipient_user_id = $1
		           AND (txid, ordinal) > ($2::text::xid8, $3::int)
		           AND txid < h.x
		         ORDER BY txid, ordinal
		         LIMIT $4
		       ) c ON true`,
		userID, strconv.FormatUint(from.TxID, 10), from.Ordinal, limit+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()

	var (
		horizon   uint64
		epoch     Epoch
		pruned    bool
		changes   []Change
		positions []Cursor // changes[i]의 위치
	)
	for rows.Next() {
		var (
			hx, sysid, tli string
			txid           *string
			ordinal        *int32
			ch             Change
			etype          *string
			eid            *uuid.UUID
			op             *string
		)
		if err := rows.Scan(&hx, &sysid, &tli, &pruned, &txid, &ordinal, &etype, &eid, &op, &ch.Version, &ch.Payload); err != nil {
			return Page{}, err
		}
		if horizon, epoch, err = parseHead(hx, sysid, tli); err != nil {
			return Page{}, err
		}
		if txid == nil {
			continue // 변경이 없는 경우의 첫 줄
		}
		t, err := strconv.ParseUint(*txid, 10, 64)
		if err != nil {
			return Page{}, err
		}
		ch.EntityType, ch.EntityID, ch.Operation = *etype, *eid, *op
		changes = append(changes, ch)
		positions = append(positions, Cursor{TxID: t, Ordinal: *ordinal})
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if pruned || epoch != from.Epoch {
		return Page{}, ErrCursorExpired
	}

	page := Page{Changes: changes}
	if len(changes) > limit {
		// 다음 페이지가 있다. 마지막으로 전달한 변경 위치에서 이어 간다. 한
		// 트랜잭션의 변경이 페이지 경계에서 갈라져도 안전하다. 그 트랜잭션은 horizon
		// 아래라 이미 끝났으므로 나머지 변경이 나중에 바뀌지 않는다.
		page.Changes = changes[:limit]
		page.Next = positions[limit-1]
		page.HasMore = true
	} else {
		// horizon 아래의 변경을 모두 전달했다. 앞으로 이 사용자에게 생길 변경은
		// 전부 txid >= horizon이므로(horizon 미만 트랜잭션은 끝났다) cursor를
		// (horizon, -1)로 당겨도 놓치는 것이 없다. 다음 읽기의 스캔 범위가 줄어든다.
		// 뒤로 가지 않도록 입력 cursor, 마지막 변경 위치와 비교해 가장 뒤를 쓴다.
		page.Next = from
		if n := len(positions); n > 0 && positions[n-1].after(page.Next) {
			page.Next = positions[n-1]
		}
		if h := (Cursor{TxID: horizon, Ordinal: -1}); h.after(page.Next) {
			page.Next = h
		}
	}
	page.Next.Epoch = epoch
	return page, nil
}

func parseHead(hx, sysid, tli string) (uint64, Epoch, error) {
	h, err := strconv.ParseUint(hx, 10, 64)
	if err != nil {
		return 0, Epoch{}, err
	}
	s, err := strconv.ParseUint(sysid, 10, 64)
	if err != nil {
		return 0, Epoch{}, err
	}
	t, err := strconv.ParseUint(tli, 10, 32)
	if err != nil {
		return 0, Epoch{}, err
	}
	return h, Epoch{SystemID: s, Timeline: uint32(t)}, nil
}
