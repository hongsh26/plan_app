# 가능 시간 검색 A1: 활동 시간 저장·API (기능 구현 완료)

- 브랜치: `feature/availability-search`
- 근거: `docs/availability_search_design.md` §3, §7.1, §11, §12, §13 활동 시간 / `.omc/plans/availability-search-implementation.md` A1

## 구현한 것

| 위치 | 내용 |
|---|---|
| `server/migrations/00016_availability_preferences.sql` | `availability_preferences`(user PK, status, time_zone, version), `availability_weekly_intervals`(user·weekday·position PK, 분 CHECK), 겹침·맞닿음 거부 지연 constraint trigger |
| `server/internal/availability/preferences.go` | `GET`/`PUT /v1/availability/preferences/me`, 정규화, `ValidTimeZone`, 본인 sync 무효화 |
| `server/internal/platform/ratelimit/limits.go` | 활동 시간 조회 60/분, 교체 10/분(사용자당, 잠정). 검색 한도(사용자·Party 20/분, 사용자 60/분, §7.2)는 상수만 미리 두고 연결하지 않았다 |
| `server/cmd/api/main.go` | handler 등록 |
| `server/openapi/openapi.yaml` | 두 endpoint와 `AvailabilityPreferences*`, `AvailabilityInterval*` schema |

## 확정한 결정과 설계 편차

1. **btree_gist exclusion constraint 대신 지연 constraint trigger.** migration 역할에 database CREATE가 없어 `CREATE EXTENSION`을 못 한다. trigger(`availability_weekly_intervals_assert_no_overlap`, 함수 `availability_assert_no_interval_overlap`)는 INSERT·UPDATE에만 걸려 커밋 시점에 같은 사용자·요일의 **겹침과 맞닿음 둘 다** 거부한다(`availability_weekly_intervals_no_overlap_check`, 23514). service가 항상 병합하므로 "저장된 구간은 병합까지 끝난 정규형"을 DB 불변식으로 삼았다. DELETE에는 걸지 않아 사용자 삭제 cascade를 막지 않는다. trigger는 동시 쓰기를 혼자 막지 못하며, PUT이 preference row를 `FOR NO KEY UPDATE`로 잡아 직렬화한다.
2. **미설정 = row 없음 = version 0.** GET은 row를 만들지 않고 `status=unconfigured, version=0, time_zone=null, weekly_intervals=[], updated_at=null`, ETag 없음을 돌려준다. `status` 열의 `unconfigured` 값은 향후 초기화를 위해 남겼고 지금 API는 configured row만 만든다.
3. **expected version 규약은 calendar `PUT /v1/calendar/connection`과 같다.** If-Match 없음 = create-only(`INSERT … ON CONFLICT DO NOTHING` 후 row 잠금, 이미 있으면 409 `version_conflict`), If-Match 있음 = 그 version일 때만 교체(설정이 없으면 409, current 0). `mutation.ExpectedVersion`이 1 이상만 받기 때문에 `"0"`을 새로 허용하지 않았다. 응답은 생성·교체 모두 200.
4. **자정 횡단 입력은 `end_minute > 1440`(최대 2880).** `end < start` 방식은 22:00→00:00(end=0)이 길이 0 머리를 만들고 start=end가 모호해 택하지 않았다. 일요일(7)의 꼬리는 월요일(1)로 간다. 응답은 저장 정규형이라 횡단 입력은 두 구간으로 돌아온다.
5. **정규화 순서:** 입력 검증(weekday 1~7, 시작 0~1410, 30분 배수, 길이 30~1440분, 입력 336개 이하) → 자정 분할 → 요일별 정렬 후 겹침(중복 포함) 거부 → 맞닿음 병합 → 요일별 3개 초과·주 전체 0개 거부 → 시작 순 position 0..n-1. 정규형이 현재 설정과 같으면 version을 올리지 않는다(검색 cache key를 불필요하게 바꾸지 않는다).
6. **time zone 규칙(`ValidTimeZone`):** `time.LoadLocation` 통과, `/` 포함, `Etc/`·`posix/`·`right/`·`SystemV/` 접두 아님, `Local` 아님, 64자 이하. UTC·GMT·EST·EST5EDT·`Etc/GMT-9`·`UTC+09:00`을 거부하고 iOS가 주는 `Area/City`를 받는다. `time/tzdata`를 패키지에 내장했다.
7. **sync 범위:** 본인에게만 `availability_preference` 변경 하나(entity_id = user_id, payload는 `status`, `version`, `updated_at`)를 보낸다. interval과 time_zone은 본인 sync에도 싣지 않는다. 클라이언트는 무효화 신호로 받고 GET으로 다시 읽는다. bootstrap에는 넣지 않았다(클라이언트는 시작 시 GET한다).
8. **요청 제한:** `Require` 다음, `mutation.Prepare`·검증 앞에서 센다. 조회는 `ModeOpen`(제한 장치 장애로 설정 화면이 막히지 않게), 교체는 `ModeClosed`.
9. **로그:** 오류 로그는 `error_type`(SQLSTATE·Go 타입)만 남긴다. interval·time zone 값은 로그·오류 메시지에 없다.

## 테스트 (`server/internal/availability/preferences_test.go`, 모두 DB 통합 + 순수 단위)

- 미설정 GET(row 미생성, ETag 없음, 빈 배열) → create-only PUT → GET
- 전체 교체, 낡은 If-Match 409, create-only 덮어쓰기 409, 설정 없는데 If-Match 409, 같은 정규형 재저장 시 version·sync 불변, time zone만 바꿔도 version 증가
- 멱등 재전송(같은 결과, `Idempotent-Replayed`, sync 1건) / 같은 키 다른 본문 409 `idempotency_mismatch`
- 검증 23종: 30분 경계, 30분 미만, start>end, 음수, 시작 1440, 24시간 초과, 요일 범위, 겹침, 중복, 자정 꼬리 겹침, 병합 후 4개, 자정 머리로 4개, 빈 주, 필드 누락, 모르는 필드, time zone 누락·고정 offset·`Etc/GMT-9`·UTC·Local·없는 이름 / 요일당 3개 허용 / Idempotency-Key 누락, 약한 ETag
- 자정 분할(월→화, 일→월), 맞닿음 병합(분할 꼬리와 다음 요일 구간 포함), position 순서
- 정규화 단위: 일요일 꼬리 병합, 정확히 24시간 횡단, 입력 상한, 30분 칸 7×48 전체 병합
- DB trigger: 직접 INSERT의 겹침·맞닿음 거부, UPDATE로 만든 겹침 거부, 떨어진 구간 허용, row CHECK(30분 배수, position, configured의 time_zone), 사용자 삭제 cascade
- 동시성: create-only 8건 경쟁과 같은 If-Match 8건 경쟁에서 각각 정확히 1건만 성공(나머지 409), `mutation.Retries()` 불변
- 개인정보: 다른 사용자 GET은 자기 미설정, 다른 사용자 경로 없음, 다른 사용자 sync_changes·bootstrap에 활동 시간 없음, 본인 sync payload에 interval·time zone 없음, 로그에 값 없음
- 요청 제한: 한도 초과 시 잘못된 본문도 400이 아니라 429(검증 앞에서 셈), 조회는 별도 scope

검증: `gofmt -l .` 빈 출력, `go vet ./...` 통과, `go test -count=1 ./...` 전 패키지 통과(skip 0), `-migrate reset` → `-migrate up` 통과.

## 남은 것

- **계정 삭제와 활동 시간:** FK는 users ON DELETE CASCADE지만 삭제 파이프라인이 `users` row를 물리 삭제하지 않아 cascade가 일어나지 않는다. 삭제된 사용자의 활동 시간이 남는다(캘린더 데이터도 같은 공백). P8에서 삭제 worker가 `availability_preferences`를 지우게 해야 한다.
- **A2 엔진과의 계약은 맞다(확인함):** `engine.WeeklyInterval`은 ISO 요일 1~7, `0 ≤ Start < End ≤ 1440`인 이 정규형을 그대로 받고, UTC 확장 뒤 자정 분할 구간(day d `[x,1440)` + day d+1 `[0,y)`)을 병합한다. A3는 저장 row를 변환 없이 넘기면 된다.
- A3가 쓸 version tuple에 `availability_preferences.version`(미설정은 0)을 넣는다. 지금 조회 함수는 패키지 내부(`loadPreference`)라 A3에서 필요한 만큼 노출한다.
- iOS(A5): 첫 저장은 If-Match 없이, 이후는 GET의 ETag로. `availability_preference` sync 변경을 받으면 GET으로 다시 읽는다.
