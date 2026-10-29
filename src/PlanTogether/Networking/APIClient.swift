import Foundation

/// 요청 하나. mutation(GET이 아닌 요청)은 `Idempotency-Key`를 싣는다. 재전송(refresh 뒤 재시도, 오프라인 큐)은
/// 같은 키와 같은 본문 바이트를 그대로 보내야 서버가 같은 요청으로 본다(§7.4).
struct APIRequest: Sendable {
    var method: String
    var path: String
    var query: [URLQueryItem] = []
    var body: Data?
    /// nil이고 GET이 아니면 보낼 때 한 번 만든다. 큐에 넣는 요청은 미리 정해 둔다.
    var idempotencyKey: String?
    /// `If-Match: "<version>"`(강한 ETag).
    var ifMatchVersion: Int64?
    var requiresAuth = true

    static func get(_ path: String, query: [URLQueryItem] = []) -> APIRequest {
        APIRequest(method: "GET", path: path, query: query)
    }

    static func json<T: Encodable>(_ method: String, _ path: String, body: T, ifMatch: Int64? = nil) throws -> APIRequest {
        APIRequest(method: method, path: path, body: try JSONCoding.makeEncoder().encode(body), ifMatchVersion: ifMatch)
    }
}

struct APIResponse: Sendable {
    let status: Int
    let headers: [String: String]
    let data: Data

    func header(_ name: String) -> String? {
        headers.first { $0.key.caseInsensitiveCompare(name) == .orderedSame }?.value
    }
}

/// 서버 오류. 클라이언트는 HTTP 상태가 아니라 `code`로 분기한다(410이 sync_cursor_expired와 party_disbanded 두 뜻이다).
struct ServerError: Error, Equatable, Sendable {
    let status: Int
    let code: String
    let message: String
    let requestID: String?
    let currentVersion: Int64?
    /// 429·503의 `Retry-After`(초).
    let retryAfter: TimeInterval?
    /// 오류 응답 본문 원문. code별 추가 필드(예: 검색 409의 self)를 읽을 때 쓴다.
    var body: Data? = nil
}

enum APIError: Error, Equatable {
    case server(ServerError)
    /// 네트워크 오류. 세션은 그대로다.
    case transport(String)
    case decoding(String)
    /// 저장된 세션이 없다.
    case notSignedIn
    /// refresh가 401·423으로 거부됐거나 계정이 잠겼다. token을 지웠다.
    case sessionEnded

    var serverCode: String? {
        if case .server(let e) = self { return e.code }
        return nil
    }
}

/// 서버 API 클라이언트(§5.2, §6).
///
/// - 인증 요청이 401이면 refresh 한 번 뒤 같은 요청(같은 Idempotency-Key·본문)을 한 번만 다시 보낸다.
/// - refresh는 single-flight다. 이미 쓴 refresh token을 다시 보내면 서버가 재사용으로 보고 기기 전체를 폐기한다.
///   그래서 동시 401은 한 refresh를 기다리고, refresh 429만 같은 token으로 곧바로 재시도한다(요청 제한은 본문 해석
///   전이라 token이 소비되지 않았다). 시간 초과·네트워크 오류·5xx는 그 자리에서 다시 보내지 않고 오류만 돌려주며
///   로그아웃하지 않는다. **위험:** 서버가 회전을 커밋했는데 응답을 잃었으면, 다음 401에서 보내는 같은 token이
///   재사용으로 판정돼 기기가 폐기되고 다시 로그인해야 한다. 서버에 유예가 없어 클라이언트만으로는 피할 수 없다.
/// - 새 token은 메모리에 먼저 반영한다. Keychain 저장이 실패해도 소비된 refresh token을 다시 쓰지 않게 하고,
///   저장은 다음 요청 때 다시 시도한다.
/// - 세대(generation): 로그아웃·세션 종료마다 올린다. 그 전에 시작한 refresh의 결과는 버린다(로그아웃 뒤 token 부활 방지).
/// - 세션을 끝내는 것은 refresh 401 `invalid_session`과 어떤 요청이든 423 `account_locked`뿐이다.
actor APIClient {
    typealias Sleeper = @Sendable (TimeInterval) async throws -> Void

    let baseURL: URL
    private let session: URLSession
    private let tokenStore: TokenStore
    private let sleep: Sleeper
    private var tokens: AuthTokens?
    private var refreshTask: Task<AuthTokens, Error>?
    private let sessionEnded = HandlerBox()
    private var generation = 0
    /// 새 token의 Keychain 저장이 실패해 다시 저장해야 한다.
    private var needsTokenSave = false
    /// refresh 429 재시도 상한. 넘으면 세션을 유지한 채 rate_limited 오류를 돌려준다.
    private let maxRefreshAttempts = 5

    init(baseURL: URL, session: URLSession = .shared, tokenStore: TokenStore,
         sleep: @escaping Sleeper = { try await Task.sleep(nanoseconds: UInt64(max(0, $0) * 1_000_000_000)) }) {
        self.baseURL = baseURL
        self.session = session
        self.tokenStore = tokenStore
        self.sleep = sleep
        self.tokens = try? tokenStore.load()
    }

    var currentTokens: AuthTokens? { tokens }

    /// 서버가 세션을 끝냈을 때(refresh 401·423, 요청 423) 부른다. 동기 등록이라 첫 요청 전에 걸 수 있다.
    nonisolated func setSessionEndedHandler(_ handler: @escaping @Sendable () async -> Void) {
        sessionEnded.set(handler)
    }

    // MARK: 인증

    /// `POST /v1/auth/apple`. 개발용 로그인은 identity token `dev:<이름>`이다(server DEV_LOGIN_ENABLED).
    /// 로그인할 때마다 서버가 새 기기를 만든다. 별도 기기 등록 endpoint는 없다.
    func signIn(_ request: SignInRequest) async throws -> AuthTokens {
        var req = try APIRequest.json("POST", "/v1/auth/apple", body: request)
        req.requiresAuth = false
        let response = try await perform(req, accessToken: nil)
        guard response.status == 200 else { throw error(from: response) }
        let fresh = AuthTokens(try decode(SessionResponse.self, response.data))
        try tokenStore.save(fresh)
        tokens = fresh
        return fresh
    }

    /// 현재 기기 세션을 서버에서 폐기하고 로컬 token을 지운다. 서버 호출이 실패해도 로컬은 지운다.
    /// 로컬을 먼저 지우고 세대를 올린 뒤 서버 호출을 한 번만 시도한다(refresh하지 않는다). 진행 중이던 refresh가
    /// 끝나도 결과를 저장하지 않는다.
    func logout() async {
        let old = tokens
        generation += 1
        tokens = nil
        refreshTask = nil
        needsTokenSave = false
        try? tokenStore.clear()
        if let old {
            var req = APIRequest(method: "POST", path: "/v1/auth/logout")
            req.idempotencyKey = UUID().uuidString
            _ = try? await perform(req, accessToken: old.accessToken)
        }
    }

    // MARK: 요청

    /// 요청을 보내고 2xx가 아니면 `APIError.server`를 던진다.
    func send(_ request: APIRequest) async throws -> APIResponse {
        var req = request
        if req.method != "GET", req.idempotencyKey == nil { req.idempotencyKey = UUID().uuidString }
        guard req.requiresAuth else {
            let response = try await perform(req, accessToken: nil)
            guard (200..<300).contains(response.status) else { throw error(from: response) }
            return response
        }
        guard let used = tokens else { throw APIError.notSignedIn }
        // 요청을 시작한 세대. 재로그인을 지나 도착한 옛 응답의 423이 새 세션을 끝내지 않게 한다.
        let gen = generation
        if needsTokenSave, (try? tokenStore.save(used)) != nil { needsTokenSave = false }
        var response = try await perform(req, accessToken: used.accessToken)
        if response.status == 401 {
            let fresh = try await refreshed(after: used.accessToken)
            response = try await perform(req, accessToken: fresh.accessToken)
        }
        if response.status == 423 {
            await endSession(gen)
            throw APIError.sessionEnded
        }
        guard (200..<300).contains(response.status) else { throw error(from: response) }
        return response
    }

    func send<T: Decodable>(_ request: APIRequest, as type: T.Type) async throws -> T {
        try decode(type, try await send(request).data)
    }

    // MARK: refresh

    /// 실패한 access token 기준 single-flight. 그사이 다른 요청이 이미 회전했으면 새 token을 그대로 쓴다.
    private func refreshed(after failedAccessToken: String) async throws -> AuthTokens {
        if let current = tokens, current.accessToken != failedAccessToken { return current }
        if let running = refreshTask { return try await running.value }
        guard let refreshToken = tokens?.refreshToken else { throw APIError.notSignedIn }
        let gen = generation
        let task = Task { try await self.performRefresh(refreshToken, generation: gen) }
        refreshTask = task
        // 로그아웃·재로그인 뒤 새 세대의 refresh를 지우지 않게 자기 task일 때만 비운다.
        defer { if refreshTask == task { refreshTask = nil } }
        return try await task.value
    }

    private func performRefresh(_ refreshToken: String, generation gen: Int) async throws -> AuthTokens {
        var req = try APIRequest.json("POST", "/v1/auth/refresh", body: RefreshRequest(refreshToken: refreshToken))
        req.requiresAuth = false
        req.idempotencyKey = nil
        var attempt = 0
        while true {
            attempt += 1
            let response = try await perform(req, accessToken: nil)
            // 기다리는 사이 로그아웃·세션 종료가 있었으면 결과를 버린다.
            guard generation == gen else { throw APIError.sessionEnded }
            switch response.status {
            case 200:
                let fresh = AuthTokens(try decode(SessionResponse.self, response.data))
                // 메모리에 먼저 반영한다. 저장 실패로 소비된 refresh token을 다시 보내면 기기가 폐기된다.
                tokens = fresh
                do {
                    try tokenStore.save(fresh)
                    needsTokenSave = false
                } catch {
                    needsTokenSave = true
                }
                return fresh
            case 429:
                // 세션 상실이 아니다(progressing.md iOS 계약). Retry-After 뒤 같은 token으로 다시 보낸다.
                let err = error(from: response)
                guard attempt < maxRefreshAttempts else { throw err }
                try await sleep(retryAfter(of: err) ?? 1)
            case 401, 423:
                await endSession(gen)
                throw APIError.sessionEnded
            default:
                throw error(from: response)
            }
        }
    }

    /// 그 세대가 아직 현재일 때만 끝낸다(이미 로그아웃했으면 아무것도 하지 않는다).
    private func endSession(_ gen: Int) async {
        guard generation == gen else { return }
        generation += 1
        tokens = nil
        refreshTask = nil
        needsTokenSave = false
        try? tokenStore.clear()
        if let handler = sessionEnded.get() { await handler() }
    }

    // MARK: 전송

    private func perform(_ req: APIRequest, accessToken: String?) async throws -> APIResponse {
        var components = URLComponents(url: baseURL.appendingPathComponent(req.path), resolvingAgainstBaseURL: false)
        if !req.query.isEmpty { components?.queryItems = req.query }
        guard let url = components?.url else { throw APIError.transport("잘못된 URL: \(req.path)") }
        var urlRequest = URLRequest(url: url)
        urlRequest.httpMethod = req.method
        urlRequest.httpBody = req.body
        urlRequest.setValue("application/json", forHTTPHeaderField: "Accept")
        if req.body != nil { urlRequest.setValue("application/json", forHTTPHeaderField: "Content-Type") }
        if let accessToken { urlRequest.setValue("Bearer \(accessToken)", forHTTPHeaderField: "Authorization") }
        if let key = req.idempotencyKey { urlRequest.setValue(key, forHTTPHeaderField: "Idempotency-Key") }
        if let v = req.ifMatchVersion { urlRequest.setValue("\"\(v)\"", forHTTPHeaderField: "If-Match") }
        do {
            let (data, response) = try await session.data(for: urlRequest)
            guard let http = response as? HTTPURLResponse else { throw APIError.transport("HTTP 응답이 아니다") }
            var headers: [String: String] = [:]
            for (k, v) in http.allHeaderFields {
                if let k = k as? String, let v = v as? String { headers[k] = v }
            }
            return APIResponse(status: http.statusCode, headers: headers, data: data)
        } catch let e as APIError {
            throw e
        } catch {
            throw APIError.transport(error.localizedDescription)
        }
    }

    private func decode<T: Decodable>(_ type: T.Type, _ data: Data) throws -> T {
        do {
            return try JSONCoding.makeDecoder().decode(type, from: data)
        } catch {
            throw APIError.decoding(String(describing: error))
        }
    }

    private func error(from response: APIResponse) -> APIError {
        let body = try? JSONCoding.makeDecoder().decode(ErrorBody.self, from: response.data)
        return .server(ServerError(
            status: response.status,
            code: body?.code ?? "http_\(response.status)",
            message: body?.message ?? "",
            requestID: body?.requestID ?? response.header("X-Request-ID"),
            currentVersion: body?.currentVersion,
            retryAfter: Self.parseRetryAfter(response.header("Retry-After")),
            body: response.data
        ))
    }

    private func retryAfter(of error: APIError) -> TimeInterval? {
        if case .server(let e) = error { return e.retryAfter }
        return nil
    }

    /// 초 단위 정수 또는 HTTP 날짜.
    static func parseRetryAfter(_ raw: String?) -> TimeInterval? {
        guard let raw = raw?.trimmingCharacters(in: .whitespaces), !raw.isEmpty else { return nil }
        if let seconds = TimeInterval(raw) { return max(0, seconds) }
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(identifier: "GMT")
        f.dateFormat = "EEE, dd MMM yyyy HH:mm:ss zzz"
        guard let date = f.date(from: raw) else { return nil }
        return max(0, date.timeIntervalSinceNow)
    }
}

/// 세션 종료 handler. actor 밖에서 동기로 등록하려고 잠금으로 보호한다.
final class HandlerBox: @unchecked Sendable {
    private let lock = NSLock()
    private var handler: (@Sendable () async -> Void)?

    func set(_ h: @escaping @Sendable () async -> Void) {
        lock.lock(); defer { lock.unlock() }
        handler = h
    }

    func get() -> (@Sendable () async -> Void)? {
        lock.lock(); defer { lock.unlock() }
        return handler
    }
}
