package syncfeed

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/account"
	"plantogether/server/internal/party"
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
// entity는 증분 변경과 같은 전체 투영이다(account/projection.go).
//
// 지금 볼 수 있는 entity는 본인 user, 폐기되지 않은 기기, 활성 Party 범위 entity다. Party entity는 증분과
// 같이 호출자의 활성 멤버십 ID를 scope로 싣는다. 제안 등은 각 도메인을 구현할 때 여기에 더한다.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) (Snapshot, error) {
	var snap Snapshot
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			// 첫 문장이 트랜잭션 snapshot을 정한다. horizon을 여기서 구해야 이후 읽는
			// snapshot 데이터와 같은 순간을 가리킨다.
			var hx, sysid, tli string
			if err := tx.QueryRow(ctx,
				`SELECT pg_snapshot_xmin(pg_current_snapshot())::text, `+epochSQL).Scan(&hx, &sysid, &tli); err != nil {
				return err
			}
			horizon, epoch, err := parseHead(hx, sysid, tli)
			if err != nil {
				return err
			}

			user, err := account.UserProjection(ctx, tx, userID)
			if err != nil {
				return err
			}
			devices, err := account.LiveDeviceProjections(ctx, tx, userID)
			if err != nil {
				return err
			}
			for _, pr := range append([]account.Projection{user}, devices...) {
				snap.Entities = append(snap.Entities, upsert(pr))
			}
			partyEntities, err := party.BootstrapProjections(ctx, tx, userID)
			if err != nil {
				return err
			}
			for _, pr := range partyEntities {
				b, err := json.Marshal(pr.Payload)
				if err != nil {
					return err
				}
				v := pr.Version
				scope := pr.Scope
				snap.Entities = append(snap.Entities, Change{EntityType: pr.EntityType, EntityID: pr.ID, Operation: "upsert", Version: &v, Payload: b, ScopeMembershipID: &scope})
			}
			// 최근 끝난 본인 멤버십은 하한 전용 tombstone 항목이다. 클라이언트는 row 없이 하한만 기록한다.
			floors, err := party.EndedMembershipFloors(ctx, tx, userID, Retention)
			if err != nil {
				return err
			}
			for _, pr := range floors {
				v, scope := pr.Version, pr.Scope
				snap.Entities = append(snap.Entities, Change{EntityType: pr.EntityType, EntityID: pr.ID, Operation: "tombstone", Version: &v, Payload: json.RawMessage("null"), ScopeMembershipID: &scope})
			}
			// watermark 아래의 변경은 다시 오지 않는데, 그보다 먼저 직렬화되고 txid가 watermark 이상인 같은 entity의
			// upsert는 다시 온다. snapshot 교체로 하한이 비워졌으므로 그런 entity의 하한을 싣는다. 대상은 watermark 뒤
			// upsert가 있는 entity뿐이라 작다.
			//   - Party 범위 entity: version 있는 tombstone의 최대 version. 끝난 entity는 같은 ID로 되살아나지 않는다.
			//     version 없는 scope tombstone 뒤에는 재가입이 같은 version을 다시 보내므로 upsert version을 하한으로
			//     쓰면 안 된다(그 경우는 규칙 (a)(b)가 맡는다).
			//   - 그 밖(user, device, availability_preference): version 없는 tombstone이 없고 version이 단조 증가하므로
			//     watermark 아래 이 수신자 행의 최대 version이 snapshot에 반영된 상태다. 기기 폐기는 revoked upsert라
			//     snapshot에서 빠지므로 이 하한이 없으면 먼저 직렬화된 옛 upsert가 폐기 기기를 되살린다.
			trows, err := tx.Query(ctx, `
				WITH redelivered AS (
				    SELECT DISTINCT entity_type, entity_id FROM sync_changes
				     WHERE recipient_user_id = $1 AND txid >= $2::text::xid8 AND operation = 'upsert'
				)
				SELECT t.entity_type, t.entity_id, max(t.entity_version), (array_agg(t.scope_membership_id ORDER BY t.txid DESC))[1]
				  FROM redelivered r
				  JOIN sync_changes t
				    ON t.recipient_user_id = $1 AND t.entity_type = r.entity_type AND t.entity_id = r.entity_id
				 WHERE t.entity_version IS NOT NULL
				   AND ((t.entity_type = ANY($3) AND t.operation = 'tombstone')
				        OR (t.entity_type <> ALL($3) AND t.txid < $2::text::xid8))
				 GROUP BY t.entity_type, t.entity_id`, userID, strconv.FormatUint(horizon, 10), party.ScopedEntityTypes())
			if err != nil {
				return err
			}
			for trows.Next() {
				var ch Change
				var v int64
				if err := trows.Scan(&ch.EntityType, &ch.EntityID, &v, &ch.ScopeMembershipID); err != nil {
					trows.Close()
					return err
				}
				ch.Operation, ch.Version, ch.Payload = "tombstone", &v, json.RawMessage("null")
				snap.Entities = append(snap.Entities, ch)
			}
			trows.Close()
			if err := trows.Err(); err != nil {
				return err
			}
			snap.Watermark = Cursor{Epoch: epoch, TxID: horizon, Ordinal: -1}
			return nil
		})
	return snap, err
}

func upsert(pr account.Projection) Change {
	b, _ := json.Marshal(pr.Payload)
	v := pr.Version
	return Change{EntityType: pr.EntityType, EntityID: pr.ID, Operation: "upsert", Version: &v, Payload: b}
}
