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

/// 서버 calendar 핸들러(connection.go, snapshot.go)를 흉내 내는 가짜. 실제 규칙은 Go 테스트가 지키고, 여기서는 클라이언트
/// 판단이 드러나는 부분을 옮긴다: create-only·If-Match, 전이 표, sync 기기(요청 기기는 bearer token으로 구별), claim 때
/// ready → syncing, syncing 중 source 변경 → selecting, 같은 revision 200, complete의 syncing·fact 수 검사, 다른 기기 403.
final class FakeCalendarServer: @unchecked Sendable {
    static let ipadDevice = "33333333-3333-3333-3333-333333333333"

    private let lock = NSLock()
    var exists = false
    var status = "disconnected"
    var version: Int64 = 0
    var syncDevice: String?
    var timeZone: String?
    var sources: [[String: Any]] = []
    var lastRevision: Int64 = 0
    var liveDevices: [String] = [TestTokens.deviceID, FakeCalendarServer.ipadDevice]
    /// token → 기기
    var tokenDevices: [String: String] = ["a-1": TestTokens.deviceID, "ipad": FakeCalendarServer.ipadDevice]
    /// 서버 tzdata에 없는 것으로 칠 시간대.
    var unknownZones: Set<String> = []
    /// 다음 연결 PUT 직전에 한 번 실행한다(다른 기기의 동시 변경 흉내).
    var beforeNextPut: ((FakeCalendarServer) -> Void)?
    var getDelay: TimeInterval = 0
    private struct Session { var id: String; var status: String; var expectedPages: Int; var facts: [Int: Int] }
    private var sessions: [Int64: Session] = [:]
    private(set) var log: [String] = []
    private(set) var puts: [[String: Any]] = []
    private(set) var pageBodies: [Data] = []

    func connectionJSON(for device: String?) -> [String: Any] {
        ["status": status, "sync_device_id": syncDevice as Any? ?? NSNull(), "is_sync_device": syncDevice != nil && syncDevice == device,
         "active_generation": 1, "last_completed_sync_at": NSNull(), "time_zone": timeZone as Any? ?? NSNull(),
         "sources": sources, "version": version]
    }

    private var enabledKeys: Set<String> { Set(sources.filter { $0["enabled"] as? Bool == true }.compactMap { $0["key"] as? String }) }

    private func abortOpen() {
        for (r, var sess) in sessions where sess.status == "open" { sess.status = "aborted"; sessions[r] = sess }
    }

    func handle(_ req: URLRequest, _ body: Data?) -> MockURLProtocol.Reply {
        lock.lock(); defer { lock.unlock() }
        let path = req.url!.path
        let method = req.httpMethod!
        let token = req.value(forHTTPHeaderField: "Authorization")?.replacingOccurrences(of: "Bearer ", with: "") ?? ""
        let device = tokenDevices[token]
        log.append("\(method) \(path) [\(token)]")
        let json = body.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } ?? [:]
        switch (method, path) {
        case ("GET", "/v1/calendar/connection"):
            let conn: [String: Any] = exists ? connectionJSON(for: device) : ["status": "disconnected", "sync_device_id": NSNull(), "is_sync_device": false,
                "active_generation": 0, "last_completed_sync_at": NSNull(), "time_zone": NSNull(), "sources": [], "version": 0]
            return .json(200, ["connection": conn], delay: getDelay)
        case ("PUT", "/v1/calendar/connection"):
            if let hook = beforeNextPut { beforeNextPut = nil; hook(self) }
            let ifMatch = req.value(forHTTPHeaderField: "If-Match")
            puts.append(json.merging(["_if_match": ifMatch ?? "none", "_device": device ?? "?"]) { a, _ in a })
            if ifMatch == nil, exists { return .error(409, code: "version_conflict", currentVersion: version) }
            if let ifMatch, ifMatch != "\"\(version)\"" { return .error(409, code: "version_conflict", currentVersion: version) }
            if !exists { exists = true; status = "disconnected" }
            var newStatus = status
            if let to = json["status"] as? String {
                guard CalendarTransition.allowed(from: status, to: to) else { return .error(409, code: "calendar_state_invalid") }
                newStatus = to
            }
            if json["claim_sync_device"] as? Bool == true, syncDevice != device {
                syncDevice = device
                if newStatus == "ready" { newStatus = "syncing" }
                abortOpen()
            }
            if let s = json["sources"] as? [[String: Any]] {
                let newKeys = Set(s.filter { $0["enabled"] as? Bool == true }.compactMap { $0["key"] as? String })
                if newKeys != enabledKeys {
                    if newStatus == "syncing" { newStatus = "selecting" }
                    abortOpen()
                }
                sources = s
            }
            if status == "syncing", newStatus != "syncing" { abortOpen() }
            if let tz = json["time_zone"] as? String { timeZone = tz }
            status = newStatus
            version += 1
            return .json(200, ["connection": connectionJSON(for: device)])
        case ("POST", "/v1/calendar/snapshots"):
            guard exists else { return .error(409, code: "calendar_state_invalid") }
            if syncDevice != nil, syncDevice != device { return .error(403, code: "forbidden") }
            guard CalendarTransition.uploadable.contains(status) else { return .error(409, code: "calendar_state_invalid") }
            guard !enabledKeys.isEmpty else { return .error(409, code: "calendar_source_required") }
            let rev = (json["revision"] as? Int).map(Int64.init) ?? 0
            if let existing = sessions[rev] {
                return .json(200, CalendarMock.snapshot(id: existing.id, revision: rev, status: existing.status, connection: connectionJSON(for: device)))
            }
            guard rev > lastRevision else { return .error(409, code: "version_conflict", currentVersion: lastRevision) }
            abortOpen()
            lastRevision = rev
            sessions[rev] = Session(id: "s-\(rev)", status: "open", expectedPages: json["expected_pages"] as? Int ?? 0, facts: [:])
            syncDevice = device
            if ["selecting", "error", "ready"].contains(status) { status = "syncing" }
            version += 1
            return .json(201, CalendarMock.snapshot(id: "s-\(rev)", revision: rev, connection: connectionJSON(for: device)))
        case ("PUT", let p) where p.contains("/pages/"):
            guard syncDevice == device else { return .error(403, code: "forbidden") }
            let parts = p.split(separator: "/")
            let sid = String(parts[3]), n = Int(parts[5])!
            guard let (rev, sess) = sessions.first(where: { $0.value.id == sid }) else { return .error(404, code: "not_found") }
            guard sess.status == "open" else { return .error(409, code: "calendar_snapshot_state") }
            let facts = (json["facts"] as? [[String: Any]]) ?? []
            if facts.contains(where: { unknownZones.contains($0["time_zone"] as? String ?? "") }) { return .error(400, code: "invalid_request") }
            pageBodies.append(body ?? Data())
            sessions[rev]!.facts[n] = facts.count
            return .json(200, [:])
        case ("POST", let p) where p.hasSuffix("/complete"):
            guard syncDevice == device else { return .error(403, code: "forbidden") }
            let sid = String(p.split(separator: "/")[3])
            guard let (rev, sess) = sessions.first(where: { $0.value.id == sid }) else { return .error(404, code: "not_found") }
            if sess.status == "completed" { return .json(200, CalendarMock.snapshot(id: sid, revision: rev, status: "completed", connection: connectionJSON(for: device))) }
            guard sess.status == "open" else { return .error(409, code: "calendar_snapshot_state") }
            guard status == "syncing" else { return .error(409, code: "calendar_state_invalid") }
            let total = sess.facts.values.reduce(0, +)
            guard sess.facts.count == sess.expectedPages, total == json["fact_count"] as? Int else { return .error(409, code: "calendar_snapshot_incomplete") }
            sessions[rev]!.status = "completed"
            status = "ready"
            version += 1
            return .json(200, CalendarMock.snapshot(id: sid, revision: rev, status: "completed", connection: connectionJSON(for: device)))
        case ("POST", let p) where p.hasSuffix("/abort"):
            let sid = String(p.split(separator: "/")[3])
            if let (rev, sess) = sessions.first(where: { $0.value.id == sid }), sess.status == "open" {
                sessions[rev]!.status = "aborted"
                if status == "syncing" { status = "ready" }
            }
            return .json(200, CalendarMock.snapshot(revision: lastRevision, status: "aborted", connection: connectionJSON(for: device)))
        case ("GET", "/v1/devices"):
            return .json(200, ["devices": liveDevices.map { ["id": $0] }])
        default:
            return .error(404, code: "not_found")
        }
    }

    var snapshotsCreated: Int { lock.lock(); defer { lock.unlock() }; return log.filter { $0.hasPrefix("POST /v1/calendar/snapshots [") }.count }
    var putCount: Int { lock.lock(); defer { lock.unlock() }; return puts.count }
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

    private func make(_ source: FakeEventSource, _ server: FakeCalendarServer, store: CalendarStateStore = InMemoryCalendarStateStore(),
                      token: String = "a-1", device: String = TestTokens.deviceID, isActive: @escaping @Sendable () -> Bool = { true }) async -> CalendarSync {
        MockURLProtocol.install { req, body in server.handle(req, body) }
        let api = APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                            tokenStore: InMemoryTokenStore(TestTokens.make(token)), sleep: { _ in })
        let now = self.now
        let tz = seoul
        let sync = CalendarSync(api: api, source: source, store: store, userID: TestTokens.userID, deviceID: device,
                                now: { now }, timeZone: { tz }, isActive: isActive)
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
        XCTAssertEqual(server.log, ["GET /v1/calendar/connection [a-1]"])
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
        let source = FakeEventSource(cals: [work, home], events: [work.id: [event("ekid-secret-work-1", day: 1), event("ekid-secret-work-2", day: 2)], home.id: [event("ekid-secret-home", day: 1)]])
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
        // 무작위 HMAC key가 우연히 짧은 문자열을 담을 수 있어, 우연히 나올 수 없는 identifier로 본다.
        XCTAssertFalse(server.pageBodies.contains { String(data: $0, encoding: .utf8)!.contains("ekid-secret") }, "EventKit identifier가 나가면 안 된다")
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

    // MARK: 리뷰 반영 회귀(I2)

    /// High: sync 기기가 아닌 iPad가 권한 거부·빈 선택으로 앱을 열어도 iPhone의 연결 상태를 바꾸지 않는다. 사용자가 iPad에서
    /// 연결을 누를 때만 iPad가 맡는다.
    func testNonSyncDeviceDoesNotOverwriteSharedConnection() async throws {
        let server = FakeCalendarServer()
        let phoneSource = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let phone = await make(phoneSource, server)
        server.exists = true; server.status = "selecting"; server.version = 1
        _ = try await phone.setSelection([work.id])
        XCTAssertEqual(server.status, "ready")
        XCTAssertEqual(server.syncDevice, TestTokens.deviceID)
        let putsAfterPhone = server.putCount

        for ipadSource in [FakeEventSource(auth: .denied), FakeEventSource(auth: .restricted), FakeEventSource(cals: [home])] {
            let ipad = await make(ipadSource, server, token: "ipad", device: FakeCalendarServer.ipadDevice)
            var status = try await ipad.refresh(.foreground)
            XCTAssertEqual(status.note, .otherSyncDevice)
            status = try await ipad.setSelection([])
            XCTAssertEqual(status.note, .otherSyncDevice)
            XCTAssertEqual(server.status, "ready", "iPad가 공유 상태를 덮어쓰면 안 된다")
            XCTAssertEqual(server.putCount, putsAfterPhone)
            XCTAssertTrue(ipadSource.readCalendarIDs.isEmpty)
        }

        // 사용자가 iPad에서 연결을 누르면 iPad가 맡고 올린다.
        let ipadSource = FakeEventSource(cals: [home], events: [home.id: [event("h1", day: 1)]])
        let ipad = await make(ipadSource, server, token: "ipad", device: FakeCalendarServer.ipadDevice)
        _ = try await ipad.setSelection([home.id])
        XCTAssertEqual(server.syncDevice, TestTokens.deviceID, "선택 변경만으로는 가져오지 않는다")
        let status = try await ipad.connect()
        XCTAssertEqual(server.syncDevice, FakeCalendarServer.ipadDevice)
        XCTAssertEqual(server.status, "ready")
        XCTAssertEqual(status.uploadedFacts, 1)
        // 이제 iPhone이 sync 기기가 아니다. iPhone의 앱 활성화는 아무것도 바꾸지 않는다.
        let before = server.putCount
        let phoneAgain = await make(phoneSource, server)
        let phoneStatus = try await phoneAgain.refresh(.foreground)
        XCTAssertEqual(phoneStatus.note, .otherSyncDevice)
        XCTAssertEqual(server.putCount, before)
    }

    /// Medium: 사용자가 끊은(disconnected) 연결은 앱 활성화·선택 변경으로 되살아나지 않는다. 연결을 누르면 다시 시작한다.
    func testDisconnectedConnectionIsNotRevivedWithoutConnect() async throws {
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "disconnected"; server.version = 7; server.syncDevice = TestTokens.deviceID
        let store = InMemoryCalendarStateStore(CalendarLocalState(userID: TestTokens.userID, secret: Data(repeating: 3, count: 32), selectedCalendarIDs: [work.id]))
        let sync = await make(source, server, store: store)
        _ = try await sync.refresh(.foreground)
        _ = try await sync.refresh(.manual)
        _ = try await sync.setSelection([work.id])
        XCTAssertEqual(server.putCount, 0)
        XCTAssertEqual(server.snapshotsCreated, 0)
        XCTAssertTrue(source.readCalendarIDs.isEmpty)
        let denied = await make(FakeEventSource(auth: .denied), server, store: store)
        _ = try await denied.refresh(.foreground)
        XCTAssertEqual(server.status, "disconnected", "권한이 없어도 끊은 연결을 denied로 바꾸지 않는다")

        _ = try await sync.connect()
        XCTAssertEqual(server.status, "ready")
        XCTAssertEqual(server.snapshotsCreated, 1)
    }

    /// Medium: 연결 PUT이 409면 같은 본문을 다시 보내지 않고 최신 연결로 다시 판단한다. 그사이 다른 살아 있는 기기가
    /// sync 기기가 됐으면 앱 활성화는 아무것도 바꾸지 않는다.
    func testVersionConflictRedecidesFromFreshState() async throws {
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "ready"; server.version = 3; server.syncDevice = TestTokens.deviceID
        let store = InMemoryCalendarStateStore(CalendarLocalState(userID: TestTokens.userID, secret: Data(repeating: 3, count: 32), selectedCalendarIDs: [work.id]))
        let sync = await make(source, server, store: store)
        server.beforeNextPut = { s in
            s.syncDevice = FakeCalendarServer.ipadDevice
            s.version += 1
        }
        let status = try await sync.refresh(.foreground)
        XCTAssertEqual(server.puts.count, 1, "409 뒤 같은 PUT을 다시 보내지 않는다")
        XCTAssertEqual(status.note, .otherSyncDevice)
        XCTAssertEqual(server.syncDevice, FakeCalendarServer.ipadDevice)
        XCTAssertEqual(server.snapshotsCreated, 0)
    }

    /// Low: needs_source_reselection은 사용자가 다시 고를 때까지 그대로다.
    func testReselectionStateIsKeptUntilUserSelects() async throws {
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "needs_source_reselection"; server.version = 2; server.syncDevice = TestTokens.deviceID
        let sync = await make(source, server)
        _ = try await sync.refresh(.foreground)
        _ = try await sync.refresh(.manual)
        XCTAssertEqual(server.status, "needs_source_reselection")
        XCTAssertEqual(server.putCount, 0)
        _ = try await sync.setSelection([work.id])
        XCTAssertEqual(server.status, "ready")
    }

    /// Low: 서버 tzdata가 모르는 시간대로 page가 400이면 모든 fact를 UTC로 바꿔 한 번 다시 올린다.
    func testUnknownServerTimeZoneFallsBackToUTC() async throws {
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1), event("w2", day: 2)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "selecting"; server.version = 1
        server.unknownZones = ["Asia/Seoul"]
        let sync = await make(source, server)
        let status = try await sync.setSelection([work.id])
        XCTAssertEqual(server.status, "ready")
        XCTAssertEqual(status.note, .timeZoneFallback)
        XCTAssertEqual(status.uploadedFacts, 2)
        XCTAssertEqual(server.snapshotsCreated, 3, "사용자 시간대가 거부되면 두 번째 단계까지 간다")
        XCTAssertTrue(server.log.contains { $0.hasPrefix("POST /v1/calendar/snapshots/s-1/abort") }, "실패한 첫 session은 abort한다")
        let zones = Set(((try JSONSerialization.jsonObject(with: server.pageBodies.last!) as? [String: Any])?["facts"] as? [[String: Any]] ?? [])
            .compactMap { $0["time_zone"] as? String })
        XCTAssertEqual(zones, ["UTC"])
    }

    /// Low: 세션이 끝난 뒤(로그아웃·재로그인) 옛 CalendarSync는 요청을 보내지 않는다.
    func testClosedSessionSendsNothing() async throws {
        let server = FakeCalendarServer()
        server.exists = true; server.status = "ready"; server.version = 1; server.syncDevice = TestTokens.deviceID
        let gate = SessionGate()
        let sync = await make(FakeEventSource(cals: [work]), server, isActive: { gate.isOpen })
        gate.close()
        do {
            _ = try await sync.refresh(.manual)
            XCTFail("오류여야 한다")
        } catch is SessionGate.Closed {}
        XCTAssertTrue(server.log.isEmpty)
    }

    /// Low: 진행 중인 실행이 있어도 선택 변경은 그 결과를 재사용하지 않고 새로 돈다.
    func testSelectionChangeRunsAfterInFlightForeground() async throws {
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "selecting"; server.version = 1
        server.getDelay = 0.2
        let sync = await make(source, server)
        async let fg = sync.refresh(.foreground)
        try await Task.sleep(nanoseconds: 50_000_000)
        async let sel = sync.setSelection([work.id])
        let (_, b) = try await (fg, sel)
        let gets = server.log.filter { $0.hasPrefix("GET /v1/calendar/connection") }.count
        XCTAssertEqual(gets, 2, "선택 변경은 진행 중 결과를 재사용하지 않고 새로 돈다")
        XCTAssertEqual(b.uploadedFacts, 1)
        XCTAssertEqual(server.status, "ready")
    }

    /// 재검증 Medium: 옛 sync 기기가 폐기된 상태(iPhone 재로그인)에서 업로드할 수 없는 iPad의 앱 활성화는 자동으로 맡지
    /// 않고 PUT하지 않는다. 그 뒤 iPhone의 앱 활성화가 맡아서 올린다.
    func testRevokedSyncDeviceIsClaimedOnlyByDeviceThatCanUpload() async throws {
        let server = FakeCalendarServer()
        server.exists = true; server.status = "ready"; server.version = 5; server.syncDevice = "old-phone"
        server.sources = [["key": "k", "enabled": true]]
        server.liveDevices = [TestTokens.deviceID, FakeCalendarServer.ipadDevice]  // old-phone은 폐기됐다

        for ipadSource in [FakeEventSource(auth: .denied), FakeEventSource(auth: .restricted), FakeEventSource(cals: [home])] {
            let ipad = await make(ipadSource, server, token: "ipad", device: FakeCalendarServer.ipadDevice)
            _ = try await ipad.refresh(.foreground)
            XCTAssertEqual(server.putCount, 0)
            XCTAssertEqual(server.syncDevice, "old-phone")
            XCTAssertEqual(server.status, "ready")
        }

        let phoneSource = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 1)]])
        let store = InMemoryCalendarStateStore(CalendarLocalState(userID: TestTokens.userID, secret: Data(repeating: 3, count: 32), selectedCalendarIDs: [work.id]))
        let phone = await make(phoneSource, server, store: store)
        let status = try await phone.refresh(.foreground)
        XCTAssertEqual(server.syncDevice, TestTokens.deviceID)
        XCTAssertEqual(server.status, "ready")
        XCTAssertEqual(status.uploadedFacts, 1)
        XCTAssertEqual(server.snapshotsCreated, 1)
    }

    /// 재검증 Low: page 거부가 시간대 때문이 아니면(두 단계 대체 모두 거부) 오류로 끝나고 대체 표시를 남기지 않는다.
    /// 일정 고유 시간대만 서버가 모르면 그 fact만 UTC로 바꾼다.
    func testTimeZoneFallbackIsStagedAndOnlyForPageRejection() async throws {
        let ny = CalendarEventOccurrence(eventIdentifier: "ny", occurrenceDate: now, start: now.addingTimeInterval(86_400), end: now.addingTimeInterval(90_000),
                                         isAllDay: false, timeZoneID: "America/New_York", availability: .busy, isCanceled: false)
        let source = FakeEventSource(cals: [work], events: [work.id: [event("w1", day: 2), ny]])
        let server = FakeCalendarServer()
        server.exists = true; server.status = "selecting"; server.version = 1
        server.unknownZones = ["America/New_York"]
        let sync = await make(source, server)
        let status = try await sync.setSelection([work.id])
        XCTAssertEqual(status.note, .timeZoneFallback)
        let zones = Set(((try JSONSerialization.jsonObject(with: server.pageBodies.last!) as? [String: Any])?["facts"] as? [[String: Any]] ?? [])
            .compactMap { $0["time_zone"] as? String })
        XCTAssertEqual(zones, ["Asia/Seoul", "UTC"], "일정 고유 시간대만 UTC로, 사용자 시간대 fact는 그대로")

        // 시간대와 무관한 거부: 모든 시간대를 막아 두 단계 모두 실패하게 한다.
        let server2 = FakeCalendarServer()
        server2.exists = true; server2.status = "selecting"; server2.version = 1
        server2.unknownZones = ["Asia/Seoul", "America/New_York", "UTC"]
        let sync2 = await make(source, server2)
        do {
            _ = try await sync2.setSelection([work.id])
            XCTFail("오류여야 한다")
        } catch CalendarUploadError.pageRejected {}
        XCTAssertEqual(server2.snapshotsCreated, 3)
    }
}
