// Package mutation은 account_backend_design.md §6·§7·§8이 모든 mutation에
// 요구하는 규칙을 한 트랜잭션 안에서 강제하는 helper다.
//
//   - Idempotency-Key: 같은 기기가 같은 키로 다시 보내면 원래 결과를 돌려준다.
//     요청 본문이 다르면 409 idempotency_mismatch (§6, §7.4, §8)
//   - 낙관적 동시성: expected_version 불일치는 409 version_conflict와 현재 version
//   - sync 변경 피드: sync_changes의 ordinal은 이 helper만 발급한다 (§7.1)
//   - outbox: 도메인 상태 전이와 같은 트랜잭션에서 커밋한다 (§8 필수 불변식)
//
// 도메인 코드는 Run에 함수 하나를 넘기고, 그 안에서 Tx로 SQL을 실행하고
// EmitChange·Enqueue를 부른다. 함수가 오류를 돌려주면 도메인 변경, sync 변경,
// outbox job, idempotency 기록이 전부 함께 롤백된다.
//
// 잠금 순서: 이 helper는 트랜잭션 맨 앞에서 idempotency_keys row를 잡는다.
// 인증 경로의 규약(users → devices → sessions, internal/auth lockOrder)과 합치면
// 전체 순서는
//
//	idempotency_keys → users → devices → sessions → sync_changes·outbox_jobs
//
// 다. idempotency row는 (user, device, key) 단위라 서로 다른 요청끼리 경합하지 않는다.
package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IdempotencyTTL은 §8의 멱등성 결과 보존 기간이다.
const IdempotencyTTL = 30 * 24 * time.Hour

// ErrIdempotencyMismatch는 같은 Idempotency-Key를 다른 요청에 재사용했을 때다.
var ErrIdempotencyMismatch = errors.New("mutation: 같은 Idempotency-Key가 다른 요청에 쓰였다")

// VersionConflictError는 expected_version이 현재 version과 다를 때다.
// 클라이언트는 Current로 서버 상태를 다시 받아 사용자에게 보여준다(§7.4).
type VersionConflictError struct {
	Current int64
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("mutation: version 충돌 (현재 %d)", e.Current)
}

// Request는 mutation 하나의 멱등성 식별 정보다.
type Request struct {
	UserID   uuid.UUID
	DeviceID uuid.UUID
	Key      string
	// Hash는 요청의 지문이다. RequestHash로 만든다.
	Hash []byte
}

// RequestHash는 method, route 패턴, 원본 본문 바이트로 요청 지문을 만든다.
//
// route 패턴을 넣는 이유: 같은 키를 다른 endpoint에 재사용하면 본문이 같아도
// 다른 요청이다. 원본 바이트를 쓰는 이유: JSON을 정규화하면 키 순서·공백 차이를
// 흡수할 수 있지만, 정규화 규칙 자체가 계약이 되고 버그 표면이 된다. 클라이언트는
// 재전송 때 저장해 둔 같은 바이트를 보내면 된다(§7.3 로컬 mutation queue가
// payload를 보관한다).
func RequestHash(method, pattern string, body []byte) []byte {
	h := sha256.New()
	// 구분자로 길이를 앞에 붙인다. "a"+"bc"와 "ab"+"c"가 같은 해시가 되지 않게 한다.
	for _, part := range [][]byte{[]byte(method), []byte(pattern), body} {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		h.Write(part)
	}
	return h.Sum(nil)
}

// Result는 저장되는 최소 결과 포인터다(§8 "멱등성 결과 보존"). 응답 본문은
// 저장하지 않는다. 재전송 응답은 이 포인터로 현재 리소스를 다시 읽어 만든다.
type Result struct {
	StatusCode      int
	ResourceType    string
	ResourceID      uuid.UUID
	ResourceVersion int64
}

// Outcome은 Run의 결과다.
type Outcome struct {
	Result
	// Replayed는 이번 요청이 실행되지 않고 저장된 결과를 돌려받았는지다.
	Replayed bool
}

// Tx는 mutation 트랜잭션이다. pgx.Tx의 모든 메서드를 그대로 쓸 수 있다.
type Tx struct {
	pgx.Tx
	ordinal int32
}

// Change는 sync_changes 한 줄이다.
type Change struct {
	Recipient  uuid.UUID
	EntityType string
	EntityID   uuid.UUID
	// Version이 있으면 upsert, nil이면 tombstone이다. 스키마 CHECK가 둘을 묶는다.
	Version *int64
	// Payload는 최소 projection이다(§7.1). 캘린더 상세와 token을 넣지 않는다.
	Payload any
}

// EmitChange는 sync 변경을 기록한다. ordinal은 트랜잭션 안에서 0부터 순서대로
// 발급된다. 수신자가 여러 명이어도 하나의 counter를 쓴다. PK가 (txid, ordinal)
// 이므로 수신자별로 세면 PK 위반이다.
func (t *Tx) EmitChange(ctx context.Context, c Change) error {
	payload, err := json.Marshal(c.Payload)
	if err != nil {
		return fmt.Errorf("mutation: sync payload를 직렬화할 수 없다: %w", err)
	}
	op := "upsert"
	if c.Version == nil {
		op = "tombstone"
	}
	if _, err := t.Exec(ctx, `
		INSERT INTO sync_changes (ordinal, recipient_user_id, entity_type, entity_id, operation, entity_version, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		t.ordinal, c.Recipient, c.EntityType, c.EntityID, op, c.Version, payload); err != nil {
		return err
	}
	t.ordinal++
	return nil
}

// Job은 outbox job 하나다.
type Job struct {
	Type    string
	Payload any
	// DedupeKey가 있으면 같은 (Type, DedupeKey)의 활성 job은 하나만 존재한다.
	// 이미 활성 job이 있으면 새로 만들지 않는다.
	DedupeKey *string
}

// Enqueue는 outbox job을 만든다.
func (t *Tx) Enqueue(ctx context.Context, j Job) error {
	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return fmt.Errorf("mutation: job payload를 직렬화할 수 없다: %w", err)
	}
	// ON CONFLICT에 partial unique index의 조건을 그대로 적어야 추론된다.
	_, err = t.Exec(ctx, `
		INSERT INTO outbox_jobs (id, type, payload, dedupe_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (type, dedupe_key) WHERE status IN ('pending', 'running', 'retryable_failed')
		DO NOTHING`,
		uuid.New(), j.Type, payload, j.DedupeKey)
	return err
}

// CheckVersion은 expected와 current가 다르면 VersionConflictError를 돌려준다.
// 도메인 코드가 row를 FOR UPDATE로 읽은 뒤 부른다.
func CheckVersion(expected, current int64) error {
	if expected != current {
		return &VersionConflictError{Current: current}
	}
	return nil
}

// Func는 도메인 mutation이다. 성공하면 저장할 결과 포인터를 돌려준다.
type Func func(ctx context.Context, tx *Tx) (Result, error)

// Run은 req의 멱등성을 보장하며 fn을 한 트랜잭션에서 실행한다.
//
// 호출 전에 인증과 권한 평가가 끝나 있어야 한다. §8은 재전송에서도 현재 인증·
// 권한을 먼저 평가하라고 한다. 권한을 잃은 사용자가 과거 성공 결과를 돌려받으면
// 안 되기 때문이다.
//
// 흐름:
//  1. idempotency row를 INSERT로 먼저 점유한다. 같은 키의 동시 요청은 unique
//     index에서 앞 트랜잭션이 끝나기를 기다린다.
//  2. 이미 있으면(앞 요청이 커밋됨) hash를 비교한다. 같으면 저장된 결과를
//     돌려주고(Replayed), 다르면 ErrIdempotencyMismatch다. 만료됐으면 지우고
//     새 요청으로 취급한다.
//  3. fn을 실행하고 결과를 idempotency row에 기록한 뒤 커밋한다.
//
// fn이 실패하면 idempotency row도 롤백된다. 실패는 기록하지 않으므로 같은 키로
// 재시도하면 다시 실행된다. version 충돌처럼 결정적인 실패는 다시 실행해도 같은
// 결과가 나오고, 일시 장애는 재시도로 회복된다.
func Run(ctx context.Context, pool *pgxpool.Pool, req Request, now time.Time, fn Func) (Outcome, error) {
	if req.Key == "" || len(req.Hash) == 0 {
		return Outcome{}, errors.New("mutation: Idempotency-Key와 요청 hash가 필요하다")
	}

	var out Outcome
	err := pgx.BeginFunc(ctx, pool, func(ptx pgx.Tx) error {
		claimed, err := claim(ctx, ptx, req, now)
		if err != nil {
			return err
		}
		if !claimed {
			stored, err := loadStored(ctx, ptx, req, now)
			switch {
			case errors.Is(err, errExpired):
				// 만료된 기록은 없는 것과 같다. 지우고 다시 점유한다.
				if _, err := ptx.Exec(ctx, `
					DELETE FROM idempotency_keys
					 WHERE user_id = $1 AND device_id = $2 AND key = $3`,
					req.UserID, req.DeviceID, req.Key); err != nil {
					return err
				}
				if claimed, err = claim(ctx, ptx, req, now); err != nil {
					return err
				}
				if !claimed {
					return errors.New("mutation: 만료된 idempotency 기록을 교체하지 못했다")
				}
			case err != nil:
				return err
			default:
				out = Outcome{Result: stored, Replayed: true}
				return nil
			}
		}

		tx := &Tx{Tx: ptx}
		res, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := ptx.Exec(ctx, `
			UPDATE idempotency_keys
			   SET status_code = $4, resource_type = $5, resource_id = $6,
			       resource_version = $7, updated_at = now()
			 WHERE user_id = $1 AND device_id = $2 AND key = $3`,
			req.UserID, req.DeviceID, req.Key,
			res.StatusCode, res.ResourceType, res.ResourceID, res.ResourceVersion); err != nil {
			return err
		}
		out = Outcome{Result: res}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

// claim은 idempotency row를 새로 만들면 true다. 같은 키가 이미 있으면 false다.
//
// ON CONFLICT DO NOTHING은 같은 키를 INSERT 중인 다른 트랜잭션이 있으면 그
// 트랜잭션이 끝나기를 기다린다. 상대가 커밋했으면 false(충돌), 롤백했으면 이
// INSERT가 성공한다. 그래서 false일 때 보이는 row는 언제나 커밋된 결과다.
func claim(ctx context.Context, tx pgx.Tx, req Request, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (id, user_id, device_id, key, request_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, device_id, key) DO NOTHING`,
		uuid.New(), req.UserID, req.DeviceID, req.Key, req.Hash, now.Add(IdempotencyTTL))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

var errExpired = errors.New("mutation: 만료된 idempotency 기록")

func loadStored(ctx context.Context, tx pgx.Tx, req Request, now time.Time) (Result, error) {
	var (
		hash      []byte
		status    *int32
		rtype     *string
		rid       *uuid.UUID
		rversion  *int64
		expiresAt time.Time
	)
	// FOR UPDATE: 만료 기록을 지우고 다시 쓰는 경로와 재전송 판정이 같은 row를
	// 동시에 다루지 않게 한다.
	err := tx.QueryRow(ctx, `
		SELECT request_hash, status_code, resource_type, resource_id, resource_version, expires_at
		  FROM idempotency_keys
		 WHERE user_id = $1 AND device_id = $2 AND key = $3
		   FOR UPDATE`,
		req.UserID, req.DeviceID, req.Key).Scan(&hash, &status, &rtype, &rid, &rversion, &expiresAt)
	if err != nil {
		return Result{}, err
	}
	if !now.Before(expiresAt) {
		return Result{}, errExpired
	}
	if string(hash) != string(req.Hash) {
		return Result{}, ErrIdempotencyMismatch
	}
	if status == nil || rtype == nil || rid == nil || rversion == nil {
		// Run은 결과를 기록한 뒤에만 커밋하므로 도달하지 않는다. 도달했다면 다른
		// 경로가 이 표에 직접 썼다는 뜻이다.
		return Result{}, errors.New("mutation: 결과가 기록되지 않은 idempotency row가 커밋돼 있다")
	}
	return Result{
		StatusCode:      int(*status),
		ResourceType:    *rtype,
		ResourceID:      *rid,
		ResourceVersion: *rversion,
	}, nil
}
