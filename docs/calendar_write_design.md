# 확정 일정의 외부 캘린더 쓰기·변경·취소 상세 설계

## 1. 설계 상태와 핵심 결정

- 상태: 확정
- 대상: `docs/feature_design_backlog.md`의 상세 설계 8 (전체 기능 목록 9번 영역)
- 선행 설계: `docs/account_backend_design.md`, `docs/calendar_privacy_sync_design.md`, `docs/party_membership_design.md`, `docs/party_visibility_design.md`, `docs/availability_search_design.md`, `docs/proposal_lifecycle_design.md`

| 결정 | 내용 | 근거 |
|---|---|---|
| 진실의 위치 | `confirmed_events`가 앱 내부 기준 데이터이며 외부 캘린더 반영 성공 여부와 완전히 분리된다 | 설계 7 §15 |
| 본문 불변 | confirmed event의 제목·장소·시각은 생성 후 바뀌지 않는다. 변경은 취소 후 재제안으로만 한다 | 설계 7 §5.3 |
| revision 의미 | 외부 반영 세대는 사용자별 `calendar_write_commands.revision`이 담당한다. `confirmed_events.revision`은 본문 세대이며 본문 불변 원칙에 따라 1로 고정된다 | 설계 3 §12, 설계 7 §5.1의 정합 |
| 반영 상태 노출 | 외부 캘린더 반영 상태는 본인 전용 리소스이며 다른 멤버에게 어떤 형태로도(집계 포함) 노출하지 않는다 | 설계 5 §노출 규칙, 설계 7 §9.1 |
| 재일정 | 기존 확정을 즉시 지우지 않고 대체 제안이 확정될 때 교체한다. 실패하면 원래 약속이 그대로 남는다 | 재확정이 보장되지 않는 흐름에서 확정을 먼저 파괴하지 않는다 |
| 약속 취소와 관계 종료 구분 | 명시적 약속 취소만 서버가 delete command를 만든다. 탈퇴·강퇴·해산은 외부 일정을 자동 삭제하지 않고 본인 기기의 로컬 정리 제안으로 처리한다 | 타인의 개인 캘린더를 그룹 관리 작업으로 훼손하지 않는다 |
| 실행 주체 | EventKit create/delete는 지정 writer device만 수행한다. 서버는 명령 상태만 소유한다 | 설계 3 §7.1, §10 |
| `update` 폐기 | 본문이 불변이므로 `update` operation을 사용하지 않는다. 설계 3 §10.1·§10.2·§14의 update 조항은 이 설계로 폐기된다 | 설계 7 §5.3 본문 불변식 |

### 백로그 미결 질문의 답

`docs/feature_design_backlog.md`의 "앱 내부 확정 후 일부 사용자의 캘린더 쓰기 실패를 어떻게 표시할지"는 다음으로 확정한다.

- 앱 내부 확정은 그룹이 공유하는 사실이고, 외부 캘린더 반영은 각 사용자의 개인 기기 작업이다.
- 실패·대기·조치 필요는 본인에게만 상세 CTA로 표시한다.
- Party 상세, 확정 일정 카드, sync, 알림, 로그 어디에도 다른 멤버의 반영 상태나 그 집계를 넣지 않는다.
- 따라서 "일부 사용자의 실패"라는 그룹 표시는 존재하지 않는다.

---

## 2. 목표와 사용자 가치

앱에서 약속이 확정되면 각 참여자의 Apple Calendar에 같은 약속이 정확히 한 번 생성되고, 약속이 취소되면 각자의 캘린더에서도 사라진다. 기기가 꺼져 있거나 권한이 바뀌어도 상태가 유실되지 않고 다시 열었을 때 이어서 반영된다.

1. 확정된 약속을 사용자가 평소 쓰는 캘린더 앱과 위젯, 알림에서 그대로 본다.
2. 재시도나 다기기 환경에서도 같은 약속이 중복 생성되지 않는다.
3. 반영이 실패해도 앱 내부 확정 사실은 흔들리지 않고, 본인만 자기 문제를 확인하고 고칠 수 있다.
4. 취소된 약속이 캘린더에 남아 잘못된 일정으로 보이지 않는다.

---

## 3. 포함 범위와 제외 범위

### 포함

- confirmed event의 취소와 재일정(취소 + 재제안) 상태 전이
- 사용자별 `calendar_write_commands` 발급·세대(revision)·중복 방지·재시도·종단 처리
- writer device의 claim/lease/보고 계약과 다기기 전환
- destination calendar 상실, 권한 철회, 기기 미접속 시의 복구 경로
- 종일 일정과 원래 time zone 재구성 규칙
- 탈퇴·강퇴·해산 시 미완료 command 처리와 이미 반영된 외부 일정의 로컬 정리
- 취소에 따른 server-side reservation 해제
- 본인 전용 반영 상태 sync와 domain outbox type

### 제외

- 알림 문구·수신자·urgency·quiet hours·묶음 처리 → 설계 9
- Google Calendar 쓰기와 제공자 간 공통 어댑터 → Post-MVP
- 외부 캘린더에서 사용자가 직접 수정한 확정 일정의 역방향 반영(외부 → 앱) → Post-MVP
- `update` operation. 본문 불변 정책에 따라 이 설계는 `create`와 `delete`만 사용한다. 설계 3 §10.1·§10.2·§14의 update 관련 조항은 이 설계로 폐기된다
- EventKit 참석자·초대장·알람(alarm) 설정 → Post-MVP
- 앱 내 반복 약속 → Post-MVP
- 결제·요금제에 따른 반영 제한 → 설계 10

### 명시적 비목표

- 외부 캘린더는 앱 상태의 사본이며 기준 데이터가 아니다. 사용자가 캘린더 앱에서 이벤트를 지우거나 옮겨도 앱 내부 확정은 바뀌지 않는다.
- 서버는 타인의 캘린더를 직접 쓰지 않는다. 모든 쓰기는 그 사용자의 기기에서 그 사용자의 권한으로 일어난다.

---

## 4. 확정된 제품 정책

### 4.1 앱 내부 확정과 외부 반영의 분리

- 설계 7 T3이 만든 `confirmed_events`는 참여자 전원의 합의 결과이며 외부 쓰기와 독립적이다.
- 같은 transaction에서 참여자 수만큼 `create` command를 만든다. command 생성 실패는 곧 confirm transaction 실패이며 부분 커밋은 없다.
- 어떤 사용자의 command가 끝내 실패해도 confirmed event는 `active`로 남고 다른 참여자의 반영은 진행된다.
- 앱의 확정 일정 화면은 항상 confirmed event 본문을 보여준다. 반영 상태 배지는 본인 행에만 붙는다.

### 4.2 변경과 취소

- **변경 없음**: 제목·장소·시각·참여자를 바꾸는 "확정 일정 수정"은 제공하지 않는다. 설계 7이 제안 본문 변경을 재제안으로 처리한 것과 같은 이유로, 확정 후 변경은 취소 + 새 제안이다.
- **취소**: 확정 일정의 생성 제안자 또는 Party 방장이 취소할 수 있다. 취소는 `confirmed_events.status`를 `cancelled`로 전이하고 모든 활성 참여자에게 `delete` command 세대를 발급한다.
- **재일정**: 새 제안을 만들되 **기존 확정을 즉시 취소하지 않는다**. 기존 confirmed event는 `active`로 남고 새 제안이 `supersedes_confirmed_event_id`로 그것을 가리킨다. 새 제안이 확정될 때 비로소 기존 확정이 `cancelled`(`replaced`)가 되고, 이전 이벤트 delete와 새 이벤트 create가 같은 transaction에서 발급된다.
- 새 제안이 종단 실패하면(`expired`, `cancelled`, `conflict_detected`, `reproposal_required`, `party_disbanded`, `superseded`) 기존 확정이 그대로 유지된다. 파생 판정이 자동으로 false가 되므로 되돌리는 쓰기가 없다.
- 이 보류가 없으면 재일정 transaction이 커밋되는 순간 전원의 캘린더에서 이벤트가 지워지고 reservation이 풀린 채 재수락은 아직 일어나지 않아, 새 시간의 freshness나 충돌 검사가 실패하면 그룹이 원래 있던 약속까지 잃는다. 사용자에게는 "장소만 바꾸려다 약속이 사라지는" 경험이 된다.
- 새 제안은 설계 7 T1 규칙을 그대로 따르며 기존 수락을 승계하지 않는다.
- 취소 사유는 `cancelled_by_member`, `replaced`, `party_disbanded`, `account_deleted` 네 가지 enum이며, 이 중 delete command를 발급하는 것은 `cancelled_by_member`와 `replaced`다.

### 4.3 destination calendar와 writer device

- destination calendar는 사용자당 하나이며 설계 3 §5.1 온보딩에서 선택하거나 앱 전용 캘린더 생성으로 만든다.
- 설계 3 §5.2의 `calendar_connections.status`를 읽기·쓰기 두 축으로 분리한다. destination은 쓰기 대상이며 읽기 snapshot의 신선도와 무관하기 때문이다.

| 축 | 값 | freshness gate 영향 |
|---|---|---|
| `source_status` | `ready`, `stale`, `denied`, `restricted`, `revoked`, `needs_source_reselection`, `error` | 설계 3 §9의 gate는 이 값만 본다 |
| `destination_status` | `selected`, `needs_reselection`, `write_denied` | 검색을 차단하지 않고 §8.2의 본인 전용 CTA만 유발한다 |

축을 나누지 않으면 한 멤버의 destination calendar 삭제가 그 Party 전원의 공통 시간 검색을 막는다. 이는 기능 회귀일 뿐 아니라, 2인 Party에서 상대가 갑자기 검색 차단을 보면 원인이 자기 문제가 아님을 알게 되므로 §1의 본인 전용 노출 원칙과도 충돌한다.
- 서버는 destination calendar의 이름을 저장하지 않는다. 기기 로컬 opaque key만 보관하고, 서버 쪽에는 "선택됨/재선택 필요" 상태만 둔다.
- writer device는 사용자당 하나다. 전환은 새 기기의 EventKit sync 확인과 marker scan 이후 미완료 command 재할당으로 처리하며 revision을 올리지 않는다.
- destination key와 EventKit mapping이 모두 기기 로컬에 있으므로 새 기기는 이전 기기가 **어느 캘린더에** 썼는지 알 수 없다. 따라서 writer 전환과 재설치 직후의 marker scan은 destination calendar가 아니라 **읽기 가능한 모든 캘린더**의 대상 시간 window를 훑는다. marker는 앱이 만든 `plantogether://` URL이므로 다른 캘린더를 훑어도 그 결과가 서버로 가지 않는다. 이 범위 확대가 없으면 이후 create가 중복 이벤트를 만든다.
- 새 기기에 로컬 destination key가 없으면 `destination_status='needs_reselection'`으로 두고 사용자에게 재선택을 요구한다.
- destination을 바꿔도 **이미 `succeeded`한 이벤트는 옛 캘린더에 그대로 남는다.** 앱이 과거 이벤트를 새 캘린더로 옮기지 않는다. 재선택은 아직 반영하지 못한 `user_action_required` 세대에만 적용된다.
- 읽기용 sync device와 쓰기용 writer device는 별개 필드지만 초기 기본값은 같은 기기다.

### 4.4 관계 종료와 이미 반영된 외부 일정

- 탈퇴·강퇴·해산은 그 사용자의 미완료 command를 `cancelled`로 종료한다.
- 이미 `succeeded`한 외부 일정은 서버가 삭제하지 않는다. 그룹 관리 작업이 개인 캘린더를 지우게 두지 않는다.
- 대신 그 사용자의 기기가 로컬 mapping으로 "떠난 Party의 확정 일정 N건" 정리 목록을 만들고, 사용자가 선택했을 때만 삭제한다. 서버 command가 아닌 순수 로컬 작업이다.
- 근거는 해산이 **그룹 관계의 종료이지 약속 자체의 취소가 아니라는** 점이다. 다음 주 저녁 약속은 단톡방이 없어져도 그대로 성립할 수 있으므로, 앱이 임의로 참여자들의 캘린더에서 지우지 않는다. 약속 자체를 무르려면 해산 전에 취소를 쓴다.
- **수용된 한계**: 사용자가 정리 제안을 무시하면 그 이벤트는 캘린더에 영구히 남는다. 앱은 다시 지우자고 조르지 않는다. 이 한계를 감수하는 대신, 그룹 관리 작업이 개인 캘린더를 말없이 바꾸는 경로를 두지 않는다.
- 정리 목록은 일회성 sync change가 아니라 **본인 전용 영속 리소스**(`calendar_cleanup_suggestions`)로 둔다. 사용자가 처리하거나 무시할 때까지 남으며 `DELETE`로 소진된다.
- 영속 리소스로 두는 이유는 세 가지다. sync change로 만들면 tombstone보다 먼저 적용되도록 `seq` 순서를 강제해야 하고, 떠난 사용자가 오래 오프라인이면 `sync_cursor_expired` 후 bootstrap에 이미 떠난 Party가 없어 hint가 영구 유실되며, "나중에"를 눌렀을 때의 재표시 규칙이 정의되지 않는다.
- 영속 리소스이므로 tombstone 적용 순서 제약은 필요 없다. 다만 기기는 tombstone으로 로컬 mapping을 지우기 전에 정리 목록에 실린 confirmed event ID의 mapping을 별도 보관 영역으로 옮긴다.
- 탈퇴·강퇴와 해산 모두 같은 규칙을 따른다.
- 떠난 사용자의 confirmed event reservation은 해제한다. 외부 캘린더에 이벤트가 남아 있으면 다음 snapshot에서 private Busy fact로 잡히므로 실제 충돌 보호는 유지된다.

### 4.5 입력값과 검증 규칙

| 요청 | 필수 입력 | 검증 |
|---|---|---|
| 취소 | `expected_version`, `Idempotency-Key` | version 일치, event `active`, Party `active`, 요청자 활성 멤버 |
| 재일정 | 새 제안 본문(제목 1~80자, 장소 0~120자, `start_at`/`end_at`/`time_zone`), `expected_version`, `Idempotency-Key` | 설계 7 T1의 본문·source 검증 전체, `end_at > start_at`, 열린 대체 제안 없음 |
| 재반영(`/rewrite`) | `confirmed_event_id`, `Idempotency-Key` | event `active`, 요청자가 참여자이며 `released`가 아님, `destination_status='selected'` |
| claim | `device_id`, `max_count`(1~20) | 활성 세션, writer device 일치 |
| 결과 보고 | `command_id`, `lease_token`, `outcome`, 조건부 `result_locator`·`failure_class` | lease 유효, `outcome`이 enum, `succeeded`면 `result_locator` 필수 |
| destination 재선택 | 기기 로컬 opaque key, `Idempotency-Key` | 기기가 그 캘린더에 쓰기 가능함을 확인한 뒤 전송 |
| writer 전환 | `device_id`, `Idempotency-Key` | 요청 기기가 그 사용자의 등록 기기이며 EventKit 권한 보유 |

`failure_class` enum은 다음 5종이며 §8.2의 CTA 표가 이 값을 소비한다.

| 값 | 의미 | 재시도 |
|---|---|---|
| `permission_revoked` | EventKit 권한이 철회됨 | 사용자 조치 후 새 revision |
| `destination_missing` | 대상 캘린더가 사라짐 | 재선택 후 새 revision |
| `write_denied` | 대상 캘린더가 읽기 전용 | 재선택 후 새 revision |
| `rewrite_target_missing` | 재반영 대상 이벤트를 찾지 못함 | 사용자 확인 후 새 revision |
| `exhausted` | 최대 시도 횟수 초과 | 수동 재시도만 |

형식과 상한:

- `lease_token`: 서버 생성 128비트 난수의 base64url, 22자 고정.
- `result_locator`: 기기별 secret HMAC-SHA256의 base64url, 43자 고정.
- claim의 `max_count`: 1~20, 기본 10. §6.3 step 3의 N이 이 값이다.
- 시각은 모두 UTC ISO-8601로 받고 `time_zone`은 IANA 식별자만 허용한다.
- 서버는 확정 일정 본문을 재검증하지 않는다. 본문은 설계 7 T1이 이미 검증한 불변 snapshot이다.
- 모든 mutation은 `Idempotency-Key`를 요구하며 24시간 동안 결과 pointer를 보존한다.
- 검증 실패 응답에 다른 사용자의 상태나 캘린더 정보를 담지 않는다.

---

## 5. 데이터 모델과 상태 전이

### 5.1 테이블

설계 3 §11과 설계 7 §5.1의 테이블을 확장한다. 새 테이블은 없다.

| 테이블 | 이 설계가 확정하는 필드·제약 |
|---|---|
| `confirmed_events` | `status` enum(`active`,`cancelled`), `cancel_reason` enum NULL, `cancelled_at` NULL, `cancelled_by_membership_id` NULL, `revision` INT NOT NULL DEFAULT 1 CONSTRAINT `ck_confirmed_events_revision_fixed` CHECK(`revision`=1) |
| `proposals` | `supersedes_confirmed_event_id` NULL. 재일정으로 만들어진 제안이 대체하려는 confirmed event를 가리킨다. 재일정 endpoint에서만 **서버가** 설정하며 클라이언트가 body로 지정할 수 없고, 생성 후 불변이다. 설계 7 §4.2가 `supersedes_proposal_id`에 건 규칙과 같은 원칙이다 |
| `confirmed_event_participants` | `status` enum(`reserved`,`released`) NOT NULL, `released_at` NULL, `released_reason` enum(`event_cancelled`,`membership_ended`,`account_deleted`) NULL |
| `calendar_write_commands` | `confirmed_event_id`, `user_id`, `executor_device_id` NULL, `operation` enum(`create`,`delete`), `revision` INT NOT NULL, `status` enum, `attempt_count`, `ever_claimed` BOOLEAN NOT NULL DEFAULT false, `next_run_at` NOT NULL DEFAULT now(), `lease_expires_at` NULL, `lease_token` NULL, `failure_class` NULL, `result_locator` NULL, `created_at`, `updated_at`, `version` |

제약:

- `calendar_write_commands`의 세대 유일성: unique `(confirmed_event_id, user_id, revision)`.
- `next_run_at`은 NOT NULL이며 기본값이 `now()`다. NULL이면 §6.3 step 3의 claim 술어에 걸리지 않아 새 command가 영원히 실행되지 않는다.
- 재일정 유일성: partial unique `(supersedes_confirmed_event_id) WHERE status IN ('open','awaiting_calendar_refresh')`. 한 confirmed event에 동시에 열린 대체 제안은 최대 하나이며, 두 번째 요청은 `409 replacement_in_progress`다.
- 활성 command 유일성: partial unique `(confirmed_event_id, user_id) WHERE status IN ('pending_device','claimed','retryable_failed','user_action_required')`.
- lease 조회: `(executor_device_id, status, next_run_at)`.
- `revision` 고정 CHECK는 이름 있는 제약으로 만든다. §15.2의 재검토 조건이 성립하면 `ALTER TABLE ... DROP CONSTRAINT`로 완화할 수 있어야 한다.
- `ever_claimed`는 그 command가 한 번이라도 `claimed`로 전이했는지를 기록하는 단조 flag다. 한 번 true가 되면 되돌아가지 않으며 `status`가 `pending_device`로 복귀해도 유지된다. 취소 시 delete 발급 판정의 유일한 근거다.
- `result_locator`는 기기가 만든 opaque 문자열이며 EventKit identifier나 calendar identifier를 담지 않는다. 기기별 secret으로 HMAC하므로 서버가 후보 identifier를 대입해 원본을 역산할 수 없다.
- 유일한 소비자는 중복 성공 보고 판별이다. 같은 세대에 서로 다른 `result_locator`로 두 번 성공이 보고되면 기기가 이벤트를 두 개 만들었다는 신호이므로 §10의 중복 정리 경로를 유발한다. 이 용도가 없어지면 컬럼도 함께 제거한다.
- reservation exclusion constraint는 `confirmed_event_participants.status='reserved'`인 row만 대상으로 하는 partial constraint다. 다른 테이블의 컬럼을 참조할 수 없으므로 confirmed event의 취소는 참여자 row의 `status`도 같은 transaction에서 함께 갱신한다.

### 5.2 Confirmed event 상태

```text
active ─┬─ 제안자/방장 취소 ──────────▶ cancelled (cancelled_by_member)
        ├─ 대체 제안 확정 ────────────▶ cancelled (replaced)
        ├─ Party 해산 ────────────────▶ cancelled (party_disbanded)
        └─ 마지막 참여자 계정 삭제 ───▶ cancelled (account_deleted)
```

"재일정 진행 중"은 confirmed event의 저장된 필드가 아니라 **파생 판정**이다.

```sql
EXISTS (SELECT 1 FROM proposals
        WHERE supersedes_confirmed_event_id = :confirmed_event_id
          AND status IN ('open','awaiting_calendar_refresh'))
```

링크를 confirmed event의 가변 필드로 두지 않는 이유는, 대체 제안이 설계 7 §5.2의 종단 상태 6개(`conflict_detected`, `reproposal_required`, `party_disbanded`, `expired`, `cancelled`, `superseded`)로 갈 수 있고 각각 다른 transaction(scheduler의 T6, membership handler T5, 해산 T7, 취소 endpoint)에서 일어나기 때문이다. 그 모두가 confirmed event row를 잠그고 되돌려야 한다면 하나만 빠져도 confirmed event가 영원히 "재일정 진행 중"으로 굳고, `confirmed_events`가 더 이상 불변이 아니게 되어 `ck_confirmed_events_revision_fixed`의 근거도 약해진다.

파생 판정은 대체 제안이 어떤 종단 상태로 가든 자동으로 false가 되므로 write-back 자체가 필요 없다.

`cancelled`는 종단 상태이며 복구는 없다. 같은 시간에 다시 모이려면 새 제안을 만든다.

### 5.3 Write command 상태와 revision

상태 기계는 설계 3 §10.1이 확정한 것을 그대로 사용한다.

```text
pending_device → claimed → succeeded
                    ├─ retryable_failed → pending_device
                    ├─ user_action_required
                    └─ cancelled
```

`succeeded`와 `cancelled`가 종단 상태다. `user_action_required`는 사용자의 조치로 새 revision이 발급될 때 `cancelled`로 종료된다.

`retryable_failed`는 기기의 실패 보고(§6.4 step 4)만 만들며 같은 transaction에서 backoff를 적용해 즉시 `pending_device`로 복귀시킨다. 따라서 조회 시점에 이 상태로 관측되는 일은 사실상 없다. lease 만료 회수(§6.3 step 6)는 `retryable_failed`를 거치지 않고 곧바로 `pending_device`로 되돌린다. §5.1의 활성 command partial unique 술어가 `retryable_failed`를 포함하는 것은 중간 상태에서도 유일성이 깨지지 않게 하는 방어다.

**revision 발급 규칙** — revision은 `(confirmed_event_id, user_id)`마다 1부터 증가하는 외부 반영 세대다.

| 계기 | 새 revision | operation |
|---|---|---|
| 확정 | 1 | `create` |
| destination calendar 재선택 | +1 | `create` (§6.8에 따라 `user_action_required` 세대에만) |
| 사용자가 로컬 이벤트 삭제 후 재반영 요청 | +1 | `create` |
| 약속 취소 | +1 | `delete` |
| writer device 전환 | 증가 없음 | 기존 command의 `executor_device_id` 재할당 |
| 자동 재시도 | 증가 없음 | `attempt_count` 증가 |

새 revision 발급은 같은 transaction에서 기존 활성 command를 `cancelled`로 종료한 뒤 insert한다. 활성 command partial unique가 이 순서를 강제한다.

**취소 시 delete 발급 조건** — 현재 세대의 상태가 아니라 `(confirmed_event_id, user_id)`의 **전 세대 이력**으로 판단한다.

> 그 사용자의 모든 `create` 세대를 통틀어 `status='succeeded'`인 세대가 하나도 없고 `ever_claimed=true`인 세대도 하나도 없을 때만 delete를 발급하지 않는다. 그 밖의 모든 경우 `delete` revision을 발급한다.

현재 상태만 보는 판정은 다음 두 경로에서 외부 캘린더에 고아 이벤트를 남긴다.

- 기기가 claim해 EventKit 저장에 성공했지만 보고 전에 종료되고, lease 만료로 command가 다시 `pending_device`가 된 뒤 취소가 도착하는 경로. 취소가 command를 `cancelled`로 종료하면 §10의 "create 성공 후 보고 실패" 복구 경로 자체가 사라진다.
- `/rewrite`로 `succeeded`인 세대 위에 새 `create` 세대가 발급되고, 새 세대가 아직 `pending_device`인 상태에서 취소가 도착하는 경로. `succeeded`는 활성 command가 아니므로 partial unique가 이 상황을 막지 않는다.

`delete`에서 기기가 대상 이벤트를 찾지 못하면 설계 3 §10.2에 따라 idempotent success로 보고한다. 따라서 과발급은 안전하고 미발급만 위험하며, 판정 기준은 항상 안전한 쪽으로 기운다.

### 5.4 불변식

- `confirmed_events`의 본문과 참여자 snapshot은 어떤 경로로도 갱신되지 않는다. `revision`은 항상 1이다.
- `cancelled` confirmed event는 다시 `active`가 되지 않는다.
- `(confirmed_event_id, user_id)`에 활성 command는 최대 하나다.
- `confirmed_event_participants.status='reserved'`인 구간은 사용자별로 겹치지 않는다.
- confirmed event가 `cancelled`면 그 event의 모든 참여자 row는 `released`다.
- `proposals.supersedes_confirmed_event_id`는 생성 후 바뀌지 않으며, 가리키는 confirmed event는 같은 Party에 속한다.
- 한 confirmed event에 대해 `open`/`awaiting_calendar_refresh` 상태의 대체 제안은 최대 하나다.
- `confirmed_events`는 `status`와 취소 관련 필드를 제외하면 어떤 경로로도 갱신되지 않는다.
- `released`된 참여자는 같은 Party에 재가입해도 reservation과 command가 복구되지 않는다. 그 확정 일정은 그 사용자 없이 진행되며, 다시 포함하려면 새 제안을 만든다.
- 상태 전이, command 발급, `sync_changes`, `outbox_jobs`는 한 transaction에서 커밋한다.
- 서버는 어떤 응답·sync·로그에도 EventKit identifier, calendar 이름, `app_confirmed_event_id`를 남기지 않는다. `app_confirmed_event_id`는 private `calendar_busy_facts` 전용이며 Party projection·sync payload·로그·지표에 넣지 않는다.
- 다른 사용자의 command 상태는 어떤 사용자의 sync에도 포함되지 않는다.

---

## 6. 트랜잭션, 경쟁과 멱등성

### 6.1 잠금 순서

설계 7 §6.1을 이어받아 `parties` → `party_memberships` → `proposals` → `confirmed_events` → `confirmed_event_participants` → `calendar_write_commands` 순으로 잠근다. 같은 테이블의 여러 row는 UUID 오름차순으로 잠근다.

설계 7 §6.1의 사용자별 transaction-scoped advisory lock 규칙을 그대로 유지한다. reservation을 **생성**하는 transaction(W1과 설계 7 T3)은 고정 참여자 user ID를 정렬해 advisory lock을 얻은 뒤 reservation을 검사·생성한다. reservation을 **해제**만 하는 transaction(W4 취소, W6 관계 종료)은 advisory lock을 취득하지 않는다. 해제는 다른 확정을 막지 않고 허용만 넓히므로 직렬화가 필요 없다. advisory lock은 항상 Party와 proposal row lock 이후에 취득하므로 순환 대기가 생기지 않는다.

§6.9가 말하는 exclusion constraint는 최후 방어선이며 advisory lock을 대체하지 않는다. 설계 7 §5.1이 constraint 구현을 "또는 동일 의미의 직렬화된 transaction guard"로 열어 두었으므로 constraint에만 의존해서는 안 된다.

### 6.2 W1. 확정 시 create command 발급

설계 7 T3의 7~8단계 사이에 삽입된다.

1. confirmed event와 참여자 reservation을 만든다.
2. 참여자 각각에 대해 `revision=1`, `operation='create'`, `status='pending_device'` command를 insert한다.
3. `executor_device_id`는 그 사용자의 현재 `calendar_writer_device_id`로 채운다. 없으면 NULL로 두고 기기 등록 시 할당한다.
4. 본인 전용 `calendar_write_status` sync change와 `confirmed_event` sync change를 기록한다.
5. `confirmed_event_created` domain outbox를 기록한다.

### 6.3 W2. Command claim과 lease

1. writer device가 `POST /v1/calendar/write-commands/claim`을 호출한다.
2. 서버는 device ID와 활성 세션, `calendar_connections.calendar_writer_device_id` 일치를 검사한다. 불일치면 `403 not_writer_device`.
3. `user_id = 인증 사용자 AND status='pending_device' AND next_run_at <= now() AND (executor_device_id = 요청 기기 OR executor_device_id IS NULL)`인 command를 `FOR UPDATE SKIP LOCKED`로 최대 N건 잠근다. `executor_device_id`가 NULL이면 요청 기기로 채운다. 이것이 §6.2 step 3이 남긴 지연 할당의 유일한 경로다.
4. `claimed`로 전이하고 `ever_claimed=true`, `lease_token`, `lease_expires_at = now() + 90초`를 설정한다.
5. 응답에는 confirmed event 본문, `revision`, `lease_token`, `operation`을 담는다.
6. lease 만료 scheduler가 `claimed` 상태로 `lease_expires_at`을 넘긴 command를 회수한다. 회수는 한 transaction에서 `attempt_count += 1`, `next_run_at = now() + backoff(attempt)`, `status='pending_device'`, `lease_token=NULL`을 함께 적용한다. `ever_claimed`는 true로 유지한다.
7. 회수가 §6.4 step 6의 최대 시도 횟수를 넘기면 `user_action_required`(`failure_class='exhausted'`)로 종료한다. 이 규칙이 없으면 claim 후 보고 없이 죽는 기기가 backoff와 상한을 우회해 무한히 재claim한다.

### 6.4 W3. 결과 보고

1. 기기가 `POST /v1/calendar/write-commands/{id}/result`에 `lease_token`, `outcome`, `result_locator`, `failure_class`를 보낸다.
2. 판정 순서를 고정한다. ① command가 종단 상태(`succeeded`/`cancelled`)면 `409 command_cancelled`를 반환한다. 단 같은 `lease_token`으로 `succeeded`를 재전송한 경우는 step 7의 멱등 응답이다. ② 그다음 `lease_token` 불일치면 `409 stale_lease`로 거절해 다른 기기로 재할당된 결과가 상태를 덮지 못하게 한다. ③ 그 외는 정상 처리한다.
   두 코드는 기기의 후속 행동이 다르다. `stale_lease`는 "로컬 mapping을 유지한 채 다음 claim을 기다린다"이고 `command_cancelled`는 "이 세대는 끝났으니 다음 세대를 기다린다"이다. 종료된 command에 `stale_lease`를 주면 기기가 오지 않을 재claim을 기다리며 로컬 상태를 정리하지 못한다.
3. `outcome='succeeded'`면 `succeeded`로 전이하고 `result_locator`를 저장한다.
4. `outcome='retryable_failed'`면 `attempt_count += 1`, `next_run_at = now() + backoff(attempt)`, `pending_device`로 되돌린다.
5. `outcome='user_action_required'`면 `failure_class`와 함께 `user_action_required`로 전이하고 `calendar_write_action_required` domain outbox를 기록한다.
6. backoff는 30초에서 시작해 2배씩 증가하며 상한 6시간, 최대 시도 8회다. 초과하면 `user_action_required`(`failure_class='exhausted'`)로 종료한다.
7. 모든 보고는 command ID와 `lease_token` 조합으로 멱등하다. 같은 결과 재전송은 현재 상태를 그대로 반환한다.

### 6.5 W4. 취소

1. Party row와 confirmed event row를 잠그고 Party가 `active`, event가 `active`인지 확인한다.
2. 요청자가 그 Party의 **현재 활성 멤버**인지 먼저 확인한다. 아니면 설계 4 §5.6에 따라 `404 not_found`. 그 위에서 원 제안의 생성자이거나 현재 방장인지 확인한다. 아니면 `403 forbidden`. §6.7에 따라 참여자가 떠나도 confirmed event는 `active`로 남으므로, 이 검사가 없으면 이미 Party를 떠난 원 제안자가 남은 사람들의 확정 일정을 취소할 수 있다. 원 제안자가 떠나도 방장이 취소할 수 있으므로 기능 손실은 없다.
3. `expected_version` 불일치는 `409 version_conflict`.
4. `status='cancelled'`, `cancel_reason='cancelled_by_member'`, `cancelled_at`, `cancelled_by_membership_id`를 기록한다.
5. 모든 참여자 row를 `released`(`event_cancelled`)로 만든다.
6. §5.3의 조건에 따라 참여자별로 기존 활성 command를 `cancelled`로 종료하고 필요한 사용자에게 `delete` revision을 발급한다. 종료된 command가 `claimed` 상태였다면 새 delete의 `next_run_at`을 그 command의 `lease_expires_at` 이후로 설정한다. 이 지연이 없으면 기기 A가 EventKit 저장을 진행하는 동안 다른 기기가 delete를 claim해 아무것도 찾지 못하고 idempotent success로 끝낸 뒤, A의 저장이 완료되어 어떤 command도 남지 않은 고아 이벤트가 생긴다.
7. 활성 멤버 대상 `confirmed_event` sync, 본인 전용 `calendar_write_status` sync, `confirmed_event_cancelled` domain outbox를 기록한다.

### 6.6 W5. 재일정 시작

1. Party row와 confirmed event row를 잠그고 Party가 `active`, event가 `active`인지 확인한다. 열린 대체 제안이 이미 있으면 `409 replacement_in_progress`. partial unique가 경쟁 요청도 막는다.
2. W4 step 2와 같은 권한 검사를 수행한다.
3. 설계 7 T1으로 새 proposal을 만든다. 본문은 요청 body에서 새로 받으며 기존 값을 승계하지 않는다.
4. 새 proposal에 `supersedes_confirmed_event_id`를 기록한다. **proposals 테이블의 `supersedes_proposal_id`/`superseded_by_proposal_id`는 쓰지 않는다.** 설계 7 §5.2에서 `superseded`는 재제안이 만드는 종단 상태이고 원본 proposal은 `confirmed`를 유지해야 하므로, 재일정 링크는 `supersedes_confirmed_event_id` 하나로만 표현한다. confirmed event 쪽은 아무것도 쓰지 않는다.
5. **취소도 delete 발급도 하지 않는다.** 기존 확정과 reservation, 이미 반영된 외부 이벤트는 그대로 유지된다.
6. 새 제안 검증 실패는 전체를 롤백하며 기존 확정에 아무 흔적을 남기지 않는다.

### 6.6.1 W5a. 대체 제안 확정

설계 7 T3이 `supersedes_confirmed_event_id`가 NOT NULL인 proposal을 확정할 때, 같은 transaction에서 다음을 함께 수행한다.

1. 기존 confirmed event를 `cancelled`(`replaced`)로 전이하고 참여자 row를 `released`(`event_cancelled`)로 만든다.
2. §5.3의 이력 기반 조건에 따라 참여자별로 기존 이벤트의 `delete` revision을 발급한다.
3. 새 confirmed event와 reservation을 만들고 참여자별 `create` revision을 발급한다(W1). **반드시 step 1의 기존 reservation 해제가 순서상 앞선다.** 새 시간이 원래 시간과 겹치는 흔한 경우(30분 이동 등)에 순서가 뒤바뀌면 reservation partial exclusion constraint가 자기 자신 때문에 insert를 거부한다.
4. delete와 create가 같은 transaction에서 발급되므로 두 이벤트가 동시에 캘린더에 존재하는 구간이 기기 실행 순서만큼만 존재하고, 어느 쪽도 유실되지 않는다.

**대체 대상의 자기 충돌 제외** — 새 시간이 기존 확정과 겹치면(예: 19:00 약속을 19:30으로 30분만 이동) 자기 자신의 reservation과 Busy fact 때문에 확정이 막힌다. 따라서 설계 7 T3 step 5의 충돌 검사에서 다음 둘을 제외한다.

- 확정 대상 proposal의 `supersedes_confirmed_event_id`가 가리키는 confirmed event의 `confirmed_event_participants` reservation. **이것이 1차 근거다.** reservation은 서버가 소유하므로 기기 snapshot의 태그 유무와 무관하게 항상 동작한다.
- 그 confirmed event를 marker로 가리키는 `calendar_busy_facts`. 이를 위해 설계 3 §7.2의 snapshot 업로드 allowlist에 `app_confirmed_event_id`를 추가한다. 기기가 `plantogether://` marker에서 읽은 값이다.

이 필드는 서버에 "이 사용자의 외부 캘린더에 이 확정 일정이 실제로 존재한다"를 알려주므로 `calendar_write_commands`에 이은 **반영 상태의 두 번째 출처**다. 따라서 private busy fact 전용으로 가두고 Party projection에는 절대 싣지 않는다. 새어 나가면 다른 멤버가 특정 멤버의 반영 여부를 직접 보게 되어 §1의 본인 전용 원칙이 깨진다.

**롤아웃 제약** — 이 필드는 새 snapshot에만 담긴다. 필드 도입 이전에 업로드된 활성 generation의 busy fact는 값이 NULL이므로, 이미 캘린더에 반영된 확정 일정이 태그 없는 Busy로 남아 재일정이 자기 자신과 충돌해 `conflict_detected`로 종료된다. 원인은 사용자에게 노출되지 않으므로(설계 7 §6.4 step 6) 영문 모를 재일정 불가 상태가 롤아웃 시점의 전 사용자에게 동시에 발생한다. 따라서 두 가지를 함께 적용한다.

1. reservation 제외를 1차 근거로 삼고 Busy fact 제외는 2차 정제로 둔다. reservation은 태그와 무관하게 동작하므로 이것만으로도 재일정이 성립한다.
2. `app_confirmed_event_id`는 nullable로 두고 **backfill도 강제 re-snapshot도 하지 않는다.** Busy fact 제외는 각 사용자의 자연스러운 다음 snapshot부터 점진 적용되며, 그사이에도 reservation 제외가 작동하므로 기능 결함이 없다.

   전 connection의 active generation을 stale 처리하는 방식은 채택하지 않는다. 설계 3 §9의 freshness gate가 "한 명이라도 stale이면 `calendar_sync_pending`"이므로 롤아웃 순간 **모든 Party의 가능 시간 검색이 동시에 차단**되고, 재-snapshot은 각 사용자의 앱 활성화에 의존하므로(설계 3 §7.3) 회복에 며칠이 걸린다. 정확성 이득은 없고 가용성 손실만 크다.

### 6.6.2 W5b. 대체 제안 종단 실패

- 설계 7의 어떤 종단 전이도 confirmed event를 건드리지 않는다. 대체 제안이 종단 상태가 되는 순간 §5.2의 파생 판정이 false가 되고 기존 확정은 `active`로, reservation은 그대로 남는다.
- 사용자에게는 재일정 시도가 실패했고 원래 약속이 유효하다고 표시한다.
- 기존 확정을 명시적으로 취소하면(W4) 열린 대체 제안도 같은 transaction에서 `cancelled`로 함께 종료한다. 이 방향의 쓰기는 proposal 쪽이므로 설계 7의 취소 규칙을 그대로 쓴다.
- partial unique는 열린 상태만 대상으로 하므로, 대체 제안이 실패한 뒤 같은 confirmed event에 다시 재일정을 열 수 있다.

### 6.7 W6. Membership 종료와 Party 해산

- `membership_ended` handler는 종료 transaction 안에서 그 사용자의 미완료 command를 `cancelled`로 종료하고, 그 사용자가 참여한 `active` confirmed event의 참여자 row를 `released`(`membership_ended`)로 만든다. confirmed event 자체는 `active`로 남는다.
- 떠난 사용자에게 `confirmed_event` tombstone을 보내고 같은 transaction에서 `calendar_cleanup_suggestions` row를 만든다. row는 confirmed event ID만 담고 본문을 담지 않는다. 기기는 자신의 로컬 mapping으로만 대상을 복원한다.
- `party_disbanded` handler는 해산 transaction 안에서 그 Party의 모든 `active` confirmed event를 `cancelled`(`party_disbanded`)로 만들고, 모든 참여자 row를 `released`로, 모든 미완료 command를 `cancelled`로 종료한다. delete command는 발급하지 않는다.
- 해산 handler도 마지막 활성 멤버 전원에게 `confirmed_event` tombstone과 `calendar_cleanup_suggestions` row를 함께 발행한다. 이것이 없으면 해산된 Party의 모든 멤버가 캘린더에 유효하지 않은 일정을 영구히 보유하고 그것을 알거나 지울 경로가 앱 어디에도 없다. 설계 4 T7 step 7의 tombstone 목록에 `confirmed_event`가 포함되어야 한다.
- 계정 삭제는 그 사용자의 command와 `result_locator`를 삭제하고 참여자 row를 `released`(`account_deleted`)로 만든다. 남은 참여자가 없으면 confirmed event를 `cancelled`(`account_deleted`)로 만든다.

세 경로의 실행 시점은 서로 다르며 claim 틈이 없는 근거도 다르다.

| 경로 | 실행 시점 | claim 틈이 없는 근거 |
|---|---|---|
| 탈퇴·강퇴 | 설계 4 T5·T6과 **같은 transaction** | 멤버십 종료와 command 종료가 원자적이다 |
| 해산 | 설계 4 T7 step 10과 **같은 transaction** | Party 해산과 command 종료가 원자적이다 |
| 계정 삭제 | 설계 2 §10의 worker 단계 | **step 1의 API transaction에서 세션과 기기 등록이 이미 폐기**되므로 그 기기는 이후 어떤 command도 claim할 수 없다 |

계정 삭제는 설계 2 §10과 설계 4 §5.8이 24시간 목표의 비동기 작업으로 확정한 것이므로 동기 실행을 요구하지 않는다. 안전성의 근거는 "동기 실행"이 아니라 "세션 폐기 선행"이다.

### 6.8 W7. Destination calendar 상실과 재선택

1. 기기가 destination calendar를 찾지 못하면 `user_action_required`(`failure_class='destination_missing'`)로 보고한다.
2. 서버는 `destination_status='needs_reselection'`으로 전이한다. §4.3에 따라 이 축은 freshness gate를 건드리지 않으므로 Party의 공통 시간 검색은 계속 동작한다.
3. 사용자가 새 destination을 선택하면 서버가 그 사용자의 모든 `user_action_required` command에 대해 새 revision을 발급한다.
4. 재선택 처리는 command 수만큼 반복되므로 한 transaction에서 배치로 처리하고 `Idempotency-Key`로 보호한다.

### 6.9 경쟁 결과

- 두 기기가 동시에 claim하면 `SKIP LOCKED`로 한 기기만 얻고, 다른 기기는 빈 목록을 받는다.
- lease 만료 후 재할당된 command에 이전 기기가 결과를 보고하면 `409 stale_lease`로 거절되며, 그 기기는 다음 sync에서 상태를 다시 읽는다.
- 취소와 결과 보고가 경쟁하면 confirmed event row lock 때문에 순서가 정해진다. 취소가 먼저면 보고는 `409 command_cancelled`를 받고, 보고가 먼저면 취소가 `succeeded`를 보고 delete를 발급한다.
- 취소와 다른 Party의 확정이 경쟁하면 reservation 해제가 먼저 커밋된 경우에만 그 시간이 다시 확정 가능해진다. 중간 상태에서 두 확정이 동시에 성립하는 경우는 exclusion constraint가 막는다.
- 모든 mutation endpoint는 `Idempotency-Key`와 `expected_version`을 요구한다. 같은 key와 다른 body는 `409 idempotency_mismatch`다.

---

## 7. API와 역할별 권한

### 7.1 Endpoint

| Method | Path | 설명 |
|---|---|---|
| GET | `/v1/parties/{id}/confirmed-events` | Party의 확정 일정 목록. 본인 반영 상태만 포함. `updated_at,id` cursor pagination, 기본 20개·최대 50개. `cancelled`는 기본 제외하며 `include_cancelled=true`로 포함 |
| GET | `/v1/confirmed-events/{id}` | 확정 일정 상세 |
| POST | `/v1/confirmed-events/{id}/cancel` | 약속 취소 |
| POST | `/v1/confirmed-events/{id}/reschedule` | 기존 확정 유지 상태로 대체 제안 생성 |
| POST | `/v1/confirmed-events/{id}/rewrite` | 본인 외부 일정 재반영 요청(새 create revision) |
| POST | `/v1/calendar/write-commands/claim` | writer device의 명령 lease |
| POST | `/v1/calendar/write-commands/{id}/result` | 실행 결과 보고 |
| GET | `/v1/calendar/write-status` | 본인 반영 상태 요약과 조치 필요 목록 |
| GET | `/v1/calendar/cleanup-suggestions` | 관계가 끝난 Party의 잔존 확정 일정 ID 목록 |
| DELETE | `/v1/calendar/cleanup-suggestions/{id}` | 정리 완료·무시로 소진 |
| POST | `/v1/calendar/destination` | destination 재선택과 대기 command 재발급 |
| POST | `/v1/calendar/writer-device` | writer device 전환과 미완료 command 재할당 |

### 7.2 권한

| 작업 | 권한 |
|---|---|
| 확정 일정 조회 | 해당 Party의 활성 멤버 |
| 취소·재일정 | 현재 활성 멤버 전제 위에서 원 제안의 생성자 또는 현재 방장 |
| 본인 재반영 요청 | 본인, 그리고 해당 confirmed event의 참여자 |
| command claim·결과 보고 | 본인의 활성 writer device |
| destination 재선택 | 본인 |
| writer device 전환 | 본인, 그리고 요청 기기가 그 사용자의 등록 기기 |

### 7.3 오류 코드

| 코드 | 의미 |
|---|---|
| `403 not_writer_device` | writer로 지정되지 않은 기기의 claim |
| `403 forbidden` | 활성 멤버지만 원 제안자도 방장도 아닌 사용자의 취소·재일정 |
| `409 stale_lease` | 만료·재할당된 lease의 결과 보고 |
| `409 command_cancelled` | 이미 종료된 command에 대한 보고 |
| `409 version_conflict` | expected version 불일치 |
| `409 event_already_cancelled` | 종단 상태 confirmed event에 대한 재취소, 재일정, `/rewrite` |
| `409 replacement_in_progress` | 이미 재일정이 진행 중인 확정 일정에 새 재일정 요청 |
| `410 party_disbanded` | 해산된 Party의 확정 일정 mutation |
| `409 destination_not_selected` | destination 미선택 상태의 재반영 요청. 문서 전반이 상태 충돌에 409를 쓰므로 통일한다 |

---

## 8. 사용자 흐름과 화면·오프라인 상태

### 8.1 확정 직후

- 확정 화면은 즉시 "약속이 확정되었습니다"를 보여준다. 외부 반영을 기다리지 않는다.
- 본인 반영 상태는 카드 하단에 작은 배지로 붙는다: `반영 중`, `내 캘린더에 추가됨`, `조치 필요`.
- 앱이 포그라운드면 claim을 즉시 시도하므로 보통 배지가 몇 초 안에 `추가됨`으로 바뀐다.

### 8.1.1 정상·빈 상태·로딩

| 상태 | 화면 |
|---|---|
| 정상 | 확정 일정 카드에 본문과 본인 반영 배지 `내 캘린더에 추가됨` |
| 빈 상태 | 확정된 약속이 없으면 "아직 확정된 약속이 없습니다"와 제안 만들기 CTA. 캘린더 연결 상태는 언급하지 않는다 |
| 로딩 | 목록은 skeleton, 반영 배지는 마지막으로 알려진 상태를 유지하고 조용히 갱신한다. 반영 상태 때문에 카드 전체를 로딩으로 덮지 않는다 |
| 부분 로딩 | claim 진행 중에는 배지만 `반영 중`으로 바꾸고 본문은 그대로 둔다 |
| 오류 | 목록 조회 실패는 캐시된 확정 일정을 계속 보여주고 상단에 재시도 배너를 둔다 |

확정 일정 본문은 서버 projection에서 오므로 캘린더 권한이 없어도 항상 보인다. 권한 문제는 반영 배지에만 나타난다.

### 8.2 조치 필요 상태

| `failure_class` | 사용자에게 보이는 것 | CTA |
|---|---|---|
| `permission_revoked` | 캘린더 권한이 해제되었습니다 | 설정에서 권한 다시 허용 |
| `destination_missing` | 저장할 캘린더를 찾을 수 없습니다 | 캘린더 다시 선택 |
| `write_denied` | 이 캘린더에 저장할 수 없습니다 | 다른 캘린더 선택 |
| `rewrite_target_missing` | 캘린더에서 일정을 찾을 수 없습니다 | 다시 추가 |
| `exhausted` | 여러 번 시도했지만 저장하지 못했습니다 | 다시 시도 |

각 CTA는 본인 화면에만 있다. Party 멤버 목록과 확정 일정 참여자 행에는 반영 상태 열 자체가 없다.

### 8.3 writer device 미접속

- 다른 기기에서 앱을 열면 "이 기기에서 캘린더에 저장하기"를 제안하고, 승인하면 writer를 전환하고 미완료 command를 재할당한다.
- 전환 전에는 `반영 대기 중 · 지정 기기에서 앱을 열어주세요`를 표시한다.

### 8.4 오프라인

- claim과 결과 보고는 네트워크가 필요하다. 오프라인이면 로컬 큐에 보고를 쌓고 복귀 시 `lease_token`과 함께 재전송한다.
- lease가 이미 만료되어 `409 stale_lease`를 받으면 로컬 mapping은 유지한 채 다음 claim을 기다린다. lease 만료는 새 revision을 발급하지 않으므로 돌아오는 것은 **같은 세대의 재claim**이며, 그 재실행이 marker scan으로 기존 이벤트를 찾아 중복을 만들지 않는다.
- 재일정 진행 중인 확정 일정은 카드에 `새 시간 제안 중` 배지를 붙이고 원래 시간을 그대로 보여준다. 대체 제안이 실패하면 배지만 사라지고 약속은 유지된다는 것을 문구로 설명한다.
- **재일정은 대체 제안이 종단 상태가 되면 종료된다.** 이어서 하려면 확정 일정에서 재일정을 다시 시작하거나, Party 화면에서 새 제안을 만든다. 두 경로 모두 정상이다.
- 다만 실패한 대체 제안 **화면 안에** 새 독립 제안을 만드는 CTA는 두지 않는다. 그 자리에서 만든 제안은 `supersedes_confirmed_event_id`를 갖지 않아 원 확정과 충돌하거나 약속을 두 개 만들기 때문이다. 설계 7 §7.2의 일반 "새 제안" 경로는 Party 화면에 그대로 남는다.
- 취소와 재일정은 오프라인에서 큐에 넣지 않는다. 참여자 전원에게 영향을 주는 작업이므로 온라인에서만 허용하고 오프라인에서는 버튼을 비활성화한다.

### 8.5 EventKit 쓰기 본문

- 제목: confirmed event 제목
- 시각: `start_at`/`end_at`을 confirmed event의 `time_zone`으로 재구성한다. 종일 일정은 `isAllDay`와 원래 time zone의 자정 경계를 사용한다.
- 장소: 장소가 있으면 `location`에 넣는다.
- URL: `plantogether://confirmed-event/{confirmed_event_id}` marker (설계 3 §10.2)
- notes: 앱 이름과 확인 링크만 넣는다. 참여자 명단, 다른 사용자의 이름, Party 이름은 넣지 않는다.
- 참석자, 주최자, 알람은 설정하지 않는다.

---

## 9. 개인정보, Sync와 Domain Outbox

### 9.1 노출 규칙

- 반영 상태는 `recipient_user_id = 본인`인 sync change로만 전달한다.
- confirmed event projection은 모든 활성 참여자에게 동일하며 반영 상태 필드를 포함하지 않는다.
- 서버는 EventKit identifier, calendar identifier, calendar 이름을 저장·전송·기록하지 않는다. `result_locator`는 이들과 역산 불가능한 opaque 값이다.
- `calendar_cleanup_suggestions`는 confirmed event ID만 담는다. 떠난 사용자의 기기가 로컬 mapping으로 본문을 복원하며 서버는 본문을 다시 보내지 않는다.
- 취소자의 표시 이름은 활성 참여자 전원에게 노출한다. 설계 7 §9.1이 응답자 표시 이름을 전원에게 노출하므로 취소자만 가리는 것은 비일관적이고, 사용자는 누가 취소했는지 알아야 한다. 이는 사용자가 직접 만든 협업 데이터이며 캘린더 내용이 아니다.

### 9.2 Sync entity type

| entity type | 수신자 | 내용 |
|---|---|---|
| `confirmed_event` | 활성 참여자 전원 | 본문 snapshot, `status`, `cancel_reason`, 취소자 표시 이름, 파생 `has_open_replacement`, `version` |
| `calendar_write_status` | 본인 | confirmed event ID, `operation`, `revision`, `status`, `failure_class` |
| `calendar_connection` | 본인 | destination 선택 상태, writer device |

떠난 사용자와 해산 Party의 마지막 활성 멤버 전원에게 `confirmed_event` tombstone과 `calendar_cleanup_suggestion`을 함께 보낸다. 두 경로 모두 §6.7이 정한 같은 transaction에서 발행한다. `calendar_cleanup_suggestion`은 본인 전용 entity type이며 bootstrap 재동기화에도 살아남는다.

### 9.3 Domain outbox type

| type | 계기 | dedupe key |
|---|---|---|
| `confirmed_event_created` | W1 | `confirmed_event_id` |
| `confirmed_event_cancelled` | W4, W6 | `confirmed_event_id` |
| `calendar_write_command_pending` | 확정 후 2시간이 지나도 create가 `pending_device`인 경우. 이후 24시간 간격으로 최대 3회까지 재발행 | `command_id:notice_seq` |
| `calendar_write_action_required` | W3 step 5 | `command_id` |

문구, 수신자 세부, urgency, quiet hours, 묶음 처리는 설계 9가 정한다. 이 설계는 type과 dedupe key만 확정한다. APNs payload는 opaque sync marker만 담고 제목·장소·시각을 담지 않는다.

---

## 10. 오류와 복구

| 상황 | 처리 |
|---|---|
| 권한 철회 | connection `revoked`, 활성 command `user_action_required(permission_revoked)`, 권한 복구 시 새 revision 발급 |
| destination 삭제 | W7 |
| 기기 초기화·재설치 | 새 device 등록, full snapshot, writer 전환, marker scan 후 미완료 command 재할당 |
| lease 만료 | scheduler가 `attempt_count`를 올리고 backoff 후 `pending_device`로 회수. 상한 초과 시 `exhausted` |
| create 성공 후 보고 실패 | 같은 기기가 로컬 mapping과 marker로 기존 이벤트를 찾아 동일 결과를 재보고 |
| create 중복 의심 | marker scan에서 같은 confirmed event ID marker가 2건 이상이고, **현재 destination calendar 안에 있으며 제목·시각이 confirmed event 본문과 일치하는** 사본만 하나 남기고 삭제한다. 옛 destination의 사본과 사용자가 직접 편집·복제한 이벤트는 지우지 않는다 |
| delete 대상 없음 | idempotent success |
| 사용자가 캘린더에서 직접 삭제 | 자동 재생성하지 않는다. 확정 일정 카드에서 `내 캘린더에 다시 추가` CTA로 새 create revision을 요청한다 |
| 사용자가 캘린더에서 직접 시간 변경 | 앱은 확정 본문을 바꾸지 않는다. 다음 snapshot에서 그 이벤트는 본인의 private Busy fact로 반영된다 |
| 대체 확정 후 old delete 실패 + new create 성공 | 캘린더에 두 이벤트가 남는다. 서버는 자동 복구하지 않고 본인 CTA(`조치 필요`)로만 정리한다 |
| 확정 시각이 지난 미완료 create | scheduler가 `end_at + 6시간` 경과 시 `cancelled`(`expired_window`)로 종료한다. 지난 약속을 뒤늦게 캘린더에 만들지 않는다 |
| 서버 command 누락 의심 | 기기가 sync의 `calendar_write_status`와 로컬 mapping을 대조해 차이를 보고하고 사용자에게 재반영 CTA를 노출한다 |

---

## 11. 테스트 가능한 인수 조건

### 확정과 create

- [ ] 3명 확정 시 각 사용자에게 `revision=1` create command가 정확히 하나씩 생긴다.
- [ ] confirm transaction이 롤백되면 command도 남지 않는다.
- [ ] writer device가 없는 사용자의 command는 `executor_device_id=NULL`로 대기하고 기기 등록 시 할당된다.
- [ ] 같은 command를 두 번 실행해도 EventKit 이벤트는 하나다.
- [ ] create 성공 후 보고가 실패하고 재시도해도 이벤트는 하나이며 최종 상태는 `succeeded`다.
- [ ] 기기를 재설치하거나 writer를 전환한 뒤에도 이전 기기가 다른 캘린더에 만든 이벤트를 marker scan이 찾아내 중복이 생기지 않는다.
- [ ] 사용자가 직접 복제하거나 편집한 이벤트, 옛 destination의 사본은 중복 정리에서 삭제되지 않는다.
- [ ] 한 멤버의 `destination_status='needs_reselection'`이 그 Party의 공통 시간 검색을 막지 않는다.
- [ ] 종일 일정과 원래 time zone이 캘린더에서 원래 날짜 경계로 재구성된다.

### 취소와 재일정

- [ ] 취소 시 모든 참여자 reservation이 `released`가 되고 같은 시간의 새 확정이 가능해진다.
- [ ] 어떤 세대에서도 claim된 적 없는 사용자(`ever_claimed=false`, `succeeded` 세대 없음)에게는 delete command가 생기지 않는다.
- [ ] claim 이력이 있으면 결과 보고 여부와 무관하게 delete command가 생긴다. claim 후 보고 전에 종료된 기기가 만든 이벤트가 취소 후에도 캘린더에 남지 않는다.
- [ ] `succeeded` 세대 위에 새 create 세대가 `pending_device`인 상태에서 취소해도 delete command가 생긴다.
- [ ] `succeeded`였던 사용자의 캘린더에서 이벤트가 사라진다.
- [ ] delete 시 이벤트가 이미 없으면 `succeeded`로 끝난다.
- [ ] 재일정 시작은 기존 확정과 reservation, 외부 이벤트를 건드리지 않는다.
- [ ] 대체 제안이 확정되면 같은 transaction에서 이전 이벤트 delete와 새 이벤트 create가 함께 발급된다.
- [ ] 대체 제안이 6개 종단 상태 각각으로 실패해도 원래 확정이 `active`로 남고 confirmed event row에 아무 쓰기가 없다.
- [ ] 대체 제안 실패 후 같은 confirmed event에 재일정을 다시 열 수 있다.
- [ ] 대체 제안을 재제안하면 손자 proposal이 `supersedes_confirmed_event_id`를 승계하고, `superseded` UPDATE가 INSERT보다 앞서 partial unique 위반이 나지 않는다.
- [ ] `app_confirmed_event_id`가 NULL인 구세대 snapshot에서도 reservation 제외만으로 재일정이 확정된다.
- [ ] 기존 확정과 겹치는 시간으로 재일정해도 자기 자신의 reservation과 Busy fact 때문에 막히지 않는다.
- [ ] 재일정 진행 중인 확정 일정에 대한 두 번째 재일정 요청은 `409 replacement_in_progress`다.
- [ ] 종단 상태 confirmed event의 재취소는 `409 event_already_cancelled`다.

### 관계 종료

- [ ] 탈퇴자의 미완료 command가 `cancelled`가 되고 그 기기가 더 이상 claim하지 못한다.
- [ ] 떠난 원 제안자의 취소 요청이 `404 not_found`로 거절되고 현재 방장의 취소는 성공한다.
- [ ] 계정 삭제 사용자의 기기는 세션 폐기 시점 이후 어떤 command도 claim하지 못한다.
- [ ] 탈퇴 후에도 confirmed event는 남은 참여자에게 `active`다.
- [ ] 탈퇴자에게 tombstone과 `calendar_cleanup_suggestion`이 함께 도착하고, cursor 만료 후 bootstrap으로 재동기화해도 정리 목록이 남는다.
- [ ] 해산은 마지막 활성 멤버 전원에게 tombstone과 `calendar_cleanup_suggestion`을 보낸다.
- [ ] 탈퇴자의 reservation이 해제되어 다른 Party에서 같은 시간이 확정 가능해진다.

### Lease와 경쟁

- [ ] 두 기기 동시 claim에서 한 기기만 command를 얻는다.
- [ ] writer가 아닌 기기의 claim은 `403 not_writer_device`다.
- [ ] claim 질의가 본인 command만 반환하며 다른 사용자의 확정 일정 본문이 응답에 나타나지 않는다.
- [ ] `executor_device_id`가 NULL인 command를 claim하면 요청 기기로 채워진다.
- [ ] 취소 시점에 `claimed`였던 create가 있으면 delete의 `next_run_at`이 그 lease 만료 이후이며, 저장을 마친 A의 이벤트가 최종적으로 삭제된다.
- [ ] lease 만료 후 이전 기기의 보고는 `409 stale_lease`다.
- [ ] lease 만료 회수가 `attempt_count`를 증가시키므로 claim 후 보고하지 않는 기기가 상한 없이 재claim하지 못한다.
- [ ] 취소와 결과 보고 동시 도착에서 최종 상태가 결정적이다.
- [ ] 8회 재시도 후 `user_action_required(exhausted)`로 종료한다.
- [ ] `end_at + 6시간`이 지난 미완료 create는 `expired_window`로 종료되어 과거 일정을 만들지 않는다.
- [ ] `calendar_write_command_pending` 알림이 확정 후 2시간에 처음 발행되고 최대 3회를 넘지 않는다.

### Privacy

- [ ] 다른 멤버의 응답·sync·화면 어디에도 반영 상태와 그 집계가 없다.
- [ ] 요청, DB, 로그, trace, APNs payload에 EventKit identifier와 calendar 이름이 없다.
- [ ] `result_locator`로 원본 identifier를 역산할 수 없다.
- [ ] `calendar_cleanup_suggestions`에 확정 일정 본문이 없다.
- [ ] 다른 멤버의 projection·sync·API 응답·지표에 `app_confirmed_event_id`가 없다.

---

## 12. 분석 지표와 민감정보 제외

### 허용 지표

- 확정 → 첫 create 성공까지의 지연 분포
- command claim 지연, 실행 시간, 재시도 횟수 분포
- `failure_class`별 발생 수와 사용자 조치 후 복구율
- writer device 전환 횟수, destination 재선택 횟수
- 취소와 재일정 발생 수, 취소 시점과 약속 시각의 간격 구간
- 종단 `user_action_required` 비율

### 금지 데이터

- 일정 제목, 장소, 메모, 참석자
- EventKit identifier, calendar identifier, calendar 이름, `app_confirmed_event_id`
- `result_locator` 원문
- 특정 사용자의 반영 실패를 다른 사용자와 연결하는 조합 키
- Party 이름과 멤버 이름

지표는 confirmed event ID와 user ID를 직접 담지 않고 집계 차원(시간대, 실패 분류, 시도 횟수 구간)만 사용한다.

---

## 13. 검증 계획

### 순수 단위 테스트

- revision 발급 규칙 표의 전 조합
- 이력 기반 delete 발급 판정: `ever_claimed`와 `succeeded` 세대 유무의 전 조합, `/rewrite` 후 취소 경로
- backoff 계산과 최대 시도 종료
- confirmed event 상태 전이표와 종단 상태 역전이 금지
- 종일·time zone 재구성과 DST 경계

### PostgreSQL 통합 테스트

- 활성 command partial unique와 세대 unique
- reservation partial exclusion constraint, 취소 후 재확정 가능
- `SKIP LOCKED` 동시 claim
- 상태 전이 + sync change + outbox 원자성
- membership 종료·해산·계정 삭제 handler의 같은 transaction 처리

### iOS 단위·통합 테스트

- 로컬 mapping과 marker scan 기반 중복 방지
- create와 delete 각각의 대상 없음 처리
- 오프라인 보고 큐와 `stale_lease` 복구
- 떠난 Party의 로컬 정리 목록 생성과 tombstone 적용 순서

### E2E

- 3명 확정 → 각자 캘린더 반영 → 취소 → 각자 캘린더 삭제
- 권한 철회 → 조치 → 복구
- destination 삭제 → 재선택 → 새 revision 성공
- writer device 전환 후 미완료 command 완료
- 재일정: 취소 + 새 제안 → 재확정 → 캘린더 교체 확인

### Privacy 회귀

- 응답·sync·로그·trace·APNs fixture allowlist 검사
- 다른 사용자 반영 상태 노출 시도 스캔

---

## 14. 현재 구현과의 차이

| 항목 | 현재 | 목표 |
|---|---|---|
| `CalendarService` | 권한 요청과 전체 캘린더 읽기만 지원 | 권한·source 선택·snapshot reader·command writer로 분리 |
| destination calendar | 개념 없음 | 사용자당 하나 선택 또는 앱 전용 캘린더 생성, `source_status`와 분리된 `destination_status` |
| writer device | 개념 없음 | 사용자당 하나, 전환과 재할당 지원 |
| 확정 일정 | 메모리 `EventProposal.isConfirmed` 계산 | 서버 `confirmed_events` projection |
| 외부 쓰기 | 없음 | 사용자별 command 상태 기계와 로컬 mapping |
| 취소 | 없음 | `cancelled` 종단 상태와 delete command |
| 로컬 mapping 저장소 | 없음 | SwiftData에 command ID, confirmed event ID, destination key, EventKit identifier |

기존 Swift 데모는 각 단계에서 계속 빌드·실행 가능해야 한다.

---

## 15. 미결정 사항과 후속 설계 제약

### 15.1 미결정 사항

| 항목 | 현재 처리 | 결정 시점 |
|---|---|---|
| 정리 제안을 무시한 잔존 이벤트 | 영구히 남는다(§4.4의 수용된 한계) | 출시 후 실제 발생 빈도를 보고 재검토 |
| 계정 삭제 사용자의 잔존 이벤트 | 앱이 삭제되어 정리 경로가 없다 | 설계 11(계정 삭제·운영)에서 안내 문구로 보완 |
| `result_locator` 유지 여부 | 중복 성공 보고 판별에만 쓴다 | W3 구현에서 실효성이 없으면 컬럼 제거 |
| 확정 일정 알람(alarm) 설정 | 설정하지 않는다 | Post-MVP 사용자 요청 기준 |
| 여러 destination calendar | 사용자당 하나 | Post-MVP 복수 캘린더 범위 |
| 반영 상태 재시도의 수동 트리거 빈도 제한 | 제한 없음 | 남용이 관측되면 rate limit 추가 |

### 15.2 후속 설계 제약

- 설계 9는 이 문서의 domain outbox type 4종에 대한 수신자·문구·urgency·quiet hours·중복 억제를 정한다.
- 설계 9의 APNs payload는 opaque sync marker만 사용하고 확정 일정 제목·장소·시각을 담지 않는다.
- 설계 9는 `calendar_write_action_required`가 본인에게만 가는 알림임을 유지한다.
- 외부 → 앱 역방향 반영, 참석자 초대, 알람 설정, Google Calendar 쓰기는 Post-MVP 검토 전 현재 schema와 UI에 추가하지 않는다.
- 확정 일정의 in-place 본문 수정은 도입하지 않는다. 필요해지면 `confirmed_events.revision`의 CHECK 제약과 설계 7의 본문 불변식을 함께 재검토한다.
