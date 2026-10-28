# 가능 시간 검색 A1: 활동 시간 저장·API (기능 구현 완료)

- 브랜치: `feature/availability-search`
- 근거: `docs/availability_search_design.md` §3, §7.1, §11, §12, §13 활동 시간 / `.omc/plans/availability-search-implementation.md` A1

## 구현한 것

| 위치 | 내용 |
|---|---|
| `server/migrations/00016_availability_preferences.sql` | `availability_preferences`(user PK, status, time_zone, version), `availability_weekly_intervals`(user·weekday·position PK, 분 CHECK), 겹침·맞닿음 거부 지연 constraint trigger |
| `server/internal/availability/preferences.go` | `GET`/`PUT /v1/availability/preferences/me`, 정규화, `ValidTimeZone`, 본인 sync 무효화, A3용 `LoadPreferences` |
| `server/internal/availability/zones_gen.go`, `tzgen/` | 내장 tzdata에서 만든 지역 시간대 허용 목록과 생성기(`go generate ./internal/availability`) |
| `server/internal/platform/ratelimit/limits.go` | 활동 시간 조회 60/분, 교체 10/분(사용자당, 잠정). 검색 한도(사용자·Party 20/분, 사용자 60/분, §7.2)는 상수만 미리 두고 연결하지 않았다 |
| `server/cmd/api/main.go` | handler 등록 |
| `server/openapi/openapi.yaml` | 두 endpoint와 `AvailabilityPreferences*`, `AvailabilityInterval*` schema |

## 확정한 결정과 설계 편차

1. **btree_gist exclusion constraint 대신 지연 constraint trigger.** migration 역할에 database CREATE가 없어 `CREATE EXTENSION`을 못 한다. trigger(`availability_weekly_intervals_assert_no_overlap`, 함수 `availability_assert_no_interval_overlap`)는 INSERT·UPDATE에만 걸려 커밋 시점에 같은 사용자·요일의 **겹침과 맞닿음 둘 다** 거부한다(`availability_weekly_intervals_no_overlap_check`, 23514). service가 항상 병합하므로 "저장된 구간은 병합까지 끝난 정규형"을 DB 불변식으로 삼았다. DELETE에는 걸지 않아 사용자 삭제 cascade를 막지 않는다. trigger는 검사 전에 소유자 preference row를 `FOR NO KEY UPDATE`로 잡아 service 밖 동시 쓰기도 직렬화한다(PUT은 트랜잭션 앞에서 같은 row를 먼저 잡는다).
2. **미설정 = row 없음 = version 0.** GET은 row를 만들지 않고 `status=unconfigured, version=0, time_zone=null, weekly_intervals=[], updated_at=null`, ETag 없음을 돌려준다. `status` 열의 `unconfigured` 값은 향후 초기화를 위해 남겼고 지금 API는 configured row만 만든다.
3. **expected version 규약은 calendar `PUT /v1/calendar/connection`과 같다.** If-Match 없음 = create-only(`INSERT … ON CONFLICT DO NOTHING` 후 row 잠금, 이미 있으면 409 `version_conflict`), If-Match 있음 = 그 version일 때만 교체(설정이 없으면 409, current 0). `mutation.ExpectedVersion`이 1 이상만 받기 때문에 `"0"`을 새로 허용하지 않았다. 응답은 생성·교체 모두 200. If-Match는 request hash에 들어가지 않는다(기존 mutation helper 규약: 같은 키는 같은 의도).
4. **자정 횡단 입력은 `end_minute > 1440`(최대 2850 = 시작 1410 + 24시간).** `end < start` 방식은 22:00→00:00(end=0)이 길이 0 머리를 만들고 start=end가 모호해 택하지 않았다. 일요일(7)의 꼬리는 월요일(1)로 간다. 응답은 저장 정규형이라 횡단 입력은 두 구간으로 돌아온다.
5. **정규화 순서:** 입력 검증(weekday 1~7, 시작 0~1410, 30분 배수, 길이 30~1440분, 입력 336개 이하) → 자정 분할 → 요일별 정렬 후 겹침(중복 포함) 거부 → 맞닿음 병합 → 요일별 3개 초과·주 전체 0개 거부 → 시작 순 position 0..n-1. 정규형이 현재 설정과 같으면 version을 올리지 않는다(검색 cache key를 불필요하게 바꾸지 않는다).
6. **time zone 규칙(`ValidTimeZone`), 서버 환경과 무관:** 64자 이하 → 정규식 `^[A-Z][A-Za-z_]+(/[A-Za-z0-9_+\-]+){1,2}$` → `Etc/`·`posix/`·`right/`·`SystemV/` 접두 거부(대소문자 무시) → 내장 tzdata에서 생성한 목록 `regionZones`에 대소문자까지 정확히 존재 → `time.LoadLocation` 성공. OS zoneinfo를 거치는 결과(macOS 대소문자 무시 FS의 `asia/seoul`, Linux의 `Asia//Seoul`)에 기대지 않는다. 위반은 전용 code **`time_zone_invalid`(400)**. UTC·GMT 거부 방침(설계 §3.3)은 유지한다. 목록은 Go toolchain의 `lib/time/zoneinfo.zip`에서 만들며 Go를 올리면 다시 생성한다(`TestRegionZonesAreLoadable`이 어긋남을 잡는다).
7. **sync 범위:** 본인에게만 `availability_preference` 변경 하나(entity_id = user_id, payload는 `status`, `version`, `updated_at`)를 보낸다. interval과 time_zone은 본인 sync에도 싣지 않는다. 클라이언트는 무효화 신호로 받고 GET으로 다시 읽는다. bootstrap에는 넣지 않았다(클라이언트는 시작 시 GET한다).
8. **요청 제한:** `Require` 다음, `mutation.Prepare`·검증 앞에서 센다. 조회는 `ModeOpen`(제한 장치 장애로 설정 화면이 막히지 않게), 교체는 `ModeClosed`.
9. **조회 일관성:** preference와 interval을 한 문장(상관 subquery `json_agg`)으로 읽어 두 시점이 섞이지 않는다. `LoadPreferences(ctx, q Querier, userIDs) map[uuid.UUID]Preference`는 요청한 모든 사용자를 돌려주고(row 없으면 unconfigured·version 0·빈 slice), `Preference.Intervals`는 `[]engine.WeeklyInterval`이라 A3가 REPEATABLE READ 트랜잭션 안에서 여러 멤버를 읽어 엔진에 그대로 넘긴다.
10. **로그:** 오류 로그는 `error_type`(SQLSTATE·Go 타입)만 남긴다. interval·time zone 값은 로그·오류 메시지에 없다.

## 테스트 (`server/internal/availability/preferences_test.go`, 모두 DB 통합 + 순수 단위)

- 미설정 GET(row 미생성, ETag 없음, 빈 배열) → create-only PUT → GET
- 전체 교체, 낡은 If-Match 409, create-only 덮어쓰기 409, 설정 없는데 If-Match 409, 같은 정규형 재저장 시 version·sync 불변, time zone만 바꿔도 version 증가
- 멱등 재전송(같은 결과, `Idempotent-Replayed`, sync 1건) / 같은 키 다른 본문 409 `idempotency_mismatch`
- 입력 검증 17종(`invalid_request`): 30분 경계, 30분 미만, start>end, 음수, 시작 1440, 24시간 초과, 요일 범위, 겹침, 중복, 자정 꼬리 겹침, 병합 후 4개, 자정 머리로 4개, 빈 주, 필드 누락, 모르는 필드 / time zone 11종(`time_zone_invalid`): 누락, 고정 offset, `Etc/GMT-9`, `etc/UTC`, UTC, GMT, Local, `asia/seoul`, `ASIA/SEOUL`, `Asia//Seoul`, 없는 이름 / 경계값 0→1440·1410→2850 허용 / 요일당 3개 허용 / Idempotency-Key 누락, 약한 ETag
- 내장 시간대 목록 전체가 규칙 통과·로딩, `LoadPreferences`를 REPEATABLE READ에서 3명(설정 2, 미설정 1) 한 번에 읽기
- 자정 분할(월→화, 일→월), 맞닿음 병합(분할 꼬리와 다음 요일 구간 포함), position 순서
- 정규화 단위: 일요일 꼬리 병합, 정확히 24시간 횡단, 입력 상한, 30분 칸 7×48 전체 병합
- DB trigger: 서비스 밖 두 트랜잭션의 동시 겹침 쓰기에서 뒤 트랜잭션이 잠금을 기다린 뒤 거부됨(잠금을 뺀 함수로 바꾸면 실패함을 확인), 직접 INSERT의 겹침·맞닿음 거부, UPDATE로 만든 겹침 거부, 떨어진 구간 허용, row CHECK(30분 배수, position, configured의 time_zone), 사용자 삭제 cascade
- 동시성: create-only 8건 경쟁과 같은 If-Match 8건 경쟁에서 각각 정확히 1건만 성공(나머지 409), `mutation.Retries()` 불변 / create-only 4건과 If-Match "1" 4건 혼합 경쟁에서 create 1건만 승리, If-Match 승리 ≤1, 최종 version = 성공 수
- 본인 `GET /v1/sync`(bootstrap watermark부터)로 `availability_preference` upsert(version 1, payload는 status·version·updated_at 3개뿐) 전달
- 개인정보: 다른 사용자 GET은 자기 미설정, 다른 사용자 경로 없음, 다른 사용자 sync_changes·bootstrap에 활동 시간 없음, 본인 sync payload에 interval·time zone 없음, 로그에 값 없음
- 요청 제한: 교체 한도 초과 시 잘못된 본문도 400이 아니라 429(검증 앞에서 셈), 조회는 별도 scope로 60회 뒤 429

검증: `gofmt -l .` 빈 출력, `go vet ./...` 통과, `go test -count=1 ./...` 전 패키지 통과(skip 0), `-migrate reset` → `-migrate up` 통과.

## 리뷰 반영 (2026-10-28, High 0)

| 등급 | 지적 | 반영 |
|---|---|---|
| M | tz 검증이 OS zoneinfo에 따라 `etc/UTC`·`asia/seoul`·`Asia//Seoul`을 통과시킴 | 정규식 + 대소문자 무시 접두 거부 + 내장 tzdata 목록 정확 일치(결정 6) |
| M | tz 오류가 일반 400과 구분되지 않음 | `time_zone_invalid` code, OpenAPI 400 설명 |
| M | 트랜잭션 밖 두 SELECT가 서로 다른 시점을 섞음 | 한 문장 조회 + export `LoadPreferences`(결정 9) |
| L | trigger가 서비스 밖 동시 쓰기를 막지 못함 | trigger 첫 문장에서 preference row `FOR NO KEY UPDATE`(00016 수정, main 미병합) |
| L | OpenAPI 최대값·헤더·오류 누락 | end_minute 최대 2850, GET·PUT 200 `ETag`, 423·500(`Internal` response 추가) |
| L | 테스트 공백 | 혼합 경쟁, 경계값, 본인 sync 전달, 조회 rate limit, 트리거 동시성 |

## iOS 계약 (A5)

- 첫 저장은 If-Match 없이(create-only), 이후는 GET의 ETag를 If-Match로 보낸다.
- **create-only를 재전송할 때는 같은 Idempotency-Key를 쓴다.** 새 key로 보내면 첫 요청이 이미 커밋된 경우 409 `version_conflict`가 된다. 409를 받으면 GET으로 현재 상태를 확인하고 사용자에게 보여준다.
- **기기 time zone identifier가 `Area/City`가 아니면(예: `GMT`) 도시 선택 UI를 띄운다.** 서버는 그 값을 `time_zone_invalid`로 거부한다.
- `availability_preference` sync 변경은 무효화 신호다. 받으면 GET으로 다시 읽는다.

## 남은 것

- **[P8 차단 항목] 계정 삭제와 활동 시간:** FK는 users ON DELETE CASCADE지만 삭제 파이프라인이 `users` row를 물리 삭제하지 않아 cascade가 일어나지 않는다. 삭제된 사용자의 활동 시간이 남는다(캘린더 데이터도 같은 공백). P8에서 삭제 worker가 `availability_preferences`를 지우게 해야 한다.
- **A2 엔진과의 계약은 맞다(확인함):** `engine.WeeklyInterval`은 ISO 요일 1~7, `0 ≤ Start < End ≤ 1440`인 이 정규형을 그대로 받고, UTC 확장 뒤 자정 분할 구간(day d `[x,1440)` + day d+1 `[0,y)`)을 병합한다. A3는 저장 row를 변환 없이 넘기면 된다.
- A3: REPEATABLE READ 트랜잭션에서 `availability.LoadPreferences`로 멤버를 한 번에 읽고, `Version`(미설정 0)을 version tuple에, `Intervals`를 `engine.Member.Weekly`에, `TimeZone`을 `time.LoadLocation`으로 `Location`에 넣는다.
