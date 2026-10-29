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

- **source_event_key와 source key는 기기 비밀 키 HMAC-SHA256의 앞 16바이트(base64url)다.** 입력은 `eventIdentifier`+`occurrenceDate`(사용자가 occurrence 시각을 옮겨도 같은 key), 캘린더는 `calendarIdentifier`. 서버는 원래 identifier를 알 수 없고, 같은 occurrence는 snapshot마다 같은 key라 서버가 projection을 매번 다시 만들지 않는다. 설계의 "무작위 key + 로컬 mapping 표" 대신 이 방식을 택했다. 표를 저장·정리할 필요가 없다. 비밀 키는 사용자별 로컬 상태 파일에 있고 로그아웃하면 지운다
- page JSON의 키는 allowlist 6개뿐이다(`source_event_key`, `start_at`, `end_at`, `all_day`, `time_zone`, `availability`). 테스트로 고정했다. 제목·장소·메모·URL·참석자·캘린더 이름은 읽지도 보내지도 않는다
- **details(제목·장소)는 보내지 않는다.** I2에는 공개 수준을 details로 바꾸는 화면이 없다(V5). snapshot은 언제나 `details_included: false`다. details를 고른 사용자는 서버가 busyOnly로 안전 강등한다. details 업로드와 `details_refresh_state` 재업로드는 V5에서 붙인다
- EventKit 원본, fact, 캘린더 identifier는 메모리에만 있고 로그에 남기지 않는다

### 연결 상태 (설계 3 §5.2, 서버 `transitionAllowed`)

- 서버 전이 표를 `CalendarTransition`으로 그대로 옮겼다. 허용되지 않는 PUT은 보내지 않는다. GET의 `stale`은 저장 상태 `ready`로 본다
- 연결이 없으면(version 0) If-Match 없는 create-only PUT, 있으면 If-Match. create가 409면 다시 읽고 한 번 더 보낸다
- 권한: 아직 묻지 않음 → `permission_required`, 제한 → `restricted`, 거부·write-only → 권한이 있던 연결(selecting·syncing·ready·error·needs_source_reselection)이면 `revoked`, 아니면 `denied`. OS 권한 요청은 사용자가 "캘린더 연결"을 누를 때만 한다(앱 활성화 때는 묻지 않는다)
- **선택을 모두 해제하면:** 업로드하던 연결(ready·syncing·error)은 `needs_source_reselection` + 빈 sources(서버가 ready → selecting을 거부한다), 그 밖은 `selecting`. 선택한 캘린더가 사라지면 남은 것만 쓰고, 다 사라지면 같은 규칙이다
- source key는 enabled 집합이 실제로 바뀔 때만 보낸다(바뀌면 서버가 usable=false로 숨기므로 같은 흐름에서 바로 업로드한다)
- 연결한 적 없는 사용자의 앱 활성화는 아무것도 만들지 않는다

### sync 기기 (설계 3 §7.1)

로그인마다 서버가 새 기기를 만들고 로그아웃은 기기를 폐기한다. 그래서 같은 폰에서 다시 로그인하면 연결의 sync 기기가 폐기된 옛 기기라 업로드가 403으로 조용히 멈추고 6시간 뒤 stale이 된다. 규칙: sync 기기가 없거나 `GET /v1/devices`에 없으면(폐기) 자동으로 가져오고, 살아 있는 다른 기기면 앱 활성화로는 가져오지 않고 "다른 기기가 동기화 중"을 보여 주며 사용자가 연결을 누를 때만 가져온다.

### 업로드 (설계 3 §7.2)

- 시점: 연결 직후(선택 후), 앱 활성화, 수동 "지금 업로드", 선택 변경. 앱 활성화는 마지막 업로드 60초 안이면 건너뛴다(서버 snapshot 한도 10/분). 백그라운드 갱신·EventKit 변경 알림 debounce는 하지 않는다
- 서버 한도: page 250 facts·본문 60,000바이트(64KB에 여유) 이하, 200 page, 20,000 facts. 넘으면 **잘라 올리지 않고** 실패한다(빠진 일정이 빈 시간으로 보인다). fact는 key 순 정렬이라 같은 일정이면 같은 page 바이트다
- revision: 사용자별 로컬 상태에 저장한 뒤 보낸다. 409 `version_conflict`면 서버 마지막 revision + 1로 한 번 다시 만든다. 같은 revision의 기존 session(200)을 돌려받으면 이미 중단된 session일 수 있어 새 revision으로 다시 만든다
- page·complete는 네트워크 오류·5xx·429(Retry-After)에서 같은 키·본문으로 최대 3번. 그 밖의 실패(400, `calendar_snapshot_incomplete` 등)는 abort(빈 본문)해 기존 ready generation을 그대로 둔다. 이미 닫힌 session(`calendar_snapshot_state`)·다른 기기(403)·세션 종료면 abort하지 않는다
- **재시작 정책:** 중단된 업로드를 이어 하지 않는다. 다음 업로드가 새 revision으로 처음부터 하고, 서버가 그때 열린 옛 session을 abort한다(설계 §12 "staging 유지 후 page 재시도"는 한 업로드 안의 재시도로만 한다)
- 사용자별 로컬 상태(비밀 키, 선택 캘린더, revision, 마지막 업로드 시각)는 I1의 `SessionGate` 안에서 쓰고, 다른 사용자 것은 읽지 않으며, 로그아웃하면 지운다

## 시연 준비: DEBUG "시연 일정 만들기" 버튼

시뮬레이터에 매번 손으로 일정을 넣는 대신 버튼을 택했다. 같은 일정을 재현할 수 있어 E2E 기록에 맞고, 실제 EventKit 경로(권한, 반복 펼치기, all-day)를 그대로 지난다.

- DEBUG 빌드에서만 컴파일된다. "PlanTogether 시연" 캘린더 하나만 만들거나 다시 쓰고, 다시 누르면 그 캘린더의 일정만 지우고 다시 만든다. 다른 캘린더는 건드리지 않는다
- 일정: 내일 회의, 주간 반복 4회, 하루 종일, 미정, 한가함(free). 로컬 캘린더에서는 미정·한가함도 Busy다(위). 읽기 대상으로 쓰려면 목록에서 직접 선택해야 한다
- 순서: 서버 탭 → 로그인 → 캘린더 연결(권한 허용) → 시연 일정 만들기 → "PlanTogether 시연" 켜기 → 서버 상태 ready

## 테스트

- iOS 게이트 83개 실행, 79개 통과, 4개 건너뜀(선택 실행)
  - 변환 9개: window, 분류, 반복 key 고정·중복 제거·입력 순서 무관, all-day(서울, 뉴욕 DST), 시간대 대체, 긴 일정 자르기·초 미만, page JSON allowlist, page 경계(250/251, 20,000 = 80 page, 20,001 실패, 각 page ≤ 60,000바이트)
  - 업로더 6개: 정상 흐름·본문, revision 충돌, 재사용 session, page 재시도(같은 키·본문, Retry-After), 거절 page abort, complete incomplete만 abort
  - 조정자 9개(서버 상태 전이를 흉내 낸 가짜 서버): 전이 표, 연결 안 한 사용자 앱 활성화, 연결 흐름(create-only, 권한, 선택 대기, 읽지 않음), 선택한 캘린더만 읽기·HMAC key·재업로드 바이트 동일, 앱 활성화 억제, revoked·denied·restricted, 전체 해제 → needs_source_reselection, 삭제된 캘린더, sync 기기 규칙(살아 있는 다른 기기·연결 누름·폐기된 기기), 다른 사용자 상태 무시
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
