import Foundation
@testable import PlanTogether

/// URLSession 요청을 가로채는 테스트 서버. handler는 요청 하나에 (상태, 헤더, 본문)을 돌려준다.
final class MockURLProtocol: URLProtocol {
    struct Reply {
        var status: Int
        var headers: [String: String] = [:]
        var body: Data = Data()
        /// 응답 지연(초). 동시 요청을 겹치게 할 때 쓴다.
        var delay: TimeInterval = 0

        static func json(_ status: Int, _ object: Any, headers: [String: String] = [:], delay: TimeInterval = 0) -> Reply {
            Reply(status: status, headers: headers.merging(["Content-Type": "application/json"]) { a, _ in a },
                  body: try! JSONSerialization.data(withJSONObject: object), delay: delay)
        }

        static func error(_ status: Int, code: String, headers: [String: String] = [:], currentVersion: Int64? = nil) -> Reply {
            var body: [String: Any] = ["code": code, "message": "m", "request_id": "req-1"]
            if let currentVersion { body["current_version"] = currentVersion }
            return json(status, body, headers: headers)
        }
    }

    /// 받은 요청(본문 포함). 순서대로 쌓인다.
    struct Recorded {
        let request: URLRequest
        let body: Data?
    }

    private static let lock = NSLock()
    private static var handler: ((URLRequest, Data?) throws -> Reply)?
    private static var recorded: [Recorded] = []

    static func install(_ handler: @escaping (URLRequest, Data?) throws -> Reply) {
        lock.lock(); defer { lock.unlock() }
        self.handler = handler
        recorded = []
    }

    static var requests: [Recorded] {
        lock.lock(); defer { lock.unlock() }
        return recorded
    }

    static func makeSession() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [MockURLProtocol.self]
        return URLSession(configuration: config)
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        // URLSession은 httpBody를 httpBodyStream으로 바꿔 넘긴다.
        let body = request.httpBody ?? request.httpBodyStream.map(Self.readAll)
        Self.lock.lock()
        Self.recorded.append(Recorded(request: request, body: body))
        let handler = Self.handler
        Self.lock.unlock()
        do {
            guard let handler else { throw URLError(.badServerResponse) }
            let reply = try handler(request, body)
            let respond = { [weak self] in
                guard let self, let client = self.client else { return }
                let response = HTTPURLResponse(url: self.request.url!, statusCode: reply.status, httpVersion: "HTTP/1.1", headerFields: reply.headers)!
                client.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
                client.urlProtocol(self, didLoad: reply.body)
                client.urlProtocolDidFinishLoading(self)
            }
            if reply.delay > 0 {
                DispatchQueue.global().asyncAfter(deadline: .now() + reply.delay, execute: respond)
            } else {
                respond()
            }
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}

    private static func readAll(_ stream: InputStream) -> Data {
        stream.open(); defer { stream.close() }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while stream.hasBytesAvailable {
            let n = stream.read(&buffer, maxLength: buffer.count)
            if n <= 0 { break }
            data.append(buffer, count: n)
        }
        return data
    }
}

enum TestTokens {
    static func make(_ access: String, refresh: String = "r-0") -> AuthTokens {
        AuthTokens(userID: "11111111-1111-1111-1111-111111111111", deviceID: "22222222-2222-2222-2222-222222222222",
                   accessToken: access, accessTokenExpiresAt: Date(timeIntervalSince1970: 2_000_000_000),
                   refreshToken: refresh, refreshTokenExpiresAt: Date(timeIntervalSince1970: 2_000_000_000))
    }

    static func sessionJSON(access: String, refresh: String) -> [String: Any] {
        ["request_id": "req", "user_id": "11111111-1111-1111-1111-111111111111",
         "device_id": "22222222-2222-2222-2222-222222222222",
         "access_token": access, "access_token_expires_at": "2033-05-18T03:33:20Z",
         "refresh_token": refresh, "refresh_token_expires_at": "2033-05-18T03:33:20.123456789Z", "new_user": false]
    }
}
