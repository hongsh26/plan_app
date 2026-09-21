package syncfeed

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaVersion은 bootstrap snapshot 형식 버전이다(§7.2). 클라이언트는 모르는 버전을
// 받으면 앱 업데이트를 요구한다.
const SchemaVersion = 1

// Snapshot은 bootstrap 결과다.
type Snapshot struct {
	Entities  []Change
	Watermark Cursor
}

// Bootstrap은 호출 사용자가 볼 수 있는 전체 상태와 cursor watermark를 만든다(§7.2).
//
// 하나의 REPEATABLE READ 트랜잭션에서 snapshot과 horizon을 함께 읽는다. 따로 읽으면
// watermark가 snapshot과 다른 순간을 가리킨다.
//
// watermark는 (horizon, -1)이다. horizon 미만 트랜잭션의 변경은 전부 snapshot에
// 반영돼 있다. horizon 이상이지만 snapshot 시점에 이미 커밋된 트랜잭션의 변경은
// snapshot에도 있고 이후 증분으로도 다시 온다. 그러므로 보장은 "정확히 한 번"이
// 아니라 "최소 한 번 전달 + version 비교로 한 번 적용"이다(§7.2, §7.4). 대신 snapshot
// 시점에 진행 중이던 트랜잭션의 변경은 빠짐없이 증분으로 온다.
//
// 지금 볼 수 있는 entity는 본인 user와 폐기되지 않은 기기뿐이다. Party, 제안 등은
// 각 도메인을 구현할 때 여기에 더한다.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, now time.Time) (Snapshot, error) {
	var snap Snapshot
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var hx string
			if err := tx.QueryRow(ctx,
				`SELECT pg_snapshot_xmin(pg_current_snapshot())::text`).Scan(&hx); err != nil {
				return err
			}
			horizon, err := strconv.ParseUint(hx, 10, 64)
			if err != nil {
				return err
			}

			var user Change
			var displayName, status string
			var version int64
			if err := tx.QueryRow(ctx, `
				SELECT display_name, status, version FROM users WHERE id = $1`, userID).Scan(
				&displayName, &status, &version); err != nil {
				return err
			}
			user = upsert("user", userID, version, map[string]any{
				"display_name": displayName, "status": status,
			})
			snap.Entities = append(snap.Entities, user)

			rows, err := tx.Query(ctx, `
				SELECT id, platform, push_authorization, time_sensitive_setting,
				       push_token_ciphertext IS NOT NULL, last_seen_at, version
				  FROM devices
				 WHERE user_id = $1 AND revoked_at IS NULL
				 ORDER BY created_at, id`, userID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var (
					id                   uuid.UUID
					platform, authz, tss string
					hasToken             bool
					lastSeen             *time.Time
					v                    int64
				)
				if err := rows.Scan(&id, &platform, &authz, &tss, &hasToken, &lastSeen, &v); err != nil {
					return err
				}
				payload := map[string]any{
					"platform": platform, "push_authorization": authz, "time_sensitive_setting": tss,
					"has_push_token": hasToken, "revoked": false, "last_seen_at": nil,
				}
				if lastSeen != nil {
					payload["last_seen_at"] = lastSeen.UTC().Format(time.RFC3339)
				}
				snap.Entities = append(snap.Entities, upsert("device", id, v, payload))
			}
			if err := rows.Err(); err != nil {
				return err
			}
			snap.Watermark = Cursor{TxID: horizon, Ordinal: -1, IssuedAt: now}
			return nil
		})
	return snap, err
}

func upsert(entityType string, id uuid.UUID, version int64, payload map[string]any) Change {
	b, _ := json.Marshal(payload)
	return Change{EntityType: entityType, EntityID: id, Operation: "upsert", Version: &version, Payload: b}
}
