# Apple Calendar 권한·동기화·공개 정책 상세 설계

## 1. 설계 상태와 핵심 결정

- 상태: 확정
- 대상: Internal Alpha와 Launch MVP
- 캘린더 제공자: Apple EventKit
- 기본 공개 수준: `busyOnly`
- `hidden`: 다른 Party 멤버의 화면·API·sync에는 존재하지 않지만 공통 시간 계산과 제안/확정 충돌 검사에는 Busy로 반영
- 실제 EventKit 읽기·쓰기는 권한을 가진 지정 iOS 기기가 수행
- 서버는 계산용 일정 사실, Party별 공개 투영, 동기화 상태와 쓰기 명령 상태를 소유

현재 `src/PlanTogether/AppStore.swift:57`은 `hidden`이면 BusyInterval을 제거하므로 확정 정책과 반대다. 구현 시 기존 동작을 먼저 회귀 테스트로 고정한 뒤 정책 테스트를 추가하고 계산 입력과 화면 투영을 분리해야 한다.

## 2. 목표와 제외 범위

### 목표

1. 사용자가 명시적으로 선택한 Apple Calendar의 일정만 읽는다.
2. 일정 원본과 계산 전용 사실, Party 공개 투영을 분리한다.
3. `hidden` 일정 때문에 실제 약속이 겹치지 않게 하면서 이벤트 존재와 소유자를 직접 노출하지 않는다.
4. 권한 철회, 캘린더 삭제, 반복/종일 일정, 시간대와 동기화 실패를 명시적 상태로 처리한다.
5. 확정 일정의 생성·변경·삭제를 지정 iOS 기기에서 중복 없이 실행한다.

### 제외 범위

- Google Calendar
- 앱에서 반복 약속 생성
- 부분 가능 시간과 멤버별 availability matrix
- hidden 일정에 대한 강한 암호학적 비추론성 보장
- EventKit 원본 메모, URL, 참석자, 주최자, 캘린더 이름의 서버 업로드

## 3. 개인정보 계층

```text
EventKit 원본 (기기 전용)
       │ 선택 필드만 추출
       ▼
Private Busy Facts (서버, 소유자/계산 엔진 전용)
       ├─ details   ─▶ Party Projection: 시간 + 제목 + 선택적 장소
       ├─ busyOnly  ─▶ Party Projection: 시간 + "일정 있음"
       └─ hidden    ─▶ Party Projection 없음
                          │
Private Busy Facts ───────┴─▶ Availability Engine
                                 └─ 전원 공통 슬롯만 반환
```

### 3.1 EventKit 원본

- EventKit identifier와 calendar identifier는 기기 로컬 mapping에만 저장한다.
- notes, URL, attendees, organizer, alarms와 calendar name은 서버로 보내지 않는다.
- 사용자가 선택하지 않은 캘린더는 읽기 query 입력에서 제외한다.

### 3.2 Private Busy Facts

- 필드: owner, 시작/종료 UTC, all-day, 원래 time zone, availability class, 기기가 생성한 무작위 source event key, source revision.
- 접근: 소유자 본인과 서버의 availability/projection service만 가능.
- Party API, 일반 sync, 푸시 payload와 분석 로그에서 직접 노출하지 않는다.

### 3.3 Party Projection

| 수준 | 다른 멤버에게 보이는 값 | 계산 반영 |
|---|---|---|
| `details` | 시작/종료, 제목, 사용자가 허용한 경우 장소 | Busy |
| `busyOnly` | 시작/종료, 고정 문구 `일정 있음` | Busy |
| `hidden` | projection 자체가 없음 | Busy |

`details`에도 notes, URL, attendees, organizer, 원본 calendar name과 EventKit identifier는 포함하지 않는다. 제목·장소는 암호화 저장하고 권한 확인 이후에만 복호화한다.

## 4. Hidden 정책과 추론 위험

### 보장하는 것

- 다른 멤버는 hidden 이벤트 row, ID, 소유자, 시작/종료, 제목과 장소를 받지 않는다.
- 공통 시간 응답은 가능한 슬롯과 Party 전체 동기화 상태만 반환한다.
- “누가”, “어떤 일정 때문에”, “몇 명이 불가능한지”를 반환하지 않는다.
- 임의 시간 제안과 확정 직전 충돌 검사에도 hidden Busy를 동일하게 반영한다.

### 보장할 수 없는 것

정확한 그룹 공통 가능 시간을 제공하는 이상, 특히 2인 Party에서는 결과 변화로 hidden 일정의 존재를 간접 추론할 수 있다. Launch MVP의 `hidden`은 이벤트 비노출 정책이며 강한 비추론성 보장은 아니다. 설정 화면에서 “일정 내용과 개별 시간은 숨겨지지만 그룹 가능 시간 결과에 영향을 줄 수 있음”을 설명한다.

### 탐색 남용 완화

- availability 검색은 활성 Party 멤버만 가능하다.
- 사용자·Party 단위 요청 제한을 기본 분당 20회로 둔다.
- 한 요청의 검색 범위는 최대 31일, 최소 약속 길이는 30분으로 제한한다.
- Launch MVP는 30분 슬롯 단위의 전원 공통 결과만 제공한다.
- 개인별 가능 여부, 제외 원인과 부분 일치 인원은 Post-MVP에서도 privacy review 전에는 추가하지 않는다.

## 5. 권한·선택·연결 상태

### 5.1 온보딩

1. 권한 요청 전에 사용 목적과 서버에 저장되는 최소 시간 데이터를 설명한다.
2. 사용자가 계속을 선택하면 EventKit full access를 요청한다.
3. 허용되면 읽을 캘린더 목록을 표시하되 기본 선택은 비워 둔다.
4. 사용자가 하나 이상 명시적으로 선택해야 초기 sync를 시작한다.
5. 확정 일정 쓰기용 destination calendar를 하나 선택하거나 앱 전용 캘린더 생성을 선택한다.

### 5.2 연결 상태

```text
disconnected → permission_required → selecting → syncing → ready
                      ├─ denied
                      └─ restricted
ready → stale → syncing → ready
ready → revoked
ready → needs_source_reselection
ready → error → syncing
```

| 상태 | 의미 | 사용자 동작 |
|---|---|---|
| `disconnected` | 연결 시작 전 | 연결 시작 |
| `permission_required` | 권한 설명 완료, OS 요청 전 | 권한 요청 |
| `selecting` | 권한 있음, source/destination 미선택 | 캘린더 선택 |
| `syncing` | snapshot 작성·업로드 중 | 진행 표시, 마지막 정상 데이터 유지 |
| `ready` | snapshot이 freshness 기준 충족 | 정상 사용 |
| `stale` | 마지막 완료 sync가 6시간 초과 | 앱 열기/새로고침 요청 |
| `denied` | 사용자가 권한 거부 | 설정 이동 안내 |
| `restricted` | OS 정책으로 접근 불가 | 원인 설명, 재요청 금지 |
| `revoked` | 기존 권한 철회 | fresh 해제, 재연결 안내 |
| `needs_source_reselection` | 선택한 읽기 캘린더 삭제/접근 불가 | source 재선택 |
| `error` | 일시적 읽기·업로드 오류 | 자동/수동 재시도 |

위 표는 읽기 축(`source_status`)이다. 쓰기 대상 캘린더 상태는 `calendar_write_design.md` §4.3이 정한 별도 축 `destination_status`(`selected`/`needs_reselection`/`write_denied`)가 소유하며, §9의 freshness gate는 `source_status`만 본다. destination 상실은 쓰기 전용 실패이므로 Party 전원의 공통 시간 검색을 막지 않는다.

권한이 없거나 선택 캘린더가 없는 상태를 “일정 없음”으로 해석하지 않는다.

## 6. 읽기 규칙

### 6.1 동기화 범위

- Internal Alpha: 현재 구현과 같은 앞으로 7일을 임시 검증 범위로 사용할 수 있다.
- Launch MVP: 사용자 time zone의 오늘 00:00부터 90일 후 23:59까지 rolling window를 유지한다.
- EventKit query는 경계를 가로지르는 이벤트를 포함하고, 서버에는 UTC start/end와 원래 time zone을 함께 저장한다.
- window 밖 facts는 마지막 정상 snapshot 교체 후 정리한다.

### 6.2 이벤트 분류

| EventKit 의미 | 계산 |
|---|---|
| busy | Busy |
| unavailable | Busy |
| tentative | Busy |
| free | 제외 |
| 취소/삭제됨 | 제외 또는 기존 fact 삭제 |

- 같은 사용자의 겹치거나 인접한 Busy 구간은 계산 전에 병합한다.
- all-day 일정은 이벤트의 원래 time zone 기준 하루 전체 Busy로 변환한다.
- 반복 일정은 EventKit query가 window 안에 확장한 occurrence만 업로드하고 recurrence rule 자체는 저장하지 않는다.
- 이동 시간, 알림과 참석자 응답은 Launch MVP 계산에 사용하지 않는다.

## 7. Snapshot 동기화

### 7.1 기기 역할

- 사용자마다 활성 `calendar_sync_device_id`와 `calendar_writer_device_id`를 각각 하나 둔다. 초기에는 같은 기기를 기본값으로 사용한다.
- 서버가 발급한 device ID와 활성 세션이 일치하는 기기만 snapshot을 업로드하거나 write command를 claim할 수 있다.
- 사용자가 기기를 전환하면 새 기기의 full snapshot 완료 전까지 연결 상태를 `syncing`으로 유지한다.

### 7.2 업로드 계약

| Method | Path | 목적 |
|---|---|---|
| GET | `/v1/calendar/connection` | 권한·선택·freshness·기기 상태 조회 |
| PUT | `/v1/calendar/connection` | 선택 source/destination과 기기 역할 갱신 |
| POST | `/v1/calendar/snapshots` | snapshot session 생성 |
| PUT | `/v1/calendar/snapshots/{id}/pages/{n}` | allowlist fact page 업로드 |
| POST | `/v1/calendar/snapshots/{id}/complete` | 검증 후 window 원자적 교체 |
| POST | `/v1/calendar/snapshots/{id}/abort` | 미완료 staging 폐기 |

- snapshot은 connection, window, revision과 page count를 가진다.
- page에는 EventKit identifier 없이 무작위 `source_event_key`와 allowlist 필드만 포함한다.
- 앱이 만든 확정 일정에는 allowlist 필드로 `app_confirmed_event_id`를 함께 올린다. `calendar_write_design.md` §6.6.1의 재일정 자기 충돌 제외가 이 값을 쓴다.
- 기기는 **앱 자신의 scheme과 일치하는 marker만** 로컬에서 파싱해 confirmed event ID만 올린다. 그 외 모든 이벤트는 `app_confirmed_event_id = NULL`로 올리며 URL 원문은 어떤 경우에도 업로드하지 않는다. §3.1의 URL 업로드 금지는 그대로 유지된다.
- `app_confirmed_event_id`는 private busy fact 전용이다. Party projection, sync payload, 로그, 지표에 포함하지 않는다. 이 값은 그 사용자의 외부 캘린더 반영 여부를 드러내므로 `calendar_write_design.md` §1의 본인 전용 원칙 대상이다.
- complete transaction에서 기존 active generation을 새 generation으로 교체하고 Party projection, sync change를 함께 갱신한다.
- 실패·중단된 snapshot은 기존 ready generation에 영향을 주지 않는다.
- 같은 revision의 complete 재호출은 동일 결과를 반환한다.

### 7.3 변경 감지

- 앱 활성화 시 권한과 선택 캘린더 유효성을 확인하고 sync한다.
- EventKit 변경 알림을 받으면 여러 이벤트를 debounce하여 새 snapshot을 만든다.
- background task와 silent APNs는 best-effort 가속 수단이며 delivery를 보장한다고 가정하지 않는다.
- 마지막 정상 sync 이후 앱이 열리지 않으면 6시간 뒤 `stale`로 표시한다.

## 8. Party 공개 설정 적용

### 설정 변경

| 변경 | 서버 동작 |
|---|---|
| details → busyOnly | 제목·장소 ciphertext 삭제, Busy projection 재생성 |
| details/busyOnly → hidden | Party projection 삭제+tombstone, private facts는 계산용 유지 |
| hidden → busyOnly | private facts로 Busy projection 생성 |
| hidden/busyOnly → details | 기기가 상세 allowlist를 업로드한 facts만 details, 나머지는 busyOnly로 안전하게 강등 |

- 설정 변경과 projection 갱신/tombstone 생성은 같은 PostgreSQL transaction에서 처리한다.
- 다른 멤버의 sync에는 변경 후 허용되는 projection만 전달한다.
- push에는 `party_changed` 같은 opaque type만 담고 일정 정보는 포함하지 않는다.

## 9. 가능 시간과 Freshness Gate

### 검색

- 모든 활성 멤버의 calendar connection이 `ready`이고 마지막 정상 sync가 6시간 이내일 때만 “모두 가능” 슬롯을 확정 결과로 반환한다.
- 한 명이라도 `source_status`가 stale/denied/revoked/error/needs_source_reselection이면 `calendar_sync_pending`을 반환한다. `destination_status`는 이 판정에 쓰지 않는다.
- 응답은 문제가 있는 멤버 수나 ID를 일반 멤버에게 제공하지 않는다. 본인 문제는 본인에게만 상세 CTA로 제공한다.

### 제안·확정

- 임의 시간 제안도 private Busy facts로 충돌을 검사하고 충돌 원인은 공개하지 않는다.
- 확정 직전 모든 참여자의 sync가 15분 이내인지 다시 확인한다.
- 15분을 넘긴 사용자가 있으면 서버가 opaque refresh request와 APNs nudge를 만들고 proposal을 `awaiting_calendar_refresh`로 둔다.
- 최대 2분 동안 새 snapshot을 기다리며, 갱신되지 않으면 자동 확정하지 않고 aggregate 재시도 안내를 표시한다.
- fresh snapshot에서 충돌이 생기면 `conflict_detected`로 전환하고 시간 재제안을 요구한다. 누구의 어떤 일정과 충돌했는지는 공개하지 않는다.

## 10. EventKit 쓰기 명령

### 10.1 명령 상태

```text
pending_device → claimed → succeeded
                    ├─ retryable_failed → pending_device
                    ├─ user_action_required
                    └─ cancelled
```

- 서버는 확정/변경/취소 transaction 안에서 사용자별 `calendar_write_command`를 생성한다.
- 지정 writer device만 command를 lease하고 EventKit create/update/delete를 수행한다.
- background push는 명령 존재만 알린다. 앱 활성화와 일반 sync에서도 미완료 명령을 가져온다.

### 10.2 중복 방지

- 앱이 생성한 이벤트에는 서버의 `confirmed_event_id`를 포함한 PlanTogether URL marker를 넣는다.
- 로컬 SwiftData에는 command ID, confirmed event ID, destination calendar ID와 EventKit identifier mapping을 저장한다.
- create 전 로컬 mapping과 target window의 URL marker를 확인한다.
- create 성공 후 서버 result 보고가 실패하면 같은 기기가 기존 event를 찾아 동일 결과를 재보고한다.
- update에서 기존 event를 찾지 못하면 새로 생성하지 않고 `user_action_required`로 보고한다.
- delete에서 event가 이미 없으면 idempotent success로 보고한다.
- 다른 기기로 writer를 전환할 때는 새 기기의 EventKit sync 확인과 marker scan 후 미완료 command를 재할당한다.

## 11. 데이터 모델

| 테이블 | 핵심 필드·제약 |
|---|---|
| `calendar_connections` | user, sync/writer device, permission/status, active generation, last completed sync, time zone, version |
| `calendar_source_selections` | connection, device-local opaque source key, enabled; calendar 이름 저장 금지 |
| `calendar_snapshot_sessions` | connection, window, revision, expected pages, status; active revision unique |
| `calendar_snapshot_facts_staging` | session, source event key, interval, all-day, time zone, availability, optional encrypted details |
| `calendar_busy_facts` | connection, generation, source event key, interval, availability; active key unique |
| `party_schedule_projections` | party, owner, source event key, visibility, interval, optional encrypted title/location |
| `calendar_write_commands` | confirmed event, user, writer device, operation, revision, status, lease, result locator; active command unique |

### Index와 보존

- Busy 범위 조회: `(user_id, start_at, end_at)`.
- Party 표시 조회: `(party_id, start_at, end_at)`.
- snapshot cleanup: `(connection_id, generation)`.
- 활성 write command: `(executor_device_id, status, created_at)`.
- 완료/중단 snapshot staging은 24시간 뒤 삭제한다.
- rolling window 밖 facts와 projection은 정상 snapshot 완료 후 삭제한다.
- 계정 삭제 시 facts, projection, connection과 command locator를 삭제한다.

## 12. 오류와 복구

| 상황 | 상태/복구 |
|---|---|
| 권한 거부 | `denied`, OS 설정 안내; 일정 없음으로 취급 금지 |
| 권한 철회 | `revoked`, freshness 즉시 무효, 기존 projection 비노출 전환 |
| 선택 source 삭제 | `needs_reselection`, 재선택 전 definitive availability 차단 |
| snapshot 일부 업로드 실패 | staging 유지 후 page 재시도, 기존 generation 유지 |
| snapshot 검증 실패 | abort 후 전체 snapshot 재작성 |
| 기기 교체/재설치 | 새 server device 등록, full snapshot, writer 전환 |
| writer device 미접속 | command pending, 사용자에게 앱 열기/기기 전환 안내 |
| destination calendar 삭제 | `user_action_required`, 재선택 후 새 revision command |
| EventKit 저장 실패 | retryable 여부 분류, backoff 또는 사용자 조치 |

권한 철회 시 private facts를 즉시 삭제하지는 않고 다른 멤버에게 보이는 projection을 즉시 비노출 처리한다. 사용자가 7일 안에 재연결하지 않으면 private facts와 projection을 삭제한다. 삭제 전에도 freshness gate 때문에 계산 결과로 사용하지 않는다.

## 13. 관측성과 개인정보

### 허용 지표

- snapshot duration, fact count, byte count, failure code
- connection status count와 stale duration
- availability request count/latency/result slot count
- write command claim/completion latency, retry/dead count
- projection rebuild/tombstone count

### 금지 데이터

- EventKit/calendar identifier
- Party projection·sync·로그·지표에 실린 `app_confirmed_event_id`
- title, location, notes, URL
- 참석자, 주최자, calendar name
- source event key 원문
- 특정 사용자의 hidden 시간 구간

로그는 request ID, 내부 actor ID, 상태 코드와 수량만 기록한다. APNs payload에는 entity ID 대신 opaque sync-needed marker를 우선 사용한다.

## 14. 인수 조건

### 권한·선택

- 권한 허용 전 EventKit query를 수행하지 않는다.
- source calendar를 하나도 선택하지 않으면 connection을 `ready`로 만들지 않는다.
- denied/restricted/revoked 상태를 일정 없음으로 처리하지 않는다.

### 공개·계산

- hidden 사용자의 Busy 시간은 공통 가능 슬롯에서 제외된다.
- 다른 멤버의 모든 API/sync/bootstrap 응답에는 hidden event ID, owner, 시간, 제목, 장소와 source key가 없다.
- details/busyOnly에서 hidden으로 변경하면 기존 projection이 같은 transaction에서 tombstone 처리되고 다음 sync 후 다른 기기 캐시에 남지 않는다.
- Busy Only에는 시간과 고정 문구 외 캘린더 정보가 없다.
- details에는 title과 명시적으로 허용된 location 외 민감 필드가 없다.
- availability 응답에는 개인별 불가능 여부와 원인이 없다.

### 읽기·동기화

- 반복 일정은 90일 window의 occurrence로 반영되고 recurrence rule은 서버에 저장되지 않는다.
- all-day와 DST 경계 테스트에서 UTC interval과 사용자 표시 날짜가 일치한다.
- tentative는 Busy, free는 제외된다.
- snapshot complete 실패 시 기존 ready generation과 계산 결과가 유지된다.
- 같은 revision complete 재전송은 facts/projection을 중복 생성하지 않는다.
- EventKit identifier가 request, DB, log와 trace에 나타나지 않는다.

### Freshness·쓰기

- 멤버 중 하나라도 6시간 초과 stale이면 definitive availability를 반환하지 않는다.
- 확정 직전 15분 freshness를 충족하지 못하면 자동 확정하지 않는다.
- writer가 아닌 기기는 calendar command를 claim할 수 없다.
- 동일 create command 재시도 시 EventKit event가 하나만 존재한다.
- update 대상이 없으면 임의 create하지 않고 사용자 조치 상태가 된다.
- 이미 삭제된 event의 delete 재시도는 성공으로 처리된다.

## 15. 검증 계획

### iOS 단위·통합

- EventKit authorization 상태 매핑
- selected source filtering
- busy/unavailable/tentative/free 분류
- 반복 occurrence, all-day, DST/time zone 변환
- EventKit identifier→local source key mapping과 snapshot serialization allowlist
- write command claim, URL marker dedupe, update-missing/delete-missing 처리

### 서버 단위·PostgreSQL 통합

- snapshot staging/complete/abort와 generation atomic swap
- visibility별 projection 생성·삭제·tombstone
- hidden fact 계산 반영과 API 비노출
- freshness gate와 confirm refresh state
- command device 권한, lease, revision unique constraint
- 권한 철회 후 projection 비노출과 7일 cleanup

### E2E

- 두 사용자, details/busyOnly/hidden 각각의 화면과 공통 시간 비교
- hidden 일정 시간에 임의 제안 후 충돌은 차단되지만 원인은 비노출
- 반복/종일 일정 snapshot 후 다른 time zone 사용자와 계산
- 권한 철회 → stale/비노출 → 재연결 또는 7일 삭제
- 확정 → 지정 기기 create → result 유실 → 재시도 시 중복 없음
- destination calendar 삭제 → 재선택 → command 새 revision 성공

### Privacy 회귀

- JSON schema와 snapshot fixture allowlist 검사
- log/trace/APNs payload 민감 키 스캔
- 비멤버, 탈퇴 멤버와 다른 Party의 projection 접근 거부
- 반복 availability probe rate limit 검증

## 16. 구현 순서

1. 현재 `CalendarServing`을 권한, source 선택, snapshot reader, command writer 경계로 분리한다.
2. `hidden`을 Busy 계산에서 제거하는 현재 동작에 실패하는 회귀 테스트를 먼저 추가하고 private facts/visible projections를 분리한다.
3. iOS 로컬 EventKit mapping과 snapshot serializer allowlist를 구현한다.
4. 서버 calendar connection/snapshot staging/generation migration과 API를 구현한다.
5. projection service와 availability privacy/freshness gate를 구현한다.
6. EventKit change debounce, foreground sync와 best-effort background sync를 연결한다.
7. device-executed write command와 URL marker 중복 방지를 구현한다.
8. 권한 철회·캘린더 삭제·기기 전환·cleanup을 구현한다.
9. 단위, PostgreSQL 통합, E2E와 privacy 회귀를 모두 통과시킨다.

## 17. 현재 구현과의 차이

- `CalendarService.swift.swift`는 존재하지 않으며 실제 파일은 `src/PlanTogether/CalendarService.swift`다.
- 현재 `CalendarServing`은 권한 요청과 모든 캘린더 읽기만 지원하며 source 선택·상태·쓰기가 없다.
- 현재 `busyIntervals`는 매번 임의 UUID를 만들어 안정적인 snapshot mapping이 없다.
- 현재 `AppStore.connectCalendar()`는 7일만 읽고 Party마다 데이터를 복제한다.
- 현재 hidden은 BusyInterval 자체를 제거해 확정 정책과 반대다.
- 현재 `AvailabilityCalculator`는 모든 멤버가 synced인지 확인하는 불변식이 있으므로 freshness 상태를 추가하는 방향으로 확장한다.

## 18. 공식 참고 문서

- [Accessing the event store](https://developer.apple.com/documentation/eventkit/accessing-the-event-store)
- [Requesting access to event store](https://developer.apple.com/documentation/eventkit/ekeventstore/requestfullaccesstoevents(completion:))
- [Retrieving events and reminders](https://developer.apple.com/documentation/eventkit/retrieving-events-and-reminders)
- [EKEventStoreChanged](https://developer.apple.com/documentation/eventkit/ekeventstorechanged)
- [Pushing background updates to your app](https://developer.apple.com/documentation/usernotifications/pushing-background-updates-to-your-app)

