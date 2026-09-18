# 알림 종류와 전송 규칙 구현 계획

기준 설계: `docs/notification_design.md`

## 전제

- 상세 설계 9가 마지막 설계다. 이 계획의 실행은 설계 1~9 구현 순서에서 가장 뒤에 온다.
- 서버 골격(P0), Party membership, calendar connection·snapshot, proposal 확정, calendar write가 모두 선행한다. 알림은 그 설계들이 발행하는 outbox를 소비할 뿐이므로 소비 대상이 없으면 구현할 수 없다.
- 설계 9는 선행 설계 2건을 개정했다. 개정 내용은 각 계획 파일에 **이미 직접 반영해 두었다.** 위임 문장만으로는 실행되지 않기 때문이다.
  - 설계 8 §9.3 `calendar_cleanup_suggested` outbox type → `calendar-write-implementation.md` W1에 기재
  - 설계 2 §8 `devices.push_authorization`/`push_environment`/`time_sensitive_setting` 열 → `account-backend-implementation.md` step 2에 기재
  - 설계 2 §7.2 bootstrap·`/v1/me`의 `notification_ref_key`, §7.3 App Group 공유 컨테이너 → `account-backend-implementation.md` step 6에 기재
- 기존 Swift 데모는 각 단계에서 계속 빌드·실행 가능해야 한다.
- 구현 브랜치는 `feature/notification`을 사용한다.

## 단계

### N0. 매핑과 판정 계약 테스트

- dispatch mapper 20종 매핑표(수신자·채널·urgency·quiet·`valid_until`·precondition)
- quiet hours 판정: 자정 넘김, DST gap/overlap, 시간대 미설정
- silent 예산 창 합산과 시간당 상한
- `notification_key` 파생, `party_ref`/`entity_ref` HMAC 파생과 사용자 간 비충돌
- 완료 기준: 설계 §15 「순수 단위 테스트」 목록이 테스트로 표현되고 구현 전 실패한다. DB 제약·iOS·E2E 조건은 각각 N1·N5·N7이 담당한다

### N1. 스키마와 제약

- `notification_settings`, `notification_category_prefs`, `notification_ref_keys`
- `notification_jobs`, `notification_deliveries`, `notification_silent_budget`
- `(notification_key, user_id)` unique index (영속, 성공 후에도 유지)
- `notification_deliveries`는 `owner_job_id`, `claimed_at`, nullable `first_sent_at`을 갖는다. claim-then-send(설계 §6.2)가 "내 이전 시도"와 "다른 worker의 claim"을 `owner_job_id`로 구별한다
- silent 예산은 `notification_silent_sends(user_id, sent_at)` 이동 창이다. 고정 창 카운터를 쓰면 설계 §4.5의 상한이 무너진다
- `devices`의 알림 3개 열과 bootstrap `notification_ref_key`가 이미 적용되어 있는지 확인
- 90일 보관 정리 job과 인덱스를 함께 설계한다
- 완료 기준: 같은 `(notification_key, user_id)` 재삽입이 DB에서 거부되고, outbox의 활성 전용 partial index와 독립임을 테스트가 보인다

### N2. Dispatch mapper와 worker

- outbox claim → 수신자 조회 → `notification_jobs` 삽입 → outbox `succeeded` 전이를 한 transaction으로
- 설계 4의 6종은 §9.3 수신자 집합을 그대로 사용(재유도 금지)
- 설계 7·8의 14종은 설계 9 §5.1 표로 수신자 결정
- `recipient_scope`가 `snapshot_at_publish`인 5종은 현재 상태가 아니라 **발행 시각 기준**으로 수신자를 재구성한다(`status='active' OR ended_at >= outbox_jobs.created_at`). 현재 상태로 조회하면 강퇴 대상·해산 전원·떠난 사용자가 전부 누락된다
- 미지 type은 알림 0건 + 운영 경보 + outbox `succeeded`. 수신자를 알 수 없으므로 silent도 추측 발송하지 않는다(설계 §5.3)
- 잠금 순서 준수(§7.2)
- silent는 mapper 시점에 즉시 발행한다. `notification_silent_sends` 이동 창 판정(최근 60분 3건)을 여기서 적용한다
- 완료 기준: worker 동시 claim에서 알림 job 중복 생성 0건, 10명 Party 이벤트가 최대 9건 생성, quiet hours 안에서도 silent가 나간다

### N3. quiet hours, defer와 합산

- `deferred_until` 계산과 창 종료 시 카테고리별 합산(`_multi` 문구)
- `drop_if_stale` 판정
- 설계 6 활동 시간 `time_zone` 참조(별도 시간대 소스 금지)
- 완료 기준: 창 안 3건이 1건으로 합산되고 `valid_until` 경과 건이 `dropped`로 끝난다

### N4. 발송 직전 precondition과 APNs 전송

- §4.6 6개 확인 항목. `recipient_scope`(`active_members_only`/`snapshot_at_publish`/`self_only`)로 멤버십 재확인 여부를 가른다
- claim-then-send 순서: precondition(후보 전체) → 통과 집합 내 합산 → delivery row + `sending` 커밋 → APNs → `sent`
- delivery row는 `ON CONFLICT DO NOTHING RETURNING owner_job_id`. 내 job id일 때만 진행(재시도), 다르면 `coalesced`
- `sending` lease 만료 회수: `owner_job_id`가 그 job이고 `first_sent_at`이 NULL이면 재발송 없이 `dead` + 경보
- provider token(JWT) 관리, HTTP/2 연결, `apns-expiration`은 상수(`now + 24시간`, refresh만 `now + 120초`)
- 응답 코드별 처리(§11.2), token 무효화
- 즉시 발송 대상 alert의 60초 창 내 silent 억제. defer·`dropped`된 alert는 억제하지 않는다
- writer device 우선 발송과 `_no_writer` 변형
- worker 폴링 주기 상한 30초. `proposal_calendar_refresh_requested`는 도메인 커밋 직후 worker 깨우기로 즉시 claim
- 완료 기준: `dropped`/`coalesced` job의 APNs 호출 0건, `410 Unregistered` 이후 재발송 0건, `429`/`503` 재시도가 unique violation 없이 통과한다

### N5. 알림 설정 API

- `GET`/`PATCH /v1/me/notification-settings`, `expected_version`
- `PUT /v1/devices/{id}/push-token`에 `push_authorization`/`push_environment`/`time_sensitive_setting` 추가. `GET /v1/me`와 bootstrap 응답에 `notification_ref_key` 추가
- 오류 코드 5종(§8.4), `activity_timezone_required` gate
- 완료 기준: 버전 충돌 `409`, 타 사용자 설정 접근 경로 부재

### N6. Payload 계약과 privacy 회귀

- alert/silent payload fixture와 금지 키·값 allowlist 스캔
- 로그·trace·분석 fixture 스캔
- `notification_ref_key`의 sync 전달 경로(`/v1/me`, `/v1/sync/bootstrap`)
- 완료 기준: CI에 스캔이 들어가고 설계 §13 「Privacy」 항목이 전부 통과한다

### N7. iOS: 권한, NSE, 라우팅

- `src/project.yml`에 NSE target + App Group entitlement + keychain access group + Time Sensitive entitlement 추가. Developer Console의 Time Sensitive Notifications capability 활성화와 provisioning profile 갱신이 선행한다
- SwiftData 저장소를 App Group 공유 컨테이너로 이전(설계 2 step 6과 동일 작업)
- 권한 요청 시점(§9.1)과 사전 설명 화면
- `party_ref` 역조회 표 구성, 키 회전 후 재구성
- NSE 문구 완성과 낮춤 3경로(로컬 없음·예산 초과·떠난 Party)
- 포그라운드 배너 억제, 알림 탭 라우팅과 fallback
- 권한 거부 사용자용 §9.4 배지·배너
- 완료 기준: NSE가 App Group 컨테이너에서 Party 이름을 읽어 문구를 완성하고 그 외 어떤 내용도 넣지 않는다. 실기기에서 Focus를 켠 채 `time-sensitive` 알림이 뚫고 들어온다. 권한 거부 사용자가 배지·배너로 전 항목을 확인한다

### N8. E2E

- 3명 Party → 제안 → 1명 거절 → 생성자만 alert
- 전원 수락 → 확정 alert 1건씩, `confirmed_event_created` 중복 없음
- quiet hours 안 제안 생성 → 아침 1건
- refresh 요청 → quiet hours 뚫고 즉시, 2분 뒤 미도달 건 폐기
- Party 해산 → 전원 `calendar_cleanup_suggested` 수신 후 정리
- 권한 거부 사용자 → 알림 0건, 앱 배지로 확인
- 완료 기준: 설계 §15 「E2E」 6개 시나리오 통과

## 위험과 대응

| 위험 | 대응 |
|---|---|
| silent push throttle로 sync 지연 | 설계 3 §7.3대로 앱 활성화 sync가 기준. silent는 가속 수단으로만 취급하고 도달률을 인수 조건에 넣지 않는다 |
| NSE 예산 초과로 문구가 늘 기본값 | N7에서 로컬 조회를 ref 역조회 표 단일 읽기로 제한하고 예산 측정을 테스트에 포함 |
| defer 합산이 중요한 알림을 삼킴 | `_multi` 문구로 존재를 알린다. `bypass` type은 defer 자체가 없어 합산 대상이 아니다(설계 §4.4). 합산은 precondition 통과 집합 안에서만 하므로 승자 탈락으로 창 전체가 0건이 되지 않는다 |
| 선행 설계가 새 outbox type을 추가하고 설계 9 표를 갱신하지 않음 | N2의 미지 type 경보로 조용한 누락을 막는다 |
| `notification_deliveries` 무한 증가 | 90일 보관 후 정리 job. N1에서 인덱스와 함께 설계 |
