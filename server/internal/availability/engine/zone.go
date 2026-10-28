package engine

import (
	"sort"
	"time"
)

// Date는 시간대가 없는 달력 날짜다(요청 시간대 또는 멤버 시간대 기준으로 해석한다).
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateOf는 t를 loc의 지역 시각으로 본 날짜를 반환한다.
func DateOf(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return Date{Year: y, Month: m, Day: d}
}

// noon은 날짜 산술용 기준점이다. 지역 자정은 DST로 없거나 두 번일 수 있으므로 UTC 정오를 쓴다.
func (d Date) noon() time.Time { return time.Date(d.Year, d.Month, d.Day, 12, 0, 0, 0, time.UTC) }

// AddDays는 n일 뒤 날짜를 반환한다(월·연 경계 정규화).
func (d Date) AddDays(n int) Date {
	y, m, dd := d.noon().AddDate(0, 0, n).Date()
	return Date{Year: y, Month: m, Day: dd}
}

// Before는 d가 o보다 앞선 날짜인지 반환한다.
func (d Date) Before(o Date) bool { return d.noon().Before(o.noon()) }

// ISOWeekday는 ISO 요일(1=월 … 7=일)을 반환한다. DB availability_weekly_intervals.weekday와 같은 값이다.
func (d Date) ISOWeekday() int { return (int(d.noon().Weekday())+6)%7 + 1 }

// boundary는 DST 중복 시각에서 어느 occurrence를 쓸지 정한다(설계 6 §3.3).
type boundary int

const (
	// asStart: 중복 시각은 첫 번째(이른) occurrence를 쓴다. 활동 시작과 날짜 경계에 쓴다.
	asStart boundary = iota
	// asEnd: 중복 시각은 두 번째(늦은) occurrence를 쓴다. 활동 종료에 써서 구간이 줄지 않게 한다.
	asEnd
)

// localCandidates는 loc에서 지역 벽시계 시각(d, minute)에 해당하는 instant 후보를 오름차순으로 반환한다.
// minute은 0~1440이며 1440은 다음 날 00:00이다.
//
// 결과 개수: 1이면 정상, 2이면 DST 중복(fall-back), 0이면 존재하지 않는 시각(spring-forward gap)이다.
// time.Date는 gap·중복에서 어느 쪽을 고를지 보장하지 않으므로 직접 판정한다. 벽시계 시각을 UTC로 본 값 w에서
// w−36h, w, w+36h 세 시점의 offset을 후보로 모으고, w−offset을 다시 loc로 바꿨을 때 같은 벽시계 시각이 되는 것만 남긴다.
// 가정: probe 창 [w−36h, w+36h] 안에 offset 전환이 2번 이상 없다. 이 가정이 맞으면 창 양 끝 offset이 전환 전후
// offset을 모두 담는다(tzdb의 실제 시간대는 전환 간격이 이보다 훨씬 길다). gapEnd도 같은 가정을 쓴다.
func localCandidates(d Date, minute int, loc *time.Location) (wall time.Time, cands []time.Time) {
	wall = time.Date(d.Year, d.Month, d.Day, 0, minute, 0, 0, time.UTC)
	offsets := make([]int, 0, 3)
	for _, probe := range []time.Duration{-36 * time.Hour, 0, 36 * time.Hour} {
		_, off := wall.Add(probe).In(loc).Zone()
		if !containsInt(offsets, off) {
			offsets = append(offsets, off)
		}
	}
	for _, off := range offsets {
		t := wall.Add(-time.Duration(off) * time.Second)
		if sameWall(t.In(loc), wall) && !containsTime(cands, t) {
			cands = append(cands, t)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Before(cands[j]) })
	return wall, cands
}

// resolveLocal은 지역 벽시계 시각을 설계 6 §3.3 규칙으로 하나의 UTC instant로 바꾼다.
//   - 존재하지 않는 시각: gap이 끝나는 instant(다음 유효 시각).
//   - 중복 시각: asStart는 이른 occurrence, asEnd는 늦은 occurrence.
func resolveLocal(d Date, minute int, loc *time.Location, b boundary) time.Time {
	wall, cands := localCandidates(d, minute, loc)
	switch len(cands) {
	case 1:
		return cands[0].UTC()
	case 0:
		return gapEnd(wall, loc).UTC()
	}
	if b == asEnd {
		return cands[len(cands)-1].UTC()
	}
	return cands[0].UTC()
}

// gapEnd는 존재하지 않는 벽시계 시각 wall이 속한 gap이 끝나는 instant(전환 시각)를 반환한다.
// gap은 offset이 늘어날 때 생기므로 이전(작은) offset으로 해석한 instant는 이미 전환 뒤에 있고,
// 그 instant가 속한 zone 구간의 시작이 곧 전환 시각이다.
func gapEnd(wall time.Time, loc *time.Location) time.Time {
	minOff := 0
	for i, probe := range []time.Duration{-36 * time.Hour, 0, 36 * time.Hour} {
		_, off := wall.Add(probe).In(loc).Zone()
		if i == 0 || off < minOff {
			minOff = off
		}
	}
	t := wall.Add(-time.Duration(minOff) * time.Second).In(loc)
	start, _ := t.ZoneBounds()
	if start.IsZero() {
		return t
	}
	return start
}

func sameWall(t, wall time.Time) bool {
	y, m, d := t.Date()
	wy, wm, wd := wall.Date()
	return y == wy && m == wm && d == wd && t.Hour() == wall.Hour() && t.Minute() == wall.Minute() && t.Second() == wall.Second()
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func containsTime(xs []time.Time, x time.Time) bool {
	for _, v := range xs {
		if v.Equal(x) {
			return true
		}
	}
	return false
}

// RequestEnvelope는 요청 날짜 범위 [startDate 00:00, endDate+1 00:00)를 요청 시간대 loc 기준 UTC 구간으로 반환한다.
// 양 끝 모두 날짜 경계이므로 asStart 규칙(중복이면 이른 occurrence, gap이면 gap 끝)을 쓴다.
// A3는 이 구간이 모든 멤버 generation window 교집합 안에 있는지 검사한다(설계 6 §4).
func RequestEnvelope(startDate, endDate Date, loc *time.Location) Interval {
	return Interval{
		Start: resolveLocal(startDate, 0, loc, asStart),
		End:   resolveLocal(endDate.AddDays(1), 0, loc, asStart),
	}
}
