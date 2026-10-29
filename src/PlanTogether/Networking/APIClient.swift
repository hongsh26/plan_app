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
///   그래서 동시 401은 한 refresh를 기다리고, refresh 429만 같은 token으로 재시도한다(요청 제한은 본문 해석 전이라
///   token이 소비되지 않았다). 시간 초과·네트워크 오류·5xx는 서버가 token을 소비했는지 알 수 없으므로 자동으로
///   다시 보내지 않고 오류만 돌려준다. 로그아웃하지 않는다.
/// - 세션을 끝내는 것은 refresh 401 `invalid_session`과 어떤 요청이든 423 `account_locked`뿐이다.
actor APIClient {
    typealias Sleeper = @Sendable (TimeInterval) async throws -> Void

    let baseURL: URL
    private let session: URLSession
    private let tokenStore: TokenStore
    private let sleep: Sleeper
    private var tokens: AuthTokens?
    private var refreshTask: Task<AuthTokens, Error>?
    private var sessionEndedHandler: (@Sendable () async -> Void)?
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

    func setSessionEndedHandler(_ handler: @escaping @Sendable () async -> Void) {
        sessionEndedHandler = handler
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
    func logout() async {
        if tokens != nil {
            _ = try? await send(APIRequest(method: "POST", path: "/v1/auth/logout"))
        }
        try? tokenStore.clear()
        tokens = nil
        refreshTask = nil
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
        var response = try await perform(req, accessToken: used.accessToken)
        if response.status == 401 {
            let fresh = try await refreshed(after: used.accessToken)
            response = try await perform(req, accessToken: fresh.accessToken)
        }
        if response.status == 423 {
            await endSession()
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
        let task = Task { try await self.performRefresh(refreshToken) }
        refreshTask = task
        defer { refreshTask = nil }
        return try await task.value
    }

    private func performRefresh(_ refreshToken: String) async throws -> AuthTokens {
        var req = try APIRequest.json("POST", "/v1/auth/refresh", body: RefreshRequest(refreshToken: refreshToken))
        req.requiresAuth = false
        req.idempotencyKey = nil
        var attempt = 0
        while true {
            attempt += 1
            let response = try await perform(req, accessToken: nil)
            switch response.status {
            case 200:
                let fresh = AuthTokens(try decode(SessionResponse.self, response.data))
                // 새 token을 저장한 뒤에 넘긴다. 사이에 앱이 죽으면 이미 소비한 refresh token만 남는다.
                try tokenStore.save(fresh)
                tokens = fresh
                return fresh
            case 429:
                // 세션 상실이 아니다(progressing.md iOS 계약). Retry-After 뒤 같은 token으로 다시 보낸다.
                let err = error(from: response)
                guard attempt < maxRefreshAttempts else { throw err }
                try await sleep(retryAfter(of: err) ?? 1)
            case 401, 423:
                await endSession()
                throw APIError.sessionEnded
            default:
                throw error(from: response)
            }
        }
    }

    private func endSession() async {
        try? tokenStore.clear()
        tokens = nil
        if let handler = sessionEndedHandler { await handler() }
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
            retryAfter: Self.parseRetryAfter(response.header("Retry-After"))
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
