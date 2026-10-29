import Foundation

/// 서버 JSON 규약(account_backend_design.md §6). 필드 이름은 모델마다 명시적 `CodingKeys`로 snake_case를 맞춘다.
/// `convertFromSnakeCase`는 `scope_membership_id`를 `scopeMembershipId`로 바꾸는 식이라 쓰지 않는다.
enum JSONCoding {
    static func makeDecoder() -> JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            let raw = try container.decode(String.self)
            guard let date = RFC3339.parse(raw) else {
                throw DecodingError.dataCorruptedError(in: container, debugDescription: "RFC 3339 시각이 아니다: \(raw)")
            }
            return date
        }
        return decoder
    }

    static func makeEncoder() -> JSONEncoder {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .custom { date, encoder in
            var container = encoder.singleValueContainer()
            try container.encode(RFC3339.format(date))
        }
        encoder.outputFormatting = [.sortedKeys]
        return encoder
    }
}

/// RFC 3339 시각. 서버는 초 단위(`2026-10-29T05:12:33Z`)와 Go `time.Time` 기본 직렬화(소수 초 최대 9자리,
/// 시간대 오프셋 가능)를 섞어 보낸다. 소수 초는 밀리초까지만 남기고 해석한다.
enum RFC3339 {
    static func parse(_ raw: String) -> Date? {
        let normalized = trimFraction(raw)
        if let date = withFraction().date(from: normalized) { return date }
        return plain().date(from: normalized)
    }

    static func format(_ date: Date) -> String {
        plain().string(from: date)
    }

    // ISO8601DateFormatter는 스레드 안전하지만 생성 비용이 있어 스레드별로 둘 필요는 없다. 매번 만들어도
    // 호출 빈도(응답당 몇 번)에서는 문제가 없고, 공유 상태를 두지 않아 동시성 검사에 걸리지 않는다.
    private static func withFraction() -> ISO8601DateFormatter {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }

    private static func plain() -> ISO8601DateFormatter {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }

    /// `.123456789` 같은 소수 초를 3자리로 자른다. 4자리 이상은 OS 버전에 따라 해석에 실패할 수 있다.
    private static func trimFraction(_ raw: String) -> String {
        guard let dot = raw.firstIndex(of: ".") else { return raw }
        var end = raw.index(after: dot)
        while end < raw.endIndex, raw[end].isASCII, raw[end].isNumber { end = raw.index(after: end) }
        let digits = raw[raw.index(after: dot)..<end]
        guard !digits.isEmpty else { return raw }
        let kept = String(digits.prefix(3)).padding(toLength: 3, withPad: "0", startingAt: 0)
        return String(raw[..<dot]) + "." + kept + String(raw[end...])
    }
}

/// sync payload처럼 모양이 entity type마다 다른 JSON 값. Bool과 숫자를 구분해 보존한다.
enum JSONValue: Codable, Equatable, Sendable {
    case null
    case bool(Bool)
    case number(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() {
            self = .null
        } else if let b = try? container.decode(Bool.self) {
            self = .bool(b)
        } else if let n = try? container.decode(Double.self) {
            self = .number(n)
        } else if let s = try? container.decode(String.self) {
            self = .string(s)
        } else if let a = try? container.decode([JSONValue].self) {
            self = .array(a)
        } else {
            self = .object(try container.decode([String: JSONValue].self))
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .null: try container.encodeNil()
        case .bool(let b): try container.encode(b)
        case .number(let n): try container.encode(n)
        case .string(let s): try container.encode(s)
        case .array(let a): try container.encode(a)
        case .object(let o): try container.encode(o)
        }
    }

    subscript(key: String) -> JSONValue? {
        if case .object(let o) = self { return o[key] }
        return nil
    }

    var stringValue: String? {
        if case .string(let s) = self { return s }
        return nil
    }

    var boolValue: Bool? {
        if case .bool(let b) = self { return b }
        return nil
    }

    var numberValue: Double? {
        if case .number(let n) = self { return n }
        return nil
    }
}
