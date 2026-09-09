# Apple Calendar 권한·동기화·공개 설계 완료

## 확정 내용

- EventKit 원본, private Busy facts, Party-visible projections를 분리한다.
- hidden 일정은 projection을 만들지 않지만 공통 시간, 임의 제안과 확정 직전 충돌 검사에는 Busy로 반영한다.
- 다른 멤버에게 hidden 일정의 ID, 소유자, 시간과 원인을 반환하지 않는다.
- 사용자가 명시적으로 선택한 source calendar만 읽고 writer calendar를 별도로 선택한다.
- Launch MVP는 rolling 90일 snapshot, 검색 6시간/확정 15분 freshness gate를 사용한다.
- 반복 일정은 EventKit occurrence로, tentative는 Busy, free는 제외한다.
- EventKit 쓰기는 지정 iOS 기기가 실행하며 서버는 command/lease/result를 관리한다.

## 산출물

- 상세 설계: `docs/calendar_privacy_sync_design.md`
- 구현 계획: `.omc/plans/calendar-privacy-sync-implementation.md`

## 기존 구현과의 차이

- 현재 코드는 hidden 일정 자체를 계산 입력에서 제거하므로 구현 시 변경이 필요하다.
- 현재는 모든 캘린더를 7일 범위로 읽고 source 선택, 안정적인 mapping, snapshot, write command가 없다.

## 검증

- 요구사항 분석에서 hidden 추론 위험, 권한 상태, snapshot 원자성, 반복/종일/timezone, writer-device 실패를 점검했다.
- 별도 검토 에이전트가 설계와 문서 일관성을 재검증한다.

## 다음 작업

- Party 생성·초대·역할·탈퇴 상세 설계
