import XCTest
@testable import PlanTogether

final class AvailabilityCalculatorTests: XCTestCase {
    func testExcludesBusyTimeAndMergesAdjacentFreeSegments() {
        let memberA = Member(id: UUID(), name: "A")
        let memberB = Member(id: UUID(), name: "B")
        let start = Date(timeIntervalSince1970: 0)
        let busy = BusyInterval(
            id: UUID(), ownerID: memberB.id,
            start: start.addingTimeInterval(3_600), end: start.addingTimeInterval(7_200), title: nil
        )

        let slots = AvailabilityCalculator.commonSlots(
            members: [memberA, memberB], syncedMemberIDs: [memberA.id, memberB.id],
            busyIntervals: [busy],
            searchRange: DateInterval(start: start, duration: 14_400), duration: 3_600
        )

        XCTAssertEqual(slots.count, 3)
        XCTAssertEqual(slots[0].start, start)
        XCTAssertEqual(slots[0].end, busy.start)
        XCTAssertEqual(slots[1].start, busy.end)
        XCTAssertEqual(slots[1].end, busy.end.addingTimeInterval(3_600))
        XCTAssertEqual(slots[2].start, busy.end.addingTimeInterval(3_600))
        XCTAssertEqual(slots[2].end, start.addingTimeInterval(14_400))
    }

    func testRejectsSlotShorterThanRequestedDuration() {
        let member = Member(id: UUID(), name: "A")
        let start = Date(timeIntervalSince1970: 0)
        let slots = AvailabilityCalculator.commonSlots(
            members: [member], syncedMemberIDs: [member.id],
            busyIntervals: [],
            searchRange: DateInterval(start: start, duration: 1_800), duration: 3_600
        )
        XCTAssertTrue(slots.isEmpty)
    }

    func testIgnoresIntervalsOutsideSearchRange() {
        let member = Member(id: UUID(), name: "A")
        let start = Date(timeIntervalSince1970: 10_000)
        let oldBusy = BusyInterval(
            id: UUID(), ownerID: member.id,
            start: start.addingTimeInterval(-7_200), end: start.addingTimeInterval(-3_600), title: nil
        )
        let slots = AvailabilityCalculator.commonSlots(
            members: [member], syncedMemberIDs: [member.id],
            busyIntervals: [oldBusy],
            searchRange: DateInterval(start: start, duration: 7_200), duration: 3_600
        )
        XCTAssertEqual(slots.count, 2)
    }

    func testReturnsNoSlotsUntilEveryMemberHasSynced() {
        let memberA = Member(id: UUID(), name: "A")
        let memberB = Member(id: UUID(), name: "B")
        let start = Date(timeIntervalSince1970: 0)
        let slots = AvailabilityCalculator.commonSlots(
            members: [memberA, memberB], syncedMemberIDs: [memberA.id], busyIntervals: [],
            searchRange: DateInterval(start: start, duration: 7_200), duration: 3_600
        )
        XCTAssertTrue(slots.isEmpty)
    }
}
