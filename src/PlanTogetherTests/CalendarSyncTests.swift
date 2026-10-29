import XCTest
@testable import PlanTogether

/// 가짜 EventKit.
final class FakeEventSource: CalendarEventSource, @unchecked Sendable {
    private let lock = NSLock()
    var auth: CalendarAuthorization
    var grantOnRequest: Bool
    var cals: [CalendarInfo]
    var events: [String: [CalendarEventOccurrence]]  // calendar id → occurrence
    private(set) var readCalendarIDs: [Set<String>] = []
    private(set) var accessRequests = 0

    init(auth: CalendarAuthorization = .fullAccess, grantOnRequest: Bool = true, cals: [CalendarInfo] = [], events: [String: [CalendarEventOccurrence]] = [:]) {
        self.auth = auth
        self.grantOnRequest = grantOnRequest
        self.cals = cals
        self.events = events
    }

    func authorization() -> CalendarAuthorization { lock.lock(); defer { lock.unlock() }; return auth }

    func requestFullAccess() async throws -> Bool {
        lock.lock(); defer { lock.unlock() }
        accessRequests += 1
        auth = grantOnRequest ? .fullAccess : .denied
        return grantOnRequest
    }

    func calendars() -> [CalendarInfo] { lock.lock(); defer { lock.unlock() }; return auth == .fullAccess ? cals : [] }

    func occurrences(in window: SnapshotWindow, calendarIDs: Set<String>) -> [CalendarEventOccurrence] {
        lock.lock(); defer { lock.unlock() }
        readCalendarIDs.append(calendarIDs)
        return calendarIDs.flatMap { events[$0] ?? [] }
    }
}

/// 서버 calendar 핸들러의 상태 전이를 흉내 내는 가짜(실제 규칙은 Go 테스트가 지킨다).
final class FakeCalendarServer: @unchecked Sendable {
    private let lock = NSLock()
    var exists = false
    var status = "disconnected"
    var version: Int64 = 0
    var syncDevice: String?
    var timeZone: String?
    var sources: [[String: Any]] = []
    var lastRevision: Int64 = 0
    var liveDevices: [String] = [TestTokens.deviceID]
    private(set) var log: [String] = []
    private(set) var puts: [[String: Any]] = []
    private(set) var pageBodies: [Data] = []

    func connectionJSON() -> [String: Any] {
        ["status": status, "sync_device_id": syncDevice as Any? ?? NSNull(), "is_sync_device": syncDevice == TestTokens.deviceID,
         "active_generation": 1, "last_completed_sync_at": NSNull(), "time_zone": timeZone as Any? ?? NSNull(),
         "sources": sources, "version": version]
    }

    func handle(_ req: URLRequest, _ body: Data?) -> MockURLProtocol.Reply {
        lock.lock(); defer { lock.unlock() }
        let path = req.url!.path
        let method = req.httpMethod!
        log.append("\(method) \(path)")
        let json = body.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } ?? [:]
        switch (method, path) {
        case ("GET", "/v1/calendar/connection"):
            return .json(200, ["connection": exists ? connectionJSON() : ["status": "disconnected", "sync_device_id": NSNull(), "is_sync_device": false, "active_generation": 0, "last_completed_sync_at": NSNull(), "time_zone": NSNull(), "sources": [], "version": 0]])
        case ("PUT", "/v1/calendar/connection"):
            puts.append(json.merging(["_if_match": req.value(forHTTPHeaderField: "If-Match") ?? "none"]) { a, _ in a })
            let ifMatch = req.value(forHTTPHeaderField: "If-Match")
            if ifMatch == nil, exists { return .error(409, code: "version_conflict", currentVersion: version) }
            if let ifMatch, ifMatch != "\"\(version)\"" { return .error(409, code: "version_conflict", currentVersion: version) }
            if !exists { exists = true; status = "disconnected" }
            if let to = json["status"] as? String {
                guard CalendarTransition.allowed(from: status, to: to) else { return .error(409, code: "calendar_state_invalid") }
                status = to
            }
            if let tz = json["time_zone"] as? String { timeZone = tz }
            if let s = json["sources"] as? [[String: Any]] { sources = s }
            if json["claim_sync_device"] as? Bool == true { syncDevice = TestTokens.deviceID }
            version += 1
            return .json(200, ["connection": connectionJSON()])
        case ("POST", "/v1/calendar/snapshots"):
            if syncDevice != nil, syncDevice != TestTokens.deviceID { return .error(403, code: "forbidden") }
            guard CalendarTransition.uploadable.contains(status) else { return .error(409, code: "calendar_state_invalid") }
            guard sources.contains(where: { $0["enabled"] as? Bool == true }) else { return .error(409, code: "calendar_source_required") }
            let rev = (json["revision"] as? Int).map(Int64.init) ?? 0
            guard rev > lastRevision else { return .error(409, code: "version_conflict", currentVersion: lastRevision) }
            lastRevision = rev
            syncDevice = TestTokens.deviceID
            status = "syncing"
            version += 1
            return .json(201, CalendarMock.snapshot(id: "s-\(rev)", revision: rev, connection: connectionJSON()))
        case ("PUT", let p) where p.contains("/pages/"):
            pageBodies.append(body ?? Data())
            return .json(200, [:])
        case ("POST", let p) where p.hasSuffix("/complete"):
            status = "ready"
            version += 1
            return .json(200, CalendarMock.snapshot(revision: lastRevision, status: "completed", connection: connectionJSON()))
        case ("POST", let p) where p.hasSuffix("/abort"):
            return .json(200, CalendarMock.snapshot(revision: lastRevision, status: "aborted", connection: connectionJSON()))
        case ("GET", "/v1/devices"):
            return .json(200, ["devices": liveDevices.map { ["id": $0] }])
        default:
            return .error(404, code: "not_found")
        }
    }

    var snapshotsCreated: Int { lock.lock(); defer { lock.unlock() }; return log.filter { $0 == "POST /v1/calendar/snapshots" }.count }
}

final class CalendarSyncTests: XCTestCase {
    private let seoul = TimeZone(identifier: "Asia/Seoul")!
    private let now = RFC3339.parse("2026-10-29T03:00:00Z")!
    private let work = CalendarInfo(id: "cal-work-identifier", title: "업무", sourceTitle: "iCloud")
    private let home = CalendarInfo(id: "cal-home-identifier", title: "집", sourceTitle: "iCloud")

    private func event(_ id: String, day: Int) -> CalendarEventOccurrence {
        let start = now.addingTimeInterval(TimeInterval(day) * 86_400)
        return CalendarEventOccurrence(eventIdentifier: id, occurrenceDate: start, start: start, end: start.addingTimeInterval(3_600),
                                       isAllDay: false, timeZoneID: "Asia/Seoul", availability: .busy, isCanceled: false)
    }

    private func make(_ source: FakeEventSource, _ server: FakeCalendarServer, store: CalendarStateStore = InMemoryCalendarStateStore()) async -> CalendarSync {
        MockURLProtocol.install { req, body in server.handle(req, body) }
        let api = APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                            tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
        let now = self.now
        let tz = seoul
        let sync = CalendarSync(api: api, source: source, store: store, userID: TestTokens.userID, deviceID: TestTokens.deviceID,
                                now: { now }, timeZone: { tz })
        await sync.setUploaderSleep { _ in }
        return sync
    }

    func testTransitionTableMatchesServer() {
        let allowed: [(String, String)] = [
            ("disconnected", "permission_required"), ("disconnected", "selecting"), ("permission_required", "selecting"),
            ("permission_required", "denied"), ("selecting", "revoked"), ("ready", "revoked"), ("ready", "needs_source_reselection"),
            ("ready", "error"), ("syncing", "denied"), ("denied", "selecting"), ("revoked", "selecting"),
            ("needs_source_reselection", "selecting"), ("error", "selecting"), ("ready", "disconnected"), ("ready", "ready"),
        ]
        let rejected: [(String, String)] = [
            ("ready", "selecting"), ("syncing", "selecting"), ("disconnected", "revoked"), ("permission_required", "revoked"),
            ("selecting", "error"), ("selecting", "needs_source_reselection"), ("disconnected", "ready"), ("ready", "permission_required"),
        ]
        for (f, t) in allowed { XCTAssertTrue(CalendarTransition.allowed(from: f, to: t), "\(f)→\(t)") }
        for (f, t) in rejected { XCTAssertFalse(CalendarTransition.allowed(from: f, to: t), "\(f)→\(t)") }
    }

    /// 연결한 적 없는 사용자의 앱 활성화는 권한을 묻지도, 연결을 만들지도 않는다.
    func testForegroundWithoutConnectionDoesNothing() async throws {
        let source = FakeEventSource(auth: .notDetermined)
        let server = FakeCalendarServer()
        let sync = await make(source, server)
        _ = try await sync.refresh(.foreground)
        XCTAssertEqual(server.log, ["GET /v1/calendar/connection"])
        XCTAssertEqual(source.accessRequests, 0)
        XCTAssertTrue(source.readCalendarIDs.isEmpty)
    }

    /// 연결: permission_required(create-only) → OS 권한 → 선택 전이므로 selecting. 선택 없이 일정을 읽지 않는다.
    func testConnectRequestsPermissionThenWaitsForSelection() async throws {
        let source = FakeEventSource(auth: .notDetermined, cals: [work, home])
        let server = FakeCalendarServer()
        let sync = await make(source, server)
        let status = try await sync.connect()
        XCTAssertEqual(source.accessRequests, 1)
        XCTAssertEqual(server.puts.map { $0["status"] as? String }, ["permission_required", "selecting"])
        XCTAssertEqual(server.puts.first?["_if_match"] as? String, "none", "첫 PUT은 If-Match 없는 create-only")
        XCTAssertEqual(server.puts.last?["_if_match"] as? String, "\"1\"")
        XCTAssertEqual(server.status, "selecting")
        XCTAssertEqual(status.note, .needsSelection)
        XCTAssertEqual(status.calendars.map(\.id), [work.id, home.id])
        XCTAssertTrue(status.selectedIDs.isEmpty, "기본 선택은 비어 있다")
        XCTAssertTrue(source.readCalendarIDs.isEmpty)
        XCTAssertEqual(server.snapshotsCreated, 0)
    }

    /// 선택하면 source key(HMAC)·sync 기기·시간대를 PUT하고 선택한 캘린더만 읽어 올린다.
    func testSelectionUploadsOnlySelectedCalendars() async throws {
        let source = FakeEventSource(cals: [work, home], events: [work.id: [event("w1", day: 1), event("w2", day: 2)], home.id: [event("h1", day: 1)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "selecting"; server.version = 2
        let store = InMemoryCalendarStateStore()
        let sync = await make(source, server, store: store)

        let status = try await sync.setSelection([work.id])

        XCTAssertEqual(source.readCalendarIDs, [[work.id]])
        XCTAssertEqual(status.uploadedFacts, 2)
        XCTAssertEqual(server.status, "ready")
        let put = try XCTUnwrap(server.puts.first)
        XCTAssertEqual(put["claim_sync_device"] as? Bool, true)
        XCTAssertEqual(put["time_zone"] as? String, "Asia/Seoul")
        let keys = (put["sources"] as? [[String: Any]])?.compactMap { $0["key"] as? String } ?? []
        XCTAssertEqual(keys.count, 1)
        XCTAssertFalse(keys[0].contains("cal-work"), "calendar identifier 원문은 보내지 않는다")
        XCTAssertNil(put["status"], "이미 업로드 가능한 상태면 status를 보내지 않는다")
        XCTAssertFalse(server.pageBodies.contains { String(data: $0, encoding: .utf8)!.contains("w1") })
        XCTAssertEqual(try store.load()?.lastRevision, 1)
        XCTAssertEqual(try store.load()?.selectedCalendarIDs, [work.id])

        // 같은 선택으로 다시: source PUT 없이 새 revision으로 올린다. key가 같아야 서버가 projection을 다시 만들지 않는다.
        _ = try await sync.refresh(.manual)
        XCTAssertEqual(server.puts.count, 1)
        XCTAssertEqual(server.lastRevision, 2)
        XCTAssertEqual(server.pageBodies[0], server.pageBodies[1], "같은 일정이면 같은 page 바이트")
    }

    func testForegroundIsThrottledAfterRecentUpload() async throws {
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "selecting"; server.version = 1
        let sync = await make(source, server)
        _ = try await sync.setSelection([work.id])
        let status = try await sync.refresh(.foreground)
        XCTAssertEqual(status.note, .throttled)
        XCTAssertEqual(server.snapshotsCreated, 1)
        await sync.setForegroundInterval(0)
        _ = try await sync.refresh(.foreground)
        XCTAssertEqual(server.snapshotsCreated, 2)
    }

    /// 권한이 있던 연결에서 권한이 사라지면 revoked, 처음부터 거부면 denied. 어느 쪽도 일정을 읽지 않는다.
    func testPermissionLossMapsToRevokedOrDenied() async throws {
        for (stored, auth, want) in [("ready", CalendarAuthorization.denied, "revoked"), ("selecting", .writeOnly, "revoked"),
                                     ("permission_required", .denied, "denied"), ("ready", .restricted, "restricted")] {
            let source = FakeEventSource(auth: auth, cals: [work])
            let server = FakeCalendarServer()
            server.exists = true; server.status = stored; server.version = 5
            let sync = await make(source, server)
            _ = try await sync.refresh(.foreground)
            XCTAssertEqual(server.status, want, "\(stored)+\(auth)")
            XCTAssertTrue(source.readCalendarIDs.isEmpty)
        }
    }

    /// 업로드하던 연결의 선택을 모두 해제하면 needs_source_reselection(ready → selecting은 서버가 거부한다).
    func testDeselectAllFromReadyGoesToReselection() async throws {
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "selecting"; server.version = 1
        let sync = await make(source, server)
        _ = try await sync.setSelection([work.id])
        XCTAssertEqual(server.status, "ready")
        let status = try await sync.setSelection([])
        XCTAssertEqual(server.status, "needs_source_reselection")
        XCTAssertEqual((server.puts.last?["sources"] as? [Any])?.count, 0)
        XCTAssertEqual(status.note, .needsSelection)
        XCTAssertEqual(server.snapshotsCreated, 1)
    }

    /// 선택한 캘린더가 사라지면 남은 것만 쓰고, 모두 사라지면 재선택 상태가 된다.
    func testDeletedCalendarIsDroppedFromSelection() async throws {
        let source = FakeEventSource(cals: [work, home], events: [work.id: [event("w1", day: 1)], home.id: [event("h1", day: 1)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "selecting"; server.version = 1
        let sync = await make(source, server)
        _ = try await sync.setSelection([work.id, home.id])
        source.cals = [work]
        let status = try await sync.refresh(.manual)
        XCTAssertEqual(status.selectedIDs, [work.id])
        XCTAssertEqual(source.readCalendarIDs.last, [work.id])
        XCTAssertEqual((server.puts.last?["sources"] as? [Any])?.count, 1)
        source.cals = []
        _ = try await sync.refresh(.manual)
        XCTAssertEqual(server.status, "needs_source_reselection")
    }

    /// 다른 살아 있는 기기가 sync 기기면 앱 활성화로는 가져오지 않는다. 사용자가 연결을 누르면 가져온다.
    /// 지정된 기기가 이미 폐기됐으면(로그아웃 뒤 재로그인) 자동으로 가져온다.
    func testSyncDeviceClaimRules() async throws {
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let store = InMemoryCalendarStateStore()
        let server = FakeCalendarServer()
        server.exists = true; server.status = "ready"; server.version = 4; server.syncDevice = "other-device"
        server.liveDevices = [TestTokens.deviceID, "other-device"]
        var sync = await make(source, server, store: store)
        _ = try? await sync.setSelection([work.id])
        server.sources = []  // 선택 PUT은 아직 없다
        var status = try await sync.refresh(.foreground)
        XCTAssertEqual(status.note, .otherSyncDevice)
        XCTAssertEqual(server.snapshotsCreated, 0)
        XCTAssertEqual(server.syncDevice, "other-device")

        status = try await sync.connect()
        XCTAssertEqual(server.syncDevice, TestTokens.deviceID)
        XCTAssertEqual(server.snapshotsCreated, 1)

        // 폐기된 옛 기기
        let server2 = FakeCalendarServer()
        server2.exists = true; server2.status = "ready"; server2.version = 4; server2.syncDevice = "revoked-device"
        sync = await make(source, server2, store: store)
        status = try await sync.refresh(.foreground)
        XCTAssertNil(status.note)
        XCTAssertEqual(server2.syncDevice, TestTokens.deviceID)
        XCTAssertEqual(server2.snapshotsCreated, 1)
    }

    /// 다른 사용자의 로컬 상태(선택·비밀 키)는 쓰지 않는다.
    func testOtherUsersLocalStateIsIgnored() async throws {
        let store = InMemoryCalendarStateStore(CalendarLocalState(userID: "someone", secret: Data(repeating: 1, count: 32), selectedCalendarIDs: [work.id], lastRevision: 9))
        let sync = await make(FakeEventSource(cals: [work]), FakeCalendarServer(), store: store)
        let ids = await sync.selectedIDs
        XCTAssertTrue(ids.isEmpty)
    }
}
