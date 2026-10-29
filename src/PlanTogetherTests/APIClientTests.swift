import XCTest
@testable import PlanTogether

final class APIClientTests: XCTestCase {
    private let base = URL(string: "http://localhost:8080")!

    private func makeClient(_ store: TokenStore, sleeps: SleepLog = SleepLog()) -> APIClient {
        APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: store,
                  sleep: { seconds in sleeps.append(seconds) })
    }

    func testAuthHeaderIdempotencyKeyAndQuotedIfMatch() async throws {
        MockURLProtocol.install { _, _ in .json(200, ["ok": true]) }
        let client = makeClient(InMemoryTokenStore(TestTokens.make("a-1")))

        _ = try await client.send(.get("/v1/me"))
        _ = try await client.send(try .json("PATCH", "/v1/me", body: ["display_name": "새 이름"], ifMatch: 7))

        let reqs = MockURLProtocol.requests
        XCTAssertEqual(reqs.count, 2)
        XCTAssertEqual(reqs[0].request.value(forHTTPHeaderField: "Authorization"), "Bearer a-1")
        XCTAssertNil(reqs[0].request.value(forHTTPHeaderField: "Idempotency-Key"), "GET은 Idempotency-Key를 싣지 않는다")
        XCTAssertNil(reqs[0].request.value(forHTTPHeaderField: "If-Match"))
        XCTAssertEqual(reqs[1].request.httpMethod, "PATCH")
        XCTAssertNotNil(UUID(uuidString: reqs[1].request.value(forHTTPHeaderField: "Idempotency-Key") ?? ""))
        XCTAssertEqual(reqs[1].request.value(forHTTPHeaderField: "If-Match"), "\"7\"")
        XCTAssertEqual(reqs[1].body.flatMap { String(data: $0, encoding: .utf8) }, #"{"display_name":"새 이름"}"#)
    }

    func testErrorBodyDecodesToServerError() async throws {
        MockURLProtocol.install { _, _ in .error(409, code: "version_conflict", currentVersion: 4) }
        let client = makeClient(InMemoryTokenStore(TestTokens.make("a-1")))
        do {
            _ = try await client.send(try .json("PATCH", "/v1/me", body: ["display_name": "x"], ifMatch: 3))
            XCTFail("오류여야 한다")
        } catch APIError.server(let e) {
            XCTAssertEqual(e.status, 409)
            XCTAssertEqual(e.code, "version_conflict")
            XCTAssertEqual(e.currentVersion, 4)
            XCTAssertEqual(e.requestID, "req-1")
        }
    }

    func testRateLimitedCarriesRetryAfter() async throws {
        MockURLProtocol.install { _, _ in .error(429, code: "rate_limited", headers: ["Retry-After": "12"]) }
        let client = makeClient(InMemoryTokenStore(TestTokens.make("a-1")))
        do {
            _ = try await client.send(.get("/v1/parties"))
            XCTFail("오류여야 한다")
        } catch APIError.server(let e) {
            XCTAssertEqual(e.code, "rate_limited")
            XCTAssertEqual(e.retryAfter, 12)
        }
    }

    func testNotSignedInWithoutTokens() async {
        MockURLProtocol.install { _, _ in .json(200, [:]) }
        let client = makeClient(InMemoryTokenStore())
        do {
            _ = try await client.send(.get("/v1/me"))
            XCTFail("오류여야 한다")
        } catch {
            XCTAssertEqual(error as? APIError, .notSignedIn)
        }
        XCTAssertTrue(MockURLProtocol.requests.isEmpty)
    }

    /// 동시 401 다섯 개가 refresh 한 번을 공유하고, 각 요청은 같은 Idempotency-Key로 한 번만 다시 간다.
    func testConcurrent401sShareOneRefresh() async throws {
        let counter = Counter()
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/auth/refresh" {
                counter.increment()
                return .json(200, TestTokens.sessionJSON(access: "a-2", refresh: "r-2"), delay: 0.2)
            }
            let auth = req.value(forHTTPHeaderField: "Authorization")
            return auth == "Bearer a-2" ? .json(200, ["ok": true]) : .error(401, code: "invalid_session")
        }
        let store = InMemoryTokenStore(TestTokens.make("a-1", refresh: "r-1"))
        let client = makeClient(store)

        try await withThrowingTaskGroup(of: Void.self) { group in
            for i in 0..<5 {
                group.addTask {
                    var req = try APIRequest.json("POST", "/v1/parties", body: ["name": "P\(i)"])
                    req.idempotencyKey = "key-\(i)"
                    _ = try await client.send(req)
                }
            }
            try await group.waitForAll()
        }

        XCTAssertEqual(counter.value, 1, "refresh는 한 번만 가야 한다")
        let current = await client.currentTokens
        XCTAssertEqual(current?.accessToken, "a-2")
        XCTAssertEqual(try store.load()?.refreshToken, "r-2", "회전한 token을 저장한다")
        let refreshBody = MockURLProtocol.requests.first { $0.request.url!.path == "/v1/auth/refresh" }?.body
        XCTAssertEqual(refreshBody.flatMap { String(data: $0, encoding: .utf8) }, #"{"refresh_token":"r-1"}"#)
        for i in 0..<5 {
            let sent = MockURLProtocol.requests.filter { $0.request.value(forHTTPHeaderField: "Idempotency-Key") == "key-\(i)" }
            XCTAssertLessThanOrEqual(sent.count, 2, "재시도는 한 번뿐이다")
            XCTAssertEqual(sent.last?.request.value(forHTTPHeaderField: "Authorization"), "Bearer a-2")
            XCTAssertEqual(Set(sent.compactMap(\.body)).count, 1, "재시도는 같은 본문 바이트다")
        }
    }

    /// refresh 429는 세션 상실이 아니다. Retry-After만큼 기다린 뒤 같은 refresh token으로 다시 보낸다.
    func testRefresh429WaitsAndRetriesWithoutLogout() async throws {
        let counter = Counter()
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/auth/refresh" {
                counter.increment()
                if counter.value < 3 { return .error(429, code: "rate_limited", headers: ["Retry-After": "2"]) }
                return .json(200, TestTokens.sessionJSON(access: "a-2", refresh: "r-2"))
            }
            return req.value(forHTTPHeaderField: "Authorization") == "Bearer a-2" ? .json(200, [:]) : .error(401, code: "invalid_session")
        }
        let sleeps = SleepLog()
        let ended = Counter()
        let client = makeClient(InMemoryTokenStore(TestTokens.make("a-1", refresh: "r-1")), sleeps: sleeps)
        await client.setSessionEndedHandler { ended.increment() }

        _ = try await client.send(.get("/v1/me"))

        XCTAssertEqual(counter.value, 3)
        XCTAssertEqual(sleeps.values, [2, 2])
        XCTAssertEqual(ended.value, 0)
        let bodies = MockURLProtocol.requests.filter { $0.request.url!.path == "/v1/auth/refresh" }.compactMap(\.body)
        XCTAssertEqual(Set(bodies).count, 1, "429 재시도는 같은 refresh token이다")
    }

    func testRefresh401EndsSession() async throws {
        MockURLProtocol.install { _, _ in .error(401, code: "invalid_session") }
        let store = InMemoryTokenStore(TestTokens.make("a-1"))
        let ended = Counter()
        let client = makeClient(store)
        await client.setSessionEndedHandler { ended.increment() }
        do {
            _ = try await client.send(.get("/v1/me"))
            XCTFail("오류여야 한다")
        } catch {
            XCTAssertEqual(error as? APIError, .sessionEnded)
        }
        XCTAssertEqual(ended.value, 1)
        XCTAssertNil(try store.load())
        let currentTokens = await client.currentTokens
        XCTAssertNil(currentTokens)
    }

    /// refresh가 네트워크 오류면 token을 소비했는지 모르므로 다시 보내지 않고 세션도 유지한다.
    func testRefreshTransportErrorKeepsSessionAndDoesNotResend() async throws {
        let counter = Counter()
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/auth/refresh" {
                counter.increment()
                throw URLError(.timedOut)
            }
            return .error(401, code: "invalid_session")
        }
        let store = InMemoryTokenStore(TestTokens.make("a-1"))
        let client = makeClient(store)
        do {
            _ = try await client.send(.get("/v1/me"))
            XCTFail("오류여야 한다")
        } catch APIError.transport {
        }
        XCTAssertEqual(counter.value, 1)
        XCTAssertEqual(try store.load()?.accessToken, "a-1")
    }

    func testAccountLockedEndsSession() async throws {
        MockURLProtocol.install { _, _ in .error(423, code: "account_locked") }
        let store = InMemoryTokenStore(TestTokens.make("a-1"))
        let client = makeClient(store)
        do {
            _ = try await client.send(.get("/v1/me"))
            XCTFail("오류여야 한다")
        } catch {
            XCTAssertEqual(error as? APIError, .sessionEnded)
        }
        XCTAssertNil(try store.load())
    }

    func testDevSignInStoresTokens() async throws {
        MockURLProtocol.install { req, _ in
            XCTAssertNil(req.value(forHTTPHeaderField: "Authorization"))
            return .json(200, TestTokens.sessionJSON(access: "a-9", refresh: "r-9"))
        }
        let store = InMemoryTokenStore()
        let client = makeClient(store)
        let tokens = try await client.signIn(SignInRequest(identityToken: "dev:alice", authorizationCode: "dev", rawNonce: "dev", displayName: "앨리스"))
        XCTAssertEqual(tokens.accessToken, "a-9")
        XCTAssertEqual(try store.load()?.refreshToken, "r-9")
        let body = try XCTUnwrap(MockURLProtocol.requests.first?.body)
        let json = try XCTUnwrap(JSONSerialization.jsonObject(with: body) as? [String: String])
        XCTAssertEqual(json["identity_token"], "dev:alice")
    }

    func testRetryAfterParsing() {
        XCTAssertEqual(APIClient.parseRetryAfter("3"), 3)
        XCTAssertNil(APIClient.parseRetryAfter(nil))
        XCTAssertNil(APIClient.parseRetryAfter("soon"))
        XCTAssertEqual(APIClient.parseRetryAfter("Wed, 21 Oct 2015 07:28:00 GMT"), 0, "지난 날짜는 0")
    }
}

final class JSONCodingTests: XCTestCase {
    func testRFC3339WithAndWithoutFraction() throws {
        let base = Date(timeIntervalSince1970: 1_761_714_753)  // 2025-10-29T05:12:33Z
        XCTAssertEqual(RFC3339.parse("2025-10-29T05:12:33Z"), base)
        XCTAssertEqual(RFC3339.parse("2025-10-29T05:12:33.5Z")?.timeIntervalSince1970, base.timeIntervalSince1970 + 0.5)
        XCTAssertEqual(RFC3339.parse("2025-10-29T05:12:33.123Z")!.timeIntervalSince1970, base.timeIntervalSince1970 + 0.123, accuracy: 0.0005)
        // Go time.Time 기본 직렬화: 소수 9자리, 오프셋
        XCTAssertEqual(RFC3339.parse("2025-10-29T14:12:33.123456789+09:00")!.timeIntervalSince1970, base.timeIntervalSince1970 + 0.123, accuracy: 0.0005)
        XCTAssertEqual(RFC3339.parse("2025-10-29T05:12:33.654321Z")!.timeIntervalSince1970, base.timeIntervalSince1970 + 0.654, accuracy: 0.0005)
        XCTAssertNil(RFC3339.parse("2025-10-29 05:12:33"))
    }

    func testSessionResponseDecodes() throws {
        let data = try JSONSerialization.data(withJSONObject: TestTokens.sessionJSON(access: "a", refresh: "r"))
        let s = try JSONCoding.makeDecoder().decode(SessionResponse.self, from: data)
        XCTAssertEqual(s.accessToken, "a")
        XCTAssertEqual(s.refreshTokenExpiresAt.timeIntervalSince1970, 2_000_000_000.123, accuracy: 0.001)
    }

    func testJSONValueKeepsBoolAndNumberApart() throws {
        let data = Data(#"{"a":1,"b":true,"c":0,"d":false,"e":"x","f":null,"g":[1.5]}"#.utf8)
        let v = try JSONCoding.makeDecoder().decode(JSONValue.self, from: data)
        XCTAssertEqual(v["a"], .number(1))
        XCTAssertEqual(v["b"], .bool(true))
        XCTAssertEqual(v["c"], .number(0))
        XCTAssertEqual(v["d"], .bool(false))
        XCTAssertEqual(v["e"], .string("x"))
        XCTAssertEqual(v["f"], .null)
        XCTAssertEqual(v["g"], .array([.number(1.5)]))
        let round = try JSONCoding.makeDecoder().decode(JSONValue.self, from: JSONCoding.makeEncoder().encode(v))
        XCTAssertEqual(round, v)
    }

    func testSyncChangeUsesExplicitSnakeCaseKeys() throws {
        let data = Data(#"{"entity_type":"party","entity_id":"e","operation":"upsert","version":3,"payload":{"name":"P"},"scope_membership_id":"m"}"#.utf8)
        let c = try JSONCoding.makeDecoder().decode(SyncChange.self, from: data)
        XCTAssertEqual(c.scopeMembershipID, "m")
        XCTAssertEqual(c.version, 3)
        XCTAssertEqual(c.payload?["name"], .string("P"))
    }
}

/// 동시 테스트용 카운터.
final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0
    var value: Int { lock.lock(); defer { lock.unlock() }; return n }
    func increment() { lock.lock(); n += 1; lock.unlock() }
}

final class SleepLog: @unchecked Sendable {
    private let lock = NSLock()
    private var log: [TimeInterval] = []
    var values: [TimeInterval] { lock.lock(); defer { lock.unlock() }; return log }
    func append(_ v: TimeInterval) { lock.lock(); log.append(v); lock.unlock() }
}
