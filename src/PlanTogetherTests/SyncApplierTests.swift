import XCTest
@testable import PlanTogether

private let me = "u-me"

private func change(_ type: String, _ id: String, _ op: String = "upsert", v: Int64? = 1, payload: [String: JSONValue]? = [:], scope: String? = nil) -> SyncChange {
    SyncChange(entityType: type, entityID: id, operation: op, version: v, payload: payload.map { .object($0) }, scopeMembershipID: scope)
}

private func bootstrap(_ snapshot: [SyncChange], cursor: String = "w1", schema: Int = 1) -> BootstrapResponse {
    BootstrapResponse(schemaVersion: schema, snapshot: snapshot, cursorWatermark: cursor)
}

final class SyncApplierTests: XCTestCase {
    func testBootstrapReplacesStateAndRefillsFloors() throws {
        var a = SyncApplier(state: SyncState(userID: me, cursor: "old"))
        a.apply(change("party", "p-old", v: 3, scope: "m-old"))
        a.apply(change("party_invite", "i-old", "tombstone", v: 5, payload: nil, scope: "m-old"))
        XCTAssertEqual(a.state.floors[EntityKey("party_invite", "i-old")], 5)

        try a.applyBootstrap(bootstrap([
            change("user", me, v: 2, payload: ["display_name": .string("나")]),
            // 같은 entity의 하한 항목이 여러 개면 최대값. scope는 null일 수 있다.
            change("party_membership", "m-ended", "tombstone", v: 2, payload: nil, scope: "m-ended"),
            change("party_membership", "m-ended", "tombstone", v: 4, payload: nil, scope: nil),
            // 하한 이하 snapshot upsert는 넣지 않는다.
            change("party_invite", "i-1", "tombstone", v: 3, payload: nil, scope: "m-1"),
            change("party_invite", "i-1", v: 3, scope: "m-1"),
        ], cursor: "w2"))

        XCTAssertEqual(a.state.cursor, "w2")
        XCTAssertNil(a.state.entities[EntityKey("party", "p-old")], "옛 entity는 지운다")
        XCTAssertNil(a.state.floors[EntityKey("party_invite", "i-old")], "옛 하한은 버린다")
        XCTAssertEqual(a.state.floors[EntityKey("party_membership", "m-ended")], 4)
        XCTAssertNil(a.state.entities[EntityKey("party_invite", "i-1")])
        XCTAssertEqual(a.state.entities[EntityKey("user", me)]?.version, 2)
        XCTAssertTrue(a.state.needsPreferenceRefresh, "bootstrap 뒤 활동 시간은 다시 읽는다")
    }

    func testUnknownSchemaVersionIsRejected() {
        var a = SyncApplier(state: SyncState(userID: me))
        XCTAssertThrowsError(try a.applyBootstrap(bootstrap([], schema: 2))) {
            XCTAssertEqual($0 as? SyncError, .unsupportedSchema(2))
        }
    }

    func testVersionAndFloorRules() {
        var a = SyncApplier(state: SyncState(userID: me))
        let k = EntityKey("party_invite", "i")
        a.apply(change("party_invite", "i", v: 2, payload: ["n": .number(2)]))
        a.apply(change("party_invite", "i", v: 1, payload: ["n": .number(1)]))
        XCTAssertEqual(a.state.entities[k]?.version, 2, "낮은 version은 버린다")
        a.apply(change("party_invite", "i", "tombstone", v: 4, payload: nil))
        XCTAssertNil(a.state.entities[k])
        a.apply(change("party_invite", "i", v: 3))
        XCTAssertNil(a.state.entities[k], "하한 이하 upsert는 되살리지 않는다")
        a.apply(change("party_invite", "i", v: 5))
        XCTAssertEqual(a.state.entities[k]?.version, 5)
    }

    func testUnversionedTombstoneLeavesNoFloor() {
        var a = SyncApplier(state: SyncState(userID: me))
        a.apply(change("party", "p", v: 3, scope: "m1"))
        a.apply(change("party", "p", "tombstone", v: nil, payload: nil, scope: "m1"))
        XCTAssertNil(a.state.entities[EntityKey("party", "p")])
        XCTAssertNil(a.state.floors[EntityKey("party", "p")])
        a.apply(change("party", "p", v: 3, scope: "m2"))
        XCTAssertEqual(a.state.entities[EntityKey("party", "p")]?.version, 3, "재가입하면 같은 version이 다시 들어온다")
    }

    func testRuleADropsUpsertFromEndedMembership() {
        var a = SyncApplier(state: SyncState(userID: me))
        a.apply(change("party_membership", "m1", "tombstone", v: 2, payload: nil, scope: "m1"))
        a.apply(change("party", "p", v: 5, scope: "m1"))
        XCTAssertNil(a.state.entities[EntityKey("party", "p")])
    }

    func testRuleBIgnoresStaleScopeTombstoneAfterRejoin() {
        var a = SyncApplier(state: SyncState(userID: me))
        a.apply(change("party", "p", v: 3, scope: "m2"))
        a.apply(change("party_membership", "m2", v: 1, payload: ["party_id": .string("p"), "user_id": .string(me), "status": .string("active")], scope: "m2"))
        a.apply(change("party", "p", "tombstone", v: nil, payload: nil, scope: "m1"))
        XCTAssertNotNil(a.state.entities[EntityKey("party", "p")])
        // 본인 옛 멤버십 끝: 다른 본인 활성 멤버십이 있으니 Party 캐시를 지우지 않는다.
        a.apply(change("party_membership", "m1", "tombstone", v: 2, payload: nil, scope: "m1"))
        XCTAssertNotNil(a.state.entities[EntityKey("party", "p")])
    }

    func testOwnMembershipEndWipesPartyCache() {
        var a = SyncApplier(state: SyncState(userID: me))
        let mine: [String: JSONValue] = ["party_id": .string("p"), "user_id": .string(me), "status": .string("active")]
        a.apply(change("party", "p", v: 1, scope: "m1"))
        a.apply(change("party", "q", v: 1, scope: "mq"))
        a.apply(change("party_membership", "m1", v: 1, payload: mine, scope: "m1"))
        a.apply(change("party_membership", "m-other", v: 1, payload: ["party_id": .string("p"), "user_id": .string("u-2"), "status": .string("active")], scope: "m1"))
        a.apply(change("party_schedule_projection", "s1", v: 1, payload: ["party_id": .string("p")], scope: "m1"))
        a.apply(change("party_invite", "i1", v: 1, payload: ["party_id": .string("p")], scope: "m1"))
        a.apply(change("party_membership", "m1", "tombstone", v: 2, payload: nil, scope: "m1"))
        XCTAssertEqual(Set(a.state.entities.keys), [EntityKey("party", "q")])
        XCTAssertEqual(a.state.floors[EntityKey("party_membership", "m1")], 2)
    }

    func testOtherMembersEndDoesNotWipe() {
        var a = SyncApplier(state: SyncState(userID: me))
        a.apply(change("party", "p", v: 1, scope: "m1"))
        a.apply(change("party_membership", "m-other", v: 1, payload: ["party_id": .string("p"), "user_id": .string("u-2"), "status": .string("active")], scope: "m1"))
        a.apply(change("party_membership", "m-other", "tombstone", v: 2, payload: nil, scope: "m1"))
        XCTAssertNotNil(a.state.entities[EntityKey("party", "p")])
        XCTAssertNil(a.state.entities[EntityKey("party_membership", "m-other")])
    }

    func testRevokedDeviceIsRemovedWithFloor() {
        var a = SyncApplier(state: SyncState(userID: me))
        a.apply(change("device", "d", v: 1, payload: ["revoked": .bool(false)]))
        a.apply(change("device", "d", v: 3, payload: ["revoked": .bool(true)]))
        a.apply(change("device", "d", v: 2, payload: ["revoked": .bool(false)]))
        XCTAssertNil(a.state.entities[EntityKey("device", "d")])
        XCTAssertEqual(a.state.floors[EntityKey("device", "d")], 3)
    }

    func testAvailabilityPreferenceOnlyMarksRefresh() {
        var a = SyncApplier(state: SyncState(userID: me))
        a.apply(change("availability_preference", me, v: 4, payload: ["status": .string("configured")]))
        XCTAssertTrue(a.state.needsPreferenceRefresh)
        XCTAssertTrue(a.state.entities.isEmpty)
    }

    func testOrphanChildIsKept() {
        var a = SyncApplier(state: SyncState(userID: me))
        a.apply(change("party_membership", "m", v: 4, payload: ["party_id": .string("p")], scope: "m-me"))
        XCTAssertEqual(a.state.entities[EntityKey("party_membership", "m")]?.version, 4)
    }
}

final class SyncStoreTests: XCTestCase {
    private var dir: URL!

    override func setUpWithError() throws {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: dir)
    }

    func testFileStoreRoundTripsAndReplacesAtomically() throws {
        let store = FileSyncStore(url: dir.appendingPathComponent("sync.json"))
        XCTAssertNil(try store.load())
        var s = SyncState(userID: me, cursor: "c1")
        s.entities[EntityKey("party", "p")] = StoredEntity(version: 2, payload: .object(["name": .string("P"), "flag": .bool(true)]), scope: "m")
        s.floors[EntityKey("party_invite", "i")] = 7
        try store.save(s)
        XCTAssertEqual(try store.load(), s)

        let replaced = SyncState(userID: me, cursor: "c2")
        try store.save(replaced)
        XCTAssertEqual(try store.load(), replaced, "새 상태로 통째로 바뀐다")
        let leftovers = try FileManager.default.contentsOfDirectory(atPath: dir.path)
        XCTAssertEqual(leftovers, ["sync.json"], "임시 파일이 남지 않는다")

        try store.clear()
        XCTAssertNil(try store.load())
    }
}

final class SyncEngineTests: XCTestCase {
    private func bootstrapJSON(cursor: String, partyName: String) -> [String: Any] {
        ["request_id": "r", "schema_version": 1, "cursor_watermark": cursor, "server_time": "2026-10-29T00:00:00Z",
         "notification_ref_key": "k",
         "snapshot": [["entity_type": "party", "entity_id": "p", "operation": "upsert", "version": 1,
                       "payload": ["name": partyName], "scope_membership_id": "m"]]]
    }

    private func pageJSON(_ changes: [[String: Any]], cursor: String, hasMore: Bool) -> [String: Any] {
        ["request_id": "r", "changes": changes, "cursor": cursor, "has_more": hasMore, "server_time": "2026-10-29T00:00:00Z"]
    }

    private func makeEngine(_ store: SyncStore) -> SyncEngine {
        let api = APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                            tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
        return SyncEngine(api: api, store: store, userID: me, pageLimit: 2)
    }

    func testBootstrapThenPagesUntilHasMoreFalse() async throws {
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/sync/bootstrap" { return .json(200, self.bootstrapJSON(cursor: "w", partyName: "P")) }
            let cursor = URLComponents(url: req.url!, resolvingAgainstBaseURL: false)!.queryItems!.first { $0.name == "cursor" }!.value
            switch cursor {
            case "w": return .json(200, self.pageJSON([["entity_type": "party", "entity_id": "p", "operation": "upsert", "version": 2,
                                                        "payload": ["name": "P2"], "scope_membership_id": "m"]], cursor: "c1", hasMore: true))
            default: return .json(200, self.pageJSON([], cursor: "c2", hasMore: false))
            }
        }
        let store = InMemorySyncStore()
        let state = try await makeEngine(store).sync()
        XCTAssertEqual(state.cursor, "c2")
        XCTAssertEqual(state.entities[EntityKey("party", "p")]?.payload["name"], .string("P2"))
        XCTAssertEqual(try store.load(), state)
        XCTAssertEqual(store.saveCount, 3, "bootstrap 한 번, 페이지마다 한 번 저장한다")
        let limits = MockURLProtocol.requests.compactMap { URLComponents(url: $0.request.url!, resolvingAgainstBaseURL: false)?.queryItems?.first { $0.name == "limit" }?.value }
        XCTAssertEqual(limits, ["2", "2"])
    }

    func testCursorExpiredRebootstrapsAtomically() async throws {
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/sync/bootstrap" { return .json(200, self.bootstrapJSON(cursor: "fresh", partyName: "새 이름")) }
            let cursor = URLComponents(url: req.url!, resolvingAgainstBaseURL: false)!.queryItems!.first { $0.name == "cursor" }!.value
            if cursor == "stale" { return .error(410, code: "sync_cursor_expired") }
            return .json(200, self.pageJSON([], cursor: "fresh2", hasMore: false))
        }
        var old = SyncState(userID: me, cursor: "stale")
        old.entities[EntityKey("party", "gone")] = StoredEntity(version: 9, payload: .object([:]), scope: "m")
        old.floors[EntityKey("party_invite", "x")] = 3
        let store = InMemorySyncStore(old)

        let state = try await makeEngine(store).sync()

        XCTAssertNil(state.entities[EntityKey("party", "gone")], "옛 캐시는 snapshot으로 교체된다")
        XCTAssertTrue(state.floors.isEmpty)
        XCTAssertEqual(state.entities[EntityKey("party", "p")]?.payload["name"], .string("새 이름"))
        XCTAssertEqual(state.cursor, "fresh2")
        XCTAssertEqual(MockURLProtocol.requests.filter { $0.request.url!.path == "/v1/sync/bootstrap" }.count, 1)
    }

    /// 410이어도 code가 sync_cursor_expired가 아니면 bootstrap하지 않는다.
    func testOtherGoneCodeIsNotCursorExpiry() async throws {
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/sync/bootstrap" { return .json(200, self.bootstrapJSON(cursor: "w", partyName: "P")) }
            return .error(410, code: "party_disbanded")
        }
        do {
            try await makeEngine(InMemorySyncStore(SyncState(userID: me, cursor: "c"))).sync()
            XCTFail("오류여야 한다")
        } catch let e as APIError {
            XCTAssertEqual(e.serverCode, "party_disbanded")
        }
        XCTAssertFalse(MockURLProtocol.requests.contains { $0.request.url!.path == "/v1/sync/bootstrap" })
    }

    func testOtherUsersCacheIsIgnored() async throws {
        let store = InMemorySyncStore(SyncState(userID: "someone-else", cursor: "theirs"))
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/sync/bootstrap" { return .json(200, self.bootstrapJSON(cursor: "w", partyName: "P")) }
            return .json(200, self.pageJSON([], cursor: "w2", hasMore: false))
        }
        let state = try await makeEngine(store).sync()
        XCTAssertEqual(state.userID, me)
        XCTAssertEqual(MockURLProtocol.requests.first?.request.url?.path, "/v1/sync/bootstrap")
    }
}
