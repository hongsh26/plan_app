import XCTest
@testable import PlanTogether

final class CalendarFactsTests: XCTestCase {
    private let keyer = CalendarKeyer(secret: Data(repeating: 7, count: 32))
    private let seoul = TimeZone(identifier: "Asia/Seoul")!
    private let newYork = TimeZone(identifier: "America/New_York")!

    private func date(_ s: String) -> Date { RFC3339.parse(s)! }

    private func occ(_ id: String, _ start: String, _ end: String, allDay: Bool = false, tz: String? = "Asia/Seoul",
                     availability: CalendarEventOccurrence.Availability = .busy, canceled: Bool = false, occurrence: String? = nil) -> CalendarEventOccurrence {
        CalendarEventOccurrence(eventIdentifier: id, occurrenceDate: date(occurrence ?? start), start: date(start), end: date(end),
                                isAllDay: allDay, timeZoneID: tz, availability: availability, isCanceled: canceled)
    }

    private var window: SnapshotWindow { SnapshotWindow.make(now: date("2026-10-29T03:00:00Z"), timeZone: seoul) }

    func testWindowIsUserTodayPlus90Days() {
        // 서울 2026-10-29 12:00 → 10-29 00:00(+09) ~ 2027-01-28 00:00(+09). 91일, 서버 상한 100일 이하.
        XCTAssertEqual(window.start, date("2026-10-28T15:00:00Z"))
        XCTAssertEqual(window.end, date("2027-01-27T15:00:00Z"))
        XCTAssertLessThanOrEqual(window.end.timeIntervalSince(window.start), 100 * 86_400)
    }

    func testClassification() {
        let facts = FactBuilder.build([
            occ("busy", "2026-10-30T01:00:00Z", "2026-10-30T02:00:00Z"),
            occ("tentative", "2026-10-30T03:00:00Z", "2026-10-30T04:00:00Z", availability: .tentative),
            occ("unavailable", "2026-10-30T05:00:00Z", "2026-10-30T06:00:00Z", availability: .unavailable),
            occ("unsupported", "2026-10-30T07:00:00Z", "2026-10-30T08:00:00Z", availability: .notSupported),
            occ("free", "2026-10-30T09:00:00Z", "2026-10-30T10:00:00Z", availability: .free),
            occ("canceled", "2026-10-30T11:00:00Z", "2026-10-30T12:00:00Z", canceled: true),
            occ("zero", "2026-10-30T13:00:00Z", "2026-10-30T13:00:00Z"),
            occ("outside", "2027-03-01T00:00:00Z", "2027-03-01T01:00:00Z"),
        ], window: window, userTimeZone: seoul, keyer: keyer)
        XCTAssertEqual(facts.map(\.availability).sorted(), ["busy", "busy", "tentative", "unavailable"])
    }

    func testRecurringOccurrencesGetDistinctStableKeys() {
        let a = occ("weekly", "2026-11-02T01:00:00Z", "2026-11-02T02:00:00Z")
        let b = occ("weekly", "2026-11-09T01:00:00Z", "2026-11-09T02:00:00Z")
        let facts = FactBuilder.build([a, b, a], window: window, userTimeZone: seoul, keyer: keyer)
        XCTAssertEqual(facts.count, 2, "같은 occurrence는 하나만")
        XCTAssertNotEqual(facts[0].sourceEventKey, facts[1].sourceEventKey)
        let again = FactBuilder.build([b, a], window: window, userTimeZone: seoul, keyer: keyer)
        XCTAssertEqual(again, facts, "입력 순서와 무관하게 같은 결과(재전송 바이트 동일)")
        // occurrenceDate 기준: 사용자가 occurrence 시각을 옮겨도 key는 그대로다.
        let moved = occ("weekly", "2026-11-09T05:00:00Z", "2026-11-09T06:00:00Z", occurrence: "2026-11-09T01:00:00Z")
        XCTAssertTrue(FactBuilder.build([moved], window: window, userTimeZone: seoul, keyer: keyer).map(\.sourceEventKey).contains(facts.first { $0.startAt == "2026-11-09T01:00:00Z" }!.sourceEventKey))
        // key는 16바이트 base64url이고 원래 identifier를 담지 않는다.
        XCTAssertEqual(facts[0].sourceEventKey.count, 22)
        XCTAssertFalse(facts[0].sourceEventKey.contains("weekly"))
        XCTAssertNotEqual(CalendarKeyer(secret: Data(repeating: 8, count: 32)).eventKey("weekly", occurrence: a.occurrenceDate), keyer.eventKey("weekly", occurrence: a.occurrenceDate))
    }

    func testAllDayCoversUserDayInUTC() {
        // EventKit all-day: 시작일 00:00 ~ 마지막 날 23:59:59(사용자 시간대).
        let facts = FactBuilder.build([occ("allday", "2026-11-04T15:00:00Z", "2026-11-06T14:59:59Z", allDay: true, tz: nil)],
                                      window: window, userTimeZone: seoul, keyer: keyer)
        XCTAssertEqual(facts.first?.startAt, "2026-11-04T15:00:00Z")  // 11-05 00:00 +09
        XCTAssertEqual(facts.first?.endAt, "2026-11-06T15:00:00Z")    // 11-07 00:00 +09 (이틀)
        XCTAssertEqual(facts.first?.allDay, true)
        XCTAssertEqual(facts.first?.timeZone, "Asia/Seoul")
    }

    func testAllDayOnDSTDayInNewYork() {
        let w = SnapshotWindow.make(now: date("2026-10-29T15:00:00Z"), timeZone: newYork)
        // 2026-11-01은 뉴욕 서머타임 종료일(25시간). 00:00 EDT(-4) ~ 다음 날 00:00 EST(-5).
        let facts = FactBuilder.build([occ("dst", "2026-11-01T04:00:00Z", "2026-11-02T04:59:59Z", allDay: true, tz: nil)],
                                      window: w, userTimeZone: newYork, keyer: keyer)
        XCTAssertEqual(facts.first?.startAt, "2026-11-01T04:00:00Z")
        XCTAssertEqual(facts.first?.endAt, "2026-11-02T05:00:00Z")
        XCTAssertEqual(w.start, date("2026-10-29T04:00:00Z"))
    }

    func testTimeZoneFallbacks() {
        let facts = FactBuilder.build([
            occ("floating", "2026-10-30T01:00:00Z", "2026-10-30T02:00:00Z", tz: nil),
            occ("weird", "2026-10-30T03:00:00Z", "2026-10-30T04:00:00Z", tz: "GMT+0900"),
            occ("ny", "2026-10-30T05:00:00Z", "2026-10-30T06:00:00Z", tz: "America/New_York"),
        ], window: window, userTimeZone: seoul, keyer: keyer)
        XCTAssertEqual(Set(facts.map(\.timeZone)), ["Asia/Seoul", "America/New_York"])
    }

    func testLongEventIsClippedToWindowAndSubsecondsDropped() {
        let facts = FactBuilder.build([
            occ("year", "2026-01-01T00:00:00Z", "2027-12-31T00:00:00Z"),
            CalendarEventOccurrence(eventIdentifier: "frac", occurrenceDate: date("2026-10-30T01:00:00Z"),
                                    start: date("2026-10-30T01:00:00.700Z"), end: date("2026-10-30T02:00:00.200Z"),
                                    isAllDay: false, timeZoneID: "Asia/Seoul", availability: .busy, isCanceled: false),
        ], window: window, userTimeZone: seoul, keyer: keyer)
        let year = facts.first { $0.startAt == RFC3339.format(window.start) }
        XCTAssertEqual(year?.endAt, RFC3339.format(window.end))
        XCTAssertTrue(facts.contains { $0.startAt == "2026-10-30T01:00:00Z" && $0.endAt == "2026-10-30T02:00:00Z" })
    }

    /// 페이지 JSON의 키는 allowlist뿐이다(제목·장소·identifier 없음).
    func testPageJSONContainsOnlyAllowlistedKeys() throws {
        let facts = FactBuilder.build([occ("a", "2026-10-30T01:00:00Z", "2026-10-30T02:00:00Z")], window: window, userTimeZone: seoul, keyer: keyer)
        let page = try XCTUnwrap(SnapshotPager.pages(facts).first)
        let obj = try XCTUnwrap(JSONSerialization.jsonObject(with: page) as? [String: Any])
        XCTAssertEqual(Set(obj.keys), ["facts"])
        let fact = try XCTUnwrap((obj["facts"] as? [[String: Any]])?.first)
        XCTAssertEqual(Set(fact.keys), ["source_event_key", "start_at", "end_at", "all_day", "time_zone", "availability"])
        XCTAssertFalse(String(data: page, encoding: .utf8)!.contains("\"a\""))
    }

    private func manyFacts(_ n: Int) -> [BusyFact] {
        (0..<n).map { i in
            BusyFact(sourceEventKey: keyer.eventKey("e\(i)", occurrence: Date(timeIntervalSince1970: 0)), startAt: "2026-10-30T01:00:00Z",
                     endAt: "2026-10-30T02:00:00Z", allDay: false, timeZone: "Asia/Seoul", availability: "busy")
        }
    }

    func testPagingBoundaries() throws {
        XCTAssertEqual(try SnapshotPager.pages([]).count, 0)
        XCTAssertEqual(try SnapshotPager.pages(manyFacts(250)).count, 1)
        let split = try SnapshotPager.pages(manyFacts(251))
        XCTAssertEqual(split.count, 2)
        for p in split {
            XCTAssertLessThanOrEqual(p.count, 65_536)
            let facts = try XCTUnwrap((JSONSerialization.jsonObject(with: p) as? [String: Any])?["facts"] as? [Any])
            XCTAssertLessThanOrEqual(facts.count, 250)
        }
        let full = try SnapshotPager.pages(manyFacts(20_000))
        XCTAssertEqual(full.count, 80)
        XCTAssertTrue(full.allSatisfy { $0.count <= SnapshotPager.maxPageBytes })
        XCTAssertThrowsError(try SnapshotPager.pages(manyFacts(20_001))) {
            XCTAssertEqual($0 as? SnapshotPagerError, .tooManyFacts(20_001))
        }
    }
}
