package syncfeed

import (
	"context"
	"encoding/json"
	"time"

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
			floors, err := party.EndedMembershipFloors(ctx, tx, userID, time.Now().Add(-Retention))
			if err != nil {
				return err
			}
			for _, pr := range floors {
				v, scope := pr.Version, pr.Scope
				snap.Entities = append(snap.Entities, Change{EntityType: pr.EntityType, EntityID: pr.ID, Operation: "tombstone", Version: &v, Payload: json.RawMessage("null"), ScopeMembershipID: &scope})
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
