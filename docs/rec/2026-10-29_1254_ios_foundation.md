# iOS 연동 I1: 기반 계층 (API 클라이언트·세션·sync 적용기·오프라인 큐)

브랜치 `feature/ios-integration`. 기존 인메모리 프로토타입(데모 탭)은 그대로 빌드·실행되고, 서버 연동은 새 "서버" 탭에서 확인한다.

## 구현 내용

| 영역 | 파일 | 내용 |
|---|---|---|
| API 클라이언트 | `src/PlanTogether/Networking/` | URLSession async/await actor. Bearer, GET이 아닌 요청마다 `Idempotency-Key`, `If-Match: "<v>"`, 오류 본문 `{code,message,request_id,current_version}` → `ServerError`, 429·503 `Retry-After`(초·HTTP 날짜). JSON은 모델마다 명시적 `CodingKeys`(snake_case 자동 변환 안 씀). 시각은 RFC 3339(소수 초 0~9자리, 오프셋 허용 — 3자리로 잘라 해석) |
| refresh | `APIClient.refreshed/performRefresh` | 실패한 access token 기준 single-flight. 401 요청은 refresh 뒤 같은 키·같은 본문 바이트로 한 번만 재시도. refresh 429만 Retry-After 뒤 같은 token으로 재시도(최대 5회, 로그아웃 안 함). 네트워크 오류·5xx는 그 자리에서 재전송하지 않고 로그아웃하지 않는다. **위험:** 서버가 회전을 커밋했는데 응답을 잃으면 다음 401의 refresh가 같은 token을 보내 재사용 탐지로 기기가 폐기되고 다시 로그인해야 한다. 서버에 유예가 없어 클라이언트만으로는 피할 수 없다. 세션 종료는 refresh 401·423과 모든 요청의 423뿐. 새 token은 메모리에 먼저 반영하고 Keychain 저장이 실패하면 다음 요청 때 다시 저장한다(소비된 refresh token을 다시 보내지 않게). 로그아웃·세션 종료마다 세대를 올려 그 전에 시작한 refresh 결과는 버린다. 세션 종료 handler는 동기로 등록한다 |
| token | `Auth/TokenStore.swift` | Keychain generic password(`AfterFirstUnlockThisDeviceOnly`)에 user_id·device_id·token 쌍. 서버는 로그인마다 기기를 발급하므로 별도 기기 등록 호출은 없다 |
| 세션 | `Auth/SessionStore.swift` | `signedOut / signedIn(user)`. 개발용 로그인(`dev:<이름>`), 복원, sync, 로그아웃(서버 `POST /v1/auth/logout` + Keychain·캐시·큐 삭제). 서버가 세션을 끝내면 같은 정리. 세션마다 `SessionGate`를 두고 큐·캐시 쓰기를 그 안에서 한다. 로그아웃·세션 종료는 gate를 먼저 닫고(진행 중인 쓰기는 끝날 때까지 기다린다) 파일을 지우므로, 그 뒤 끝나는 flush·sync가 파일을 되살리지 못한다 |
| 캐시 | `Sync/LocalStore.swift`, `SyncState.swift` | Application Support/PlanTogether/sync-cache.json 한 파일(백업 제외, 파일 보호). cursor·entity·하한을 함께 `.atomic`으로 써서 bootstrap 교체와 페이지 적용이 각각 원자적이다. 다른 user_id 캐시는 읽지 않는다 |
| 적용기 | `Sync/SyncApplier.swift` | §7 계약 전부(아래). 순수 값 타입이라 fixture로 재생한다 |
| 엔진 | `Sync/SyncEngine.swift` | cursor 없으면 bootstrap, 있으면 `has_more` false까지 페이지(페이지마다 저장). `410`이고 **code가** `sync_cursor_expired`일 때만 bootstrap으로 교체(같은 410인 `party_disbanded`와 구별). 동시 호출은 한 번을 공유 |
| 큐 | `Sync/MutationQueue.swift` | 파일 저장. Idempotency-Key와 **본문 바이트**를 저장해 재실행 후에도 그대로 재전송. 파일에 주인 user_id를 두고 다른 사용자(또는 주인 없는) 큐는 버린다. 2xx 완료, 탈퇴는 410 `party_disbanded`(code로 판정)도 완료. 네트워크·5xx·429·401은 남기고 멈춤(순서 보존), 세션이 끝났으면 저장 없이 멈춤, 그 밖 4xx는 failed로 옮김. 저장이 실패하면 멈추고, 한 flush는 시작할 때 있던 개수만큼만 돈다. flush는 single-flight |
| 화면 | `ServerDebugView.swift` | 개발용 로그인, sync, 캐시 개수, Party 이름, 로그아웃. 디자인 없음 |

### 적용 규칙 (`SyncApplier`, 기준 구현 `party_sync_test.go`의 `client.apply`)

- upsert는 version이 로컬과 하한보다 클 때만. 부모 Party보다 먼저 온 하위 entity도 보관
- version 있는 tombstone: 지우고 하한 기록. version 없는 tombstone: 지우기만
- 규칙 (a): `scope_membership_id`가 하한이 기록된 멤버십이면 upsert 버림
- 규칙 (b): version 없는 tombstone은 같은 Party에 scope가 아닌 본인 활성 멤버십이 있으면 무시
- bootstrap: 옛 entity·하한을 버리고, 하한 항목(같은 entity 여러 개면 최대, scope null 가능)을 먼저 기록한 뒤 하한보다 큰 upsert만 넣는다
- 기기 폐기(`revoked: true` upsert): 목록에서 빼고 그 version을 하한으로
- 본인 멤버십 종료(version 있는 본인 멤버십 tombstone. 본인 것은 scope가 그 멤버십 자신이다): 그 Party의 entity(Party·멤버십·초대·공개 설정·projection)를 모두 지운다. **단 같은 Party에 다른 본인 활성 멤버십이 있으면 지우지 않는다** — 재현 3(재가입 부트스트랩이 옛 탈퇴보다 먼저 옴)에서 무조건 지우면 재가입 데이터가 사라진다
- `availability_preference`: 저장하지 않고 `needsPreferenceRefresh`만 켠다. bootstrap 뒤에도 켠다(§7.2)

## 결정

- **저장소: SwiftData 대신 JSON 파일 하나.** 캐시는 수백 entity이고 질의는 Party별 필터뿐이다. 한 파일 원자 교체가 "bootstrap을 한 트랜잭션에서 교체"(§7.2)를 가장 단순하게 만족한다. 설계 §7.3의 SwiftData·App Group은 알림 확장(설계 9)이 캐시를 읽어야 할 때 옮긴다. `notification_ref_key`도 그때 공유 Keychain에 둔다(I1은 읽지 않는다)
- **ATS:** `src/PlanTogether/Info.plist`(`NSAllowsLocalNetworking`)를 `INFOPLIST_FILE`로 연결하고 `GENERATE_INFOPLIST_FILE: YES`로 합친다. 빌드 산출물 plist에서 ATS 키와 캘린더 사용 설명이 모두 있는 것을 `plutil`로 확인했다. `INFOPLIST_FILE`을 쓰자 `INFOPLIST_KEY_NSCalendarsFullAccessUsageDescription` 빌드 설정 값이 산출 plist에 보이지 않아 캘린더 설명을 같은 파일로 옮겼다(XcodeGen이 sources의 Info.plist를 스스로 잡아 원인을 따로 떼어 보지는 못했다)
- **테스트 target Info.plist:** `PlanTogetherTests`에 `GENERATE_INFOPLIST_FILE: YES`가 없어 `xcodebuild test`가 코드 서명 단계에서 실패했다(main에서도 같다). 켰다
- **fixture 방식:** 별도 Go 테스트를 만들지 않고 기존 인수·역전 재현 테스트에 수동 기록기를 붙였다(`server/internal/syncfeed/swift_fixture_test.go`). `DUMP_SWIFT_FIXTURES=<디렉터리>`일 때만 세션별 bootstrap·페이지 원문·checkpoint(converge 시점 bootstrap)·최종 기대 모델을 기록하고 **테스트가 통과한 뒤에만** 쓴다. 변수가 없으면 요청·쿼리를 더하지 않아 CI 시간·순서가 그대로이고, 건너뛰는 테스트도 생기지 않는다. `request_id`·`server_time`·`notification_ref_key`는 지운다. 생성 명령은 그 파일 머리말에 있다
- **대조군:** fixture마다 끄면 bootstrap과 달라져야 하는 스위치를 적는다. rules(재현 1·3·5), floors(재현 2·5·6·7), orphans(재현 4). 재현 1·2·3·5·6·7의 대조군은 서버 Go 테스트가 먼저 증명한 것이고, 재현 5의 rules와 재현 4의 orphans는 Swift 대조군 테스트로만 확인했다. 재현 8은 서버 성질(snapshot entity에 하한을 싣지 않음)이라 클라이언트 대조군이 없다고 명시했다
- **fixture 리소스:** `PlanTogetherTests/Fixtures/*.json`은 XcodeGen이 sources 아래 JSON을 자동으로 test target 리소스에 넣는다(`project.yml`에 따로 `resources:`를 적지 않았다). 빠지면 `SyncFixtureTests`의 시나리오 집합 단언이 실패한다
- **큐 범위:** 최소. flush는 single-flight(동시 호출이 같은 mutation을 겹쳐 보내지 않는다), 생성 순서 직렬 전송, 독립 entity 병렬 전송과 bootstrap 뒤 의존 version 재검증(§7.2 마지막 항목)은 화면이 mutation을 만들기 시작하는 I3에서 한다

## 설계 대비 편차

- **A0 전제 변화:** 설계 6의 A0(Swift 계약 테스트)는 iOS가 가능 시간을 계산한다는 전제였지만 검색은 서버로 옮겨졌다(A1~A4). `AvailabilityCalculator`와 테스트 4개는 데모 탭이 쓰므로 I1에서는 남기고, **I3에서 서버 검색 화면으로 바꿀 때 제거한다**
- §7.3의 SwiftData·App Group → JSON 파일(위 결정)
- §7.3 큐 필드 중 `entity_type`·`entity_id`·`expected_version` 대신 method·path·본문·If-Match를 저장한다. 의존 재검증을 붙일 때 더한다

## 발견한 서버 공백

- **로그인이 새 기기의 sync 변경을 발행하지 않는다**(`auth.signInTx`의 `INSERT INTO devices`). 같은 사용자의 기존 세션은 새 기기를 bootstrap이나 `GET /v1/devices`로만 안다. 순차 fixture의 B1이 B2 기기를 증분으로 받지 못해 드러났다. Swift fixture 비교는 그 세션이 bootstrap·페이지로 **한 번도 받지 못한** 기기의 누락만 허용한다. 받은 기기가 빠지거나, 남거나(폐기 기기 부활), version이 다르면 실패다. 기기 목록 화면을 만들 때 서버에서 발행하거나 화면이 GET을 쓰도록 정한다

## 테스트

- iOS: `xcodebuild … build test` 53개 실행 — 52개 통과(기존 4개 포함), 1개(실서버 스모크) 건너뜀. iPhone 16(iOS 18.3.1)과 iPhone 15(iOS 17.4)에서 같다. 시뮬레이터 런타임이 18.3.1이라 `OS=18.3` destination은 맞지 않아 기기 id로 지정했다
  - API: 인증 헤더·Idempotency-Key·If-Match 따옴표·본문, 오류 해석, 429 Retry-After, 동시 401 다섯 개 → refresh 1회와 각 요청 1회 재시도(같은 키·본문), refresh 429 재시도(로그아웃 없음), refresh 401·423 세션 종료, refresh 네트워크 오류 시 재전송 없음·세션 유지, 개발용 로그인
  - 날짜(소수 0~9자리, 오프셋), JSONValue Bool·숫자 구분, snake_case 키
  - fixture 9개 시나리오(순차 인수 5세션 — limit=2 페이지·늦은 bootstrap 포함, 재현 8개): checkpoint와 최종 모델이 bootstrap과 같음. 대조군 rules·floors·orphans가 해당 fixture에서 달라짐
  - 적용기 단위(하한, 규칙 (a)(b), 캐시 삭제 조건, 기기 폐기, 활동 시간 표시, bootstrap 교체), 파일 저장소 원자 교체, 엔진 페이지·410 재bootstrap·다른 410 구별·다른 사용자 캐시 무시
  - 세션 경계(리뷰 반영): flush 중 refresh 401로 세션 종료 → 큐·캐시 파일 없음, 로그아웃 중 끝난 flush·sync → 파일 없음·오류 표시 없음, 다른 사용자·주인 없는 큐 폐기, 로그아웃 중 끝난 refresh → Keychain 비어 있음, refresh 뒤 Keychain 저장 실패 → 새 token 사용·refresh 재전송 없음·다음 요청에서 저장, 큐 저장 실패 → 무한 재전송 없음. 각 테스트는 수정 전 동작으로 되돌리면 실패하는 것을 확인했다
  - 큐: 재실행 후 같은 키·본문 재전송, 동시 flush에서 한 번만 전송, 탈퇴 200·410 완료, 거절은 failed, 429에서 순서 보존. 로그아웃이 Keychain·캐시·큐를 지움, Keychain 왕복
- 실서버 스모크: `run_api_dev.sh`로 띄운 서버에 `TEST_RUNNER_PLAN_LIVE_SERVER_URL=http://localhost:8080`으로 실행해 통과(로그인 → `/v1/me` → bootstrap → 증분 → Party 생성 → 증분에 Party·멤버십 도착 → 로그아웃, 서버 로그 전부 2xx). 앱 프로세스에서 http://localhost가 ATS에 막히지 않음을 함께 확인했다
- 서버: gofmt·vet 통과, `REQUIRE_DB_TESTS=1 go test -count=1 ./...` 596개 통과, skip 0

## 남은 것

- I2: 캘린더 snapshot 업로드. I3: 화면(Party·초대·공개 수준·활동 시간·가능 시간 검색)과 `AvailabilityCalculator` 제거, `needsPreferenceRefresh` 소비
- Sign in with Apple(nonce, `ASAuthorizationController`)
- 큐: 의존 version 재검증, 독립 entity 병렬 전송, 앱 활성화·네트워크 복구 시 자동 flush
- 알림 단계에서 캐시를 App Group으로, `notification_ref_key`를 공유 Keychain으로
- 위 서버 공백(새 기기 sync 변경)
