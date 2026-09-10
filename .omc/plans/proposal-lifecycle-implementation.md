# 제안·응답·확정 상태 전이 구현 계획

기준 설계: `docs/proposal_lifecycle_design.md`

## 전제

- 사용자 결정에 따라 상세 설계 9까지 완료한 뒤 구현을 시작한다.
- Party 서버 골격·membership·calendar private facts·availability calculationVersion이 선행한다.
- 기존 Swift 데모는 각 단계에서 계속 빌드·실행 가능해야 한다.
- 구현 브랜치는 `feature/proposal-lifecycle`을 사용한다.

## 단계

### Q0. 상태 전이 계약 테스트

- proposal/response 상태 table, 종단 상태 역전이 금지
- 탈퇴·해산·기한·재제안과 freshness 결과 fixture
- 현재 `isConfirmed` 즉시 계산 동작과 목표 차이를 실패 테스트로 고정
- 완료 기준: 설계 §11 핵심 상태 전이가 테스트로 표현되고 목표 테스트가 구현 전 실패한다

### Q1. 스키마와 reservation

- `proposals`, `proposal_participants`, `confirmed_events`, `confirmed_event_participants` migration
- version/check/unique/FK와 활성 reservation overlap guard
- domain outbox·sync entity type 확장
- 완료 기준: 중복 participant/confirmed event와 겹치는 active reservation이 DB에서 거부된다

### Q2. 제안 생성·조회

- availability/custom source validation, active membership snapshot과 creator accepted
- create/list/detail API, idempotency와 권한
- 신규 가입자의 읽기 전용 projection과 탈퇴자 tombstone
- 완료 기준: stale calculationVersion·hidden Busy custom 충돌·비멤버 접근 계약 테스트 통과

### Q3. 응답과 자동 확정

- 본인 response PUT, expected proposal/response version
- 전원 accepted 판정, 15분 freshness, 2분 refresh attempt
- private facts + active reservation 충돌 검사와 confirmed event 원자 생성
- 완료 기준: 마지막 동시 수락과 서로 다른 Party 동시 확정 테스트에서 중복·겹침이 없다

### Q4. Membership·종단 전이

- membership 종료 transaction의 `reproposal_required`
- Party 해산, scheduler 만료, 생성자/owner 취소
- immutable-body 재제안과 supersede link
- 완료 기준: 상태 변경과 sync/outbox가 같은 transaction에서 커밋되고 종단 상태가 재개되지 않는다

### Q5. Sync·Outbox·Privacy

- proposal/response/confirmed event projection과 delta/bootstrap
- domain outbox type·dedupe key 발행
- response/error/sync/log/trace/APNs fixture allowlist 검사
- 완료 기준: 떠난 사용자 캐시 정리와 민감정보 비노출 회귀 통과

### Q6. iOS 화면과 오프라인 큐

- 작성, 응답, freshness 대기, conflict/reproposal/expired/superseded 상태
- 가입 전 생성 제안 읽기 전용 표시
- 생성·응답 offline queue와 stale conflict 복구
- 현재 `EventProposal.isConfirmed` 제거, 서버 projection 상태 사용
- 완료 기준: Swift 빌드·단위/UI 상태 테스트와 offline replay 시나리오 통과

### Q7. E2E 인수 조건

- 3명 제안 → 응답 변경 → 전원 수락 → 앱 내부 확정
- stale refresh timeout/retry, hidden Busy 충돌, 탈퇴 후 재제안
- 동시 확정, idempotency replay와 privacy payload scan
- 완료 기준: `docs/proposal_lifecycle_design.md` §11 체크리스트 자동화

## 의존성

```text
상세 설계 8~9 완료
        │
P0/P1 + availability A3/A4 + calendar freshness
        └─▶ Q0 ─▶ Q1 ─▶ Q2 ─▶ Q3 ─▶ Q4 ─▶ Q5 ─▶ Q6 ─▶ Q7
```

Q0의 순수 상태 테스트와 iOS projection 모델 초안은 서버 선행 작업과 병행할 수 있지만, 사용자 결정에 따라 실제 구현 착수는 상세 설계 9 완료 뒤로 제한한다.

## 이 계획에서 하지 않는 것

- EventKit create/update/delete와 확정 일정 변경·취소
- 실제 APNs 문구·전송 정책
- 일부 참여자 선택, 최소 인원과 복수 후보 투표
- 반복 약속, 메모·첨부와 결제 권한
