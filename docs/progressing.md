# 진행 상황

## 현재

- iOS 앱, SwiftUI/iOS 17, EventKit 우선 연동으로 구현 방향을 확정했다.
- `src/`에 XcodeGen 프로젝트 정의와 SwiftUI 앱 골격이 있으며 Xcode 프로젝트가 생성되었다.
- 메모리 기반 데모 Party, 공개 수준 모델, 공통 가능 시간 계산, 약속 제안, EventKit 권한 및 일정 읽기 경계를 구현했다.
- 공통 시간 계산 단위 테스트 4개를 추가했다.
- `docs/feature_design_backlog.md`에 전체 기능을 단계와 영역별로 다시 나누고 상세 설계 순서를 정리했다.
- `Internal Alpha` / `Launch MVP` / `Post-MVP` 컷라인을 확정했다. 첫 출시는 Apple 생태계의 완결된 약속 확정 흐름에 집중하고 Google 연동과 결제는 후속 범위로 둔다.
- 계정·서버·기기 간 데이터 소유권 상세 설계를 `docs/account_backend_design.md`에 작성했다.
- Go 모듈러 모놀리스, PostgreSQL, REST/OpenAPI, 사용자별 증분 동기화, PostgreSQL outbox/job 구조를 확정했다.
- 계정·서버 설계는 빠른 MVP보다 인프라 통제와 장기 확장성을 우선하며, 자체 API와 서버 소유 도메인 데이터를 기본 원칙으로 삼는다.
- Apple Calendar 권한·선택·snapshot 동기화·Party 공개·기기 실행 쓰기 명령 설계를 `docs/calendar_privacy_sync_design.md`에 작성했다.
- hidden 일정은 다른 멤버에게 존재를 직접 노출하지 않되 공통 시간과 충돌 계산에는 Busy로 반영한다.
- Party 생성·초대·역할·탈퇴 상세 설계를 `docs/party_membership_design.md`에 작성했다. 상세 설계 형식 12개 항목을 모두 채웠다.
- Party 제품 정책 3건을 확정했다. 방장은 위임 후에만 탈퇴할 수 있고, 초대는 재사용 링크 + 앱 내 계정 필수이며, 활성 멤버 상한은 방장 포함 10명이다.
- 구현 계획 문서를 `.omc/plans/`로 통합했다. 이전 `.omx/plans/` 경로의 계획 2건을 옮기고 rec 문서의 경로 표기를 맞췄다.
- Party 설계에 대해 검토 에이전트가 3회 검토했다. 1차 Critical 4건 포함 28건, 2차 High 2건 포함 11건, 3차 High 1건 포함 5건을 지적했고 전부 반영했다. 상세는 `docs/rec/2026-09-09_1700_party_membership_design.md`에 있다.

## 다음 작업

1. Party별 공개 수준과 계산 반영 규칙을 상세 설계한다. (설계 5)
2. 공통 가능 시간의 활동 시간·검색 조건·결과 정렬을 상세 설계한다. (설계 6)
3. 제안·응답·확정 상태 전이를 확정한다. 떠난 참여자가 있는 제안의 확정 조건이 여기서 결정된다. (설계 7)
4. 캘린더 쓰기·변경·취소 → 알림 → 운영 검증을 설계한다. (설계 8~9)
5. 설계가 확정된 기능부터 기능별 브랜치에서 구현하고 관련 테스트를 실행한다. Party는 `feature/party-membership`에서 `.omc/plans/party-membership-implementation.md`의 P0부터 시작한다.
6. Google 연동, 결제와 Premium 등 Post-MVP 항목은 출시 검증 이후 다시 우선순위를 정한다.

## 유의 사항

- 원본 MVP의 Google 연동과 결제 목표는 장기 제품 백로그로 보존하되 첫 출시 범위에서는 제외한다.
- 기존 코드가 추가되거나 발견되면 `src/base/` 실행 가능성을 보존한다.
- Xcode 16.2에서 프로젝트 생성과 빌드 성공을 확인했다. 한 차례 실행은 당시 누락된 빌드 번호로 실패했고 이후 설정과 산출물의 `CFBundleVersion = 1`을 확인했지만, 수정 후 실행 및 최신 단위 테스트 결과 번들은 별도로 남아 있지 않다.
- 현재 앱 데이터는 메모리에만 보관되어 재실행 시 초기화된다.
- 설계 4까지 확정된 내용과 현재 Swift 코드 사이에 차이가 있다. 특히 `AvailabilitySlot`의 `availableMemberCount`와 `totalMemberCount`는 전원 공통 슬롯만 반환한다는 확정 설계와 충돌하므로 구현 시 둘 다 제거한다. 전체 차이는 `docs/party_membership_design.md` §13에 있다.
- 서버 코드는 아직 존재하지 않는다. Party 구현은 서버 골격(P0) 없이는 시작할 수 없다.
- `docs/implementation_plan.md`에 "현재 체크아웃 브랜치는 master"라고 적혀 있으나 실제 브랜치는 `main`이다. 문서 정리 시 수정한다.
