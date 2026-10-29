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
        let first = MutationQueue(api: makeAPI(), store: store, userID: TestTokens.userID)
        let queued = try await first.enqueue(try .json("POST", "/v1/parties", body: ["name": "주말 모임"]))

        let left1 = await first.flush()
        XCTAssertEqual(left1, 1)
        let left2 = await first.flush()
        XCTAssertEqual(left2, 1)
        let pending = await first.pending
        XCTAssertEqual(pending.first?.attemptCount, 2)

        let relaunched = MutationQueue(api: makeAPI(), store: store, userID: TestTokens.userID)
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
        let queue = MutationQueue(api: makeAPI(), store: InMemoryMutationQueueStore(), userID: TestTokens.userID)
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
        let queue = MutationQueue(api: makeAPI(), store: InMemoryMutationQueueStore(), userID: TestTokens.userID)
        try await queue.enqueue(try .json("PATCH", "/v1/me", body: ["display_name": "x"], ifMatch: 3))
        try await queue.enqueue(try .json("POST", "/v1/parties", body: ["name": "P"]))
        let left = await queue.flush()
        XCTAssertEqual(left, 0)
        let failed = await queue.failed
        XCTAssertEqual(failed.map(\.path), ["/v1/me"])
        XCTAssertEqual(failed.first?.lastError, "version_conflict")
        func gone(_ status: Int, _ code: String) -> APIError {
            .server(ServerError(status: status, code: code, message: "", requestID: nil, currentVersion: nil, retryAfter: nil))
        }
        XCTAssertEqual(MutationQueue.classify(gone(410, "party_disbanded"), kind: .generic), .rejected)
        // 탈퇴의 410도 code가 party_disbanded일 때만 완료다(§6: code로 분기).
        XCTAssertEqual(MutationQueue.classify(gone(410, "party_disbanded"), kind: .leaveParty), .done)
        XCTAssertEqual(MutationQueue.classify(gone(410, "sync_cursor_expired"), kind: .leaveParty), .rejected)
        // refresh 뒤 재전송도 401이면 세션 문제다. 버리지 않고 남긴다.
        XCTAssertEqual(MutationQueue.classify(gone(401, "invalid_session"), kind: .generic), .retryLater)
        XCTAssertEqual(MutationQueue.classify(.sessionEnded, kind: .generic), .stop)
    }

    func testRateLimitedStopsAndKeepsOrder() async throws {
        MockURLProtocol.install { _, _ in .error(429, code: "rate_limited", headers: ["Retry-After": "5"]) }
        let queue = MutationQueue(api: makeAPI(), store: InMemoryMutationQueueStore(), userID: TestTokens.userID)
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
        let calendarStore = InMemoryCalendarStateStore(CalendarLocalState(userID: "11111111-1111-1111-1111-111111111111", secret: Data(count: 32), selectedCalendarIDs: ["c"]))
        let session = SessionStore(api: api, syncStore: syncStore, queueStore: queueStore, calendarSource: FakeEventSource(), calendarStore: calendarStore)

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
        XCTAssertNil(try calendarStore.load(), "캘린더 선택·비밀 키도 지운다")
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

final class MutationQueueConcurrencyTests: XCTestCase {
    /// flush를 동시에 불러도 같은 mutation은 한 번만 간다.
    func testConcurrentFlushSendsEachMutationOnce() async throws {
        MockURLProtocol.install { _, _ in .json(201, [:], delay: 0.1) }
        let api = APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                            tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
        let queue = MutationQueue(api: api, store: InMemoryMutationQueueStore(), userID: TestTokens.userID)
        try await queue.enqueue(try .json("POST", "/v1/parties", body: ["name": "A"]))
        async let first = queue.flush()
        async let second = queue.flush()
        let results = await [first, second]
        XCTAssertEqual(results, [0, 0])
        XCTAssertEqual(MockURLProtocol.requests.count, 1)
        let failed = await queue.failed
        XCTAssertTrue(failed.isEmpty)
    }
}

/// 세션 경계 회귀 테스트(리뷰 H1·M1·M2·M3·L1).
final class SessionBoundaryTests: XCTestCase {
    private let base = URL(string: "http://localhost:8080")!

    private func bootstrapReply() -> MockURLProtocol.Reply {
        .json(200, ["schema_version": 1, "cursor_watermark": "w", "snapshot": [] as [Any]])
    }

    /// H1: flush 중 refresh가 401이면 세션이 끝나고 큐 파일이 되살아나지 않는다.
    @MainActor
    func testSessionEndDuringFlushLeavesNoQueueFile() async throws {
        MockURLProtocol.install { req, _ in
            switch req.url!.path {
            case "/v1/auth/apple": return .json(200, TestTokens.sessionJSON(access: "a-1", refresh: "r-1"))
            case "/v1/sync/bootstrap": return self.bootstrapReply()
            case "/v1/sync": return .json(200, ["changes": [] as [Any], "cursor": "w2", "has_more": false])
            default: return .error(401, code: "invalid_session")  // POST /v1/parties와 refresh
            }
        }
        let queueStore = InMemoryMutationQueueStore()
        let syncStore = InMemorySyncStore()
        let api = APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: InMemoryTokenStore(), sleep: { _ in })
        let session = SessionStore(api: api, syncStore: syncStore, queueStore: queueStore)
        await session.devLogin(name: "alice", displayName: "앨리스")
        try await session.mutationQueue?.enqueue(try .json("POST", "/v1/parties", body: ["name": "P"]))
        XCTAssertNotNil(try queueStore.load())

        await session.syncNow()

        XCTAssertEqual(session.state, .signedOut)
        XCTAssertNil(try queueStore.load(), "세션 종료 뒤 큐 파일이 다시 써지면 안 된다")
        XCTAssertNil(try syncStore.load())
    }

    /// H1·L1: 로그아웃하는 동안 진행 중이던 flush·sync가 끝나도 큐·캐시 파일을 되살리지 않는다.
    @MainActor
    func testLogoutDuringFlushAndSyncLeavesNoFiles() async throws {
        let posted = Counter()
        MockURLProtocol.install { req, _ in
            switch req.url!.path {
            case "/v1/auth/apple": return .json(200, TestTokens.sessionJSON(access: "a-1", refresh: "r-1"))
            case "/v1/sync/bootstrap": return self.bootstrapReply()
            case "/v1/sync": return .json(200, ["changes": [] as [Any], "cursor": "w2", "has_more": false], delay: 0.3)
            case "/v1/parties":
                posted.increment()
                return .json(201, [:], delay: 0.3)
            default: return MockURLProtocol.Reply(status: 204)
            }
        }
        let queueStore = InMemoryMutationQueueStore()
        let syncStore = InMemorySyncStore()
        let api = APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: InMemoryTokenStore(), sleep: { _ in })
        let session = SessionStore(api: api, syncStore: syncStore, queueStore: queueStore)
        await session.devLogin(name: "alice", displayName: "앨리스")
        try await session.mutationQueue?.enqueue(try .json("POST", "/v1/parties", body: ["name": "A"]))
        try await session.mutationQueue?.enqueue(try .json("POST", "/v1/parties", body: ["name": "B"]))

        let running = Task { await session.syncNow() }
        while posted.value == 0 { try await Task.sleep(nanoseconds: 10_000_000) }
        await session.logout()
        await running.value

        XCTAssertEqual(session.state, .signedOut)
        XCTAssertNil(session.syncState)
        XCTAssertNil(session.lastError)
        XCTAssertNil(try queueStore.load())
        XCTAssertNil(try syncStore.load())
    }

    /// H1: 다른 사용자(또는 주인 없는 옛 파일)의 큐는 읽지 않고 보내지 않는다.
    func testQueueOfAnotherUserIsDiscarded() async throws {
        MockURLProtocol.install { _, _ in .json(201, [:]) }
        let m = QueuedMutation(id: UUID(), idempotencyKey: "k", kind: .generic, method: "POST", path: "/v1/parties",
                               body: Data("{}".utf8), ifMatchVersion: nil, createdAt: Date(), attemptCount: 0, lastError: nil)
        let api = APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
        for owner in ["someone-else", nil] {
            let queue = MutationQueue(api: api, store: InMemoryMutationQueueStore(MutationQueueSnapshot(userID: owner, pending: [m])), userID: TestTokens.userID)
            let pending = await queue.pending
            XCTAssertTrue(pending.isEmpty)
            let left = await queue.flush()
            XCTAssertEqual(left, 0)
        }
        XCTAssertTrue(MockURLProtocol.requests.isEmpty)
    }

    /// M1: 로그아웃하는 동안 끝난 refresh는 token을 저장하지 않는다.
    func testRefreshCompletingAfterLogoutDoesNotRestoreTokens() async throws {
        let refreshing = Counter()
        MockURLProtocol.install { req, _ in
            switch req.url!.path {
            case "/v1/auth/refresh":
                refreshing.increment()
                return .json(200, TestTokens.sessionJSON(access: "a-2", refresh: "r-2"), delay: 0.3)
            case "/v1/auth/logout": return MockURLProtocol.Reply(status: 204)
            default: return .error(401, code: "invalid_session")
            }
        }
        let store = InMemoryTokenStore(TestTokens.make("a-1", refresh: "r-1"))
        let api = APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: store, sleep: { _ in })
        let request = Task { try await api.send(.get("/v1/me")) }
        while refreshing.value == 0 { try await Task.sleep(nanoseconds: 10_000_000) }
        await api.logout()
        _ = try? await request.value
        try await Task.sleep(nanoseconds: 400_000_000)

        XCTAssertNil(try store.load(), "Keychain이 비어 있어야 한다")
        let current = await api.currentTokens
        XCTAssertNil(current)
    }

    /// M2: refresh 200 뒤 Keychain 저장이 실패해도 새 token을 쓰고, 소비된 refresh token을 다시 보내지 않는다.
    /// 저장은 다음 요청 때 다시 시도한다.
    func testRefreshSaveFailureKeepsNewTokensInMemory() async throws {
        let refreshes = Counter()
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/auth/refresh" {
                refreshes.increment()
                return .json(200, TestTokens.sessionJSON(access: "a-2", refresh: "r-2"))
            }
            return req.value(forHTTPHeaderField: "Authorization") == "Bearer a-2" ? .json(200, [:]) : .error(401, code: "invalid_session")
        }
        let store = FlakyTokenStore(TestTokens.make("a-1", refresh: "r-1"), failingSaves: 1)
        let api = APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: store, sleep: { _ in })

        _ = try await api.send(.get("/v1/me"))
        XCTAssertEqual(try store.load()?.accessToken, "a-1", "첫 저장은 실패했다")
        _ = try await api.send(.get("/v1/me"))

        XCTAssertEqual(refreshes.value, 1, "소비된 refresh token을 다시 보내지 않는다")
        XCTAssertEqual(try store.load()?.refreshToken, "r-2", "다음 요청에서 저장을 다시 시도한다")
    }

    /// M3: 큐 저장이 실패해도 같은 mutation을 끝없이 다시 보내지 않는다.
    func testQueueSaveFailureStopsFlush() async throws {
        MockURLProtocol.install { _, _ in .json(201, [:]) }
        let api = APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
        let gate = SessionGate()
        let queue = MutationQueue(api: api, store: GatedMutationQueueStore(base: InMemoryMutationQueueStore(), gate: gate), userID: TestTokens.userID)
        try await queue.enqueue(try .json("POST", "/v1/parties", body: ["name": "A"]))
        try await queue.enqueue(try .json("POST", "/v1/parties", body: ["name": "B"]))
        gate.close()
        let left = await queue.flush()
        XCTAssertEqual(left, 2)
        XCTAssertEqual(MockURLProtocol.requests.count, 1)
    }
}

/// 처음 n번 save가 실패하는 token 저장소.
final class FlakyTokenStore: TokenStore, @unchecked Sendable {
    private let lock = NSLock()
    private var tokens: AuthTokens?
    private var failing: Int

    init(_ tokens: AuthTokens?, failingSaves: Int) {
        self.tokens = tokens
        self.failing = failingSaves
    }

    func load() throws -> AuthTokens? {
        lock.lock(); defer { lock.unlock() }
        return tokens
    }

    func save(_ tokens: AuthTokens) throws {
        lock.lock(); defer { lock.unlock() }
        if failing > 0 {
            failing -= 1
            throw TokenStoreError.keychain(-1)
        }
        self.tokens = tokens
    }

    func clear() throws {
        lock.lock(); defer { lock.unlock() }
        tokens = nil
    }
}

/// 재로그인 경계 회귀 테스트(재검증 Low 1·2).
final class GenerationBoundaryTests: XCTestCase {
    private let base = URL(string: "http://localhost:8080")!

    private func signInRequest() -> SignInRequest {
        SignInRequest(identityToken: "dev:alice", authorizationCode: "dev", rawNonce: "dev", displayName: nil)
    }

    private func refreshBodies(_ token: String) -> Int {
        MockURLProtocol.requests.filter {
            $0.request.url!.path == "/v1/auth/refresh" && $0.body.flatMap { String(data: $0, encoding: .utf8) } == #"{"refresh_token":"\#(token)"}"#
        }.count
    }

    /// 옛 세대 refresh가 재로그인 뒤 늦게 끝나도 새 세대의 refresh task를 지우지 않는다. 지우면 다음 401이 같은
    /// refresh token을 다시 보내 서버가 기기를 폐기한다.
    func testLateOldRefreshDoesNotClearNewGenerationTask() async throws {
        MockURLProtocol.install { req, body in
            let path = req.url!.path
            let text = body.flatMap { String(data: $0, encoding: .utf8) } ?? ""
            switch path {
            case "/v1/auth/refresh" where text.contains("r-1"):
                return .json(200, TestTokens.sessionJSON(access: "a-2", refresh: "r-2"), delay: 0.4)
            case "/v1/auth/refresh":
                return .json(200, TestTokens.sessionJSON(access: "a-new2", refresh: "r-new2"), delay: 0.9)
            case "/v1/auth/apple": return .json(200, TestTokens.sessionJSON(access: "a-new", refresh: "r-new"))
            case "/v1/auth/logout": return MockURLProtocol.Reply(status: 204)
            default:
                return req.value(forHTTPHeaderField: "Authorization") == "Bearer a-new2" ? .json(200, [:]) : .error(401, code: "invalid_session")
            }
        }
        let api = APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: InMemoryTokenStore(TestTokens.make("a-1", refresh: "r-1")), sleep: { _ in })

        let old = Task { try await api.send(.get("/v1/me")) }
        while refreshBodies("r-1") == 0 { try await Task.sleep(nanoseconds: 10_000_000) }
        await api.logout()
        _ = try await api.signIn(signInRequest())
        let first = Task { try await api.send(.get("/v1/me")) }
        while refreshBodies("r-new") == 0 { try await Task.sleep(nanoseconds: 10_000_000) }
        _ = try? await old.value  // 옛 응답 도착(버려진다). 옛 호출자의 정리가 여기서 돈다.
        let second = Task { try await api.send(.get("/v1/me")) }
        _ = try await first.value
        _ = try await second.value

        XCTAssertEqual(refreshBodies("r-new"), 1, "새 refresh token은 한 번만 보낸다")
        let current = await api.currentTokens
        XCTAssertEqual(current?.accessToken, "a-new2")
    }

    /// 재로그인 전에 보낸 요청의 423이 늦게 와도 새 세션을 끝내지 않는다.
    func testLate423FromOldGenerationDoesNotEndNewSession() async throws {
        MockURLProtocol.install { req, _ in
            switch req.url!.path {
            case "/v1/auth/apple": return .json(200, TestTokens.sessionJSON(access: "a-new", refresh: "r-new"))
            case "/v1/auth/logout": return MockURLProtocol.Reply(status: 204)
            default:
                return req.value(forHTTPHeaderField: "Authorization") == "Bearer a-1"
                    ? .error(423, code: "account_locked").withDelay(0.4) : .json(200, [:])
            }
        }
        let store = InMemoryTokenStore(TestTokens.make("a-1", refresh: "r-1"))
        let ended = Counter()
        let api = APIClient(baseURL: base, session: MockURLProtocol.makeSession(), tokenStore: store, sleep: { _ in })
        api.setSessionEndedHandler { ended.increment() }

        let old = Task { try await api.send(.get("/v1/parties")) }
        while MockURLProtocol.requests.isEmpty { try await Task.sleep(nanoseconds: 10_000_000) }
        await api.logout()
        _ = try await api.signIn(signInRequest())
        _ = try? await old.value

        XCTAssertEqual(ended.value, 0)
        let current = await api.currentTokens
        XCTAssertEqual(current?.accessToken, "a-new")
        XCTAssertEqual(try store.load()?.accessToken, "a-new")
    }
}

private extension MockURLProtocol.Reply {
    func withDelay(_ d: TimeInterval) -> MockURLProtocol.Reply {
        var r = self
        r.delay = d
        return r
    }
}
