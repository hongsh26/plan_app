import Foundation

// Party 화면용 값(설계 4). 목록·상세는 sync 캐시(SyncState)에서 만들고, 생성·초대·수락·공개 수준은 API를 직접 부른다.

struct MemberSummary: Identifiable, Equatable, Sendable {
    let id: String          // membership id
    let userID: String
    let displayName: String
    let role: String        // owner | member
}

struct PartySummary: Identifiable, Equatable, Sendable {
    let id: String
    let name: String
    let members: [MemberSummary]
    /// 호출자 본인의 활성 멤버십.
    let myMembership: MemberSummary?

    var isOwner: Bool { myMembership?.role == "owner" }
}

extension SyncState {
    /// 활성 Party와 활성 멤버(이름 순, 방장 먼저). 본인 멤버십이 없는 Party(부모보다 먼저 온 하위 entity 등)는 뺀다.
    func parties() -> [PartySummary] {
        var membersByParty: [String: [MemberSummary]] = [:]
        for (id, e) in entities(ofType: SyncEntityType.partyMembership) {
            guard e.payload["status"]?.stringValue == "active", let pid = e.payload["party_id"]?.stringValue else { continue }
            membersByParty[pid, default: []].append(MemberSummary(
                id: id, userID: e.payload["user_id"]?.stringValue ?? "",
                displayName: e.payload["display_name"]?.stringValue ?? "",
                role: e.payload["role"]?.stringValue ?? "member"))
        }
        return entities(ofType: SyncEntityType.party).compactMap { id, e in
            guard e.payload["status"]?.stringValue == "active" else { return nil }
            let members = (membersByParty[id] ?? []).sorted {
                ($0.role == "owner" ? 0 : 1, $0.displayName, $0.id) < ($1.role == "owner" ? 0 : 1, $1.displayName, $1.id)
            }
            guard let mine = members.first(where: { $0.userID == userID }) else { return nil }
            return PartySummary(id: id, name: e.payload["name"]?.stringValue ?? "", members: members, myMembership: mine)
        }
        .sorted { ($0.name, $0.id) < ($1.name, $1.id) }
    }

    /// 본인의 이 Party 공개 수준(sync 캐시). 서버가 본인 것만 보낸다.
    func myVisibility(party partyID: String) -> String? {
        entities(ofType: SyncEntityType.partyVisibilitySetting).first {
            $0.entity.payload["party_id"]?.stringValue == partyID && $0.entity.payload["user_id"]?.stringValue == userID
        }?.entity.payload["visibility_level"]?.stringValue
    }
}

/// 초대 token. AASA(Universal Link) 전이라 사용자가 링크나 코드를 붙여 넣는다. `https://…/i/{token}` 링크면 token만
/// 꺼낸다. token은 base64url 문자(A-Z a-z 0-9 - _)이고 서버 상한은 128자다.
enum InviteToken {
    static func parse(_ raw: String) -> String? {
        var text = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if let url = URL(string: text), url.scheme != nil {
            let parts = url.path.split(separator: "/")
            guard let i = parts.firstIndex(of: "i"), i + 1 < parts.count else { return nil }
            text = String(parts[i + 1])
        }
        let allowed = Set("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")
        guard (16...128).contains(text.count), text.allSatisfy(allowed.contains) else { return nil }
        return text
    }
}

struct InvitePreview: Decodable, Equatable, Sendable {
    let partyName: String
    let memberCount: Int
    let inviterDisplayName: String

    enum CodingKeys: String, CodingKey {
        case partyName = "party_name"
        case memberCount = "member_count"
        case inviterDisplayName = "inviter_display_name"
    }
}

struct CreatedInvite: Equatable, Sendable {
    let token: String
    let expiresAt: Date
    let maxUses: Int
}

struct VisibilityState: Equatable, Sendable {
    let level: String
    let version: Int64
}

/// Party API. 응답 본문 중 화면에 필요한 것만 읽는다(나머지는 sync로 받는다).
struct PartyAPI {
    let api: APIClient

    private struct PartyEnvelope: Decodable {
        struct Party: Decodable { let id: String }
        let party: Party
    }

    private struct InviteEnvelope: Decodable {
        struct Invite: Decodable {
            let token: String?
            let expiresAt: Date
            let maxUses: Int
            enum CodingKeys: String, CodingKey {
                case token
                case expiresAt = "expires_at"
                case maxUses = "max_uses"
            }
        }
        let invite: Invite
    }

    private struct PreviewEnvelope: Decodable { let preview: InvitePreview }

    private struct AcceptEnvelope: Decodable {
        struct Party: Decodable { let id: String }
        let party: Party
    }

    private struct VisibilityEnvelope: Decodable {
        struct Visibility: Decodable {
            let visibilityLevel: String
            let version: Int64
            enum CodingKeys: String, CodingKey {
                case version
                case visibilityLevel = "visibility_level"
            }
        }
        let visibility: Visibility
    }

    /// 이름만 필수다(설계 4 §3.1). 앞뒤 공백은 서버도 정리한다.
    func createParty(name: String) async throws -> String {
        try await api.send(try .json("POST", "/v1/parties", body: ["name": name]), as: PartyEnvelope.self).party.id
    }

    /// 기본 7일, 남은 정원만큼. token 원문은 이 응답에만 있다.
    func createInvite(partyID: String) async throws -> CreatedInvite {
        let req = APIRequest(method: "POST", path: "/v1/parties/\(partyID)/invites", body: Data("{}".utf8))
        let inv = try await api.send(req, as: InviteEnvelope.self).invite
        guard let token = inv.token else { throw APIError.decoding("초대 token이 없다") }
        return CreatedInvite(token: token, expiresAt: inv.expiresAt, maxUses: inv.maxUses)
    }

    func preview(token: String) async throws -> InvitePreview {
        try await api.send(.get("/v1/invites/\(token)/preview"), as: PreviewEnvelope.self).preview
    }

    /// 수락하면 Party ID를 돌려준다. 이미 멤버(409 already_member)면 호출자가 Party로 이동한다.
    func accept(token: String) async throws -> String {
        try await api.send(APIRequest(method: "POST", path: "/v1/invites/\(token)/accept"), as: AcceptEnvelope.self).party.id
    }

    func visibility(partyID: String) async throws -> VisibilityState {
        let v = try await api.send(.get("/v1/parties/\(partyID)/visibility/me"), as: VisibilityEnvelope.self).visibility
        return VisibilityState(level: v.visibilityLevel, version: v.version)
    }

    /// 공개 수준을 바꾼다. details는 I3에서 고를 수 없다(V5). share_location은 details가 아니면 false여야 한다.
    func setVisibility(partyID: String, level: String, ifMatch version: Int64) async throws -> VisibilityState {
        struct Body: Encodable {
            let visibilityLevel: String
            let shareLocation = false
            enum CodingKeys: String, CodingKey {
                case visibilityLevel = "visibility_level"
                case shareLocation = "share_location"
            }
        }
        let req = try APIRequest.json("PATCH", "/v1/parties/\(partyID)/visibility/me", body: Body(visibilityLevel: level), ifMatch: version)
        let v = try await api.send(req, as: VisibilityEnvelope.self).visibility
        return VisibilityState(level: v.visibilityLevel, version: v.version)
    }
}

/// 서버 오류 code를 사용자 문구로(설계 4 §8.3 등). 원인을 더 드러내지 않는다.
enum UserMessage {
    static func of(_ error: Error) -> String {
        guard let api = error as? APIError else { return "요청을 처리하지 못했습니다." }
        switch api {
        case .server(let e):
            switch e.code {
            case "not_found": return "초대가 유효하지 않거나 대상을 찾을 수 없습니다."
            case "party_full": return "정원이 찼습니다."
            case "already_member": return "이미 이 Party의 멤버입니다."
            case "party_invite_limit_reached": return "활성 초대가 너무 많습니다. 기존 초대를 쓰거나 무효화하세요."
            case "party_owned_limit_reached": return "만들 수 있는 Party 수를 넘었습니다."
            case "party_joined_limit_reached": return "참여할 수 있는 Party 수를 넘었습니다."
            case "party_disbanded": return "해산된 Party입니다."
            case "forbidden": return "권한이 없습니다."
            case "rate_limited": return "요청이 많습니다. \(Int(e.retryAfter ?? 60))초 뒤 다시 시도하세요."
            case "version_conflict": return "다른 곳에서 먼저 바뀌었습니다. 다시 불러왔으니 확인 후 다시 저장하세요."
            case "time_zone_invalid": return "이 시간대는 쓸 수 없습니다. 도시를 선택하세요."
            case "search_range_invalid": return "검색 기간이 너무 길거나 캘린더 동기화 범위를 벗어났습니다. 기간을 줄여 보세요."
            case "availability_temporarily_unavailable": return "잠시 후 다시 시도하세요."
            case "invalid_request": return "입력 값이 올바르지 않습니다."
            default: return "요청을 처리하지 못했습니다. (\(e.code))"
            }
        case .transport: return "연결할 수 없습니다. 네트워크를 확인하세요."
        case .sessionEnded, .notSignedIn: return "다시 로그인해 주세요."
        case .decoding: return "응답을 해석하지 못했습니다."
        }
    }
}
