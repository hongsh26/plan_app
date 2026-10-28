// Package engine은 공통 가능 시간 검색의 순수 계산 모듈이다(설계 6 §5.2~5.3).
//
// DB와 I/O에 의존하지 않는다. 모든 구간은 반개구간 [Start, End)이며 경계가 닿기만 하면 겹치지 않는다.
// 같은 입력은 항상 같은 순서의 같은 결과를 만든다.
package engine

import (
	"sort"
	"time"
)

// Interval은 반개구간 [Start, End)다. 결과로 반환하는 값은 항상 UTC다.
type Interval struct {
	Start, End time.Time
}

func (iv Interval) empty() bool { return !iv.Start.Before(iv.End) }

func utc(iv Interval) Interval { return Interval{Start: iv.Start.UTC(), End: iv.End.UTC()} }

// MergeIntervals는 구간을 시작 순으로 정렬하고 겹치거나 인접한 구간을 합친다. 길이가 0 이하인 구간은 버린다.
// 입력 slice는 변경하지 않는다.
func MergeIntervals(ivs []Interval) []Interval {
	sorted := make([]Interval, 0, len(ivs))
	for _, iv := range ivs {
		if !iv.empty() {
			sorted = append(sorted, utc(iv))
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].Start.Equal(sorted[j].Start) {
			return sorted[i].Start.Before(sorted[j].Start)
		}
		return sorted[i].End.Before(sorted[j].End)
	})
	out := sorted[:0]
	for _, iv := range sorted {
		if n := len(out); n > 0 && !iv.Start.After(out[n-1].End) {
			if iv.End.After(out[n-1].End) {
				out[n-1].End = iv.End
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// Subtract는 base에서 cut을 뺀 구간을 반환한다. 두 입력은 임의 순서여도 되며 내부에서 병합한다.
func Subtract(base, cut []Interval) []Interval {
	base, cut = MergeIntervals(base), MergeIntervals(cut)
	var out []Interval
	j := 0
	for _, b := range base {
		cur := b.Start
		// b보다 완전히 앞선 cut은 이후 base에도 영향이 없으므로 건너뛴다.
		for j < len(cut) && !cut[j].End.After(b.Start) {
			j++
		}
		for k := j; k < len(cut) && cut[k].Start.Before(b.End); k++ {
			if cut[k].Start.After(cur) {
				out = append(out, Interval{Start: cur, End: cut[k].Start})
			}
			if cut[k].End.After(cur) {
				cur = cut[k].End
			}
		}
		if cur.Before(b.End) {
			out = append(out, Interval{Start: cur, End: b.End})
		}
	}
	return out
}

// Intersect는 두 구간 집합의 교집합을 반환한다.
func Intersect(a, b []Interval) []Interval {
	a, b = MergeIntervals(a), MergeIntervals(b)
	var out []Interval
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		s, e := maxTime(a[i].Start, b[j].Start), minTime(a[i].End, b[j].End)
		if s.Before(e) {
			out = append(out, Interval{Start: s, End: e})
		}
		if a[i].End.Before(b[j].End) {
			i++
		} else {
			j++
		}
	}
	return out
}

// IntersectAll은 모든 집합의 교집합을 반환한다. 집합이 0개면 빈 결과다(호출자는 멤버 1명 이상을 보장한다).
func IntersectAll(sets [][]Interval) []Interval {
	if len(sets) == 0 {
		return nil
	}
	acc := MergeIntervals(sets[0])
	for _, s := range sets[1:] {
		if len(acc) == 0 {
			return nil
		}
		acc = Intersect(acc, s)
	}
	return acc
}

// clip은 정렬·병합된 구간을 env 안으로 자른다.
func clip(ivs []Interval, env Interval) []Interval {
	var out []Interval
	for _, iv := range ivs {
		s, e := maxTime(iv.Start, env.Start), minTime(iv.End, env.End)
		if s.Before(e) {
			out = append(out, Interval{Start: s, End: e})
		}
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
