import Combine
import Foundation

// 활동 시간(설계 6 §3, rec 2026-10-28_1404). 요일별 0~3구간, 30분 경계, 자정을 넘으면 end_minute > 1440.

struct WeeklyInterval: Codable, Equatable, Hashable, Sendable {
    /// ISO 요일 1=월 … 7=일.
    var weekday: Int
    var startMinute: Int
    /// 1440 초과는 다음 요일로 이어진다(입력에서만. 서버 응답은 두 구간으로 나뉘어 온다).
    var endMinute: Int

    enum CodingKeys: String, CodingKey {
        case weekday
        case startMinute = "start_minute"
        case endMinute = "end_minute"
    }
}

struct AvailabilityPreferences: Decodable, Equatable, Sendable {
    let status: String          // unconfigured | configured
    let timeZone: String?
    let version: Int64
    let weeklyIntervals: [WeeklyInterval]

    enum CodingKeys: String, CodingKey {
        case status, version
        case timeZone = "time_zone"
        case weeklyIntervals = "weekly_intervals"
    }
}

/// 화면에서 고치는 구간 하나. 시작·끝은 그날 0:00부터의 분(30분 단위). 끝 ≤ 시작이면 다음 날로 넘어간다(예 22:00~02:00).
struct EditableInterval: Identifiable, Equatable, Hashable, Sendable {
    let id: UUID
    var startMinute: Int
    var endMinute: Int

    init(id: UUID = UUID(), start: Int, end: Int) {
        self.id = id
        startMinute = start
        endMinute = end
    }

    var crossesMidnight: Bool { endMinute <= startMinute }
}

enum PreferencesRules {
    static let maxPerDay = 3
    /// 저장 전 제안값(설계 6 §3.1). 저장해야 효력이 있다.
    static let suggestion = EditableInterval(start: 9 * 60, end: 22 * 60)
    static let weekdays = [1, 2, 3, 4, 5, 6, 7]
    static let weekdayNames = [1: "월", 2: "화", 3: "수", 4: "목", 5: "금", 6: "토", 7: "일"]

    /// 서버 ValidTimeZone과 같은 모양 검사(Area/City, Etc/·posix/·right/·SystemV/ 제외). 서버는 내장 tzdata 목록과도
    /// 대조한다. 여기서 통과해도 서버가 time_zone_invalid로 거부하면 도시 선택을 띄운다.
    static func isRegionZone(_ id: String) -> Bool {
        guard id.count <= 64, id.range(of: #"^[A-Z][A-Za-z_]+(/[A-Za-z0-9_+\-]+){1,2}$"#, options: .regularExpression) != nil else { return false }
        let lower = id.lowercased()
        return !["etc/", "posix/", "right/", "systemv/"].contains { lower.hasPrefix($0) }
    }

    /// 도시 선택 후보(기기 tz DB의 지역 시간대).
    static var regionZones: [String] { TimeZone.knownTimeZoneIdentifiers.filter(isRegionZone).sorted() }

    /// 화면 구간을 서버 입력으로 바꾼다. 끝 ≤ 시작이면 end + 1440(다음 날로 이어짐).
    static func wire(_ days: [Int: [EditableInterval]]) -> [WeeklyInterval] {
        weekdays.flatMap { day in
            (days[day] ?? []).sorted { $0.startMinute < $1.startMinute }.map {
                WeeklyInterval(weekday: day, startMinute: $0.startMinute, endMinute: $0.crossesMidnight ? $0.endMinute + 1440 : $0.endMinute)
            }
        }
    }

    /// 서버 정규형을 화면 구간으로. 응답은 이미 자정에서 나뉘어 있어 그대로 보인다(끝 1440은 24:00 = 다음 날 0:00).
    static func editable(_ intervals: [WeeklyInterval]) -> [Int: [EditableInterval]] {
        var days: [Int: [EditableInterval]] = [:]
        for i in intervals {
            days[i.weekday, default: []].append(EditableInterval(start: i.startMinute, end: i.endMinute % 1440))
        }
        return days
    }

    /// 저장 전 검사(서버가 최종 판정한다). 오류 문구 또는 nil.
    static func validate(_ days: [Int: [EditableInterval]]) -> String? {
        let all = days.values.flatMap { $0 }
        if all.isEmpty { return "최소 한 요일에 활동 시간을 넣어 주세요." }
        for (day, list) in days {
            if list.count > maxPerDay { return "\(weekdayNames[day] ?? "")요일 구간은 최대 \(maxPerDay)개입니다." }
            for i in list where i.startMinute % 30 != 0 || i.endMinute % 30 != 0 {
                return "시간은 30분 단위로 고를 수 있습니다."
            }
            // 끝 ≤ 시작은 다음 날로 이어진다(같으면 24시간). 서버 상한도 24시간이다.
            let sorted = list.map { ($0.startMinute, $0.crossesMidnight ? $0.endMinute + 1440 : $0.endMinute) }.sorted { $0.0 < $1.0 }
            for (a, b) in zip(sorted, sorted.dropFirst()) where b.0 < a.1 {
                return "\(weekdayNames[day] ?? "")요일 구간이 겹칩니다."
            }
        }
        return nil
    }

    static func label(_ minute: Int) -> String {
        let m = ((minute % 1440) + 1440) % 1440
        return String(format: "%02d:%02d", m / 60, m % 60)
    }
}

/// 활동 시간 화면 상태.
@MainActor
final class PreferencesViewModel: ObservableObject {
    @Published var days: [Int: [EditableInterval]] = [:]
    @Published var timeZone: String
    @Published private(set) var status: String = "unconfigured"
    @Published private(set) var version: Int64 = 0
    /// 저장하지 않은 제안값을 보여 주는 중이다.
    @Published private(set) var isSuggestion = false
    @Published var needsCityPicker = false
    @Published private(set) var message: String?
    @Published private(set) var isBusy = false
    /// 서버 값을 한 번이라도 읽었다. 첫 읽기가 실패하면 화면에 다시 들어올 때·다시 불러오기에서 다시 읽는다.
    @Published private(set) var loaded = false

    private let api: APIClient
    /// create-only 재전송에 같은 Idempotency-Key를 쓰려고 보관한다(rec 1404 iOS 계약).
    private var pendingCreateKey: String?

    init(api: APIClient, deviceTimeZone: String = TimeZone.current.identifier) {
        self.api = api
        self.timeZone = deviceTimeZone
        needsCityPicker = !PreferencesRules.isRegionZone(deviceTimeZone)
    }

    /// 읽기에 성공하면 true.
    @discardableResult
    func load() async -> Bool {
        isBusy = true
        defer { isBusy = false }
        do {
            struct Envelope: Decodable { let preferences: AvailabilityPreferences }
            let p = try await api.send(.get("/v1/availability/preferences/me"), as: Envelope.self).preferences
            apply(p)
            message = nil
            loaded = true
            return true
        } catch {
            message = UserMessage.of(error)
            return false
        }
    }

    func apply(_ p: AvailabilityPreferences) {
        status = p.status
        version = p.version
        if p.status == "configured" {
            days = PreferencesRules.editable(p.weeklyIntervals)
            if let tz = p.timeZone { timeZone = tz }
            isSuggestion = false
        } else {
            // 미설정: 매일 09:00~22:00 제안. 저장해야 효력이 있다.
            days = Dictionary(uniqueKeysWithValues: PreferencesRules.weekdays.map { ($0, [EditableInterval(start: 540, end: 1320)]) })
            isSuggestion = true
        }
        needsCityPicker = !PreferencesRules.isRegionZone(timeZone)
    }

    func addInterval(_ day: Int) {
        guard (days[day]?.count ?? 0) < PreferencesRules.maxPerDay else { return }
        days[day, default: []].append(EditableInterval(start: 18 * 60, end: 20 * 60))
    }

    func removeInterval(_ day: Int, _ id: UUID) {
        days[day]?.removeAll { $0.id == id }
    }

    /// 저장. 처음이면 If-Match 없이(create-only), 이후는 현재 version. 409면 다시 읽어 보여 준다.
    @discardableResult
    func save() async -> Bool {
        if let error = PreferencesRules.validate(days) {
            message = error
            return false
        }
        guard PreferencesRules.isRegionZone(timeZone) else {
            needsCityPicker = true
            message = "이 시간대는 쓸 수 없습니다. 도시를 선택하세요."
            return false
        }
        struct Body: Encodable {
            let timeZone: String
            let weeklyIntervals: [WeeklyInterval]
            enum CodingKeys: String, CodingKey {
                case timeZone = "time_zone"
                case weeklyIntervals = "weekly_intervals"
            }
        }
        isBusy = true
        defer { isBusy = false }
        do {
            var req = try APIRequest.json("PUT", "/v1/availability/preferences/me",
                                          body: Body(timeZone: timeZone, weeklyIntervals: PreferencesRules.wire(days)),
                                          ifMatch: version > 0 ? version : nil)
            if version == 0 {
                let key = pendingCreateKey ?? UUID().uuidString
                pendingCreateKey = key
                req.idempotencyKey = key
            }
            struct Envelope: Decodable { let preferences: AvailabilityPreferences }
            let p = try await api.send(req, as: Envelope.self).preferences
            pendingCreateKey = nil
            apply(p)
            message = "저장했습니다."
            return true
        } catch let e as APIError where e.serverCode == "version_conflict" || e.serverCode == "idempotency_mismatch" {
            // idempotency_mismatch: 첫 저장이 반영됐는데 응답을 잃은 뒤 내용을 고쳐 같은 키로 보냈다. 키를 비우고 서버 값을
            // 다시 읽는다(그 뒤 저장은 If-Match로 간다).
            pendingCreateKey = nil
            await load()
            message = "서버에 저장된 값을 다시 불러왔습니다. 확인 후 다시 저장하세요."
            return false
        } catch let e as APIError where e.serverCode == "time_zone_invalid" {
            needsCityPicker = true
            message = UserMessage.of(e)
            return false
        } catch {
            message = UserMessage.of(error)
            return false
        }
    }
}
