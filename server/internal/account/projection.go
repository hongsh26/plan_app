package account

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// sync 변경 피드와 bootstrap snapshot이 싣는 entity projection이다.
//
// 증분 변경과 snapshot은 같은 entity에 대해 언제나 같은 모양의 **전체** projection을
// 싣는다. 바뀐 필드만 보내면 클라이언트가 병합 규칙을 알아야 하고, 로컬에 없는
// entity에 대한 일부 필드 upsert를 받았을 때 채울 방법이 없다. 클라이언트는 version이
// 더 큰 upsert를 받으면 payload로 로컬 entity를 통째로 바꾼다.
//
// 필드를 더할 때는 이 파일만 고친다. mutation과 bootstrap이 모두 여기를 부른다.

// Projection은 entity 하나의 전체 투영이다.
type Projection struct {
	EntityType string
	ID         uuid.UUID
	Version    int64
	Payload    map[string]any
}

// Querier는 pgx.Tx와 pgxpool.Pool이 모두 만족한다.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// UserProjection은 사용자 투영이다.
func UserProjection(ctx context.Context, q Querier, userID uuid.UUID) (Projection, error) {
	var name, status string
	var version int64
	if err := q.QueryRow(ctx,
		`SELECT display_name, status, version FROM users WHERE id = $1`, userID).Scan(&name, &status, &version); err != nil {
		return Projection{}, err
	}
	return Projection{EntityType: entityUser, ID: userID, Version: version,
		Payload: map[string]any{"display_name": name, "status": status}}, nil
}

// device 투영의 열. push token은 암호문이라도 싣지 않고 유무만 싣는다.
const deviceProjectionColumns = `
		SELECT id, platform, push_authorization, time_sensitive_setting,
		       push_token_ciphertext IS NOT NULL, revoked_at IS NOT NULL, last_seen_at, version
		  FROM devices`

func scanDeviceProjection(row pgx.Row) (Projection, error) {
	var (
		id                   uuid.UUID
		platform, authz, tss string
		hasToken, revoked    bool
		lastSeen             *time.Time
		version              int64
	)
	if err := row.Scan(&id, &platform, &authz, &tss, &hasToken, &revoked, &lastSeen, &version); err != nil {
		return Projection{}, err
	}
	payload := map[string]any{
		"platform": platform, "push_authorization": authz, "time_sensitive_setting": tss,
		"has_push_token": hasToken, "revoked": revoked, "last_seen_at": nil,
	}
	if lastSeen != nil {
		payload["last_seen_at"] = lastSeen.UTC().Format(time.RFC3339)
	}
	return Projection{EntityType: entityDevice, ID: id, Version: version, Payload: payload}, nil
}

// DeviceProjection은 기기 하나의 투영이다. 폐기된 기기도 revoked=true로 나온다.
func DeviceProjection(ctx context.Context, q Querier, deviceID uuid.UUID) (Projection, error) {
	return scanDeviceProjection(q.QueryRow(ctx, deviceProjectionColumns+` WHERE id = $1`, deviceID))
}

// LiveDeviceProjections는 사용자의 폐기되지 않은 기기 투영이다. bootstrap이 쓴다.
func LiveDeviceProjections(ctx context.Context, q Querier, userID uuid.UUID) ([]Projection, error) {
	rows, err := q.Query(ctx, deviceProjectionColumns+`
		 WHERE user_id = $1 AND revoked_at IS NULL
		 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Projection
	for rows.Next() {
		p, err := scanDeviceProjection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
