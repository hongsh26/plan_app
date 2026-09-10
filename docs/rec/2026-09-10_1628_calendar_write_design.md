# 확정 일정 외부 캘린더 쓰기·변경·취소 설계 완료

## 완료 내용

- 상세 설계 8을 `docs/calendar_write_design.md`에 작성했다. 백로그의 상세 설계 형식 12개 항목을 15개 절로 모두 채웠다.
- 구현 순서를 `.omc/plans/calendar-write-implementation.md`(W0~W8)에 작성했다.
- `docs/feature_design_backlog.md`에 `확정 설계 8` 절을 추가하고 현재 실행 컷라인을 상세 설계 9로 옮겼다.
- 백로그 미결 항목이던 "앱 내부 확정 후 일부 사용자의 캘린더 쓰기 실패 표시" 질문을 해소하고 목록에서 제거했다.
- `docs/progressing.md`에 완료 상태와 다음 작업을 반영했다.

## 확정한 핵심 정책

- `confirmed_events`가 앱 내부 기준 데이터이며 사용자별 EventKit 반영 성공 여부와 완전히 분리된다.
- 확정 일정 본문은 수정하지 않는다. 변경은 취소 + 재제안이며, 재일정은 기존 확정을 유지한 채 대체 제안을 만들고 그것이 확정될 때 교체한다. 대체 제안이 실패하면 원래 약속이 그대로 남는다.
- 외부 반영 상태는 본인 전용 리소스다. 다른 멤버에게 집계 형태로도 노출하지 않으므로 "일부 사용자 실패"라는 그룹 표시는 존재하지 않는다.
- 명시적 약속 취소와 대체 확정만 delete command를 발급한다. 탈퇴·강퇴·해산은 이미 반영된 외부 일정을 자동 삭제하지 않고 본인 전용 `calendar_cleanup_suggestions`로 정리를 제안한다.
- 취소 권한은 원 제안의 생성자와 Party 방장에게 있다.

## 선행 설계와의 정합

- revision 의미 충돌을 한 모델로 정리했다. 외부 반영 세대는 사용자별 `calendar_write_commands.revision`이 담당하고, 설계 7이 정한 `confirmed_events.revision`은 본문 세대로서 본문 불변 원칙에 따라 1로 고정된다.
- command 상태 기계는 설계 3 §10.1을 그대로 사용하고 이 설계는 revision 발급 규칙, 취소 시 delete 발급 조건, lease 계약, 종단 처리만 추가한다.
- 취소가 `confirmed_event_participants`를 `released`로 만들어 reservation exclusion constraint가 같은 시간의 재확정을 허용하도록 했다. constraint가 다른 테이블 컬럼을 참조할 수 없으므로 참여자 row의 status를 같은 transaction에서 갱신하는 것을 명시했다.
- 떠난 사용자의 reservation은 해제하되 외부 캘린더에 남은 이벤트가 다음 snapshot에서 private Busy fact로 잡히므로 실제 충돌 보호는 유지된다.

## 후속 경계

- domain outbox type 4종(`confirmed_event_created`, `confirmed_event_cancelled`, `calendar_write_command_pending`, `calendar_write_action_required`)의 문구·수신자·urgency·quiet hours는 상세 설계 9에서 확정한다.
- 외부 → 앱 역방향 반영, 참석자 초대, 알람 설정, Google Calendar 쓰기는 Post-MVP다.
- 사용자 결정에 따라 상세 설계 9 완료 전에는 구현에 착수하지 않는다.

## 검토 에이전트 재검증

검토 에이전트가 선행 설계 2~7과 대조해 총 43건을 지적했고 전부 반영했다. 반영 여부는 문서 전문 검색으로 35개 대표 문구를 확인했다.

| 등급 | 건수 | 대표 지적 |
|---|---:|---|
| Critical | 2 | 취소 시 delete 미발급 조건이 현재 세대 상태만 보아 고아 이벤트 잔존(C1), 해산 시 잔존 일정 회수 경로 부재(C2) |
| High | 13 | writer 전환 중 취소의 create/delete 역순(H1), 떠난 원 제안자의 취소 가능(H2), 재설치 후 중복 생성(H3), destination 상실이 전 Party 검색 차단(H4), cleanup hint 전달 경로 미정의(H5), 계정 삭제 동기/비동기 모순(H6), claim 질의 user 스코프 누락(H7), update 폐기 잔재(H8), 재일정이 확정을 즉시 파괴(H9), advisory lock 누락(H10), 재일정 링크의 6개 경로 결합(N1), `app_confirmed_event_id` 노출 경계(N2), 롤아웃 시 전 사용자 재일정 차단(N7) |
| Medium | 18 | destination 재선택 범위(M1), 재가입자 처리(M3), 과거 일정 command 종료(M4), 알림 임계값(M5), `result_locator` 소비자(M6), 중복 정리 조건(M7), 취소자 노출 일관성(M8), 계획 완료 기준·누락 작업(M11~M13), marker 파싱 한정(N3), reservation 교체 순서(N4), T3 예외 범위(N6), 손자 proposal 승계(N8), 강제 stale의 가용성 손실(A1) |
| Low | 10 | 오류 코드 우선순위(L2), pagination 규격(L4), `next_run_at` 제약 누락(L7), 계획 병렬화(L8) 등 |

### 설계 방향이 바뀐 지적

- **H9**: 사용자 결정에 따라 재일정을 "즉시 취소 + 새 제안"에서 "기존 확정 유지 + 대체 제안 확정 시 교체"로 바꿨다.
- **N1**: 재일정 링크를 `confirmed_events`의 가변 필드가 아니라 `proposals.supersedes_confirmed_event_id` 불변 필드로 두고, "재일정 진행 중"은 열린 대체 제안의 존재로 파생 판정한다. 6개 종단 경로의 write-back이 사라지고 `confirmed_events`가 다시 불변이 되었다.
- **H4**: `calendar_connections.status`를 `source_status`와 `destination_status` 두 축으로 분리했다. 쓰기 전용 실패가 읽기 freshness gate를 막지 않는다.
- **H5**: cleanup hint를 일회성 sync change가 아니라 본인 전용 영속 리소스로 바꿔 cursor 만료 후 bootstrap에서도 유실되지 않게 했다.
- **A1**: N7 수정안 중 강제 re-snapshot을 철회했다. 롤아웃 순간 전 Party의 검색이 며칠간 차단되는 반면 정확성 이득은 없다.

### 미결로 남긴 사용자 결정

- Party 해산 시 이미 반영된 외부 일정은 delete를 발급하지 않고 정리 제안만 한다. 검토는 반대 방향을 권고했으나 사용자가 현행 유지를 선택했고, 정리 제안을 무시하면 이벤트가 영구히 남는다는 한계를 §4.4에 명시했다.

### 함께 갱신한 선행 설계

- `docs/proposal_lifecycle_design.md`: T3 충돌 검사 예외 3조건, T6 승계 규칙과 문 순서, `supersedes_confirmed_event_id` 불변식, 잠금 순서
- `docs/calendar_privacy_sync_design.md`: `source_status` 축 분리, snapshot allowlist `app_confirmed_event_id`와 marker 파싱 한정, 금지 데이터
- `docs/party_membership_design.md`: T7 step 7 tombstone 목록과 step 10 확정
- `docs/party_visibility_design.md`: projection 복사 제외
- `docs/account_backend_design.md`: 캘린더 dedupe key 위임

## 검증

- 백로그 상세 설계 형식 12개 항목을 모두 충족했다. 신설한 §4.5(입력값과 검증 규칙), §8.1.1(정상·빈 상태·로딩), §15.1(미결정 사항)로 미충족 3건을 해소했다.
- 현재 Swift 코드는 변경하지 않았다.
