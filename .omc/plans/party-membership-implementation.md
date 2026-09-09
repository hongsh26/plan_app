# Party 생성·초대·역할·탈퇴 구현 계획

기준 설계: `docs/party_membership_design.md`
선행 설계: `docs/account_backend_design.md`, `docs/calendar_privacy_sync_design.md`

## 전제

- 서버(Go + PostgreSQL)를 먼저 구현하고 iOS를 뒤에 붙인다.
- 서버 저장소는 아직 없다. P0에서 `account_backend_design.md` §13 저장소 구조를 실제로 만든다.
- 기존 Swift 코드는 각 단계에서 계속 빌드·실행 가능해야 한다.
- 브랜치: `feature/party-membership`에서 작업하고 완료 후 `main`에 병합한다.

## 단계

### P0. 서버 골격 (선행 조건)

- Go 모듈, `cmd/api`, 마이그레이션 러너, 로컬 PostgreSQL 실행 방법
- `users`, `auth_identities`, `devices`, `sessions`, `sync_changes`, `idempotency_keys`, `outbox_jobs` 최소 마이그레이션
- 완료 기준: 로컬에서 마이그레이션이 적용되고 헬스체크가 응답한다

### P1. 스키마

- `parties`: `member_limit`, `created_by_user_id`, `disbanded_at` 추가
- `party_memberships`: `joined_at`, `ended_at`, `end_reason`, `invite_id` 추가
- `party_invites`: `created_by_membership_id`, `revoked_at`, `last_used_at` 추가, `token_hash`를 NULL 허용으로 변경
- `party_visibility_settings`와 `party_schedule_projections` 최소 스키마 생성. 설계 5 확정 전이므로 P2·P3의 insert와 P5의 delete에 필요한 컬럼만 만든다
- partial unique index 3종
  - `(party_id, user_id) WHERE status='active'`
  - `(party_id) WHERE role='owner' AND status='active'`
  - `party_invites(token_hash) WHERE token_hash IS NOT NULL`
- `parties.owner_membership_id` FK를 `DEFERRABLE INITIALLY DEFERRED`로 선언
- check constraint: `used_count <= max_uses`, `member_limit BETWEEN 1 AND 10`, enum 허용값
- 지연 검증 `CONSTRAINT TRIGGER` 3종 (설계 §5.1 검증 시점): active Party의 소유권 3조건, active Party의 활성 멤버 1명 이상, disbanded Party의 활성 하위 row 0건
- 완료 기준: 통합 테스트가 두 번째 활성 멤버십과 두 번째 활성 방장 삽입을 거부하고, T1·T7이 중간 상태에서 막히지 않고 커밋된다

### P2. Party 생성·조회·수정

- `POST /v1/parties`, `GET /v1/parties/{id}`, `PATCH /v1/parties/{id}`, `GET /v1/parties/{id}/memberships`
- 이름 정규화·검증 (설계 §4.1), 소유 Party 한도 검사 (§4.3)
- T1 트랜잭션과 `sync_changes` 발행. 생성자의 `busyOnly` projection 생성 포함(설계 §5.5.1)
- 완료 기준: 권한 표(§6)의 조회·수정 행이 계약 테스트를 통과한다

### P3. 초대

- `POST/GET/DELETE /v1/parties/{id}/invites`, `GET/POST /v1/invites/{token}/*`
- token 생성과 SHA-256 저장, 원문 1회 반환
- 만료·소진·무효 판정을 `404 not_found`로 통일
- T3 수락 트랜잭션: `parties` → 초대 순 잠금(설계 §5.5 잠금 순서), 정원 검사, 가입 시 `busyOnly` projection 생성(§5.5.1)
- 요청 제한: 생성 Party당 시간당 10회, 수락 사용자당 분당 5회, 미리보기 사용자당 분당 10회
- Universal Link AASA 호스팅과 앱 미설치 웹 폴백 페이지
- 완료 기준: 동시 수락 경쟁 통합 테스트에서 정원 초과가 발생하지 않는다

### P4. 소유권 위임

- `POST /v1/parties/{id}/owner`
- T4 트랜잭션, 활성 초대 일괄 `revoked`, 커밋 전 소유권 불변식 재검증
- 완료 기준: 동시 위임 요청 중 하나만 성공하고 방장이 항상 정확히 1명이다

### P5. 탈퇴·강퇴·해산

- `DELETE /v1/parties/{id}/memberships/me`, `DELETE .../{user_id}`, `POST /v1/parties/{id}/disband`
- T5·T6·T7 트랜잭션
- `party_schedule_projections`, `party_visibility_settings` 삭제와 tombstone 발행
- 단독 방장 탈퇴 → 해산 자동 전환
- 완료 기준: Privacy 회귀 테스트에서 탈퇴 후 projection 0건, 양쪽 tombstone 확인

### P6. 동기화 전파

- entity type 6종 발행(`party`, `party_membership`, `party_invite`, `party_visibility_setting`, `party_schedule_projection`, `proposal`), 신규 멤버 Party 단위 부트스트랩
- `party_invite` 변경은 현재 방장에게만. 위임 무효화 tombstone만 직전 방장에게도 전달
- 완료 기준: 두 세션에서 멤버 변경이 cursor 증분으로 정확히 반영된다

### P7. 알림 job

- outbox job type 6종 발행과 dedupe key
- 실제 APNs 전송 정책은 설계 9 이후
- 완료 기준: 각 전이가 정확히 하나의 job을 만들고 재시도로 중복되지 않는다

### P8. 계정 삭제 연동

- 삭제 작업에 강제 위임·해산 단계 추가
- 승계자 결정 규칙: 삭제 대상 본인과 `users.status != 'active'`인 사용자를 제외한 활성 멤버 중 `joined_at`, `user_id` 오름차순. 후보가 없으면 해산
- 완료 기준: 방장 계정 삭제 후 Party가 유지되고 방장이 1명이다

### P9. iOS

- `Membership`, `PartyInvite`, `PartyStatus` 타입 신설, `Party` 구조 변경
- `AvailabilitySlot.availableMemberCount`와 `totalMemberCount` 제거
- `visibilityByMember`를 본인 전용 설정으로 분리
- Universal Link 처리와 초대 미리보기·수락 화면
- 오프라인 큐 허용/금지 정책 (§8.2)
- 완료 기준: E2E 시나리오 4종 통과

### P10. 인수 조건 자동화

- 설계 §10 체크리스트를 테스트로 옮기고 CI에서 실행

## 의존성

```text
P0 ─▶ P1 ─▶ P2 ─▶ P3 ─▶ P4 ─▶ P5 ─▶ P6 ─▶ P7
                                └─▶ P8
P6 ─▶ P9 ─▶ P10
```

P8은 P5 완료 후 P6과 병행 가능하다.

## 이 계획에서 하지 않는 것

- 제안·응답·확정 상태 전이 (설계 7)
- 캘린더 쓰기 명령 (설계 8)
- 알림 문구와 전송 정책 (설계 9)
- 결제와 요금제 한도 (Post-MVP)
