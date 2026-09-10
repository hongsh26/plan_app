# Party별 공개 수준 구현 계획

기준 설계: `docs/party_visibility_design.md`

## 전제

- Party 서버 골격과 `.omc/plans/party-membership-implementation.md`의 P0·P1이 선행한다.
- 계산 입력은 private Busy facts, Party 화면 입력은 projection으로 분리한다.
- 기존 Swift 앱은 각 단계에서 계속 빌드·실행 가능해야 한다.
- 구현 브랜치는 `feature/party-visibility`를 사용한다. membership P1과 같은 시기에 구현하면 해당 브랜치에서
  visibility schema를 먼저 합의하고 중복 migration을 만들지 않는다.

## 단계

### V0. 회귀 계약 고정

- 현재 hidden이 계산 BusyInterval을 제거하는 동작을 드러내는 테스트 추가
- API/sync/log allowlist fixture와 금지 키 스캔 준비
- 완료 기준: 기존 동작과 목표 동작의 차이가 실패 테스트로 재현된다

### V1. 스키마와 projection service

- `party_visibility_settings`와 `party_schedule_projections`를 설계 §6대로 구현
- private fact → 시간 projection diff, snapshot staging 상세 allowlist → details projection 복사, safe downgrade,
  source generation·setting version guard 구현
- 완료 기준: PostgreSQL constraint와 전이별 projection 통합 테스트 통과

### V2. Visibility self API

- `GET/PATCH /v1/parties/{party_id}/visibility/me`
- 활성 멤버 권한, expected version, idempotency, 분당 변경 제한
- 완료 기준: 본인·타인·비멤버·과거 멤버 계약 테스트 통과

### V3. Sync와 캐시 삭제

- 본인 전용 setting sync, 공개 수준 변경 시 활성 멤버 대상 projection upsert/tombstone
- 탈퇴·강퇴·해산 시 전이 직전 멤버와 떠난 사용자까지 포함하는 cleanup tombstone
- cursor 만료 Party scope replacement와 탈퇴·로그아웃 캐시 삭제
- 완료 기준: hidden 하향 후 offline 기기 복귀 E2E에서 상세 잔존 0건

### V4. 계산 경계 분리

- availability/conflict engine이 projection이 아닌 private Busy facts만 읽게 한다
- 전원 공통 결과에서 인원·원인 필드 제거
- 완료 기준: 같은 facts의 details/busyOnly/hidden 결과가 동일하다

### V5. iOS 설정 화면

- 본인 `PartyVisibilitySetting`, 세 수준 설명, location opt-in, details pending 상태
- `Party.visibilityByMember`와 다른 멤버 연결 상태 보유 제거
- 완료 기준: UI 상태 테스트와 Swift 빌드·기존 availability 테스트 통과

### V6. Privacy·경쟁 검증

- snapshot/설정, 탈퇴/설정 경쟁 테스트
- DB/API/sync/log/trace/APNs 금지 데이터 스캔
- 완료 기준: `docs/party_visibility_design.md` §13 인수 조건 자동화

## 의존성

```text
membership P0/P1 ─▶ V0 ─▶ V1 ─▶ V2 ─▶ V3 ─▶ V4 ─▶ V5 ─▶ V6
```

V0 테스트 작성은 P0와 병행할 수 있지만 V1 이후 구현은 최종 membership schema가 필요하다.

## 이 계획에서 하지 않는 것

- 검색 활동 시간·정렬·최대 결과 수 (설계 6)
- 제안·응답·확정 상태 전이 (설계 7)
- 캘린더 쓰기와 알림 정책 (설계 8~9)
- 일정별·멤버별 공개 예외와 Google Calendar
