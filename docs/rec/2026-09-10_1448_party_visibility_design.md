# Party별 공개 수준과 계산 반영 규칙 설계 완료

## 확정한 정책

- 공개 수준은 사용자·Party 단위의 `details`, `busyOnly`, `hidden` 세 가지다. Party 생성·가입·재가입의 기본값은 `busyOnly`다.
- `details`는 시간과 제목을 공유하며 장소는 기본 `false`인 `share_location` 별도 동의가 있어야 공유한다.
- `busyOnly` projection에는 상세 ciphertext와 원본 상세 존재 여부를 저장하거나 전송하지 않는다.
- `hidden`은 Party projection을 만들지 않지만 private Busy fact는 가능 시간과 제안·확정 충돌 계산에 동일하게 반영한다.
- 공개 설정값과 변경 이력은 본인만 볼 수 있고 방장도 다른 멤버의 값을 읽거나 바꿀 수 없다.
- Launch MVP는 전원 공통 슬롯만 반환하며 가능 인원, 불가능 원인, hidden 멤버 수와 충돌 소유자를 노출하지 않는다.

## 데이터와 동기화 경계

- 계산 입력은 `calendar_busy_facts`, Party 일정 화면 입력은 `party_schedule_projections`로 분리한다.
- details 상향과 장소 활성화는 `pending_details_refresh` 후 새 snapshot의 상세 allowlist가 와야 적용한다. 그전에는 busyOnly로 안전 강등한다.
- 제목·장소는 private Busy facts로 옮기지 않고 snapshot complete transaction에서 허용된 Party projection에만 복사한다.
- 공개 수준 하향은 상세 삭제 또는 projection 삭제와 sync tombstone 기록을 같은 transaction에서 끝낸다.
- source generation과 setting version을 함께 검사해 hidden·하향 변경 뒤 stale snapshot이 상세 projection을 되살리지 못하게 한다.
- 공개 수준 변경의 projection 변경은 현재 활성 멤버에게 전달한다. 탈퇴·강퇴·해산 cleanup은 전이 직전 멤버와 떠난 사용자도 수신해 로컬 캐시를 삭제한다.

## 산출물

- 상세 설계: `docs/party_visibility_design.md`
- 구현 계획: `.omc/plans/party-visibility-implementation.md`
- 백로그: `docs/feature_design_backlog.md`의 `확정 설계 5`
- Party membership P1: 최소 임시 스키마 문구를 설계 5의 최종 필드·constraint 적용으로 갱신
- 구현 기준 브랜치 문서: 실제 저장소와 맞게 `main`으로 수정
- 진행 상황: 다음 작업을 설계 6으로 이동

## 현재 구현과의 차이

- `Party.visibilityByMember`는 다른 멤버의 설정까지 메모리에 보유하지만 확정 설계는 본인 설정만 수신한다.
- `AppStore.connectCalendar()`는 hidden이면 BusyInterval을 제거하지만 확정 설계는 계산용 private Busy fact를 유지한다.
- 현재 BusyInterval은 계산과 화면 표시를 겸하지만 확정 설계는 private fact와 projection을 분리한다.
- `AvailabilitySlot.availableMemberCount`와 `totalMemberCount`는 개인정보 응답 계약과 충돌해 구현 시 제거한다.

이번 작업은 문서 설계이므로 Swift·서버 코드는 변경하지 않았다.

## 검증

- `docs/account_backend_design.md`, `docs/calendar_privacy_sync_design.md`, `docs/party_membership_design.md`와 현재 Swift 모델을 대조했다.
- `git diff --check`로 Markdown whitespace 오류가 없음을 확인했다.
- 별도 `code-reviewer` 에이전트가 12개 관련 파일을 재검증했다. Critical/High는 없었고 Medium 1건, Low 2건을 보고했다.
- Medium: visibility 변경 수신자 규칙을 멤버십 종료 cleanup에도 일반화하면 떠난 사용자의 캐시가 남을 수 있었다. 두 규칙을 분리하고 탈퇴·강퇴·해산에는 전이 직전 수신자 집합을 사용하도록 반영했다.
- Low: `progressing.md`의 설계 5/branch 표기를 갱신하고 이 작업 기록을 추가했다.
- 코드 변경이 없어 Xcode 빌드와 단위 테스트는 실행하지 않았다. 최신 실행 결과가 별도로 남아 있지 않은 기존 검증 공백은 유지된다.

## 다음 작업

- 공통 가능 시간의 활동 시간, 검색 범위·기간·슬롯 조건, 최대 결과 수와 결과 정렬을 상세 설계한다. (설계 6)
