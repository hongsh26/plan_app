import XCTest
@testable import PlanTogether

final class MutationQueueTests: XCTestCase {
    private func makeAPI() -> APIClient {
        APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                  tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
    }

    /// 실패한 mutation은 남고, 앱을 다시 띄운 뒤(새 큐 인스턴스)에도 같은 Idempotency-Key·같은 본문 바이트로 재전송한다.
    func testRetryReusesIdempotencyKeyAndBodyAcrossRelaunch() async throws {
        let attempts = Counter()
        MockURLProtocol.install { _, _ in
            attempts.increment()
            if attempts.value == 1 { throw URLError(.notConnectedToInternet) }
            if attempts.value == 2 { return .error(503, code: "internal") }
            return .json(201, ["party": ["id": "p"]])
        }
        let store = InMemoryMutationQueueStore()
        let first = MutationQueue(api: makeAPI(), store: store)
        let queued = try await first.enqueue(try .json("POST", "/v1/parties", body: ["name": "주말 모임"]))

        let left1 = await first.flush()
        XCTAssertEqual(left1, 1)
        let left2 = await first.flush()
        XCTAssertEqual(left2, 1)
        let pending = await first.pending
        XCTAssertEqual(pending.first?.attemptCount, 2)

        let relaunched = MutationQueue(api: makeAPI(), store: store)
        let left3 = await relaunched.flush()
        XCTAssertEqual(left3, 0)
        let after = await relaunched.pending
        XCTAssertTrue(after.isEmpty)

        let sent = MockURLProtocol.requests
        XCTAssertEqual(sent.count, 3)
        XCTAssertEqual(Set(sent.compactMap { $0.request.value(forHTTPHeaderField: "Idempotency-Key") }), [queued.idempotencyKey])
        XCTAssertEqual(Set(sent.compactMap(\.body)).count, 1)
        XCTAssertEqual(try store.load()?.pending, [])
    }

    func testLeavePartyTreats200And410AsDone() async throws {
        MockURLProtocol.install { req, _ in
            req.url!.path.contains("p-gone") ? .error(410, code: "party_disbanded") : .json(200, ["membership": ["status": "left"]])
        }
        let queue = MutationQueue(api: makeAPI(), store: InMemoryMutationQueueStore())
        try await queue.enqueueLeaveParty("p-ok")
        try await queue.enqueueLeaveParty("p-gone")
        let left = await queue.flush()
        XCTAssertEqual(left, 0)
        let failed = await queue.failed
        XCTAssertTrue(failed.isEmpty)
        XCTAssertEqual(MockURLProtocol.requests.map { $0.request.httpMethod }, ["DELETE", "DELETE"])
    }

    /// 410이 완료인 것은 탈퇴뿐이다. 다른 mutation의 410·409는 다시 보내도 같으므로 failed로 옮기고 다음으로 넘어간다.
    func testRejectedMutationMovesToFailedAndQueueContinues() async throws {
        MockURLProtocol.install { req, _ in
            req.url!.path == "/v1/me" ? .error(409, code: "version_conflict", currentVersion: 5) : .json(201, [:])
        }
        let queue = MutationQueue(api: makeAPI(), store: InMemoryMutationQueueStore())
        try await queue.enqueue(try .json("PATCH", "/v1/me", body: ["display_name": "x"], ifMatch: 3))
        try await queue.enqueue(try .json("POST", "/v1/parties", body: ["name": "P"]))
        let left = await queue.flush()
        XCTAssertEqual(left, 0)
        let failed = await queue.failed
        XCTAssertEqual(failed.map(\.path), ["/v1/me"])
        XCTAssertEqual(failed.first?.lastError, "version_conflict")
        XCTAssertEqual(MutationQueue.classify(.server(ServerError(status: 410, code: "party_disbanded", message: "", requestID: nil, currentVersion: nil, retryAfter: nil)), kind: .generic), .rejected)
    }

    func testRateLimitedStopsAndKeepsOrder() async throws {
        MockURLProtocol.install { _, _ in .error(429, code: "rate_limited", headers: ["Retry-After": "5"]) }
        let queue = MutationQueue(api: makeAPI(), store: InMemoryMutationQueueStore())
        try await queue.enqueue(try .json("POST", "/v1/parties", body: ["name": "A"]))
        try await queue.enqueue(try .json("POST", "/v1/parties", body: ["name": "B"]))
        let left = await queue.flush()
        XCTAssertEqual(left, 2)
        XCTAssertEqual(MockURLProtocol.requests.count, 1, "앞이 막히면 뒤를 보내지 않는다")
    }

    func testFileQueueStoreRoundTrip() throws {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".json")
        defer { try? FileManager.default.removeItem(at: url) }
        let store = FileMutationQueueStore(url: url)
        let m = QueuedMutation(id: UUID(), idempotencyKey: "k", kind: .leaveParty, method: "DELETE", path: "/v1/parties/p/memberships/me",
                               body: Data("{\"a\":1}".utf8), ifMatchVersion: 2, createdAt: Date(timeIntervalSince1970: 1_000), attemptCount: 1, lastError: "x")
        try store.save(MutationQueueSnapshot(pending: [m], failed: []))
        XCTAssertEqual(try store.load()?.pending, [m])
        try store.clear()
        XCTAssertNil(try store.load())
    }
}

final class SessionStoreTests: XCTestCase {
    /// 로그아웃은 서버 세션 폐기를 부르고 Keychain·sync 캐시·큐를 모두 지운다.
    @MainActor
    func testLogoutClearsTokensCacheAndQueue() async throws {
        MockURLProtocol.install { req, _ in
            switch req.url!.path {
            case "/v1/auth/apple": return .json(200, TestTokens.sessionJSON(access: "a-1", refresh: "r-1"))
            case "/v1/sync/bootstrap":
                return .json(200, ["schema_version": 1, "cursor_watermark": "w", "snapshot": [
                    ["entity_type": "user", "entity_id": "11111111-1111-1111-1111-111111111111", "operation": "upsert",
                     "version": 1, "payload": ["display_name": "앨리스", "status": "active"], "scope_membership_id": NSNull()]]])
            case "/v1/sync": return .json(200, ["changes": [], "cursor": "w2", "has_more": false])
            case "/v1/auth/logout": return Reply(status: 204)
            default: return .error(404, code: "not_found")
            }
        }
        let tokens = InMemoryTokenStore()
        let syncStore = InMemorySyncStore()
        let queueStore = InMemoryMutationQueueStore()
        let api = APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(), tokenStore: tokens, sleep: { _ in })
        let session = SessionStore(api: api, syncStore: syncStore, queueStore: queueStore)

        await session.devLogin(name: "alice", displayName: "앨리스")
        XCTAssertNil(session.lastError)
        XCTAssertEqual(session.state, .signedIn(SignedInUser(userID: "11111111-1111-1111-1111-111111111111", deviceID: "22222222-2222-2222-2222-222222222222")))
        XCTAssertEqual(session.syncState?.cursor, "w2")
        XCTAssertNotNil(try syncStore.load())
        try await session.mutationQueue?.enqueueLeaveParty("p")

        await session.logout()

        XCTAssertEqual(session.state, .signedOut)
        XCTAssertNil(try tokens.load())
        XCTAssertNil(try syncStore.load())
        XCTAssertNil(try queueStore.load())
        XCTAssertTrue(MockURLProtocol.requests.contains { $0.request.url!.path == "/v1/auth/logout" })
    }

    func testKeychainTokenStoreRoundTrip() throws {
        let store = KeychainTokenStore(service: "com.example.PlanTogether.tests.\(UUID().uuidString)")
        defer { try? store.clear() }
        XCTAssertNil(try store.load())
        try store.save(TestTokens.make("a-1"))
        try store.save(TestTokens.make("a-2"))
        XCTAssertEqual(try store.load()?.accessToken, "a-2")
        try store.clear()
        XCTAssertNil(try store.load())
    }
}

private typealias Reply = MockURLProtocol.Reply
