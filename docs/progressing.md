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

### 최근 결정

- 설계 2 §7.1의 sync 변경 피드 순서 키를 `BIGSERIAL`에서 `(txid, ordinal)` + settled horizon 읽기로 개정했다. `BIGSERIAL`은 번호 순서와 커밋 순서를 일치시키지 않아 먼저 번호를 받고 나중에 커밋한 트랜잭션의 변경이 영구 유실됐다. 클라이언트 cursor도 opaque 문자열로 바꿨다. 상세와 탈락한 대안은 `docs/rec/2026-09-18_1719_sync_cursor_ordering_amendment.md`에 있다.
- 같은 검토에서 설계 9가 요구한 개정 3건 중 2건(§7.2 `notification_ref_key`, §7.3 App Group 공유 컨테이너)이 §17 목록에만 있고 본문에 반영되지 않은 것을 발견해 함께 적용했다.

## 다음 작업

1. **P0 서버 골격.** `feature/server-skeleton` 브랜치에서 진행하고 완료 후 main에 병합한다. P0는 설계 4~9 전체의 공통 선행 조건이라 기능 브랜치에 두지 않는다.
   - `server/` Go 모듈, `cmd/api`·`cmd/worker`·`cmd/scheduler` 세 진입점, 설정 검증, graceful shutdown
   - liveness(DB 비의존)와 readiness(DB 의존) 두 endpoint를 분리한다
   - 마이그레이션 8개 테이블: `users`, `auth_identities`, `devices`, `sessions`, `sync_changes`, `idempotency_keys`, `outbox_jobs`, `audit_events`
   - `devices`는 설계 9의 알림 열 3개를 지금 함께 만든다(두 번째 마이그레이션 방지)
   - 상태 문자열은 PostgreSQL enum이 아니라 check constraint로 둔다. 이후 설계가 계속 값을 추가하므로 `ALTER TYPE` 마찰을 피한다
   - `.gitignore`에 `.env`를 먼저 넣고 예시 파일만 커밋한다
   - 완료 기준: 마이그레이션이 로컬에 적용되고 liveness·readiness가 응답한다. 인증은 P0 범위가 아니다
2. P0 이후 의존성 순서대로 기능별 브랜치에서 구현한다. Party membership → 공개 수준 → 캘린더 동기화 → 가능 시간 검색 → 제안·확정 → 캘린더 쓰기 → 알림.
3. 상세 설계 10(결제)과 11(운영·출시 검증)은 위 구현 진행 후 다시 우선순위를 정한다.

## 유의 사항

- 원본 MVP의 Google 연동과 결제는 장기 백로그로 보존하되 첫 출시 범위에서 제외한다.
- 기존 Swift 코드는 각 단계에서 계속 빌드·실행 가능해야 한다.
- **현재 Swift 코드는 설계 1~9와 상당히 어긋나 있다.** 알림은 iOS·서버 양쪽에 전혀 없고 Notification Service Extension target도 없다. `CalendarService`에 destination calendar 선택·writer device·외부 쓰기 명령이 없다. hidden이 계산 입력에서도 제거되고, 활동 시간·시간대·검색 조건 모델이 없으며, Party가 모든 멤버의 공개 설정을 보유하고 proposal이 단순 응답 map의 `isConfirmed` 계산만 쓴다. 전체 차이는 각 설계의 "현재 구현 차이" 절에 있다.
- 현재 앱 데이터는 메모리에만 있어 재실행 시 초기화된다.
- 구체 Go 라이브러리와 버전(pgx major, 마이그레이션 러너)은 설계 2 §3.2에 따라 착수 시점에 공식 지원 상태를 확인해 잠근다. 기억으로 고정하지 않는다.
