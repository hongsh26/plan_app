package calendar

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Querier는 pgxpool.Pool과 pgx.Tx가 만족한다.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// FreshUsers는 freshness 기준(source_status가 ready이고 마지막 정상 sync가 6시간 이내)을 충족하는
// 사용자 집합을 돌려준다(설계 3 §9). 목록에 없는 사용자는 연결이 없거나 stale·denied·revoked·error 등이다.
// 가능 시간 검색은 모든 활성 멤버가 여기 있을 때만 확정 결과를 반환한다. destination 상태는 보지 않는다.
func FreshUsers(ctx context.Context, q Querier, userIDs []uuid.UUID, now time.Time) (map[uuid.UUID]bool, error) {
	fresh := make(map[uuid.UUID]bool, len(userIDs))
	if len(userIDs) == 0 {
		return fresh, nil
	}
	rows, err := q.Query(ctx, `
		SELECT user_id FROM calendar_connections
		 WHERE user_id = ANY($1)
		   AND source_status = 'ready'
		   AND last_completed_sync_at > $2`, userIDs, now.Add(-FreshnessWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		fresh[id] = true
	}
	return fresh, rows.Err()
}
