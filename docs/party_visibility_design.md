# Party별 공개 수준과 계산 반영 규칙 상세 설계

## 0. 설계 상태

- 상태: 확정
- 대상: Internal Alpha와 Launch MVP
- 순서: `docs/feature_design_backlog.md`의 상세 설계 진행 순서 5번
- 선행 설계: `docs/account_backend_design.md`, `docs/calendar_privacy_sync_design.md`, `docs/party_membership_design.md`
- 후속 설계: 6(가능 시간 검색), 7(제안·응답·확정), 8(캘린더 쓰기), 9(알림)

이 설계는 한 사용자가 Party마다 선택하는 공개 수준의 의미, 필드 마스킹, 계산 입력, API·sync 계약,
동시 변경과 실패 복구, 개인정보 관측 기준을 확정한다. 선행 설계에서 이미 정한 EventKit 원본 비업로드,
private Busy fact, Party projection, freshness gate는 재정의하지 않고 실행 가능한 계약으로 구체화한다.

### 이번 설계에서 확정한 제품 정책

| 항목 | 결정 | 근거 |
|---|---|---|
| 공개 단위 | 사용자·Party 단위 1개 설정 | 일정마다 설정하면 사용자가 예측하기 어렵고 변경·삭제 누락 위험이 커진다 |
| 기본값 | Party 생성·가입·재가입 때마다 `busyOnly` | 일정 내용 노출 없이 공통 시간과 Party 화면을 바로 사용할 수 있다 |
| `details` 장소 | 별도 `share_location` 동의, 기본 `false` | 제목 공유 동의가 장소 공유 동의를 자동으로 포함하지 않게 한다 |
| `hidden` 의미 | Party projection은 0건, private Busy fact는 계산에 계속 반영 | 일정 존재를 직접 노출하지 않으면서 겹치는 약속 확정을 막는다 |
| 설정값 가시성 | 본인만 조회·변경 | `hidden` 선택 사실 자체가 일정 존재 추론의 단서가 되는 것을 막는다 |
| 결과 공개 | 전원 공통 슬롯만 반환하고 인원·원인·소유자를 반환하지 않음 | 반복 탐색으로 개인 일정을 추론하는 표면을 줄인다 |

---

## 1. 목표와 비목표

### 목표

1. 사용자가 Party마다 자신의 일정 공개 범위를 이해하고 안전하게 바꿀 수 있다.
2. 화면에 보이는 일정과 공통 시간 계산 입력을 분리해 `hidden`도 충돌 계산에는 반영한다.
3. 공개 수준 하향, Party 탈퇴와 권한 철회 후 다른 멤버의 기기에 상세 데이터가 남지 않는다.
4. 모든 API·sync·로그·분석·푸시 경로가 같은 필드 허용 목록을 따른다.
5. snapshot 완료와 공개 수준 변경이 경쟁해도 더 넓은 공개가 되살아나지 않는다.

### 비목표

- 일정별·캘린더별·멤버별 공개 수준
- `details`에서 제목만 골라 숨기는 일정별 예외
- 부분 가능 인원, 개인별 가능 여부와 충돌 원인 표시
- `hidden` 일정에 대한 강한 암호학적 비추론성 보장
- notes, URL, 참석자, 주최자, 원본 캘린더 이름 공유
- Google Calendar와 Premium 정책

---

## 2. 개념과 불변식

```text
EventKit 원본(기기 전용)
        │ allowlist 추출
        ▼
calendar_busy_facts (소유자 + 계산 엔진 전용)
        ├─ 모든 수준 ───────────────▶ Availability/Conflict Engine
        │                              └─ 전원 공통 결과만 반환
        └─ 사용자·Party 설정 적용
             ├─ details  ───────────▶ Party projection(시간+제목+선택적 장소)
             ├─ busyOnly ───────────▶ Party projection(시간+고정 문구)
             └─ hidden   ───────────▶ projection 없음
```

항상 유지해야 하는 불변식은 다음과 같다.

1. 활성 멤버십마다 본인 소유의 `party_visibility_settings`가 정확히 1건 있다.
2. 설정 row가 없거나 해석할 수 없는 값이면 읽기·쓰기 모두 `busyOnly`보다 넓게 공개하지 않는다.
3. `hidden` 사용자의 해당 Party projection은 0건이다.
4. 공개 수준과 관계없이 `ready` generation의 private Busy fact는 동일하게 계산된다.
5. `busyOnly` projection에는 제목·장소 ciphertext 자체가 존재하지 않는다.
6. `details` projection에도 notes, URL, 참석자, 주최자, 캘린더 이름과 EventKit identifier가 없다.
7. 다른 멤버는 공개 설정값과 공개 수준 변경 이력을 조회할 수 없다.
8. 설정 하향과 기존 projection tombstone은 같은 transaction에서 커밋된다.
9. stale generation이나 stale setting version으로 생성된 projection은 활성 결과가 될 수 없다.

---

## 3. 공개 수준의 정확한 의미

| 수준 | Party 일정 화면 | 계산·충돌 검사 | 서버에 허용되는 Party projection 필드 |
|---|---|---|---|
| `details` | 시작·종료, 제목, 동의 시 장소 | Busy | projection ID, owner 표시용 ID, 시간, 암호화 제목, 선택적 암호화 장소 |
| `busyOnly` | 시작·종료, `일정 있음` | Busy | projection ID, owner 표시용 ID, 시간 |
| `hidden` | 행 자체가 없음 | Busy | 없음 |

### 3.1 `details`

- 제목은 Party에 공유한다. 제목이 비어 있으면 원문을 만들지 않고 고정 문구 `제목 없는 일정`을 표시한다.
- 장소는 `share_location=true`일 때만 공유한다. 기본값은 `false`다.
- `share_location=false`로 바꾸면 모든 장소 ciphertext를 같은 transaction에서 삭제하고 tombstone 대신
  projection upsert를 발행한다. 시간과 제목은 유지되기 때문이다.
- 상세 필드는 저장 시 envelope encryption을 사용하고, 활성 멤버 권한을 확인한 응답 경로에서만 복호화한다.
- private Busy facts에는 제목·장소를 저장하지 않는다. 서버가 현재 snapshot session의 상세 allowlist를 아직 받지
  못한 이벤트는 `busyOnly`로 안전하게 강등한다.

### 3.2 `busyOnly`

- 다른 멤버에게 시간 구간과 소유자 표시만 제공한다.
- 제목은 서버나 클라이언트가 원문을 변형해 만들지 않고 로컬 고정 문구 `일정 있음`을 사용한다.
- 제목·장소 ciphertext, 원본 제목 존재 여부, 원본 장소 존재 여부를 응답과 sync에 포함하지 않는다.

### 3.3 `hidden`

- Party 화면/API/sync/bootstrap에는 projection, projection ID, 소유자, 시간 구간이 모두 없다.
- private Busy fact는 삭제하지 않으며 가능 시간 검색, 임의 시간 제안과 확정 직전 충돌 검사에 반영한다.
- 일반 멤버에게 hidden 이벤트 개수, hidden을 선택한 멤버 수와 refresh 대상자를 반환하지 않는다.
- 정확한 전원 공통 결과 때문에 특히 2인 Party에서는 결과 변화로 일정 존재를 간접 추론할 수 있다.
  설정 화면에 이 한계를 공개하고 강한 비추론성을 약속하지 않는다.

---

## 4. 사용자 흐름과 화면 계약

### 4.1 설정 화면

- Party 설정의 `내 일정 공개` 화면에서 본인의 현재 값만 보여준다.
- 멤버 목록과 방장 관리 화면에는 다른 멤버의 공개 수준, 캘린더 연결 상태와 변경 시각을 표시하지 않는다.
- 세 수준을 다음 문구로 설명한다.
  - 상세 공개: `시간과 제목을 공유합니다. 장소는 별도로 선택할 수 있습니다.`
  - 시간만 공개: `시간만 일정 있음으로 표시합니다.`
  - 숨김: `일정은 보이지 않지만 모두 가능한 시간과 충돌 검사에는 반영됩니다.`
- `details`를 선택했을 때만 `장소도 공유` 토글을 표시하며 기본은 꺼짐이다.
- `hidden` 선택 전에는 그룹 결과 변화로 일정 존재가 간접 추론될 수 있다는 설명을 한 번 명시한다.

### 4.2 변경 결과

- 저장 성공 즉시 본인 설정 UI를 갱신한다.
- `details` 상향 또는 `share_location` 활성화에는 새 상세 snapshot이 필요하므로 설정은 저장하되
  `202 pending_details_refresh`를 반환한다. 기존 사실은 `busyOnly` projection으로만 제공하고, 지정 sync 기기가
  상세 allowlist를 담은 snapshot을 완료하면 자동으로 details로 승격한다.
- `busyOnly` 또는 `hidden`으로 하향하는 변경은 기기 접속을 기다리지 않고 서버에서 즉시 적용한다.
- 오프라인에서 공개 수준 mutation을 새로 큐에 넣지 않는다. 개인정보 변경은 서버 적용 여부를 즉시 확인해야 한다.

---

## 5. 입력값과 검증 규칙

| 필드 | 규칙 |
|---|---|
| `visibility_level` | `details`, `busyOnly`, `hidden` 중 하나 |
| `share_location` | boolean. `details`가 아니면 반드시 `false` |
| `expected_version` | 현재 본인 설정 version과 일치해야 함 |
| `Idempotency-Key` | PATCH마다 필수. 같은 key와 같은 body는 원래 결과 재사용 |

- 알 수 없는 수준이나 `busyOnly/hidden + share_location=true`는 `400 invalid_request`다.
- 활성 멤버십이 없으면 비멤버에게 Party 존재를 알리지 않도록 `404 not_found`다.
- 과거 멤버십이 있고 Party가 해산된 경우에만 `410 party_disbanded`를 사용할 수 있다.
- version 불일치는 `409 version_conflict`와 본인이 읽을 수 있는 최신 설정 version만 반환한다.
- 현재 값과 동일한 mutation도 멱등 성공으로 처리하며 불필요한 projection rebuild와 sync change를 만들지 않는다.
- 사용자·Party당 변경은 분당 10회로 제한한다. 초과 시 `429 rate_limited`이며 현재 설정은 바뀌지 않는다.

---

## 6. 데이터 모델

### 6.1 `party_visibility_settings`

| 필드 | 계약 |
|---|---|
| `party_id`, `user_id` | 복합 unique, 활성 멤버십과 연결 |
| `visibility_level` | `details`, `busyOnly`, `hidden` check constraint |
| `share_location` | 기본 `false`; details가 아니면 false인 check constraint |
| `version` | mutation마다 증가 |
| `updated_at` | 서버 시각 |

- Party 생성·가입·재가입 transaction이 `busyOnly`, `share_location=false`, `version=1`로 생성한다.
- 탈퇴·강퇴·해산 transaction이 row를 삭제하고 본인 기기에 `party_visibility_setting` tombstone을 발행한다.
- 설정값과 tombstone 수신자는 해당 사용자 본인뿐이다.

### 6.2 `party_schedule_projections`

| 필드 | 계약 |
|---|---|
| `id` | Party 범위 무작위 projection UUID. API/sync에서 사용하는 유일한 일정 식별자 |
| `party_id`, `owner_user_id` | 활성 Party·멤버 확인용 |
| `source_event_key` | 내부 join 전용. API, sync, 로그에 출력 금지 |
| `source_generation`, `setting_version` | stale rebuild 차단 |
| `visibility_level` | `details` 또는 `busyOnly`만 허용 |
| `start_at`, `end_at`, `all_day`, `time_zone` | 표시 허용 시간 필드 |
| `title_ciphertext`, `location_ciphertext` | details일 때만 선택적으로 존재 |
| `version`, `updated_at` | sync 충돌 해결 |

- unique key는 `(party_id, owner_user_id, source_event_key)`다.
- `hidden` 값을 저장하지 못하도록 check constraint를 둔다.
- `busyOnly` row에 상세 ciphertext가 있거나 `details + share_location=false`인데 location ciphertext가 있으면
  DB 또는 service validation이 commit을 거부한다.
- source event key 원문은 HMAC 기반 내부 lookup 값으로 취급하고 외부 식별자로 재사용하지 않는다.

---

## 7. Projection 생성과 변경 transaction

### 7.1 공통 규칙

1. actor의 활성 멤버십과 본인 설정 row를 확인한다.
2. `expected_version`을 검사한다.
3. 설정 version을 증가시키고 새 값을 기록한다.
4. 현재 active generation의 private Busy facts로 시간 projection diff를 만든다. 상세 필드는 snapshot staging에
   현재 요청에서 들어온 allowlist가 있을 때만 사용한다.
5. 허용되는 upsert와 삭제를 적용한다.
6. 공개 수준 변경으로 생긴 projection tombstone과 upsert를 `sync_changes`에 현재 활성 멤버 수신자별로 기록한다.
7. 본인에게만 `party_visibility_setting` upsert를 기록한다.
8. 상태 변경과 sync change를 한 transaction에서 commit한다.

### 7.2 전이별 동작

| 전이 | 동작 |
|---|---|
| `details → busyOnly` | 제목·장소 ciphertext 삭제, 같은 projection ID로 busyOnly upsert |
| `details → details`, 장소 off | 장소 ciphertext 삭제, 같은 projection ID로 details upsert |
| `details/busyOnly → hidden` | 해당 사용자의 Party projection 전부 삭제, 각 ID tombstone |
| `hidden → busyOnly` | private facts에서 busyOnly projection 생성 |
| `hidden/busyOnly → details` | 우선 busyOnly로 생성하고 `pending_details_refresh`; 새 snapshot의 상세 allowlist가 있는 항목만 details로 승격 |

공개 수준 상향은 시간 projection 생성이 일부 실패하면 설정까지 rollback한다. `pending_details_refresh`는 실패가 아니라
명시적인 안전 강등 상태이므로 설정을 `details`로 저장하고 현재 private fact를 busyOnly로 투영한다. 제목·장소는
`calendar_busy_facts`로 옮기지 않고 snapshot complete transaction에서 허용된 Party projection으로만 복사한 뒤 staging
보존 기간이 지나면 삭제한다.

### 7.3 Snapshot과의 경쟁

- snapshot 완료는 commit 시점에 활성 멤버십과 최신 설정 version을 다시 읽고, 해당 snapshot staging의 상세
  allowlist를 `details` Party projection에만 복사한다.
- `app_confirmed_event_id`는 이 복사 대상에서 제외한다. private busy fact 전용 필드이며 Party projection에
  실리면 다른 멤버가 특정 멤버의 확정 일정 캘린더 반영 여부를 직접 보게 되어 `calendar_write_design.md` §1의
  본인 전용 원칙이 깨진다.
- rebuild 결과는 `source_generation`과 `setting_version`이 모두 현재 값과 일치할 때만 활성 upsert가 된다.
- stale worker가 늦게 도착하면 아무 변경 없이 종료하고 최신 generation rebuild를 다시 enqueue한다.
- `hidden` 또는 탈퇴가 commit된 뒤 stale snapshot이 projection을 되살리는 것을 허용하지 않는다.
- 동일 projection diff의 재실행은 같은 projection ID와 version 결과를 유지해 중복 sync row를 만들지 않는다.

---

## 8. API와 권한

`account_backend_design.md` §6의 `/v1`, 인증, request ID, version과 멱등성 규칙을 따른다.

| Method | Path | 권한 | 응답 |
|---|---|---|---|
| GET | `/v1/parties/{party_id}/visibility/me` | 본인 활성 멤버 | 본인 수준, `share_location`, version, details refresh 상태 |
| PATCH | `/v1/parties/{party_id}/visibility/me` | 본인 활성 멤버 | 적용 결과 또는 pending 상태 |
| GET | `/v1/parties/{party_id}/schedule` | 활성 멤버 | 요청 범위의 허용 projection 목록과 projection version |

### 8.1 응답 허용 목록

`GET .../visibility/me`:

```json
{
  "visibilityLevel": "busyOnly",
  "shareLocation": false,
  "version": 3,
  "detailsRefreshState": "not_required"
}
```

`GET .../schedule`의 item:

```json
{
  "id": "projection-uuid",
  "ownerUserId": "member-user-uuid",
  "startAt": "2026-09-10T04:00:00Z",
  "endAt": "2026-09-10T05:00:00Z",
  "allDay": false,
  "timeZone": "Asia/Seoul",
  "displayKind": "busyOnly",
  "title": null,
  "location": null,
  "version": 7
}
```

- `displayKind`는 실제 projection row의 `details`/`busyOnly`이며 사용자의 설정값 전체를 뜻하지 않는다.
- schedule 응답에는 `source_event_key`, source generation, 설정 version, 캘린더 연결 상태를 포함하지 않는다.
- 범위는 최대 31일이며 Party 활성 멤버만 조회한다.
- 다른 멤버가 `/visibility/{user_id}` 형태로 접근할 endpoint는 만들지 않는다.

---

## 9. Sync와 로컬 캐시

- `party_visibility_setting` upsert/delete는 소유자 본인에게만 전달한다.
- 공개 수준 변경 중 생긴 `party_schedule_projection` upsert/delete는 현재 활성 멤버에게 전달한다.
- 탈퇴·강퇴·해산으로 생긴 projection delete는 `party_membership_design.md` §5.5의 전이 직전 수신자 집합을
  사용한다. 떠난 사용자와 해산 직전 멤버도 자신의 로컬 Party 캐시를 지울 수 있도록 반드시 포함한다.
- 신규 멤버 bootstrap에는 현재 허용되는 projection만 포함하며 다른 멤버의 설정 row는 포함하지 않는다.
- visibility 하향 transaction이 생성한 tombstone은 upsert보다 먼저 적용할 수 있도록 entity version을 포함한다.
- 클라이언트는 더 낮은 version 변경을 무시하고, delete tombstone version 이상인 로컬 상세를 즉시 제거한다.
- cursor 만료로 전체 bootstrap을 하면 해당 Party의 projection 로컬 저장소를 먼저 scope 교체한다. merge만 해서
  서버에 없는 hidden projection을 남기지 않는다.
- 로그아웃, 멤버십 종료와 Party 해산 때 해당 Party projection 캐시를 삭제한다.

---

## 10. 계산 반영과 결과 보호

### 10.1 계산 입력

- availability와 proposal conflict engine은 `party_schedule_projections`가 아니라 모든 활성 멤버의
  `calendar_busy_facts`를 읽는다.
- 계산 전 같은 사용자의 겹치거나 인접한 Busy interval을 병합한다.
- visibility level, projection 유무와 details refresh 상태는 Busy 여부를 바꾸지 않는다.
- 한 명이라도 freshness gate를 만족하지 않으면 definitive 결과를 반환하지 않는다.

### 10.2 응답 제한

- 결과는 전원 공통인 `startAt`, `endAt` 슬롯과 Party/membership 계산 version만 포함한다.
- `availableMemberCount`, `totalMemberCount`, unavailable member ID, 충돌 event ID와 사유를 포함하지 않는다.
- 임의 시간 검증도 `available` 또는 aggregate `conflict_detected`만 반환한다.
- 일반 멤버에게 freshness 문제 사용자의 수와 ID를 반환하지 않는다. 본인 문제만 별도 self 상태에서 설명한다.

### 10.3 탐색 남용 완화

- 활성 멤버만 요청할 수 있다.
- 사용자·Party 단위 기본 분당 20회, 검색 범위 최대 31일, 최소 약속 길이 30분을 적용한다.
- 결과 슬롯은 Launch MVP에서 30분 경계로 양자화한다.
- rate limit·범위 제한 우회 시도와 반복 conflict probe는 보안 지표로 집계하되 요청 시간 구간을 로그에 남기지 않는다.

검색 시간대, 활동 시간, 최대 결과 수와 정렬 우선순위는 설계 6이 확정한다.

---

## 11. 실패와 복구

| 상황 | 동작 |
|---|---|
| 설정 변경 중 projection diff 실패 | 전체 transaction rollback, 이전 설정·projection 유지 |
| details 상향·장소 활성화 | `202 pending_details_refresh`, busyOnly 안전 강등, 기기 refresh 요청 |
| sync 기기 장기 미접속 | 안전 강등 유지, 앱 열기 안내; details라고 허위 표시하지 않음 |
| 권한 철회 | freshness 무효화, 모든 Party projection 즉시 비노출, 7일 후 private facts 삭제 |
| 멤버십 종료와 설정 변경 경쟁 | 멤버십 종료가 이기며 설정 API는 `404`; projection은 종료 transaction이 삭제 |
| stale snapshot/rebuild 도착 | generation·setting version 불일치로 폐기 후 최신 rebuild 예약 |
| cursor 만료 | Party projection scope를 bootstrap snapshot으로 원자 교체 |
| 복호화 실패 | 해당 field를 반환하지 않고 projection을 busyOnly로 강등, 운영 경보와 재생성 job |

개인정보 하향 변경은 비동기 worker 성공에 의존하지 않는다. 요청 transaction 자체가 상세 ciphertext 삭제 또는 projection 삭제와
tombstone 기록까지 완료해야 성공을 반환한다.

---

## 12. 관측성·감사·민감정보 제외

### 허용 지표

- 수준별 설정 변경 횟수(최소 집계 크기 20 이상, 24시간 bucket)
- projection upsert/delete/tombstone 수와 처리 지연
- details 안전 강등 수와 refresh 완료 지연
- version conflict, rate limit, rebuild retry와 복호화 실패 수
- availability 요청 수, 지연과 반환 슬롯 수

### 금지 데이터

- Party ID와 사용자 ID를 공개 수준 값 또는 시간 구간과 결합한 분석 이벤트
- hidden을 선택한 특정 사용자, Party와 변경 시각
- 일정 시작·종료, 제목, 장소, 원본 제목·장소 존재 여부
- source event key, EventKit/calendar identifier, projection 복호화 결과
- availability 검색 범위와 conflict probe 대상 시간

감사 로그에는 actor 내부 ID, `visibility_setting_changed` action, 이전/이후 version, 성공/실패 코드와 request ID만 남긴다.
이전/이후 공개 수준 값과 `share_location` 값은 기록하지 않는다. APNs payload는 opaque `sync_needed`만 담는다.

---

## 13. 인수 조건

### 설정과 권한

- [ ] Party 생성·가입·재가입 시 본인 설정이 `busyOnly`, `share_location=false`로 시작한다.
- [ ] 본인은 자신의 설정만 조회·변경할 수 있고 방장도 다른 멤버의 설정을 볼 수 없다.
- [ ] 비멤버는 Party 존재를 구분할 수 없으며 설정 endpoint에서 `404`를 받는다.
- [ ] stale `expected_version` mutation은 기존 설정과 projection을 바꾸지 않는다.
- [ ] 동일 idempotency key 재전송은 projection과 sync change를 중복 생성하지 않는다.

### 필드 마스킹

- [ ] busyOnly API/sync/DB projection에 제목·장소 ciphertext와 원본 존재 여부가 없다.
- [ ] details에는 제목과 명시적으로 허용된 장소 외 민감 필드가 없다.
- [ ] `share_location=false` 전환 transaction 뒤 DB, API, sync와 로컬 캐시에 장소가 없다.
- [ ] hidden API/sync/bootstrap에는 projection ID, owner, 시간과 event 개수가 없다.
- [ ] source event key와 EventKit identifier가 API, sync, 로그, trace와 APNs에 나타나지 않는다.

### 계산과 경쟁

- [ ] 같은 Busy facts에서 세 공개 수준 모두 동일한 공통 가능 시간 결과를 만든다.
- [ ] hidden 일정과 겹치는 임의 제안은 거부되지만 사용자·일정·원인은 노출되지 않는다.
- [ ] details/busyOnly에서 hidden으로 변경한 transaction이 projection 삭제와 tombstone을 원자적으로 커밋한다.
- [ ] hidden 변경 뒤 이전 snapshot worker가 완료돼도 projection이 되살아나지 않는다.
- [ ] details 상세가 없으면 busyOnly로만 보이고 snapshot 완료 후에만 details로 승격한다.
- [ ] 멤버십 종료와 공개 설정 변경 경쟁 후 해당 사용자의 Party projection과 설정 row가 0건이다.

### Sync와 복구

- [ ] 다른 멤버의 sync에는 공개 설정 row가 없고 허용 projection만 있다.
- [ ] cursor 만료 bootstrap 뒤 서버에 없는 hidden projection이 로컬에 남지 않는다.
- [ ] 하향 변경 성공 응답 시 상세 데이터 삭제와 tombstone 기록이 이미 commit되어 있다.
- [ ] 복호화 실패 projection은 상세를 반환하지 않고 busyOnly로 안전 강등된다.

---

## 14. 검증 계획

### 서버 단위 테스트

- 공개 수준·`share_location` 조합 validation
- 전이별 projection diff와 field allowlist
- details safe downgrade와 refresh state
- setting/source generation version guard
- 로그·분석 payload redaction

### PostgreSQL 통합 테스트

- 활성 멤버별 설정 unique와 check constraint
- busyOnly/hidden row·ciphertext 불변식
- 설정 변경 + projection diff + sync change 원자성
- snapshot 완료와 visibility mutation 경쟁
- 탈퇴와 visibility mutation 경쟁
- 동일 멱등성 key의 중복 방지

### API·Sync 계약 테스트

- 본인/다른 멤버/비멤버/과거 멤버 권한 조합
- 세 수준과 장소 토글별 JSON schema allowlist
- visibility setting의 본인 전용 recipient
- projection upsert/tombstone version 적용 순서
- cursor 만료 후 scope replacement

### iOS 회귀 테스트

- `hidden`이어도 private Busy input이 계산에 남는지
- `Party.visibilityByMember` 제거 후 본인 설정만 저장하는지
- `AvailabilitySlot`에서 인원 필드가 제거되는지
- hidden 안내, details pending과 location toggle 화면 상태
- tombstone 수신·로그아웃·탈퇴 시 로컬 projection 삭제

### E2E·Privacy 회귀

- 두 사용자로 details/busyOnly/hidden을 순환해 화면 필드와 동일한 공통 시간 결과 비교
- 다른 멤버가 visibility endpoint·sync·bootstrap으로 설정값을 얻지 못하는지 검사
- hidden 변경 직후 offline 기기가 돌아와도 기존 projection이 삭제되는지 확인
- API JSON, DB fixture, log, trace와 APNs payload의 금지 키·값 스캔
- 반복 availability/conflict probe의 rate limit과 무원인 응답 검증

---

## 15. 현재 구현과의 차이

`src/PlanTogether/Models.swift`, `AppStore.swift`, `RootView.swift` 기준이다.

| 현재 | 확정 설계 | 필요한 변경 |
|---|---|---|
| `Party.visibilityByMember`가 모든 멤버 설정을 보유 | 본인 설정은 Party 밖의 별도 entity이고 타인 값은 수신하지 않음 | `PartyVisibilitySetting` 분리 |
| hidden이면 `BusyInterval`을 추가하지 않음 | hidden도 private Busy input에 포함 | 계산 facts와 화면 projection 분리 |
| busy interval 하나가 계산과 화면 표시를 겸함 | private fact와 Party projection이 별도 | 저장소·DTO·sync 모델 분리 |
| details가 title만 조건부 보관 | title 공유 + 장소 별도 동의, 기타 필드 금지 | allowlist serializer 추가 |
| `AvailabilitySlot`에 가능/전체 인원수 존재 | 전원 공통 슬롯만 반환 | 두 count 필드 제거 |
| 공개 설정이 메모리에서 즉시 바뀜 | 서버 version·멱등성·projection transaction 필요 | API client와 sync 적용 추가 |
| 다른 멤버의 캘린더 연결 여부를 `syncedMemberIDs`로 보유 | 일반 멤버에게 개별 상태 비노출 | aggregate gate와 본인 상태 분리 |

이번 작업은 설계 확정 단계이므로 Swift 코드는 변경하지 않는다. 기존 프로젝트는 그대로 빌드·실행 가능한 상태를 유지한다.

---

## 16. 구현 순서

1. `party_visibility_settings`에 `share_location`, version 제약을 추가하고 projection 스키마를 확정한다.
2. visibility self API와 권한·version·멱등성 계약을 구현한다.
3. private Busy facts와 Party projection service를 분리한다.
4. 전이별 projection diff, tombstone과 recipient별 sync를 구현한다.
5. snapshot 완료와 visibility 변경의 generation/version guard를 구현한다.
6. iOS에서 본인 설정 모델, 공개 설정 화면과 details pending 상태를 구현한다.
7. `Party.visibilityByMember`, count 기반 availability UI와 hidden 계산 제거 동작을 교체한다.
8. §14의 단위·PostgreSQL·API·sync·iOS·E2E·Privacy 테스트를 자동화한다.

Party 서버 골격과 membership P0·P1이 선행한다. P1의 최소 visibility/projection schema는 이 설계의 최종 필드와
constraint로 확장한 뒤 P2·P3의 초기 busyOnly projection 생성에 사용한다.

---

## 17. 후속 설계 제약

- 설계 6은 전원 공통 슬롯만 반환하고 `availableMemberCount`, 개인별 원인과 hidden 멤버 수를 추가하지 않는다.
- 설계 7은 임의 제안·확정 충돌 검사도 private Busy facts를 사용하고 충돌 소유자를 반환하지 않는다.
- 설계 8은 확정 일정 쓰기 결과를 Party projection 원본으로 사용하지 않는다. 다음 정상 snapshot이 별도로 반영한다.
- 설계 9의 푸시는 공개 수준과 일정 내용을 포함하지 않고 opaque sync marker만 전달한다.
- 일정별·멤버별 예외 공개가 필요해지면 별도 privacy review와 migration 설계 없이는 현재 setting 의미를 확장하지 않는다.
