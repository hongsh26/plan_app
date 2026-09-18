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
- Codex 시작 시 경고를 발생시키던 Claude 전용 `oh-my-claudecode@omc` 플러그인을 전역 설정에서 비활성화했다. Codex 전용 `oh-my-codex`는 유지했으며, 재검증에서 SessionEnd 경고와 `t` MCP 시작 실패가 제거되었다.
- Party별 공개 수준과 계산 반영 규칙을 `docs/party_visibility_design.md`에 확정했다. `details`의 장소는 별도 동의, `busyOnly`는 상세 ciphertext 비저장, `hidden`은 projection 0건이지만 private Busy 계산 반영으로 정했다.
- 공개 설정은 본인 전용 리소스이고 projection은 활성 멤버에게만 전달한다. 하향 변경은 상세 삭제·tombstone을 같은 transaction에서 끝내며 generation·setting version guard로 stale snapshot의 재노출을 막는다.
- 공개 수준 구현 계획을 `.omc/plans/party-visibility-implementation.md`에 작성했다. 검토 에이전트 재검증에서 Critical/High는 없었고, 멤버십 종료 시 떠난 사용자도 cleanup tombstone을 받아야 한다는 Medium 1건을 반영했다.
- 공통 가능 시간 검색 조건과 결과 표시를 `docs/availability_search_design.md`에 확정했다. 활동 시간은 사용자 확인형 주간 규칙, 기본 검색은 오늘 포함 14일·60분, 슬롯은 30분 grid, 결과는 하루 3개·전체 20개로 정했다.
- 검색은 모든 활성 멤버의 활동 시간에서 private Busy facts를 뺀 교집합만 계산한다. generation window UTC 교집합, 6시간 freshness `validUntil`, opaque `calculationVersion`으로 누락·만료 결과의 제안 재사용을 막는다.
- 검색 구현 계획을 `.omc/plans/availability-search-implementation.md`에 작성했다. 검토 에이전트의 Medium 3건(window 경계, 시간 경과 freshness, DST gap)을 모두 설계와 인수 조건에 반영했다.
- 현재 설계 작업의 컷라인을 상세 설계 9(알림)까지로 정했다. 상세 설계 7~9를 모두 확정한 뒤 상세 설계 1~9에서 확정한 기능을 의존성 순서에 따라 단계별로 구현하고, 상세 설계 10~11은 이후 다시 우선순위를 정한다.
- 제안·응답·확정 상태 전이를 `docs/proposal_lifecycle_design.md`에 확정했다. 참여자는 생성 시 활성 멤버 전원으로 고정하고, 전원 수락·15분 freshness·private Busy와 앱 내부 reservation 충돌 없음이 모두 충족될 때만 앱 내부 확정을 만든다.
- 미확정 제안의 참여자가 떠나면 남은 인원으로 조용히 확정하지 않고 `reproposal_required`로 종료한다. 제목·장소·시간·기한 변경도 기존 수락을 승계하지 않고 새 제안으로 만든다.
- 제안 상태 전이 구현 계획을 `.omc/plans/proposal-lifecycle-implementation.md`에 작성했다.
- 확정 일정의 외부 캘린더 쓰기·변경·취소를 `docs/calendar_write_design.md`에 확정했다. 앱 내부 `confirmed_events`를 기준 데이터로 두고 사용자별 EventKit 반영과 완전히 분리했다.
- 확정 일정 본문은 수정하지 않고 취소 + 재제안(재일정)으로만 바꾼다. 외부 반영 세대는 사용자별 `calendar_write_commands.revision`이 담당하고 `confirmed_events.revision`은 본문 세대로서 1에 고정된다.
- 외부 반영 상태는 본인 전용 리소스로 정했다. 다른 멤버에게는 집계 형태로도 노출하지 않으며, 이로써 백로그의 "일부 사용자 캘린더 쓰기 실패 표시" 미결 질문이 해소됐다.
- 명시적 약속 취소만 delete command를 발급하고 탈퇴·강퇴·해산은 이미 반영된 외부 일정을 자동 삭제하지 않는다. 대신 본인 기기의 로컬 정리 제안으로 처리하고 떠난 사용자의 reservation은 해제한다.
- 캘린더 쓰기 구현 계획을 `.omc/plans/calendar-write-implementation.md`에 작성했다.
- 설계 8에 대해 검토 에이전트가 총 43건(Critical 2, High 13, Medium 18, Low 10)을 지적했고 전부 반영했다. 상세는 `docs/rec/2026-09-10_1628_calendar_write_design.md`에 있다.
- 검토 결과로 재일정 방식을 바꿨다. 기존 확정을 즉시 취소하지 않고 유지한 채 대체 제안을 만들며, 대체 제안이 확정될 때 교체한다. 실패하면 원래 약속이 그대로 남는다.
- 재일정 링크는 `proposals.supersedes_confirmed_event_id` 불변 필드이며 "재일정 진행 중"은 열린 대체 제안의 존재로 파생 판정한다. `confirmed_events`는 status와 취소 필드를 빼면 완전히 불변이다.
- `calendar_connections.status`를 `source_status`와 `destination_status` 두 축으로 분리했다. 쓰기 대상 캘린더 상실이 Party 전원의 가능 시간 검색을 막지 않는다.
- 설계 8 반영 과정에서 선행 설계 5건(2·3·4·5·7)의 관련 절을 함께 갱신했다.
- 알림 종류와 전송 규칙을 `docs/notification_design.md`에 확정했다. **이로써 상세 설계 1~9가 모두 끝났고 설계 단계가 종료됐다.**
- 알림 채널을 silent push(sync 재촉)와 alert push(사용자에게 보이는 알림)로 나누고 domain outbox 20종의 수신자·채널·urgency·quiet hours·묶음을 표로 확정했다.
- 알림 문구는 서버가 만들지 않는다. payload는 localization key와 사용자별 HMAC 참조만 담고 Notification Service Extension이 로컬 데이터로 Party 이름을 채운다. 이로써 설계 5·7·8의 "APNs payload는 opaque marker만" 확정을 개정 없이 유지했다.
- quiet hours의 결과를 bypass/defer/drop_if_stale 3종으로 정했다. 기한이 2분인 refresh 요청만 뚫고 나가고, 캘린더 반영 대기 알림은 밤 사이 쌓였다가 아침에 몰려나가지 않는다.
- 중복 억제를 outbox index에 의존하지 않기로 했다. `outbox_jobs`의 partial unique index는 활성 상태에만 걸려 성공 후 재삽입을 허용하므로 `(notification_key, user_id)` 영속 전달 기록을 따로 둔다.
- 같은 순간 두 번 울리는 경로 2개를 제거했다. `confirmed_event_created`와 `proposal_superseded`는 alert를 만들지 않고 silent만 만든다.
- 초대 발송 푸시는 만들지 않기로 확정했다. 재사용 링크는 앱 밖에서 전달되어 서버가 아는 수신자가 없다. 백로그의 "초대 알림" 항목이 이로써 해소됐다.
- 설계 9가 선행 설계 4건을 개정했다. 설계 8에 `calendar_cleanup_suggested` outbox type을 추가했고, 설계 2에 `devices` 알림 열 3개, bootstrap의 `notification_ref_key`, SwiftData의 App Group 공유 컨테이너 이전을 추가했다. 개정 내용은 각 설계 문서와 구현 계획 파일에 직접 반영했다.
- 알림 구현 계획을 `.omc/plans/notification-implementation.md`에 작성했다.
- 설계 9에 대해 검토 에이전트가 2회 검토했다. 1차 Critical 3건 포함 21건, 2차 Critical 3건 포함 20건을 지적했고 전부 반영했다. 2차 지적의 대부분은 1차 수정 자체가 만든 새 모순이었다. 상세는 `docs/rec/2026-09-15_1306_notification_design.md`에 있다.
- 검토 결과로 §5.1 매핑 표에 `category`·`recipient_scope`·`entity_type`·`entity_id` 열 4개를 추가했다. 표가 dispatch mapper의 완전한 명세여야 하는데 구현에 필요한 열이 빠져 있었고, 이 수정 하나로 Critical 1건과 High 1건이 함께 닫혔다.
- 수신자 판정을 `recipient_scope` 3종으로 바꿨다. 이전의 "활성 멤버가 아니면 폐기" 전역 규칙은 강퇴 대상·해산 직후 전원·떠난 사용자 대상 알림을 전부 폐기하는 설계였다.
- 알림 전송을 claim-then-send로 바꾸고 **at-most-once**를 명시적으로 택했다. worker가 APNs 호출 중 죽었을 때 알림이 영구 정지하거나 두 번 발송되는 경로를 막았다.
- payload에서 `valid_until`을 제거하고 `apns-expiration`을 상수로 바꿨다. 확정된 약속의 정확한 시작 시각이 APNs 헤더로 노출되는 경로였고, 1차에서 넣은 `min(valid_until, 36시간)` 상한은 36시간 안의 약속에 대해 아무 일도 하지 않았다.
- `notification_jobs`와 `notification_deliveries`를 alert 전용으로 확정했다. `notification_key`에 채널이 없어 `A+S` 16종의 alert와 silent가 같은 key를 갖고 서로를 막는 구조였다.
- 알림 전달 기록에 `owner_job_id`를 두어 "내 이전 시도"와 "다른 worker의 claim"을 구별하게 했다. 이것이 없으면 APNs 재시도가 unique index에 막히거나, 막지 않으면 동시 claim 방어가 무너진다.
- quiet hours 모드를 `bypass`/`defer` 2종으로 줄였다. `drop_if_stale`은 defer와 동작이 같아 이름만 하나 더 있었다.
- silent를 mapper 시점에 즉시 발행하도록 정했다. alert 발송 단계에 묶으면 defer된 alert의 silent도 아침까지 미뤄져 밤사이 sync가 정지한다.

## 다음 작업

1. **설계 단계가 끝났다. 구현에 착수한다.** 첫 구현은 Party 계획의 선행 조건인 `.omc/plans/party-membership-implementation.md`의 P0(서버 골격)이다. 서버 코드가 없으면 설계 4~9의 어떤 부분도 구현할 수 없다.
2. P0 이후 설계 1~9에서 확정한 기능을 의존성 순서에 따라 기능별 브랜치에서 단계별로 구현하고 관련 테스트를 실행한다. 대략적 순서는 서버 골격 → Party membership → 공개 수준 → calendar 동기화 → 가능 시간 검색 → 제안·확정 → 캘린더 쓰기 → 알림이다.
3. 상세 설계 10(결제)과 11(운영·출시 검증)은 상세 설계 1~9의 기능 구현 진행 후 다시 우선순위를 정한다.

## 유의 사항

- 원본 MVP의 Google 연동과 결제 목표는 장기 제품 백로그로 보존하되 첫 출시 범위에서는 제외한다.
- 기존 코드가 추가되거나 발견되면 `src/base/` 실행 가능성을 보존한다.
- Xcode 16.2에서 프로젝트 생성과 빌드 성공을 확인했다. 한 차례 실행은 당시 누락된 빌드 번호로 실패했고 이후 설정과 산출물의 `CFBundleVersion = 1`을 확인했지만, 수정 후 실행 및 최신 단위 테스트 결과 번들은 별도로 남아 있지 않다.
- 현재 앱 데이터는 메모리에만 보관되어 재실행 시 초기화된다.
- 상세 설계 9까지 확정된 내용과 현재 Swift 코드 사이에 차이가 있다. 알림은 iOS·서버 양쪽에 전혀 구현되어 있지 않으며 Notification Service Extension target도 없다. `CalendarService`에는 destination calendar 선택, writer device, 외부 쓰기 명령이 없다. 특히 hidden이 계산 입력에서도 제거되고, 활동 시간·시간대·검색 조건 모델이 없으며, Party가 모든 멤버의 공개 설정과 동기화 상태를 보유하고 proposal이 단순 응답 map의 `isConfirmed` 계산만 사용한다. 전체 차이는 각 상세 설계의 현재 구현 차이 절에 있다.
- 서버 코드는 아직 존재하지 않는다. Party 구현은 서버 골격(P0) 없이는 시작할 수 없다.
