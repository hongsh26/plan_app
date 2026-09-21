package syncfeed

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 보존 기간(§7.1). sync_changes row는 created_at 기준 Retention이 지나면 정리 대상이다.
// 정리 작업은 아직 없다(scheduler).
const Retention = 30 * 24 * time.Hour

// CursorMaxAge는 cursor가 유효한 최대 나이다. 이보다 오래된 cursor는 410
// sync_cursor_expired로 전체 재동기화를 요구한다.
//
// Retention보다 하루 짧은 이유: cursor 발급 시점에 아직 커밋되지 않은 트랜잭션의
// row는 그 cursor 뒤에 나타나는데, 그 row의 created_at은 트랜잭션 시작 시각이라
// 발급 시각보다 이를 수 있다(created_at 순서와 txid 순서는 일치하지 않는다).
// 정리 작업이 "created_at < now - Retention"으로 지우면, 발급 시각이 now - 29일보다
// 늦은 cursor 뒤의 row는 트랜잭션이 하루 넘게 열려 있지 않은 한 지워지지 않는다.
//
// 이 계산은 두 조건에 기댄다. 정리 작업을 만들 때 둘 다 지켜야 한다.
//  1. 정리는 created_at < now - Retention인 row만 지운다.
//  2. 어떤 트랜잭션도 하루(Retention - CursorMaxAge) 넘게 열려 있지 않다. DB의
//     idle_in_transaction_session_timeout과 statement_timeout으로 강제한다.
const CursorMaxAge = Retention - 24*time.Hour

const (
	defaultLimit = 500
	maxLimit     = 1000
)

var (
	// ErrCursorExpired는 §7.1의 410 sync_cursor_expired다.
	ErrCursorExpired = errors.New("syncfeed: cursor가 보존 기간을 지났다")
)

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
func Read(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, from Cursor, limit int, now time.Time) (Page, error) {
	if now.Sub(from.IssuedAt) > CursorMaxAge {
		return Page{}, ErrCursorExpired
	}
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}

	// LEFT JOIN LATERAL: 변경이 하나도 없어도 horizon 한 줄은 돌려받는다.
	// LIMIT은 한 줄 더 읽어 다음 페이지 유무를 판단한다.
	rows, err := pool.Query(ctx, `
		WITH h AS (SELECT pg_snapshot_xmin(pg_current_snapshot()) AS x)
		SELECT h.x::text, c.txid::text, c.ordinal, c.entity_type, c.entity_id,
		       c.operation, c.entity_version, c.payload
		  FROM h
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

	var horizon uint64
	var changes []Change
	var positions []Cursor // changes[i]의 위치
	for rows.Next() {
		var (
			hx      string
			txid    *string
			ordinal *int32
			ch      Change
			etype   *string
			eid     *uuid.UUID
			op      *string
		)
		if err := rows.Scan(&hx, &txid, &ordinal, &etype, &eid, &op, &ch.Version, &ch.Payload); err != nil {
			return Page{}, err
		}
		if horizon, err = strconv.ParseUint(hx, 10, 64); err != nil {
			return Page{}, err
		}
		if txid == nil {
			continue // 변경이 없는 경우의 horizon 줄
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
	page.Next.IssuedAt = now
	return page, nil
}
