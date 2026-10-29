import Foundation
import Security

/// 서버 세션(§5.2). access·refresh token, 서버가 발급한 user_id·device_id.
struct AuthTokens: Codable, Equatable, Sendable {
    var userID: String
    var deviceID: String
    var accessToken: String
    var accessTokenExpiresAt: Date
    var refreshToken: String
    var refreshTokenExpiresAt: Date

    init(_ response: SessionResponse) {
        userID = response.userID
        deviceID = response.deviceID
        accessToken = response.accessToken
        accessTokenExpiresAt = response.accessTokenExpiresAt
        refreshToken = response.refreshToken
        refreshTokenExpiresAt = response.refreshTokenExpiresAt
    }

    init(userID: String, deviceID: String, accessToken: String, accessTokenExpiresAt: Date,
         refreshToken: String, refreshTokenExpiresAt: Date) {
        self.userID = userID
        self.deviceID = deviceID
        self.accessToken = accessToken
        self.accessTokenExpiresAt = accessTokenExpiresAt
        self.refreshToken = refreshToken
        self.refreshTokenExpiresAt = refreshTokenExpiresAt
    }
}

/// token 저장소. token은 Keychain에만 두고 로그에 남기지 않는다(§5.2).
protocol TokenStore: Sendable {
    func load() throws -> AuthTokens?
    func save(_ tokens: AuthTokens) throws
    func clear() throws
}

enum TokenStoreError: Error, Equatable {
    case keychain(OSStatus)
}

/// Keychain generic password 항목 하나에 JSON으로 저장한다. 첫 잠금 해제 뒤 이 기기에서만 읽을 수 있다.
struct KeychainTokenStore: TokenStore {
    var service = "com.example.PlanTogether.session"
    var account = "tokens"

    private var query: [String: Any] {
        [kSecClass as String: kSecClassGenericPassword,
         kSecAttrService as String: service,
         kSecAttrAccount as String: account]
    }

    func load() throws -> AuthTokens? {
        var q = query
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = item as? Data else { throw TokenStoreError.keychain(status) }
        return try JSONCoding.makeDecoder().decode(AuthTokens.self, from: data)
    }

    func save(_ tokens: AuthTokens) throws {
        let data = try JSONCoding.makeEncoder().encode(tokens)
        let update: [String: Any] = [kSecValueData as String: data]
        var status = SecItemUpdate(query as CFDictionary, update as CFDictionary)
        if status == errSecItemNotFound {
            var add = query
            add[kSecValueData as String] = data
            add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
            status = SecItemAdd(add as CFDictionary, nil)
        }
        guard status == errSecSuccess else { throw TokenStoreError.keychain(status) }
    }

    func clear() throws {
        let status = SecItemDelete(query as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw TokenStoreError.keychain(status) }
    }
}

/// 테스트와 미리보기용.
final class InMemoryTokenStore: TokenStore, @unchecked Sendable {
    private let lock = NSLock()
    private var tokens: AuthTokens?
    private(set) var saveCount = 0

    init(_ tokens: AuthTokens? = nil) { self.tokens = tokens }

    func load() throws -> AuthTokens? {
        lock.lock(); defer { lock.unlock() }
        return tokens
    }

    func save(_ tokens: AuthTokens) throws {
        lock.lock(); defer { lock.unlock() }
        self.tokens = tokens
        saveCount += 1
    }

    func clear() throws {
        lock.lock(); defer { lock.unlock() }
        tokens = nil
    }
}
