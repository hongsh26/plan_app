# 알림 종류와 전송 규칙 상세 설계

## 1. 설계 상태와 핵심 결정

- 상세 설계 진행 순서 9번. 선행 설계: 2(계정·서버), 3(캘린더 권한·동기화), 4(Party 멤버십), 5(공개 수준), 6(가능 시간 검색), 7(제안·응답·확정), 8(캘린더 쓰기)
- 후속 설계: 10(결제), 11(계정 삭제·분석·관측)
- 이 설계는 선행 설계 4·7·8이 발행하는 domain outbox 이벤트를 실제 푸시 전송으로 옮기는 규칙을 확정한다. 이벤트를 **만드는** 조건은 각 선행 설계가 계속 소유하고, 이 설계는 **수신자·채널·urgency·문구·quiet hours·묶음·중복 억제**만 소유한다.

### 핵심 결정

1. **채널을 둘로 나눈다.** silent push(백그라운드 sync 재촉)와 alert push(사용자에게 보이는 알림)는 서로 다른 규칙을 따른다. 한 이벤트가 둘 다 만들 수도, 하나만 만들 수도, 아무것도 만들지 않을 수도 있다.
2. **문구는 기기에서 완성한다.** APNs payload는 localization key와 opaque 참조만 담고 사람이 읽을 문자열을 담지 않는다. Notification Service Extension(NSE)이 이미 동기화된 로컬 데이터로 Party 이름을 채운다. 로컬 데이터가 없으면 일반 문구로 낮춘다. 이로써 설계 5·7·8의 "APNs payload는 opaque marker만" 확정을 개정 없이 유지한다.
3. **Party와 entity 참조는 사용자별 HMAC 참조다.** payload에 UUID를 직접 담지 않는다. 서버는 사용자 전용 `notification_ref_key`로 파생한 `party_ref`/`entity_ref`를 담고, 기기가 같은 키로 로컬 후보를 대조해 역조회한다. 키는 인증된 `/sync` 채널로만 전달한다.
4. **만료 판정은 서버가 소유한다.** 모든 알림 job은 `valid_until`과 발송 직전 precondition 재확인을 가진다. 기한이 지났거나 대상 상태가 이미 해소되었으면 발송하지 않는다. NSE는 알림을 억제할 수 없으므로 억제 수단으로 쓰지 않는다.
5. **중복 억제는 outbox index에 의존하지 않는다.** `outbox_jobs`의 partial unique index는 활성 상태에만 걸려 있어 성공 후 같은 key의 재삽입을 허용한다. 그래서 설계 9는 `(notification_key, user_id)` 단위의 영속 전달 기록을 따로 둔다. 설계 8의 `calendar_write_command_pending` 3회 재발행은 이와 무관하게 `command_id:notice_seq`로 key 자체가 달라서 통과한다.
6. **설계 4가 확정한 수신자 집합은 바꾸지 않는다.** 설계 4 §9.3의 6개 job은 수신자를 이미 확정했고 설계 9에 문구·urgency·quiet hours·묶음만 위임했다. 설계 9는 그 집합 안에서 행위자에게 alert 대신 silent만 보내는 선택만 한다.
7. **quiet hours의 결과는 두 가지다.** bypass(그대로 발송)와 defer(창이 끝날 때까지 미룸). 2분 기한의 refresh 요청만 bypass한다. 기한이 걸린 알림을 8시간 뒤에 배달하지 않는 것은 별도 모드가 아니라 §4.6의 `valid_until` 확인이 모든 job에 적용되어 해결한다.
8. **선행 설계 2건에 개정 4항목을 넣는다.** 설계 8 §9.3에 `calendar_cleanup_suggested` outbox type을 추가하고(§5.2), 설계 2에 `devices` 알림 열 3개, bootstrap·`/v1/me`의 `notification_ref_key`, SwiftData의 App Group 공유 컨테이너 이전을 추가한다. 전체 목록과 이유는 §17.2에 있다.

### 백로그 미결 질문의 답

| 질문 | 답 |
|---|---|
| 초대 알림을 어떻게 보내는가 | **초대 발송 푸시는 없다.** 설계 4는 재사용 링크 + 앱 내 계정 필수를 확정했다. 링크는 앱 밖(메시지·공유 시트)에서 전달되므로 서버가 아는 수신자가 없다. 가입·수락 후 기존 멤버에게 `notify_member_joined`만 간다. |
| 응답 하나하나를 전원에게 알리는가 | 아니다. 수락은 silent만 보낸다. 앱에서 sync로 본다. 거절만 제안 생성자에게 alert로 간다. 재제안이 필요한 행동 신호이기 때문이다. |
| 확정 알림이 두 번 가는가 | 아니다. 설계 7의 `proposal_confirmed`와 설계 8의 `confirmed_event_created`는 같은 transaction에서 같은 대상에게 발행된다. alert 소유권은 `proposal_confirmed`에 두고 `confirmed_event_created`는 silent만 만든다. |
| 사용자가 알림을 전부 끌 수 있는가 | 그렇다. 4개 카테고리 전부 앱 안에서 끌 수 있다. iOS 시스템 설정으로도 꺼지므로 앱이 강제할 수 없다. 끄면 앱 내부 상태로만 확인한다고 설정 화면에 명시한다. |

---

## 2. 목표와 사용자 가치

1. 약속을 만들고 확정하는 흐름에서 **행동이 필요한 순간**에만 알림을 받는다.
2. 다른 사람의 일정 제목·장소·시각과 응답 내용이 잠금 화면에 나타나지 않는다.
3. 한 Party의 알림이 알림 센터에서 하나로 묶여 여러 Party를 함께 쓰는 사용자가 출처를 구분한다.
4. 밤에는 기한이 걸린 알림만 울리고 나머지는 아침에 모아서 받는다.
5. 알림을 껐거나 놓쳐도 앱을 열면 동일한 상태를 sync로 확인한다. 알림은 가속 수단이지 유일한 전달 경로가 아니다.

---

## 3. 포함 범위와 제외 범위

### 포함

- domain outbox 20종(설계 4의 6종, 설계 7의 9종, 설계 8의 4종 + 신규 1종)에 대한 채널·수신자·urgency·quiet hours·묶음 정책
- silent push 예산과 합산 규칙
- APNs payload 계약과 사용자별 opaque 참조 파생
- NSE 문구 완성과 낮춤 규칙
- 알림 설정 리소스(카테고리 4종 토글, quiet hours)
- 알림 job 모델, fan-out 3단계 멱등성, 전달 기록과 억제 창
- 푸시 권한 거부·토큰 무효·기기 폐기 경로
- writer device 우선 발송

### 제외

- 이벤트 발행 조건과 dedupe key 정의 → 설계 4·7·8이 소유
- 앱 내부 알림함(inbox) 화면. 앱 상태는 sync가 기준이며 별도 알림 이력 entity를 만들지 않는다
- 이메일·SMS·웹 푸시. Launch MVP는 APNs 단일 채널
- 알림에서 바로 수락·거절하는 action button → Post-MVP
- Live Activity, Widget, critical alert entitlement
- 마케팅·재참여 알림
- 결제·구독 관련 알림 → 설계 10

### 명시적 비목표

- 알림 도달을 보장하지 않는다. silent push가 best-effort라는 것은 설계 3 §7.3이, 앱 활성화 시 항상 sync한다는 것은 설계 2 §7.3이 확정했다.
- 다른 멤버의 캘린더 반영 상태를 집계 형태로도 알리지 않는다. 설계 8 §9.1을 그대로 따른다.
- 알림 문구로 누가 어떤 응답을 했는지 알려주지 않는다.

---

## 4. 확정된 제품 정책

### 4.1 두 채널

| 항목 | silent push | alert push |
|---|---|---|
| `apns-push-type` | `background` | `alert` |
| `apns-priority` | 5 | 10(time_sensitive/active), 5(passive) |
| payload | `content-available: 1` + `sync_needed` marker | loc-key + opaque 참조 + `mutable-content: 1` + `content-available: 1` |
| 목적 | 앱을 깨워 `/sync`를 당긴다 | 사용자에게 행동을 알린다 |
| 권한 | 푸시 권한과 무관하게 전송 가능 | `UNAuthorizationStatus == authorized/provisional`일 때만 |
| quiet hours | 적용하지 않는다 | 적용한다 |
| 예산 | 사용자당 합산·제한(§4.5) | 이벤트별 정책대로 |

- alert payload에 `content-available: 1`을 함께 실어 앱 본체를 깨우려 시도한다(§10.2). 그래서 같은 사용자에게 같은 순간 alert가 나가면 별도 silent를 만들지 않는다. iOS가 이 조합의 백그라운드 실행을 보장하지 않으므로 이 절약은 best-effort이며, 실패해도 앱 활성화 sync가 상태를 복구한다.
- silent push는 delivery를 가정하지 않는다. 설계 2 §7.3의 "background notification이 도착하지 않아도 앱 활성화 시 항상 sync"가 유일한 보장이다.

### 4.2 알림 문구 생성

서버는 문자열을 만들지 않는다. payload는 `loc_key`(예: `proposal_created`)와 `party_ref`, `entity_ref`만 담는다.

NSE 동작 순서:

1. `loc_key`로 앱 번들 `Localizable.strings`의 기본 문구를 읽는다.
2. `party_ref`를 로컬 Party 목록에 대조한다(§4.2.1).
3. 일치하면 Party 이름을 넣은 변형 문구로 교체한다.
4. 일치하지 않거나 로컬 저장소를 열 수 없으면 기본 문구를 그대로 쓴다.
5. `thread-id`와 `collapse-id`는 payload 값을 그대로 유지한다.

| 상황 | 결과 |
|---|---|
| 로컬 Party 있음 | `'주말 모임'에 새 제안이 있어요` |
| 첫 실행, bootstrap 이전 | `새 제안이 있어요` |
| NSE 예산(30초/24MB) 초과 | `새 제안이 있어요` |
| 사용자가 이미 떠난 Party | `새 제안이 있어요` |

- NSE는 네트워크를 쓰지 않는다. 로컬 SwiftData 읽기 전용으로만 동작한다.
- **NSE는 앱 본체와 별도 샌드박스에서 실행된다.** 앱의 SwiftData 저장소를 읽으려면 저장소가 App Group 공유 컨테이너에 있어야 하고, `notification_ref_key`를 읽으려면 키가 공유 keychain access group에 있어야 한다. 둘 다 설계 2의 개정을 요구한다(§17.2).
- NSE는 일정 제목·장소·시각을 문구에 넣지 않는다. Party 이름과 카테고리 문구까지만 허용한다.
- NSE가 알림을 억제할 수는 없다. 만료·해소 판정은 서버가 발송 전에 끝낸다(§4.6).

#### 4.2.1 사용자별 opaque 참조

```text
notification_ref_key = 사용자별 32바이트 무작위 키
party_ref  = base64url( 첫 16바이트( HMAC-SHA256(notification_ref_key, "p:" || party_id) ) )
entity_ref = base64url( 첫 16바이트( HMAC-SHA256(notification_ref_key, "e:" || entity_type || ":" || entity_id) ) )
```

- `notification_ref_key`는 본인 전용 리소스이며 `/v1/sync/bootstrap`과 `/v1/me` 응답으로만 전달한다. payload에 절대 담지 않는다.
- 잘림은 **첫 16바이트**이며 base64url 인코딩 후 22자다. APNs `apns-collapse-id`의 64바이트 제한을 여유 있게 만족한다. 서버(Go)와 기기(Swift)가 각자 구현해 대조하는 값이므로 바이트 수와 인코딩을 여기서 고정한다. `entity_type`과 `entity_id`는 §5.1 표 A의 값을 그대로 쓴다.
- 기기는 로컬이 아는 Party와 entity에 대해 같은 함수로 ref를 미리 계산해 역조회 표를 만든다.
- 같은 Party라도 사용자마다 ref가 다르므로 APNs 경유 트래픽으로 사용자 간 연결이 드러나지 않는다.
- 키 회전 시 서버는 새 키로만 발행하고, 기기는 bootstrap 이후 표를 다시 만든다. 회전 직후 도착한 구 키 알림은 기본 문구로 낮아질 뿐 실패하지 않는다.
- **위협 모델을 명시한다.** 이 파생이 막는 것은 *사용자 간 연결*이다. 같은 Party의 두 멤버가 받는 알림을 APNs 경로에서 같은 Party로 묶을 수 없다. 막지 못하는 것은 *단일 사용자에 대한 관찰*이다. device token을 이미 가진 관찰자는 `thread-id`의 분포로 "이 사람은 Party를 N개 쓰고 그중 하나에서 주당 M건을 받는다"를 축적할 수 있다. payload에 이름·UUID·시각이 없으므로 얻을 수 있는 정보는 빈도와 개수로 제한되며, 축적 기간은 키 회전 주기가 정한다.
- 회전 시점에 이미 만들어진 `deferred` job은 **구 키로 파생한 ref를 유지한다.** 발송 시점에 다시 파생하면 기기가 그 사이에 새 키를 받았을 때만 맞고 아니면 틀리는데, defer된 job은 바로 그 시차를 겪는다. 구 키를 유지하면 이미 배달된 같은 Party 알림과 `thread-id`가 일치해 묶음이 깨지지 않는다. `notification_ref_keys`는 이를 위해 직전 키 1개를 `previous_ref_key_ciphertext`로 보관하고 90일 뒤 지운다.
- `notification_ref_key`의 회전 주기 상한은 **90일**이다. `notification_deliveries` 보관 기간과 같게 두어 운영을 단순하게 유지한다. 더 짧은 주기와 자동화 방식은 설계 11이 정한다.

### 4.3 urgency 등급

| 등급 | `interruption-level` | `apns-priority` | 소리 | quiet hours |
|---|---|---:|---|---|
| `time_sensitive` | `time-sensitive` | 10 | 있음 | bypass |
| `active` | `active` | 10 | 있음 | defer |
| `passive` | `passive` | 5 | 없음 | defer |
| `silent` | — | 5 | — | 해당 없음 |

- `critical` 등급은 쓰지 않는다. Apple entitlement가 필요하고 이 제품의 어떤 알림도 그 기준을 충족하지 않는다.
- `time_sensitive`는 기한이 분 단위인 알림 하나(`proposal_calendar_refresh_requested`)에만 쓴다. 남용하면 iOS가 사용자 설정으로 전체를 차단한다.
- **`time_sensitive`도 entitlement가 필요하다.** `com.apple.developer.usernotifications.time-sensitive` entitlement, Developer Console의 Time Sensitive Notifications capability 활성화, provisioning profile 갱신이 모두 있어야 한다. 없으면 `interruption-level`이 조용히 무시되어 quiet hours를 뚫는 유일한 경로가 작동하지 않는다.
- 사용자는 `UNNotificationSettings.timeSensitiveSetting`으로 이 등급을 앱별로 끌 수 있다. 기기가 `PUT /v1/devices/{id}/push-token`으로 이 상태를 보고하며(§8.1), 꺼져 있으면 서버는 `interruption-level`만 `active`로 강등하고 **quiet hours `bypass`는 유지한다.** quiet hours까지 적용하면 2분 기한 알림이 최대 23.5시간 미뤄져 §4.6에서 확정적으로 폐기되고, 강등이 곧 전면 차단이 된다. `active`로도 즉시 전달은 이루어지며 시스템 Focus만 뚫지 못한다.

### 4.4 quiet hours

- 기본값은 **비활성**이다. 사용자가 켜면 기본 구간은 로컬 시각 `22:00`–`08:00`이며 조정할 수 있다.
- 시간대는 **설계 6 §7.1의 활동 시간 `time_zone`(사용자 전역 IANA ID)을 그대로 쓴다.** 별도 시간대 소스를 만들지 않는다. 활동 시간이 미설정이면 quiet hours도 적용하지 않는다.
- 구간이 자정을 넘는 설정을 허용한다(`22:00`–`08:00`).
- DST로 로컬 시각이 건너뛰거나 겹치면 설계 6 §3.3과 **같은 원리를 반대 방향으로** 적용한다. 설계 6의 활동 시간은 가용 창이라 구간을 넓히는 쪽(시작=첫 occurrence, 종료=두 번째)을 택하지만, quiet hours는 억제 창이므로 같은 선택을 하면 조용한 시간이 불필요하게 길어진다. quiet hours는 **구간을 좁히는 쪽**을 택한다: gap에 걸린 경계는 gap 직후 유효 시각으로 밀고, overlap은 시작에 두 번째 occurrence, 종료에 첫 번째 occurrence를 쓴다. 그 결과 길이가 0 이하가 되면 그 날짜에는 quiet hours를 적용하지 않는다.

| 결과 | 동작 |
|---|---|
| `bypass` | 구간과 무관하게 즉시 발송 |
| `defer` | `deferred_until = 구간 종료 시각`으로 미룬다. 창이 끝나면 발송한다. 단 §4.6의 `valid_until` 확인이 모든 job에 적용되므로, 창 안에서 만료된 job은 창이 끝날 때 `dropped`된다 |

`drop_if_stale`을 따로 두지 않는다. defer된 job도 §4.6에서 만료 확인을 거치므로 동작이 같고, 이름만 셋이고 동작이 둘이면 구현자가 다른 코드 경로를 만든다. 대신 만료가 실제로 걸려야 하는 type은 §5.1 표 B가 `valid_until`에 유한 상한을 둔다.

- defer된 같은 카테고리의 알림이 창 안에 여러 건 쌓이면 창이 끝날 때 카테고리당 1건으로 합산한다(§4.5). `bypass` type은 defer되지 않으므로 합산 대상이 아니다.

### 4.5 묶음과 예산

**alert 묶음**

- `thread-id = party_ref`. iOS 알림 센터가 같은 Party의 알림을 하나로 묶는다.
- `apns-collapse-id = entity_ref`. 같은 entity의 후속 알림이 잠금 화면과 알림 센터에서 이전 것을 대체한다. entity 단위는 §5.1 표 A가 type별로 확정한다. 서로 다른 사건이 같은 entity로 묶이지 않게 하는 것이 중요하며, Party 묶음은 `thread-id`가 담당한다.
- quiet hours defer 합산: 창 종료 시 `(user_id, party_id, category)`당 1건만 남기고 나머지는 `coalesced`로 끝낸다. 남기는 것은 가장 늦게 생성된 건이며, 2건 이상이 합쳐졌으면 `loc_key`에 `_multi` 접미사를 붙인다(`'주말 모임'에 새 소식이 있어요`).

**silent 예산**

- 사용자당 최소 간격 60초. 창 안의 모든 silent 요청을 1건으로 합산한다.
- 사용자당 최근 60분 이동 창에서 최대 3건. 고정 시간 창을 쓰면 10:59에 3건, 11:01에 3건이 가능해 상한이 무너진다. Apple이 background notification을 throttle하므로 그 이상은 어차피 의미가 없다.
- 초과분은 버린다. 재시도하지 않는다. 앱 활성화 sync가 대체 경로다.
- **silent는 mapper가 job을 만드는 시점에 즉시 발행한다.** alert의 defer를 기다리지 않는다. silent에는 quiet hours가 적용되지 않으므로(§4.1) 밤사이에도 sync가 당겨져야 한다.
- 억제는 alert가 **defer 없이 즉시 발송 대상일 때만** 적용한다. defer되는 alert는 몇 시간 뒤에 나가므로 지금의 silent를 대신하지 못한다. precondition에서 `dropped`된 alert도 억제하지 않는다 — 나가지 않은 alert는 앱을 깨우지 않는다.

### 4.6 발송 직전 precondition

모든 알림 job은 발송 직전에 아래를 다시 확인한다. 하나라도 어긋나면 `dropped`로 끝내고 APNs를 호출하지 않는다.

| 확인 | 폐기 조건 |
|---|---|
| `valid_until` | `now() > valid_until` |
| 수신자 자격 | §5.1 표 A의 `recipient_scope`가 `active_members_only`인데 수신자가 더 이상 활성 멤버가 아니다. `snapshot_at_publish`와 `self_only`는 이 확인을 하지 않는다 |
| 대상 상태 | 해당 entity가 이미 해소되었다(§5.1 `precondition` 열) |
| 사용자 설정 | 해당 카테고리가 꺼져 있다 |
| 기기 | 활성 push token이 없거나 `push_authorization`이 `denied`. 이 경우 alert만 `dropped`되고 silent는 계속 나간다. `dropped`된 alert는 §4.5의 silent 억제를 발동시키지 않는다 |
| 계정 | 사용자가 삭제 요청 상태다 |

### 4.7 writer device 우선

`calendar_write_command_pending`, `calendar_write_action_required`, `calendar_cleanup_suggested`는 본인 전용이며 EventKit 실행 기기에 관한 알림이다.

- 사용자가 지정한 writer device가 활성 token을 가지면 **그 기기에만** 보낸다.
- writer device가 없거나 폐기·토큰 무효 상태면 사용자의 모든 활성 기기에 보낸다. 이 경우 `loc_key`에 `_no_writer` 변형을 쓴다(`캘린더에 반영할 기기를 선택해주세요`).
- `calendar_cleanup_suggested`는 예외적으로 `_no_writer` 변형을 쓰지 않는다. 이 알림의 수신자는 떠난 사용자·해산 Party 멤버라 `calendar_connection`이 이미 정리되었을 수 있고, 의도가 "반영하세요"가 아니라 "지우세요"이기 때문이다. writer device가 없으면 기본 문구를 그대로 전 기기에 보낸다.
- 다른 알림은 사용자의 모든 활성 기기에 보낸다.

### 4.8 행위자 처리

- 자기 행동의 결과는 알리지 않는다. 행위자에게는 alert 대신 silent만 보낸다.
- 예외: `notify_member_removed`의 **강퇴 대상**은 행위자가 아니라 대상이며 alert를 받는다.
- 예외: `notify_party_disbanded`의 해산 실행자는 행위자이므로 alert를 받지 않는다. 설계 4가 단독 방장 탈퇴 경로에서 이 job 자체를 발행하지 않는 것과 일관된다.

### 4.9 알림 카테고리

사용자에게 노출하는 토글은 4개다. 20종 type을 그대로 노출하지 않는다.

| 카테고리 | 포함 type | 기본값 |
|---|---|---|
| `party` | `notify_member_joined`, `notify_member_left`, `notify_member_removed`, `notify_owner_transferred`, `notify_owner_transferred_by_deletion`, `notify_party_disbanded` | 켜짐 |
| `proposal` | `proposal_created`, `proposal_response_changed`, `proposal_expired`, `proposal_cancelled`, `proposal_superseded`¹ | 켜짐 |
| `confirmation` | `proposal_confirmed`, `proposal_conflict_detected`, `proposal_reproposal_required`, `confirmed_event_created`¹, `confirmed_event_cancelled` | 켜짐 |
| `calendar_action` | `proposal_calendar_refresh_requested`, `calendar_write_command_pending`, `calendar_write_action_required`, `calendar_cleanup_suggested` | 켜짐 |

¹ silent 전용이라 `notification_jobs` row도 토글도 만들지 않는다. 표 A의 `category` 값은 문서상 분류일 뿐이며, §14의 카테고리별 지표는 alert에만 존재한다.

20종이 정확히 한 카테고리에 배정된다. 어떤 type도 두 카테고리에 속하지 않으며 배정은 §5.1 표 A의 `category` 열과 일치한다.

- 카테고리를 끄면 해당 type의 alert만 멈춘다. silent push와 sync는 계속한다.
- 설정 화면에 "끄면 앱을 열었을 때만 확인할 수 있습니다"를 표시한다.

---

## 5. 이벤트 → 알림 매핑

### 5.1 매핑 표

`A` = alert, `S` = silent. 수신자 열의 "설계 4 확정"은 설계 4 §9.3의 집합을 그대로 물려받았다는 뜻이다.

수신자·`recipient_scope`·entity·category·채널·urgency·quiet·`valid_until`·precondition은 이 두 표가 전부 정한다. 표 밖에서 결정되는 것은 기기 선택(§4.7), `time_sensitive_setting`이 꺼진 사용자의 강등(§4.3), 창 종료 합산(§4.5), 수신자 역할에 따른 urgency 분기(§5.3) 네 가지뿐이며 모두 수신자 집합이 아니라 전달 방식에만 관여한다.

`recipient_scope`의 의미:

| 값 | 뜻 |
|---|---|
| `active_members_only` | 발송 직전에 수신자가 여전히 활성 멤버인지 재확인한다. 아니면 폐기한다 |
| `snapshot_at_publish` | 발행 시점에 수신자였던 사람을 유지한다. 그 뒤 멤버십이 끝나도 폐기하지 않는다. 목록을 저장하지 않고 **발행 시각 기준으로 재구성**한다: `status = 'active' OR ended_at >= outbox_jobs.created_at`. `party_memberships.ended_at`(설계 4)과 `parties.disbanded_at`이 이미 있으므로 추가 저장이 필요 없다 |
| `self_only` | 수신자가 본인 1명이다. 멤버십을 확인하지 않는다 |

#### 표 A. 수신자와 식별

| outbox type | 소유 | category | 수신자 | `recipient_scope` | `entity_type` | `entity_id` |
|---|---|---|---|---|---|---|
| `notify_member_joined` | 4 | `party` | 신규 멤버 제외 활성 멤버 (설계 4 확정) | `active_members_only` | `party` | `membership_id` |
| `notify_member_left` | 4 | `party` | 남은 활성 멤버 (설계 4 확정) | `active_members_only` | `party` | `membership_id` |
| `notify_member_removed` | 4 | `party` | 강퇴 대상 + 남은 활성 멤버 (설계 4 확정) | `snapshot_at_publish` | `party` | `membership_id` |
| `notify_owner_transferred` | 4 | `party` | 활성 멤버 전원 (설계 4 확정) | `active_members_only` | `party` | `party_id:party_version` |
| `notify_owner_transferred_by_deletion` | 4 | `party` | 활성 멤버 전원 (설계 4 확정) | `active_members_only` | `party` | `party_id:party_version` |
| `notify_party_disbanded` | 4 | `party` | 해산 직전 활성 멤버 (설계 4 확정) | `snapshot_at_publish` | `party` | `party_id` |
| `proposal_created` | 7 | `proposal` | 생성자 제외 참여자 | `active_members_only` | `proposal` | `proposal_id` |
| `proposal_response_changed` | 7 | `proposal` | **수락**: 없음 / **거절**: 생성자 | `active_members_only` | `proposal` | `proposal_id` |
| `proposal_calendar_refresh_requested` | 7 | `calendar_action` | refresh 대상 사용자 본인 | `self_only` | `proposal` | `proposal_id:confirm_attempt` |
| `proposal_confirmed` | 7 | `confirmation` | 참여자 전원 (행위자 제외 alert) | `snapshot_at_publish` | `confirmed_event` | `confirmed_event_id` |
| `proposal_conflict_detected` | 7 | `confirmation` | 참여자 전원 | `active_members_only` | `proposal` | `proposal_id` |
| `proposal_reproposal_required` | 7 | `confirmation` | 남은 활성 참여자 | `active_members_only` | `proposal` | `proposal_id` |
| `proposal_expired` | 7 | `proposal` | 참여자 전원 | `active_members_only` | `proposal` | `proposal_id` |
| `proposal_cancelled` | 7 | `proposal` | 취소자 제외 참여자 | `active_members_only` | `proposal` | `proposal_id` |
| `proposal_superseded` | 7 | `proposal` | 참여자 전원 | `active_members_only` | `proposal` | `old_proposal_id` |
| `confirmed_event_created` | 8 | `confirmation` | 참여자 전원 | `snapshot_at_publish` | `confirmed_event` | `confirmed_event_id` |
| `confirmed_event_cancelled` | 8 | `confirmation` | 취소자 제외 참여자 | `snapshot_at_publish` | `confirmed_event` | `confirmed_event_id` |
| `calendar_write_command_pending` | 8 | `calendar_action` | 본인, writer device 우선 | `self_only` | `calendar_command` | `command_id` |
| `calendar_write_action_required` | 8 | `calendar_action` | 본인, writer device 우선 | `self_only` | `calendar_command` | `command_id` |
| `calendar_cleanup_suggested` | 8(신규) | `calendar_action` | 본인, writer device 우선 | `self_only` | `cleanup` | `confirmed_event_id` |

- 설계 4의 6종은 `entity_id`가 Party가 아니라 **사건 단위**(`membership_id`, `party_id:party_version`)다. Party 단위로 잡으면 멤버 3명이 순차 합류할 때 `apns-collapse-id`가 앞의 알림을 덮어 사용자가 1건만 보게 된다. 묶음은 `thread-id = party_ref`가 담당하고 collapse는 같은 사건의 후속 알림에만 쓴다.
- `snapshot_at_publish`인 4종은 정의상 수신자가 활성 멤버가 아닐 수 있다. 강퇴 대상, 해산 직후 전원, 해산 transaction에서 취소된 확정 일정의 참여자, 떠난 사용자가 그렇다.

#### 표 B. 전송 정책

| outbox type | 채널 | urgency | quiet | `valid_until` | precondition |
|---|---|---|---|---|---|
| `notify_member_joined` | A+S | passive | defer | 생성 +7일 | Party 활성 |
| `notify_member_left` | A+S | passive | defer | 생성 +7일 | Party 활성 |
| `notify_member_removed` | A+S | 대상 active / 나머지 passive | defer | 생성 +7일 | — |
| `notify_owner_transferred` | A+S | 새 방장 active / 나머지 passive | defer | 생성 +7일 | Party 활성 |
| `notify_owner_transferred_by_deletion` | A+S | 새 방장 active / 나머지 passive | defer | 생성 +7일 | Party 활성 |
| `notify_party_disbanded` | A+S | active | defer | 생성 +7일 | — |
| `proposal_created` | A+S | active | defer | 제안 `response_due_at` | proposal이 `open` |
| `proposal_response_changed` | 수락 S / 거절 A+S | 수락 silent / 거절 active | defer | 제안 `response_due_at` | proposal이 `open`, 응답이 여전히 declined |
| `proposal_calendar_refresh_requested` | A+S | **time_sensitive** | **bypass** | `refresh_deadline_at` (+2분) | proposal이 `awaiting_calendar_refresh`, 같은 `confirm_attempt` |
| `proposal_confirmed` | A+S | active | defer | `min(약속 시작 시각, 생성 +36시간)` | confirmed event가 `active` |
| `proposal_conflict_detected` | A+S | active | defer | 제안 `response_due_at` | proposal이 `conflict_detected` |
| `proposal_reproposal_required` | A+S | active | defer | 생성 +7일 | — |
| `proposal_expired` | A+S | passive | defer | 생성 +3일 | — |
| `proposal_cancelled` | A+S | passive | defer | 생성 +3일 | — |
| `proposal_superseded` | **S만** | silent | — | — | — |
| `confirmed_event_created` | **S만** | silent | — | — | — |
| `confirmed_event_cancelled` | A+S | active | defer | `min(원래 약속 시작 시각, 생성 +36시간)` | — |
| `calendar_write_command_pending` | A+S | passive | defer | `min(다음 notice 예정 시각, 생성 +12시간)`. `notice_seq = 3`은 `생성 +12시간` | command가 여전히 `pending_device` |
| `calendar_write_action_required` | A+S | active | defer | 생성 +7일 | command가 `user_action_required` |
| `calendar_cleanup_suggested` | A+S | passive | defer | 생성 +30일 | cleanup suggestion row 존재 |

**표에서 확정한 것**

- `proposal_superseded`는 alert를 만들지 않는다. 대체 제안의 `proposal_created`가 같은 대상에게 이미 alert를 만들기 때문이다.
- `confirmed_event_created`는 alert를 만들지 않는다. 같은 transaction의 `proposal_confirmed`가 alert를 소유한다.
- `proposal_response_changed`는 수락에 alert가 없다. 10명 Party에서 9번 울리는 것을 막는다. 거절만 생성자에게 간다.
- `proposal_calendar_refresh_requested`만 quiet hours를 뚫는다. 기한이 2분이라 미루면 배달 시점에 이미 무의미하다.
- `calendar_write_command_pending`의 `valid_until`을 생성 +12시간으로 묶었다. 상한이 없으면 다음 notice가 24시간 뒤라 quiet hours 창(최대 23.5시간) 안에서 만료가 한 번도 걸리지 않고, 밤 사이 쌓인 알림이 아침에 그대로 나간다.
- `valid_until`에 약속 시작 시각을 쓰는 두 행은 `생성 +36시간` 상한을 함께 둔다. 몇 달 뒤 약속의 확정 알림이 그때까지 `pending`으로 남아 있을 이유가 없기 때문이다. `apns-expiration`은 §11.1대로 `valid_until`에서 파생되지 않으므로 이 상한이 privacy를 담당하지는 않는다.
- `proposal_calendar_refresh_requested`는 기한이 2분이라 worker 폴링 주기를 기다리지 않는다. 도메인 transaction을 커밋한 요청 handler가 커밋 **직후** worker를 깨워 이 outbox job을 즉시 claim하게 한다. §7.1의 경계는 그대로다. `notification_jobs`는 여전히 worker가 별도 transaction에서 만들며, 바뀌는 것은 claim 시점뿐이다. 일반 폴링 주기는 이 type의 2분 기한보다 짧아야 하므로 **30초를 상한으로 둔다.** 그래도 깨우기와 폴링이 모두 늦으면 §4.6이 `dropped`시키며 이 알림은 손실된다. 그 경우 §9.4의 `awaiting_calendar_refresh` 배너가 유일한 회복 경로다.

### 5.2 설계 8 개정: `calendar_cleanup_suggested` 추가

설계 8 §6.7은 떠난 사용자와 해산 Party의 마지막 활성 멤버에게 `confirmed_event` tombstone과 `calendar_cleanup_suggestions` row를 같은 transaction에서 발행한다. 그러나 §9.3의 outbox type 4종에는 대응 항목이 없어 푸시 경로가 없었다. 앱을 열지 않으면 외부 캘린더에 유효하지 않은 일정이 남은 사실을 알 수 없다.

설계 8 §9.3에 아래 1행을 추가한다.

| type | 계기 | dedupe key |
|---|---|---|
| `calendar_cleanup_suggested` | 설계 8 §6.7(membership 종료·Party 해산)에서 `calendar_cleanup_suggestions` row 생성 | `confirmed_event_id:user_id` |

- 수신자는 본인 1명이며 다른 멤버에게 어떤 형태로도 노출하지 않는다. 설계 8 §9.1의 본인 전용 원칙을 유지한다.
- 떠난 사용자에게 가는 알림이므로 §4.6의 "활성 멤버" precondition을 적용하지 않는다. precondition은 cleanup suggestion row의 존재다.
- 같은 사용자에게 여러 건이 동시에 생기면(해산 시 확정 일정 다수) §4.5의 창 합산이 1건으로 줄인다.

### 5.3 dispatch mapper

설계 4는 이미 `notify_*` 이름의 알림 job이고 설계 7·8은 이름 없는 domain event다. **이름을 통일하지 않는다.** 설계 9는 20종을 이름과 무관하게 받는 단일 dispatch mapper를 정의한다.

```text
outbox_jobs (type, dedupe_key, payload)
        │  worker가 claim
        ▼
dispatch mapper  ── §5.1 표 조회 ──▶ 수신자 목록 × 채널 × urgency
        │
        ├─▶ notification_jobs (alert 전용, 수신자별 1행)
        └─▶ notification_silent_sends (silent, 사용자별 이동 창 판정. job row 없음)
```

- mapper는 순수 함수다. `(type, payload, outbox job 발행 시각, 멤버십·제안·command 상태)`를 받아 job 목록을 만든다. 테스트에서 DB 없이 검증한다.
- **발행 시각이 입력에 필요한 이유**는 `snapshot_at_publish` 5종 때문이다. 이 type들은 "지금 활성인 사람"이 아니라 "발행 시점에 수신자였던 사람"을 대상으로 하므로, 현재 상태만으로는 답을 낼 수 없다. 재구성 술어는 §5.1의 `recipient_scope` 정의에 있다.
- mapper는 수신자별 `urgency` 분기도 수행한다. 표 B의 `대상 active / 나머지 passive`, `새 방장 active / 나머지 passive` 3행이 여기 해당하며, 같은 job에서 수신자 역할에 따라 `notification_jobs.urgency` 값이 갈린다.
- 표에 없는 type이 들어오면 **알림을 전혀 만들지 않고** 운영 경보만 올린 뒤 outbox job을 `succeeded`로 넘긴다. 수신자를 결정하는 유일한 근거가 그 표이므로 누구에게 보낼지 알 수 없고, 추측해서 보내면 잘못된 사람에게 간다. 앱 활성화 sync가 대체 경로라는 §3 비목표와 일관된다. 선행 설계가 새 type을 추가했는데 설계 9가 따라가지 못한 상태를 조용히 넘기지 않는 것이 경보의 목적이다.

---

## 6. 데이터 모델과 상태 전이

### 6.1 테이블

| 테이블 | 핵심 필드/제약 |
|---|---|
| `notification_settings` | `user_id` PK, `quiet_hours_enabled`, `quiet_start_local time`, `quiet_end_local time`, `version bigint`, `updated_at` |
| `notification_category_prefs` | `user_id`, `category`, `enabled`; unique `(user_id, category)` |
| `notification_ref_keys` | `user_id` PK, `ref_key_ciphertext`, `rotated_at` |
| `notification_jobs` | `id`, `outbox_job_id`, `type`, `category`, `user_id`, `party_id`(`self_only` 3종 중 Party 맥락이 없는 `calendar_write_*`·`calendar_cleanup_suggested`도 NOT NULL. §4.5의 합산 키가 `(user_id, party_id, category)`이므로 NULL이면 합산 그룹이 정의되지 않는다. 해당 confirmed event의 Party를 쓴다), `entity_type`, `entity_id`, `notification_key`, `urgency`, `valid_until`, `deferred_until`, `status`, `attempt_count`, `locked_until`, `created_at` |
| `notification_deliveries` | `notification_key`, `user_id`, `owner_job_id`, `claimed_at`, `first_sent_at`(nullable), `device_count`; unique `(notification_key, user_id)` |
| `notification_silent_sends` | `user_id`, `sent_at`; index `(user_id, sent_at)`. 이동 창 판정은 `count(*) WHERE user_id = $1 AND sent_at > now() - interval '60 min'`. 60분 초과 row는 정리 job이 지운다 |

`devices`(설계 2 §8)에 아래 3개 열을 추가한다. 기존 열은 그대로 둔다.

| 열 | 값 |
|---|---|
| `push_authorization` | `unknown` / `authorized` / `provisional` / `denied` |
| `push_environment` | `sandbox` / `production` |
| `time_sensitive_setting` | `enabled` / `disabled` / `not_supported` |

`push_authorization`은 사용자가 권한을 거부했는지와 아직 묻지 않았는지를 구분해야 §9.4의 화면 상태를 만들 수 있다. `push_token_ciphertext`의 null 여부만으로는 구분되지 않는다.

**`notification_jobs`와 `notification_deliveries`는 alert 전용이다.** silent는 job row를 만들지 않고 `notification_silent_sends`의 발송 기록으로만 존재한다. §4.5가 silent를 사용자당 60초 창으로 합산하므로 사건별 식별자가 필요 없고, 애초에 payload에 Party·entity 참조가 없다(§10.3).

이 분리가 필요한 이유는 `notification_key`에 채널이 들어 있지 않기 때문이다. silent도 job이라면 `A+S` 16종에서 alert job과 silent job이 같은 key를 갖고, claim-then-send가 APNs 호출 전에 delivery row를 잡으므로 먼저 도는 쪽이 다른 쪽을 unique index로 막아버린다. silent를 job에서 빼면 이 충돌 자체가 생기지 않는다.

`notification_key` 정의:

```text
notification_key = outbox type || ":" || outbox dedupe_key
```

- outbox가 확정한 재발행 의도를 그대로 계승한다. `calendar_write_command_pending`의 `command_id:notice_seq`는 seq마다 다른 key가 되어 3회 발행이 정상 동작한다.
- `proposal_response_changed`의 `proposal_id:user_id:response_version`도 버전마다 다르다.

### 6.2 알림 job 상태

```text
pending ──▶ deferred ──▶ pending
   │            │           │
   │            └──▶ dropped / coalesced
   ├──────────────────────▶ sending ──▶ sent
   │                           ├──▶ retryable_failed ──▶ pending
   │                           │            └──▶ dead
   │                           └──▶ dead            (lease 만료 회수, delivery row 존재)
   ├──▶ dropped      (precondition 불충족, valid_until 경과)
   └──▶ coalesced    (같은 창의 다른 job에 합산됨)
```

- `sent`, `dropped`, `coalesced`, `dead`는 종단 상태다. `sending`은 종단이 아니므로 반드시 회수 경로를 가진다.
- 재시도는 설계 2 §9의 지수 backoff와 jitter를 그대로 쓰며 최대 5회다.
- `dead`는 운영 경보만 만들고 사용자에게 알리지 않는다. 사용자는 앱 상태로 같은 정보를 본다.

#### Claim-then-send

worker가 APNs 호출 도중 죽으면 `sending`에서 멈춘다. 이때 회수하지 않으면 알림이 사라지고, 기록 없이 회수하면 같은 alert가 두 번 나가 불변식 1이 깨진다. 그래서 전송 순서를 뒤집는다.

1. §4.6의 precondition을 **후보 전체**에 대해 평가한다. 불충족 job은 `dropped`로 끝낸다.
2. **통과한 집합 안에서만** 창 종료 합산(§4.5)을 수행한다. 진 job은 `coalesced`로 끝낸다. 순서가 반대이면 승자가 precondition에서 탈락할 때 패자들이 이미 `coalesced` 종단 상태라 되살아나지 못해 창 전체가 0건이 된다.
3. 승자만 `notification_deliveries` row 삽입과 `sending` 전이를 **같은 transaction에서** 커밋한다.

   ```sql
   INSERT INTO notification_deliveries (notification_key, user_id, owner_job_id, claimed_at)
   VALUES ($key, $user, $job, now())
   ON CONFLICT (notification_key, user_id) DO NOTHING
   RETURNING owner_job_id;
   ```

   반환이 없으면 다른 row가 이미 있다는 뜻이므로 그 row의 `owner_job_id`를 읽는다. **내 job id와 같을 때만** 진행한다(내 이전 시도의 재시도다). 다르면 다른 worker가 이미 잡은 것이므로 `coalesced`로 끝내고 APNs를 호출하지 않는다. 이 시점의 `first_sent_at`은 NULL이다.
4. APNs를 호출한다. `200`이면 `first_sent_at`을 채우고 `sent`로 전이한다.
5. `429`/`503`/`403`은 `retryable_failed → pending`으로 되돌린다. 재시도가 3단계에 다시 오면 `owner_job_id`가 자기 자신이므로 통과한다. `first_sent_at`이 이미 채워진 row를 만나면 발송이 성공했던 것이므로 재발송하지 않고 `sent`로 정정한다.
6. `locked_until`이 만료된 `sending` job은 scheduler가 회수한다. delivery row의 `owner_job_id`가 그 job이고 `first_sent_at`이 NULL이면 **재발송하지 않고 `dead`로 종료하고 운영 경보를 만든다.**

- `owner_job_id`가 없으면 "내 이전 시도"와 "다른 worker의 claim"을 영속 상태만으로 구별할 수 없다. 그 구별이 없으면 `ON CONFLICT DO NOTHING` 후 무조건 진행이 §6.3의 동시 claim 방어를 무력화하고, 무조건 차단이 §11.2의 재시도 규칙을 죽은 문장으로 만든다.
- delivery row 삽입은 반드시 precondition·합산 **뒤**이며 `dropped`·`coalesced` job에는 만들지 않는다. 그렇지 않으면 탈락한 job이 자기 delivery 기록 때문에 영구히 억제된다.
- 이 설계는 **at-most-once**를 택한다. worker 크래시라는 드문 경우에 알림 1건이 사라지는 것을, 같은 알림이 두 번 울리는 것보다 낫다고 본다. 재시도 가능한 APNs 오류는 손실이 아니라 5단계로 회수되며, 최대 시도를 넘긴 것만 `dead`가 된다. 사라진 알림의 내용은 `/sync`가 이미 전달했고 §9.4의 앱 내부 표시가 대체 경로다.

### 6.3 Fan-out 3단계 멱등성

| 단계 | 단위 | 보장 수단 |
|---|---|---|
| 1. domain event | `(type, dedupe_key)` | `outbox_jobs` partial unique index (설계 2 §9). **활성 상태에만 적용**되므로 성공 후 재삽입을 허용한다 |
| 2. 수신자별 알림 | `(notification_key, user_id)` | `notification_deliveries` unique index. 영속이며 성공 후에도 남는다 |
| 3. 기기별 전송 | `(notification_key, user_id)`의 기기 목록 | 2단계 delivery row를 먼저 잡으므로 재시도가 목록 전체를 다시 보내지 않는다. 한 사용자의 기기 목록은 한 번에 처리하고 부분 실패는 §11.2의 응답 코드별 처리로 끝낸다 |

- worker가 같은 outbox job을 두 번 claim해도 2단계 unique index가 두 번째 전송을 막는다.
- `apns-collapse-id`는 멱등성 수단이 아니다. 아직 배달되지 않았거나 알림 센터에 남은 알림을 교체할 뿐이며 중복 전송을 막지 않는다. 3단계 보장은 2단계의 claim이 담당한다.
- `notification_deliveries` row는 90일 보관 후 정리한다. 그 이전에 같은 key가 다시 오는 경로는 없다.

### 6.4 불변식

1. 한 `(notification_key, user_id)`에 대해 alert는 최대 1회 전송된다.
2. `dropped`, `coalesced` job은 APNs를 호출하지 않는다.
3. 즉시 발송 대상 alert가 나가는 60초 창 안에서 같은 사용자에게 silent를 전송하지 않는다. defer된 alert와 `dropped`된 alert는 silent를 억제하지 않는다.
4. 모든 `notification_jobs` row는 alert이며 `valid_until`을 가진다. silent는 job이 아니라 `notification_silent_sends`의 발송 기록이므로 `valid_until`도 delivery row도 type·category 기록도 갖지 않는다.
5. type 정책으로서 quiet hours가 `bypass`인 것은 `proposal_calendar_refresh_requested` 하나뿐이다. `time_sensitive_setting`이 꺼진 사용자에 대한 이 type의 `bypass`도 유지된다(§4.3). 시간대가 유효하지 않을 때의 즉시 발송(§12)은 정책이 아니라 fallback이며 §14에서 따로 계수한다.
6. payload와 APNs 요청 헤더에 사람이 읽을 문자열, UUID, 시각, 사용자 식별자가 없다. `apns-expiration`은 `valid_until`에서 파생되지 않는 상수이므로 약속 시각과 같아질 수 없다.
7. `notification_ref_key`는 APNs 요청 본문 어디에도 나타나지 않는다.
8. 설계 4 §9.3이 정한 수신자 집합에서 사용자를 추가하거나 빼지 않는다. 그 안에서 채널만 조정한다.

---

## 7. 트랜잭션, 경쟁과 중복 방지

### 7.1 발행 경계

- 도메인 상태 전이와 `outbox_jobs` 생성은 선행 설계대로 한 transaction이다. `notification_jobs`는 그 transaction에 **넣지 않는다.** worker가 outbox job을 claim한 뒤 별도 transaction에서 만든다.
- 이유: 수신자 목록은 발행 시점이 아니라 발송 직전 상태로 판단해야 §4.6의 precondition이 의미를 갖는다. 또 10명 fan-out을 도메인 transaction 안에 넣으면 잠금 시간이 길어진다.
- `notification_jobs` 삽입과 outbox job의 `succeeded` 전이는 같은 transaction이다. 중간 실패 시 outbox가 `pending`으로 남아 재시도한다. 재시도는 2단계 unique index로 중복을 만들지 않는다.

### 7.2 잠금 순서

`outbox_jobs` → `party_memberships`(읽기) → `notification_settings`(읽기) → `notification_jobs` → `notification_deliveries` → `notification_silent_sends`

설계 2·4·7·8의 잠금 순서 뒤에 이어 붙는 순서이므로 교착이 생기지 않는다.

### 7.3 경쟁 결과

| 상황 | 결과 |
|---|---|
| worker 2개가 같은 outbox job claim | `FOR UPDATE SKIP LOCKED`로 1개만 진행 |
| 같은 key의 outbox가 성공 후 재삽입 | 2단계 unique index가 alert 중복을 막는다. 의도적 재발행(`notice_seq` 증가)은 key가 달라 통과한다 |
| defer 중 사용자가 카테고리를 끔 | 발송 직전 precondition에서 `dropped` |
| defer 중 사용자가 Party를 떠남 | `recipient_scope`가 `active_members_only`면 `dropped`, `snapshot_at_publish`·`self_only`면 그대로 발송(§4.6) |
| defer 중 제안이 확정됨 | `proposal_created` job은 precondition(`open`) 불충족으로 `dropped`. `proposal_confirmed` job이 새로 나간다 |
| 같은 창에 같은 Party·카테고리 3건 | 가장 늦은 1건만 `_multi` 문구로 발송, 나머지 `coalesced` |
| refresh 요청 중 기기가 snapshot 갱신 완료 | precondition(`awaiting_calendar_refresh`) 불충족으로 `dropped` |
| defer 중 사용자가 quiet hours를 끄거나 구간을 줄임 | PATCH transaction 안에서 해당 사용자의 `deferred` job의 `deferred_until`을 재계산한다. 창 밖이 되면 즉시 발송 대상이 된다 |
| defer 중 사용자가 quiet hours 구간을 늘림 | 재계산하지 않는다. 이미 미뤄진 알림을 더 미루지 않는다 |
| 사용자가 알림 설정을 동시에 두 기기에서 변경 | `expected_version` 불일치로 두 번째가 `409 version_conflict` |

---

## 8. API와 역할별 권한

### 8.1 Endpoint

| method | path | 설명 |
|---|---|---|
| GET | `/v1/me/notification-settings` | 본인 quiet hours와 카테고리 토글 조회 |
| PATCH | `/v1/me/notification-settings` | 본인 설정 수정, `expected_version` 필수 |
| PUT | `/v1/devices/{id}/push-token` | 기존(설계 2). 요청 본문에 `push_authorization`, `push_environment`, `time_sensitive_setting` 추가 |
| GET | `/v1/me` | 기존(설계 2). 응답에 `notification_ref_key` 추가 |
| GET | `/v1/sync/bootstrap` | 기존(설계 2). 응답에 `notification_ref_key` 추가 |

- 알림 설정은 **사용자 전역**이며 Party별 설정을 만들지 않는다. Party별 mute는 Post-MVP다.
- 다른 사용자의 알림 설정을 조회하는 endpoint를 만들지 않는다.
- 알림 이력 조회 endpoint를 만들지 않는다. 앱 상태는 sync가 기준이다.

### 8.2 입력값과 검증 규칙

| 필드 | 규칙 |
|---|---|
| `quiet_hours_enabled` | boolean |
| `quiet_start_local` | `HH:MM`, 30분 단위. 설계 6의 활동 시간 grid와 같은 단위 |
| `quiet_end_local` | `HH:MM`, 30분 단위, `start`와 달라야 한다 |
| `categories[].category` | `party` / `proposal` / `confirmation` / `calendar_action` 중 하나 |
| `categories[].enabled` | boolean |
| `expected_version` | 현재 `notification_settings.version`과 일치 |
| `push_authorization` | `unknown` / `authorized` / `provisional` / `denied` |
| `push_environment` | `sandbox` / `production`. 기기가 빌드 구성으로 판단해 보고한다. TestFlight와 App Store 빌드는 `production`, Xcode 직접 실행은 `sandbox`다 |
| `time_sensitive_setting` | `enabled` / `disabled` / `not_supported`. `UNNotificationSettings.timeSensitiveSetting` 값 |

- `quiet_hours_enabled = true`인데 설계 6의 활동 시간 `time_zone`이 미설정이면 `409 activity_timezone_required`를 반환한다. 시간대 없이 로컬 구간을 해석할 수 없다.
- 클라이언트가 카테고리 목록에 없는 값을 보내면 `422`이며 부분 적용하지 않는다.
- 클라이언트가 `urgency`, 채널, 수신자를 입력으로 보내는 것을 허용하지 않는다. 서버가 §5.1 표로만 결정한다.

### 8.3 권한

| 대상 | 권한 |
|---|---|
| 본인 알림 설정 조회·수정 | 본인만 |
| 본인 기기 push token 등록·교체 | 본인, 해당 기기 세션 |
| 다른 멤버 알림 설정 | 없음. 방장도 조회할 수 없다 |
| 알림 강제 발송 | 없음. 사용자·방장 모두 임의 발송 수단이 없다 |

### 8.4 오류 코드

| HTTP | code | 처리 |
|---:|---|---|
| 409 | `version_conflict` | 현재 설정과 version 반환, 사용자가 다시 적용 |
| 409 | `activity_timezone_required` | 활동 시간 설정 CTA 표시 |
| 422 | `invalid_quiet_window` | 30분 단위·동일 시각 위반 |
| 422 | `unknown_category` | 알 수 없는 카테고리 |
| 404 | `device_not_found` | 폐기된 기기의 token 등록 시도 |

---

## 9. 사용자 흐름과 화면·오프라인 상태

### 9.1 권한 요청 시점

- 앱 첫 실행에 묻지 않는다. **첫 Party 참여 직후** 또는 **첫 제안 생성·수락 직후**에 사전 설명 화면을 보이고 시스템 권한을 요청한다.
- 사전 설명 화면은 "약속 제안과 확정, 캘린더 반영에 필요한 알림만 보냅니다"를 표시한다.
- 사용자가 거부하면 다시 시스템 대화상자를 띄우지 않는다. 설정 앱으로 가는 링크만 제공한다.

### 9.2 상태별 화면

| 상태 | 표시 |
|---|---|
| 정상 | 알림 설정 화면에 카테고리 4개 토글과 quiet hours 구간 |
| 빈 상태 | 알림 이력 화면이 없으므로 해당 없음 |
| 로딩 | 설정 조회 중 토글은 마지막 로컬 값으로 표시하고 비활성화한다 |
| 오류 | 설정 저장 실패 시 이전 값으로 되돌리고 재시도 CTA |
| 오프라인 | 토글 변경을 로컬 mutation queue에 넣고 낙관적으로 표시한다. 설계 2 §7.3의 큐를 그대로 쓴다 |
| 권한 미요청 | `알림 켜기` 카드. 토글은 표시하되 "알림이 꺼져 있습니다" 배너 |
| 권한 거부 | 토글을 비활성화하고 "iOS 설정에서 알림을 켜주세요" + 설정 앱 링크 |
| quiet hours 미설정 시간대 | 활동 시간 설정 CTA. quiet hours 토글은 비활성 |

### 9.3 포그라운드 처리

- 앱이 포그라운드이고 사용자가 해당 Party 화면을 보고 있으면 배너를 표시하지 않는다. 화면이 이미 sync로 갱신된다.
- 다른 화면에 있으면 배너를 표시한다.
- 알림을 탭하면 `entity_ref` 역조회로 해당 제안·확정 일정·Party 화면으로 이동한다. 역조회에 실패하면 Party 목록으로 보낸다.

### 9.4 권한 거부 사용자의 대체 경로

설계 8 §8.2의 "조치 필요" 상태와 §8.3의 "writer device 미접속"은 알림이 도달한다고 가정한다. 알림이 없어도 사용자가 같은 정보를 얻어야 한다.

- 홈 화면 상단에 미해결 `calendar_write_command`와 `calendar_cleanup_suggestion` 개수를 배지로 표시한다.
- Party 상세에 `awaiting_calendar_refresh` 제안이 있으면 상단 배너로 표시한다.
- 이 표시는 알림 권한과 무관하게 항상 동작한다. 데이터는 `/sync`가 이미 전달한다.

### 9.5 오프라인

- 오프라인에서는 알림을 받지 못한다. 재연결 시 `/sync`가 놓친 상태를 전부 전달하며 서버는 지난 알림을 재발송하지 않는다.
- `valid_until`이 지난 job은 재연결 시점에 이미 `dropped`다.

---

## 10. 개인정보 노출 규칙과 payload 계약

### 10.1 노출 규칙

- alert 문구는 **카테고리**와 **Party 이름**까지만 드러낸다. 그 이상은 기기에서도 넣지 않는다.
- 카테고리 자체가 잠금 화면에 드러나는 것은 허용한다. `notify_member_removed`는 수신자 본인이 강퇴 대상이고 `calendar_write_action_required`는 본인 기기 문제이므로 모두 수신자 자신에 관한 정보다.
- 다른 멤버의 이름·응답·일정 제목·장소·시각을 어떤 채널에도 넣지 않는다.
- 설계 6 §6·§12의 제약대로 `availability_setup_pending`·`calendar_sync_pending`의 원인이 된 다른 멤버와 그 ID를 알림에 넣지 않는다. 이 두 상태는 애초에 알림을 만들지 않는다.
- 설계 8 §9.1대로 다른 멤버의 캘린더 반영 상태를 집계로도 알리지 않는다.

### 10.2 alert payload

```json
{
  "aps": {
    "alert": { "loc-key": "proposal_created" },
    "mutable-content": 1,
    "interruption-level": "active",
    "thread-id": "<party_ref>",
    "sound": "default",
    "content-available": 1
  },
  "n": {
    "v": 1,
    "p": "<party_ref>",
    "e": "<entity_ref>",
    "t": "proposal"
  }
}
```

- `alert.loc-args`를 쓰지 않는다. 치환값을 서버가 만들지 않는다는 뜻이다.
- `t`는 `party` / `proposal` / `confirmed_event` / `calendar_command` / `cleanup` 5종 enum이며 `loc-key`가 이미 드러내는 것 이상을 드러내지 않는다.
- **시각을 담지 않는다.** `valid_until`을 payload에 넣지 않으며 만료 판정은 §4.6이 서버에서 끝낸다. 약속 시작 시각이 `valid_until`인 두 type이 있으므로, 이 값을 payload에 넣으면 확정된 약속의 정확한 시각이 APNs에 노출된다.
- `content-available: 1`은 alert 전달 시 앱 본체를 깨우려는 best-effort다. `mutable-content: 1`이 깨우는 것은 NSE(별도 샌드박스)이고 NSE는 네트워크를 쓰지 않으므로, 이 키가 없으면 alert가 sync를 전혀 당기지 못한다. iOS가 실행을 보장하지 않으므로 §4.1의 silent 억제는 best-effort 전제 위에 있고, 앱 활성화 sync가 여전히 유일한 보장이다.
- `apns-collapse-id` 헤더는 `entity_ref`다. 이것은 **표시 정책**이며 전달 멱등성 수단이 아니다.

### 10.3 silent payload

```json
{
  "aps": { "content-available": 1 },
  "n": { "v": 1, "k": "sync_needed" }
}
```

- Party·entity 참조를 담지 않는다. 앱은 전체 `/sync`를 수행한다. 설계 3 §8의 "push에는 opaque type만" 규칙과 일치한다.

### 10.4 금지 데이터

| 금지 | 적용 범위 |
|---|---|
| 일정 제목·장소·시각 | payload, 로그, trace, 분석 |
| 사용자 ID·표시 이름·이메일 | payload |
| Party ID·제안 ID·확정 일정 ID 원본 UUID | payload |
| 응답 값(accepted/declined/pending) | payload, 분석 |
| 공개 수준과 `share_location` | 전 채널 |
| EventKit identifier, 캘린더 이름 | 전 채널. 설계 8 §12와 동일 |
| `notification_ref_key` | payload, 로그, trace |
| Busy 구간과 충돌 원인 | 전 채널 |

로그는 `notification_key` 해시, 내부 actor ID, type, category, urgency, 상태와 수량만 기록한다.

---

## 11. 외부 시스템 연동과 재시도

### 11.1 APNs

- provider token(JWT) 방식을 쓴다. token은 20분마다 갱신하고 60분을 넘기지 않는다.
- HTTP/2 연결을 유지하며 worker당 연결 수를 제한한다.
- `apns-expiration`은 **`valid_until`과 무관한 상수**로 설정한다. 기본 `now() + 24시간`, `proposal_calendar_refresh_requested`만 `now() + 120초`다. silent는 `apns-expiration = 0`(즉시 시도, 저장하지 않음)이다.
- `min(valid_until, 상한)` 방식을 쓰지 않는 이유: 약속이 상한보다 가까우면 `min`이 `valid_until`을 그대로 고르므로 헤더가 약속 시작 시각과 **정확히 같아진다.** 오늘 확정해서 오늘 저녁에 만나는 흔한 경우가 전부 여기 해당해 상한이 아무 일도 하지 않는다. 반올림·양자화도 근사 누출이 남으므로 쓰지 않는다.
- 헤더의 유일한 역할은 APNs의 store-and-late-deliver 차단이다. 실제 만료 판정은 §4.6이 서버에서 이미 끝냈으므로 헤더가 정밀할 이유가 없고, 상수면 정보량이 0이다.

### 11.2 응답 처리

| 응답 | 처리 |
|---|---|
| `200` | `sent`, `notification_deliveries` 기록 |
| `400 BadDeviceToken` | 해당 token 비활성화, 재발송 없음 (설계 2 §11과 동일) |
| `410 Unregistered` | token 비활성화, `devices.push_authorization = unknown` |
| `403 ExpiredProviderToken` | provider token 재발급 후 1회 즉시 재시도 |
| `429 TooManyRequests` | backoff 후 재시도, 최대 5회 |
| `503` | backoff 후 재시도 |
| `413 PayloadTooLarge` | 즉시 `dead` + 운영 경보. payload가 고정 크기이므로 발생하면 버그다 |

### 11.3 중복 방지 요약

- domain event 중복 → 설계 2 §9의 partial unique index
- 수신자별 중복 → `notification_deliveries` unique index (영속)
- 잠금 화면 누적 → `apns-collapse-id`
- 알림 센터 산개 → `thread-id`
- quiet hours 폭발 → 창 종료 시 카테고리별 합산
- silent 폭주 → 사용자당 60초 창 + 시간당 3건

---

## 12. 오류와 복구

| 상황 | 처리 |
|---|---|
| dispatch mapper가 모르는 type을 받음 | 알림을 만들지 않고 운영 경보만. 수신자를 알 수 없으므로 추측해서 보내지 않는다(§5.3) |
| 수신자 조회 중 Party가 해산됨 | `recipient_scope`가 `active_members_only`면 `dropped`, `snapshot_at_publish`·`self_only`면 진행한다(§4.6). 예외 목록을 §12에 따로 두지 않고 §5.1 표 A의 열 하나로만 판단한다 |
| `notification_ref_key`가 없음 | 발송 직전 생성하고 다음 sync로 기기에 전달. 그 사이 알림은 기본 문구로 표시된다 |
| quiet hours 계산 중 시간대가 유효하지 않음 | quiet hours를 적용하지 않고 즉시 발송. 사용자에게 시간대 재설정 CTA. 이것은 type 정책의 `bypass`가 아니라 fallback이므로 §14에서 따로 계수한다(불변식 5) |
| worker가 defer job을 창 종료 후에도 못 집음 | `valid_until` 판정으로 `dropped` 또는 늦게라도 발송. 무한 대기 없음 |
| worker가 APNs 호출 도중 죽어 job이 `sending`에 멈춤 | `locked_until` 만료 시 scheduler가 회수한다. delivery row가 이미 있으므로 재발송하지 않고 `dead`로 종료 + 운영 경보(§6.2). at-most-once를 택한 결과다 |
| `notification_jobs`가 `dead`에 도달 | 운영 경보만. 사용자 화면은 sync로 이미 정확하다 |
| 사용자가 계정 삭제 요청 | 모든 `pending`/`deferred` job을 `dropped`로 종료하고 token을 비활성화 |
| 기기가 초기화되어 로컬 데이터가 없음 | 모든 알림이 기본 문구로 낮아진다. bootstrap 이후 정상화 |

---

## 13. 테스트 가능한 인수 조건

### 매핑과 채널

- [ ] `A+S` 18종이 §5.1 표의 urgency·수신자와 일치하는 alert job을 만들고, `S만` 2종은 alert job을 만들지 않는다.
- [ ] `proposal_superseded`와 `confirmed_event_created`는 alert job을 만들지 않는다.
- [ ] 제안 수락은 alert를 만들지 않고 거절만 생성자에게 alert를 만든다.
- [ ] 확정 순간 참여자는 `proposal_confirmed` alert를 정확히 1건 받고 `confirmed_event_created`로 추가 alert를 받지 않는다.
- [ ] 표에 없는 type이 들어오면 알림이 0건 나가고 경보가 올라간다.

### 수신자

- [ ] 설계 4의 6종 수신자 집합이 설계 4 §9.3과 완전히 일치한다.
- [ ] 행위자는 자기 행동의 alert를 받지 않는다.
- [ ] 강퇴 대상은 `notify_member_removed` alert를 받는다.
- [ ] `calendar_write_command_pending`, `calendar_write_action_required`, `calendar_cleanup_suggested` 3종이 writer device에만 간다. writer device가 없으면 전 기기로 가며, 앞의 두 종만 `_no_writer` 문구를 쓴다.
- [ ] `recipient_scope`가 `snapshot_at_publish`인 4종은 수신자가 이미 활성 멤버가 아니어도 발송된다.
- [ ] quiet hours가 비활성일 때 멤버 3명이 순차 합류하면 사용자가 알림 3건을 모두 본다. `entity_id`가 사건 단위라 collapse가 앞의 것을 덮지 않는다. quiet hours 안이면 §4.5의 창 합산으로 `_multi` 1건이 된다.
- [ ] 떠난 사용자가 `calendar_cleanup_suggested` alert를 받는다.
- [ ] 10명 Party의 한 이벤트가 최대 9명에게 각 1건씩만 만든다.

### quiet hours

- [ ] quiet hours 안에 생긴 `active`/`passive` alert가 창 종료 시각에 발송된다.
- [ ] defer 중 사용자가 quiet hours를 끄면 `deferred_until`이 재계산되어 즉시 발송된다. 구간을 늘리면 재계산하지 않는다.
- [ ] `time_sensitive_setting`이 `disabled`인 사용자는 refresh 알림을 `active`로 받되 quiet hours 안에서도 즉시 발송된다.
- [ ] `proposal_calendar_refresh_requested`는 quiet hours 안에서도 즉시 발송된다.
- [ ] `calendar_write_command_pending`의 `valid_until`이 창 안에서 지나면 발송되지 않는다.
- [ ] 자정을 넘는 구간(`22:00`–`08:00`)이 올바르게 판정된다.
- [ ] DST gap에 걸린 구간 경계가 gap 직후 시각으로 밀린다.
- [ ] 활동 시간 시간대 미설정 사용자는 quiet hours를 켤 수 없다.

### 중복 억제

- [ ] 같은 `(notification_key, user_id)`에 대해 worker를 3번 돌려도 APNs 호출이 1회다.
- [ ] APNs 호출 직전에 worker를 죽이면 job이 `sending`에 남고, lease 만료 후 회수 시 재발송하지 않고 `dead`로 끝난다.
- [ ] precondition에 걸려 `dropped`된 job은 delivery row를 만들지 않는다.
- [ ] outbox job이 성공 후 같은 key로 재삽입되어도 alert가 다시 나가지 않는다.
- [ ] `calendar_write_command_pending`의 `notice_seq` 증가는 정상적으로 3회까지 발송된다.
- [ ] 같은 창에 같은 Party·카테고리 3건이면 1건만 `_multi`로 나가고 2건이 `coalesced`다.
- [ ] alert가 나간 사용자에게 같은 60초 창의 silent가 나가지 않는다.
- [ ] silent는 사용자당 시간당 3건을 넘지 않는다.

### precondition

- [ ] defer 중 제안이 확정되면 `proposal_created` job이 `dropped`다.
- [ ] defer 중 Party를 떠나면 `active_members_only` job만 `dropped`다. `snapshot_at_publish`·`self_only` job은 그대로 발송된다.
- [ ] defer 중 카테고리를 끄면 job이 `dropped`다.
- [ ] refresh 완료 후 `proposal_calendar_refresh_requested`가 `dropped`다.
- [ ] `dropped`와 `coalesced` job에서 APNs 호출이 0건이다.

### Privacy

- [ ] APNs 요청 본문·헤더에 UUID, 사람이 읽을 문자열, 시각, 사용자 식별자가 없다. `apns-expiration`이 약속 시작 시각과 같은 값이 되는 경우가 없다.
- [ ] `notification_ref_key`가 payload·로그·trace에 없다.
- [ ] 같은 Party의 `party_ref`가 사용자마다 다르다.
- [ ] 로그·trace·분석에 일정 제목·장소·응답 값·공개 수준이 없다.
- [ ] NSE가 Party 이름 외의 어떤 내용도 문구에 넣지 않는다.

### 권한과 설정

- [ ] 권한 거부 사용자에게 alert job이 `dropped`되고 silent는 계속 나간다.
- [ ] 권한 거부 사용자가 §9.4의 배지·배너로 미해결 조치를 확인한다.
- [ ] `410 Unregistered`를 받은 token으로 재발송하지 않는다.
- [ ] 설정 변경이 `expected_version` 불일치 시 `409`다.
- [ ] 오프라인 설정 변경이 재연결 후 서버에 반영된다.

---

## 14. 분석 지표와 민감정보 제외

### 허용 지표

| 지표 | 목적 |
|---|---|
| type별 alert 생성·발송·`dropped`·`coalesced` 건수 | 알림량과 억제 효과 |
| `dropped` 사유 분포 | precondition이 실제로 무의미한 알림을 막는지 |
| 카테고리별 토글 off 비율 | 알림이 시끄러운 카테고리 식별 |
| quiet hours 활성 비율과 defer 건수 | 기능 사용도 |
| 권한 요청 → 허용 전환율 | §9.1 시점이 적절한지 |
| APNs 응답 코드 분포 | 전송 건강도 |
| 시간대 무효 fallback 발송 건수 | 불변식 5의 예외가 얼마나 자주 발생하는지 |
| `sending` lease 만료 회수 건수 | at-most-once 선택으로 실제 손실되는 알림의 규모 |
| `time_sensitive_setting`이 `disabled`인 기기 비율 | refresh 알림 bypass 경로의 실효성 |
| `proposal_created` alert 후 24시간 내 응답률 | 알림 유효성 |
| `calendar_write_command_pending` 3회 발송 후에도 미해결 비율 | 설계 8 escalation 유효성 |

- 모든 지표는 사용자 단위가 아니라 집계다. 최소 집계 크기 20 미만 cohort는 내보내지 않는다. 설계 6 §11과 동일 기준이다.

### 금지 데이터

- 알림 문구 실제 문자열, Party 이름, 일정 제목·장소·시각
- 사용자 ID, 기기 ID 원본, push token
- 응답 값과 누가 거절했는지
- `notification_ref_key`, `party_ref`, `entity_ref`
- 알림 탭 후 이동한 구체 entity ID
- quiet hours 실제 구간 값(활성 여부만 기록)

---

## 15. 검증 계획

### 순수 단위 테스트

- dispatch mapper: 20종 type × 수신자·채널·urgency 매핑
- quiet hours 판정: 자정 넘김, DST gap/overlap, 시간대 미설정
- `valid_until` 계산과 만료 판정
- silent 예산 창 합산
- `notification_key` 파생
- `party_ref`/`entity_ref` HMAC 파생과 사용자 간 비충돌

### PostgreSQL 통합 테스트

- `(notification_key, user_id)` unique index의 성공 후 재삽입 차단
- outbox 재발행(`notice_seq` 증가) 통과
- worker 동시 claim에서 APNs 호출 1회
- defer job의 창 종료 후 합산
- precondition 불충족 시 `dropped` 전이
- 잠금 순서 교착 부재

### API·계약 테스트

- 알림 설정 조회·수정·버전 충돌·오류 코드
- APNs payload fixture allowlist 검사(금지 키·값 스캔)
- 로그·trace·분석 fixture 민감 키 스캔
- `push_authorization` 전이

### iOS 단위·통합 테스트

- NSE 문구 완성: 로컬 데이터 있음/없음/예산 초과
- `party_ref` 역조회 표 구성과 키 회전 후 재구성
- 포그라운드 배너 억제
- 알림 탭 라우팅과 역조회 실패 fallback
- 권한 거부 시 §9.4 배지·배너 표시

### E2E

- 3명 Party → 제안 → 1명 거절 → 생성자만 alert 수신
- 전원 수락 → 확정 alert 1건씩, 캘린더 반영 alert 중복 없음
- quiet hours 안에서 제안 생성 → 아침에 1건 수신
- refresh 요청 → quiet hours 안에서도 즉시 수신, 2분 뒤 미도달 건 폐기
- Party 해산 → 전원 `calendar_cleanup_suggested` 수신 후 정리
- 권한 거부 사용자 → 알림 0건, 앱 배지로 전 항목 확인

### Privacy 회귀

- APNs fixture, 로그, trace, 분석 payload의 금지 키·값 스캔을 CI에 넣는다. 설계 5·6·8과 같은 검사 파이프라인을 재사용한다.

---

## 16. 현재 구현과의 차이

현재 Swift 코드에는 알림 관련 구현이 전혀 없다.

| 항목 | 현재 | 설계 9 |
|---|---|---|
| 푸시 | 없음 | APNs alert + silent 2채널 |
| NSE 타깃 | 없음 | 신규 target 필요 |
| App Group | 없음 | SwiftData 저장소와 keychain을 NSE와 공유하려면 필수 |
| Time Sensitive entitlement | 없음 | `com.apple.developer.usernotifications.time-sensitive` + Developer Console capability + provisioning profile 갱신 |
| 알림 권한 | 요청하지 않음 | §9.1 시점에 요청 |
| 알림 설정 | 없음 | 카테고리 4종 + quiet hours |
| 서버 | 없음 | `notification_jobs`, `notification_deliveries` 등 6개 테이블과 worker |
| `devices` 테이블 | 없음 | `push_authorization`, `push_environment`, `time_sensitive_setting` 3개 열 추가 |

- 이 설계는 서버 골격(설계 2 P0) 없이는 어떤 부분도 구현할 수 없다.
- iOS 측 NSE target은 `src/project.yml`에 새 target으로 추가해야 한다. App Group entitlement, keychain access group, Time Sensitive entitlement도 함께 필요하다.

---

## 17. 미결정 사항과 후속 설계 제약

### 17.1 미결정 사항

| 항목 | 상태 |
|---|---|
| Party별 mute | Post-MVP. Launch MVP는 사용자 전역 카테고리 토글만 |
| 알림 action button(알림에서 바로 수락·거절) | Post-MVP. 인증 토큰을 알림 action에 실어야 해서 별도 설계가 필요하다 |
| `notification_ref_key` 회전 주기 | 현재 수동 회전만. 자동 주기는 설계 11에서 정한다 |
| 약속 시작 전 리마인더 | 범위 밖. 외부 캘린더의 기본 알림이 담당한다 |
| 알림 문구 A/B | 범위 밖 |
| Android·웹 | 범위 밖 |

### 17.2 선행 설계 개정

- **설계 8 §9.3에 `calendar_cleanup_suggested` outbox type을 추가한다.** dedupe key는 `confirmed_event_id:user_id`, 계기는 W6이다.
- **설계 2 §8 `devices` 테이블에 `push_authorization`, `push_environment`, `time_sensitive_setting` 3개 열을 추가한다.** 기존 열과 제약은 그대로다.
- **설계 2 §7.3의 iOS 로컬 SwiftData 저장소를 App Group 공유 컨테이너로 옮긴다.** NSE가 별도 샌드박스에서 실행되므로 공유 컨테이너가 아니면 Party 이름을 읽을 수 없고 §4.2의 문구 완성이 항상 기본 문구로 낮아진다. `notification_ref_key`도 공유 keychain access group에 저장한다.
- **설계 2 §7.2의 `/v1/sync/bootstrap` 응답에 `notification_ref_key`를 추가한다.** 현재 응답 필드가 `schema_version`, `snapshot`, `cursor_watermark`, `server_time`의 닫힌 목록이다. `GET /v1/me`도 같은 필드를 반환한다.
- 설계 5 §12·§17, 설계 7 §9.3, 설계 8 §9.3의 "APNs payload는 opaque marker만" 문장은 **개정하지 않는다.** §4.2의 기기 측 문구 완성이 그 제약을 그대로 만족한다.

### 17.3 후속 설계 제약

- 설계 10(결제)이 구독 만료·갱신 실패 알림을 추가하면 새 카테고리 `billing`을 만들고 §5.1 표에 행을 추가한다. 기존 4개 카테고리에 섞지 않는다.
- 설계 11(계정 삭제·관측)은 삭제 요청 시 모든 알림 job을 `dropped`로 종료하는 §12 항목을 삭제 절차에 포함한다.
- 새 domain outbox type을 추가하는 모든 후속 설계는 §5.1 표에 행을 함께 추가해야 한다. 추가하지 않으면 §5.3의 경보가 발생한다.
