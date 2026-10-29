import Foundation

// 서버 응답 모델(server/openapi/openapi.yaml). 필요한 필드만 둔다.

/// 로그인·refresh 응답(SessionResponse).
struct SessionResponse: Decodable, Equatable {
    let userID: String
    let deviceID: String
    let accessToken: String
    let accessTokenExpiresAt: Date
    let refreshToken: String
    let refreshTokenExpiresAt: Date
    let newUser: Bool

    enum CodingKeys: String, CodingKey {
        case userID = "user_id"
        case deviceID = "device_id"
        case accessToken = "access_token"
        case accessTokenExpiresAt = "access_token_expires_at"
        case refreshToken = "refresh_token"
        case refreshTokenExpiresAt = "refresh_token_expires_at"
        case newUser = "new_user"
    }
}

struct SignInRequest: Encodable {
    let identityToken: String
    let authorizationCode: String
    let rawNonce: String
    let displayName: String?

    enum CodingKeys: String, CodingKey {
        case identityToken = "identity_token"
        case authorizationCode = "authorization_code"
        case rawNonce = "raw_nonce"
        case displayName = "display_name"
    }
}

struct RefreshRequest: Encodable {
    let refreshToken: String

    enum CodingKeys: String, CodingKey {
        case refreshToken = "refresh_token"
    }
}

/// 오류 응답(§6). `current_version`은 version_conflict에서만 온다.
struct ErrorBody: Decodable, Equatable {
    let code: String
    let message: String
    let requestID: String?
    let currentVersion: Int64?

    enum CodingKeys: String, CodingKey {
        case code, message
        case requestID = "request_id"
        case currentVersion = "current_version"
    }
}

struct UserResource: Decodable, Equatable {
    let id: String
    let displayName: String
    let status: String
    let version: Int64

    enum CodingKeys: String, CodingKey {
        case id, status, version
        case displayName = "display_name"
    }
}

struct MeResponse: Decodable, Equatable {
    let user: UserResource
}

/// 변경 하나(SyncChange). bootstrap snapshot 항목도 같은 모양이다.
struct SyncChange: Codable, Equatable, Sendable {
    let entityType: String
    let entityID: String
    let operation: String
    let version: Int64?
    let payload: JSONValue?
    let scopeMembershipID: String?

    enum CodingKeys: String, CodingKey {
        case entityType = "entity_type"
        case entityID = "entity_id"
        case operation, version, payload
        case scopeMembershipID = "scope_membership_id"
    }
}

struct SyncPage: Decodable, Equatable {
    let changes: [SyncChange]
    let cursor: String
    let hasMore: Bool

    enum CodingKeys: String, CodingKey {
        case changes, cursor
        case hasMore = "has_more"
    }
}

/// bootstrap 응답. `notification_ref_key`는 알림 단계(설계 9)에서 Keychain에 둔다. I1에서는 읽지 않는다.
struct BootstrapResponse: Decodable, Equatable {
    let schemaVersion: Int
    let snapshot: [SyncChange]
    let cursorWatermark: String

    enum CodingKeys: String, CodingKey {
        case snapshot
        case schemaVersion = "schema_version"
        case cursorWatermark = "cursor_watermark"
    }
}
