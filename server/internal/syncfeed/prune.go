package syncfeed

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention은 §7.1의 변경 피드 보존 기간이다. created_at 기준이다.
const Retention = 30 * 24 * time.Hour

// Prune은 before보다 오래된(created_at 기준) 변경을 지우고 정리 워터마크를 올린다.
// 지운 개수를 돌려준다. scheduler가 주기적으로 부른다(아직 연결되지 않았다).
//
// 삭제와 워터마크 갱신은 한 트랜잭션이다. 읽기는 한 문장에서 워터마크와 변경을 함께
// 보므로, 둘 중 하나만 반영된 상태를 볼 수 없다.
//
// 워터마크는 지운 row 중 가장 뒤의 위치다. created_at 순서와 txid 순서가 어긋나므로
// 지운 row 사이에 살아 남은 row가 있을 수 있다. 그런 row는 워터마크 이하의 cursor를
// 가진 클라이언트에게는 410으로 전체 재동기화를 요구하는 대가로 버려진다. 워터마크
// 뒤의 cursor를 가진 클라이언트는 이미 그 위치를 지났으므로 영향이 없다.
//
// 워터마크는 내려가지 않는다.
func Prune(ctx context.Context, pool *pgxpool.Pool, before time.Time) (int64, error) {
	var deleted int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// 동시에 두 정리가 돌지 않게 워터마크 줄을 먼저 잠근다.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM sync_prune_state FOR UPDATE`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			WITH d AS (
				DELETE FROM sync_changes WHERE created_at < $1
				RETURNING txid, ordinal
			), last AS (
				SELECT txid, ordinal FROM d ORDER BY txid DESC, ordinal DESC LIMIT 1
			), upd AS (
				UPDATE sync_prune_state s
				   SET pruned_through_txid = last.txid,
				       pruned_through_ordinal = last.ordinal,
				       pruned_at = now()
				  FROM last
				 WHERE (last.txid, last.ordinal) > (s.pruned_through_txid, s.pruned_through_ordinal)
			)
			SELECT count(*) FROM d`, before).Scan(&deleted)
	})
	return deleted, err
}
