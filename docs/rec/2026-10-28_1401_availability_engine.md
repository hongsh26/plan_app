# 공통 가능 시간 순수 계산 엔진(A2) — 기능 구현 완료

- 일시: 2026-10-28 14:01
- 브랜치: `feature/availability-search`
- 기준: `docs/availability_search_design.md` §2, §3.2~3.3, §4, §5.2~5.3, §13 계산 정확성, §14 순수 계산
- 위치: `server/internal/availability/engine/` (DB·I/O 없음)
- A0(서버 측): §13·§14 항목을 테이블 기반 계약 테스트로 먼저 작성하고 구현을 맞췄다. Swift는 범위 밖이다.

## API

```go
type Interval struct{ Start, End time.Time }             // 반개구간 [Start, End), 결과는 UTC
type Date struct{ Year int; Month time.Month; Day int }   // 시간대 없는 날짜
type WeeklyInterval struct{ Weekday, StartMinute, EndMinute int } // ISO 1=월…7=일, 0~1440
type Member struct{ Location *time.Location; Weekly []WeeklyInterval; Busy []Interval }
type Params struct{ StartDate, EndDate Date; Duration time.Duration; Location *time.Location; MaxResults int; Now time.Time }

func Search(members []Member, p Params) []Interval
func RequestEnvelope(startDate, endDate Date, loc *time.Location) Interval
func ExpandWeekly(weekly []WeeklyInterval, loc *time.Location, env Interval) []Interval
func MemberFree(m Member, env Interval) []Interval
func MergeIntervals(ivs []Interval) []Interval
func Subtract(base, cut []Interval) []Interval
func Intersect(a, b []Interval) []Interval
func IntersectAll(sets [][]Interval) []Interval
func GenerateSlots(common []Interval, p Params) []Interval
func LimitPerDay(slots []Interval, loc *time.Location, perDay, maxResults int) []Interval
func EarliestStart(p Params) time.Time
func DateOf(t time.Time, loc *time.Location) Date
func (d Date) AddDays(n int) Date / Before(o Date) bool / ISOWeekday() int
```

- `WeeklyInterval.Weekday`는 DB `availability_weekly_intervals.weekday`(ISO 1~7)와 같은 값이다.
- `Member.Busy`는 `calendar_busy_facts.start_at/end_at`(UTC)을 그대로 넣는다. all-day도 저장된 UTC 범위를 쓴다.
- 입력 범위 검증(31일, 30~480분, 1~20개, 과거 날짜)은 호출자(A4) 책임이다. 엔진은 멤버 0명·Location nil·
  Duration ≤ 0·MaxResults ≤ 0이면 빈 결과를 반환한다.
- 범위 밖이거나 start ≥ end인 `WeeklyInterval`은 오류 없이 건너뛴다. 규칙 검증·정규화는 A1 저장 시점 책임이다.
- A3는 `RequestEnvelope`로 요청 UTC envelope를 구해 generation window 교집합 포함 여부를 검사한다.

## 파이프라인

1. `RequestEnvelope` = 요청 시간대의 `[StartDate 00:00, EndDate+1 00:00)`.
2. 멤버별 `ExpandWeekly`: envelope를 멤버 시간대 날짜로 본 범위 앞뒤 하루씩 더 확장 → DST 규칙으로 UTC 변환 →
   병합 → envelope로 자름. 병합 단계에서 자정 분할 구간(월 22:00~24:00 + 화 00:00~02:00)이 이어진다.
3. `MemberFree` = 활동 − 병합된 Busy. 4. `IntersectAll`. 5. `GenerateSlots`. 6. `LimitPerDay(3, MaxResults)`.

## DST resolver 설계

`time.Date`는 gap·중복에서 어느 쪽을 고를지 보장하지 않으므로 직접 판정한다(`zone.go`).

- 벽시계 시각을 UTC로 본 값 `w`에서 −36h/0/+36h 시점의 zone offset을 후보로 모은다.
- 각 offset으로 `w − offset`을 만들고 `In(loc)` 왕복 시 같은 벽시계 시각인 것만 남긴다.
  - 1개: 정상, 2개: 중복(fall-back), 0개: 존재하지 않음(spring-forward gap).
- 중복: 활동 시작·날짜 경계는 이른 occurrence(`asStart`), 활동 종료는 늦은 occurrence(`asEnd`)로 구간을 줄이지 않는다.
- gap: 다음 유효 시각 = gap이 끝나는 전환 instant. gap은 offset 증가로만 생기므로 작은(이전) offset으로 해석한 instant는
  전환 뒤에 있고, 그 instant의 `ZoneBounds()` 시작이 전환 시각이다.
- 변환 후 `start >= end`인 occurrence만 버린다. 같은 날 다른 구간은 영향받지 않는다.
- 중복 규칙 때문에 같은 날 두 구간이 UTC에서 겹칠 수 있다(예: NY 11/1 00:00~01:00, 01:30~03:00). 확장 결과를 병합해 처리한다.
- `EndMinute` 1440은 다음 날 00:00으로 해석한다(`asEnd` 규칙).

## Grid 결정

- 후보 시작은 요청 시간대 각 날짜의 벽시계 00/30분이다. UTC에서 30분씩 더하지 않고 벽시계 시각마다 resolver로 계산한다.
  그래서 `Asia/Kathmandu`(+05:45) 요청은 UTC :15/:45 시작이 된다.
- 존재하지 않는 grid 시각(spring-forward)은 **건너뛴다**. gap 끝으로 옮기면 grid 밖 시작이 생길 수 있기 때문이다
  (NY 3/8은 46개 후보).
- 중복 grid 시각(fall-back)은 **두 occurrence 모두** 후보다. 같은 벽시계 경계를 가진 서로 다른 시각이며,
  UI는 약식 time zone(EDT/EST)으로 구분한다(NY 11/1은 50개 후보). 날짜별 3개 제한은 그대로 적용된다.
- 날짜 경계(envelope 끝, 날짜별 제한의 날짜 판정)는 `asStart` 규칙을 쓴다.
- 가장 이른 시작 = `max(Now + 30분, StartDate 00:00)` 이상인 첫 grid 시각(올림). 정확히 경계면 그 시각을 쓴다.
- 슬롯 종료 = 시작 + Duration(절대 시간). envelope 끝을 넘으면 제외한다.
- 공통 free 구간은 병합 후 정렬돼 있어 이분 탐색으로 포함 구간을 찾는다.

## 테스트 (`go test ./internal/availability/engine/...`)

- `TestMergeIntervals`: 겹침·인접 병합, 정렬, 포함, 길이 0 제거
- `TestSubtractAndIntersect`: 활동 − Busy, 경계에 닿는 Busy, 전체 덮음, 겹치는 Busy, 다중 교집합, 닿기만 하는 교집합, 집합 0개
- `TestDateHelpers`: ISO 요일(일=7), 연말 AddDays
- `TestResolveLocalDST`: Seoul, NY 3/8 gap·11/1 overlap, London 3/29·10/25, Lord Howe 30분 gap·overlap.
  fixture 날짜가 실제 전환일인지 먼저 검사한다. `time/tzdata`를 테스트에 포함해 호스트 zoneinfo와 무관하다.
- `TestExpandWeeklyDST`: gap 안 occurrence 버림 + 같은 날 다른 구간 유지, gap 횡단 축소, overlap 시작/종료 규칙, overlap 겹침 병합
- `TestRequestEnvelope`: 일반 날짜, 25시간 fall-back 날
- `TestSearch`: 활동 − Busy, 활동 밖 슬롯 없음, 인접 Busy 병합, 멤버 교집합, 서울+뉴욕 UTC 교차, 자정 횡단 슬롯,
  all-day Busy, end bound, Kathmandu grid, fall-back 두 occurrence·하루 3개, spring-forward 건너뜀, 하루 3개·전체 20개,
  MaxResults, 멤버 0명
- `TestLeadTime`: 10:00:00 → 10:30, 10:00:01 → 11:00, 10:15 → 11:00, 시작일 이전 now, 오늘 모두 지남
- `TestLimitPerDayStable`, `TestDeterministic`(멤버·Busy 순서 섞어도 같은 결과)
- `TestPerformance31Days` + `BenchmarkSearch31Days20Members`: 5개 시간대 20명 × 31일 × 멤버당 Busy 60개, 매일 종일 활동.
  결과 20개가 나오고 어느 Busy와도 겹치지 않는지 확인한다. ≈ 1.1ms/op (Apple M2)

## 검증

```
cd server && gofmt -l . && go vet ./... && go test -count=1 ./internal/availability/engine/...
```

모두 통과했다.

## 후속

- A3: `RequestEnvelope`로 generation window 검사, DB에서 `Member` 입력 조립.
- A4: 입력 한도 검증 뒤 `Search` 호출, 결과를 `startAt`/`endAt`으로 직렬화.
