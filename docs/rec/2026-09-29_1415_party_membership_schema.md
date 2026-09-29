# Party 스키마 (P1) 구현

- 작업 시각: 2026-09-29 14:15 KST
- 브랜치: `feature/party-membership`
- 유형: 기능 구현 완료
- 계획: `.omc/plans/party-membership-implementation.md` P1
- 설계: `docs/party_membership_design.md` §5.1, `docs/party_visibility_design.md` §6

## 한 일

migration `00010_party_membership.sql`과 통합 테스트 `server/test/party_schema_test.go`를 추가했다.
테이블 5개, 부분 unique index 3종, 지연 검증 CONSTRAINT TRIGGER 3종을 만들었다.

- `parties`, `party_memberships`, `party_invites`
- `party_visibility_settings`, `party_schedule_projections`

## 확정한 것과 그 근거

### 계획의 "추가"는 실제로는 전체 CREATE였다

계획 P1과 설계 §5.1은 세 테이블이 `account_backend_design.md` §8에 "이미 고정"됐다고 보고
`member_limit` 추가, `token_hash` NULL 허용으로 변경처럼 ALTER 형태로 서술한다. 그러나
00001~00009 어디에도 Party 관련 테이블이 없다. §8은 테이블과 핵심 필드를 나열한 표일 뿐이고
P0 골격은 인증·동기화 경로에 필요한 것만 만들었다. 그래서 00010은 §8의 기존 열과 §5.1의
추가 열을 합친 전체 CREATE다. 계획 문구를 그대로 따랐다면 컴파일되지 않는 DDL이 나왔다.

### version 열을 둔 테이블과 두지 않은 테이블

00001이 정한 규약(클라이언트가 expected_version으로 다루거나 sync가 투영하는 row에만 둔다)을 적용했다.

| 테이블 | version | 근거 |
|---|---|---|
| `parties` | 있음 | §5.5 "모든 Party mutation에서 expected_version은 `parties.version`을 의미한다" |
| `party_memberships` | 있음 | §5.1 "멤버십 row의 version은 sync 투영용" |
| `party_visibility_settings` | 있음 | 공개 수준 §6.1 "mutation마다 증가" |
| `party_schedule_projections` | 있음 | 공개 수준 §6.2 "sync 충돌 해결" |
| `party_invites` | **없음** | 클라이언트가 `expected_version`을 보내는 경로가 없다. `updated_at`만 둔다 |

### `party_visibility_settings`에 `id`를 추가했다

공개 수준 §6.1은 `(party_id, user_id)` 복합 unique만 요구한다. 그런데 §9.2의 sync entity type
`party_visibility_setting`이 `entity_id` 한 열을 요구하고 복합 키를 거기 담을 수 없다.
그래서 `id uuid PRIMARY KEY`를 두고 `(party_id, user_id)`는 unique index로 유지했다.

### 순환 FK 처리

`parties → party_memberships → parties`, `party_memberships → party_invites → party_memberships`가
서로를 참조한다. 테이블을 먼저 만들고 FK는 `ALTER TABLE`로 붙였다.
`parties.owner_membership_id` FK만 `DEFERRABLE INITIALLY DEFERRED`다. 열 자체는 `NOT NULL`을
유지했다. PostgreSQL은 `NOT NULL`을 지연하지 않으므로 T1은 멤버십 UUID를 먼저 만들어야 한다.

### service가 담당해야 하는 검증 하나

공개 수준 §6.2의 "details인데 `share_location=false`면 location ciphertext 금지"는 DB CHECK로
막을 수 없다. `share_location`이 `party_visibility_settings`의 열이라 projection row CHECK의
가시 범위 밖이다. 설계가 "DB 또는 service validation"으로 허용한 대로 service 몫으로 남겼다.
`busyOnly` row의 상세 ciphertext 금지는 DB CHECK로 막았다.

## 설계와 어긋난 곳 (후속 판단 필요)

### §5.1의 "하나의 UPDATE ... CASE로 처리할 수 없다"는 과장이다

설계 §5.1은 활성 방장 unique index가 즉시 검증이므로 T4가 강등과 승격을 한 문장으로
처리할 수 **없다**고 썼다. 실제로는 한 문장 `UPDATE ... CASE`가 **통과했다**. 한 문장 안의
row 처리 순서가 보장되지 않아서, 강등 row가 먼저 걸리면 통과하고 승격 row가 먼저 걸리면
실패한다. 즉 금지되는 것이 아니라 순서에 따라 결과가 갈린다.

설계의 처방(강등 먼저, 승격 나중)은 그대로 옳다. 바뀌어야 할 것은 근거 문장이다.
"처리할 수 없다"가 아니라 "순서가 보장되지 않아 믿을 수 없으므로 나눠야 한다"다.
테스트는 순서가 확정된 두 경로만 본다. 승격 먼저는 반드시 실패하고, 강등 먼저는 반드시 통과한다.
한 문장 방식은 어느 쪽으로도 단정할 수 없어 테스트하지 않는다.

### "active Party는 활성 멤버 1명 이상"은 방장 불변식에 포함된다

활성 멤버가 0명이면 방장 멤버십도 활성일 수 없으므로, active Party에 한해 이 불변식은
"`owner_membership_id` 3조건"이 이미 함의한다. 위반 트랜잭션에서 어느 트리거가 먼저 실패할지는
트리거 이름 순서라는 구현 세부에 달렸다. 그래서 그 테스트는 제약 이름을 하나로 단정하지 않는다.

트리거를 지우지는 않았다. 방장 불변식이 나중에 완화되면 혼자 남아야 하고, 설계가 별도 불변식으로
세어 두었기 때문이다. 대신 이 트리거가 죽은 코드가 아님을 확인하려고, 트랜잭션 안에서 방장 트리거
2개를 `DROP TRIGGER`한 뒤(rollback으로 되돌아간다) 멤버 수 트리거가 혼자 거부하는지 보는 테스트를
따로 두었다.

## 검증

로컬 Docker PostgreSQL 18.2에서 확인했다.

- `-migrate up` → `-migrate down` → `-migrate up` 왕복. down 뒤 `party%` 테이블과 `party_%` 함수가 0건이다
- P1 완료 기준 양쪽을 모두 확인했다
  - 거부: 두 번째 활성 멤버십과 두 번째 활성 방장이 각각 이름 붙은 unique index로 거부된다
  - 통과: T1(멤버십보다 먼저 Party insert)과 T7(멤버십 종료보다 먼저 해산)이 중간 상태에서 막히지 않는다
- 음성 대조로 테스트가 공회전이 아님을 확인했다. `SET CONSTRAINTS ALL IMMEDIATE`를 먼저 건 트랜잭션에서
  T1의 첫 INSERT가 `parties_owner_membership_id_fkey`로 거부된다. 지연이 없으면 통과하지 않는다
- `pg_trigger`에서 트리거 8개가 모두 `tgdeferrable=true, tginitdeferred=true`다
- api 역할로 실제 T1 트랜잭션을 `COMMIT`까지 실행했다. `SET CONSTRAINTS`가 아닌 진짜 커밋에서도
  트리거가 통과한다. 런타임 역할 4종 권한도 default privileges로 새 테이블에 붙었다
  (api·worker는 SELECT/INSERT/UPDATE/DELETE, readonly는 SELECT)
- `REQUIRE_DB_TESTS=1 go test -count=1 ./...` 전체 통과. 새 테스트는 30개 케이스다

### 테스트가 쓰는 기법

테스트는 서로의 데이터를 보지 않도록 rollback으로 끝나는데 rollback은 지연 검사를 실행하지 않는다.
그대로 두면 "거부해야 할 것을 거부했는지"를 전혀 확인하지 못한다. 그래서 검사 지점에서
`SET CONSTRAINTS ALL IMMEDIATE`로 지연된 검사를 그 자리에서 실행시킨다.

**이 문장은 남은 트랜잭션 전체의 검사 시점을 바꾼다.** 정상 경로 테스트 중간에 부르면 확인하려는
지연 자체가 사라져 테스트가 조용히 무의미해진다. T7 테스트에서 실제로 이 실수를 했고 고쳤다.
새 테스트를 추가할 때 이 함수는 트랜잭션당 한 번, 마지막에만 부른다.

### 알아 둘 것

`CREATE CONSTRAINT TRIGGER`의 이름은 63자를 넘으면 PostgreSQL이 **조용히 자른다**. 처음 쓴
`party_schedule_projections_assert_disbanded_has_no_active_children`이 잘려 있었다. 해산 검사
트리거 4개를 `*_assert_disbanded_is_empty`로 줄였다. 가장 긴 이름이 52자다.

`TG_OP`, `NEW`, `OLD`, `TG_ARGV`는 트리거 함수 자신의 지역 변수라 다른 함수에서 보이지 않는다.
공용 헬퍼로 뽑으려다 실패했고 세 함수가 각자 추출한다.

## 이 커밋이 살려 둔 결함

**계정 삭제가 Party를 정리하지 않는다.** 삭제 worker(`internal/account/deletion.go`)는
sessions·devices·auth_identities·audit_events·users만 건드린다. Party 테이블이 없던 때
쓰인 코드이고 00010이 그 전제를 깼다. 이제 활성 멤버십을 가진 사용자가 `status='deleted'`까지
가면서 Party의 활성 멤버로, 경우에 따라 활성 방장으로 남는다.

DB는 막지 못한다. 지연 트리거 3종은 Party 안의 정합성만 보고 `users.status`를 보지 않으며,
삭제 파이프라인이 `users` row를 물리 삭제하지 않아 FK RESTRICT도 걸리지 않는다. 계정 삭제
테스트가 통과하는 것은 그 테스트에 Party row가 없기 때문이지 경로가 안전해서가 아니다.

설계 §5.8과 계획 P8(강제 위임·해산)이 주인이다. P8을 P5 뒤로 미루더라도 그때까지 결함은
살아 있다. 이 커밋에서 고치지 않은 이유는 P1 범위가 스키마이고, 승계자 결정 규칙이 P8의
계약이기 때문이다.

## 설계 문서 수정

§5.1의 T4 근거 문장을 고쳤다. "하나의 `UPDATE ... CASE` 문으로 처리할 수 없고"에서
"금지되기 때문이 아니라 결과를 믿을 수 없기 때문"으로 바꿨다. 처방(강등 먼저, 승격 나중)은
그대로다.

## 다음 단계

P2(Party 생성·조회·수정 endpoint)다. 착수 전에 정할 것 하나가 있다.

**잠금 규약을 Party 경로까지 확장해야 한다.** 설계 §5.5는 `parties` → `party_memberships` →
`party_invites` 순서와 대상 Party row `FOR UPDATE`를 요구한다. 그런데 P1 인증이 확정한 규약은
`users`·`devices` row에 `FOR UPDATE`를 쓰지 않는 것이다(`docs/rec/2026-09-21_1651_mutation_helper.md`).
`party_memberships.user_id`가 `users`를 참조하므로 멤버십 INSERT는 `users` row에 KEY SHARE를 건다.
mutation helper의 `idempotency_keys` INSERT도 같은 row에 KEY SHARE를 건다. 두 경로의 상호작용을
P2 착수 시점에 확인하고, `mutation.Retries()`가 0이 아니면 규약이 깨진 것으로 본다.
