import EventKit
import Foundation

protocol CalendarServing {
    func requestAccess() async throws -> Bool
    func busyIntervals(in range: DateInterval, ownerID: UUID) async throws -> [BusyInterval]
}

final class EventKitCalendarService: CalendarServing {
    private let store = EKEventStore()

    func requestAccess() async throws -> Bool {
        try await store.requestFullAccessToEvents()
    }

    func busyIntervals(in range: DateInterval, ownerID: UUID) async throws -> [BusyInterval] {
        let predicate = store.predicateForEvents(withStart: range.start, end: range.end, calendars: nil)
        return store.events(matching: predicate).map {
            BusyInterval(id: UUID(), ownerID: ownerID, start: $0.startDate, end: $0.endDate, title: $0.title)
        }
    }
}
