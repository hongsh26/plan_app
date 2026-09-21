// Package test는 실제 PostgreSQL에 대한 통합 테스트를 담는다.
//
// account_backend_design.md §8의 "필수 DB 불변식"은 애플리케이션 코드가 아니라
// DB 제약이 지켜야 하는 것들이다. 제약이 실제로 위반을 거부하는지는 실행 중인
// PostgreSQL 없이 확인할 수 없으므로 여기서만 검증한다.
//
// 실행 방법:
//
//	docker compose up -d
//	go run ./cmd/api -migrate up      # MIGRATION_DATABASE_URL 필요
//	TEST_DATABASE_URL=... go test ./test/...
//
// TEST_DATABASE_URL이 없으면 skip한다. REQUIRE_DB_TESTS=1이면 skip 대신
// 실패한다. skip이 통과로 위장되지 않게 CI에서 이 변수를 켠다.
package test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL SQLSTATE. 제약 위반을 "그냥 오류가 났다"가 아니라 "그 제약이
// 거부했다"로 확인하기 위해 쓴다.
const (
	pgUniqueViolation = "23505"
	pgCheckViolation  = "23514"
)

// connect는 테스트용 연결을 연다. DB가 없으면 skip하거나, REQUIRE_DB_TESTS가
// 켜져 있으면 실패한다.
func connect(t *testing.T) *pgx.Conn {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	required := os.Getenv("REQUIRE_DB_TESTS") == "1"

	if url == "" {
		msg := "TEST_DATABASE_URL이 설정되지 않아 PostgreSQL 통합 테스트를 실행할 수 없다"
		if required {
			t.Fatal(msg + " (REQUIRE_DB_TESTS=1이므로 skip하지 않는다)")
		}
		t.Skip(msg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		msg := "TEST_DATABASE_URL로 PostgreSQL에 연결할 수 없다"
		if required {
			t.Fatalf("%s: %v", msg, err)
		}
		t.Skipf("%s: %v", msg, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// begin은 테스트가 끝나면 반드시 rollback되는 트랜잭션을 연다. 테스트가 서로의
// 데이터를 보지 않게 한다.
func begin(t *testing.T, conn *pgx.Conn) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("트랜잭션을 시작할 수 없다: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// assertUniqueViolation은 제약 위반이 실제로 일어났는지 확인한다. 단순히
// "오류가 났다"가 아니라 unique 제약이 거부했음을 확인해야, 오타 때문에 난
// 다른 오류를 성공으로 오해하지 않는다.
func assertUniqueViolation(t *testing.T, err error, wantConstraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("제약 %s가 위반을 거부하지 않고 삽입을 허용했다", wantConstraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("PostgreSQL 오류가 아니다: %v", err)
	}
	if pgErr.Code != pgUniqueViolation {
		t.Fatalf("SQLSTATE = %s, want %s (unique_violation): %v", pgErr.Code, pgUniqueViolation, err)
	}
	if pgErr.ConstraintName != wantConstraint {
		t.Errorf("거부한 제약 = %q, want %q", pgErr.ConstraintName, wantConstraint)
	}
}

// assertCheckViolation은 CHECK constraint가 실제로 거부했는지 확인한다.
//
// "오류가 났다"만 보면 제약이 통째로 사라져도 초록불이 된다. 열 이름이 바뀌어
// undefined_column(42703)이 나거나 타입 오류가 나도 err != nil이기 때문이다.
// SQLSTATE 23514와 제약 이름까지 봐야 그 CHECK가 거부했음이 성립한다.
func assertCheckViolation(t *testing.T, err error, wantConstraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("제약 %s가 위반을 거부하지 않고 삽입을 허용했다", wantConstraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("PostgreSQL 오류가 아니다: %v", err)
	}
	if pgErr.Code != pgCheckViolation {
		t.Fatalf("SQLSTATE = %s, want %s (check_violation). "+
			"제약이 아니라 다른 이유로 실패했다: %v", pgErr.Code, pgCheckViolation, err)
	}
	if pgErr.ConstraintName != wantConstraint {
		t.Errorf("거부한 제약 = %q, want %q", pgErr.ConstraintName, wantConstraint)
	}
}

// insertUser는 테스트용 사용자를 만든다.
func insertUser(t *testing.T, tx pgx.Tx) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := tx.Exec(context.Background(),
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`, id, "테스트 사용자")
	if err != nil {
		t.Fatalf("사용자를 만들 수 없다: %v", err)
	}
	return id
}

// insertDevice는 테스트용 기기를 만든다.
func insertDevice(t *testing.T, tx pgx.Tx, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := tx.Exec(context.Background(),
		`INSERT INTO devices (id, user_id, platform) VALUES ($1, $2, 'ios')`, id, userID)
	if err != nil {
		t.Fatalf("기기를 만들 수 없다: %v", err)
	}
	return id
}

// §8 필수 불변식: 외부 identity 하나는 사용자 하나에만 연결된다.
// 이 제약이 없으면 같은 Apple 계정으로 두 번 로그인할 때 중복 사용자가 생긴다
// (§14 인증 인수 조건).
func TestAuthIdentityProviderSubjectIsUnique(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	userA := insertUser(t, tx)
	userB := insertUser(t, tx)
	const subject = "000123.abcdef0123456789.0000"

	_, err := tx.Exec(ctx,
		`INSERT INTO auth_identities (id, user_id, provider, provider_subject)
		 VALUES ($1, $2, 'apple', $3)`, uuid.New(), userA, subject)
	if err != nil {
		t.Fatalf("첫 identity 삽입이 실패했다: %v", err)
	}

	// 같은 (provider, provider_subject)를 다른 사용자에 붙이려는 시도.
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint를 만들 수 없다: %v", err)
	}
	_, err = sp.Exec(ctx,
		`INSERT INTO auth_identities (id, user_id, provider, provider_subject)
		 VALUES ($1, $2, 'apple', $3)`, uuid.New(), userB, subject)
	assertUniqueViolation(t, err, "auth_identities_provider_subject_key")
	_ = sp.Rollback(ctx)

	// 다른 subject는 허용된다. 제약이 너무 넓지 않은지 확인한다.
	_, err = tx.Exec(ctx,
		`INSERT INTO auth_identities (id, user_id, provider, provider_subject)
		 VALUES ($1, $2, 'apple', $3)`, uuid.New(), userB, subject+".other")
	if err != nil {
		t.Fatalf("다른 subject 삽입이 거부됐다: %v", err)
	}
}

// §8 필수 불변식: 멱등성 키는 (user_id, device_id, key)당 하나다.
// 이 제약이 오프라인 재전송에서 제안이 두 번 생기는 것을 막는다 (§14).
func TestIdempotencyKeyIsUniquePerUserAndDevice(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	userID := insertUser(t, tx)
	deviceA := insertDevice(t, tx, userID)
	deviceB := insertDevice(t, tx, userID)
	const key = "b3f1c2d4-0000-4000-8000-000000000001"

	insert := func(q pgx.Tx, device uuid.UUID) error {
		_, err := q.Exec(ctx,
			`INSERT INTO idempotency_keys (id, user_id, device_id, key, request_hash, expires_at)
			 VALUES ($1, $2, $3, $4, $5, now() + interval '30 days')`,
			uuid.New(), userID, device, key, []byte("request-hash"))
		return err
	}

	if err := insert(tx, deviceA); err != nil {
		t.Fatalf("첫 멱등성 키 삽입이 실패했다: %v", err)
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint를 만들 수 없다: %v", err)
	}
	assertUniqueViolation(t, insert(sp, deviceA), "idempotency_keys_user_device_key_key")
	_ = sp.Rollback(ctx)

	// 같은 키라도 기기가 다르면 별개다. 멱등성은 기기별 계약이다 (§6).
	if err := insert(tx, deviceB); err != nil {
		t.Fatalf("다른 기기의 같은 키가 거부됐다: %v", err)
	}
}

// §8 필수 불변식: 활성 outbox dedupe는
// (type, dedupe_key) WHERE status IN ('pending','running','retryable_failed').
//
// partial unique의 핵심은 양쪽 모두다. 활성 상태에서는 거부하고, 종료 상태
// (succeeded, dead) 뒤에는 같은 key를 다시 쓸 수 있어야 한다. 후자가 없으면
// 같은 알림을 다시는 보낼 수 없게 된다.
func TestOutboxDedupeKeyBlocksOnlyActiveJobs(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	insert := func(q pgx.Tx, status, dedupeKey string) (uuid.UUID, error) {
		id := uuid.New()
		_, err := q.Exec(ctx,
			`INSERT INTO outbox_jobs (id, type, payload, status, dedupe_key)
			 VALUES ($1, 'party_invite_accepted', '{}'::jsonb, $2, $3)`,
			id, status, dedupeKey)
		return id, err
	}

	// 활성 상태 세 가지는 모두 같은 dedupe_key를 막아야 한다.
	for _, activeStatus := range []string{"pending", "running", "retryable_failed"} {
		t.Run("활성_"+activeStatus+"는_중복을_막는다", func(t *testing.T) {
			tx := begin(t, conn)
			dedupeKey := "dedupe-" + uuid.NewString()

			if _, err := insert(tx, activeStatus, dedupeKey); err != nil {
				t.Fatalf("첫 job 삽입이 실패했다: %v", err)
			}

			sp, err := tx.Begin(ctx)
			if err != nil {
				t.Fatalf("savepoint를 만들 수 없다: %v", err)
			}
			_, err = insert(sp, "pending", dedupeKey)
			assertUniqueViolation(t, err, "outbox_jobs_active_dedupe_key")
			_ = sp.Rollback(ctx)
		})
	}

	// 종료 상태 두 가지는 같은 dedupe_key의 재삽입을 허용해야 한다.
	for _, terminalStatus := range []string{"succeeded", "dead"} {
		t.Run("종료_"+terminalStatus+"_뒤에는_재삽입을_허용한다", func(t *testing.T) {
			tx := begin(t, conn)
			dedupeKey := "dedupe-" + uuid.NewString()

			firstID, err := insert(tx, "pending", dedupeKey)
			if err != nil {
				t.Fatalf("첫 job 삽입이 실패했다: %v", err)
			}

			if _, err := tx.Exec(ctx,
				`UPDATE outbox_jobs SET status = $1 WHERE id = $2`, terminalStatus, firstID); err != nil {
				t.Fatalf("job을 %s로 바꿀 수 없다: %v", terminalStatus, err)
			}

			if _, err := insert(tx, "pending", dedupeKey); err != nil {
				t.Fatalf("%s 뒤 같은 dedupe_key 재삽입이 거부됐다. "+
					"partial unique의 WHERE 절이 종료 상태를 제외하지 못하고 있다: %v",
					terminalStatus, err)
			}
		})
	}

	// dedupe_key가 NULL이면 중복 판정 대상이 아니다. 모든 job이 dedupe를
	// 요구하지는 않는다 (§9).
	t.Run("dedupe_key가_NULL이면_제한하지_않는다", func(t *testing.T) {
		tx := begin(t, conn)
		for i := 0; i < 3; i++ {
			if _, err := tx.Exec(ctx,
				`INSERT INTO outbox_jobs (id, type, payload, status, dedupe_key)
				 VALUES ($1, 'account_deletion', '{}'::jsonb, 'pending', NULL)`,
				uuid.New()); err != nil {
				t.Fatalf("dedupe_key가 NULL인 job %d개째 삽입이 실패했다: %v", i+1, err)
			}
		}
	})
}

// 개정된 §7.1의 순서 키를 P0 비용으로 지킨다.
//
// 같은 트랜잭션에서 쓴 두 변경은 같은 txid를 갖고 ordinal로만 구분된다.
// 이것이 성립해야 (txid, ordinal) cursor가 트랜잭션 경계와 어긋나지 않는다.
// 순서 역전 회귀 테스트 전체(§15)는 읽기 쿼리가 필요하므로 P6이다.
func TestSyncChangesShareTxidWithinTransaction(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	userID := insertUser(t, tx)

	insertChange := func(q pgx.Tx, ordinal int) error {
		_, err := q.Exec(ctx,
			`INSERT INTO sync_changes
			   (ordinal, recipient_user_id, entity_type, entity_id, operation, entity_version, payload)
			 VALUES ($1, $2, 'party', $3, 'upsert', 1, '{}'::jsonb)`,
			ordinal, userID, uuid.New())
		return err
	}

	if err := insertChange(tx, 0); err != nil {
		t.Fatalf("ordinal 0 삽입이 실패했다: %v", err)
	}
	if err := insertChange(tx, 1); err != nil {
		t.Fatalf("ordinal 1 삽입이 실패했다: %v", err)
	}

	var distinctTxids int
	if err := tx.QueryRow(ctx,
		`SELECT count(DISTINCT txid) FROM sync_changes WHERE recipient_user_id = $1`,
		userID).Scan(&distinctTxids); err != nil {
		t.Fatalf("txid를 조회할 수 없다: %v", err)
	}
	if distinctTxids != 1 {
		t.Fatalf("한 트랜잭션의 변경 2건이 서로 다른 txid %d개를 가졌다. "+
			"txid 기본값이 pg_current_xact_id()가 아닌 것으로 보인다", distinctTxids)
	}

	// PK (txid, ordinal)이 같은 트랜잭션 안의 ordinal 중복을 거부해야 한다.
	// 거부하지 않으면 cursor가 두 변경을 구분할 수 없어 하나가 유실된다.
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint를 만들 수 없다: %v", err)
	}
	assertUniqueViolation(t, insertChange(sp, 0), "sync_changes_pkey")
	_ = sp.Rollback(ctx)
}

// §8: sync_changes와 audit_events는 append-only이므로 version이 없다.
// 이후 단계가 무심코 version을 붙이면 여기서 걸린다.
func TestAppendOnlyTablesHaveNoVersionColumn(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	for _, table := range []string{"sync_changes", "audit_events"} {
		var count int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'version'`,
			table).Scan(&count); err != nil {
			t.Fatalf("%s의 열을 조회할 수 없다: %v", table, err)
		}
		if count != 0 {
			t.Errorf("append-only 테이블 %s에 version 열이 있다 (§8 위반)", table)
		}
	}
}

// 상태 문자열은 CHECK constraint로 허용 값 밖의 입력을 거부한다 (§8).
func TestStatusCheckConstraintsRejectUnknownValues(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	t.Run("users.status", func(t *testing.T) {
		tx := begin(t, conn)
		_, err := tx.Exec(ctx,
			`INSERT INTO users (id, display_name, status) VALUES ($1, '테스트', 'pending_approval')`,
			uuid.New())
		assertCheckViolation(t, err, "users_status_check")
	})

	t.Run("devices.push_authorization", func(t *testing.T) {
		tx := begin(t, conn)
		userID := insertUser(t, tx)
		_, err := tx.Exec(ctx,
			`INSERT INTO devices (id, user_id, platform, push_authorization)
			 VALUES ($1, $2, 'ios', 'maybe')`, uuid.New(), userID)
		assertCheckViolation(t, err, "devices_push_authorization_check")
	})

	t.Run("outbox_jobs.status", func(t *testing.T) {
		tx := begin(t, conn)
		_, err := tx.Exec(ctx,
			`INSERT INTO outbox_jobs (id, type, payload, status)
			 VALUES ($1, 'x', '{}'::jsonb, 'cancelled')`, uuid.New())
		assertCheckViolation(t, err, "outbox_jobs_status_check")
	})
}

// users.deleted_at은 §10의 삭제 파이프라인을 막지 않아야 한다.
//
// 삭제는 여러 단계에 걸쳐 진행된다(§10): deletion_requested -> deleting에서
// 실제 삭제 작업이 돌고 -> deleted로 끝난다. deleted_at을 deleted 상태에만
// 허용하면 실제 삭제가 도는 deleting 단계에 시각을 기록할 수 없다.
func TestUserDeletedAtAllowsDeletionPipeline(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	insert := func(q pgx.Tx, status string, deletedAt any) error {
		_, err := q.Exec(ctx,
			`INSERT INTO users (id, display_name, status, deleted_at) VALUES ($1, '테스트', $2, $3)`,
			uuid.New(), status, deletedAt)
		return err
	}

	// 삭제가 진행 중이거나 끝난 계정은 deleted_at을 가질 수 있다.
	for _, status := range []string{"deleting", "deleted"} {
		t.Run(status+"은_deleted_at을_허용한다", func(t *testing.T) {
			tx := begin(t, conn)
			if err := insert(tx, status, time.Now()); err != nil {
				t.Fatalf("%s 상태에 deleted_at을 기록할 수 없다. "+
					"§10의 삭제 파이프라인이 막힌다: %v", status, err)
			}
		})
	}

	// 삭제 전 상태에는 deleted_at이 없어야 한다. 있으면 §14의 "삭제된 계정은
	// 보호 API를 쓸 수 없다"를 판정하는 근거가 모순된다.
	for _, status := range []string{"active", "disabled", "deletion_requested"} {
		t.Run(status+"은_deleted_at을_거부한다", func(t *testing.T) {
			tx := begin(t, conn)
			assertCheckViolation(t, insert(tx, status, time.Now()),
				"users_deleted_at_requires_terminal_status_check")
		})
	}

	// 삭제 파이프라인 중간 단계는 deleted_at 없이도 성립한다.
	t.Run("deleting은_deleted_at_없이도_허용한다", func(t *testing.T) {
		tx := begin(t, conn)
		if err := insert(tx, "deleting", nil); err != nil {
			t.Fatalf("deleted_at 없는 deleting이 거부됐다: %v", err)
		}
	})
}

// §8 FK 삭제 정책: 기기와 세션은 cascade다. 계정 삭제 시 세션이 남으면
// §14의 "삭제 요청 즉시 모든 세션 폐기"가 깨진다.
func TestDeviceAndSessionCascadeOnUserDelete(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	userID := insertUser(t, tx)
	deviceID := insertDevice(t, tx, userID)

	if _, err := tx.Exec(ctx,
		`INSERT INTO sessions (id, user_id, device_id, token_family_id, refresh_token_hash, expires_at)
		 VALUES ($1, $2, $3, $4, $5, now() + interval '30 days')`,
		uuid.New(), userID, deviceID, uuid.New(), []byte(uuid.NewString())); err != nil {
		t.Fatalf("세션을 만들 수 없다: %v", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("사용자를 삭제할 수 없다: %v", err)
	}

	for _, table := range []string{"devices", "sessions"} {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE user_id = $1`, userID).Scan(&count); err != nil {
			t.Fatalf("%s를 조회할 수 없다: %v", table, err)
		}
		if count != 0 {
			t.Errorf("사용자 삭제 후 %s에 %d건이 남았다 (cascade가 걸리지 않았다)", table, count)
		}
	}
}

// §10 7단계: `deleted` 전환과 `auth_identities` row 물리 삭제를 같은 트랜잭션에서
// 커밋하면 같은 Apple subject로 재가입할 수 있고 새 user_id를 받는다 (§14).
//
// 이 테스트가 지키는 것은 index 자체가 아니라 index와 삭제 파이프라인 사이의
// 의존이다. `auth_identities_provider_subject_key`는 조건 없는 unique이므로,
// 삭제 파이프라인이 row를 남기는 순간 같은 Apple 계정은 영원히 재가입할 수 없다.
// 삭제 단계에서 물리 삭제가 빠지면 이 테스트가 실패한다.
func TestDeletedAccountReleasesAppleSubjectForResignup(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	const subject = "000777.fedcba9876543210.0001"

	oldUser := insertUser(t, tx)
	oldIdentity := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_identities (id, user_id, provider, provider_subject)
		 VALUES ($1, $2, 'apple', $3)`, oldIdentity, oldUser, subject); err != nil {
		t.Fatalf("최초 가입 identity 삽입이 실패했다: %v", err)
	}

	// 삭제가 끝나기 전(deletion_requested / deleting)에는 같은 subject로 새 계정을
	// 만들 수 없다. §5.3에 따라 이 구간의 로그인은 차단된 기존 계정으로 해석된다.
	if _, err := tx.Exec(ctx,
		`UPDATE users SET status = 'deleting', deleted_at = now() WHERE id = $1`,
		oldUser); err != nil {
		t.Fatalf("deleting 전환이 실패했다: %v", err)
	}

	midUser := insertUser(t, tx)
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint를 만들 수 없다: %v", err)
	}
	_, err = sp.Exec(ctx,
		`INSERT INTO auth_identities (id, user_id, provider, provider_subject)
		 VALUES ($1, $2, 'apple', $3)`, uuid.New(), midUser, subject)
	assertUniqueViolation(t, err, "auth_identities_provider_subject_key")
	_ = sp.Rollback(ctx)

	// §10 7단계: deleted 전환과 identity 물리 삭제가 한 트랜잭션에서 커밋된다.
	if _, err := tx.Exec(ctx,
		`UPDATE users SET status = 'deleted' WHERE id = $1`, oldUser); err != nil {
		t.Fatalf("deleted 전환이 실패했다: %v", err)
	}
	ct, err := tx.Exec(ctx, `DELETE FROM auth_identities WHERE user_id = $1`, oldUser)
	if err != nil {
		t.Fatalf("identity 물리 삭제가 실패했다: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("identity 삭제 건수가 1이 아니다: %d", ct.RowsAffected())
	}

	// 재가입이 성공하고 새 user_id를 받는다.
	newUser := insertUser(t, tx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_identities (id, user_id, provider, provider_subject)
		 VALUES ($1, $2, 'apple', $3)`, uuid.New(), newUser, subject); err != nil {
		t.Fatalf("삭제 완료 후 같은 Apple subject 재가입이 거부됐다. "+
			"§10 7단계의 identity 물리 삭제가 빠졌거나 index가 바뀌었다: %v", err)
	}
	if newUser == oldUser {
		t.Fatal("재가입이 기존 user_id를 재사용했다. §10 7단계는 새 계정 생성이다")
	}

	// 삭제된 사용자는 identity를 보유하지 않는다. §8 불변식의 "활성" 한정이
	// 조건 없는 unique로도 성립하는 근거다.
	var remaining int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM auth_identities WHERE user_id = $1`, oldUser,
	).Scan(&remaining); err != nil {
		t.Fatalf("잔존 identity 조회가 실패했다: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("삭제된 사용자에게 identity가 %d건 남아 있다", remaining)
	}
}

// push token과 APNs 환경은 함께 있거나 함께 없다(00002). token만 있으면 worker가
// sandbox와 production 중 어디로 보낼지 알 수 없다.
func TestDevicePushTokenRequiresEnvironment(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()
	userID := insertUser(t, tx)
	deviceID := insertDevice(t, tx, userID)

	cases := []struct {
		name  string
		token []byte
		env   *string
		ok    bool
	}{
		{"둘 다 없음", nil, nil, true},
		{"둘 다 있음", []byte{1}, ptr("sandbox"), true},
		{"token만 있음", []byte{1}, nil, false},
		{"environment만 있음", nil, ptr("production"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sp.Rollback(ctx) }()
			_, err = sp.Exec(ctx,
				`UPDATE devices SET push_token_ciphertext = $2, push_environment = $3 WHERE id = $1`,
				deviceID, tc.token, tc.env)
			if tc.ok {
				if err != nil {
					t.Fatalf("허용돼야 하는 조합이 거부됐다: %v", err)
				}
				return
			}
			assertCheckViolation(t, err, "devices_push_token_environment_pairing_check")
		})
	}
}

func ptr(s string) *string { return &s }
