package engine

import (
	"sort"
	"time"
)

// WeeklyInterval은 멤버 시간대 기준 한 요일의 활동 구간 [StartMinute, EndMinute)다.
// A1이 저장한 정규화 결과(같은 날, 겹침 없음, 30분 경계, 0 ≤ Start < End ≤ 1440)를 그대로 받는다.
// EndMinute 1440은 다음 날 00:00이다. 자정 횡단 활동은 두 요일 구간으로 나뉘어 저장되며 확장 후 병합된다.
type WeeklyInterval struct {
	Weekday     int // ISO 요일: 1=월 … 7=일
	StartMinute int
	EndMinute   int
}

// Member는 한 활성 멤버의 계산 입력이다.
type Member struct {
	Location *time.Location   // 활동 시간 설정의 IANA 시간대
	Weekly   []WeeklyInterval // 주간 활동 규칙
	Busy     []Interval       // private Busy facts(UTC [start_at, end_at)). all-day도 저장된 UTC 범위 그대로 넣는다.
}

// Params는 검색 파라미터다. 범위 검증(31일, 30~480분, 1~20개 등)은 호출자가 끝낸 값이다.
type Params struct {
	StartDate, EndDate Date           // 요청 시간대 기준, 양 끝 포함
	Duration           time.Duration  // 약속 길이(절대 시간)
	Location           *time.Location // 요청 표시 시간대(slot grid와 날짜별 제한의 기준)
	MaxResults         int            // 전체 최대 결과 수. 0 이하면 결과가 없다.
	Now                time.Time      // 서버 현재 시각
}

const (
	gridStep    = 30 // 분
	leadTime    = 30 * time.Minute
	perDayCap   = 3
	minuteOfDay = 24 * 60
)

// ExpandWeekly는 주간 규칙을 loc 기준 날짜별 UTC 구간으로 확장해 env와 겹치는 부분만 병합해 반환한다.
// 멤버 시간대와 요청 시간대가 다를 수 있으므로 env를 loc 날짜로 본 범위 앞뒤로 하루씩 더 확장한 뒤 자른다.
// DST 규칙(설계 6 §3.3): 시작은 asStart, 종료는 asEnd로 변환하고 변환 뒤 start ≥ end인 occurrence만 버린다.
func ExpandWeekly(weekly []WeeklyInterval, loc *time.Location, env Interval) []Interval {
	if loc == nil || env.empty() {
		return nil
	}
	first := DateOf(env.Start, loc).AddDays(-1)
	last := DateOf(env.End, loc).AddDays(1)
	var out []Interval
	for d := first; !last.Before(d); d = d.AddDays(1) {
		wd := d.ISOWeekday()
		for _, w := range weekly {
			if w.Weekday != wd || w.StartMinute < 0 || w.EndMinute > minuteOfDay || w.StartMinute >= w.EndMinute {
				continue
			}
			s := resolveLocal(d, w.StartMinute, loc, asStart)
			e := resolveLocal(d, w.EndMinute, loc, asEnd)
			if s.Before(e) {
				out = append(out, Interval{Start: s, End: e})
			}
		}
	}
	// 자정 분할 구간(22:00~24:00 + 다음 날 00:00~02:00)과 DST 중복으로 겹친 구간을 여기서 합친다.
	return clip(MergeIntervals(out), env)
}

// MemberFree는 한 멤버의 free interval(활동 시간 - 병합된 Busy)을 env 안에서 반환한다.
func MemberFree(m Member, env Interval) []Interval {
	return Subtract(ExpandWeekly(m.Weekly, m.Location, env), m.Busy)
}

// gridStarts는 요청 시간대의 각 날짜 00/30분 벽시계 경계에 해당하는 instant를 오름차순으로 반환한다.
//   - 존재하지 않는 grid 시각(spring-forward gap)은 건너뛴다. gap 끝으로 옮기면 grid 밖 시작이 생기기 때문이다.
//   - 중복 grid 시각(fall-back)은 두 occurrence를 모두 후보로 둔다. 둘 다 같은 벽시계 경계를 가진 서로 다른 시각이다.
//
// UTC에서 30분씩 더하지 않고 벽시계 시각에서 계산하므로 +05:45 같은 시간대에서도 지역 00/30분에 맞는다.
func gridStarts(startDate, endDate Date, loc *time.Location) []time.Time {
	var out []time.Time
	for d := startDate; !endDate.Before(d); d = d.AddDays(1) {
		for m := 0; m < minuteOfDay; m += gridStep {
			_, cands := localCandidates(d, m, loc)
			for _, c := range cands {
				out = append(out, c.UTC())
			}
		}
	}
	return out
}

// EarliestStart는 max(now + 30분, startDate 00:00)이다. 실제 첫 후보는 이 시각 이상인 첫 grid 시각(올림)이다.
func EarliestStart(p Params) time.Time {
	return maxTime(p.Now.Add(leadTime), RequestEnvelope(p.StartDate, p.EndDate, p.Location).Start).UTC()
}

// GenerateSlots는 공통 free interval 안에 완전히 들어가는 grid 슬롯을 시작 오름차순으로 반환한다.
// 시작은 EarliestStart 이상인 grid 시각이고, 종료(시작 + Duration)는 요청 envelope 끝을 넘지 않는다.
func GenerateSlots(common []Interval, p Params) []Interval {
	if p.Location == nil || p.Duration <= 0 || p.EndDate.Before(p.StartDate) {
		return nil
	}
	free := MergeIntervals(common)
	env := RequestEnvelope(p.StartDate, p.EndDate, p.Location)
	earliest := EarliestStart(p)
	var out []Interval
	for _, s := range gridStarts(p.StartDate, p.EndDate, p.Location) {
		e := s.Add(p.Duration)
		if s.Before(earliest) || e.After(env.End) {
			continue
		}
		// free는 서로 떨어진 정렬 구간이므로 s를 담을 수 있는 후보는 End > s인 첫 구간 하나뿐이다.
		i := sort.Search(len(free), func(i int) bool { return free[i].End.After(s) })
		if i < len(free) && !free[i].Start.After(s) && !e.After(free[i].End) {
			out = append(out, Interval{Start: s, End: e})
		}
	}
	sortSlots(out)
	return out
}

func sortSlots(slots []Interval) {
	sort.SliceStable(slots, func(i, j int) bool {
		if !slots[i].Start.Equal(slots[j].Start) {
			return slots[i].Start.Before(slots[j].Start)
		}
		return slots[i].End.Before(slots[j].End)
	})
}

// LimitPerDay는 슬롯을 (시작, 종료) 오름차순으로 정렬한 뒤 loc 기준 시작 날짜별 앞 perDay개만 남기고,
// 그중 앞 maxResults개를 반환한다(설계 6 §5.3). 입력 slice는 변경하지 않는다.
func LimitPerDay(slots []Interval, loc *time.Location, perDay, maxResults int) []Interval {
	if loc == nil || perDay <= 0 || maxResults <= 0 {
		return nil
	}
	sorted := make([]Interval, len(slots))
	for i, s := range slots {
		sorted[i] = utc(s)
	}
	sortSlots(sorted)
	count := map[Date]int{}
	var out []Interval
	for _, s := range sorted {
		d := DateOf(s.Start, loc)
		if count[d] >= perDay {
			continue
		}
		count[d]++
		out = append(out, s)
		if len(out) == maxResults {
			break
		}
	}
	return out
}

// Search는 모든 멤버가 공통으로 가능한 슬롯을 결정적으로 계산한다(설계 6 §5.2~5.3).
// 멤버가 0명이면 빈 결과다. 호출자는 활성 멤버 1명 이상과 준비 조건(활동 시간 configured, freshness)을 보장한다.
func Search(members []Member, p Params) []Interval {
	if len(members) == 0 || p.Location == nil || p.MaxResults <= 0 {
		return nil
	}
	env := RequestEnvelope(p.StartDate, p.EndDate, p.Location)
	if env.empty() {
		return nil
	}
	sets := make([][]Interval, len(members))
	for i, m := range members {
		sets[i] = MemberFree(m, env)
	}
	return LimitPerDay(GenerateSlots(IntersectAll(sets), p), p.Location, perDayCap, p.MaxResults)
}
