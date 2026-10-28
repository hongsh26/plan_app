# 가능 시간 검색 A3·A4: snapshot·freshness와 Party 검색 API·캐시 (기능 구현 완료)

- 일시: 2026-10-28 14:31
- 브랜치: `feature/availability-search`
- 근거: `docs/availability_search_design.md` §2, §4, §5.1, §6, §7.2, §9, §10, §12, §13 / `.omc/plans/availability-search-implementation.md` A3, A4
- 선행: A1 `2026-10-28_1404_availability_preferences.md`, A2 `2026-10-28_1401_availability_engine.md`

## 구현한 것

| 위치 | 내용 |
|---|---|
| `server/internal/availability/search_snapshot.go` (A3) | REPEATABLE READ READ ONLY snapshot, 준비 gate와 `self` 상태, window 교집합 검사, `fresh_until`, version tuple 인코딩, `Versioner`(키 파생·token·`VerifyCalculationVersion`), serialization 재시도 |
| `server/internal/availability/search.go` (A4) | `POST /v1/parties/{party_id}/availability/search`, 입력 기본값·검증, 요청 제한, 3초 기한, 응답 직렬화, 프로세스 메모리 LRU 캐시, 지표 로그 |
| `server/internal/availability/preferences.go` | `Deps.VersionSecret`, `NewHandler`가 `(*Handler, error)`를 돌려준다(빈 secret 거부), route 등록, `Handler.Versioner()` |
| `server/cmd/api/main.go` | `VersionSecret: rateSettings.Key`(RATE_LIMIT_KEY, local은 config 고정 기본 키) |
| `server/openapi/openapi.yaml` | 검색 endpoint와 `AvailabilitySearchRequest/Response`, `AvailabilitySlot`, `AvailabilityPendingError`, `AvailabilitySelfState`, ErrorResponse code enum 추가(`time_zone_invalid` 포함) |

## 처리 순서

`Require` → `ratelimit.Enforce`(ModeClosed, 사용자·Party 20/분 + 사용자 60/분을 한 호출에, 초과 시 `RecordExceeded`) → party_id 파싱(실패는 404) → 본문 decode(모르는 필드 거부) → 기본값·검증 → 3초 context → snapshot tx → 캐시 조회 → (miss) 엔진 → 응답.

snapshot 판정 순서: Party·호출자 멤버십(404) → 활동 시간 미설정(409 setup) → 캘린더 비정상·stale·window 없음(409 sync) → envelope 포함(400 range) → facts 읽기.

## 확정한 결정과 설계·지시 편차

1. **JSON은 snake_case다.** 설계 §7.2 예시는 camelCase지만 A1과 기존 API 전체가 snake_case라 맞췄다: 요청 `start_date`, `end_date`, `duration_minutes`, `time_zone`, `max_results` / 응답 `slots[].start_at·end_at`, `calculation_version`, `generated_at`, `valid_until`, 그리고 다른 endpoint처럼 `request_id`. camelCase 키(`timeZone`)는 모르는 필드로 400이다.
2. **`time_zone`은 필수다.** 서버는 기기 시간대를 모른다. 검증은 A1 `ValidTimeZone`, 실패 code는 `time_zone_invalid`.
3. **입력 한도:** 과거 시작일, end < start, 날짜 형식 오류, 30~480분이 아니거나 30분 배수가 아닌 길이, 1~20이 아닌 max_results → 400 `invalid_request`. 시작일 포함 31일 초과, end_date > 오늘+90일 → 400 `search_range_invalid`. 오늘은 요청 time_zone 기준이다.
4. **setup이 calendar보다 우선한다.** 둘 다 미준비면 `availability_setup_pending`. 활동 시간은 앱 안에서 바로 고치는 앞 단계다(§3.1). `self`는 항상 호출자 본인의 `activity`(configured/unconfigured)와 `calendar`를 함께 준다. 본인은 준비됐는데 409면 "그룹 준비를 기다리는 중"이다. 다른 멤버의 수·ID·원인은 어디에도 없다.
5. **self.calendar 값:** `ready`, `not_connected`(연결 없음·disconnected), `denied`(permission_required·denied·restricted), `revoked`, `syncing`(selecting·syncing), `source_required`(needs_source_reselection), `error`, `stale`.
6. **stale 판정:** `last_completed_sync_at + 6h ≤ now`이면 stale(`calendar.FreshnessWindow`, `FreshUsers`와 같은 경계). now는 handler 시계(`Deps.Now`)이며 DB `now()`를 쓰지 않는다.
7. **active generation window는 그 generation을 만든 completed `calendar_snapshot_sessions` row에서 읽는다.** session은 `calendar.Prune`가 created_at 기준 24시간 뒤 지운다. 그래서 session이 18시간 넘게 열려 있다가 완료되면 연결은 fresh인데 window row가 없을 수 있다. **이때는 fail closed로 `calendar_sync_pending`(본인은 `stale`)을 준다.** 400 range가 아닌 이유는 날짜를 바꿔도 고쳐지지 않고 재동기화만 고치기 때문이다. migration으로 window를 연결에 복사하는 방법은 범위 밖이라 택하지 않았다(후속 후보).
8. **envelope 포함(설계 §4 문구 편차):** 모든 멤버 window `[window_start, window_end)`의 교집합이 `[max(envelope 시작, now), envelope 끝)`을 덮어야 한다. 경계가 정확히 같으면 포함이다. 실패하면 멤버 정보 없이 400 `search_range_invalid`. **하한을 envelope 시작 대신 max(envelope 시작, now)로 둔 이유:** 멤버 window는 그 멤버 시간대의 오늘 00:00부터라(설계 3 §6) 요청 시간대가 더 서쪽이면 "오늘" 검색의 envelope 시작(요청 시간대 오늘 00:00)보다 늦다. 문구대로면 서로 다른 시간대 Party의 기본 검색이 하루 대부분 400이 된다(재현 테스트로 확인). `[envelope 시작, now)`는 이미 지나 lead time 때문에 후보가 없고, now 이후 슬롯과 겹치는 일정은 window 경계를 가로질러도 업로드되므로(설계 3 §6.1) "누락 구간을 free로 해석하지 않는다"는 불변식은 유지된다. 끝 쪽은 그대로다(짧은 window_end는 실제 누락).
9. **facts:** `(connection_id, active_generation)` 쌍으로 거르고 envelope과 겹치는 것만 읽는다. availability 값 busy·tentative·unavailable은 모두 Busy다.
10. **version tuple:** `party_id, parties.version, (user_id, membership_id, membership.version)*, (user_id, preference version)*, (user_id, connection_id, active_generation)*, fresh_until(µs), start_date, end_date, duration, time_zone, max_results`를 길이 접두 인코딩 하나로 묶는다(user_id 오름차순). 세 hash를 따로 만들지 않고 HMAC 하나에 넣는다. 공개 수준과 호출자 ID는 tuple에 없다.
11. **calculationVersion 형식(지시 편차):** 지시는 "tuple의 HMAC"이었지만 그러면 Verify가 서버 상태 없이 `validUntil`을 알 수 없고, 클라이언트가 준 validUntil은 위조할 수 있다. 그래서 token = `base64url(0x01 ‖ validUntil Unix초 8바이트 ‖ HMAC-SHA256(key, canonical ‖ validUntil))`(41바이트)로 했다. 내부 ID·version은 HMAC 안에만 있어 복원할 수 없다. 같은 입력이라도 새로 계산할 때마다 validUntil이 달라 token이 다르다(캐시 hit이면 같은 token).
12. **키:** `hkdf.Key(sha256.New, RATE_LIMIT_KEY 바이트, salt 없음, "plantogether/availability-calculation-version/v1", 32)`. local은 `config.LoadRateLimit`의 고정 기본 키를 그대로 쓴다. **RATE_LIMIT_KEY를 교체하면 남은 version이 모두 stale이 된다.** 유효기간이 최대 5분이라 허용한다. 빈 secret이면 `NewHandler`가 오류다(인스턴스마다 임의 키가 생기면 다른 인스턴스의 version을 검증하지 못한다).
13. **validUntil** = `min(generatedAt + 5분, fresh_until)`, 둘 다 초 단위로 내림. 내림 결과가 generatedAt 이하이면(fresh_until이 1초 안에 끝남) 결과를 주지 않고 409 `availability_changed_retry`다.
14. **캐시(지시 편차):** key = `(party_id, sha256(canonical tuple), engine.EarliestStart)`. 지시의 `(party, calculationVersion)`은 결정 11 때문에 쓸 수 없다. EarliestStart를 넣은 이유는 lead time(현재+30분) 경계를 지난 뒤 캐시가 이미 30분 안으로 들어온 슬롯을 돌려주지 않게 하기 위해서다. 값은 슬롯·token·generatedAt·validUntil이고 hit이면 그대로 돌려준다(generatedAt은 계산 시각). 항목은 자기 validUntil까지만 쓰고(`now ≥ validUntil`이면 제거), 최대 1024개 LRU다.
15. **캐시 trade-off:** snapshot은 매 요청 읽는다. 캐시는 엔진 계산만 아낀다. 그래서 correctness는 캐시에 의존하지 않지만 DB 비용은 줄지 않는다. 엔진이 31일·20명에서 약 1ms라 이 trade-off를 택했다.
16. **기한 3초:** snapshot과 계산 전체가 3초 context 안에 있다. 넘기면 503 `availability_temporarily_unavailable` + `Retry-After: 1`이고 부분 결과는 없다. 엔진은 goroutine에서 돌고 기한이 지나면 결과를 버린다.
17. **serialization 재시도는 방어용이다.** READ ONLY REPEATABLE READ 트랜잭션은 사실상 40001을 내지 않아 DB에서 재현하지 않았다. `withSerializationRetry`를 주입한 오류로 단위 테스트했다(1회 재시도, 두 번째도 40001이면 409 `availability_changed_retry`).
18. **404:** 없는 Party, 해산된 Party, 비멤버, 떠난 멤버, 잘못된 UUID 모두 같은 code·message다. 해산된 Party도 visibility API의 `party_disbanded`와 달리 404다(지시: 존재 은닉).
19. **로그(§12):** 검색마다 한 줄 `action=availability.search`, `result_code`, `duration_bucket`(30/60/90-120/150+), `range_days_bucket`(1-7/8-14/15-31), `slot_count`, `cache_hit`. 날짜·슬롯 시각·Party·멤버 ID·time zone·version은 남기지 않는다. 내부 오류는 `error_type`만 남긴다.

20. **A1 경쟁 결함 수정(`preferences.go`):** If-Match PUT이 create 커밋 전에 `SELECT … FOR NO KEY UPDATE`를 실행하면 잠글 row가 없어 잠금 없이 지나간다. 그 사이 create가 커밋되면 다음 문장이 version 1을 보고 검사를 통과한 뒤 다른 If-Match 교체를 덮어썼다(lost update). `TestMixedCreateAndIfMatchRace`가 200회 중 13회 실패했다(전체 실행에서 원인 불명 FAIL로 한 번 보였던 것). 잠금 문장이 0 row면 409 `version_conflict`(current 0)로 끝내게 고쳤고 200회 0 실패다.

## 설계 7·iOS 계약

- **설계 7은 제안 생성 경계에서 `Handler.Versioner().VerifyCalculationVersion(ctx, q, partyID, callerID, params, version, now)`을 반드시 부른다.** `ErrResultStale` → 409 `availability_result_stale`, `IsNotFound(err)` → 404. q에 자기 트랜잭션을 넘겨도 된다(facts는 읽지 않는다).
- params는 검색이 기본값을 채운 뒤의 값과 같아야 한다. **클라이언트는 날짜를 명시해서 보내고 제안 생성에도 같은 값을 보낸다.** 날짜를 생략한 검색은 자정을 넘으면 기본 시작일이 바뀌어 같은 조건이 stale이 된다.
- 슬롯은 UTC instant이고 iOS는 약식 시간대와 함께 표시한다(A2 표시 계약).
- 409 `availability_changed_retry`는 그대로 재검색한다. 409 pending은 `self`로 본인 CTA만 띄운다.

## 테스트

`server/internal/availability/search_snapshot_test.go`(A3), `search_test.go`(A4). 모두 DB 통합(실제 Party·초대·가입·탈퇴 API, 활동 시간 PUT API, 캘린더는 직접 seed) + 순수 단위.

- A3: 재시도 wrapper(성공, 1회 후 성공, 2회 실패, 다른 오류 비재시도), 키 파생·token 형식, version 검증(변화 없음·다른 멤버 검증 nil / validUntil 경과·파라미터 변경·형식 오류·변조·활동 시간 변경·generation 변경·가장 이른 last sync 변경·party version 변경·가입·탈퇴·calendar freshness 만료 → stale / 떠난 멤버 → not found), active generation facts만·envelope과 겹치는 facts만 읽기, 비멤버·없는 Party 동일 오류
- A4: 서울·뉴욕·런던 3명 ready 결과가 같은 입력의 엔진 결과와 정확히 같고 활동 시간·Busy·날짜당 3개·validUntil 규칙을 지킴, 응답 version 검증 통과 / 빈 결과 `"slots":[]` / setup·sync pending의 self(미설정, 연결 없음, 상태 8종, window row 정리, 시간 경과 stale, setup 우선)와 본문 key 고정·다른 멤버 ID 없음 / window 미포함 400, 끝 경계 포함, 시작 1초 늦으면 미포함 / 404 5종(비멤버·없는 Party·떠난 멤버·잘못된 UUID·해산된 Party) 동일 / 입력 검증 표(정상 6, 오류 22: tz 4, 과거·역순·형식·없는 날짜, 길이 4, max 2, 모르는 필드 3, 비JSON·빈 본문, 범위 3) / 기본값 / 요청 제한(잘못된 본문 20회 뒤 429+Retry-After, 사용자 전체 60회 뒤 다른 Party도 429) / 캐시(hit 시 같은 version·generatedAt, 파라미터·활동 시간·generation·가입 변경 miss, validUntil 경과 miss, LRU 상한) / 자정 횡단(월 22:00~화 02:00 입력이 두 row로 저장되고 23:30~00:30 포함 정확한 슬롯 목록) / 공개 수준 details·busyOnly·hidden 결과 동일(캐시 비움) / 응답에 다른 멤버 ID·member·count·visibility 등 없음, 검색 로그에 슬롯 시각·날짜·tz·Party·멤버 ID·version 없음 / 기한 지난 context의 snapshot 오류가 503으로 매핑 / 오늘 검색에서 멤버 window가 envelope 시작과 now 사이에서 시작하면 200(기본·명시 둘 다), now+2h에서 시작하면 400

검증: `cmd/api` 기동 후 `/livez` 200, 미인증 검색 401(route 등록 확인). `gofmt -l .` 빈 출력, `go vet ./...` 통과, `go test -count=1 ./...` 전 패키지 통과, skip 0, `-migrate reset` → `-migrate up` 통과.

## 남은 것

- **A5 iOS:** 검색 form(기본 14일·60분, 날짜 명시 전송), 결과·준비 대기·빈 결과 상태 분리, `self` CTA, `AvailabilitySlot` count 제거, calculation_version 전달.
- **A6 privacy 스캔:** 로그·trace·분석·APNs 금지 필드 자동 스캔, 결과 만료 → 재검색 → 제안 진입 E2E.
- **설계 7:** 제안 생성에서 `VerifyCalculationVersion` 호출(위 계약).
- **결정 필요: 일상 재동기화가 Party 전체를 막는다.** snapshot complete는 연결이 `syncing`이어야 하므로 멤버가 다시 올릴 때마다 `ready → syncing`이 되고, 그동안 Party 전체가 `calendar_sync_pending`이다. 앱 종료로 업로드가 끊기면 다시 열거나 6시간 stale이 될 때까지 이어질 수 있다. 지금은 지시·`FreshUsers`대로 ready만 인정한다. "이전 generation이 fresh인 syncing을 ready로 볼지"는 lead 결정이다.
- window row 24시간 정리 공백(결정 7)을 없애려면 complete 시 window를 `calendar_connections`에 복사하는 migration을 검토한다.
- 캐시는 인스턴스별이라 다중 인스턴스에서는 hit률만 떨어진다(correctness 영향 없음).
- 계정 삭제(P8)와의 공백은 A1 rec과 같다.
