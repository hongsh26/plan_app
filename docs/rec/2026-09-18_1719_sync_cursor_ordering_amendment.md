# sync 변경 피드 순서 키 개정과 설계 9 미반영 개정 적용

- 일시: 2026-09-18 17:19
- 종류: 설계 방향 확정 (설계 2 개정)
- 대상 문서: `docs/account_backend_design.md`, `docs/party_membership_design.md`

## 계기

P0(서버 골격)의 `sync_changes` 마이그레이션을 쓰기 직전 §7.1을 검토하다가 변경 유실 경로를 발견했다.

## 발견 1: `BIGSERIAL` 순서 키는 변경을 영구 유실시킨다

기존 §7.1은 `sync_changes.seq`를 "전체 시스템에서 단조 증가하는 `BIGINT`", 클라이언트 cursor를 "마지막으로 반영한 `seq` 하나"로 정의하고 "중간 번호가 비어도 정상"이라고 했다.

`BIGSERIAL`의 `nextval`은 트랜잭션 밖에서 즉시 소비되므로 번호 순서와 커밋 순서가 일치하지 않는다.

1. 트랜잭션 A가 `seq 5`를, B가 `seq 6`을 받는다.
2. B가 먼저 커밋된다.
3. 클라이언트가 sync해 `seq 6`까지 읽고 cursor를 6으로 올린다.
4. A가 커밋한다. `seq 5`는 `seq > 6` 조회에 걸리지 않아 어떤 cursor로도 다시 조회되지 않는다.

"중간 번호가 비어도 정상"이라는 문장이 이 경우를 정상 동작으로 가려버리고 있었다.

## 검토한 대안과 탈락 이유

### 절충안: `BIGSERIAL` 유지 + `txid` 열로 horizon 필터 — 탈락

`txid xid8` 열을 덧붙이고 `WHERE txid < pg_snapshot_xmin(pg_current_snapshot())`으로 거르는 방식. 성립하지 않는다. 트랜잭션은 `sync_changes`에 쓰기 한참 전의 다른 쓰기에서 `txid`를 먼저 얻으므로 `txid` 순서와 `seq` 순서가 뒤집힐 수 있다.

- C(txid 99)가 A(txid 100)보다 **늦게** `sync_changes`에 삽입 → A는 `seq 5`, C는 `seq 6`.
- A가 진행 중이면 horizon은 100. C는 `txid 99 < 100`으로 필터를 통과해 `seq 6`이 전달된다.
- cursor가 6으로 올라간 뒤 A가 커밋 → `seq 5` 유실. 원래 문제가 그대로 남는다.

순서 키 자체가 `txid`여야 이 역전이 사라진다.

### 대안: commit 직전 advisory lock으로 순번 부여 — 탈락

정확하지만 "모든 도메인 트랜잭션이 모든 row lock 이후 마지막에 같은 전역 lock을 잡는다"는 규약을 설계 4~9 전체와 이후 모든 구현 단계에 영원히 퍼뜨린다. 설계 9의 검토 2회차에서 대부분의 지적이 1차 수정 자체가 만든 모순이었던 전례를 감안하면, 여덟 개 구현 단계가 각자 기억해야 하는 불변식이 더 취약하다.

추가로 P3의 인수 조건인 "동시 초대 수락 경쟁 테스트"를 전역 직렬화가 자동 통과시켜 §5.5 잠금 순서의 실제 결함을 가린다.

## 확정: `(txid, ordinal)` 순서 키

- `txid xid8 NOT NULL DEFAULT pg_current_xact_id()`, `ordinal INT`(트랜잭션 내 0부터), PK `(txid, ordinal)`. 전역 `BIGSERIAL`은 두지 않는다.
- 읽기: `horizon = pg_snapshot_xmin(pg_current_snapshot())`을 구하고 `WHERE recipient_user_id = $1 AND (txid, ordinal) > cursor AND txid < horizon ORDER BY txid, ordinal`.
- `horizon` 미만 트랜잭션은 전부 종료되었으므로 진행 중 트랜잭션의 변경이 cursor 뒤에 나타날 수 없다. 순서 키가 `txid`이므로 역전도 없다.
- **클라이언트 cursor는 opaque 문자열로 바꿨다.** 내부 표현이 다시는 계약이 되지 않게 한다. 하위 설계는 모두 cursor를 불투명하게만 쓰고 있어 파급이 없다.
- `ordinal`은 mutation transaction helper가 단독 발급한다. 개별 call site가 세지 않는다.
- 보존 30일 판정은 `txid`가 아니라 `created_at`으로 한다. 만료 코드는 기존대로 `sync_cursor_expired`를 유지한다(`party_membership_design.md`:573의 "이 코드만 bootstrap을 트리거한다" 규약 보존).

### 새로 생긴 운영 실패 모드

오래 도는 **쓰기** 트랜잭션 하나가 horizon을 붙잡아 전체 사용자의 피드를 정지시킬 수 있다. `BIGSERIAL`에는 없던 모드다. §12에 `sync horizon lag` 지표를 추가했다(p95 5초 이하, 30초 초과 경보). 읽기 전용 트랜잭션은 `txid`를 받지 않아 horizon을 붙잡지 않는다.

## 발견 2: 설계 9의 개정 2건이 본문에 반영되지 않았다

§17이 설계 9의 요구 개정 3건을 나열하고 있었으나 실제 본문 반영은 1건뿐이었다.

| 개정 | 이전 상태 |
|---|---|
| §8 `devices` 알림 열 3개 | 반영됨 |
| §7.2 bootstrap·`/v1/me`의 `notification_ref_key` | **§17 목록에만 있고 §7.2 본문은 닫힌 4필드 그대로** |
| §7.3 SwiftData App Group 공유 컨테이너 + 공유 keychain | **§17 목록에만 있음** |

§7.2를 구현 근거로 읽는 사람은 `notification_ref_key`를 빠뜨리게 된다. 둘 다 이번에 본문에 반영하고 §17 항목에 "반영됨"을 표시했다.

## 변경한 문서

- `docs/account_backend_design.md`: §1(개정 이력), §7.1(순서 키·opaque cursor·근거), §7.2(opaque watermark·`notification_ref_key`), §7.3(App Group), §8(테이블·불변식 index), §12(horizon lag 지표), §14(인수 조건), §15(순서 역전 회귀 테스트), §17(반영 표시)
- `docs/party_membership_design.md`: §9.2 신규 멤버 부트스트랩 문장을 `txid` 근거로 정정

## 다음

P0 서버 골격. `feature/server-skeleton` 브랜치에서 진행하고 main에 병합한 뒤 기능별 브랜치를 딴다. P0는 설계 4~9 전체의 공통 선행 조건이라 `feature/party-membership`에 두지 않는다.

환경은 Go 로컬 설치(완료) + PostgreSQL Docker Compose로 확정했다.
