import CryptoKit
import Foundation

// 캘린더 snapshot 변환(설계 3 §6, §7.2). EventKit을 모르는 순수 값 계층이라 단위 테스트로 검증한다.

/// EventKit 이벤트(반복은 occurrence 하나)에서 계산에 필요한 값만 옮긴 것. 제목·장소·메모·참석자·URL은 없다.
struct CalendarEventOccurrence: Equatable, Sendable {
    enum Availability: Equatable, Sendable { case busy, tentative, unavailable, free, notSupported }

    /// EventKit eventIdentifier. 기기 밖으로 나가지 않고 source_event_key의 HMAC 입력으로만 쓴다.
    let eventIdentifier: String
    /// 반복 occurrence를 구별한다(EKEvent.occurrenceDate). 반복이 아니면 시작 시각과 같다.
    let occurrenceDate: Date
    let start: Date
    let end: Date
    let isAllDay: Bool
    /// 이벤트 시간대. nil이면 floating(사용자 시간대로 해석).
    let timeZoneID: String?
    let availability: Availability
    let isCanceled: Bool
}

/// 서버로 보내는 fact. 이 필드가 allowlist 전부다(server calendar.factJSON). 제목·장소(details)는 V5에서 더한다.
struct BusyFact: Encodable, Equatable, Sendable {
    let sourceEventKey: String
    let startAt: String
    let endAt: String
    let allDay: Bool
    let timeZone: String
    let availability: String

    enum CodingKeys: String, CodingKey {
        case sourceEventKey = "source_event_key"
        case startAt = "start_at"
        case endAt = "end_at"
        case allDay = "all_day"
        case timeZone = "time_zone"
        case availability
    }
}

/// snapshot window: 사용자 시간대 오늘 00:00부터 90일 뒤 23:59까지(끝은 그다음 날 00:00, 배타). 서버 상한은 100일이다.
struct SnapshotWindow: Equatable, Sendable {
    let start: Date
    let end: Date

    static let days = 90

    static func make(now: Date, timeZone: TimeZone) -> SnapshotWindow {
        let cal = calendar(timeZone)
        let start = cal.startOfDay(for: now)
        let end = cal.date(byAdding: .day, value: days + 1, to: start)!
        return SnapshotWindow(start: start, end: end)
    }

    static func calendar(_ tz: TimeZone) -> Calendar {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = tz
        return cal
    }
}

/// EventKit identifier를 서버에 보낼 무작위 key로 바꾼다. 이 기기·사용자의 비밀 키로 만든 HMAC이라 서버는 원래 값을
/// 알 수 없고, 같은 occurrence는 snapshot마다 같은 key가 된다(key가 바뀌면 서버가 projection을 모두 다시 만든다).
struct CalendarKeyer: Sendable {
    let secret: SymmetricKey

    init(secret: Data) { self.secret = SymmetricKey(data: secret) }

    static func newSecret() -> Data {
        SymmetricKey(size: .bits256).withUnsafeBytes { Data($0) }
    }

    /// 16바이트 base64url(패딩 없음). 서버는 16~32바이트를 받는다.
    func eventKey(_ eventIdentifier: String, occurrence: Date) -> String {
        mac("event|\(eventIdentifier)|\(Int64(occurrence.timeIntervalSince1970.rounded(.down)))")
    }

    /// 읽기 source(캘린더) key. calendarIdentifier 원문은 서버로 보내지 않는다.
    func sourceKey(_ calendarIdentifier: String) -> String {
        mac("source|\(calendarIdentifier)")
    }

    private func mac(_ input: String) -> String {
        let code = HMAC<SHA256>.authenticationCode(for: Data(input.utf8), using: secret)
        return Data(code.prefix(16)).base64URLEncodedString()
    }
}

extension Data {
    func base64URLEncodedString() -> String {
        base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
}

enum FactBuilder {
    static let maxFactDuration: TimeInterval = 366 * 86_400

    /// occurrence를 fact로 바꾼다(설계 3 §6.2).
    /// - free·취소는 제외. busy·unavailable·tentative는 그대로, availability를 지원하지 않는 캘린더는 busy.
    /// - 참석자 응답은 보지 않는다(설계 3 §6.2: Launch MVP 계산에 쓰지 않는다).
    /// - all-day는 사용자 시간대의 하루 전체(시작일 00:00 ~ 마지막 날 다음 00:00)다. floating 시각도 사용자 시간대.
    /// - 끝이 시작 이후가 아니면 제외. 366일보다 긴 일정은 window로 자른다. window와 겹치지 않으면 제외.
    /// - 같은 key는 하나만 남기고 key 순으로 정렬한다. 같은 입력이면 같은 바이트가 나온다(page 재전송 안전).
    static func build(_ events: [CalendarEventOccurrence], window: SnapshotWindow, userTimeZone: TimeZone, keyer: CalendarKeyer) -> [BusyFact] {
        let userTZ = validIdentifier(userTimeZone.identifier) ?? "UTC"
        let cal = SnapshotWindow.calendar(TimeZone(identifier: userTZ) ?? .gmt)
        var byKey: [String: BusyFact] = [:]
        for e in events {
            guard !e.isCanceled, let availability = wireAvailability(e.availability) else { continue }
            var start: Date
            var end: Date
            let tz: String
            if e.isAllDay {
                start = cal.startOfDay(for: e.start)
                let lastDay = cal.startOfDay(for: max(e.start, e.end.addingTimeInterval(-1)))
                end = cal.date(byAdding: .day, value: 1, to: lastDay)!
                tz = userTZ
            } else {
                start = e.start
                end = e.end
                tz = e.timeZoneID.flatMap(validIdentifier) ?? userTZ
            }
            start = floorSecond(start)
            end = floorSecond(end)
            if end.timeIntervalSince(start) > maxFactDuration {
                start = max(start, window.start)
                end = min(end, window.end)
            }
            guard end > start, end > window.start, start < window.end else { continue }
            let key = keyer.eventKey(e.eventIdentifier, occurrence: e.occurrenceDate)
            byKey[key] = BusyFact(sourceEventKey: key, startAt: RFC3339.format(start), endAt: RFC3339.format(end),
                                  allDay: e.isAllDay, timeZone: tz, availability: availability)
        }
        return byKey.values.sorted { $0.sourceEventKey < $1.sourceEventKey }
    }

    static func wireAvailability(_ a: CalendarEventOccurrence.Availability) -> String? {
        switch a {
        case .busy, .notSupported: return "busy"
        case .tentative: return "tentative"
        case .unavailable: return "unavailable"
        case .free: return nil
        }
    }

    /// 서버(Go time.LoadLocation)가 받는 IANA 이름만 쓴다. "GMT+0900" 같은 이름은 page 전체를 400으로 만든다.
    static func validIdentifier(_ id: String) -> String? {
        guard id != "Local", id.count <= 64 else { return nil }
        if id == "UTC" || id == "GMT" { return id }
        return TimeZone.knownTimeZoneIdentifiers.contains(id) ? id : nil
    }

    private static func floorSecond(_ d: Date) -> Date {
        Date(timeIntervalSince1970: d.timeIntervalSince1970.rounded(.down))
    }
}

enum SnapshotPagerError: Error, Equatable {
    /// 서버 상한(20,000 facts, 200 page)을 넘는다. 잘라서 올리면 빠진 일정이 빈 시간으로 보이므로 올리지 않는다.
    case tooManyFacts(Int)
}

/// fact 목록을 page 본문으로 나눈다. page당 250 facts, 본문 64KB(여유를 두고 60,000바이트) 이하.
enum SnapshotPager {
    static let maxFactsPerPage = 250
    static let maxPageBytes = 60_000
    static let maxPages = 200
    static let maxTotalFacts = 20_000

    static func pages(_ facts: [BusyFact]) throws -> [Data] {
        guard facts.count <= maxTotalFacts else { throw SnapshotPagerError.tooManyFacts(facts.count) }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let head = Data(#"{"facts":["#.utf8)
        let tail = Data("]}".utf8)
        var pages: [Data] = []
        var current: [Data] = []
        var size = head.count + tail.count
        func flush() {
            var body = head
            for (i, f) in current.enumerated() {
                if i > 0 { body.append(UInt8(ascii: ",")) }
                body.append(f)
            }
            body.append(tail)
            pages.append(body)
            current = []
            size = head.count + tail.count
        }
        for fact in facts {
            let encoded = try encoder.encode(fact)
            let added = encoded.count + (current.isEmpty ? 0 : 1)
            if !current.isEmpty, current.count == maxFactsPerPage || size + added > maxPageBytes { flush() }
            size += encoded.count + (current.isEmpty ? 0 : 1)
            current.append(encoded)
        }
        if !current.isEmpty { flush() }
        guard pages.count <= maxPages else { throw SnapshotPagerError.tooManyFacts(facts.count) }
        return pages
    }
}
