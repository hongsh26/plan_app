// Party 스키마(migration 00010)의 DB 불변식 통합 테스트.
//
// 근거는 docs/party_membership_design.md §5.1과 docs/party_visibility_design.md §6.
// 실행 조건은 schema_invariants_test.go의 주석과 같다.
//
// 지연 검증을 확인하는 방법:
//
// 설계 §5.1의 세 불변식은 트랜잭션 중간에 일시적으로 깨지고 커밋 시점에만 검증된다.
// 테스트는 서로의 데이터를 보지 않도록 rollback으로 끝나는데, rollback은 지연 제약을
// 실행하지 않으므로 그대로 두면 "거부해야 할 것을 거부했는지"를 전혀 확인하지 못한다.
// 그래서 검사 지점에서 SET CONSTRAINTS ALL IMMEDIATE로 지연된 검사를 그 자리에서
// 실행시킨다. 이것이 통과하면 커밋도 통과하고, 실패하면 커밋도 실패한다.
package test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// checkDeferred는 지연된 제약과 트리거를 지금 실행시킨다. 반환된 오류가 곧
// 커밋이 반환했을 오류다.
func checkDeferred(tx pgx.Tx) error {
	_, err := tx.Exec(context.Background(), `SET CONSTRAINTS ALL IMMEDIATE`)
	return err
}

// requireDeferredOK는 지연 검사가 통과하는지 확인한다. 정상 트랜잭션이 중간
// 상태 때문에 막히지 않는다는 것을 보는 쪽이다.
func requireDeferredOK(t *testing.T, tx pgx.Tx, what string) {
	t.Helper()
	if err := checkDeferred(tx); err != nil {
		t.Fatalf("%s이(가) 커밋 시점 검증에서 막혔다: %v", what, err)
	}
}

// assertCheckViolationAmong은 여러 불변식이 동시에 깨져서 어느 것이 먼저 보고될지
// 단정할 수 없을 때 쓴다. SQLSTATE는 그대로 23514를 요구하므로 "제약이 거부했다"는
// 확인은 유지되고, 이름만 후보 집합으로 느슨하게 본다.
func assertCheckViolationAmong(t *testing.T, err error, wantConstraints ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("제약 %v 중 어느 것도 위반을 거부하지 않았다", wantConstraints)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("PostgreSQL 오류가 아니다: %v", err)
	}
	if pgErr.Code != pgCheckViolation {
		t.Fatalf("SQLSTATE = %s, want %s (check_violation): %v",
			pgErr.Code, pgCheckViolation, err)
	}
	for _, want := range wantConstraints {
		if pgErr.ConstraintName == want {
			return
		}
	}
	t.Errorf("거부한 제약 = %q, want 다음 중 하나: %v", pgErr.ConstraintName, wantConstraints)
}

// insertParty는 설계 §5.5 T1과 같은 순서로 Party와 방장 멤버십을 만든다.
// 멤버십 UUID를 먼저 만들어 parties에 넣고 멤버십 row는 뒤에 insert한다.
func insertParty(t *testing.T, tx pgx.Tx) (partyID, membershipID, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	userID = insertUser(t, tx)
	partyID = uuid.New()
	membershipID = uuid.New()

	if _, err := tx.Exec(ctx,
		`INSERT INTO parties (id, name, owner_membership_id, created_by_user_id)
		 VALUES ($1, $2, $3, $4)`,
		partyID, "테스트 Party", membershipID, userID); err != nil {
		t.Fatalf("Party를 만들 수 없다: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO party_memberships (id, party_id, user_id, role)
		 VALUES ($1, $2, $3, 'owner')`,
		membershipID, partyID, userID); err != nil {
		t.Fatalf("방장 멤버십을 만들 수 없다: %v", err)
	}

	return partyID, membershipID, userID
}

// insertMember는 일반 멤버 한 명을 추가한다.
func insertMember(t *testing.T, tx pgx.Tx, partyID uuid.UUID) (membershipID, userID uuid.UUID) {
	t.Helper()
	userID = insertUser(t, tx)
	membershipID = uuid.New()
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO party_memberships (id, party_id, user_id, role)
		 VALUES ($1, $2, $3, 'member')`,
		membershipID, partyID, userID); err != nil {
		t.Fatalf("멤버 멤버십을 만들 수 없다: %v", err)
	}
	return membershipID, userID
}

// insertInvite는 활성 초대 하나를 만든다.
func insertInvite(t *testing.T, tx pgx.Tx, partyID, createdBy uuid.UUID, tokenHash []byte) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO party_invites
		     (id, party_id, token_hash, expires_at, max_uses, created_by_membership_id)
		 VALUES ($1, $2, $3, now() + interval '7 days', 5, $4)`,
		id, partyID, tokenHash, createdBy); err != nil {
		t.Fatalf("초대를 만들 수 없다: %v", err)
	}
	return id
}

// insertProjection은 busyOnly 투영 하나를 만든다.
func insertProjection(t *testing.T, tx pgx.Tx, partyID, ownerUserID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	start := time.Now().Add(time.Hour)
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO party_schedule_projections
		     (id, party_id, owner_user_id, source_event_key, source_generation,
		      setting_version, visibility_level, start_at, end_at, time_zone)
		 VALUES ($1, $2, $3, $4, 1, 1, 'busyOnly', $5, $6, 'Asia/Seoul')`,
		id, partyID, ownerUserID, []byte(id.String()), start, start.Add(time.Hour)); err != nil {
		t.Fatalf("투영을 만들 수 없다: %v", err)
	}
	return id
}

// §5.1 "검증 시점": T1은 멤버십 row가 생기기 전에 active Party를 insert한다.
// owner_membership_id FK가 DEFERRABLE INITIALLY DEFERRED가 아니거나 두 지연
// 트리거가 즉시 검증이면 이 정상 트랜잭션이 중간 상태에서 막힌다.
func TestPartyCreationCommitsThroughDeferredOwnerReference(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	userID := insertUser(t, tx)
	partyID := uuid.New()
	membershipID := uuid.New()

	// 존재하지 않는 멤버십을 가리키는 Party. 지연 FK 덕분에 여기서는 통과한다.
	if _, err := tx.Exec(ctx,
		`INSERT INTO parties (id, name, owner_membership_id, created_by_user_id)
		 VALUES ($1, '주말 모임', $2, $3)`,
		partyID, membershipID, userID); err != nil {
		t.Fatalf("멤버십보다 먼저 Party를 만들 수 없다. FK가 지연되지 않는다: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO party_memberships (id, party_id, user_id, role)
		 VALUES ($1, $2, $3, 'owner')`,
		membershipID, partyID, userID); err != nil {
		t.Fatalf("방장 멤버십을 만들 수 없다: %v", err)
	}

	requireDeferredOK(t, tx, "T1 Party 생성 트랜잭션")
}

// §5.1 "검증 시점": T7은 멤버십을 종료하기 전에 Party를 disbanded로 바꾼다.
// 해산 검사가 즉시 검증이면 이 정상 트랜잭션이 첫 UPDATE에서 막힌다.
func TestPartyDisbandCommitsThroughDeferredChildCleanup(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	partyID, ownerMembershipID, ownerUserID := insertParty(t, tx)
	insertMember(t, tx, partyID)
	insertInvite(t, tx, partyID, ownerMembershipID, []byte("token-hash-disband"))
	insertProjection(t, tx, partyID, ownerUserID)

	// 여기서 검사하지 않는다. SET CONSTRAINTS ALL IMMEDIATE는 남은 트랜잭션 전체의
	// 검사 시점을 바꾸므로 중간에 부르면 확인하려는 지연 자체가 사라진다.
	//
	// T7 step 1: 하위 row를 정리하기 전에 Party부터 해산한다.
	if _, err := tx.Exec(ctx,
		`UPDATE parties SET status = 'disbanded', disbanded_at = now() WHERE id = $1`,
		partyID); err != nil {
		t.Fatalf("Party를 해산할 수 없다: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE party_memberships
		 SET status = 'removed', ended_at = now(), end_reason = 'party_disbanded'
		 WHERE party_id = $1 AND status = 'active'`, partyID); err != nil {
		t.Fatalf("멤버십을 종료할 수 없다: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE party_invites SET status = 'revoked', revoked_at = now()
		 WHERE party_id = $1 AND status = 'active'`, partyID); err != nil {
		t.Fatalf("초대를 무효화할 수 없다: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM party_schedule_projections WHERE party_id = $1`, partyID); err != nil {
		t.Fatalf("투영을 삭제할 수 없다: %v", err)
	}

	requireDeferredOK(t, tx, "T7 해산 트랜잭션")
}

// §8 필수 불변식: Party의 활성 멤버는 (party_id, user_id)당 하나다.
func TestSecondActiveMembershipForSameUserIsRejected(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)

	partyID, _, ownerUserID := insertParty(t, tx)

	_, err := tx.Exec(context.Background(),
		`INSERT INTO party_memberships (id, party_id, user_id, role)
		 VALUES ($1, $2, $3, 'member')`,
		uuid.New(), partyID, ownerUserID)
	assertUniqueViolation(t, err, "party_memberships_active_member_key")
}

// §5.1: 한 Party에 활성 방장은 최대 1명이다. 이 index는 지연되지 않으므로
// 위반 문장에서 바로 거부한다.
func TestSecondActiveOwnerIsRejectedImmediately(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)

	partyID, _, _ := insertParty(t, tx)
	secondUser := insertUser(t, tx)

	_, err := tx.Exec(context.Background(),
		`INSERT INTO party_memberships (id, party_id, user_id, role)
		 VALUES ($1, $2, $3, 'owner')`,
		uuid.New(), partyID, secondUser)
	assertUniqueViolation(t, err, "party_memberships_active_owner_key")
}

// §5.1: 활성 방장 unique index는 지연되지 않으므로 T4(위임)는 강등을 먼저 실행한 뒤
// 승격해야 한다.
//
// 설계 §5.1은 한 문장 UPDATE ... CASE 방식이 row 처리 순서에 따라 결과가 갈려 믿을 수
// 없다고 쓴다. 강등 row가 먼저 걸리면 통과하고 승격 row가 먼저 걸리면 실패한다. 그 때문에
// 한 문장 방식은 어느 쪽으로도 단정할 수 없어 테스트하지 않는다. 대신 순서가 확정된 두
// 경로를 본다: 승격 먼저는 반드시 실패하고, 강등 먼저는 반드시 통과한다.
func TestOwnerTransferMustDemoteBeforePromote(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	t.Run("승격을 먼저 하면 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, _ := insertParty(t, tx)
		targetMembershipID, _ := insertMember(t, tx, partyID)

		_, err := tx.Exec(ctx,
			`UPDATE party_memberships SET role = 'owner' WHERE id = $1`,
			targetMembershipID)
		assertUniqueViolation(t, err, "party_memberships_active_owner_key")
	})

	t.Run("강등 후 승격은 통과한다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, ownerMembershipID, _ := insertParty(t, tx)
		targetMembershipID, _ := insertMember(t, tx, partyID)

		if _, err := tx.Exec(ctx,
			`UPDATE party_memberships SET role = 'member' WHERE id = $1`,
			ownerMembershipID); err != nil {
			t.Fatalf("기존 방장을 강등할 수 없다: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE party_memberships SET role = 'owner' WHERE id = $1`,
			targetMembershipID); err != nil {
			t.Fatalf("대상을 승격할 수 없다: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE parties SET owner_membership_id = $1, version = version + 1 WHERE id = $2`,
			targetMembershipID, partyID); err != nil {
			t.Fatalf("Party의 방장 참조를 옮길 수 없다: %v", err)
		}

		requireDeferredOK(t, tx, "T4 위임 트랜잭션")
	})
}

// §5.1: active Party의 owner_membership_id는 같은 Party의 active·owner 멤버십을
// 가리킨다. 세 조건을 각각 깨뜨려 본다.
func TestActivePartyOwnerReferenceIsValidatedAtCommit(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	t.Run("방장을 강등만 하고 참조를 그대로 두면 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, ownerMembershipID, _ := insertParty(t, tx)
		insertMember(t, tx, partyID)

		if _, err := tx.Exec(ctx,
			`UPDATE party_memberships SET role = 'member' WHERE id = $1`,
			ownerMembershipID); err != nil {
			t.Fatalf("강등할 수 없다: %v", err)
		}

		assertCheckViolation(t, checkDeferred(tx), "party_active_owner_membership_check")
	})

	t.Run("방장 멤버십이 비활성이면 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, ownerMembershipID, _ := insertParty(t, tx)
		insertMember(t, tx, partyID)

		if _, err := tx.Exec(ctx,
			`UPDATE party_memberships
			 SET status = 'left', ended_at = now(), end_reason = 'left_voluntarily'
			 WHERE id = $1`, ownerMembershipID); err != nil {
			t.Fatalf("방장 멤버십을 종료할 수 없다: %v", err)
		}

		assertCheckViolation(t, checkDeferred(tx), "party_active_owner_membership_check")
	})

	t.Run("다른 Party의 멤버십을 가리키면 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, _ := insertParty(t, tx)
		_, otherMembershipID, _ := insertParty(t, tx)

		if _, err := tx.Exec(ctx,
			`UPDATE parties SET owner_membership_id = $1 WHERE id = $2`,
			otherMembershipID, partyID); err != nil {
			t.Fatalf("방장 참조를 바꿀 수 없다: %v", err)
		}

		assertCheckViolation(t, checkDeferred(tx), "party_active_owner_membership_check")
	})
}

// §5.1: active Party는 활성 멤버가 1명 이상이다. 마지막 활성 멤버가 사라지는
// 트랜잭션은 같은 트랜잭션에서 Party를 disbanded로 만들어야 한다(T5의 단독 방장
// 탈퇴 -> 해산 자동 전환).
func TestActivePartyMustKeepAtLeastOneActiveMember(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	endLastMember := func(t *testing.T, tx pgx.Tx, membershipID uuid.UUID) {
		t.Helper()
		if _, err := tx.Exec(ctx,
			`UPDATE party_memberships
			 SET status = 'left', ended_at = now(), end_reason = 'left_voluntarily'
			 WHERE id = $1`, membershipID); err != nil {
			t.Fatalf("마지막 멤버를 종료할 수 없다: %v", err)
		}
	}

	// 두 지연 불변식이 함께 깨지므로 제약 이름을 하나로 단정하지 않는다.
	//
	// 활성 멤버가 0명이면 방장 멤버십도 활성일 수 없다. 그래서 "활성 멤버 1명 이상"은
	// active Party에 한해 "owner_membership_id 3조건"에 포함되고, 둘 중 어느 트리거가
	// 먼저 실패할지는 트리거 이름 순서라는 구현 세부에 달렸다. 거기에 테스트를 묶으면
	// 트리거 이름만 바꿔도 깨진다. 확인해야 할 것은 "이 트랜잭션이 커밋되지 않는다"다.
	t.Run("해산 없이 마지막 멤버가 떠나면 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		_, ownerMembershipID, _ := insertParty(t, tx)
		endLastMember(t, tx, ownerMembershipID)

		assertCheckViolationAmong(t, checkDeferred(tx),
			"party_active_party_has_member_check",
			"party_active_owner_membership_check")
	})

	// 위 테스트는 방장 트리거가 먼저 걸려도 통과하므로, 멤버 수 트리거가 죽어 있어도
	// 초록불이 된다. 그것까지 막으려면 방장 트리거를 치우고 혼자 세워 봐야 한다.
	//
	// 트랜잭션 안의 DROP TRIGGER는 rollback으로 되돌아가므로 스키마에 남지 않는다.
	// 테스트 역할이 plantogether_migration(테이블 소유자)이라 실행할 수 있다.
	t.Run("방장 트리거를 빼도 멤버 수 트리거가 혼자 거부한다", func(t *testing.T) {
		tx := begin(t, conn)
		_, ownerMembershipID, _ := insertParty(t, tx)

		for _, trigger := range []string{
			"parties_assert_active_owner_membership ON parties",
			"party_memberships_assert_active_owner_membership ON party_memberships",
		} {
			if _, err := tx.Exec(ctx, `DROP TRIGGER `+trigger); err != nil {
				t.Fatalf("방장 트리거를 치울 수 없다 (%s): %v", trigger, err)
			}
		}

		endLastMember(t, tx, ownerMembershipID)
		assertCheckViolation(t, checkDeferred(tx), "party_active_party_has_member_check")
	})

	t.Run("같은 트랜잭션에서 해산하면 통과한다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, ownerMembershipID, _ := insertParty(t, tx)
		endLastMember(t, tx, ownerMembershipID)

		if _, err := tx.Exec(ctx,
			`UPDATE parties SET status = 'disbanded', disbanded_at = now() WHERE id = $1`,
			partyID); err != nil {
			t.Fatalf("Party를 해산할 수 없다: %v", err)
		}

		requireDeferredOK(t, tx, "T5 단독 방장 탈퇴 후 해산 전환")
	})
}

// §5.1: disbanded Party에는 활성 멤버십, 활성 초대, 활성 projection이 없다.
func TestDisbandedPartyRejectsActiveChildren(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	disband := func(t *testing.T, tx pgx.Tx, partyID uuid.UUID) {
		t.Helper()
		if _, err := tx.Exec(ctx,
			`UPDATE parties SET status = 'disbanded', disbanded_at = now() WHERE id = $1`,
			partyID); err != nil {
			t.Fatalf("Party를 해산할 수 없다: %v", err)
		}
	}
	// 멤버십을 남겨 두면 그쪽에서 먼저 걸리므로, 초대·투영을 볼 때는 먼저 종료한다.
	endMemberships := func(t *testing.T, tx pgx.Tx, partyID uuid.UUID) {
		t.Helper()
		if _, err := tx.Exec(ctx,
			`UPDATE party_memberships
			 SET status = 'removed', ended_at = now(), end_reason = 'party_disbanded'
			 WHERE party_id = $1 AND status = 'active'`, partyID); err != nil {
			t.Fatalf("멤버십을 종료할 수 없다: %v", err)
		}
	}

	t.Run("활성 멤버십이 남으면 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, _ := insertParty(t, tx)
		disband(t, tx, partyID)

		assertCheckViolation(t, checkDeferred(tx), "party_disbanded_has_no_active_children_check")
	})

	t.Run("활성 초대가 남으면 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, ownerMembershipID, _ := insertParty(t, tx)
		insertInvite(t, tx, partyID, ownerMembershipID, []byte("token-hash-left-active"))
		disband(t, tx, partyID)
		endMemberships(t, tx, partyID)

		assertCheckViolation(t, checkDeferred(tx), "party_disbanded_has_no_active_children_check")
	})

	t.Run("투영이 남으면 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, ownerUserID := insertParty(t, tx)
		insertProjection(t, tx, partyID, ownerUserID)
		disband(t, tx, partyID)
		endMemberships(t, tx, partyID)

		assertCheckViolation(t, checkDeferred(tx), "party_disbanded_has_no_active_children_check")
	})

	t.Run("해산된 Party에 활성 초대를 새로 만들 수 없다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, ownerMembershipID, _ := insertParty(t, tx)
		disband(t, tx, partyID)
		endMemberships(t, tx, partyID)

		// 여기까지가 정상 해산이다. 검사 시점을 즉시로 바꾼 뒤 초대를 넣어, 이미
		// 해산된 Party에 활성 초대가 다시 생기는 것이 막히는지 본다.
		requireDeferredOK(t, tx, "정상 해산")

		_, err := tx.Exec(ctx,
			`INSERT INTO party_invites
			     (id, party_id, token_hash, expires_at, max_uses, created_by_membership_id)
			 VALUES ($1, $2, $3, now() + interval '7 days', 5, $4)`,
			uuid.New(), partyID, []byte("token-hash-after-disband"), ownerMembershipID)
		assertCheckViolation(t, err, "party_disbanded_has_no_active_children_check")
	})
}

// §5.1: used_count는 max_uses를 넘지 않는다.
func TestInviteUsedCountCannotExceedMaxUses(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)

	partyID, ownerMembershipID, _ := insertParty(t, tx)
	inviteID := insertInvite(t, tx, partyID, ownerMembershipID, []byte("token-hash-uses"))

	_, err := tx.Exec(context.Background(),
		`UPDATE party_invites SET used_count = max_uses + 1 WHERE id = $1`, inviteID)
	assertCheckViolation(t, err, "party_invites_used_count_within_max_check")
}

// §5.1: token_hash는 살아 있는 값만 유일하다. 정리 작업이 NULL로 지운 감사 row는
// 서로 충돌하지 않아야 한다. 전역 unique로 되돌리면 두 번째 NULL row에서 실패한다.
func TestInviteTokenHashIsUniqueOnlyWhenPresent(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	t.Run("같은 해시는 거부된다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, ownerMembershipID, _ := insertParty(t, tx)
		insertInvite(t, tx, partyID, ownerMembershipID, []byte("token-hash-dup"))

		_, err := tx.Exec(ctx,
			`INSERT INTO party_invites
			     (id, party_id, token_hash, expires_at, max_uses, created_by_membership_id)
			 VALUES ($1, $2, $3, now() + interval '7 days', 5, $4)`,
			uuid.New(), partyID, []byte("token-hash-dup"), ownerMembershipID)
		assertUniqueViolation(t, err, "party_invites_token_hash_key")
	})

	t.Run("지워진 해시는 여러 건이어도 통과한다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, ownerMembershipID, _ := insertParty(t, tx)

		first := insertInvite(t, tx, partyID, ownerMembershipID, []byte("token-hash-a"))
		second := insertInvite(t, tx, partyID, ownerMembershipID, []byte("token-hash-b"))

		if _, err := tx.Exec(ctx,
			`UPDATE party_invites SET status = 'expired', token_hash = NULL
			 WHERE id IN ($1, $2)`, first, second); err != nil {
			t.Fatalf("만료된 초대의 해시를 지울 수 없다. 부분 unique가 아니다: %v", err)
		}
	})
}

// §5.3: 종료 사유는 상태와 짝을 이룬다. 본인 의사로 끝난 것은 left,
// 타인·시스템이 끝낸 것은 removed다.
func TestMembershipEndReasonMustMatchStatus(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	t.Run("left에 강퇴 사유를 쓸 수 없다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, _ := insertParty(t, tx)
		memberID, _ := insertMember(t, tx, partyID)

		_, err := tx.Exec(ctx,
			`UPDATE party_memberships
			 SET status = 'left', ended_at = now(), end_reason = 'removed_by_owner'
			 WHERE id = $1`, memberID)
		assertCheckViolation(t, err, "party_memberships_end_reason_check")
	})

	t.Run("활성 멤버십에 종료 시각을 남길 수 없다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, _ := insertParty(t, tx)
		memberID, _ := insertMember(t, tx, partyID)

		_, err := tx.Exec(ctx,
			`UPDATE party_memberships SET ended_at = now() WHERE id = $1`, memberID)
		assertCheckViolation(t, err, "party_memberships_end_fields_match_status_check")
	})

	t.Run("종료된 멤버십은 사유가 있어야 한다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, _ := insertParty(t, tx)
		memberID, _ := insertMember(t, tx, partyID)

		_, err := tx.Exec(ctx,
			`UPDATE party_memberships SET status = 'left', ended_at = now() WHERE id = $1`,
			memberID)
		assertCheckViolation(t, err, "party_memberships_end_fields_match_status_check")
	})
}

// 공개 수준 §6.1: details가 아니면 장소를 공유하지 않는다. 하향 시 share_location을
// 함께 내리지 않으면 여기서 막힌다.
func TestVisibilityShareLocationRequiresDetails(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)

	partyID, _, ownerUserID := insertParty(t, tx)

	_, err := tx.Exec(context.Background(),
		`INSERT INTO party_visibility_settings
		     (id, party_id, user_id, visibility_level, share_location)
		 VALUES ($1, $2, $3, 'busyOnly', true)`,
		uuid.New(), partyID, ownerUserID)
	assertCheckViolation(t, err, "party_visibility_settings_share_location_check")
}

// 공개 수준 §6.1: 설정은 (party_id, user_id)당 하나다.
func TestVisibilitySettingIsUniquePerPartyAndUser(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	partyID, _, ownerUserID := insertParty(t, tx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO party_visibility_settings (id, party_id, user_id)
		 VALUES ($1, $2, $3)`, uuid.New(), partyID, ownerUserID); err != nil {
		t.Fatalf("공개 수준 설정을 만들 수 없다: %v", err)
	}

	_, err := tx.Exec(ctx,
		`INSERT INTO party_visibility_settings (id, party_id, user_id)
		 VALUES ($1, $2, $3)`, uuid.New(), partyID, ownerUserID)
	assertUniqueViolation(t, err, "party_visibility_settings_party_user_key")
}

// 공개 수준 §6.2: hidden은 투영 자체가 없으므로 저장할 수 없고, busyOnly row에는
// 제목·장소 ciphertext가 존재할 수 없다. 두 번째가 깨지면 busyOnly 화면에 남은
// 암호문이 그대로 노출 후보가 된다.
func TestProjectionRejectsHiddenAndBusyOnlyDetails(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	t.Run("hidden 투영은 저장할 수 없다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, ownerUserID := insertParty(t, tx)
		start := time.Now().Add(time.Hour)

		_, err := tx.Exec(ctx,
			`INSERT INTO party_schedule_projections
			     (id, party_id, owner_user_id, source_event_key, source_generation,
			      setting_version, visibility_level, start_at, end_at, time_zone)
			 VALUES ($1, $2, $3, $4, 1, 1, 'hidden', $5, $6, 'Asia/Seoul')`,
			uuid.New(), partyID, ownerUserID, []byte("key-hidden"), start, start.Add(time.Hour))
		assertCheckViolation(t, err, "party_schedule_projections_level_check")
	})

	t.Run("busyOnly 투영에 상세 암호문을 남길 수 없다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, ownerUserID := insertParty(t, tx)
		projectionID := insertProjection(t, tx, partyID, ownerUserID)

		_, err := tx.Exec(ctx,
			`UPDATE party_schedule_projections SET title_ciphertext = $1 WHERE id = $2`,
			[]byte("암호화된 제목"), projectionID)
		assertCheckViolation(t, err,
			"party_schedule_projections_busy_only_has_no_details_check")
	})

	t.Run("같은 source_event_key는 Party·소유자당 하나다", func(t *testing.T) {
		tx := begin(t, conn)
		partyID, _, ownerUserID := insertParty(t, tx)
		start := time.Now().Add(time.Hour)
		key := []byte("key-dup")

		for i := 0; i < 2; i++ {
			_, err := tx.Exec(ctx,
				`INSERT INTO party_schedule_projections
				     (id, party_id, owner_user_id, source_event_key, source_generation,
				      setting_version, visibility_level, start_at, end_at, time_zone)
				 VALUES ($1, $2, $3, $4, 1, 1, 'busyOnly', $5, $6, 'Asia/Seoul')`,
				uuid.New(), partyID, ownerUserID, key, start, start.Add(time.Hour))
			if i == 0 && err != nil {
				t.Fatalf("첫 투영을 만들 수 없다: %v", err)
			}
			if i == 1 {
				assertUniqueViolation(t, err, "party_schedule_projections_source_key")
			}
		}
	})
}

// §4.3 정원과 §5.1 member_limit 범위.
func TestPartyMemberLimitRange(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)

	userID := insertUser(t, tx)
	_, err := tx.Exec(context.Background(),
		`INSERT INTO parties (id, name, owner_membership_id, created_by_user_id, member_limit)
		 VALUES ($1, '정원 초과', $2, $3, 11)`,
		uuid.New(), uuid.New(), userID)
	assertCheckViolation(t, err, "parties_member_limit_check")
}

// §5.2: active Party에 해산 시각이 남으면 안 된다.
func TestActivePartyCannotCarryDisbandedAt(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)

	partyID, _, _ := insertParty(t, tx)

	_, err := tx.Exec(context.Background(),
		`UPDATE parties SET disbanded_at = now() WHERE id = $1`, partyID)
	assertCheckViolation(t, err, "parties_disbanded_at_requires_disbanded_check")
}

// 멤버십의 party_id를 바꾸면 떠나온 Party도 커밋 시점에 검증된다. 새 Party만 보면
// 원래 Party가 방장과 멤버 없이 active로 남는다.
func TestMovingMembershipRevalidatesPreviousParty(t *testing.T) {
	conn := connect(t)
	tx := begin(t, conn)
	ctx := context.Background()

	partyA, ownerA, _ := insertParty(t, tx)
	partyB, _, _ := insertParty(t, tx)

	if _, err := tx.Exec(ctx,
		`UPDATE party_memberships SET party_id = $1, role = 'member' WHERE id = $2`,
		partyB, ownerA); err != nil {
		t.Fatalf("멤버십을 옮길 수 없다: %v", err)
	}

	err := checkDeferred(tx)
	assertCheckViolationAmong(t, err,
		"party_active_owner_membership_check", "party_active_party_has_member_check")
	_ = partyA
}
