# P6-lite: Party sync 전파 감사·보강과 내 Party 목록 (기능 구현·검증 완료)

- 일시: 2026-10-28 16:13
- 브랜치: `feature/party-sync`
- 근거: `docs/party_membership_design.md` §5.5·§9.2, `docs/party_visibility_design.md`, `docs/account_backend_design.md` §7, `.omc/plans/party-membership-implementation.md` P6, `docs/project_goal.md` 체크리스트 2

## 1. 감사 매트릭스

entity: P=`party`, M=`party_membership`, I=`party_invite`, V=`party_visibility_setting`, S=`party_schedule_projection`.
모든 발행은 상태 전이와 같은 mutation 트랜잭션 안이다. "v"는 version 있는 tombstone, "∅"는 version 없는 tombstone.

| 경로 | 수신자와 변경 | 설계 대비 |
|---|---|---|
| T1 생성 | 생성자: P·M·V upsert, 본인 S upsert | 일치. **순서 수정**(아래 2-②) |
| Party 이름 수정 | 활성 멤버 전원: P upsert | 일치 |
| T2 초대 생성 | 현재 방장: I upsert(token 없음) | 일치 |
| 초대 무효화 | 현재 방장: I ∅ | 일치 |
| T3 수락 | 신규 멤버: P, 모든 활성 M, 본인 V, 다른 멤버 S(Party 단위 부트스트랩). 기존 멤버: P, 신규 M. 전원: 신규 멤버 S. 방장: I upsert | **소진 시 I upsert → I ∅로 수정**(2-①), 순서 수정 |
| 초대 만료 | 발행 없음(만료 scheduler가 없다. status는 active로 남는다) | 편차 3-② |
| T4 위임 | 활성 멤버 전원: P·옛 방장 M·새 방장 M upsert. 이전·새 방장에게만: 무효화한 I ∅ | 일치 |
| T5 탈퇴 / T6 강퇴 | 떠난 사람: 본인 M v, P ∅, 다른 M ∅, 다른 멤버 S ∅, 본인 S v, 본인 V v. 남은 멤버: 떠난 M v, 떠난 사람 S v, P upsert | 일치(10-14 rec의 tombstone version 규칙) |
| 단독 방장 탈퇴 → T7 | T7과 같음 | 일치 |
| T7 해산 | 마지막 활성 멤버 전원: P v, 모든 M v, 모든 S v, 본인 V v. 방장만: 활성 I ∅ | 일치 |
| 공개 수준 변경 | 본인: V upsert. 활성 멤버 전원: 본인 S upsert·v(재구성 결과) | 일치(details ciphertext는 sync에 없다) |
| 캘린더 snapshot complete·abort·연결 PUT | `RebuildBusyProjections*`가 활성 멤버 전원에게 S upsert·v | 일치(`active_generation_usable` 판정 포함) |
| 표시 이름 변경(PATCH /v1/me) | 본인: user upsert. **각 Party 활성 멤버 전원: 본인 M upsert(새 version)** | **신설**(2-③) |
| 계정 삭제 | Party 발행 없음 | P8 범위(남은 것) |
| 제안 | 없음 | 설계 7 범위 |

bootstrap(`party.BootstrapProjections`)은 활성 Party마다 P, 활성 M 전부, 본인 V, 방장이면 status=active I, Party의 S 전부를
같은 투영 함수로 싣는다. S는 비노출 상태면 이미 지워져 있으므로(가시성은 projection 존재로 표현) 추가 필터가 필요 없다.

## 2. 메운 공백

1. **소진 초대(`2de0e87`):** 수락으로 `exhausted`가 되면 방장에게 status=exhausted upsert를 보냈는데 bootstrap은 활성 초대만 싣는다. 방장 모델에 소진 초대가 영구히 남았다. 소진이면 I ∅를 보낸다. 수정을 되돌리면 인수 테스트가 `[accept B] A1 남음 party_invite … status:exhausted`로 실패함을 확인했다.
2. **부모 먼저(`97fbf97`):** T1·T3에서 S upsert가 같은 트랜잭션 안에서 P·M보다 먼저 기록됐다. T1은 P·M·V 발행 뒤 projection을 만들고, T3는 `emitJoin` 뒤로 projection 생성을 옮겼다(calendar connection 잠금이 가장 마지막이 되는 부수 효과). 인수 테스트 모델이 "P보다 먼저 온 하위 upsert"를 실패로 보고, 옛 코드에서 실패함을 확인했다. **한 트랜잭션 안의 순서만 보장한다**(4절).
3. **표시 이름(`49d0456`, lead 미응답으로 제안 A안 적용):** M 투영에 `display_name`을 더했다(bootstrap·증분 공통 함수). `party.PropagateDisplayName`이 `LockUserPartiesShared`(parties FOR SHARE, id 순) 뒤 본인 활성 멤버십 version을 올리고 Party별 활성 멤버 전원에게 M upsert를 보낸다. 잠금은 users(이미 FOR NO KEY UPDATE) → parties → 멤버십이라 T3(users → parties FOR UPDATE)·탈퇴(parties FOR UPDATE → 멤버십)와 어긋나지 않는다. Party 잠금 없이는 동시 가입자가 옛 이름만 받을 수 있다.
   - 부수 효과: 멤버십 version이 가능 시간 calculationVersion tuple에 있어 이름 변경이 검색 결과(최대 5분)를 stale로 만든다. 허용했다.

## 3. 설계 편차·계약

1. **`GET /v1/parties`(`83eab1d`):** 설계 §9.1 표에 없어 응답을 정했다. `{request_id, parties: [{party, membership(호출자 본인), member_count(활성)}]}`, 호출자 가입 시각 순, 페이지 없음(참여 한도 20). 활성 멤버십 + 활성 Party만(bootstrap scope와 같다). `Require` → `ratelimit.Enforce`(`party.list`, 사용자당 60/분, **ModeOpen**: 본인 데이터 읽기라 장애 시 첫 화면이 막히지 않게) → 조회. `Cache-Control: no-store`.
2. **초대 만료:** 만료 scheduler가 없어 status=active로 남고 bootstrap도 그대로 싣는다(목록 API만 `expires_at > now`로 거른다). 증분과 bootstrap은 일치한다. **iOS 계약:** `expires_at`이 지난 초대는 숨긴다. **향후 만료 job은 방장에게 I tombstone을 같은 트랜잭션에서 보내야 한다.**
3. 신규 멤버 부트스트랩은 별도 메커니즘 없이 T3의 증분 row로 보낸다(설계 §9.2 그대로). T3 txid는 신규 멤버의 기존 cursor보다 크다.

## 4. 발견한 결함(미해결, lead 결정 대기)

**version 없는 tombstone이 동시 실행에서 stale upsert에 덮인다.** `mutation.Run`의 idempotency INSERT가 첫 쓰기라 txid를 Party 잠금 전에 받는다. 피드는 txid 순, 직렬화는 잠금 순이라 어긋날 수 있다. version 있는 변경은 version 비교로 무해하지만 탈퇴자 scope 정리용 ∅(P, 다른 M, 다른 S)와 초대 ∅는 뒤에 오는(더 큰 txid) 옛 upsert를 막지 못한다.

- 결정적 재현: 탈퇴 L이 claim 뒤 잠금 직전 멈춘 사이 방장 이름 변경 E가 잠금·커밋 → 탈퇴자 피드 마지막 P 변경이 upsert, bootstrap 0건. 같은 유형: 위임 vs 동시 수락(옛 방장 모델에 초대 부활), 같은 사용자의 탈퇴 vs 재가입(재가입 뒤 모델이 빔).
- 서버 잠금 순서만으로는 못 고친다. 행 잠금이 곧 xid 배정이고 users → parties 두 행을 잡는 경로에서 다시 어긋난다.
- 제안한 선택지: (1) 초대 tombstone을 version 있게 + Party 범위 변경에 수신자 당시 멤버십 ID(scope) 추가와 iOS 규칙, (2) 초대만 고치고 나머지는 계약 공백으로 기록, (3) 커밋 순서 키(비추천). 결정 결과는 이 절에 덧붙인다.
- 같은 이유로 **한 트랜잭션 밖의 순서는 보장되지 않는다.** 동시 실행에서는 하위 entity upsert가 그 P보다 먼저 올 수 있다. iOS는 부모 없는 entity를 버리지 말고 보관한다.

## 5. 테스트

- `server/internal/syncfeed/party_sync_test.go`(실제 HTTP: auth Require → account·party·sync handler, 캘린더는 seed)
  - `TestPartySyncIncrementalMatchesBootstrap`: 3명, 세션 5개(A 두 기기 중 하나는 limit=2 페이지로 몇 단계씩 건너 읽음, B 두 번째 기기는 중간에 처음 bootstrap). 생성 → 1회용 초대 → 수락(소진) → 공개 수준 hidden → busyOnly(versioned tombstone 뒤 새 ID) → 표시 이름 변경 → 이름 변경·초대 2개·무효화 → 위임 → 새 방장 초대 → C 가입 → C 탈퇴 → C 재가입(∅가 하한을 남기지 않음) → 강퇴 → 해산. 단계마다 각 세션이 자기 cursor로 끝까지 읽어 iOS 계약대로 적용한 모델이 새 bootstrap과 같은지(Party 5종, payload는 JSON decode 비교) 10초 수렴 루프로 확인한다. 같은 방식으로 틀려도 통과하지 않게 단계마다 type별 절대 개수도 본다(신규 멤버 부트스트랩, 떠난 멤버 0건, C의 다른 Party Q 유지, 초대는 방장만). `mutation.Retries()` 증가 0.
  - `TestDisplayNameRacingAcceptConverges`: 이름 변경과 같은 Party 가입을 5회 동시 실행. 가입 전에 bootstrap한 가입자 모델이 증분만으로 최종 이름에 수렴, 재시도 0.
  - 돌연변이 확인: 소진 수정 제거, `emitPartyProjectionsTo` 제거, 생성 순서 원복 각각에서 실패.
- `server/internal/party/list_test.go`: 활성만·탈퇴·해산 제외·본인 멤버십·멤버 수·비멤버 빈 배열·미인증 401. `ratelimit_test.go` `TestListPartiesLimit`: 61번째 429.
- OpenAPI(`1b0a6eb`): 등록된 route 전부 등재(`GET·POST /v1/parties`, Party 상세·수정, 멤버 목록, 위임, 초대 생성·목록·무효화·미리보기·수락, `GET /v1/calendar/connection`, snapshot abort). Party·PartyMembership schema 공유, 오류 code 9개 추가. ruby YAML 파싱과 `$ref` 전수 해석 확인.
- 매 커밋 전: `gofmt -l` 빈 출력, `go vet ./...`, `go test -count=1 ./...` 전 패키지 통과, skip 0, `-migrate reset` → `-migrate up`.

## 6. 남은 것

- 4절 결정과 그 구현.
- `proposal` entity 발행(T5·T7 tombstone 포함) → 설계 7.
- P8 계정 삭제: 강제 위임·해산과 함께 익명화된 표시 이름(`탈퇴한 사용자`)도 `PropagateDisplayName`으로 전파해야 한다.
- 초대 만료 job(3-②의 tombstone 계약).
- iOS: sync 적용기(version·하한 규칙, 부모 없는 entity 보관, 만료 초대 숨김), `GET /v1/parties` 목록 화면, cursor 만료 시 scope 교체.
- OpenAPI 응답 자동 대조(§15), `PUT /v1/calendar/connection` 요청 schema.
