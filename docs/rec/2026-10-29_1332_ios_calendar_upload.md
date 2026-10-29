# iOS 연동 I2: 캘린더 연결·업로드

브랜치 `feature/ios-integration`. 설계 3(`docs/calendar_privacy_sync_design.md`) §5~§7의 iOS 쪽이다. 서버 C1~C5(`2026-09-30_1749`)와 `active_generation_usable` 규칙(`2026-10-28_1431` 결정 6)에 맞춘다. 화면은 "서버" 탭의 캘린더 절(디자인 없음)이다.

## 구현 내용

| 계층 | 파일 | 내용 |
|---|---|---|
| 변환 | `Calendar/CalendarFacts.swift` | window(사용자 시간대 오늘 00:00 ~ 91일 뒤 00:00, 배타. 90일 뒤 23:59까지), 분류, all-day, key, page 분할 |
| EventKit 경계 | `Calendar/CalendarEventSource.swift` | 권한 매핑(`.authorized`→full, `.writeOnly`→읽기 불가), 캘린더 목록, 선택 캘린더의 occurrence. DEBUG 시연 일정 |
| 업로더 | `Calendar/CalendarUploader.swift` | create → page → complete, revision, 재시도, abort |
| 조정자 | `Calendar/CalendarSync.swift` | 연결 상태 PUT, 선택, sync 기기, 업로드 시점, 사용자별 로컬 상태 |
| 연결 | `Auth/SessionStore.swift`, `PlanTogetherApp.swift`, `ServerDebugView.swift` | 세션마다 `CalendarSync`, 앱 활성화 갱신, 화면 |

### 읽기 규칙 (설계 3 §6)

- busy·unavailable·tentative는 그대로, availability를 지원하지 않는 캘린더(`notSupported`)는 busy, free·취소는 제외
- **참석자 응답은 보지 않는다.** 지시는 "참석 거절 등 제외"였지만 설계 §6.2가 "참석자 응답은 Launch MVP 계산에 사용하지 않는다"고 정한다. lead가 설계안(A)으로 확정했다. **거절한 일정도 Busy로 남는다(설계 §6.2, Post-MVP 검토)**
- all-day: 사용자 시간대의 시작일 00:00 ~ 마지막 날 다음 00:00(EventKit의 23:59:59 끝을 처리). Calendar 날짜 연산이라 DST 날(25시간)도 맞다. floating 시각과 IANA가 아닌 시간대(`GMT+0900` 등, 서버가 page 전체를 400으로 거부)는 사용자 시간대
- 반복 일정은 EventKit이 window 안에서 펼친 occurrence. 끝 ≤ 시작은 제외, 366일 넘는 일정은 window로 자름, window와 겹치지 않으면 제외, 초 미만은 버림
- **선택이 비면 EventKit을 읽지 않는다.** `predicateForEvents`의 calendars가 nil·빈 배열이면 모든 캘린더를 읽기 때문이다. 기본 선택은 비어 있다(§5.1)
- 시뮬레이터의 로컬("나의 iPhone") 캘린더는 availability를 지원하지 않아 free·tentative도 모두 `notSupported`, 즉 Busy로 읽힌다. iCloud·Exchange 캘린더는 구별한다. 안전한 쪽(빈 시간으로 오인하지 않음)이다

### 개인정보

- **source_event_key와 source key는 기기 비밀 키 HMAC-SHA256의 앞 16바이트(base64url)다.** 입력은 `eventIdentifier`+`occurrenceDate`, 캘린더는 `calendarIdentifier`. 반복 일정의 occurrence 하나를 옮기면 `occurrenceDate`는 원래 시각이라 key가 그대로다. 반복이 아닌 일정은 `occurrenceDate`가 시작 시각이라 시각을 옮기면 key가 바뀐다(서버에서는 옛 fact 삭제 + 새 fact다). 서버는 원래 identifier를 알 수 없고, 같은 occurrence는 snapshot마다 같은 key라 서버가 projection을 매번 다시 만들지 않는다. 설계의 "무작위 key + 로컬 mapping 표" 대신 이 방식을 택했다. 표를 저장·정리할 필요가 없다. 비밀 키는 사용자별 로컬 상태 파일에 있고 로그아웃하면 지운다
- page JSON의 키는 allowlist 6개뿐이다(`source_event_key`, `start_at`, `end_at`, `all_day`, `time_zone`, `availability`). 테스트로 고정했다. 제목·장소·메모·URL·참석자·캘린더 이름은 읽지도 보내지도 않는다
- **details(제목·장소)는 보내지 않는다.** I2에는 공개 수준을 details로 바꾸는 화면이 없다(V5). snapshot은 언제나 `details_included: false`다. details를 고른 사용자는 서버가 busyOnly로 안전 강등한다. details 업로드와 `details_refresh_state` 재업로드는 V5에서 붙인다
- EventKit 원본, fact, 캘린더 identifier는 메모리에만 있고 로그에 남기지 않는다

### 연결 상태 (설계 3 §5.2, 서버 `transitionAllowed`)

- 서버 전이 표를 `CalendarTransition`으로 그대로 옮겼다. 허용되지 않는 PUT은 보내지 않는다. GET의 `stale`은 저장 상태 `ready`로 본다
- 연결이 없으면(version 0) If-Match 없는 create-only PUT, 있으면 If-Match. **PUT이 409(version_conflict)면 같은 본문을 다시 보내지 않는다.** 그사이 다른 기기가 연결을 바꾼 것이므로 최신 연결을 다시 읽고 처음부터 한 번 더 판단한다(create-only 409도 같다)
- 권한: 아직 묻지 않음 → `permission_required`, 제한 → `restricted`, 거부·write-only → 권한이 있던 연결(selecting·syncing·ready·error·needs_source_reselection)이면 `revoked`, 아니면 `denied`. OS 권한 요청은 사용자가 "캘린더 연결"을 누를 때만 한다(앱 활성화 때는 묻지 않는다)
- **선택을 모두 해제하면:** 업로드하던 연결(ready·syncing·error)은 `needs_source_reselection` + 빈 sources(서버가 ready → selecting을 거부한다), 그 밖은 `selecting`. 선택한 캘린더가 사라지면 남은 것만 쓰고, 다 사라지면 같은 규칙이다
- source key는 enabled 집합이 실제로 바뀔 때만 보낸다(바뀌면 서버가 usable=false로 숨기므로 같은 흐름에서 바로 업로드한다)
- 연결이 없거나 사용자가 끊은(`disconnected`) 연결은 사용자가 이 기기에서 "캘린더 연결"을 누를 때만 다시 시작한다. 앱 활성화·선택 변경·수동 새로고침은 끊은 연결을 되살리지 않는다(서버가 지운 일정을 다시 올리지 않는다)
- `needs_source_reselection`은 사용자가 다시 고를 때까지 그대로 둔다(설계 §12). 선택이 생기면 `selecting`으로 바꾸고 올린다

### sync 기기 (설계 3 §7.1)

로그인마다 서버가 새 기기를 만들고 로그아웃은 기기를 폐기한다. 그래서 같은 폰에서 다시 로그인하면 연결의 sync 기기가 폐기된 옛 기기라 업로드가 403으로 조용히 멈추고 6시간 뒤 stale이 된다. 규칙: sync 기기가 없거나 `GET /v1/devices`에 없으면(폐기) 자동으로 가져오고, 살아 있는 다른 기기면 "다른 기기가 동기화 중"을 보여 주며 사용자가 이 기기에서 연결을 누를 때만 가져온다.

**이 판정은 상태를 바꾸는 모든 연결 PUT보다 먼저다(리뷰 High).** 연결 상태는 사용자 하나에 하나라, sync 기기가 아닌 iPad가 자기 권한(거부·제한, MDM이면 사용자 행동 없이도)이나 빈 선택으로 PUT하면 iPhone의 projection이 숨겨지고 두 기기가 앱을 열 때마다 상태를 번갈아 뒤집는다. 그래서 다른 살아 있는 기기가 맡고 있으면 iPad는 앱 활성화·선택 변경·새로고침에서 아무 PUT도 하지 않는다. 선택은 iPad 로컬에만 저장된다.

**자동으로 맡는 것도 올릴 수 있는 기기만 한다(재검증 Medium).** sync 기기가 폐기됐어도(iPhone 재로그인) 권한이 없거나 선택이 빈 iPad가 앱을 열면 맡지 않고 아무것도 바꾸지 않는다. 맡으면서 거부·재선택 상태를 PUT하면 iPhone이 그 뒤 iPad를 살아 있는 sync 기기로 보고 물러나 projection이 숨겨진 채 남기 때문이다. iPhone의 다음 앱 활성화가 맡아 올린다. sync 기기가 한 번도 없던 연결(올린 적 없어 노출할 generation이 없다)에서는 맡지 않고 권한 상태만 맞춘다.

### 업로드 (설계 3 §7.2)

- 시점: 연결 직후(선택 후), 앱 활성화, 수동 "지금 업로드", 선택 변경. 앱 활성화는 마지막 업로드 60초 안이면 건너뛴다(서버 snapshot 한도 10/분). 백그라운드 갱신·EventKit 변경 알림 debounce는 하지 않는다
- 서버 한도: page 250 facts·본문 60,000바이트(64KB에 여유) 이하, 200 page, 20,000 facts. 넘으면 **잘라 올리지 않고** 실패한다(빠진 일정이 빈 시간으로 보인다). fact는 key 순 정렬이라 같은 일정이면 같은 page 바이트다
- revision: 사용자별 로컬 상태에 저장한 뒤 보낸다. 409 `version_conflict`면 서버 마지막 revision + 1로 다시 만든다. 같은 revision의 기존 session(200)을 돌려받으면 열려 있어도 이어 쓰지 않고 revision을 올린다(옛 session의 다른 page에 남은 staging과 새 page의 key가 겹쳐 400이 날 수 있다. 새 revision의 create가 서버에서 옛 열린 session을 abort한다). 최대 세 번(로그아웃이 로컬 revision을 지운 뒤 재로그인해도 409 → 서버 revision + 1로 이어진다)
- **시간대 대체:** 서버 snapshot 검증(`calendar.validTimeZone`)은 빈 값·64자 초과·`Local`만 따로 거부하고 나머지는 `time.LoadLocation`이 열면 받는다. 서버는 `time/tzdata`를 내장한다(`internal/calendar/calendar.go`, `internal/availability/preferences.go`의 `_ "time/tzdata"`, `go list -deps ./cmd/api`에 포함). `LoadLocation`은 호스트 zoneinfo를 먼저 보고 없으면 내장분을 쓴다. 이 규칙은 가능 시간 설정(`availability.ValidTimeZone`: Area/City 정규식 + 내장 tzdata로 만든 `zones_gen` 목록)보다 넓어 `UTC`·`GMT`·`Etc/…`도 받는다. 그래서 page가 시간대 때문에 거부되는 경우는 **기기 tz DB가 서버의 tzdata(호스트 zoneinfo와 서버 Go 버전의 내장분)보다 새로워 기기만 아는 IANA 이름**뿐이다. 기기 쪽은 `TimeZone.knownTimeZoneIdentifiers`에 있는 이름과 UTC·GMT만 보내므로 표시용 이름(`GMT+0900`)은 미리 사용자 시간대로 바꾼다. **page가 400으로 거부될 때만**(create·complete의 400은 해당 없음) 그 session을 abort하고 두 단계로 다시 올린다: ① 사용자 시간대와 다른(일정 고유) 시간대만 UTC로, ② 그래도 거부되면 사용자 시간대까지 UTC로. 시작·끝은 이미 UTC라 바쁜 시간은 같고 원래 시간대 표시만 잃는다. 시간대와 무관한 거부면 두 단계 모두 실패해 오류로 끝나고 대체 표시도 남지 않는다
- page·complete는 네트워크 오류·5xx·429(Retry-After)에서 같은 키·본문으로 최대 3번. 그 밖의 실패(400, `calendar_snapshot_incomplete` 등)는 abort(빈 본문)해 기존 ready generation을 그대로 둔다. 이미 닫힌 session(`calendar_snapshot_state`)·다른 기기(403)·세션 종료면 abort하지 않는다
- **재시작 정책:** 중단된 업로드를 이어 하지 않는다. 다음 업로드가 새 revision으로 처음부터 하고, 서버가 그때 열린 옛 session을 abort한다(설계 §12 "staging 유지 후 page 재시도"는 한 업로드 안의 재시도로만 한다)
- 사용자별 로컬 상태(비밀 키, 선택 캘린더, revision, 마지막 업로드 시각)는 I1의 `SessionGate` 안에서 쓰고, 다른 사용자 것은 읽지 않으며, 로그아웃하면 지운다. `CalendarSync`와 업로더는 요청마다 gate가 열려 있는지 확인한다. 로그아웃 뒤 재로그인하면 같은 APIClient가 새 token을 쓰므로, 옛 세션의 실행이 새 token으로 PUT하지 않게 한다
- 실행은 사슬로 하나씩 돈다. 앱 활성화만 진행 중 결과를 함께 쓰고, 연결·선택 변경·새로고침은 앞 실행이 끝난 뒤 새로 돈다. 참고: 앱 활성화가 받는 결과는 진행 중이던 실행(연결일 수도 있다)의 것이라 그 note는 앱 활성화 의도와 다를 수 있다. 서버 상태에는 영향이 없다

## 시연 준비: DEBUG "시연 일정 만들기" 버튼

시뮬레이터에 매번 손으로 일정을 넣는 대신 버튼을 택했다. 같은 일정을 재현할 수 있어 E2E 기록에 맞고, 실제 EventKit 경로(권한, 반복 펼치기, all-day)를 그대로 지난다.

- DEBUG 빌드에서만 컴파일된다. "PlanTogether 시연" 캘린더 하나만 만들거나 다시 쓰고, 다시 누르면 그 캘린더의 일정만 지우고 다시 만든다. 다른 캘린더는 건드리지 않는다
- 일정: 내일 회의, 주간 반복 4회, 하루 종일, 미정, 한가함(free). 로컬 캘린더에서는 미정·한가함도 Busy다(위). 읽기 대상으로 쓰려면 목록에서 직접 선택해야 한다
- 순서: 서버 탭 → 로그인 → 캘린더 연결(권한 허용) → 시연 일정 만들기 → "PlanTogether 시연" 켜기 → 서버 상태 ready

## 테스트

- iOS 게이트 94개 실행, 90개 통과, 4개 건너뜀(선택 실행)
  - 변환 9개: window, 분류, 반복 key 고정·중복 제거·입력 순서 무관, all-day(서울, 뉴욕 DST), 시간대 대체, 긴 일정 자르기·초 미만, page JSON allowlist, page 경계(250/251, 20,000 = 80 page, 20,001 실패, 각 page ≤ 60,000바이트)
  - 업로더 8개: 정상 흐름·본문, revision 충돌, 재사용 session(닫힘·열림 모두 새 revision), 409 → 닫힌 200 → 생성, page 재시도(같은 키·본문, Retry-After), 거절 page abort, complete incomplete만 abort
  - 조정자 16개. 가짜 서버는 실제 handler의 create-only·If-Match, 전이 표, 기기별 판정(token으로 구별), claim 때 ready → syncing, syncing 중 source 변경 → selecting, 같은 revision 200, complete의 syncing·fact 수 검사, 다른 기기 403을 흉내 낸다. 리뷰 반영 7개: 두 기기(iPad 거부·제한·빈 선택이 iPhone 상태를 안 바꿈, 연결 누르면 가져옴, 그 뒤 iPhone은 안 바꿈), disconnected 되살림 없음, 409 뒤 다시 판단, needs_source_reselection 유지, 시간대 UTC 대체, 닫힌 세션은 요청 없음, 진행 중 실행 뒤 선택 변경이 새로 돔. 재검증 반영 2개: 폐기된 sync 기기 + 올릴 수 없는 iPad 앱 활성화 → PUT 없음 → iPhone이 맡아 올림, 시간대 대체 두 단계(일정 고유 시간대만 UTC, 무관한 거부는 오류). 두 기기·disconnected·폐기 sync 기기 테스트는 수정 전 동작으로 되돌리면 실패하는 것을 확인했다. 그 밖: 전이 표, 연결 안 한 사용자 앱 활성화, 연결 흐름(create-only, 권한, 선택 대기, 읽지 않음), 선택한 캘린더만 읽기·HMAC key·재업로드 바이트 동일, 앱 활성화 억제, revoked·denied·restricted, 전체 해제 → needs_source_reselection, 삭제된 캘린더, sync 기기 규칙(살아 있는 다른 기기·연결 누름·폐기된 기기), 다른 사용자 상태 무시
  - 로그아웃이 캘린더 로컬 상태를 지운다
- 선택 실행(로컬 서버 `run_api_dev.sh`, 시뮬레이터 권한 `xcrun simctl privacy <udid> grant calendar com.example.PlanTogether`), 모두 통과
  - `LiveCalendarSmokeTests`: 가짜 source 301개 일정 → 2 page → ready, 두 번째 revision, 전체 해제 → needs_source_reselection. 서버 로그 모두 2xx
  - `LiveEventKitTests`: 실제 EventKit에서 시연 일정 8 occurrence(반복 4 펼침), 다시 만들어도 늘지 않음, all-day = 로컬 하루, 선택 비면 읽지 않음, 실제 EventKit + 실서버 업로드 → ready(8 facts)

## 남은 것

- V5: details 업로드(제목·장소, `details_included`, `details_refresh_state` 재업로드)
- EventKit 변경 알림(`EKEventStoreChanged`) debounce 업로드, 백그라운드 갱신
- destination calendar 선택·쓰기(설계 8)
- 연결 해제(`disconnected`) 화면, 권한 거부 시 설정 앱 이동 버튼
- sync 기기 전환 UI(지금은 연결을 다시 누르면 가져온다)
