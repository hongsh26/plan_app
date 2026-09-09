# 초기 출시 범위 확정

## 확정 내용

- `Internal Alpha`는 로컬/테스트 사용자와 Apple Calendar 읽기를 이용해 Party → 공통 시간 → 제안 → 전원 수락 → 앱 내부 확정 흐름을 검증한다.
- `Launch MVP`는 Apple 로그인, 서버 동기화, 실제 Party 초대, 공개 설정, 공통 시간, 제안·응답·확정, Apple Calendar 쓰기·변경·취소, 필수 푸시 알림, 계정 삭제를 포함한다.
- `Post-MVP`는 Google/이메일 로그인, Google Calendar, 부분 가능 시간, 반복 약속, 결제·Premium, 고급 알림, Widget, 추천 기능을 포함한다.

## 결정 이유

- 첫 출시에서 핵심 가치인 “여러 사용자가 실제 캘린더를 기반으로 약속을 잡고 반영하는 흐름”을 완결한다.
- Google 연동과 결제의 인증·동기화·복구 복잡도를 첫 출시의 필수 경로에서 분리한다.
- 원본 설계의 기능 목표는 삭제하지 않고 후속 제품 백로그로 보존한다.

## 다음 설계

- 계정·서버·기기 간 데이터 소유권
- 이후 캘린더 동기화와 Party 정책을 순차적으로 설계한다.

## 검증

- `docs/feature_design_backlog.md`, `docs/progressing.md`, `docs/implementation_plan.md`에 동일한 컷라인을 반영한다.
- 별도 검토 에이전트가 구현 계획에 남아 있던 두 번째 캘린더 제공자와 결제 단계를 발견했다.
- Google Calendar와 결제를 명시적인 Post-MVP 단계로 이동하고 Launch MVP 제품 검증에서 결제 지표를 분리했다.
- 수정된 문서 간 일관성을 다시 검증한다.
