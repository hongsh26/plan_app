# 진행 상황

## 현재

**설계 단계는 끝났다. 상세 설계 1~9가 모두 확정됐고 각각 구현 계획이 `.omc/plans/`에 1:1로 있다. 지금은 구현 단계다.**

| 설계 | 문서 | 구현 계획 |
|---|---|---|
| 1~2 계정·서버·데이터 소유권 | `docs/account_backend_design.md` | `.omc/plans/account-backend-implementation.md` |
| 3 캘린더 권한·동기화 | `docs/calendar_privacy_sync_design.md` | (설계 5 계획에 포함) |
| 4 Party 생성·초대·역할·탈퇴 | `docs/party_membership_design.md` | `.omc/plans/party-membership-implementation.md` |
| 5 공개 수준 | `docs/party_visibility_design.md` | `.omc/plans/party-visibility-implementation.md` |
| 6 공통 가능 시간 검색 | `docs/availability_search_design.md` | `.omc/plans/availability-search-implementation.md` |
| 7 제안·응답·확정 | `docs/proposal_lifecycle_design.md` | `.omc/plans/proposal-lifecycle-implementation.md` |
| 8 확정 일정 캘린더 쓰기 | `docs/calendar_write_design.md` | `.omc/plans/calendar-write-implementation.md` |
| 9 알림 | `docs/notification_design.md` | `.omc/plans/notification-implementation.md` |

각 설계의 확정 근거와 검토 이력은 `docs/rec/`에 있다. 컷라인과 범위는 `docs/feature_design_backlog.md`가 소유한다.

### 구현 환경

- 서버: Go(로컬 설치 완료) + PostgreSQL(Docker Compose). 저장소 구조는 `account_backend_design.md` §13.
- iOS: `src/`에 XcodeGen + SwiftUI 앱 골격. Xcode 16.2에서 빌드 성공을 확인했다.

### P1 인증 진행 상황

`feature/auth`에서 구현했다. 독립 리뷰 1회를 받아 반영했고(`2d2ef70`), 재검증에서 MERGE-READY를 받은 뒤 `main`에 fast-forward로 병합했다. 상세와 확정한 방향은 `docs/rec/2026-09-21_1618_p1_auth.md`에 있다.

- 구현: `POST /v1/auth/apple|refresh|logout`, 인증 미들웨어, `GET /v1/me`, `GET /v1/devices`, `request_id` 미들웨어(L-6), §6 오류 응답, DB 역할 분리 자동 테스트, README Go 버전 표기(L-4)
- 통합 테스트는 api 런타임 역할로 접속한다. 원격 CI: 리뷰 전 `db04442` run `35572217145` PASS 152·SKIP 0, 리뷰 반영 `2d2ef70` run `35572702395` PASS 166·SKIP 0(KMS 기동 거부 스텝 포함)
- **잠금 규약: `users` → `devices` → `sessions`, 그리고 `users`·`devices` row는 `FOR UPDATE`가 아니라 `FOR NO KEY UPDATE`로 잠근다.** mutation helper의 `idempotency_keys` INSERT가 FK 검사로 두 row에 KEY SHARE를 먼저 걸기 때문이다(`docs/rec/2026-09-21_1651_mutation_helper.md`). 계정 삭제도 이 규약을 따라야 한다

**P1에서 의도적으로 미룬 것 (잊으면 안 되는 것):**

- **KMS 구현.** 이것이 없으면 api는 `APP_ENV=local`이 아닐 때 기동을 거부한다. 스테이징·프로덕션 배포는 여기에 막혀 있다. 구현할 때 api 역할은 암호화 권한만 갖고, 복호화는 삭제 worker만 할 수 있어야 한다(§11)
- 계정 삭제 요청(`DELETE /v1/me`)과 §10 삭제 파이프라인. worker 없이 넣으면 계정이 `deletion_requested`에 갇힌다. 위 잠금 규약을 지키고, 상태 변경과 세션 전체 폐기를 한 트랜잭션에서 한다
- 인증 rate limit (§11). 인증 없는 로그인 실패마다 `audit_events`가 쌓이는 문제도 여기서 막는다
- 만료·폐기된 `sessions` row와 만료된 `idempotency_keys` row 정리 (scheduler)
- OpenAPI 스키마와 실제 응답의 자동 대조 (§15 API 계약 테스트)
- 로그인·refresh와 계정 상태 변경의 동시성 테스트(`FOR SHARE OF u` 회귀 감지). 계정 삭제와 함께
- Apple 서버 쪽 오류(`invalid_client` 등)가 HTTP 500과 `apple_error` 로그 필드로 나가는지 보는 HTTP 계층 테스트. 서비스 계층 테스트만 있다

### mutation helper 진행 상황

`feature/mutation-helper`에서 구현했다. 상세는 `docs/rec/2026-09-21_1651_mutation_helper.md`에 있다.

- `internal/platform/mutation`: 멱등성(키 선점, 지문 = method + 실제 경로 + 원본 본문), version 충돌 409, sync 변경 ordinal 발급, outbox. 교착·직렬화 실패는 최대 3회 재시도하고 `mutation.Retries()`로 노출한다. **0이 아니면 잠금 규약이 깨진 것이다**
- `PATCH /v1/me`, `DELETE /v1/devices/{id}`, `PUT /v1/devices/{id}/push-token`
- 00002: push token과 APNs environment 짝 제약
- **sync 변경은 쓰기만 있고 읽는 곳이 없다.** settled horizon 읽기와 §15 순서 역전 회귀 테스트는 sync 구현 때 반드시 함께 한다

### P0에서 남은 것

리뷰 지적 중 남은 Low:

| # | 내용 | 비고 |
|---|---|---|
| L-1 | readiness·worker 실패 로그에 DB 사용자명·DB명·host·port가 남는다 | `migrate.go`의 연결 실패 경로도 같다. 함께 본다 |
| L-3 | goose `StatementBegin/End`가 308줄 마이그레이션 전체를 한 문장으로 감싼다 | 진단 비용 |
| L-5 | `Pinger` interface가 소비자(`httpapi`)가 아니라 제공자(`postgres`)에 있다 | 기능 영향 없음. P1 새 코드는 소비자 쪽에 뒀다 |

- **§7.1의 settled horizon 읽기 쿼리는 미구현이다.** sync 변경 유실 방지의 핵심은 읽기 경로에 있으므로, 이것이 구현되기 전까지 개정은 절반만 적용된 상태다. §15의 순서 역전 회귀 테스트와 함께 sync 구현 때 반드시 처리한다.
- **`main`은 보호 브랜치다.** 저장소를 public으로 바꾼 뒤 required check `test`(GitHub Actions 고정), 관리자 포함 적용, force push 금지를 걸었다. `main`에 직접 커밋해 push할 수 없다. 문서 변경도 `feature/**` 브랜치에서 CI를 통과시킨 뒤 fast-forward한다(`docs/rec/2026-09-21_1558_ci_paths_filter_removal.md`)

### 최근 결정

- 설계 2 §7.1의 sync 변경 피드 순서 키를 `BIGSERIAL`에서 `(txid, ordinal)` + settled horizon 읽기로 개정했다. `BIGSERIAL`은 번호 순서와 커밋 순서를 일치시키지 않아 먼저 번호를 받고 나중에 커밋한 트랜잭션의 변경이 영구 유실됐다. 클라이언트 cursor도 opaque 문자열로 바꿨다. 상세와 탈락한 대안은 `docs/rec/2026-09-18_1719_sync_cursor_ordering_amendment.md`에 있다.
- 같은 검토에서 설계 9가 요구한 개정 3건 중 2건(§7.2 `notification_ref_key`, §7.3 App Group 공유 컨테이너)이 §17 목록에만 있고 본문에 반영되지 않은 것을 발견해 함께 적용했다.
- §10 7단계에 `auth_identities` row 물리 삭제를 명시했다. 삭제된 사용자가 identity를 보유하면 같은 Apple 계정의 재가입이 영구히 막혀 §10 7단계 자신과 충돌했다. 근거와 탈락한 대안(`detached_at` + partial unique)은 `docs/rec/2026-09-18_1756_apple_subject_resignup_fix.md`에 있다.

## 다음 작업

1. sync 읽기 경로(`GET /v1/sync`, bootstrap, settled horizon, 구현 계획 6단계)와 worker 골격(7단계). 그 다음 의존성 순서대로 기능별 브랜치에서 구현한다. Party membership → 공개 수준 → 캘린더 동기화 → 가능 시간 검색 → 제안·확정 → 캘린더 쓰기 → 알림.
2. 상세 설계 10(결제)과 11(운영·출시 검증)은 위 구현 진행 후 다시 우선순위를 정한다.

## 유의 사항

- 원본 MVP의 Google 연동과 결제는 장기 백로그로 보존하되 첫 출시 범위에서 제외한다.
- 기존 Swift 코드는 각 단계에서 계속 빌드·실행 가능해야 한다.
- **현재 Swift 코드는 설계 1~9와 상당히 어긋나 있다.** 알림은 iOS·서버 양쪽에 전혀 없고 Notification Service Extension target도 없다. `CalendarService`에 destination calendar 선택·writer device·외부 쓰기 명령이 없다. hidden이 계산 입력에서도 제거되고, 활동 시간·시간대·검색 조건 모델이 없으며, Party가 모든 멤버의 공개 설정을 보유하고 proposal이 단순 응답 map의 `isConfirmed` 계산만 쓴다. 전체 차이는 각 설계의 "현재 구현 차이" 절에 있다.
- 현재 앱 데이터는 메모리에만 있어 재실행 시 초기화된다.
- 구체 Go 라이브러리와 버전(pgx major, 마이그레이션 러너)은 설계 2 §3.2에 따라 착수 시점에 공식 지원 상태를 확인해 잠근다. 기억으로 고정하지 않는다.
