# 진행 상황

## 현재

**설계 단계는 끝났다. 상세 설계 1~9가 확정됐고 각각 구현 계획이 `.omc/plans/`에 1:1로 있다. 요청 제한 설계(횡단 인프라)는 초안이다. 지금은 구현 단계다.**

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
| 횡단 요청 제한 | `docs/rate_limit_design.md` | `.omc/plans/rate-limit-implementation.md` |

각 설계의 확정 근거와 검토 이력은 `docs/rec/`에 있다. 컷라인과 범위는 `docs/feature_design_backlog.md`가 소유한다.

### 구현 환경

- 서버: Go(로컬 설치 완료) + PostgreSQL(Docker Compose). 저장소 구조는 `account_backend_design.md` §13.
- iOS: `src/`에 XcodeGen + SwiftUI 앱 골격. Xcode 16.2에서 빌드 성공을 확인했다.

### 병합 완료된 서버 기반 (요약, 상세는 `docs/rec/`)

인증(`2026-09-21_1618_p1_auth.md`), mutation helper(`..._1651_mutation_helper.md`), sync 읽기(`..._1742_sync_read_path.md`), worker·scheduler(`2026-09-22_1154_worker_scheduler_skeleton.md`)는 `main`에 있다. 지켜야 할 규약만 남긴다.

- **잠금 규약: `users` → `devices` → `sessions`, 그리고 `users`·`devices` row에는 `FOR UPDATE`를 쓰지 않는다(읽기 `FOR SHARE`, 수정 `FOR NO KEY UPDATE`).** mutation helper의 `idempotency_keys` INSERT가 FK 검사로 두 row에 KEY SHARE를 먼저 걸기 때문이다. `mutation.Retries()`가 0이 아니면 규약이 깨진 것이다
- 경보 규칙이 잡을 로그 값: `result=dead`, `result=dead_failed`, `result=stalled`. handler·작업 기한은 lease보다 7초 짧다(`WORKER_LEASE_DURATION`·`Task.Timeout` 최솟값 17s)
- sync 읽기는 primary에서 해야 한다. 파일시스템 스냅샷 복구는 cursor 세대로 감지되지 않으므로 운영 절차로 전체 재동기화를 강제한다
- **`main`은 보호 브랜치다.** required check `test`, 관리자 포함, force push 금지. 문서 변경도 `feature/**`에서 CI를 통과시킨 뒤 fast-forward한다

**아직 남은 것 (잊으면 안 되는 것):**

- **KMS 구현.** 없으면 api는 `APP_ENV=local`이 아닐 때 기동을 거부한다. 스테이징·프로덕션 배포가 막혀 있다. api 역할은 암호화만, 복호화는 삭제 worker만(§11)
- OpenAPI 스키마와 실제 응답의 자동 대조(§15)
- 로그인·refresh와 계정 상태 변경의 동시성 테스트(`FOR SHARE OF u` 회귀 감지)
- Apple 서버 오류(`invalid_client` 등)가 HTTP 500과 `apple_error` 로그 필드로 나가는지 보는 HTTP 계층 테스트

### 계정 삭제와 Party의 공백 (지금 살아 있는 결함)

migration 00010으로 Party 테이블이 생겼지만 삭제 worker(`internal/account/deletion.go`)는 아직 sessions·devices·auth_identities·audit_events·users만 건드린다. 그래서 **활성 멤버십을 가진 사용자가 `status='deleted'`까지 가면서 Party의 활성 멤버로, 경우에 따라 활성 방장으로 남는다.**

DB는 이것을 막지 못한다. 지연 트리거 3종은 Party 안의 정합성만 보고 `users.status`를 보지 않으며, 삭제 파이프라인은 `users` row를 물리 삭제하지 않아 FK RESTRICT도 걸리지 않는다. 현재 테스트가 통과하는 이유는 계정 삭제 테스트에 Party row가 없기 때문이지 이 경로가 안전해서가 아니다.

설계 §5.8과 계획 P8(강제 위임·해산)이 이 공백의 주인이다. **P8을 P5 이후로 미루더라도 이 결함은 그때까지 살아 있다.** 승계자 결정 규칙은 계획 P8에 있다.

### Party P1~P4, 요청 제한 완료

스키마(00010), API(생성·조회·수정·멤버 목록), 초대(P3), 소유권 위임(P4, `POST /v1/parties/{id}/owner`), 공용 요청 제한(00011)을 구현했다. 상세와 설계 편차는 `docs/rec/`의 `..._09-29_1415_party_membership_schema`, `..._09-30_1621_party_invites`, `..._09-30_1710_rate_limit_impl`, `..._09-30_1720_party_owner_transfer`.

- 이름 길이는 40 **rune** 기준이다. 초대 재전송은 token 원문을 다시 주지 못한다(무효화 후 새로 만든다)
- **`SET CONSTRAINTS ALL IMMEDIATE`는 남은 트랜잭션 전체의 검사 시점을 바꾼다.** 지연 검증 테스트에서는 트랜잭션당 한 번, 마지막에만 부른다. `CREATE CONSTRAINT TRIGGER` 이름이 63자를 넘으면 PostgreSQL이 조용히 자른다
- **새 소비자는 `Require` 다음, 모든 입력 검증·조회·`mutation.Prepare` 앞에서 `ratelimit.Enforce`를 호출한다.** 트랜잭션 안에서 세면 롤백과 함께 카운트가 사라진다. 한도는 `internal/platform/ratelimit/limits.go` 한 곳
- **배포 전:** `CLIENT_IP_SOURCE`(비로컬 필수)·`TRUSTED_PROXY_HOPS`·`RATE_LIMIT_KEY`를 설정하고 실제 헤더로 hops를 검증한다. `client_ip_fallback`·`limiter_error` 로그를 경보에 건다
- **iOS 계약:** refresh 429는 세션 상실이 아니다. `Retry-After` 뒤 재시도하며 로그아웃하지 않는다
- **남은 것:** AASA 호스팅과 웹 폴백(`invite.web` 제한 포함), `notify_member_joined`·`notify_owner_transferred` worker kind(job은 pending으로 쌓인다), 가입 시 busy projection(`calendar_busy_facts` 없음), 공개 수준·검색·제안의 요청 제한 소비자, 초대·Party endpoint의 OpenAPI 등재

### P0에서 남은 것

리뷰 지적 중 남은 Low:

| # | 내용 | 비고 |
|---|---|---|
| L-1 | readiness·worker 실패 로그에 DB 사용자명·DB명·host·port가 남는다 | `migrate.go`의 연결 실패 경로도 같다. 함께 본다 |
| L-3 | goose `StatementBegin/End`가 308줄 마이그레이션 전체를 한 문장으로 감싼다 | 진단 비용 |
| L-5 | `Pinger` interface가 소비자(`httpapi`)가 아니라 제공자(`postgres`)에 있다 | 기능 영향 없음. P1 새 코드는 소비자 쪽에 뒀다 |

- **`main`은 보호 브랜치다.** 저장소를 public으로 바꾼 뒤 required check `test`(GitHub Actions 고정), 관리자 포함 적용, force push 금지를 걸었다. `main`에 직접 커밋해 push할 수 없다. 문서 변경도 `feature/**` 브랜치에서 CI를 통과시킨 뒤 fast-forward한다(`docs/rec/2026-09-21_1558_ci_paths_filter_removal.md`)

### 최근 결정

- 설계 2 §7.1의 sync 변경 피드 순서 키를 `BIGSERIAL`에서 `(txid, ordinal)` + settled horizon 읽기로 개정했다. `BIGSERIAL`은 번호 순서와 커밋 순서를 일치시키지 않아 먼저 번호를 받고 나중에 커밋한 트랜잭션의 변경이 영구 유실됐다. 클라이언트 cursor도 opaque 문자열로 바꿨다. 상세와 탈락한 대안은 `docs/rec/2026-09-18_1719_sync_cursor_ordering_amendment.md`에 있다.
- 같은 검토에서 설계 9가 요구한 개정 3건 중 2건(§7.2 `notification_ref_key`, §7.3 App Group 공유 컨테이너)이 §17 목록에만 있고 본문에 반영되지 않은 것을 발견해 함께 적용했다.
- §10 7단계에 `auth_identities` row 물리 삭제를 명시했다. 삭제된 사용자가 identity를 보유하면 같은 Apple 계정의 재가입이 영구히 막혀 §10 7단계 자신과 충돌했다. 근거와 탈락한 대안(`detached_at` + partial unique)은 `docs/rec/2026-09-18_1756_apple_subject_resignup_fix.md`에 있다.

## 다음 작업

**목표: 핵심 가치(공통 가능 시간 찾기)를 보여주는 시연.** 사용자가 정한 순서다. 시연에 필요 없는 P5·P6~P8·제안·캘린더 쓰기·알림은 뒤로 미루되, P8(계정 삭제 연동)은 출시 전 필수다.

1. ✅ 개발용 로그인(A): `DEV_LOGIN_ENABLED=true`(로컬 전용)에서 identity_token `dev:<이름>`. `server/scripts/`의 `run_api_dev.sh`, `demo_api.sh`로 서버 API 시연 가능
2. **캘린더 동기화 서버**(설계 3, `calendar_busy_facts` 테이블과 busy 업로드·projection 생성). T1·T3의 busy projection stub이 여기서 채워진다
3. **공개 수준**(설계 5, `.omc/plans/party-visibility-implementation.md`)
4. **가능 시간 검색 서버**(설계 6, `.omc/plans/availability-search-implementation.md`)
5. **iOS 연동**: API 클라이언트, 로그인, Party·초대·캘린더 권한·가능 시간 화면. 기존 Swift는 설계와 크게 어긋나 새로 연결한다. 디자인은 `docs/design_references.md`

## 유의 사항

- 원본 MVP의 Google 연동과 결제는 장기 백로그로 보존하되 첫 출시 범위에서 제외한다.
- 화면 설계 때 참고할 외부 디자인 레퍼런스는 `docs/design_references.md`에 모은다(사용자가 추가로 제공할 예정).
- 기존 Swift 코드는 각 단계에서 계속 빌드·실행 가능해야 한다.
- **현재 Swift 코드는 설계 1~9와 상당히 어긋나 있다.** 알림은 iOS·서버 양쪽에 전혀 없고 Notification Service Extension target도 없다. `CalendarService`에 destination calendar 선택·writer device·외부 쓰기 명령이 없다. hidden이 계산 입력에서도 제거되고, 활동 시간·시간대·검색 조건 모델이 없으며, Party가 모든 멤버의 공개 설정을 보유하고 proposal이 단순 응답 map의 `isConfirmed` 계산만 쓴다. 전체 차이는 각 설계의 "현재 구현 차이" 절에 있다.
- 현재 앱 데이터는 메모리에만 있어 재실행 시 초기화된다.
- 구체 Go 라이브러리와 버전(pgx major, 마이그레이션 러너)은 설계 2 §3.2에 따라 착수 시점에 공식 지원 상태를 확인해 잠근다. 기억으로 고정하지 않는다.
