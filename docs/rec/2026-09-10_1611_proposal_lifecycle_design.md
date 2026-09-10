# 제안·응답·확정 상태 전이 설계 완료

## 완료 내용

- 상세 설계 7을 `docs/proposal_lifecycle_design.md`에 작성했다.
- 구현 순서를 `.omc/plans/proposal-lifecycle-implementation.md`에 작성했다.
- `docs/feature_design_backlog.md`와 `docs/progressing.md`에 완료 상태와 다음 작업을 반영했다.

## 확정한 핵심 정책

- 제안 참여자는 생성 시점의 활성 Party 멤버 전원으로 고정한다.
- 생성자는 자동 수락하며 고정 참여자 전원 수락을 기본 확정 조건으로 사용한다.
- 미확정 제안의 참여자가 떠나면 정족수에서 제외하지 않고 `reproposal_required`로 종료한다.
- 제안 본문과 참여자 snapshot은 불변이며 수정은 새 proposal을 만드는 재제안으로 처리한다.
- 전원 수락 뒤 15분 freshness와 private Busy·앱 내부 reservation 충돌 검사를 통과해야 앱 내부 확정을 만든다.
- confirmed event와 reservation, proposal 상태, sync와 domain outbox는 같은 transaction에서 커밋한다.

## 후속 경계

- EventKit 쓰기, 확정 일정 변경·취소와 사용자별 반영 상태는 상세 설계 8에서 확정한다.
- 실제 알림 문구·수신자·urgency·quiet hours와 묶음 처리는 상세 설계 9에서 확정한다.
- 사용자 결정에 따라 상세 설계 9 완료 전에는 구현에 착수하지 않는다.

## 검증

- 선행 설계의 고정 참여자 snapshot, membership 종료 신호, 15분 freshness, hidden Busy와 서버 source-of-truth 불변식을 반영했다.
- 현재 Swift 코드는 변경하지 않았다.
