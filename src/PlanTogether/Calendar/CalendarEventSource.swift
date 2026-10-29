import EventKit
import Foundation

enum CalendarAuthorization: Equatable, Sendable {
    case notDetermined, fullAccess, writeOnly, denied, restricted
}

/// 읽기 후보 캘린더. identifier는 기기 안에서만 쓴다(서버에는 HMAC source key만 간다).
struct CalendarInfo: Identifiable, Equatable, Sendable {
    let id: String
    let title: String
    let sourceTitle: String
}

/// EventKit 경계. 테스트는 이 프로토콜의 가짜를 쓴다.
protocol CalendarEventSource: Sendable {
    func authorization() -> CalendarAuthorization
    /// 권한 요청 UI를 띄운다. 사용자가 연결을 누른 경우에만 부른다(설계 3 §5.1, 앱 활성화 때는 부르지 않는다).
    func requestFullAccess() async throws -> Bool
    func calendars() -> [CalendarInfo]
    /// 선택한 캘린더의 window 안 occurrence(반복은 펼친 것). 선택이 비면 아무것도 읽지 않는다.
    func occurrences(in window: SnapshotWindow, calendarIDs: Set<String>) -> [CalendarEventOccurrence]
}

final class EventKitEventSource: CalendarEventSource, @unchecked Sendable {
    private let store = EKEventStore()

    func authorization() -> CalendarAuthorization {
        switch EKEventStore.authorizationStatus(for: .event) {
        case .fullAccess, .authorized: return .fullAccess
        case .writeOnly: return .writeOnly
        case .denied: return .denied
        case .restricted: return .restricted
        case .notDetermined: return .notDetermined
        @unknown default: return .denied
        }
    }

    func requestFullAccess() async throws -> Bool {
        try await store.requestFullAccessToEvents()
    }

    func calendars() -> [CalendarInfo] {
        guard authorization() == .fullAccess else { return [] }
        store.refreshSourcesIfNecessary()
        return store.calendars(for: .event)
            .map { CalendarInfo(id: $0.calendarIdentifier, title: $0.title, sourceTitle: $0.source?.title ?? "") }
            .sorted { ($0.sourceTitle, $0.title) < ($1.sourceTitle, $1.title) }
    }

    func occurrences(in window: SnapshotWindow, calendarIDs: Set<String>) -> [CalendarEventOccurrence] {
        // predicateForEvents의 calendars가 nil(또는 빈 배열)이면 모든 캘린더를 읽는다. 선택하지 않은 캘린더는
        // 읽지 않는다(설계 3 §3.1).
        guard authorization() == .fullAccess, !calendarIDs.isEmpty else { return [] }
        let selected = store.calendars(for: .event).filter { calendarIDs.contains($0.calendarIdentifier) }
        guard !selected.isEmpty else { return [] }
        let predicate = store.predicateForEvents(withStart: window.start, end: window.end, calendars: selected)
        return store.events(matching: predicate).map { e in
            CalendarEventOccurrence(
                eventIdentifier: e.eventIdentifier ?? e.calendarItemIdentifier,
                occurrenceDate: e.occurrenceDate ?? e.startDate,
                start: e.startDate, end: e.endDate, isAllDay: e.isAllDay,
                timeZoneID: e.timeZone?.identifier,
                availability: Self.map(e.availability),
                isCanceled: e.status == .canceled)
        }
    }

    private static func map(_ a: EKEventAvailability) -> CalendarEventOccurrence.Availability {
        switch a {
        case .busy: return .busy
        case .free: return .free
        case .tentative: return .tentative
        case .unavailable: return .unavailable
        case .notSupported: return .notSupported
        @unknown default: return .busy
        }
    }

    #if DEBUG
    static let sampleCalendarTitle = "PlanTogether 시연"

    /// 시연용 일정을 "PlanTogether 시연" 캘린더 하나에만 만든다(DEBUG 빌드 전용). 다시 누르면 그 캘린더의 일정만
    /// 지우고 새로 만든다. 다른 캘린더는 건드리지 않는다. 읽기 대상으로 쓰려면 목록에서 직접 선택해야 한다.
    func createSampleEvents(now: Date = Date()) throws {
        guard authorization() == .fullAccess else { return }
        let calendar = try sampleCalendar()
        let cal = Calendar.current
        let today = cal.startOfDay(for: now)
        let horizon = cal.date(byAdding: .day, value: 120, to: today)!
        for e in store.events(matching: store.predicateForEvents(withStart: cal.date(byAdding: .day, value: -30, to: today)!, end: horizon, calendars: [calendar])) {
            try store.remove(e, span: .futureEvents, commit: false)
        }
        func at(_ day: Int, _ hour: Int) -> Date { cal.date(byAdding: .hour, value: hour, to: cal.date(byAdding: .day, value: day, to: today)!)! }
        func add(_ title: String, _ start: Date, _ end: Date, allDay: Bool = false, availability: EKEventAvailability = .busy, weekly: Bool = false) {
            let e = EKEvent(eventStore: store)
            e.calendar = calendar
            e.title = title
            e.startDate = start
            e.endDate = end
            e.isAllDay = allDay
            e.availability = availability
            if weekly { e.addRecurrenceRule(EKRecurrenceRule(recurrenceWith: .weekly, interval: 1, end: EKRecurrenceEnd(occurrenceCount: 4))) }
            try? store.save(e, span: .thisEvent, commit: false)
        }
        add("시연: 회의", at(1, 10), at(1, 11))
        add("시연: 주간 수업", at(2, 14), at(2, 15), weekly: true)
        add("시연: 하루 종일", at(3, 0), at(3, 0).addingTimeInterval(86_399), allDay: true)
        add("시연: 미정 약속", at(4, 19), at(4, 20), availability: .tentative)
        // 로컬 캘린더는 availability를 지원하지 않아 free·tentative도 Busy로 읽힌다(iCloud·Exchange는 구별한다).
        add("시연: 한가함 표시(지원 캘린더에서만 제외)", at(1, 15), at(1, 16), availability: .free)
        try store.commit()
    }

    private func sampleCalendar() throws -> EKCalendar {
        if let existing = store.calendars(for: .event).first(where: { $0.title == Self.sampleCalendarTitle }) { return existing }
        let c = EKCalendar(for: .event, eventStore: store)
        c.title = Self.sampleCalendarTitle
        guard let source = store.sources.first(where: { $0.sourceType == .local }) ?? store.defaultCalendarForNewEvents?.source else {
            throw CocoaError(.featureUnsupported)
        }
        c.source = source
        try store.saveCalendar(c, commit: true)
        return c
    }
    #endif
}
