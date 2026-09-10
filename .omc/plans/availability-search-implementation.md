# 공통 가능 시간 검색 구현 계획

기준 설계: `docs/availability_search_design.md`

## 전제

- Party 서버 골격, membership과 calendar private Busy facts가 선행한다.
- 공개 수준과 Party projection은 계산 입력으로 사용하지 않는다.
- 기존 Swift 계산 테스트를 먼저 보존하고 목표 동작을 실패 테스트로 추가한다.
- 구현 브랜치는 `feature/availability-search`를 사용한다.

## 단계

### A0. 계산 계약 테스트

- 활동 시간 차집합·멤버 교집합·반개구간 경계 테스트
- 30분 grid, 날짜당 3개·전체 20개 제한, DST·시간대 테스트
- 현재 count 필드와 무설정/미동기 빈 배열 동작의 교체 지점을 고정
- 완료 기준: 기존 4개 테스트를 보존하고 새 목표가 실패 테스트로 재현된다

### A1. 활동 시간 저장·API

- `availability_preferences`, `availability_weekly_intervals` migration
- 본인 GET/PUT, 정규화, version·멱등성·overlap 제약
- 완료 기준: 주간 규칙 전체 교체와 경쟁 통합 테스트 통과

### A2. 순수 interval 엔진

- IANA time zone별 주간 규칙 UTC 확장
- Busy 병합, 활동 시간 차집합, 전원 교집합과 30분 후보 생성
- 완료 기준: cross-time-zone, DST, 자정 횡단, all-day fixture 통과

### A3. 검색 snapshot과 freshness

- REPEATABLE READ version tuple, 모든 generation window UTC 교집합, 6시간 `fresh_until`, aggregate pending 상태
- opaque `calculationVersion` 생성과 stale 검증
- 완료 기준: 상태 변경 경쟁·window 경계·freshness 시간 경과에서 부분·혼합·만료 결과를 반환하지 않는다

### A4. Party 검색 API·캐시

- `/v1/parties/{id}/availability/search`, 입력 한도와 rate limit
- 날짜당 3개·전체 max 결과 정렬, 5분 version-key cache
- 완료 기준: 권한·입력·ready/pending/empty API 계약 테스트 통과

### A5. iOS 설정·검색·결과

- 활동 시간 설정, 기본 14일·60분 검색 form
- 날짜별 결과, 준비 대기와 정상 빈 결과 상태 분리
- `AvailabilitySlot` count 제거와 calculationVersion 전달
- 완료 기준: Swift 빌드, 기존/신규 단위 테스트와 UI 상태 테스트 통과

### A6. Privacy·E2E

- 다른 멤버의 활동 시간·준비 원인 비노출
- 로그·trace·분석·APNs 금지 필드 스캔
- 결과 만료 → 재검색 → 제안 진입 E2E
- 완료 기준: `docs/availability_search_design.md` §13 인수 조건 자동화

## 의존성

```text
server/calendar prerequisites ─▶ A0 ─▶ A1 ─▶ A2 ─▶ A3 ─▶ A4 ─▶ A5 ─▶ A6
```

A0과 iOS 순수 계산 모델은 서버 P0와 병행할 수 있다. A3 이후는 membership·activity·calendar version 계약이 필요하다.

## 이 계획에서 하지 않는 것

- 일부 멤버 제외와 부분 가능 availability
- Party별 활동 시간 override와 날짜별 예외
- 제안·응답·확정 상태 전이
- AI·장소·이동 시간 기반 추천
