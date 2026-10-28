# P6-lite: Party sync 전파 감사·보강과 내 Party 목록 (기능 구현·검증 완료)

- 일시: 2026-10-28 16:13
- 브랜치: `feature/party-sync`
- 근거: `docs/party_membership_design.md` §5.5·§9.2, `docs/party_visibility_design.md`, `docs/account_backend_design.md` §7, `.omc/plans/party-membership-implementation.md` P6, `docs/project_goal.md` 체크리스트 2

## 1. 감사 매트릭스

entity: P=`party`, M=`party_membership`, I=`party_invite`, V=`party_visibility_setting`, S=`party_schedule_projection`.
모든 발행은 상태 전이와 같은 mutation 트랜잭션 안이다. "v"는 version 있는 tombstone, "∅"는 version 없는 tombstone.
아래는 최종 상태다(초대 tombstone은 4절 수정으로 모두 v). 모든 행은 수신자별 `scope_membership_id`를 싣는다(4절).

| 경로 | 수신자와 변경 | 설계 대비 |
|---|---|---|
| T1 생성 | 생성자: P·M·V upsert, 본인 S upsert | 일치. **순서 수정**(아래 2-②) |
| Party 이름 수정 | 활성 멤버 전원: P upsert | 일치 |
| T2 초대 생성 | 현재 방장: I upsert(token 없음) | 일치 |
| 초대 무효화 | 현재 방장: I v | 일치(∅ → v, 4절) |
| T3 수락 | 신규 멤버: P, 모든 활성 M, 본인 V, 다른 멤버 S(Party 단위 부트스트랩). 기존 멤버: P, 신규 M. 전원: 신규 멤버 S. 방장: I upsert | **소진 시 I upsert → I tombstone으로 수정**(2-①, 지금은 v), 순서 수정 |
| 초대 만료 | 발행 없음(만료 scheduler가 없다. status는 active로 남는다) | 편차 3-② |
| T4 위임 | 활성 멤버 전원: P·옛 방장 M·새 방장 M upsert. 이전·새 방장에게만: 무효화한 I v | 일치 |
| T5 탈퇴 / T6 강퇴 | 떠난 사람: 본인 M v, P ∅, 다른 M ∅, 다른 멤버 S ∅, 본인 S v, 본인 V v. 남은 멤버: 떠난 M v, 떠난 사람 S v, P upsert | 일치(10-14 rec의 tombstone version 규칙) |
| 단독 방장 탈퇴 → T7 | T7과 같음 | 일치 |
| T7 해산 | 마지막 활성 멤버 전원: P v, 모든 M v, 모든 S v, 본인 V v. 방장만: 활성 I v | 일치 |
| 공개 수준 변경 | 본인: V upsert. 활성 멤버 전원: 본인 S upsert·v(재구성 결과) | 일치(details ciphertext는 sync에 없다) |
| 캘린더 snapshot complete·abort·연결 PUT | `RebuildBusyProjections*`가 활성 멤버 전원에게 S upsert·v | 일치(`active_generation_usable` 판정 포함) |
| 표시 이름 변경(PATCH /v1/me) | 본인: user upsert. **각 Party 활성 멤버 전원: 본인 M upsert(새 version)** | **신설**(2-③) |
| 계정 삭제 완료 | 각 Party 활성 멤버 전원: 삭제 사용자 M upsert(이름 `탈퇴한 사용자`, 새 version). 멤버십 종료는 없음 | 이름 전파는 이번에 수정(4-2), 강제 위임·해산은 P8 |
| 제안 | 없음 | 설계 7 범위 |

bootstrap(`party.BootstrapProjections`)은 활성 Party마다 P, 활성 M 전부, 본인 V, 방장이면 status=active I, Party의 S 전부를
같은 투영 함수로 싣는다. S는 비노출 상태면 이미 지워져 있으므로(가시성은 projection 존재로 표현) 추가 필터가 필요 없다.
최근 30일 안에 끝난 본인 멤버십은 하한 전용 M v 항목으로 싣는다(4절).

## 2. 메운 공백

1. **소진 초대(`2de0e87`):** 수락으로 `exhausted`가 되면 방장에게 status=exhausted upsert를 보냈는데 bootstrap은 활성 초대만 싣는다. 방장 모델에 소진 초대가 영구히 남았다. 소진이면 I tombstone을 보낸다(4절에서 version 있는 tombstone으로 바꿨다). 수정을 되돌리면 인수 테스트가 `[accept B] A1 남음 party_invite … status:exhausted`로 실패함을 확인했다.
2. **부모 먼저(`97fbf97`):** T1·T3에서 S upsert가 같은 트랜잭션 안에서 P·M보다 먼저 기록됐다. T1은 P·M·V 발행 뒤 projection을 만들고, T3는 `emitJoin` 뒤로 projection 생성을 옮겼다(calendar connection 잠금이 가장 마지막이 되는 부수 효과). 인수 테스트 모델이 "P보다 먼저 온 하위 upsert"를 실패로 보고, 옛 코드에서 실패함을 확인했다. **한 트랜잭션 안의 순서만 보장한다**(4절).
3. **표시 이름(`49d0456`, lead 승인 A안):** M 투영에 `display_name`을 더했다(bootstrap·증분 공통 함수). `party.PropagateDisplayName`이 `LockUserPartiesShared`(parties FOR SHARE, id 순) 뒤 본인 활성 멤버십 version을 올리고 Party별 활성 멤버 전원에게 M upsert를 보낸다. 잠금은 users(이미 FOR NO KEY UPDATE) → parties → 멤버십이라 T3(users → parties FOR UPDATE)·탈퇴(parties FOR UPDATE → 멤버십)와 어긋나지 않는다. Party 잠금 없이는 동시 가입자가 옛 이름만 받을 수 있다.
   - 부수 효과: 멤버십 version이 가능 시간 calculationVersion tuple에 있어 이름 변경이 검색 결과(최대 5분)를 stale로 만든다. lead 결정으로 허용했다(검토 결과는 4-2).

## 3. 설계 편차·계약

1. **`GET /v1/parties`(`83eab1d`):** 설계 §9.1 표에 없어 응답을 정했다. `{request_id, parties: [{party, membership(호출자 본인), member_count(활성)}]}`, 호출자 가입 시각 순, 페이지 없음(참여 한도 20). 활성 멤버십 + 활성 Party만(bootstrap scope와 같다). `Require` → `ratelimit.Enforce`(`party.list`, 사용자당 60/분, **ModeOpen**: 본인 데이터 읽기라 장애 시 첫 화면이 막히지 않게) → 조회. `Cache-Control: no-store`.
2. **초대 만료:** 만료 scheduler가 없어 status=active로 남고 bootstrap도 그대로 싣는다(목록 API만 `expires_at > now`로 거른다). 증분과 bootstrap은 일치한다. **iOS 계약:** `expires_at`이 지난 초대는 숨긴다. **향후 만료 job은 방장에게 I tombstone을 같은 트랜잭션에서 보내야 한다.**
3. 신규 멤버 부트스트랩은 별도 메커니즘 없이 T3의 증분 row로 보낸다(설계 §9.2 그대로). T3 txid는 신규 멤버의 기존 cursor보다 크다.

## 4. 순서 역전 결함과 scope_membership_id (lead 결정: 1안, `a264511`·`42a286d`)

**결함:** `mutation.Run`의 idempotency INSERT가 첫 쓰기라 txid가 Party 잠금 전에 배정된다. 피드는 txid 순, 직렬화는
잠금 순이라 어긋날 수 있다. version 있는 변경은 version 비교로 수렴하지만 version 없는 tombstone(탈퇴자 scope 정리용
P·다른 M·다른 S, 그리고 당시의 초대 ∅)은 뒤에 오는 옛 upsert를 막지 못했다. 결정적 재현 3종: 탈퇴 vs 동시 Party
변경(탈퇴자 모델에 Party 부활), 위임 vs 동시 수락(옛 방장 모델에 초대 부활), 같은 사용자의 탈퇴 vs 재가입(재가입했는데
모델이 빔). 서버 잠금 순서로는 못 고친다(행 잠금 자체가 xid를 배정하고 T3가 users → parties 두 행을 잡는다).

**수정:**

1. **초대 tombstone은 모두 version이 있다**: 무효화·소진·위임·해산 모두 `used_count + 2`(마지막 upsert `used_count + 1`보다
   1 큼, projection·설정의 "마지막 version + 1" 규칙과 같다). 초대는 다시 active가 되지 않아 하한이 안전하다.
2. **`sync_changes.scope_membership_id`(migration 00018, nullable, FK 없음, down은 열 삭제):** 모든 Party 범위 변경이
   `party.emitScoped`를 지나며 수신자의 이 Party 활성 멤버십, 없으면 가장 최근에 끝난 멤버십(`(status = 'active') DESC,
   ended_at DESC NULLS LAST, id DESC`)을 채운다. `joined_at`은 생성 경로마다 DB 시계(T1)와 앱 시계(T3 `h.now()`)가 섞여
   재가입 멤버십이 옛 멤버십보다 앞선다고 보장하지 못해 쓰지 않는다(틀리면 규칙 (a)가 재가입 뒤 변경을 모두 버린다). 활성 멤버는 활성
   멤버십, 방금 떠났거나 해산으로 끝난 수신자는 이 트랜잭션에서 끝난 그 멤버십, 재가입 뒤에는 새 멤버십이다.
   **sync_changes 행은 이미 수신자별(`recipient_user_id`)이라 값도 수신자별로 정확하다.** 수신자 멤버십이 없으면
   오류로 트랜잭션을 실패시킨다. bootstrap 항목은 호출자의 활성 멤버십을 싣는다. Party 밖 entity는 NULL이다.
3. **bootstrap 하한 항목:** snapshot은 호출자가 피드 보존 기간(30일) 안에 끝낸 본인 멤버십을 version 있는
   `party_membership` tombstone 항목으로 싣는다. snapshot 교체가 하한을 비우므로, 탈퇴는 snapshot에 반영됐지만 그보다
   먼저 직렬화되고 txid가 watermark 뒤인 변경이 다시 오면 규칙 (a)가 그 멤버십이 끝났음을 알아야 한다(재현 5).
   **재검증 High 반영:** 같은 구조가 활성 멤버에게도 생긴다. watermark 아래의 version 있는 tombstone(위임의 초대 무효화,
   hidden 전환의 projection 삭제)은 다시 오지 않는데 그보다 먼저 직렬화된 같은 entity의 upsert는 txid가 watermark 이상이라
   다시 온다. 규칙 (a)는 활성 멤버에게 관여하지 않는다. 그래서 snapshot은 **watermark 이상 txid의 upsert가 이 수신자에게
   기록된 entity 중 version 있는 tombstone이 있는 것**의 최대 tombstone version을 하한 항목으로 싣는다(`syncfeed/bootstrap.go`).
   끝난 entity는 같은 ID로 되살아나지 않으므로 하한이 안전하고, 대상은 watermark 뒤 upsert가 있는 entity뿐이라 작다(재현 6).
   **별도 리뷰 Med 반영(기기):** 기기 폐기는 tombstone이 아니라 revoked=true upsert라 snapshot에서 빠지고 위 하한에도 걸리지 않았다.
   폐기 R(작은 txid)과 같은 기기의 push 갱신 U(큰 txid, 먼저 커밋, revoked=false) 역전 뒤 horizon을 붙잡은 bootstrap이면 U가
   다시 와 폐기 기기가 되살아난다(재현 7). Party 밖 entity(user, device, availability_preference)는 **watermark 아래 이 수신자
   행의 최대 version**을 하한으로 싣도록 일반화했다. 이 entity들은 version 없는 tombstone이 없고 version이 단조 증가한다.
   Party 범위는 일반화하지 않았다. version 없는 scope tombstone 뒤 재가입이 같은 version을 다시 보내므로 upsert version
   하한은 재가입 entity를 막는다.
   - **availability_preference는 bootstrap에 넣지 않는다.** 이 변경은 구간·시간대 없는 본인 무효화 신호이고 클라이언트는
     받으면 `GET /v1/availability/preferences/me`로 현재 값을 다시 읽는다. 옛 신호가 다시 와도 결과는 GET의 현재 값이라
     되살아날 상태가 없다. 그래도 위 일반화로 watermark 뒤 옛 신호는 하한에 걸린다. snapshot에 넣으면 활동 시간 API와
     sync가 같은 값을 두 경로로 갖게 돼 넣지 않았다.
   - **재검증 High 반영:** 순서가 역전되면 비-Party 하한(watermark 아래 최대 version)이 snapshot upsert의 version과 같아진다
     (같은 사용자의 이름 변경 두 건 역전 등). 둘을 함께 실으면 "하한보다 큰 upsert만 적용" 규칙에서 본인 user나 살아 있는
     기기가 사라진다. 서버가 snapshot에 있는 entity에는 하한 항목을 싣지 않는다(재현 8). snapshot version이 로컬 version이
     되어 옛 upsert는 version 비교로 막힌다. 클라이언트 규칙을 "snapshot upsert는 하한과 무관하게 적용"으로 바꾸는 대안은
     구현 실수에 약해 택하지 않았다.
   - 발행된 scope type이 모두 `party.ScopedEntityTypes`에 있는지 순차 인수 테스트가 대조한다(새 Party 범위 type 누락 방지).
   - `ended_at`은 앱 시계(`h.now()`)로 기록하고 창은 DB `now()`로 계산해 두 시계가 섞이지만 30일 창에서는 무시할 수준이다.
   - 30일 창의 기준 시각을 앱 시계에서 DB `now()`로 바꿨다. 창은 horizon이 보존 기간 이상 묶이지 않는다는 가정이며 §12
     horizon lag 경보와 연결된다(설계 §7.2에 명시).
4. **클라이언트 규칙(설계 2 §7.1·§7.2 개정, OpenAPI `SyncChange` 설명·`scope_membership_id` 필드):**
   (a) scope가 끝난 멤버십(하한이 기록된 `party_membership` ID)인 upsert는 버린다.
   (b) version 없는 tombstone은 그 entity의 Party에 scope가 아닌 본인 활성 멤버십이 로컬에 있으면 무시한다.
   부모 없는 entity는 버리지 않고 보관한다.
   - **(b)를 "로컬 사본의 scope가 같을 때만 삭제"로 정했다가 바꿨다.** 재가입 부트스트랩은 다른 멤버 entity를 같은
     version으로 다시 보내므로 사본의 scope가 갱신되지 않아 재현 3이 여전히 실패했다. 본인 활성 멤버십 기준이면 사본별
     scope를 저장할 필요도 없다. 정상 탈퇴는 `endMembershipTx`가 본인 멤버십 tombstone을 scope tombstone보다 먼저
     보내므로 남은 본인 활성 멤버십이 없다.
- 탈락: 서버만으로 순서를 맞추는 안(잠금 뒤 txid 배정) — 두 행을 잡는 경로에서 다시 어긋난다. 커밋 순서 키 — 설계 §7.1이
  기각한 전역 직렬화. 재가입 시 공유 entity version을 일괄 올리는 안 — projection 수천 건 재발행.

## 4-1. Party 밖 감사 (보고만)

- `user`·`device`(폐기는 revoked upsert)·`availability_preference`(본인 무효화 신호)는 모두 version 있는 upsert만 발행하고
  version 없는 tombstone이 없다. 순서가 역전돼도 version 비교로 수렴한다. 같은 유형 문제 없음.
- calendar 계층은 sync를 직접 쓰지 않고 `party.RebuildBusyProjections*`를 지나므로 위 scope·versioned tombstone 규칙을 그대로
  따른다(projection 삭제는 `last + 1` versioned tombstone). 공개 수준 설정 삭제도 versioned tombstone이다.
- 참고: `availability_preference`는 bootstrap에 없다(증분 전용 무효화 신호, 클라이언트가 GET으로 다시 읽는다). 이번 범위의
  "증분 == bootstrap" 비교 대상(Party 5종)이 아니다.

## 4-2. 표시 이름 조건(lead)과 리뷰 반영

- 같은 이름 PATCH는 아무것도 바꾸지 않는다(users version 유지, 재발행 없음). 투영에는 `display_name`만 더했다(이메일 등 없음).
- 계정 삭제 완료(`deletion.go` finish)가 이름을 `탈퇴한 사용자`로 바꾸는 같은 트랜잭션에서 `PropagateDisplayName`을 부른다
  (`mutation.NewTx`로 worker 트랜잭션을 감싼다. 잠금은 users → devices → sessions → parties FOR SHARE → 멤버십). 멤버십 자체
  정리는 P8이다.
- **calculationVersion 검토(보고만, 바꾸지 않음):** tuple의 `(user_id, membership_id, membership.version)`에서 version은
  가능 시간에 영향이 없는 변경(이름 변경, 위임의 role 변경)으로도 오른다. 가입·탈퇴·재가입은 멤버십 ID 집합(재가입은 새 ID)으로
  이미 드러나므로 version을 빼고 `(user_id, membership_id)`만 넣어도 정확성은 유지되고 이름 변경·위임으로 인한 불필요한 stale이
  사라진다. `parties.version`도 이름 변경·위임으로 오르므로 같은 이유로 뺄 수 있다. 현재는 이름 변경 시 검색 결과가 최대 5분
  유효기간 안에서 stale(409 `availability_result_stale` → 재검색)이 되는 것을 허용한다.
- 리뷰(COMMENT, High 0): [Med] 소진 초대 version 없는 tombstone → 1번으로 해소. [Med] 삭제 익명화 미전파 → 위 수정. [Low]
  같은 이름 재발행 → 수정. [Low] 테스트 모델이 "로컬 version > tombstone version"이면 삭제를 건너뜀 → 계약 위반으로 실패시키게
  고쳤다(서버는 마지막 upsert보다 큰 version만 보낸다). [Low] 이름 변경 vs 탈퇴(§4 범위 확대) → 재현 1에 포함, 규칙 (a)로 해소.
  [Low] 비결정적 경쟁 테스트 → 결정적 재현(이름 변경 vs 가입)을 더했다.

## 5. 테스트

- `server/internal/syncfeed/party_sync_test.go`(실제 HTTP: auth Require → account·party·sync handler, 캘린더는 seed)
  - `TestPartySyncIncrementalMatchesBootstrap`: 3명, 세션 5개(A 두 기기 중 하나는 limit=2 페이지로 몇 단계씩 건너 읽음, B 두 번째 기기는 중간에 처음 bootstrap). 생성 → 1회용 초대 → 수락(소진) → 공개 수준 hidden → busyOnly(versioned tombstone 뒤 새 ID) → 표시 이름 변경 → 이름 변경·초대 2개·무효화 → 위임 → 새 방장 초대 → C 가입 → C 탈퇴 → C 재가입(∅가 하한을 남기지 않음) → 강퇴 → 해산. 단계마다 각 세션이 자기 cursor로 끝까지 읽어 iOS 계약대로 적용한 모델이 새 bootstrap과 같은지(Party 5종, payload는 JSON decode 비교) 10초 수렴 루프로 확인한다. 같은 방식으로 틀려도 통과하지 않게 단계마다 type별 절대 개수도 본다(신규 멤버 부트스트랩, 떠난 멤버 0건, C의 다른 Party Q 유지, 초대는 방장만). `mutation.Retries()` 증가 0.
  - `TestDisplayNameRacingAcceptConverges`: 이름 변경과 같은 Party 가입을 5회 동시 실행(비결정적 보조). 가입 전에 bootstrap한 가입자 모델이 증분만으로 최종 이름에 수렴, 재시도 0.
  - **결정적 순서 역전 재현.** 요청 기기의 `devices` row를 별도 트랜잭션이 `FOR UPDATE`로 잡으면 멱등성 INSERT가 heap에 쓴 뒤(txid 배정) FK 검사(KEY SHARE)에서 멈춘다. `pg_blocking_pids`로 그 대기를 확인한 뒤 다른 요청을 끝까지 실행하고 풀어 "txid는 작은데 나중에 직렬화된" 트랜잭션을 만든다. 각 테스트는 sync_changes txid로 역전이 실제로 일어났는지 먼저 단언한다(같은 key의 ON CONFLICT 대기는 xid 배정 전이라 쓸 수 없었다).
    - `TestLeaveRacingPartyChangeDoesNotResurrect`(탈퇴 vs Party 이름 변경 + 남은 멤버 표시 이름 변경), `TestRejoinRacingLeaveKeepsNewScope`(같은 사용자 재가입 vs 다른 기기 탈퇴): 규칙 모델 == bootstrap, **규칙 (a)(b)를 끈 대조군은 bootstrap과 다름**을 단언.
    - `TestTransferRacingAcceptDoesNotResurrectInvite`: 옛 방장 피드에서 초대 tombstone 뒤에 수락 upsert가 오고 version 하한이 막는다. 옛 방장은 활성 멤버라 규칙 (a)(b)가 관여하지 않으므로 **대조군은 하한 없이 tombstone을 적용하는 모델**이며 그 모델은 bootstrap과 다름을 단언한다.
    - 대조군 단언 전에는 그 수신자의 sync_changes 마지막 위치까지 cursor가 넘을 때까지 읽는다(horizon 지연으로 일부만 받은 상태의 오판 방지). 재현 5는 horizon을 붙잡은 상태에서 watermark가 탈퇴 txid를 넘을 때까지 다시 bootstrap한다. party·calendar·availability 전체 테스트를 동시에 돌리며 재현 테스트 `-count=20` 통과.
    - `TestRenameRacingJoinDeliversNewName`: 이름 변경 upsert가 가입 부트스트랩보다 먼저(부모 없이) 와도 새 이름으로 수렴.
    - `TestBootstrapCarriesTombstoneFloorsForRedeliveredUpserts`(재현 6): 재현 2 + horizon을 붙잡은 bootstrap. snapshot에 초대 하한이 있고 E의 초대 upsert가 다시 와도 버려진다. 하한 없는 모델은 초대가 되살아나고, 새 하한 쿼리를 끄면 실패함을 확인했다.
    - `TestBootstrapOmitsFloorsForSnapshotEntities`(재현 8): 같은 사용자의 이름 변경 두 건 역전 + horizon을 붙잡은 bootstrap. snapshot에 user upsert(최신 version)만 있고 하한 항목은 없으며, 실제 클라이언트처럼 한 저장소에 적용(하한 먼저)한 모델의 증분 결과가 새 bootstrap과 같다. 제외를 끄면 명시 단언을 빼도 모델에서 user가 사라져 실패함을 확인했다.
    - 테스트 모델은 snapshot의 하한 항목과 upsert를 같은 저장소에 적용한다(Party 모델 `fetchBootstrapFloors`, 기기·user 모델 `bootstrapEntities`). 비교 기준은 snapshot의 upsert만이다.
    - `TestBootstrapFloorKeepsRevokedDeviceRevoked`(재현 7): 기기 폐기 vs 같은 기기 push 갱신 역전 + horizon을 붙잡은 bootstrap. 하한 있는 기기 모델의 증분 결과가 새 bootstrap과 같고 하한 없는 모델은 폐기 기기가 되살아난다. 일반화를 끄면 실패함을 확인했다.
    - `TestBootstrapCarriesEndedMembershipFloors`: L과 E 사이 txid를 받은 열린 트랜잭션이 horizon을 붙잡은 채 bootstrap → snapshot은 Party 없음·하한 있음, E가 다시 와도 버려진다. snapshot 하한을 지운 모델은 Party가 되살아난다.
  - `TestSameDisplayNameIsNoop`(version 유지·재발행 0), `TestAccountDeletionAnonymizesNameForCoMembers`(삭제 worker 뒤 동료 모델 이름 = `탈퇴한 사용자` = bootstrap; 수정 제거 시 실패 확인).
  - 테스트 모델은 Party 범위 변경에 scope가 없으면, version 있는 tombstone이 로컬 version 이하이면 실패시킨다.
  - 돌연변이 확인: 소진 수정 제거, `emitPartyProjectionsTo` 제거, 생성 순서 원복, 삭제 이름 전파 제거 각각에서 실패.
- `server/internal/party/invites_test.go` `TestInviteTombstonesAreVersionedAboveUpserts`: 소진·무효화·위임(2)·해산 tombstone 5건 모두 version이 같은 수신자의 모든 upsert보다 크다.
- `server/internal/party/list_test.go`: 활성만·탈퇴·해산 제외·본인 멤버십·멤버 수·비멤버 빈 배열·미인증 401. `ratelimit_test.go` `TestListPartiesLimit`: 61번째 429.
- OpenAPI(`1b0a6eb`): 등록된 route 전부 등재(`GET·POST /v1/parties`, Party 상세·수정, 멤버 목록, 위임, 초대 생성·목록·무효화·미리보기·수락, `GET /v1/calendar/connection`, snapshot abort). Party·PartyMembership schema 공유, 오류 code 9개 추가. ruby YAML 파싱과 `$ref` 전수 해석 확인.
- **테스트 격리(410 flake):** 정리 테스트가 방금 만든 row를 40일 전으로 바꿔 지워 전역 정리 워터마크가 "지금"에 가까워지면, 다른 열린 트랜잭션이 horizon을 붙잡은 동안 bootstrap watermark가 그 이하가 되어 sync 테스트가 410을 받았다(다른 패키지 동시 실행에서 20회 중 8건 재현, 같은 DB를 쓰는 다른 실행의 정리도 관측). 운영 결함이 아니다(30일 넘게 열린 트랜잭션이 필요하다). cursor를 읽는 syncfeed 테스트는 advisory lock을 공유로, 정리 테스트는 배타로 잡고 끝나면 워터마크를 시작 전 값으로 되돌린다(`feed_test.go` `pruneLockKey`). 단독 `-count=30` 2회, 동시 부하 `-count=20` 0건.
- 매 커밋 전: `gofmt -l` 빈 출력, `go vet ./...`, `go test -count=1 ./...` 전 패키지 통과, skip 0, `-migrate reset` → `-migrate up`.

## 6. 남은 것

- **iOS sync 적용기는 설계 2 §7.1 규칙 (a)(b)와 bootstrap 하한 항목을 구현해야 한다.** 이 테스트 모델(`party_sync_test.go` `client.apply`)이 기준 구현이다.
- calculationVersion에서 membership·party version을 빼는 개선(4-2, 보고만).
- bootstrap 하한 쿼리(`syncfeed/bootstrap.go`) 성능: watermark 뒤 upsert 집합을 먼저 구해 tombstone과 JOIN하도록 바꿨지만 인덱스는 `(recipient_user_id, txid, ordinal)`뿐이다. tombstone이 많은 사용자(잦은 해산·hidden 전환)에서 EXPLAIN으로 계획을 확인하고 필요하면 `(recipient_user_id, entity_type, entity_id)` index를 검토한다.
- 재검증 Low 반영: `projectionBatch`로 생성·이름 수정·가입·위임·탈퇴의 party·멤버십·설정 upsert를 모아 entity당 한 번 읽고 `emitScoped`(scope 조회)를 트랜잭션 단계당 한 번 부른다. 위임 초대 tombstone도 한 번에 보낸다. 해산은 수신자별로 끊어 보내므로 멤버 수(최대 정원)만큼이다. `emitScoped`는 호출자 슬라이스를 복사해 고친다. 삭제 이름 전파 테스트는 worker 역할 pool로 돈다. 남은 참고: 정상 탈퇴에서 본인 멤버십 tombstone을 먼저 보내는 순서는 규칙 (b)가 scope 멤버십을 제외하므로 결과에 영향이 없다(순서 고정 테스트는 없다).
- `proposal` entity 발행(T5·T7 tombstone 포함) → 설계 7.
- P8 계정 삭제: 강제 위임·해산(익명화 이름 전파는 이번에 했다).
- 초대 만료 job(3-②의 tombstone 계약).
- iOS: sync 적용기(version·하한 규칙, 부모 없는 entity 보관, 만료 초대 숨김), `GET /v1/parties` 목록 화면, cursor 만료 시 scope 교체.
- OpenAPI 응답 자동 대조(§15), `PUT /v1/calendar/connection` 요청 schema.
