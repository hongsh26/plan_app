package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/platform/mutation"
)

// device sync 투영(account_backend_design.md §7.1). devices 테이블을 만들고 폐기하는 auth가 소유한다. account
// 패키지는 auth를 import하므로 반대 방향으로 부를 수 없어서, account.DeviceProjection은 이 함수를 감싼다. 필드를
// 더할 때는 이 파일만 고친다.

// EntityDevice는 device sync entity type이다.
const EntityDevice = "device"

// DeviceRow는 기기 하나의 전체 투영이다.
type DeviceRow struct {
	ID      uuid.UUID
	Version int64
	Payload map[string]any
}

// Querier는 pgx.Tx와 pgxpool.Pool이 모두 만족한다.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// device 투영의 열. push token은 암호문이라도 싣지 않고 유무만 싣는다.
const deviceColumns = `
		SELECT id, platform, push_authorization, time_sensitive_setting,
		       push_token_ciphertext IS NOT NULL, revoked_at IS NOT NULL, last_seen_at, version
		  FROM devices`

func scanDevice(row pgx.Row) (DeviceRow, error) {
	var (
		id                   uuid.UUID
		platform, authz, tss string
		hasToken, revoked    bool
		lastSeen             *time.Time
		version              int64
	)
	if err := row.Scan(&id, &platform, &authz, &tss, &hasToken, &revoked, &lastSeen, &version); err != nil {
		return DeviceRow{}, err
	}
	payload := map[string]any{
		"platform": platform, "push_authorization": authz, "time_sensitive_setting": tss,
		"has_push_token": hasToken, "revoked": revoked, "last_seen_at": nil,
	}
	if lastSeen != nil {
		payload["last_seen_at"] = lastSeen.UTC().Format(time.RFC3339)
	}
	return DeviceRow{ID: id, Version: version, Payload: payload}, nil
}

// LoadDevice는 기기 하나의 투영이다. 폐기된 기기도 revoked=true로 나온다.
func LoadDevice(ctx context.Context, q Querier, deviceID uuid.UUID) (DeviceRow, error) {
	return scanDevice(q.QueryRow(ctx, deviceColumns+` WHERE id = $1`, deviceID))
}

// LoadLiveDevices는 사용자의 폐기되지 않은 기기 투영이다. bootstrap이 쓴다.
func LoadLiveDevices(ctx context.Context, q Querier, userID uuid.UUID) ([]DeviceRow, error) {
	rows, err := q.Query(ctx, deviceColumns+`
		 WHERE user_id = $1 AND revoked_at IS NULL
		 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceRow
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// emitDevice는 방금 만들거나 폐기한 기기의 전체 투영을 본인에게 sync 변경으로 남긴다. 같은 사용자의 다른 세션이
// 새 기기·폐기를 증분으로 받는다(증분 == bootstrap). 폐기는 tombstone이 아니라 revoked=true upsert다.
// mtx는 트랜잭션마다 mutation.NewTx로 하나만 만들어 그 트랜잭션의 모든 sync 변경이 같은 ordinal counter를 쓰게 한다
// (PK가 (txid, ordinal)이다).
func emitDevice(ctx context.Context, mtx *mutation.Tx, userID, deviceID uuid.UUID) error {
	d, err := LoadDevice(ctx, mtx, deviceID)
	if err != nil {
		return err
	}
	return mtx.EmitChange(ctx, mutation.Change{
		Recipient: userID, EntityType: EntityDevice, EntityID: d.ID, Version: &d.Version, Payload: d.Payload,
	})
}
