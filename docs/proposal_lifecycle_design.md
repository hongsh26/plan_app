# 제안·응답·확정 상태 전이 상세 설계

## 1. 설계 상태

- 상태: 확정
- 대상: Internal Alpha와 Launch MVP
- 순서: `docs/feature_design_backlog.md`의 상세 설계 진행 순서 7번
- 선행 설계: 계정·서버, 캘린더 privacy/sync, Party membership, Party visibility, availability search
- 후속 설계: 8(외부 캘린더 쓰기·변경·취소), 9(알림)
- 현재 구현 연결점: `src/PlanTogether/Models.swift`, `AppStore.swift`, `RootView.swift`

이 문서는 제안 생성부터 응답, 앱 내부 확정까지의 서버 기준 상태 전이를 확정한다. 외부 Apple Calendar 반영과 확정 일정의 변경·취소는 상세 설계 8, 실제 알림 문구·urgency·quiet hours는 상세 설계 9가 소유한다.

---

## 2. 목표와 사용자 가치

1. 활성 Party 멤버 전원이 같은 제안 내용과 응답 상태를 본다.
2. 전원 수락, 최신 캘린더 확인과 충돌 재검사를 모두 통과한 경우에만 앱 내부 일정을 확정한다.
3. 탈퇴·강퇴·동시 응답·오프라인 재전송에도 중복 확정이나 조용한 참여자 축소가 발생하지 않는다.
4. hidden 일정도 충돌 계산에는 반영하되 누구의 어떤 일정이 원인인지는 노출하지 않는다.
5. 기존 수락의 의미가 바뀌지 않도록 제안 핵심 내용은 생성 후 불변으로 유지한다.

---

## 3. 포함 범위와 제외 범위

### 포함

- 공통 가능 시간 결과 또는 임의 시간에서 제안 생성
- 제안 생성 시 활성 Party 멤버 전원의 고정 참여자 snapshot
- `pending`, `accepted`, `declined`, `tentative` 응답과 기한 전 응답 변경
- 전원 수락 후 15분 freshness 확인과 private Busy·앱 내부 확정 일정 충돌 재검사
- 탈퇴·강퇴·계정 삭제, Party 해산, 기한 만료, 취소와 재제안 상태 전이
- 버전, 멱등성, 동시성, sync와 domain outbox 계약
- iOS의 정상·대기·오프라인·충돌·종단 상태

### 제외

- 일부 멤버만 선택하는 제안, 최소 인원/정족수 확정, 익명 응답
- 제안 본문의 in-place 수정과 기존 수락 승계
- 확정 후 시간·제목·장소 변경과 취소
- EventKit create/update/delete 명령과 사용자별 쓰기 상태
- APNs 문구, 전송 시각, quiet hours와 묶음 처리
- 반복 약속, 투표형 복수 후보, 참석자 추가, 메모와 첨부

앞의 세 항목은 Launch MVP에서 제외한다. 외부 캘린더 처리는 상세 설계 8, 전송 정책은 상세 설계 9로 넘긴다.

---

## 4. 확정된 제품 정책과 입력 규칙

### 4.1 참여자와 확정 조건

- 제안 참여자는 생성 트랜잭션에서 조회한 **활성 Party 멤버 전원**이다. 클라이언트가 참여자를 추가·제외할 수 없다.
- 생성자는 참여자 snapshot에 포함되며 초기 응답은 `accepted`, 나머지는 `pending`이다.
- 생성 뒤 가입한 멤버는 해당 제안을 읽을 수 있지만 응답하거나 확정 조건에 포함되지 않는다. UI에는 `가입 전에 생성된 제안`으로 표시한다.
- 미확정 제안의 고정 참여자 한 명이라도 탈퇴·강퇴·계정 삭제로 비활성화되면 제안은 `reproposal_required`로 종료한다.
- 떠난 참여자를 분모에서 제외해 남은 사람만으로 확정하거나 최소 인원을 적용하지 않는다. 참여자 구성이 달라졌음을 명시하고 현재 활성 멤버 snapshot으로 새 제안을 만든다.
- 확정 조건은 고정 참여자 전원의 활성 멤버십 유지, 전원 `accepted`, 응답 기한 미도래, Party 활성, 15분 freshness와 충돌 재검사 통과다.

### 4.2 제안 본문

| 필드 | 규칙 |
|---|---|
| `title` | trim 후 1~80자, 제어 문자 금지 |
| `location` | 선택, trim 후 최대 120자, 빈 문자열은 `null` |
| `start_at`, `end_at` | RFC 3339 UTC, 반개구간 `[start,end)`, 시작 < 종료 |
| `time_zone` | 유효한 IANA time zone |
| 시간 grid | 표시 time zone의 00/30분 경계에서 시작, 30분 단위 길이 |
| 길이 | 30분~8시간 |
| `response_due_at` | 현재 서버 시각보다 뒤이고 `start_at` 이하. 생략하면 `start_at` |
| `source` | `availability` 또는 `custom` |
| `calculation_version` | `availability`일 때 필수, `custom`일 때 금지 |
| `supersedes_proposal_id` | 재제안 endpoint에서만 서버가 설정 |

- `availability` 제안은 `calculationVersion`, `validUntil`, 검색 파라미터 hash와 현재 Party/membership/activity/calendar tuple이 모두 일치해야 한다.
- `custom` 제안은 모든 고정 참여자의 calendar가 6시간 freshness를 충족해야 하며 private Busy와 앱 내부 확정 reservation 충돌을 즉시 검사한다.
- 생성 시 충돌하거나 준비가 덜 된 경우 proposal row를 만들지 않는다. 각각 aggregate `proposal_time_conflict`, `calendar_sync_pending`을 반환한다.
- 제목·장소·시간·기한·참여자 snapshot은 생성 후 수정하지 않는다. 바꾸려면 재제안을 만든다.

### 4.3 응답 정책

- 응답은 `pending`, `accepted`, `declined`, `tentative` 네 값이다.
- 고정 참여자이면서 현재 활성 멤버인 사용자만 본인 응답을 변경할 수 있다.
- 응답은 proposal이 `open` 또는 `awaiting_calendar_refresh`이고 기한이 지나지 않았을 때만 변경할 수 있다.
- `declined`와 `tentative`는 즉시 proposal을 종료하지 않는다. 기한 전 `accepted`로 바꿀 수 있다.
- `awaiting_calendar_refresh`에서 누군가 `accepted`가 아닌 값으로 바꾸면 대기 중 confirm attempt를 무효화하고 proposal을 `open`으로 되돌린다.
- 서버 version과 수신 시각이 기준이며 기기 시각이나 last-write-wins를 사용하지 않는다.

### 4.4 취소와 재제안

- proposal 생성자 또는 현재 Party owner만 미확정 proposal을 `cancelled`로 종료할 수 있다.
- 현재 활성 멤버 누구나 별도 proposal을 새로 만들 수 있다.
- 기존 proposal을 명시적으로 대체하는 재제안은 기존 생성자 또는 현재 owner만 수행한다.
- 재제안은 기존 row를 수정하지 않고 새 proposal과 새 참여자 snapshot을 만든 뒤 기존 proposal을 `superseded`로 종료한다.
- `confirmed` proposal은 재제안·취소 endpoint의 대상이 아니다. 확정 일정 변경·취소는 상세 설계 8을 따른다.

---

## 5. 데이터 모델과 상태 전이

### 5.1 테이블

| 테이블 | 핵심 필드·제약 |
|---|---|
| `proposals` | `id`, `party_id`, `created_by_membership_id`, 본문 필드, `source`, `status`, `response_due_at`, `confirm_attempt`, `refresh_deadline_at`, `supersedes_proposal_id`, `superseded_by_proposal_id`, `supersedes_confirmed_event_id`(설계 8 재일정 전용, 생성 후 불변), `version`, timestamps |
| `proposal_participants` | `proposal_id`, `user_id`, `membership_id`, `response`, `responded_at`, `version`; unique `(proposal_id,user_id)`와 `(proposal_id,membership_id)` |
| `confirmed_events` | `id`, `proposal_id`, `party_id`, 불변 본문 snapshot, `status='active'`, `revision=1`, `version`; proposal당 unique |
| `confirmed_event_participants` | `confirmed_event_id`, `user_id`, `membership_id`, `busy_range`; unique `(confirmed_event_id,user_id)` |

- 협업 기록 FK는 cascade 삭제하지 않고 계정 삭제 시 표시 정보를 익명화한다.
- `confirmed_event_participants.busy_range`는 `[start_at,end_at)`이며 확정 전 충돌 검사와 외부 쓰기 전 server-side reservation에 사용한다.
- 사용자별 활성 reservation이 겹치지 않도록 `btree_gist` 기반 exclusion constraint 또는 동일 의미의 직렬화된 transaction guard를 둔다. 구현은 PostgreSQL 통합 테스트로 동시 확정을 증명해야 한다.

### 5.2 Proposal 상태

```text
open
 ├─ 전원 accepted + freshness 부족 ─────────▶ awaiting_calendar_refresh
 ├─ 전원 accepted + fresh + 충돌 없음 ─────▶ confirmed
 ├─ 전원 accepted + fresh + 충돌 있음 ─────▶ conflict_detected
 ├─ 고정 참여자 membership 종료 ───────────▶ reproposal_required
 ├─ Party 해산 ─────────────────────────────▶ party_disbanded
 ├─ response_due_at 도달 ──────────────────▶ expired
 ├─ 생성자/owner 취소 ─────────────────────▶ cancelled
 └─ 명시적 재제안 ─────────────────────────▶ superseded

awaiting_calendar_refresh
 ├─ fresh snapshot + 충돌 없음 ─────────────▶ confirmed
 ├─ fresh snapshot + 충돌 있음 ─────────────▶ conflict_detected
 ├─ accepted 철회 ──────────────────────────▶ open
 ├─ 참여자 종료 ────────────────────────────▶ reproposal_required
 ├─ Party 해산 ─────────────────────────────▶ party_disbanded
 ├─ 기한 만료 ──────────────────────────────▶ expired
 ├─ 취소 ───────────────────────────────────▶ cancelled
 └─ 재제안 ─────────────────────────────────▶ superseded
```

`confirmed`, `conflict_detected`, `reproposal_required`, `party_disbanded`, `expired`, `cancelled`, `superseded`는 proposal의 종단 상태다. `awaiting_calendar_refresh`의 2분 대기 시간이 끝나도 별도 종단 상태로 바꾸지 않고 `refresh_deadline_at`을 지난 차단 상태로 유지한다. 사용자가 캘린더 새로고침 후 확인 재시도를 요청할 수 있다.

### 5.3 불변식

- proposal의 본문과 참여자 snapshot은 생성 후 바뀌지 않는다. `supersedes_confirmed_event_id`도 생성 후 바뀌지 않으며 재일정 endpoint에서만 서버가 설정한다.
- 미확정 proposal은 `open` 또는 `awaiting_calendar_refresh`뿐이다.
- proposal당 `confirmed_events`는 최대 하나이며 confirmed proposal에는 정확히 하나가 있다.
- `confirmed_events`와 `confirmed_event_participants` 생성, proposal `confirmed` 전이, sync와 domain outbox는 한 transaction에서 커밋한다.
- `confirmed` 전이 시 모든 고정 참여자 membership이 여전히 active이고 모든 응답이 accepted다.
- `reproposal_required`와 `party_disbanded` proposal은 어떤 endpoint나 worker로도 다시 열린 상태가 되지 않는다.
- 신규 멤버 가입은 기존 참여자 snapshot과 확정 조건을 바꾸지 않는다.
- 떠난 사용자에게 proposal tombstone을 보내는 것과 서버 proposal 상태 전이는 별도 효과이며 둘 다 같은 membership transaction에서 끝난다.

---

## 6. 트랜잭션, 경쟁과 멱등성

### 6.1 공통 잠금 순서

Stage 7 mutation은 `parties` → 관련 `party_memberships` → `proposals` → `proposal_participants` → `confirmed_events` 순으로 잠근다. 여러 row는 UUID 오름차순으로 잠근다. 대체 확정은 원 confirmed event와 새 confirmed event 두 row를 같은 transaction에서 다루므로 이 오름차순 규칙이 두 row에 그대로 적용된다. 서로 다른 Party에서 같은 사용자의 동시 확정을 직렬화하기 위해 고정 참여자 user ID를 정렬해 transaction-scoped advisory lock을 얻은 뒤 reservation을 검사·생성한다.

### 6.2 T1. 제안 생성

1. Party row를 `FOR UPDATE`로 잠그고 `active`인지 확인한다.
2. 요청자가 활성 멤버인지 확인하고 활성 membership 전원을 고정 snapshot으로 읽는다.
3. 입력과 `source`별 freshness/version/충돌 조건을 검증한다.
4. proposal을 `open`, 생성자 참여자를 `accepted`, 나머지를 `pending`으로 insert한다.
5. 활성 Party 멤버 대상 `proposal`·`proposal_response` sync upsert와 `proposal_created` domain outbox를 기록한다.
6. idempotency 결과 pointer와 함께 commit한다.

### 6.3 T2. 응답 변경과 자동 확정 시도

1. Party, proposal, 요청자의 participant row를 잠그고 active membership, proposal 상태·기한, expected version을 확인한다.
2. 응답과 participant version을 갱신한다.
3. 전원 accepted가 아니면 필요 시 `awaiting_calendar_refresh → open`으로 되돌리고 sync/outbox를 기록해 commit한다.
4. 전원 accepted면 T3를 같은 transaction에서 실행한다.

### 6.4 T3. 확정 시도

1. 고정 참여자 membership이 모두 active인지 다시 확인한다. 하나라도 아니면 `reproposal_required`로 종료한다.
2. 참여자별 advisory lock을 정렬 획득한다.
3. 모든 calendar snapshot이 서버 시각 기준 15분 이내인지 확인한다.
4. stale이면 `confirm_attempt += 1`, `awaiting_calendar_refresh`, `refresh_deadline_at=now()+2분`으로 전이하고 opaque refresh request와 domain outbox를 만든다.
5. fresh이면 private `calendar_busy_facts`와 활성 `confirmed_event_participants` reservation을 `[start,end)`로 검사한다.

   이 proposal의 `supersedes_confirmed_event_id`(= X)가 NOT NULL이면 자기 자신이 만든 확정이 자기 재일정을 막지 않도록 아래 예외를 적용한다. 예외가 넓으면 서로 다른 두 약속이 겹쳐 확정되어 reservation 비겹침 불변식이 깨지고, 좁으면 재일정이 자기 자신과 충돌해 거부되므로 범위를 정확히 고정한다.

   - **reservation 제외**: `confirmed_event_id = X`이고 `status='reserved'`인 row만 제외한다. 다른 confirmed event의 reservation은 어떤 경우에도 제외하지 않는다. **이것이 1차 근거다.** reservation은 서버가 소유하고 활성 event에 항상 존재하므로 기기 snapshot의 태그 유무와 무관하게 동작한다.
   - **Busy fact 제외**: `app_confirmed_event_id = X`인 fact만 제외한다. 원 event의 상태와 무관하게 제외한다. 원 event가 이미 `cancelled`이고 delete가 아직 실행되지 않았다면 외부 이벤트는 남아 있지만 죽은 일정이므로 대체를 막을 이유가 없다. 이쪽은 외부 사본의 이중 계상을 막는 2차 정제다.
   - 두 제외 모두 **확정 중인 proposal의 참여자 집합 안에서만** 적용한다. 대체 proposal의 참여자는 T1에 따라 현재 활성 멤버이므로 X의 불변 참여자 snapshot과 다를 수 있고, X에만 있던 사용자의 reservation은 이 예외가 아니라 취소 경로가 해제한다.
6. 충돌이면 `conflict_detected`로 종료한다. 응답·sync·로그에는 원인 사용자, 일정, hidden 여부를 넣지 않는다.
7. 충돌이 없으면 confirmed event와 참여자 reservation을 만들고 proposal을 `confirmed`로 전이한다. `supersedes_confirmed_event_id`가 NOT NULL이면 `calendar_write_design.md` §6.6.1에 따라 **기존 reservation 해제를 먼저 수행한 뒤** 새 reservation을 만든다. 순서가 뒤바뀌면 겹치는 시간의 대체 확정이 자기 자신 때문에 거부된다.
8. `proposal`, `proposal_response`, `confirmed_event` sync와 `proposal_confirmed` domain outbox를 같은 transaction에 기록한다.

### 6.5 T4. Freshness 재평가

- snapshot complete handler 또는 `POST .../confirmation-retry`가 `awaiting_calendar_refresh` proposal을 재평가한다.
- handler는 proposal의 `confirm_attempt`을 함께 검사해 과거 refresh 결과가 새 시도를 완료하지 못하게 한다.
- 2분 안에 모두 fresh가 되면 T3의 충돌 검사부터 재개한다.
- 2분이 지나면 자동 확정하지 않는다. 상태는 유지하고 aggregate `calendar_refresh_required`를 표시하며 명시적 재시도만 허용한다.

### 6.6 T5. Membership 종료와 Party 해산

- `membership_ended` handler는 membership 종료 transaction 안에서 해당 사용자가 참여한 모든 `open`/`awaiting_calendar_refresh` proposal을 `reproposal_required`로 바꾼다.
- `party_disbanded` handler도 해산 transaction 안에서 모든 미확정 proposal을 `party_disbanded`로 바꾼다.
- 두 전이는 비동기 eventual handler에 맡기지 않는다. membership/Party 상태와 proposal 상태 사이에 확정 가능한 틈이 없어야 한다.
- 이미 confirmed인 event에 대한 떠난 사용자의 외부 일정 처리는 상세 설계 8이 결정한다.

### 6.7 T6. 만료·취소·재제안

- scheduler는 `response_due_at <= now()`인 `open`/`awaiting_calendar_refresh`를 잠그고 `expired`로 종료한다. scheduler 지연과 무관하게 모든 mutation endpoint가 기한을 직접 검사한다.
- 취소는 expected proposal version과 권한을 검사해 `cancelled`로 전이한다.
- 재제안은 한 transaction에서 기존 proposal을 잠그고 새 제안을 T1 규칙으로 만든 뒤 기존 proposal을 `superseded`로 전이하며 양방향 ID를 기록한다.
- 재제안 대상에 `supersedes_confirmed_event_id`가 있으면 **서버가** 새 proposal에 그대로 복사한다. 클라이언트는 이 값을 지정할 수 없다. 승계하지 않으면 손자 proposal이 원 확정의 reservation을 제외하지 못해 `conflict_detected`로 막히거나, 운 좋게 확정되면 원 확정이 `active`로 남아 약속이 두 개 생긴다.
- 승계 시에는 **기존 proposal을 `superseded`로 UPDATE한 뒤에 새 proposal을 INSERT한다.** `calendar_write_design.md` §5.1의 partial unique `(supersedes_confirmed_event_id) WHERE status IN ('open','awaiting_calendar_refresh')`가 즉시 검증되므로, 순서를 지키지 않으면 재제안이 제약 위반으로 실패한다. 설계 4 §5.1 T4의 owner partial unique와 같은 패턴이다.
- 대체 대상 confirmed event가 이미 `cancelled`면 승계하지 않고 새 proposal은 독립 제안이 된다. 예외 규칙은 대상이 `active`일 때만 의미가 있다.

### 6.8 경쟁 결과

- 마지막 두 수락이 동시에 도착해도 proposal row lock 뒤 하나만 확정한다.
- 응답 변경과 membership 종료가 경쟁하면 공통 Party row lock 때문에 하나가 먼저 완료되며 최종적으로 떠난 참여자가 있는 미확정 proposal은 반드시 `reproposal_required`다.
- 서로 다른 Party의 겹치는 시간 확정은 user advisory lock과 reservation constraint로 하나만 성공하고 다른 하나는 `conflict_detected`가 된다.
- 모든 mutation은 `Idempotency-Key`와 expected version을 요구한다. 같은 key/body 재전송은 원래 resource pointer를 반환하고 다른 body는 `409 idempotency_mismatch`다.
- 멱등성 replay 전에도 현재 인증·권한을 다시 검사한다.

---

## 7. API와 역할별 권한

### 7.1 Endpoint

| Method | Path | 권한·목적 |
|---|---|---|
| POST | `/v1/parties/{party_id}/proposals` | 활성 멤버가 새 제안 생성 |
| GET | `/v1/parties/{party_id}/proposals` | 현재 활성 멤버가 목록 조회 |
| GET | `/v1/proposals/{id}` | 현재 활성 Party 멤버가 상세 조회 |
| PUT | `/v1/proposals/{id}/response/me` | 고정 참여자가 본인 응답 전체 교체 |
| POST | `/v1/proposals/{id}/confirmation-retry` | 고정 참여자가 freshness 재평가 요청 |
| POST | `/v1/proposals/{id}/cancel` | 생성자 또는 현재 owner가 미확정 제안 취소 |
| POST | `/v1/proposals/{id}/reproposal` | 생성자 또는 현재 owner가 새 제안으로 대체 |

- 모든 mutation은 `Idempotency-Key`가 필수다. 응답·취소·재시도는 `expected_version`, 응답 변경은 participant `expected_response_version`도 요구한다.
- list는 `updated_at,id` cursor pagination을 사용하고 기본 20개, 최대 50개를 반환한다.

### 7.2 권한

| 행위 | 고정 참여자·활성 | 가입 후 활성 비참여자 | 생성자 | owner | 비활성/비멤버 |
|---|---:|---:|---:|---:|---:|
| 조회 | O | O(읽기 전용) | O | O | X |
| 새 독립 제안 | O | O | O | O | X |
| 본인 응답 | O | X | O | 참여자일 때 O | X |
| confirmation retry | O | X | O | 참여자일 때 O | X |
| 취소·명시적 재제안 | 참여자 여부와 무관하게 생성자만 O | X | O | O | X |

- 비멤버·탈퇴자의 조회는 `404 not_found`, 비활성 참여자의 응답 mutation은 `403 forbidden`이다.
- 해산된 Party의 로컬 잔존 proposal mutation은 `410 party_disbanded`이며 조회는 `404 not_found`다.

---

## 8. 사용자 흐름과 화면·오프라인 상태

| 화면/상태 | 표시와 행동 |
|---|---|
| 작성 | 제목·선택 장소·시간·기한, availability 출처면 선택 슬롯 표시 |
| `open` | 참여자별 응답, 남은 시간, 본인 응답 변경 |
| 가입 전 생성 | 읽기 전용 문구, 응답 CTA 없음 |
| `awaiting_calendar_refresh` | `캘린더 확인 중`; 2분 뒤에도 미완료면 본인 원인일 때만 refresh CTA, 타인 원인은 aggregate |
| `confirmed` | 앱 내부 확정 표시. 외부 캘린더 반영 상태는 상세 설계 8 UI |
| `conflict_detected` | `새로운 일정 충돌이 확인됨`; 원인 비노출, 재제안 CTA |
| `reproposal_required` | `참여자 구성이 변경됨`; 현재 멤버로 재제안 CTA |
| `expired` | 응답 기한 만료, 재제안 CTA |
| `cancelled`/`superseded` | 종료 사유와 대체 제안 link |
| `party_disbanded` | Party 로컬 정리 후 목록으로 이동 |

- iOS는 server projection을 SwiftData에 캐시한다. 서버 성공 전 UI에서 확정으로 표시하지 않는다.
- proposal 생성과 응답은 offline mutation queue에 넣을 수 있지만 재연결 시 version·기한·membership·권한을 다시 검사한다.
- 취소, 재제안, confirmation retry는 온라인에서만 허용한다. 여러 entity와 최신 freshness 판단을 묶기 때문이다.
- offline response가 충돌하면 현재 서버 상태를 먼저 보여주고 사용자가 다시 응답하게 한다. 자동 덮어쓰지 않는다.
- 앱 foreground와 APNs sync marker 수신 시 proposal/response/confirmed event delta를 동기화한다.

---

## 9. 개인정보, Sync와 Domain Outbox

### 9.1 노출 규칙

- 현재 활성 Party 멤버에게 proposal 제목·장소·시간과 응답자 표시 이름·응답을 노출한다. 이는 사용자가 직접 작성한 협업 데이터다.
- proposal 응답에는 calendar event, Busy interval, 공개 수준, conflict owner·count·원인이 없다.
- 떠난 사용자의 표시 이름은 협업 기록에 남을 수 있고 계정 삭제 시 `탈퇴한 사용자`로 익명화한다.
- hidden 일정과 충돌해도 `proposal_time_conflict` 또는 `conflict_detected`만 반환한다.

### 9.2 Sync

- entity type은 `proposal`, `proposal_response`, `confirmed_event`를 사용한다.
- 생성·응답·상태 전이는 transaction 안에서 현재 활성 Party 멤버별 `sync_changes`를 만든다.
- proposal 참여자가 아니지만 생성 후 가입한 멤버에게도 proposal과 응답의 읽기 전용 projection을 bootstrap/sync한다.
- 탈퇴자는 membership transaction의 proposal tombstone으로 로컬 proposal·response를 삭제한다.
- payload는 허용 필드만 포함하며 서버 내부 `confirm_attempt`, refresh 대상 사용자, Busy/reservation 자료를 포함하지 않는다.

### 9.3 Domain outbox

| type | dedupe key | 의미 |
|---|---|---|
| `proposal_created` | `proposal_id` | 새 제안 |
| `proposal_response_changed` | `proposal_id:user_id:response_version` | 응답 변경 |
| `proposal_calendar_refresh_requested` | `proposal_id:confirm_attempt` | 기기 snapshot refresh 요청 |
| `proposal_confirmed` | `confirmed_event_id` | 앱 내부 확정 |
| `proposal_conflict_detected` | `proposal_id:proposal_version` | 확정 직전 충돌 |
| `proposal_reproposal_required` | `proposal_id:proposal_version` | 참여자 변경 |
| `proposal_expired` | `proposal_id` | 기한 만료 |
| `proposal_cancelled` | `proposal_id:proposal_version` | 취소 |
| `proposal_superseded` | `old_proposal_id:new_proposal_id` | 재제안 |

outbox는 내부 domain event다. 실제 수신자, APNs 문구, urgency, quiet hours와 묶음 정책은 상세 설계 9가 정한다. APNs payload에는 제목·장소·시간·응답·사용자 ID를 넣지 않고 opaque sync-needed marker만 보낸다.

---

## 10. 오류와 복구

| HTTP | code | 처리 |
|---:|---|---|
| 400 | `invalid_request` | 필드·시간·grid 검증 실패 |
| 403 | `forbidden` | 비활성 멤버 또는 권한 없는 mutation |
| 403 | `not_proposal_participant` | 가입 후 멤버의 응답 시도 |
| 404 | `not_found` | 비멤버 조회, 숨겨야 하는 proposal |
| 409 | `availability_result_stale` | availability 재검색 |
| 409 | `calendar_sync_pending` | aggregate 준비 대기 |
| 409 | `proposal_time_conflict` | 생성 시간 변경 안내, 원인 비노출 |
| 409 | `proposal_not_open` | 현재 종단 상태와 version 반환 |
| 409 | `version_conflict` | 최신 proposal/response sync 후 재시도 |
| 409 | `idempotency_mismatch` | 새 key 사용 전 요청 내용 확인 |
| 409 | `calendar_refresh_required` | 본인 calendar refresh 또는 잠시 후 명시적 retry |
| 410 | `party_disbanded` | 해당 Party 로컬 정리 |
| 429 | `rate_limited` | `Retry-After` 이후 재시도 |

- 모든 오류는 다른 멤버의 준비 상태, Busy 시간, hidden 여부와 원인 사용자를 포함하지 않는다.
- timeout이나 worker 장애는 partial confirmation을 만들지 않는다. transaction rollback 후 같은 idempotency key로 안전하게 재시도한다.

---

## 11. 테스트 가능한 인수 조건

### 생성·응답

- [ ] availability 결과의 version/hash/validUntil 중 하나라도 다르면 proposal이 생성되지 않는다.
- [ ] custom 시간이 hidden Busy와 겹치면 원인을 노출하지 않고 생성이 거부된다.
- [ ] 생성 시점의 활성 멤버 전원이 한 번씩 snapshot되고 생성자만 accepted다.
- [ ] 생성 뒤 가입한 멤버는 읽을 수 있지만 응답하지 못한다.
- [ ] declined/tentative 사용자가 기한 전 accepted로 바꿀 수 있다.
- [ ] stale response version은 최신 응답을 덮어쓰지 않는다.

### 확정·경쟁

- [ ] 전원 accepted라도 15분 freshness를 충족하지 못하면 confirmed event가 없다.
- [ ] 2분 안에 fresh snapshot이 모이면 hidden Busy와 reservation을 포함해 재검사한다.
- [ ] 충돌이 있으면 `conflict_detected`이고 사용자·일정·hidden 여부가 노출되지 않는다.
- [ ] 마지막 두 응답의 동시 요청에도 confirmed event와 reservation은 정확히 하나씩 생성된다.
- [ ] 서로 다른 Party에서 같은 사용자의 겹치는 두 제안을 동시에 확정할 수 없다.
- [ ] confirmed event 생성과 proposal 전이, sync, outbox 사이에 partial commit이 없다.

### 멤버십·종단 상태

- [ ] 고정 참여자 탈퇴·강퇴·계정 삭제와 같은 transaction에서 미확정 proposal이 `reproposal_required`가 된다.
- [ ] 신규 가입은 기존 proposal의 참여자와 확정 조건을 바꾸지 않는다.
- [ ] Party 해산 후 미확정 proposal은 어떤 경로로도 confirmed가 되지 않는다.
- [ ] response deadline이 지나면 scheduler 실행 전에도 새 응답·확정이 거부된다.
- [ ] 재제안은 새 ID와 현재 활성 멤버 snapshot을 만들고 기존 수락을 복사하지 않는다.
- [ ] confirmed proposal은 Stage 7 취소·재제안 endpoint로 변경할 수 없다.

### 멱등성·Privacy·Offline

- [ ] 같은 key/body 생성·응답 재전송은 같은 결과를 반환하고 row/outbox를 중복 생성하지 않는다.
- [ ] 같은 key의 다른 body는 `idempotency_mismatch`다.
- [ ] 탈퇴자에게 tombstone이 전달되고 이후 proposal 조회는 `404`다.
- [ ] sync/bootstrap/log/trace/APNs fixture에 Busy 원인, calendar identifier, visibility와 hidden owner가 없다.
- [ ] offline 응답은 재연결 시 기한·membership·version을 재검사하고 stale 상태를 자동 덮어쓰지 않는다.

---

## 12. 분석 지표와 민감정보 제외

### 허용 지표

- proposal 생성 성공/실패 건수와 source 구분
- 생성→첫 응답, 생성→전원 응답, 생성→확정까지의 bucketed duration
- accepted/declined/tentative aggregate 비율
- confirmed, expired, cancelled, superseded, conflict, reproposal-required 전이 수
- freshness 대기·timeout·confirmation retry 수
- idempotency replay, version conflict와 offline mutation 복구 수

### 금지 데이터

- proposal 제목, 장소와 정확한 시작·종료 시각
- 개인별 응답과 사용자 ID를 결합한 분석 자료
- calendar event 제목·장소·identifier·source key
- Busy interval, conflict owner, hidden 여부와 공개 수준
- APNs token과 기기별 민감 locator

시간 분석은 날짜·시각 원문 대신 사전 정의된 소요 시간 bucket만 사용한다.

---

## 13. 검증 계획

### 순수 단위 테스트

- proposal/response 상태 전이 table test
- 입력 정규화, IANA time zone, 30분 grid와 deadline 경계
- 전원 accepted 계산과 신규 가입 비참여 처리
- 종단 상태의 역전이 금지

### PostgreSQL 통합 테스트

- participant unique, proposal당 confirmed event unique, reservation overlap 제약
- Party/membership/proposal 잠금 경쟁
- 마지막 동시 수락, 서로 다른 Party 동시 확정, 응답 철회와 refresh 경쟁
- membership 종료·해산과 proposal 상태 전이 원자성
- idempotency pointer와 outbox dedupe

### API·Sync·Privacy 계약 테스트

- 역할별 403/404/409/410 구분과 expected version
- offline batch replay와 만료·membership 변경 거부
- 현재 멤버 upsert, 탈퇴자 tombstone, 신규 가입자의 읽기 전용 bootstrap
- 성공·오류·sync·log·trace·APNs 금지 필드 검사

### iOS·E2E

- 가능 시간 선택 → 제안 → 응답 변경 → 전원 수락 → 앱 내부 확정
- custom 충돌, stale refresh, 2분 timeout과 retry
- 참여자 탈퇴 → reproposal required → 현재 멤버로 새 제안
- 오프라인 응답 → 서버 version 충돌 → 최신 상태 표시 후 사용자 재입력
- 확정 직전 다른 Party reservation 생성 → 하나만 확정

---

## 14. 현재 구현과의 차이

| 현재 | 확정 설계 | 필요한 변경 |
|---|---|---|
| 메모리 dictionary에 proposal 저장 | 서버 source of truth + SwiftData projection | API/repository/sync 경계 추가 |
| 응답이 pending/accepted/declined 세 값 | tentative와 participant row/version | 모델 분리 |
| 응답 map이 곧 참여자 | 생성 시 membership snapshot | participant entity 추가 |
| all accepted computed property가 즉시 확정 | proposal state + freshness/conflict transaction | 상태 머신과 service 추가 |
| 제안 본문에 title/start/end만 존재 | location/time zone/deadline/source/version | 입력·projection 확장 |
| 취소·만료·재제안·탈퇴 처리 없음 | 명시적 종단 상태와 동기 전이 | command/API/UI 추가 |
| 앱 내부 확정 reservation 없음 | 외부 쓰기 전에도 충돌 차단 | confirmed event participant 저장 |
| sync·멱등성·권한 없음 | 서버 version, key, role enforcement | 공통 infrastructure 연결 |

이번 작업은 설계 확정 단계이므로 Swift 코드는 변경하지 않는다. 기존 프로젝트는 그대로 빌드·실행 가능한 상태를 유지한다.

---

## 15. 후속 설계 제약

- 상세 설계 8은 `confirmed_events`를 앱 내부 기준 데이터로 사용하고 사용자별 EventKit 쓰기 성공 여부와 분리한다.
- 상세 설계 8이 confirmed event의 변경·취소, revision, 떠난 사용자의 기존 일정 처리와 Party 해산 시 미완료 command 처리를 확정했다. 재일정은 기존 확정을 유지한 채 대체 제안을 만들고 확정 시점에 교체한다. 링크는 `proposals.supersedes_confirmed_event_id` 불변 필드 하나이며 "재일정 진행 중"은 열린 대체 제안의 존재로 파생 판정하므로, 이 문서의 종단 전이 handler는 confirmed event를 건드리지 않는다.
- 상세 설계 9는 이 문서의 domain outbox type별 수신자·문구·urgency·quiet hours·중복 억제를 정한다.
- 상세 설계 9의 APNs payload는 opaque sync marker만 사용하고 proposal 제목·장소·시간·응답을 포함하지 않는다.
- 일부 참여자 선택, 최소 인원, 복수 후보 투표는 Post-MVP privacy·제품 검토 전 현재 schema와 UI에 추가하지 않는다.
