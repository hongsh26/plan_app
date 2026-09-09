# 계정·백엔드·데이터 소유권 상세 설계

## 1. 설계 상태

- 상태: 확정
- 대상: Launch MVP
- 서버 기준: Go 자체 API + PostgreSQL
- 우선순위: 초기 개발 속도보다 인프라 통제, 데이터 소유권, 장기 확장성
- 현재 iOS 구현 연결점: `src/PlanTogether/Models.swift`, `src/PlanTogether/AppStore.swift`, `src/PlanTogether/CalendarService.swift`

이 설계는 계정, 세션, 기기, 서버 데이터 소유권, 동기화, 비동기 작업과 배포 경계를 확정한다. Party의 초대·역할·탈퇴 정책은 후속 설계에서 확정하며, 캘린더 공개·동기화 세부 계약은 `docs/calendar_privacy_sync_design.md`를 따른다.

## 2. 목표와 제외 범위

### 목표

1. Apple 로그인 사용자를 서버의 안정적인 사용자 ID와 연결한다.
2. Party, 멤버십, 공개 정책, 제안·응답·확정 상태를 서버 기준 데이터로 관리한다.
3. 여러 iOS 기기가 같은 상태를 증분 동기화하고 오프라인 변경을 안전하게 재전송한다.
4. 서버는 캘린더 쓰기 명령과 상태를 관리하고, EventKit 권한을 가진 iOS 기기가 실제 쓰기를 수행하도록 분리한다.
5. 특정 BaaS나 클라우드의 데이터 모델에 종속되지 않는다.
6. 계정 삭제 시 접근 차단, 개인정보 삭제, 협업 기록 익명화를 검증 가능하게 만든다.

### 제외 범위

- Google/이메일 로그인과 Google Calendar
- 결제와 Premium
- WebSocket 기반 상시 연결
- 마이크로서비스, Kubernetes, Kafka
- 멀티 리전 active-active
- Party 세부 정책과 공개 수준의 최종 의미

## 3. 아키텍처 결정

### 3.1 형태

모듈러 모놀리스를 사용한다. 하나의 코드베이스와 컨테이너 이미지를 사용하되 실행 역할은 분리한다.

```text
iOS App
  ├─ EventKit: 개인 캘린더 원본
  ├─ SwiftData: 서버 데이터의 로컬 투영/오프라인 큐
  └─ HTTPS REST + APNs invalidation
                 │
          ┌──────▼──────┐
          │ Go API      │ 인증·권한·도메인 상태 전이
          └──────┬──────┘
                 │ PostgreSQL transaction
       ┌─────────▼─────────┐
       │ PostgreSQL        │ 기준 데이터·변경 로그·outbox/job
       └──────┬─────────┬──┘
              │         │
       ┌──────▼───┐ ┌──▼────────┐
       │ Worker   │ │ Scheduler │
       │ APNs/정리│ │ 만료/재시도│
       └──────────┘ └───────────┘
```

### 3.2 기술 경계

| 영역 | 결정 | 이유 |
|---|---|---|
| 서버 | 지원 중인 안정 Go 버전 | 작은 런타임, 단순한 병렬 처리와 컨테이너 배포 |
| HTTP | `net/http` + 얇은 라우터 | 프레임워크 종속 최소화 |
| API | HTTPS REST/JSON, `/v1` 경로, OpenAPI 계약 | iOS 연동과 오류/멱등성 계약을 명시하기 쉬움 |
| DB | PostgreSQL, SQL 마이그레이션 | 트랜잭션, 제약 조건, 이식성 |
| DB 접근 | PostgreSQL 전용 드라이버 + 명시적 SQL | 쿼리와 잠금 동작 통제 |
| 작업 큐 | PostgreSQL outbox/job | 도메인 변경과 작업 생성을 원자적으로 커밋 |
| 캐시 | 초기 필수 구성에서 제외 | 기준 데이터 이중화와 무효화 복잡도 방지 |
| 푸시 | APNs | Launch MVP의 iOS 알림과 동기화 알림 |
| 로컬 저장 | SwiftData | 서버 투영 캐시와 오프라인 mutation 보관 |

구체 라이브러리와 버전은 구현 착수 시 공식 지원 상태, 라이선스와 보안 공지를 다시 확인해 잠근다.

## 4. 데이터 소유권

| 데이터 | 기준 소유자 | 복제/캐시 | 원칙 |
|---|---|---|---|
| Apple 계정 자격 | Apple | subject와 삭제 시 revoke할 refresh token을 저장 | 이메일을 계정 키로 사용하지 않음 |
| 앱 사용자·프로필 | 서버 | iOS SwiftData | 서버 버전이 기준 |
| Party·멤버십·초대 | 서버 | iOS SwiftData | 모든 권한 검증은 서버에서 수행 |
| 공개 설정 | 서버 | iOS SwiftData | 서버가 응답 필터와 계산 입력을 강제 |
| EventKit 원본 ID·메모·참석자 | 사용자 기기 | 서버 요청에도 원문 포함 금지 | 서버 로그·trace에도 포함 금지 |
| 일정 시간 구간 | 서버 | iOS에도 원본/투영 존재 | 공통 시간 계산과 다기기 동기화에 사용 |
| 상세 공유 제목·장소 | 서버의 Party별 투영 | 권한 있는 Party 기기에만 캐시 | 후속 공개 설계의 명시적 동의와 필드 제한 적용 |
| 제안·응답·확정 | 서버 | iOS SwiftData | 서버 상태 전이만 유효 |
| 캘린더 쓰기 명령·상태 | 서버 | 지정된 iOS 기기가 명령 실행 | EventKit 작업 자체는 서버가 실행하지 않음 |
| 푸시 토큰 | 서버 | 기기 Keychain/OS | 원문 로그 금지, 폐기 응답 시 제거 |

### 캘린더 데이터 최소화

- 서버의 비공개 일정 사실은 `start_at`, `end_at`, `all_day`, `time_zone`, 기기가 생성한 무작위 `source_event_key`, `source_revision`만 저장한다.
- iOS는 EventKit identifier와 `source_event_key`의 대응을 로컬에 보관한다. 서버에는 EventKit identifier를 보내지 않는다.
- 로컬 대응이 유실되면 날짜 범위별 `snapshot_replace` 동기화로 서버 구간을 교체하며 원본 identifier 복구를 시도하지 않는다.
- 제목·장소는 `details` 공개가 필요한 Party별 투영에만 제한적으로 저장한다.
- 메모, URL, 참석자, 주최자, 캘린더 이름은 Launch MVP 서버에 업로드하지 않는다.
- Party 탈퇴 또는 공개 수준 하향 시 해당 Party의 상세 투영을 삭제하고 변경 로그에 tombstone을 남긴다.

## 5. 계정과 인증

### 5.1 Apple 로그인 흐름

```text
iOS ── Apple authorization + nonce ──▶ Apple
iOS ◀── identity token/code ───────── Apple
iOS ── token + single-use code + nonce ──▶ API
API ── 서명·issuer·audience·만료·nonce 검증, code 즉시 교환
API ── (provider=apple, subject) 조회/생성
API ── server device id + access token + rotating refresh token ──▶ iOS Keychain
```

- Apple의 안정적인 subject를 외부 계정 식별자로 사용한다.
- 이메일과 표시 이름은 최초 동의 시 프로필 초기값으로만 사용하며 로그인 키로 사용하지 않는다.
- 서버는 토큰의 서명, issuer, audience, expiration과 nonce를 검증한다.
- authorization code는 Apple 계약에 따라 5분 이내 한 번만 교환한다. 교환용 client secret은 secret manager의 Apple private key로 서버에서 생성한다.
- Apple refresh token은 KMS 기반 envelope encryption으로 저장하고 계정 삭제 시 Apple revoke endpoint를 호출한 뒤 폐기한다.
- 동일한 Apple subject는 이메일이 달라져도 같은 `user_id`에 연결한다.
- 계정 연결 테이블은 다중 provider를 허용하되 Launch MVP API는 Apple만 노출한다.

### 5.2 세션

- Access token: 서명된 단기 토큰, 기본 15분.
- Refresh token: 256-bit 이상 난수 opaque token, 기본 30일, DB에는 해시만 저장.
- Refresh 성공 시 이전 토큰을 폐기하고 새 토큰으로 회전한다.
- 이미 사용된 refresh token이 재사용되면 해당 기기의 token family 전체를 폐기한다.
- iOS는 토큰을 Keychain에 저장하고 로그·분석 이벤트에 기록하지 않는다.
- device ID는 최초 로그인 때 서버가 발급하고 access/session에 결합한다. 클라이언트가 임의 device ID를 권한 근거로 만들 수 없다.
- 로그아웃은 현재 기기 세션만 폐기한다. 사용자는 설정에서 다른 기기 세션을 조회하고 폐기할 수 있다.
- 계정 비활성화·삭제 요청 시 모든 세션을 즉시 폐기한다.

### 5.3 계정 상태

```text
active → deletion_requested → deleting → deleted
   └──────────────→ disabled ────────────┘
```

- `active`: 정상 사용.
- `disabled`: 보안/운영 차단. 로그인과 모든 mutation 금지.
- `deletion_requested`: 모든 세션 즉시 폐기, 로그인과 mutation 금지.
- `deleting`: Apple token revoke, 외부 연결 및 개인정보 삭제 작업 진행.
- `deleted`: 재인증 불가. 협업 기록에는 익명화된 tombstone만 유지.

삭제 요청은 취소 가능한 유예 기간 없이 즉시 접근을 차단하고 24시간 안에 삭제 작업을 완료하는 것을 Launch MVP 목표로 한다. 법적 보존 의무가 확인되면 보존 범위와 기간을 별도 정책으로 덮어쓴다.

## 6. API 계약

### 공통 규칙

- 모든 API는 `/v1` 아래에 둔다.
- 요청·응답 시각은 RFC 3339 UTC로 전달하고 표시 시간대는 별도 필드로 보존한다.
- 모든 mutation은 `Idempotency-Key`를 요구한다. device ID는 access token claim에서 가져오며 path/header 값이 있으면 claim과 일치해야 한다.
- 수정/삭제는 `If-Match` 또는 요청 본문의 `expected_version`을 요구한다.
- 성공 응답은 `request_id`, 변경된 resource와 `version`을 반환한다.
- 오류는 `code`, `message`, `request_id`, 선택적 `current_version`을 반환한다.

### 핵심 endpoint

| Method | Path | 목적 |
|---|---|---|
| POST | `/v1/auth/apple` | Apple credential 검증, 가입/로그인 |
| POST | `/v1/auth/refresh` | refresh token 회전 |
| POST | `/v1/auth/logout` | 현재 기기 세션 폐기 |
| GET | `/v1/me` | 사용자와 계정 상태 조회 |
| PATCH | `/v1/me` | 프로필 수정 |
| DELETE | `/v1/me` | 계정 삭제 요청 |
| GET | `/v1/devices` | 로그인·푸시 기기 조회 |
| DELETE | `/v1/devices/{id}` | 특정 기기 세션·푸시 폐기 |
| PUT | `/v1/devices/{id}/push-token` | APNs token 등록·교체 |
| POST | `/v1/sync` | 로컬 mutation batch 전송 후 증분 변경 수신 |
| GET | `/v1/sync?cursor=` | 증분 변경 수신 |
| GET | `/v1/sync/bootstrap` | 권한 있는 전체 snapshot과 cursor watermark 수신 |
| POST | `/v1/calendar-write-commands/{id}/claim` | 지정 기기가 EventKit 명령 lease 획득 |
| POST | `/v1/calendar-write-commands/{id}/result` | EventKit 실행 결과와 로컬 locator 보고 |

Party, 공개 설정, 제안 API는 후속 설계에서 추가하되 공통 멱등성·버전·권한 규칙을 그대로 적용한다.

### 오류 코드

| HTTP | code | 의미 |
|---:|---|---|
| 400 | `invalid_request` | 형식 또는 필드 검증 실패 |
| 401 | `invalid_session` | access/refresh token 무효 |
| 403 | `forbidden` | 인증됐지만 대상 권한 없음 |
| 404 | `not_found` | 존재하지 않거나 노출할 수 없는 대상 |
| 409 | `version_conflict` | 낡은 버전으로 mutation 시도 |
| 409 | `idempotency_mismatch` | 같은 키를 다른 요청 본문에 재사용 |
| 410 | `sync_cursor_expired` | 보존 기간을 지난 cursor, 전체 재동기화 필요 |
| 423 | `account_locked` | 비활성화·삭제 중인 계정 |
| 429 | `rate_limited` | 인증/초대 등의 요청 제한 초과 |

## 7. 동기화와 오프라인

### 7.1 서버 변경 피드

- `sync_changes.seq`는 전체 시스템에서 단조 증가하는 `BIGINT`다.
- 각 변경은 `recipient_user_id`를 가져 사용자별로 접근을 제한한다.
- 클라이언트 cursor는 마지막으로 반영한 `seq` 하나다. 중간 번호가 비어도 정상이다.
- 변경 payload는 전체 객체가 아닌 type, id, operation, version과 최소 projection을 포함한다.
- 삭제와 탈퇴는 tombstone으로 전달한다.
- 변경 피드는 기본 30일 보존하며 만료된 cursor는 `410`을 반환해 전체 재동기화를 요구한다.

### 7.2 Bootstrap

- `/v1/sync/bootstrap`은 `schema_version`, `snapshot`, `cursor_watermark`, `server_time`을 반환한다.
- snapshot은 호출 사용자가 현재 볼 수 있는 사용자, Party, 멤버십, 공개 설정, 제안, 확정 이벤트와 캘린더 명령만 포함한다.
- iOS는 한 개의 SwiftData transaction에서 기존 서버 투영을 snapshot으로 교체하고 cursor를 watermark로 설정한다.
- 아직 전송하지 않은 로컬 mutation은 별도 queue에 보존하고 snapshot 반영 후 의존 entity/version을 다시 확인해 재전송하거나 conflict로 표시한다.
- tombstone을 먼저 적용해 삭제된 entity를 되살리지 않은 뒤 version이 더 큰 upsert만 적용한다.

### 7.3 iOS 로컬 상태

- SwiftData는 서버 데이터의 읽기 캐시이며 기준 데이터가 아니다.
- 로컬 서버 투영은 `server_id`, `server_version`, `updated_at`, `deleted_at`을 공통으로 가진다.
- 로컬 mutation queue는 `operation_id`, `idempotency_key`, `entity_type`, `entity_id`, `expected_version`, `payload`, `created_at`, `attempt_count`, `last_error`를 가진다.
- EventKit mapping은 `source_event_key`, 로컬 EventKit identifier, calendar identifier와 마지막 확인 시각을 기기에만 저장한다.
- 로컬 mutation은 UUID operation ID, idempotency key, expected version과 함께 저장한다.
- 네트워크 복구 시 생성 순서대로 전송하되 독립 entity는 병렬 전송할 수 있다.
- 서버 성공 응답을 받은 뒤에만 로컬 mutation을 완료 처리한다.
- APNs background notification은 변경 사실만 알리고 실제 데이터는 `/sync`에서 가져온다.
- background notification이 도착하지 않아도 앱 활성화 시 항상 sync를 수행한다.

### 7.4 충돌

- 생성: 동일 idempotency key 재전송은 원래 결과를 반환한다.
- 수정/삭제: expected version 불일치 시 `409`와 현재 version을 반환한다.
- 제안 응답도 last-write-wins를 사용하지 않고 버전 기반으로 처리한다.
- 클라이언트는 현재 서버 상태를 표시하고 사용자가 다시 적용할 수 있게 한다.
- 서버 시간과 version을 기준으로 하며 기기 시각으로 mutation 순서를 결정하지 않는다.

## 8. PostgreSQL 데이터 모델

모든 ID는 UUID, 시각은 `timestamptz`, 변경 가능한 도메인 row는 `version bigint`와 `updated_at`을 가진다.

| 테이블 | 핵심 필드/제약 |
|---|---|
| `users` | `id`, `display_name`, `status`, `version`, `deleted_at` |
| `auth_identities` | `user_id`, `provider`, `provider_subject`, `provider_refresh_token_ciphertext`; unique `(provider, provider_subject)` |
| `devices` | `id`, `user_id`, `platform`, `push_token_ciphertext`, `last_seen_at`, `revoked_at` |
| `sessions` | `id`, `user_id`, `device_id`, `token_family_id`, `refresh_token_hash`, `expires_at`, `used_at`, `revoked_at` |
| `parties` | `id`, `name`, `status`, `version`, `owner_membership_id` |
| `party_memberships` | `party_id`, `user_id`, `role`, `status`, `version`; 활성 membership unique |
| `party_invites` | `party_id`, `token_hash`, `status`, `expires_at`, `max_uses`, `used_count` |
| `party_visibility_settings` | `party_id`, `user_id`, `visibility_level`, `version`; unique `(party_id, user_id)` |
| `calendar_connections` | `user_id`, `provider`, `status`, `last_sync_at`, `sync_revision`; 활성 provider unique |
| `calendar_busy_facts` | `user_id`, `start_at`, `end_at`, `all_day`, `time_zone`, 무작위 `source_event_key`, `source_revision` |
| `party_schedule_projections` | `party_id`, `owner_user_id`, 시간 구간, `visibility_level`, 선택적 암호화된 제목·장소 |
| `proposals` | `party_id`, `created_by`, 시간/제목/장소, `status`, `version` |
| `proposal_participants` | `proposal_id`, `user_id`, `response`, `version`; unique `(proposal_id, user_id)` |
| `confirmed_events` | `proposal_id`, `status`, `version`; proposal당 활성 확정 unique |
| `calendar_write_commands` | `confirmed_event_id`, `user_id`, `executor_device_id`, `operation`, `revision`, `status`, `lease_until`, `result_locator`; 활성 command unique |
| `idempotency_keys` | `user_id`, `device_id`, `key`, `request_hash`, 최소 result pointer, `expires_at`; unique `(user_id, device_id, key)` |
| `sync_changes` | `seq`, `recipient_user_id`, `entity_type`, `entity_id`, `operation`, `entity_version`, `payload` |
| `outbox_jobs` | `type`, `payload`, `status`, `attempt_count`, `next_run_at`, `locked_until`, `dedupe_key` |
| `audit_events` | actor/action/target/result/request ID; 캘린더 내용과 token 저장 금지 |

### 필수 DB 불변식

- 외부 identity 하나는 활성 사용자 하나에만 연결된다.
- Party의 활성 멤버는 `(party_id, user_id)`당 하나다.
- 제안 참여자는 제안 생성 시 고정된 snapshot이며 중복되지 않는다.
- 제안 하나에는 활성 확정 이벤트가 최대 하나다.
- 도메인 상태 전이와 `sync_changes`/`outbox_jobs`/`calendar_write_commands` 생성은 같은 트랜잭션에서 커밋한다.
- 멱등성 키의 request hash가 다르면 원래 결과를 재사용하지 않고 오류를 반환한다.
- `sync_changes`는 `(recipient_user_id, seq)` index로 사용자 cursor 조회를 보장한다.
- 활성 membership은 `(party_id, user_id) WHERE status = 'active'` partial unique index를 가진다.
- 활성 outbox dedupe는 `(type, dedupe_key) WHERE status IN ('pending','running','retryable_failed')` partial unique index를 가진다.
- 일정 범위 조회는 `(user_id, start_at, end_at)` index를 가진다.
- 상태 문자열은 check constraint 또는 PostgreSQL enum으로 허용 값 밖의 입력을 거부한다.
- FK 삭제 정책은 calendar facts/session/device는 cascade, 협업 기록은 restrict 후 익명화로 구분한다.

### 멱등성 결과 보존

- 전체 응답 본문 대신 `status_code`, `resource_type`, `resource_id`, `resource_version`만 기본 30일 저장한다.
- 재전송에서도 현재 인증·권한을 먼저 평가한다. 권한을 잃은 사용자는 과거 성공 payload 대신 현재 `403/404`를 받는다.
- 민감한 결과가 불가피하면 암호화된 최소 envelope만 저장하며 캘린더 상세와 token은 포함하지 않는다.

## 9. 비동기 작업

### 역할

- `api`: 인증, 권한, 상태 전이, sync.
- `worker`: APNs 발송, 계정 삭제, 명령 만료·재할당과 서버 측 재시도 상태 관리. EventKit 작업은 수행하지 않는다.
- `scheduler`: 만료 초대, 알림 예약, 장기 실행 작업 복구, 삭제 작업 시작.

### job 상태

```text
pending → running → succeeded
             └──→ retryable_failed → pending
             └──→ dead
```

- worker는 `FOR UPDATE SKIP LOCKED`와 lease를 사용한다.
- 재시도는 지수 backoff와 jitter를 적용하며 작업 종류별 최대 횟수를 둔다.
- 동일 외부 작업은 `dedupe_key`로 하나만 활성화한다.
- 캘린더 명령 dedupe key는 `confirmed_event_id:user_id:operation:revision`이다.
- 명령은 EventKit 권한과 활성 상태가 확인된 `executor_device_id` 하나에 lease된다.
- iOS는 앱이 만든 URL/metadata의 안정적인 event key와 로컬 mapping을 먼저 조회해 create 중복을 방지한 뒤 EventKit 작업 결과를 서버에 보고한다.
- lease 만료는 같은 지정 기기의 재시도를 우선하고, 다른 기기로의 재할당은 사용자가 calendar writer device를 전환했을 때만 허용한다.
- 영구 실패는 `dead`로 이동하고 사용자 조치가 필요한 상태를 sync feed에 기록한다.
- worker crash 후 `locked_until`이 지나면 다른 worker가 안전하게 재개한다.

## 10. 계정 삭제

1. API 트랜잭션에서 계정을 `deletion_requested`로 바꾸고 모든 세션·기기를 폐기한다.
2. 사용자가 Party 소유자면 후속 Party 정책에 따라 위임 가능한 관리자에게 넘기고, 불가능하면 Party를 종료한다.
3. worker가 저장된 Apple refresh token으로 Apple revoke endpoint를 호출하고, 성공 또는 최종 확인 후 token ciphertext를 폐기한다.
4. worker가 캘린더 연결, 푸시 token, 일정 사실과 Party별 상세 투영을 삭제한다.
5. 멤버십, 제안, 확정 기록의 사용자 표시 정보는 `탈퇴한 사용자`로 익명화한다.
6. 보안 audit는 사용자 ID를 비가역 내부 tombstone으로 치환하고 기본 90일 뒤 삭제한다.
7. 24시간 안에 `deleted` 상태로 전환하고 재로그인을 새 계정 생성 흐름으로 처리한다.

삭제 실패는 재시도하며 24시간 목표를 넘기면 운영 경보를 발생시킨다. 정확한 법적 보존 요건이 생기면 별도 데이터 보존 정책을 우선 적용한다.

## 11. 권한과 보안

- 모든 Party 데이터 접근은 활성 membership을 서버에서 확인한다.
- 소유권/역할 검증은 handler가 아닌 도메인 service 경계에서 수행한다.
- 인증, 초대 코드 검증, 계정 삭제에는 별도 rate limit을 적용한다.
- TLS 외 평문 통신을 허용하지 않는다.
- refresh token, 초대 token은 DB에 해시만 저장한다.
- Apple provider refresh token은 revoke를 위해서만 KMS envelope encryption으로 저장하며 일반 API DB 역할은 복호화 권한을 갖지 않는다.
- 푸시 token과 Party 상세 일정 필드는 KMS 기반 envelope encryption 대상으로 둔다.
- DB 역할은 migration, api, worker, read-only 운영 계정으로 분리한다.
- 로그에는 token, Apple credential, 캘린더 제목·장소·메모, 초대 원문을 남기지 않는다.
- 구조화 로그는 `request_id`, actor 내부 ID, action, result, latency만 기록한다.

## 12. 배포와 운영

### Launch MVP 토폴로지

- 단일 리전, 다중 가용 영역의 관리형 PostgreSQL.
- 외부 HTTPS load balancer 뒤에 최소 2개 API task.
- 최소 1개 worker와 단일 leader scheduler. scheduler 작업 자체는 DB lease로 중복을 방지한다.
- container registry, secrets manager, KMS와 중앙 로그/metric 저장소.
- 자동 DB 백업과 point-in-time recovery를 활성화한다.
- 스테이징과 프로덕션은 계정, DB, Apple/APNs 자격을 분리한다.

### 초기 목표

| 지표 | 목표 |
|---|---|
| 일반 API latency | 서버 측 p95 300ms 이하 |
| sync latency | 앱 활성화 후 정상 네트워크에서 5초 안에 최신 변경 반영 |
| outbox/캘린더 명령 생성 | 도메인 commit과 원자적, 누락 0건 |
| 알림 작업 생성 | 도메인 이벤트 commit 후 5초 이내 worker 대상화 |
| 캘린더 명령 claim | 앱 활성화 및 sync 후 5초 이내 지정 기기가 claim 가능 |
| 캘린더 명령 완료 | 정상 EventKit 환경에서 claim 후 10초 이내 결과 보고 |
| 캘린더 명령 실패 | dead command rate와 writer-device unavailable 건수를 경보화 |
| DB 가용성 | 관리형 Multi-AZ 구성 |
| 복구 목표 | RPO 5분 이하, RTO 60분 이하 |
| 계정 삭제 | 요청 후 24시간 이내 완료 |

실제 SLO는 내부 Alpha 계측 후 조정하되, 완화할 때는 변경 이유를 기록한다.

## 13. 저장소 구조

현재 iOS 앱과 같은 저장소에 서버를 추가해 계약 변경을 함께 검증한다.

```text
project-root/
├─ src/                         # iOS 앱
├─ server/
│  ├─ cmd/api/
│  ├─ cmd/worker/
│  ├─ cmd/scheduler/
│  ├─ internal/auth/
│  ├─ internal/account/
│  ├─ internal/sync/
│  ├─ internal/party/
│  ├─ internal/calendar/
│  ├─ internal/proposal/
│  ├─ internal/notification/
│  ├─ internal/platform/postgres/
│  ├─ internal/platform/appleid/
│  ├─ internal/platform/apns/
│  ├─ migrations/
│  ├─ openapi/
│  └─ test/
└─ docs/
```

모듈은 서로의 DB 테이블을 직접 수정하지 않고 service/transaction 경계를 통과한다. 실제 트래픽과 팀 규모가 분리를 요구하기 전에는 별도 서비스로 쪼개지 않는다.

## 14. 인수 조건

### 인증·세션

- 같은 Apple subject로 다시 로그인하면 기존 `user_id`를 반환하고 중복 사용자를 만들지 않는다.
- device A 로그아웃 후 A의 refresh는 실패하지만 device B 세션은 유지된다.
- 사용된 refresh token 재사용 시 해당 device token family가 전부 폐기된다.
- `disabled`, `deletion_requested`, `deleting`, `deleted` 계정은 보호 API를 사용할 수 없다.

### 동기화·충돌

- device A의 변경 이후 device B가 이전 cursor로 sync하면 변경을 한 번 이상 전달받고 version 기준으로 정확히 한 번 적용한다.
- 같은 멱등성 키로 제안을 재전송해도 proposal ID는 하나만 생성된다.
- 같은 키에 다른 request body를 보내면 `idempotency_mismatch`가 반환된다.
- stale version mutation은 `409`와 최신 version을 반환한다.
- 30일이 지난 cursor는 `410`을 반환하고 전체 sync로 복구된다.
- bootstrap 중 로컬 미전송 mutation은 유실되지 않고 snapshot 이후 재검증된다.

### 데이터·권한

- Party 비멤버는 Party, 공개 설정, 일정 투영, 제안과 sync 변경을 읽거나 수정할 수 없다.
- Busy Only 응답에는 제목, 위치, 메모, 참석자, EventKit identifier가 포함되지 않는다.
- 도메인 상태 전이와 sync/outbox 생성 사이에 부분 commit이 발생하지 않는다.
- 모든 멤버의 캘린더 sync 조건을 만족하지 못하면 공통 가능 시간을 확정 결과로 반환하지 않는다. 이는 현재 `AvailabilityCalculator`의 전원 동기화 불변식과 일치해야 한다.

### 삭제·작업

- 계정 삭제 요청 즉시 모든 세션이 폐기되고 24시간 안에 개인정보 삭제/익명화가 완료된다.
- 지정 iOS 기기가 동일 캘린더 명령을 재실행해도 EventKit 이벤트가 중복 생성되지 않는다.
- 서버는 EventKit identifier를 요청, DB, 로그와 trace 어디에도 수신하거나 저장하지 않는다.
- dead job은 운영 경보와 사용자 조치 상태를 만들며 무한 재시도하지 않는다.
- APNs token 무효 응답을 받으면 해당 token을 비활성화하고 다시 발송하지 않는다.

## 15. 검증 계획

### 단위 테스트

- Apple claim 검증과 계정 매칭
- authorization code 1회 교환, provider token 암호화와 revoke
- 세션 회전·재사용 탐지
- 계정 상태 전이
- 버전 충돌과 멱등성 request hash
- job backoff·dedupe key
- 로그/응답 개인정보 필터

### PostgreSQL 통합 테스트

- unique/check/FK 제약
- 상태 전이 + sync change + outbox 원자성
- 두 transaction의 동시 응답/확정 경쟁
- `SKIP LOCKED` worker 경쟁과 lease 복구
- 계정 삭제 cascade와 익명화

### API 계약 테스트

- OpenAPI schema와 실제 handler 응답 일치
- 인증/권한별 401·403·404 구분
- idempotency replay와 mismatch
- sync cursor 정상·만료·tombstone
- bootstrap snapshot/watermark와 로컬 mutation 보존

### E2E

- 두 기기 로그인 → Party 상태 변경 → APNs nudge → delta sync
- 오프라인 제안 → 재연결 → 중복 없이 생성
- 확정 → 서버 명령 생성 → 지정 iOS 기기 EventKit 실행 → 부분 실패 → 동일 기기 재시도
- 계정 삭제 → 세션 차단 → 개인정보 삭제 → 협업 기록 익명화

### 운영 검증

- DB 백업 복구 훈련으로 RPO/RTO 확인
- worker 강제 종료 후 lease 복구
- Apple 공개키 회전, provider token revoke와 APNs token 무효화 시뮬레이션
- 민감정보 로그 스캔

## 16. 리스크와 완화

| 리스크 | 완화 |
|---|---|
| 캘린더 민감정보 과수집 | 사실/Party 투영 분리, 필드 allowlist, 로그 금지 |
| 오프라인 mutation 중복 | 기기별 멱등성 키와 request hash |
| stale 상태 덮어쓰기 | expected version과 409 충돌 |
| 확정과 캘린더 명령 불일치 | 동일 DB transaction에서 사용자별 명령 생성 |
| EventKit 기기 실행 중복 | 지정 writer device, command lease, 로컬 app event key 조회 |
| PostgreSQL queue 병목 | 지표로 확인 후 worker/shard 또는 외부 queue로 이전 가능한 인터페이스 |
| 토큰 탈취 | 짧은 access token, refresh 회전·해시·재사용 탐지 |
| 모듈러 모놀리스 결합 | 도메인 service 경계와 모듈별 table ownership |
| 운영 복잡도 조기 증가 | 표준 컨테이너·관리형 PostgreSQL, 캐시/브로커/Kubernetes 보류 |

## 17. 후속 설계에 넘길 결정

- Party: 역할, 소유자 탈퇴/삭제, 초대 만료·재사용, 멤버 상한.
- 가능 시간: sync freshness, 검색 기간, 활동 시간, 슬롯 단위.
- 제안: 참여자 snapshot, 응답 변경, 확정 조건과 취소 상태.
- 캘린더 쓰기: 사용자별 상태와 영구 실패 UX.
- 알림: 알림별 urgency, quiet hours, 중복 억제.

## 18. 참고 공식 문서

- [Sign in with Apple REST API](https://developer.apple.com/documentation/signinwithapplerestapi)
- [Communicating with APNs](https://developer.apple.com/library/archive/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/CommunicatingwithAPNs.html)
- [Pushing background updates to your app](https://developer.apple.com/documentation/usernotifications/pushing-background-updates-to-your-app)
- [Accessing the Event Store](https://developer.apple.com/documentation/eventkit/accessing-the-event-store)
- [Generate and validate tokens](https://developer.apple.com/documentation/signinwithapplerestapi/generate-and-validate-tokens)
- [Revoke tokens](https://developer.apple.com/documentation/signinwithapplerestapi/revoke-tokens)
- [Go release history](https://go.dev/doc/devel/release)
- [PostgreSQL versioning policy](https://www.postgresql.org/support/versioning/)
