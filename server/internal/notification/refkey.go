// Package notification은 알림 도메인이다. 지금은 사용자별 참조 키 발급만 있다.
// 발송, 수신자 산정, 문구는 docs/notification_design.md를 구현할 때 들어온다.
package notification

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RefKeySize는 설계 9 §4.3의 키 길이다.
const RefKeySize = 32

// SealPurposeRefKey는 참조 키 암호문의 associated data다.
const SealPurposeRefKey = "notification_ref_keys.ref_key"

// Box는 참조 키를 봉인하고 연다. secretbox가 구현한다.
type Box interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

// DB는 *pgxpool.Pool이 만족한다.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// EnsureRefKey는 사용자의 참조 키를 돌려준다. 없으면 만든다.
//
// 동시에 두 요청이 처음 만들어도 하나의 키에 수렴한다. INSERT ... ON CONFLICT DO
// NOTHING 뒤에 별도 문장으로 다시 읽는다. 두 기기가 서로 다른 키를 받으면 한쪽
// 기기의 알림 역조회가 영원히 실패한다.
//
// db는 pool이어야 한다(READ COMMITTED 자동 커밋). 다시 읽기는 새 문장 snapshot으로
// 상대가 커밋한 row를 봐야 하는데, REPEATABLE READ 트랜잭션 안에서는 보이지 않는다.
// INSERT와 SELECT를 한 문장(CTE)으로 합쳐도 같은 이유로 보이지 않는다.
//
// 설계 9 §9는 "발송 직전 생성"도 허용하지만, 기기가 알림보다 먼저 키를 받아 둬야
// 첫 알림부터 문구를 완성할 수 있으므로 조회 시점에 만든다.
func EnsureRefKey(ctx context.Context, db DB, box Box, userID uuid.UUID) ([]byte, error) {
	sealed, err := loadSealed(ctx, db, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		key := make([]byte, RefKeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, errors.New("notification: 참조 키 난수를 만들 수 없다")
		}
		fresh, err := box.Seal(key, SealPurposeRefKey)
		if err != nil {
			return nil, err
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO notification_ref_keys (user_id, ref_key_ciphertext)
			VALUES ($1, $2)
			ON CONFLICT (user_id) DO NOTHING`, userID, fresh); err != nil {
			return nil, err
		}
		// 다른 요청이 먼저 만들었으면 그 키가 나온다.
		sealed, err = loadSealed(ctx, db, userID)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return box.Open(sealed, SealPurposeRefKey)
}

func loadSealed(ctx context.Context, db DB, userID uuid.UUID) ([]byte, error) {
	var sealed []byte
	err := db.QueryRow(ctx,
		`SELECT ref_key_ciphertext FROM notification_ref_keys WHERE user_id = $1`, userID).Scan(&sealed)
	return sealed, err
}

// EncodeRefKey는 응답에 싣는 표현이다. base64url, 패딩 없음.
func EncodeRefKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }
