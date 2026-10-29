import Combine
import Foundation

// 공통 가능 시간 검색(설계 6 §4·§6·§8, rec 2026-10-28_1401 표시 계약).

/// 검색 조건. 날짜는 검색 시간대의 달력 날짜다.
struct SearchForm: Equatable, Sendable {
    var startDate: Date
    var endDate: Date
    var durationMinutes: Int
    var timeZone: TimeZone

    static let defaultDays = 14
    static let maxDays = 31
    /// 서버: end_date ≤ 오늘 + 89일(멤버 snapshot 90일, 시간대 차이로 하루 여유).
    static let horizonDays = 89
    static let durations = Array(stride(from: 30, through: 480, by: 30))

    /// 기본: 오늘 포함 14일, 60분.
    static func defaults(now: Date, timeZone: TimeZone) -> SearchForm {
        let cal = SnapshotWindow.calendar(timeZone)
        let today = cal.startOfDay(for: now)
        return SearchForm(startDate: today, endDate: cal.date(byAdding: .day, value: defaultDays - 1, to: today)!, durationMinutes: 60, timeZone: timeZone)
    }

    /// 보내기 전 검사(서버가 최종 판정한다). 오류 문구 또는 nil.
    func validate(now: Date) -> String? {
        let cal = SnapshotWindow.calendar(timeZone)
        let today = cal.startOfDay(for: now)
        let start = cal.startOfDay(for: startDate), end = cal.startOfDay(for: endDate)
        if start < today { return "시작일은 오늘 이후여야 합니다." }
        if end < start { return "끝 날짜가 시작일보다 앞입니다." }
        if let days = cal.dateComponents([.day], from: start, to: end).day, days + 1 > Self.maxDays { return "기간은 최대 \(Self.maxDays)일입니다." }
        if let limit = cal.date(byAdding: .day, value: Self.horizonDays, to: today), end > limit { return "오늘부터 \(Self.horizonDays)일 안에서 고르세요." }
        if !Self.durations.contains(durationMinutes) { return "약속 길이는 30분~8시간, 30분 단위입니다." }
        if !PreferencesRules.isRegionZone(timeZone.identifier) { return "검색 시간대로 도시를 선택하세요." }
        return nil
    }

    /// 요청 본문(snake_case, 날짜는 YYYY-MM-DD).
    var body: [String: AnyEncodable] {
        ["start_date": AnyEncodable(Self.day(startDate, timeZone)), "end_date": AnyEncodable(Self.day(endDate, timeZone)),
         "duration_minutes": AnyEncodable(durationMinutes), "time_zone": AnyEncodable(timeZone.identifier), "max_results": AnyEncodable(20)]
    }

    static func day(_ d: Date, _ tz: TimeZone) -> String {
        let c = SnapshotWindow.calendar(tz).dateComponents([.year, .month, .day], from: d)
        return String(format: "%04d-%02d-%02d", c.year!, c.month!, c.day!)
    }
}

struct AnyEncodable: Encodable, Equatable, Sendable {
    private let value: String
    private let number: Int?
    init(_ s: String) { value = s; number = nil }
    init(_ n: Int) { value = ""; number = n }
    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        if let number { try c.encode(number) } else { try c.encode(value) }
    }
}

struct SlotRow: Identifiable, Equatable, Sendable {
    let id: String
    let start: Date
    let end: Date
    /// 예 "10:00–11:00 EDT". 약식 시간대가 DST로 두 번 나오는 같은 벽시계 시각을 구별한다.
    let label: String
}

struct DaySection: Identifiable, Equatable, Sendable {
    let id: String      // YYYY-MM-DD
    let title: String   // 예 "10월 30일 (금)"
    let slots: [SlotRow]
}

/// 결과 화면 상태. 준비 대기와 빈 결과를 구별한다(설계 6 §6: 준비 미완료를 빈 배열로 보이지 않는다).
enum SearchState: Equatable {
    case idle
    case loading
    case results([DaySection], validUntil: Date)
    /// 모든 준비 조건을 충족했지만 공통 시간이 없다.
    case empty
    /// 그룹 준비를 기다리는 중. self는 본인 상태(본인 문제일 때만 CTA).
    case pending(code: String, selfActivityUnconfigured: Bool, selfCalendarState: String)
    case failed(String)
}

enum SearchResultFormatter {
    /// 슬롯을 검색 시간대 날짜로 묶는다. 서버가 이미 날짜당 3개, 시작 순으로 준다.
    static func sections(_ slots: [(Date, Date)], timeZone: TimeZone, locale: Locale = Locale(identifier: "ko_KR")) -> [DaySection] {
        let dayTitle = DateFormatter()
        dayTitle.locale = locale
        dayTitle.timeZone = timeZone
        dayTitle.dateFormat = "M월 d일 (E)"
        var order: [String] = []
        var groups: [String: [SlotRow]] = [:]
        for (start, end) in slots.sorted(by: { $0.0 < $1.0 }) {
            let key = SearchForm.day(start, timeZone)
            if groups[key] == nil { order.append(key) }
            groups[key, default: []].append(SlotRow(id: "\(start.timeIntervalSince1970)", start: start, end: end,
                                                     label: label(start, end, timeZone)))
        }
        return order.map { key in DaySection(id: key, title: dayTitle.string(from: groups[key]![0].start), slots: groups[key]!) }
    }

    /// "10:00–11:00 EDT". 시작과 끝의 약식 시간대가 다르면(DST 전환을 걸침) 둘 다 쓴다: "01:30 EDT–01:00 EST".
    static func label(_ start: Date, _ end: Date, _ tz: TimeZone) -> String {
        let time = DateFormatter()
        time.locale = Locale(identifier: "en_US_POSIX")
        time.timeZone = tz
        time.dateFormat = "HH:mm"
        let a = abbreviation(tz, at: start), b = abbreviation(tz, at: end)
        return a == b ? "\(time.string(from: start))–\(time.string(from: end)) \(a)"
                      : "\(time.string(from: start)) \(a)–\(time.string(from: end)) \(b)"
    }

    /// 약식 시간대(EDT, EST, GMT+9 …).
    static func abbreviation(_ tz: TimeZone, at date: Date) -> String {
        tz.abbreviation(for: date) ?? tz.identifier
    }
}

/// 검색 화면 상태.
@MainActor
final class SearchViewModel: ObservableObject {
    @Published var form: SearchForm
    @Published private(set) var state: SearchState = .idle
    @Published private(set) var validationMessage: String?

    private let api: APIClient
    let partyID: String
    private let now: () -> Date

    init(api: APIClient, partyID: String, timeZone: TimeZone = .current, now: @escaping () -> Date = { Date() }) {
        self.api = api
        self.partyID = partyID
        self.now = now
        form = SearchForm.defaults(now: now(), timeZone: timeZone)
    }

    /// 결과가 만료됐다(valid_until 지남). 결과는 다시 검색해야 쓸 수 있다.
    var isExpired: Bool {
        if case .results(_, let until) = state { return now() >= until }
        return false
    }

    func search() async {
        if let error = form.validate(now: now()) {
            validationMessage = error
            return
        }
        validationMessage = nil
        state = .loading
        struct Response: Decodable {
            struct Slot: Decodable { let startAt: Date; let endAt: Date
                enum CodingKeys: String, CodingKey { case startAt = "start_at"; case endAt = "end_at" } }
            let slots: [Slot]
            let validUntil: Date
            enum CodingKeys: String, CodingKey { case slots; case validUntil = "valid_until" }
        }
        do {
            let req = try APIRequest.json("POST", "/v1/parties/\(partyID)/availability/search", body: form.body)
            // 검색은 mutation이 아니지만 POST라 APIClient가 Idempotency-Key를 싣는다. 서버는 쓰지 않는다.
            let response = try await api.send(req)
            let r = try JSONCoding.makeDecoder().decode(Response.self, from: response.data)
            if r.slots.isEmpty {
                state = .empty
            } else {
                state = .results(SearchResultFormatter.sections(r.slots.map { ($0.startAt, $0.endAt) }, timeZone: form.timeZone), validUntil: r.validUntil)
            }
        } catch APIError.server(let e) where e.status == 409 && (e.code == "availability_setup_pending" || e.code == "calendar_sync_pending") {
            state = Self.pendingState(e)
        } catch {
            state = .failed(UserMessage.of(error))
        }
    }

    /// 409 본문의 self는 호출자 본인 상태뿐이다(다른 멤버 수·원인은 없다).
    static func pendingState(_ e: ServerError) -> SearchState {
        struct Pending: Decodable {
            struct SelfState: Decodable { let activity: String; let calendar: String }
            let selfState: SelfState
            enum CodingKeys: String, CodingKey { case selfState = "self" }
        }
        let p = e.body.flatMap { try? JSONCoding.makeDecoder().decode(Pending.self, from: $0) }
        return .pending(code: e.code, selfActivityUnconfigured: p?.selfState.activity == "unconfigured",
                        selfCalendarState: p?.selfState.calendar ?? "ready")
    }
}
