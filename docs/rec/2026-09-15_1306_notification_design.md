# 알림 종류와 전송 규칙 상세 설계 확정 (상세 설계 9)

## 작업 내용

상세 설계 9를 `docs/notification_design.md`에 확정했다. 상세 설계 형식 12개 항목을 모두 채웠다.
이로써 상세 설계 1~9가 전부 끝났고 설계 단계가 종료됐다.

## 배경

설계 4·7·8이 domain outbox 이벤트를 발행하면서 문구·수신자·urgency·quiet hours·묶음 처리를
모두 설계 9로 넘겨둔 상태였다. 설계 9는 그 미결 항목을 받아 전송 규칙만 확정하고,
이벤트를 만드는 조건은 각 선행 설계가 계속 소유하게 했다.

## 확정한 핵심 결정

1. **채널 분리.** silent push(sync 재촉)와 alert push(사용자에게 보이는 알림)를 나누고
   한 이벤트가 둘 다, 하나만, 또는 아무것도 만들지 않을 수 있게 했다.
   선행 설계들이 이미 두 경로를 전제하고 있었는데(설계 2 §7.3, 설계 3 §7.3/§8/§10.1)
   설계 9 이전까지는 명시적으로 분리된 적이 없었다.

2. **문구는 기기에서 완성한다.** 사용자 선택에 따라 payload는 localization key와 opaque 참조만 담고
   Notification Service Extension이 로컬 데이터로 Party 이름을 채운다.
   대안은 서버가 Party 이름을 payload에 넣는 것이었으나, 설계 5·7·8에 이미 확정된
   "APNs payload는 opaque marker만" 문장 3곳을 개정해야 했다. 선택한 방식은 개정이 필요 없다.

3. **사용자별 HMAC 참조.** payload에 UUID를 담지 않는다.
   `party_ref = HMAC(notification_ref_key, party_id)` 형태로 파생하며 키는 인증된 sync 채널로만 전달한다.
   같은 Party라도 사용자마다 ref가 달라 APNs 경유 트래픽으로 사용자 간 연결이 드러나지 않는다.

4. **quiet hours의 결과는 3종.** bypass / defer / drop_if_stale.
   `proposal_calendar_refresh_requested`는 기한이 2분(설계 3 §9, 설계 7 T4)이라 미루면 배달 시점에 무의미하므로 bypass한다.
   `calendar_write_command_pending`은 밤 사이 쌓였다가 아침에 한꺼번에 나가는 것을 막기 위해 drop_if_stale이다.

5. **중복 억제를 outbox index에 의존하지 않는다.**
   `outbox_jobs`의 partial unique index는 `status IN ('pending','running','retryable_failed')`에만 걸려
   성공 후 같은 key의 재삽입을 허용한다. 설계 8의 `calendar_write_command_pending` 3회 재발행이 이에 의존하므로
   그 동작을 깨지 않으면서 중복만 막으려면 `(notification_key, user_id)` 영속 기록이 따로 필요했다.

6. **설계 4의 수신자 집합은 재유도하지 않는다.**
   설계 4 §9.3은 6종의 수신자를 이미 확정했고 설계 9에 문구·urgency·quiet hours·묶음만 위임했다.
   설계 9는 그 집합 안에서 행위자에게 alert 대신 silent만 보내는 채널 조정만 한다.

7. **알림 중복 제거.** 같은 순간 같은 대상에게 두 번 울리는 경로 2개를 찾아 막았다.
   - `proposal_confirmed`(설계 7)와 `confirmed_event_created`(설계 8)는 같은 transaction·같은 대상 → 후자를 silent만으로
   - `proposal_superseded`와 대체 제안의 `proposal_created`도 같은 대상 → 전자를 silent만으로

## 백로그 미결 질문 해소

- **초대 알림**: 설계 4가 재사용 링크 + 앱 내 계정 필수를 확정했으므로 서버가 아는 수신자가 없다.
  초대 발송 푸시는 만들지 않고 가입 후 기존 멤버에게 `notify_member_joined`만 보낸다.
  백로그 전체 기능 목록 10행에서 "초대"를 빼고 "초대 푸시는 없음"을 명시했다.

## 선행 설계 개정 4건

| 설계 | 개정 | 이유 |
|---|---|---|
| 8 §9.3 | `calendar_cleanup_suggested` outbox type 추가 (dedupe `confirmed_event_id:user_id`) | 설계 8은 `calendar_cleanup_suggestions`를 sync entity로만 정의해 푸시 경로가 없었다. 앱을 열지 않으면 떠난 Party·해산 Party의 외부 캘린더에 남은 일정을 알 수 없다 |
| 2 §8 | `devices`에 `push_authorization`, `push_environment`, `time_sensitive_setting` 열 추가 | token null 여부로는 권한 거부와 미요청을 구분할 수 없다. Time Sensitive는 사용자가 앱별로 끌 수 있어 서버가 상태를 알아야 강등할 수 있다 |
| 2 §7.2 | bootstrap과 `GET /v1/me` 응답에 `notification_ref_key` 추가 | 기기가 opaque 참조를 역조회하려면 키가 필요한데 응답 필드가 닫힌 목록이었다 |
| 2 §7.3 | SwiftData 저장소를 App Group 공유 컨테이너로, 키를 공유 keychain access group으로 | NSE는 별도 샌드박스라 앱 본체 저장소를 읽을 수 없다. 이게 없으면 문구 완성이 항상 기본 문구로 낮아져 핵심 결정 2가 무력해진다 |

개정 내용은 해당 설계 문서와 각 구현 계획 파일에 직접 반영했다. 위임 문장만 남기면 실행되지 않기 때문이다.

설계 5 §11, 설계 7 §9.3, 설계 8 §9.3의 "APNs payload는 opaque marker만" 문장은 개정하지 않았다.
기기 측 문구 완성이 그 제약을 그대로 만족한다.

## 변경 파일

| 파일 | 변경 |
|---|---|
| `docs/notification_design.md` | 신규. 상세 설계 9 본문 |
| `.omc/plans/notification-implementation.md` | 신규. N0~N8 구현 계획 |
| `docs/calendar_write_design.md` | `calendar_cleanup_suggested` type 추가, outbox 4종 → 5종 |
| `docs/account_backend_design.md` | `devices` 열 2개 추가, §17 알림 항목에 설계 9 링크 |
| `docs/feature_design_backlog.md` | 현재 실행 컷라인을 구현 착수로 전환, 확정 설계 9 절 추가, 기능 목록 10행 갱신 |
| `docs/implementation_plan.md` | "상세 설계 1~7 완료" 표기를 1~9 완료로 수정 |
| `docs/progressing.md` | 설계 9 확정과 다음 작업 갱신 |

## 재검증 1차 (검토 에이전트)

검토 에이전트가 설계 9와 선행 설계 8건을 대조해 Critical 3건, High 6건, Medium 7건, Low 5건을 지적했고 전부 반영했다. 판정은 REVISE였다.

**Critical 3건**

1. **§4.6의 전역 precondition이 §5.1이 약속한 수신자를 폐기했다.** "활성 멤버가 아니면 폐기"가 전역 규칙이었는데, 강퇴 대상·해산 직후 전원·해산 transaction에서 취소된 확정 일정의 참여자는 정의상 활성 멤버가 아니다. 이 알림들이 100% `dropped`될 설계였고 §13의 인수 조건과 정면으로 충돌했다. → §5.1에 `recipient_scope` 열(`active_members_only`/`snapshot_at_publish`/`self_only`)을 신설해 type별로 판단하게 바꿨다.
2. **`sending` 상태에 출구 간선이 없었다.** worker가 APNs 호출 중 죽으면 회수 경로가 없어 알림이 영구 정지하고, 회수하면 `notification_deliveries`가 `200`에만 기록되므로 중복 발송돼 불변식 1이 깨졌다. → claim-then-send로 순서를 뒤집고(precondition → delivery row + `sending` 커밋 → APNs), 회수 간선을 추가하고, **at-most-once**를 명시적으로 택했다.
3. **약속 시작 시각이 payload `u`와 `apns-expiration` 헤더로 새어 나갔다.** `proposal_confirmed`의 `valid_until`이 곧 약속 시작 시각인데 §10.4가 시각을 금지하고 있었다. → `u`를 삭제하고 `apns-expiration`에 `now + 36시간` 상한을 뒀다.

**High 6건**

- "alert push는 앱을 깨우므로"가 사실이 아니었다. `mutable-content`가 깨우는 것은 NSE이고 NSE는 네트워크를 쓰지 않으므로, silent 억제 규칙이 백그라운드 sync를 통째로 없앴다. → alert payload에 `content-available: 1`을 추가하고 best-effort임을 명시했다.
- `time_sensitive`도 entitlement가 필요하다. 없으면 quiet hours를 뚫는 유일한 경로가 조용히 작동하지 않는다. 사용자가 앱별로 끌 수도 있어 서버가 상태를 받아 강등해야 한다.
- `drop_if_stale`이 지정된 유일한 type에서 절대 발동하지 않았다. `valid_until`이 최소 +24시간인데 quiet hours 창은 최대 23.5시간이다. 또 마지막 notice에는 `valid_until`이 정의되지 않아 불변식 4를 위반했다. → 생성 +12시간 상한을 뒀다.
- §5.1이 `entity_type`/`entity_id`를 정의하지 않아 `collapse-id` 의미가 미정이었다. Party 단위로 잡으면 멤버 3명 순차 합류 시 알림 1건만 보인다. 또 §6.3이 collapse-id를 멱등성 수단으로 오용했다. → 표 A에 entity 열을 넣고 설계 4의 6종을 사건 단위로 잡았으며, §6.3에서 collapse-id를 제거했다.
- NSE는 App Group 없이 앱의 SwiftData도 `notification_ref_key`도 읽을 수 없다. 설계 2 개정 2건이 누락돼 있었다.
- 구현 계획이 두 개정을 "다른 계획 파일의 특정 단계에 포함"으로 위임했는데 그 파일들에 해당 내용이 없었다. 게다가 다음에 실행될 account-backend P0가 바로 그 열을 모르는 상태였다. → 두 계획 파일에 직접 기재했다.

**Medium·Low 12건** — 교차 참조 5건 오류(설계 7의 기한 필드는 `deadline_at`이 아니라 `response_due_at`, DST 규칙은 설계 6 §6이 아니라 §3.3이고 overlap 처리 방향도 달랐다), `confirmed_event_created`가 카테고리 미배정, 미지 type fallback이 실행 불가능, defer 중 quiet hours 변경 경로 누락, 불변식 5와 §12 충돌, silent 예산이 고정 창이라 상한 우회 가능, HMAC 잘림 표기 모호 등. 전부 반영했다.

검토가 지적한 가장 높은 레버리지 수정은 **§5.1 표에 열 4개(`category`, `recipient_scope`, `entity_type`, `entity_id`) 추가**였다. 이 하나로 Critical 1건과 High 1건, Medium 2건이 함께 닫혔다. 표가 dispatch mapper의 완전한 명세여야 하는데 구현에 필요한 열이 빠져 있었던 것이 근본 원인이다.

1차 반영 직후 자체 점검에서 모순 1건을 더 찾았다. `notification_key`에 채널이 없어 `A+S` 16종의 alert job과 silent job이 같은 key를 갖는데, claim-then-send가 APNs 호출 전에 delivery row를 잡으므로 먼저 도는 쪽이 다른 쪽을 unique index로 막는다. H1에서 방금 고친 silent 소실이 그대로 되돌아오는 경로였다. `notification_jobs`·`notification_deliveries`를 alert 전용으로 확정해 충돌 자체를 없앴다.

## 재검증 2차 (검토 에이전트)

1차 수정의 델타만 범위로 다시 검토했다. 11건 중 6건은 완전히 닫혔고 5건이 부분 종결이었다. 새로 Critical 3건, High 8건, Medium 5건, Low 4건을 지적받아 전부 반영했다. **지적의 대부분이 1차 수정 자체가 만든 새 모순이었다.**

**Critical 3건**

1. **`snapshot_at_publish`의 계약이 mapper 입력 계약과 충돌했다.** "발행 시점의 수신자 목록을 고정한다"고 정의해 놓고, mapper 입력은 "현재 멤버십·제안·command 상태"로 못박혀 있었다. 고정할 목록을 만드는 시점 자체가 설계에 없었다. → 목록을 저장하지 않고 발행 시각 기준으로 재구성하는 술어(`status='active' OR ended_at >= outbox_jobs.created_at`)를 §5.1에 박고 mapper 입력에 발행 시각을 추가했다. 설계 4의 `ended_at`이 이미 있어 추가 저장이 필요 없다.
2. **재시도 경로가 delivery row unique index와 충돌했다.** claim-then-send가 APNs 호출 전에 row를 잡는데 `429`/`503` 재시도가 다시 들어오면 unique violation이 난다. `ON CONFLICT DO NOTHING` 후 무조건 진행하면 동시 claim 방어가 무너지고, 무조건 막으면 재시도 규칙이 죽은 문장이 된다. 영속 상태만으로 "내 이전 시도"와 "다른 worker의 claim"을 구별할 수단이 없었다. → `owner_job_id` 열을 추가하고 `ON CONFLICT DO NOTHING RETURNING`으로 자기 job일 때만 진행하게 했다.
3. **1차의 시각 누출 수정이 절반만 닫혀 있었다.** `apns-expiration = min(valid_until, now+36시간)`인데 약속이 36시간 **안**이면 `min`이 `valid_until`을 그대로 고른다. 오늘 확정해서 오늘 저녁에 만나는 흔한 경우가 전부 여기 해당해 상한이 아무 일도 하지 않았다. 게다가 불변식 6과 Privacy 인수 조건이 "그런 경우가 없다"고 단언해 CI가 거짓 음성을 냈다. → 헤더를 `valid_until`과 완전히 분리해 상수(`now+24시간`, refresh만 `now+120초`)로 바꿨다.

**High 8건 중 주요한 것**

- **합산이 precondition보다 앞서서 창 전체가 0건이 될 수 있었다.** 승자가 precondition에서 탈락하면 패자들은 이미 `coalesced` 종단이라 되살아나지 못한다. → 순서를 뒤집어 precondition 통과 집합 안에서만 합산한다.
- **silent 예산 스키마가 §4.5가 명시적으로 배격한 고정 창이었다.** 채널 분리로 이 테이블이 silent의 유일한 표현이 되어 하중을 받는 구조가 됐는데도 `window_started_at`+카운터였다. → `notification_silent_sends(user_id, sent_at)` 이동 창으로 바꿨다.
- **`A+S` 16종에서 silent를 언제 내보내는지가 미정이었고 구현 계획이 나쁜 쪽으로 해소했다.** 계획이 silent를 alert 발송 단계에 넣어, defer된 alert의 silent도 아침까지 미뤄져 밤사이 sync가 정지할 설계였다. → silent는 mapper 시점에 즉시 발행하고, 억제는 alert가 defer 없이 즉시 발송될 때만 적용한다.
- **2분 기한 알림을 지연시키는 두 경로가 "보장"으로 서술돼 있었다.** 즉시 깨우기 실패 시 "일반 폴링이 뒤늦게 집으므로 누락되지 않는다"고 썼는데 폴링 주기가 문서에 없었고, `time_sensitive_setting` 강등 시 quiet hours를 적용하면 확정적으로 폐기되는데 인수 조건은 "받는다"고 단언했다. → 폴링 주기 상한 30초를 박고 손실 가능성을 명시했으며, 강등 시 `bypass`는 유지하도록 바꿨다.
- 1차 수정이 §7.3·§13·§6.1·§16·§4.9에 전파되지 않아 고친 절과 안 고친 절이 반대 결과를 내는 곳이 5군데였다.
- **구현 계획이 설계가 금지한 동작을 지시하고 있었다.** 미지 type에 "silent만 발행"이라 적혀 있었는데 §5.3은 "수신자를 알 수 없으므로 추측 발송하지 않는다"로 고쳐진 상태였다.

**Medium·Low 9건** — `defer`와 `drop_if_stale`이 동작상 구별되지 않아(둘 다 §4.6에서 만료 확인을 거친다) 모드를 2종으로 줄였고, 순차 합류 인수 조건이 quiet hours 안에서 거짓이라 단서를 붙였다. 그 밖에 계획 파일 3건, 상태도 간선 1건, 인용 1건, 세는 법 1건, 라벨 충돌 1건을 정리했다.

**문서에 없던 것 5건**도 함께 채웠다. worker 폴링 주기, `self_only` 3종의 `party_id` 값(합산 키가 `(user_id, party_id, category)`이라 NULL이면 그룹이 정의되지 않는다), 권한 거부 시 `dropped`된 alert가 silent를 억제하지 않는다는 규칙, 키 회전 중 `deferred` job의 ref 처리(구 키 유지 + 직전 키 1개 보관), 30일 defer와 90일 정리의 관계.

## 다음 작업

설계 단계가 끝났다. 다음은 `.omc/plans/party-membership-implementation.md`의 P0(서버 골격) 구현이다.
서버 코드가 없으면 설계 4~9의 어떤 부분도 구현할 수 없다.
