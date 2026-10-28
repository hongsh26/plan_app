package engine

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	// 호스트 zoneinfo와 무관하게 같은 tzdb로 테스트한다.
	_ "time/tzdata"
)

func mustLoc(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func ts(t testing.TB, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func iv(t testing.TB, s, e string) Interval { return Interval{Start: ts(t, s), End: ts(t, e)} }

// fmtIvs는 비교용 문자열이다. time.Time은 loc 포인터가 달라 DeepEqual로 비교하지 않는다.
func fmtIvs(ivs []Interval) []string {
	out := make([]string, len(ivs))
	for i, v := range ivs {
		out[i] = v.Start.UTC().Format(time.RFC3339) + "/" + v.End.UTC().Format(time.RFC3339)
	}
	return out
}

func assertIvs(t *testing.T, got []Interval, want []string) {
	t.Helper()
	g := fmtIvs(got)
	if len(g) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("got  %v\nwant %v", g, want)
	}
}

func date(y int, m time.Month, d int) Date { return Date{Year: y, Month: m, Day: d} }

// allWeek는 모든 요일에 같은 구간을 둔 주간 규칙이다.
func allWeek(start, end int) []WeeklyInterval {
	var w []WeeklyInterval
	for wd := 1; wd <= 7; wd++ {
		w = append(w, WeeklyInterval{Weekday: wd, StartMinute: start, EndMinute: end})
	}
	return w
}

// assertTransition은 fixture 날짜가 실제 DST 전환일인지 확인한다. tzdb 규칙이 바뀌면 테스트가 조용히 무의미해지지 않게 한다.
func assertTransition(t *testing.T, loc *time.Location, d Date) {
	t.Helper()
	_, before := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc).Zone()
	_, after := time.Date(d.Year, d.Month, d.Day, 23, 0, 0, 0, loc).Zone()
	if before == after {
		t.Fatalf("%s %v is not a DST transition date", loc, d)
	}
}

func TestMergeIntervals(t *testing.T) {
	cases := []struct {
		name string
		in   []Interval
		want []string
	}{
		{"겹침 병합", []Interval{iv(t, "2026-11-02T01:00:00Z", "2026-11-02T03:00:00Z"), iv(t, "2026-11-02T02:00:00Z", "2026-11-02T04:00:00Z")},
			[]string{"2026-11-02T01:00:00Z/2026-11-02T04:00:00Z"}},
		{"인접 병합", []Interval{iv(t, "2026-11-02T02:00:00Z", "2026-11-02T03:00:00Z"), iv(t, "2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z")},
			[]string{"2026-11-02T01:00:00Z/2026-11-02T03:00:00Z"}},
		{"분리 유지·정렬", []Interval{iv(t, "2026-11-02T05:00:00Z", "2026-11-02T06:00:00Z"), iv(t, "2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z")},
			[]string{"2026-11-02T01:00:00Z/2026-11-02T02:00:00Z", "2026-11-02T05:00:00Z/2026-11-02T06:00:00Z"}},
		{"포함 구간", []Interval{iv(t, "2026-11-02T01:00:00Z", "2026-11-02T06:00:00Z"), iv(t, "2026-11-02T02:00:00Z", "2026-11-02T03:00:00Z")},
			[]string{"2026-11-02T01:00:00Z/2026-11-02T06:00:00Z"}},
		{"길이 0 제거", []Interval{iv(t, "2026-11-02T01:00:00Z", "2026-11-02T01:00:00Z")}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertIvs(t, MergeIntervals(c.in), c.want) })
	}
}

func TestSubtractAndIntersect(t *testing.T) {
	base := []Interval{iv(t, "2026-11-02T00:00:00Z", "2026-11-02T04:00:00Z")}
	cases := []struct {
		name string
		cut  []Interval
		want []string
	}{
		{"가운데 Busy", []Interval{iv(t, "2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z")},
			[]string{"2026-11-02T00:00:00Z/2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z/2026-11-02T04:00:00Z"}},
		{"경계에 닿는 Busy는 빼지 않는다", []Interval{iv(t, "2026-11-01T22:00:00Z", "2026-11-02T00:00:00Z"), iv(t, "2026-11-02T04:00:00Z", "2026-11-02T05:00:00Z")},
			[]string{"2026-11-02T00:00:00Z/2026-11-02T04:00:00Z"}},
		{"전체 덮음", []Interval{iv(t, "2026-11-01T00:00:00Z", "2026-11-03T00:00:00Z")}, nil},
		{"겹치는 Busy 여러 개", []Interval{iv(t, "2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z"), iv(t, "2026-11-02T01:30:00Z", "2026-11-02T03:00:00Z")},
			[]string{"2026-11-02T00:00:00Z/2026-11-02T01:00:00Z", "2026-11-02T03:00:00Z/2026-11-02T04:00:00Z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertIvs(t, Subtract(base, c.cut), c.want) })
	}

	t.Run("교집합", func(t *testing.T) {
		a := []Interval{iv(t, "2026-11-02T00:00:00Z", "2026-11-02T03:00:00Z"), iv(t, "2026-11-02T05:00:00Z", "2026-11-02T08:00:00Z")}
		b := []Interval{iv(t, "2026-11-02T02:00:00Z", "2026-11-02T06:00:00Z")}
		c := []Interval{iv(t, "2026-11-02T00:00:00Z", "2026-11-02T09:00:00Z")}
		assertIvs(t, IntersectAll([][]Interval{a, b, c}),
			[]string{"2026-11-02T02:00:00Z/2026-11-02T03:00:00Z", "2026-11-02T05:00:00Z/2026-11-02T06:00:00Z"})
	})
	t.Run("닿기만 하는 구간의 교집합은 비어 있다", func(t *testing.T) {
		a := []Interval{iv(t, "2026-11-02T00:00:00Z", "2026-11-02T03:00:00Z")}
		b := []Interval{iv(t, "2026-11-02T03:00:00Z", "2026-11-02T06:00:00Z")}
		assertIvs(t, IntersectAll([][]Interval{a, b}), nil)
	})
	t.Run("집합 0개는 빈 결과", func(t *testing.T) { assertIvs(t, IntersectAll(nil), nil) })
}

func TestDateHelpers(t *testing.T) {
	cases := []struct {
		d    Date
		want int
	}{
		{date(2026, 11, 2), 1}, // 월
		{date(2026, 11, 7), 6}, // 토
		{date(2026, 11, 8), 7}, // 일 → ISO 7
	}
	for _, c := range cases {
		if got := c.d.ISOWeekday(); got != c.want {
			t.Errorf("%v ISOWeekday = %d, want %d", c.d, got, c.want)
		}
	}
	if got := date(2026, 12, 31).AddDays(1); got != date(2027, 1, 1) {
		t.Errorf("AddDays 연말 = %v", got)
	}
}

func TestResolveLocalDST(t *testing.T) {
	ny, london, seoul, lh := mustLoc(t, "America/New_York"), mustLoc(t, "Europe/London"), mustLoc(t, "Asia/Seoul"), mustLoc(t, "Australia/Lord_Howe")
	for _, f := range []struct {
		loc *time.Location
		d   Date
	}{{ny, date(2026, 3, 8)}, {ny, date(2026, 11, 1)}, {london, date(2026, 3, 29)}, {london, date(2026, 10, 25)}, {lh, date(2026, 10, 4)}, {lh, date(2026, 4, 5)}} {
		assertTransition(t, f.loc, f.d)
	}

	cases := []struct {
		name      string
		loc       *time.Location
		d         Date
		minute    int
		startWant string
		endWant   string
		nCands    int
	}{
		{"서울 일반", seoul, date(2026, 11, 2), 9 * 60, "2026-11-02T00:00:00Z", "2026-11-02T00:00:00Z", 1},
		{"서울 1440은 다음 날 00:00", seoul, date(2026, 11, 2), 1440, "2026-11-02T15:00:00Z", "2026-11-02T15:00:00Z", 1},
		{"NY gap 02:00 → 03:00 EDT", ny, date(2026, 3, 8), 2 * 60, "2026-03-08T07:00:00Z", "2026-03-08T07:00:00Z", 0},
		{"NY gap 02:30 → 03:00 EDT", ny, date(2026, 3, 8), 2*60 + 30, "2026-03-08T07:00:00Z", "2026-03-08T07:00:00Z", 0},
		{"NY gap 직후 03:00", ny, date(2026, 3, 8), 3 * 60, "2026-03-08T07:00:00Z", "2026-03-08T07:00:00Z", 1},
		{"NY overlap 01:00 시작=EDT 종료=EST", ny, date(2026, 11, 1), 60, "2026-11-01T05:00:00Z", "2026-11-01T06:00:00Z", 2},
		{"NY overlap 01:30", ny, date(2026, 11, 1), 90, "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", 2},
		{"NY overlap 직후 02:00은 하나", ny, date(2026, 11, 1), 120, "2026-11-01T07:00:00Z", "2026-11-01T07:00:00Z", 1},
		{"London gap 01:30 → 02:00 BST", london, date(2026, 3, 29), 90, "2026-03-29T01:00:00Z", "2026-03-29T01:00:00Z", 0},
		{"London overlap 01:30", london, date(2026, 10, 25), 90, "2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z", 2},
		{"Lord Howe 30분 gap 02:00 → 02:30", lh, date(2026, 10, 4), 120, "2026-10-03T15:30:00Z", "2026-10-03T15:30:00Z", 0},
		{"Lord Howe 30분 overlap 01:30", lh, date(2026, 4, 5), 90, "2026-04-04T14:30:00Z", "2026-04-04T15:00:00Z", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, cands := localCandidates(c.d, c.minute, c.loc); len(cands) != c.nCands {
				t.Fatalf("candidates = %d, want %d", len(cands), c.nCands)
			}
			if got := resolveLocal(c.d, c.minute, c.loc, asStart).Format(time.RFC3339); got != c.startWant {
				t.Errorf("asStart = %s, want %s", got, c.startWant)
			}
			if got := resolveLocal(c.d, c.minute, c.loc, asEnd).Format(time.RFC3339); got != c.endWant {
				t.Errorf("asEnd = %s, want %s", got, c.endWant)
			}
		})
	}
}

func TestExpandWeeklyDST(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	sun := 7
	cases := []struct {
		name   string
		d      Date
		weekly []WeeklyInterval
		want   []string
	}{
		{"gap 안 occurrence만 버리고 같은 날 다른 구간은 유지", date(2026, 3, 8),
			[]WeeklyInterval{{sun, 120, 150}, {sun, 9 * 60, 10 * 60}},
			[]string{"2026-03-08T13:00:00Z/2026-03-08T14:00:00Z"}},
		{"gap을 가로지르는 구간은 1시간으로 줄어든다", date(2026, 3, 8),
			[]WeeklyInterval{{sun, 60, 180}},
			[]string{"2026-03-08T06:00:00Z/2026-03-08T07:00:00Z"}},
		{"overlap: 시작은 첫 occurrence, 종료는 두 번째", date(2026, 11, 1),
			[]WeeklyInterval{{sun, 0, 90}},
			[]string{"2026-11-01T04:00:00Z/2026-11-01T06:30:00Z"}},
		{"overlap으로 겹친 같은 날 구간은 병합", date(2026, 11, 1),
			[]WeeklyInterval{{sun, 0, 60}, {sun, 90, 180}},
			[]string{"2026-11-01T04:00:00Z/2026-11-01T08:00:00Z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := RequestEnvelope(c.d, c.d, ny)
			assertIvs(t, ExpandWeekly(c.weekly, ny, env), c.want)
		})
	}
}

func TestRequestEnvelope(t *testing.T) {
	seoul, ny := mustLoc(t, "Asia/Seoul"), mustLoc(t, "America/New_York")
	assertIvs(t, []Interval{RequestEnvelope(date(2026, 11, 2), date(2026, 11, 3), seoul)},
		[]string{"2026-11-01T15:00:00Z/2026-11-03T15:00:00Z"})
	// fall-back 날은 25시간이다.
	assertIvs(t, []Interval{RequestEnvelope(date(2026, 11, 1), date(2026, 11, 1), ny)},
		[]string{"2026-11-01T04:00:00Z/2026-11-02T05:00:00Z"})
}

type searchCase struct {
	name    string
	members []Member
	p       Params
	raw     bool // true면 날짜별·전체 제한 전 GenerateSlots 결과를 비교한다
	want    []string
	check   func(t *testing.T, got []Interval)
}

func runCase(t *testing.T, c searchCase) {
	t.Helper()
	var got []Interval
	if c.raw {
		env := RequestEnvelope(c.p.StartDate, c.p.EndDate, c.p.Location)
		sets := make([][]Interval, len(c.members))
		for i, m := range c.members {
			sets[i] = MemberFree(m, env)
		}
		got = GenerateSlots(IntersectAll(sets), c.p)
	} else {
		got = Search(c.members, c.p)
	}
	if c.check != nil {
		c.check(t, got)
		return
	}
	assertIvs(t, got, c.want)
}

func TestSearch(t *testing.T) {
	seoul, ny, ktm := mustLoc(t, "Asia/Seoul"), mustLoc(t, "America/New_York"), mustLoc(t, "Asia/Kathmandu")
	past := ts(t, "2026-01-01T00:00:00Z")
	mon := 1
	params := func(loc *time.Location, s, e Date, dur time.Duration) Params {
		return Params{StartDate: s, EndDate: e, Duration: dur, Location: loc, MaxResults: 20, Now: past}
	}
	nov2, nov9, nov10 := date(2026, 11, 2), date(2026, 11, 9), date(2026, 11, 10)

	cases := []searchCase{
		{
			name: "활동 - Busy, 경계에 닿는 Busy는 겹치지 않음",
			members: []Member{{Location: seoul, Weekly: []WeeklyInterval{{mon, 9 * 60, 12 * 60}},
				Busy: []Interval{iv(t, "2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z")}}},
			p:    params(seoul, nov2, nov2, time.Hour),
			want: []string{"2026-11-02T00:00:00Z/2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z/2026-11-02T03:00:00Z"},
		},
		{
			name:    "Busy가 없어도 활동 시간 밖 슬롯은 없다",
			members: []Member{{Location: seoul, Weekly: []WeeklyInterval{{mon, 9 * 60, 10 * 60}}}},
			p:       params(seoul, nov2, date(2026, 11, 8), time.Hour),
			want:    []string{"2026-11-02T00:00:00Z/2026-11-02T01:00:00Z"},
		},
		{
			name:    "활동 구간보다 긴 약속은 없다",
			members: []Member{{Location: seoul, Weekly: []WeeklyInterval{{mon, 9 * 60, 10 * 60}}}},
			p:       params(seoul, nov2, nov2, 90*time.Minute),
			want:    nil,
		},
		{
			name: "인접·겹침 Busy 병합 뒤 중복·누락 없음",
			members: []Member{{Location: seoul, Weekly: []WeeklyInterval{{mon, 9 * 60, 13 * 60}}, Busy: []Interval{
				iv(t, "2026-11-02T01:00:00Z", "2026-11-02T01:30:00Z"),
				iv(t, "2026-11-02T01:30:00Z", "2026-11-02T02:00:00Z"),
				iv(t, "2026-11-02T01:15:00Z", "2026-11-02T01:45:00Z"),
			}}},
			p:   params(seoul, nov2, nov2, 30*time.Minute),
			raw: true,
			want: []string{
				"2026-11-02T00:00:00Z/2026-11-02T00:30:00Z", "2026-11-02T00:30:00Z/2026-11-02T01:00:00Z",
				"2026-11-02T02:00:00Z/2026-11-02T02:30:00Z", "2026-11-02T02:30:00Z/2026-11-02T03:00:00Z",
				"2026-11-02T03:00:00Z/2026-11-02T03:30:00Z", "2026-11-02T03:30:00Z/2026-11-02T04:00:00Z",
			},
		},
		{
			name: "멤버 교집합",
			members: []Member{
				{Location: seoul, Weekly: []WeeklyInterval{{mon, 9 * 60, 12 * 60}}},
				{Location: seoul, Weekly: []WeeklyInterval{{mon, 10 * 60, 14 * 60}}, Busy: []Interval{iv(t, "2026-11-02T02:30:00Z", "2026-11-02T03:00:00Z")}},
			},
			p:    params(seoul, nov2, nov2, time.Hour),
			raw:  true,
			want: []string{"2026-11-02T01:00:00Z/2026-11-02T02:00:00Z", "2026-11-02T01:30:00Z/2026-11-02T02:30:00Z"},
		},
		{
			name: "서울 + 뉴욕 멤버의 UTC 교차",
			members: []Member{
				{Location: seoul, Weekly: []WeeklyInterval{{mon, 9 * 60, 12 * 60}}}, // 월 00:00~03:00Z
				{Location: ny, Weekly: []WeeklyInterval{{7, 20*60 + 30, 23 * 60}}},  // 일 20:30~23:00 EST = 월 01:30~04:00Z
			},
			p:    params(seoul, nov9, nov9, time.Hour),
			raw:  true,
			want: []string{"2026-11-09T01:30:00Z/2026-11-09T02:30:00Z", "2026-11-09T02:00:00Z/2026-11-09T03:00:00Z"},
		},
		{
			name:    "자정 분할 활동은 이어져 자정을 넘는 슬롯을 허용",
			members: []Member{{Location: seoul, Weekly: []WeeklyInterval{{1, 22 * 60, 1440}, {2, 0, 120}}}},
			p:       params(seoul, nov9, nov10, 2*time.Hour),
			raw:     true,
			want: []string{
				"2026-11-09T13:00:00Z/2026-11-09T15:00:00Z", "2026-11-09T13:30:00Z/2026-11-09T15:30:00Z",
				"2026-11-09T14:00:00Z/2026-11-09T16:00:00Z", "2026-11-09T14:30:00Z/2026-11-09T16:30:00Z",
				"2026-11-09T15:00:00Z/2026-11-09T17:00:00Z",
			},
		},
		{
			name: "all-day Busy(UTC 범위)가 있는 날은 제외",
			members: []Member{{Location: seoul, Weekly: allWeek(9*60, 22*60),
				Busy: []Interval{iv(t, "2026-11-09T15:00:00Z", "2026-11-10T15:00:00Z")}}},
			p: params(seoul, nov9, date(2026, 11, 11), time.Hour),
			want: []string{
				"2026-11-09T00:00:00Z/2026-11-09T01:00:00Z", "2026-11-09T00:30:00Z/2026-11-09T01:30:00Z", "2026-11-09T01:00:00Z/2026-11-09T02:00:00Z",
				"2026-11-11T00:00:00Z/2026-11-11T01:00:00Z", "2026-11-11T00:30:00Z/2026-11-11T01:30:00Z", "2026-11-11T01:00:00Z/2026-11-11T02:00:00Z",
			},
		},
		{
			name:    "end bound: 종료일 다음 날 00:00을 넘는 슬롯 제외",
			members: []Member{{Location: seoul, Weekly: allWeek(0, 1440)}},
			p:       params(seoul, nov2, nov2, time.Hour),
			raw:     true,
			check: func(t *testing.T, got []Interval) {
				if len(got) != 47 {
					t.Fatalf("slots = %d, want 47", len(got))
				}
				if last := fmtIvs(got[len(got)-1:])[0]; last != "2026-11-02T14:00:00Z/2026-11-02T15:00:00Z" {
					t.Fatalf("last = %s", last)
				}
			},
		},
		{
			name:    "요청 시간대 +05:45에서도 지역 00/30분 grid",
			members: []Member{{Location: seoul, Weekly: []WeeklyInterval{{mon, 9 * 60, 12 * 60}}}}, // 00:00~03:00Z
			p:       params(ktm, nov9, nov9, time.Hour),
			raw:     true,
			want: []string{
				"2026-11-09T00:15:00Z/2026-11-09T01:15:00Z", "2026-11-09T00:45:00Z/2026-11-09T01:45:00Z",
				"2026-11-09T01:15:00Z/2026-11-09T02:15:00Z", "2026-11-09T01:45:00Z/2026-11-09T02:45:00Z",
			},
		},
		{
			name:    "fall-back 요청일은 중복 grid 시각 두 occurrence를 모두 후보로",
			members: []Member{{Location: ny, Weekly: allWeek(0, 1440)}},
			p:       params(ny, date(2026, 11, 1), date(2026, 11, 1), 30*time.Minute),
			raw:     true,
			check: func(t *testing.T, got []Interval) {
				if len(got) != 50 {
					t.Fatalf("slots = %d, want 50 (25시간)", len(got))
				}
				s := fmtIvs(got)
				for _, w := range []string{"2026-11-01T05:00:00Z/2026-11-01T05:30:00Z", "2026-11-01T06:00:00Z/2026-11-01T06:30:00Z"} {
					if !containsStr(s, w) {
						t.Fatalf("missing %s", w)
					}
				}
			},
		},
		{
			name:    "spring-forward 요청일은 없는 grid 시각을 건너뜀",
			members: []Member{{Location: ny, Weekly: allWeek(0, 1440)}},
			p:       params(ny, date(2026, 3, 8), date(2026, 3, 8), 30*time.Minute),
			raw:     true,
			check: func(t *testing.T, got []Interval) {
				if len(got) != 46 {
					t.Fatalf("slots = %d, want 46 (23시간)", len(got))
				}
				for _, s := range got {
					if m := s.Start.In(ny).Minute(); m != 0 && m != 30 {
						t.Fatalf("off-grid start %v", s.Start.In(ny))
					}
				}
			},
		},
		{
			name:    "fall-back 날도 하루 3개 제한",
			members: []Member{{Location: ny, Weekly: allWeek(0, 1440)}},
			p:       params(ny, date(2026, 11, 1), date(2026, 11, 1), 30*time.Minute),
			want: []string{
				"2026-11-01T04:00:00Z/2026-11-01T04:30:00Z", "2026-11-01T04:30:00Z/2026-11-01T05:00:00Z", "2026-11-01T05:00:00Z/2026-11-01T05:30:00Z",
			},
		},
		{
			name:    "하루 3개 · 전체 20개",
			members: []Member{{Location: seoul, Weekly: allWeek(0, 1440)}},
			p:       params(seoul, nov2, date(2026, 11, 15), time.Hour),
			check: func(t *testing.T, got []Interval) {
				if len(got) != 20 {
					t.Fatalf("slots = %d, want 20", len(got))
				}
				per := map[Date]int{}
				for i, s := range got {
					per[DateOf(s.Start, seoul)]++
					if i > 0 && !got[i-1].Start.Before(s.Start) {
						t.Fatalf("not sorted at %d", i)
					}
					if lm := s.Start.In(seoul); lm.Hour()*60+lm.Minute() != (i%3)*30 {
						t.Fatalf("slot %d starts %v, want 하루 첫 3개", i, lm)
					}
				}
				for d, n := range per {
					if n > 3 {
						t.Fatalf("%v has %d slots", d, n)
					}
				}
				if last := DateOf(got[19].Start, seoul); last != date(2026, 11, 8) {
					t.Fatalf("last date %v", last)
				}
			},
		},
		{
			name:    "MaxResults 적용",
			members: []Member{{Location: seoul, Weekly: allWeek(0, 1440)}},
			p:       Params{StartDate: nov2, EndDate: date(2026, 11, 15), Duration: time.Hour, Location: seoul, MaxResults: 5, Now: past},
			check: func(t *testing.T, got []Interval) {
				if len(got) != 5 {
					t.Fatalf("slots = %d, want 5", len(got))
				}
			},
		},
		{
			name:    "멤버 0명은 빈 결과",
			members: nil,
			p:       params(seoul, nov2, nov2, time.Hour),
			want:    nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runCase(t, c) })
	}
}

func TestLeadTime(t *testing.T) {
	seoul := mustLoc(t, "Asia/Seoul")
	member := []Member{{Location: seoul, Weekly: allWeek(0, 1440)}}
	nov2 := date(2026, 11, 2)
	cases := []struct {
		name, now, firstStart string
	}{
		{"정각 10:00 → 10:30", "2026-11-02T01:00:00Z", "2026-11-02T01:30:00Z"},
		{"10:00:01 → 11:00", "2026-11-02T01:00:01Z", "2026-11-02T02:00:00Z"},
		{"슬롯 중간 10:15 → 11:00", "2026-11-02T01:15:00Z", "2026-11-02T02:00:00Z"},
		{"시작일 이전 now → 시작일 00:00", "2026-10-30T03:00:00Z", "2026-11-01T15:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Params{StartDate: nov2, EndDate: nov2, Duration: 30 * time.Minute, Location: seoul, MaxResults: 20, Now: ts(t, c.now)}
			got := Search(member, p)
			if len(got) == 0 {
				t.Fatal("no slots")
			}
			if s := got[0].Start.Format(time.RFC3339); s != c.firstStart {
				t.Fatalf("first start = %s, want %s", s, c.firstStart)
			}
		})
	}
	t.Run("오늘이 모두 지났으면 빈 결과", func(t *testing.T) {
		p := Params{StartDate: nov2, EndDate: nov2, Duration: 30 * time.Minute, Location: seoul, MaxResults: 20, Now: ts(t, "2026-11-02T14:40:00Z")}
		assertIvs(t, Search(member, p), nil)
	})
}

func TestLimitPerDayStable(t *testing.T) {
	seoul := mustLoc(t, "Asia/Seoul")
	in := []Interval{
		iv(t, "2026-11-02T03:00:00Z", "2026-11-02T04:00:00Z"),
		iv(t, "2026-11-02T00:00:00Z", "2026-11-02T01:00:00Z"),
		iv(t, "2026-11-03T00:00:00Z", "2026-11-03T01:00:00Z"),
		iv(t, "2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z"),
		iv(t, "2026-11-02T00:00:00Z", "2026-11-02T00:30:00Z"),
		iv(t, "2026-11-02T02:00:00Z", "2026-11-02T03:00:00Z"),
	}
	assertIvs(t, LimitPerDay(in, seoul, 3, 20), []string{
		"2026-11-02T00:00:00Z/2026-11-02T00:30:00Z", "2026-11-02T00:00:00Z/2026-11-02T01:00:00Z", "2026-11-02T01:00:00Z/2026-11-02T02:00:00Z",
		"2026-11-03T00:00:00Z/2026-11-03T01:00:00Z",
	})
}

// randomMembers는 성능·결정성 테스트용 고정 seed 입력이다.
// allDay면 매일 00:00~24:00 활동으로 두어 시간대가 달라도 공통 후보가 남게 한다.
func randomMembers(t testing.TB, n, busyPerMember int, seed int64, allDay bool) []Member {
	zones := []string{"Asia/Seoul", "America/New_York", "Europe/London", "Asia/Kathmandu", "Australia/Lord_Howe"}
	r := rand.New(rand.NewSource(seed))
	base := ts(t, "2026-10-25T00:00:00Z")
	members := make([]Member, n)
	for i := range members {
		var weekly []WeeklyInterval
		for wd := 1; wd <= 7; wd++ {
			if allDay {
				weekly = append(weekly, WeeklyInterval{wd, 0, 1440})
				continue
			}
			weekly = append(weekly, WeeklyInterval{wd, 0, 120}, WeeklyInterval{wd, 9 * 60, 13 * 60}, WeeklyInterval{wd, 18 * 60, 1440})
		}
		busy := make([]Interval, busyPerMember)
		for j := range busy {
			s := base.Add(time.Duration(r.Intn(40*24*4)) * 15 * time.Minute)
			busy[j] = Interval{Start: s, End: s.Add(time.Duration(1+r.Intn(8)) * 15 * time.Minute)}
		}
		members[i] = Member{Location: mustLoc(t, zones[i%len(zones)]), Weekly: weekly, Busy: busy}
	}
	return members
}

func TestDeterministic(t *testing.T) {
	seoul := mustLoc(t, "Asia/Seoul")
	members := randomMembers(t, 3, 30, 7, false)
	p := Params{StartDate: date(2026, 11, 1), EndDate: date(2026, 11, 30), Duration: time.Hour, Location: seoul, MaxResults: 20, Now: ts(t, "2026-10-28T00:00:00Z")}
	first := fmtIvs(Search(members, p))
	if len(first) == 0 {
		t.Fatal("fixture should produce slots")
	}
	// Busy 순서와 멤버 순서를 섞어도 결과가 같다.
	r := rand.New(rand.NewSource(1))
	for k := 0; k < 5; k++ {
		shuffled := make([]Member, len(members))
		for i, idx := range r.Perm(len(members)) {
			m := members[idx]
			busy := append([]Interval(nil), m.Busy...)
			r.Shuffle(len(busy), func(a, b int) { busy[a], busy[b] = busy[b], busy[a] })
			m.Busy = busy
			shuffled[i] = m
		}
		if got := fmtIvs(Search(shuffled, p)); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n%v\n%v", k, got, first)
		}
	}
}

func TestPerformance31Days(t *testing.T) {
	seoul := mustLoc(t, "Asia/Seoul")
	members := randomMembers(t, 20, 60, 42, true)
	p := Params{StartDate: date(2026, 11, 1), EndDate: date(2026, 12, 1), Duration: 30 * time.Minute, Location: seoul, MaxResults: 20, Now: ts(t, "2026-10-28T00:00:00Z")}
	start := time.Now()
	got := Search(members, p)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("20명·31일 검색이 %v 걸렸다", el)
	}
	// fixture가 실제 후보를 만들어 포함 검사와 날짜별 제한까지 거치는지 확인한다.
	if len(got) != 20 {
		t.Fatalf("slots = %d, want 20", len(got))
	}
	for _, s := range got {
		for _, m := range members {
			for _, b := range m.Busy {
				if s.Start.Before(b.End) && b.Start.Before(s.End) {
					t.Fatalf("slot %v overlaps busy %v", s, b)
				}
			}
		}
	}
}

func BenchmarkSearch31Days20Members(b *testing.B) {
	seoul := mustLoc(b, "Asia/Seoul")
	members := randomMembers(b, 20, 60, 42, true)
	p := Params{StartDate: date(2026, 11, 1), EndDate: date(2026, 12, 1), Duration: 30 * time.Minute, Location: seoul, MaxResults: 20, Now: ts(b, "2026-10-28T00:00:00Z")}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Search(members, p)
	}
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
