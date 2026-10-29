import Foundation

/// 이 기기의 캘린더 로컬 상태(사용자별). HMAC 비밀 키, 선택한 캘린더 identifier, 마지막 revision.
/// 기기 밖으로 나가지 않고, 로그아웃하면 지운다.
struct CalendarLocalState: Codable, Equatable, Sendable {
    var userID: String
    var secret: Data
    var selectedCalendarIDs: [String] = []
    var lastRevision: Int64 = 0
    var lastUploadAt: Date?

    enum CodingKeys: String, CodingKey {
        case secret
        case userID = "user_id"
        case selectedCalendarIDs = "selected_calendar_ids"
        case lastRevision = "last_revision"
        case lastUploadAt = "last_upload_at"
    }
}

protocol CalendarStateStore: Sendable {
    func load() throws -> CalendarLocalState?
    func save(_ state: CalendarLocalState) throws
    func clear() throws
}

struct FileCalendarStateStore: CalendarStateStore {
    let url: URL

    static func standard() throws -> FileCalendarStateStore {
        FileCalendarStateStore(url: try FileSyncStore.standard().url.deletingLastPathComponent().appendingPathComponent("calendar-state.json"))
    }

    func load() throws -> CalendarLocalState? {
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        return try JSONCoding.makeDecoder().decode(CalendarLocalState.self, from: Data(contentsOf: url))
    }

    func save(_ state: CalendarLocalState) throws {
        try JSONCoding.makeEncoder().encode(state).write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
    }

    func clear() throws {
        if FileManager.default.fileExists(atPath: url.path) { try FileManager.default.removeItem(at: url) }
    }
}

final class InMemoryCalendarStateStore: CalendarStateStore, @unchecked Sendable {
    private let lock = NSLock()
    private var state: CalendarLocalState?
    init(_ s: CalendarLocalState? = nil) { state = s }
    func load() throws -> CalendarLocalState? { lock.lock(); defer { lock.unlock() }; return state }
    func save(_ s: CalendarLocalState) throws { lock.lock(); state = s; lock.unlock() }
    func clear() throws { lock.lock(); state = nil; lock.unlock() }
}

struct GatedCalendarStateStore: CalendarStateStore {
    let base: CalendarStateStore
    let gate: SessionGate
    func load() throws -> CalendarLocalState? { try base.load() }
    func save(_ s: CalendarLocalState) throws { try gate.write { try base.save(s) } }
    func clear() throws { try base.clear() }
}

/// 서버 연결 상태 전이(server calendar.transitionAllowed를 그대로 옮긴 것). 허용되지 않는 PUT은 보내지 않는다.
enum CalendarTransition {
    static func allowed(from: String, to: String) -> Bool {
        if from == to || to == "disconnected" { return true }
        switch from {
        case "disconnected": return ["permission_required", "selecting", "denied", "restricted"].contains(to)
        case "permission_required": return ["selecting", "denied", "restricted"].contains(to)
        case "selecting": return ["denied", "restricted", "revoked", "permission_required"].contains(to)
        case "syncing", "ready": return ["revoked", "needs_source_reselection", "error", "denied", "restricted"].contains(to)
        case "denied", "restricted", "revoked", "needs_source_reselection", "error":
            return ["selecting", "permission_required", "revoked", "denied", "restricted", "needs_source_reselection", "error"].contains(to)
        default: return false
        }
    }

    /// 권한·선택이 갖춰져 snapshot을 만들 수 있는 저장 상태(server createSnapshot).
    static let uploadable: Set<String> = ["selecting", "syncing", "ready", "error"]
    /// 권한이 있던 연결. 여기서 권한이 사라지면 denied가 아니라 revoked다(설계 3 §5.2).
    static let wasAuthorized: Set<String> = ["selecting", "syncing", "ready", "error", "needs_source_reselection"]
}

/// 화면에 보일 캘린더 연결 상태.
struct CalendarStatus: Equatable, Sendable {
    enum Note: Equatable, Sendable {
        /// 다른 살아 있는 기기가 sync 기기다. 이 기기에서 업로드하려면 명시적으로 가져와야 한다.
        case otherSyncDevice
        /// 읽을 캘린더를 골라야 한다.
        case needsSelection
        /// 최근에 올렸다(앱 활성화 연속 호출 억제).
        case throttled
    }

    var authorization: CalendarAuthorization
    var connection: CalendarConnection?
    var calendars: [CalendarInfo] = []
    var selectedIDs: Set<String> = []
    var note: Note?
    var uploadedFacts: Int?
}

private struct PutConnectionBody: Encodable {
    var status: String?
    var timeZone: String?
    var sources: [CalendarConnection.Source]?
    var claimSyncDevice = false

    enum CodingKeys: String, CodingKey {
        case status, sources
        case timeZone = "time_zone"
        case claimSyncDevice = "claim_sync_device"
    }

    // nil 필드는 보내지 않는다(서버는 없는 필드를 "바꾸지 않음"으로 본다).
    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encodeIfPresent(status, forKey: .status)
        try c.encodeIfPresent(timeZone, forKey: .timeZone)
        try c.encodeIfPresent(sources, forKey: .sources)
        try c.encode(claimSyncDevice, forKey: .claimSyncDevice)
    }
}

private struct DevicesResponse: Decodable {
    struct Device: Decodable { let id: String }
    let devices: [Device]
}

/// 캘린더 연결·업로드 조정자(설계 3 §5·§7). 언제 부르나: 사용자가 연결을 누름(`connect`), 앱 활성화(`foreground`),
/// 수동 새로고침(`manual`), 캘린더 선택 변경. 백그라운드 갱신은 하지 않는다.
///
/// 1. 권한이 없으면(거부·제한·write-only·철회) 서버 상태를 denied·restricted·revoked로 맞추고 읽지 않는다.
/// 2. 권한이 있으면 선택한 캘린더(기본은 비어 있다)의 source key를 맞춘다. 선택이 비면 selecting(업로드하던 연결이면
///    needs_source_reselection)으로 두고 읽지 않는다.
/// 3. sync 기기가 이 기기가 아니면: 지정된 기기가 없거나 이미 폐기됐으면 가져오고, 살아 있으면 사용자가 연결을 누른
///    경우에만 가져온다.
/// 4. snapshot을 만들어 올린다. 앱 활성화로 부른 경우 최근 60초 안에 올렸으면 건너뛴다(서버 snapshot 한도 10/분).
actor CalendarSync {
    enum Trigger: Sendable { case connect, foreground, manual, selectionChanged }

    private let api: APIClient
    private let source: CalendarEventSource
    private let store: CalendarStateStore
    private let userID: String
    private let deviceID: String
    private let now: @Sendable () -> Date
    private let timeZone: @Sendable () -> TimeZone
    private var local: CalendarLocalState
    private var running: Task<CalendarStatus, Error>?
    var minForegroundInterval: TimeInterval = 60
    var uploaderSleep: CalendarUploader.Sleeper = { try await Task.sleep(nanoseconds: UInt64(max(0, $0) * 1_000_000_000)) }

    init(api: APIClient, source: CalendarEventSource, store: CalendarStateStore, userID: String, deviceID: String,
         now: @escaping @Sendable () -> Date = { Date() }, timeZone: @escaping @Sendable () -> TimeZone = { TimeZone.current }) {
        self.api = api
        self.source = source
        self.store = store
        self.userID = userID
        self.deviceID = deviceID
        self.now = now
        self.timeZone = timeZone
        let saved = (try? store.load()).flatMap { $0 }
        local = saved?.userID == userID ? saved! : CalendarLocalState(userID: userID, secret: CalendarKeyer.newSecret())
    }

    var selectedIDs: Set<String> { Set(local.selectedCalendarIDs) }

    func setForegroundInterval(_ v: TimeInterval) { minForegroundInterval = v }
    func setUploaderSleep(_ s: @escaping CalendarUploader.Sleeper) { uploaderSleep = s }

    /// 사용자가 연결을 눌렀다. 권한을 아직 묻지 않았으면 먼저 permission_required를 기록하고 OS 권한을 요청한다.
    func connect() async throws -> CalendarStatus {
        if source.authorization() == .notDetermined {
            var conn = try await fetchConnection()
            if conn.storedStatus == "disconnected" || conn.version == 0 {
                conn = try await put(conn, PutConnectionBody(status: "permission_required", timeZone: currentTZ()))
            }
            _ = try? await source.requestFullAccess()
        }
        return try await refresh(.connect)
    }

    func setSelection(_ ids: Set<String>) async throws -> CalendarStatus {
        var next = local
        next.selectedCalendarIDs = ids.sorted()
        try store.save(next)
        local = next
        return try await refresh(.selectionChanged)
    }

    func refresh(_ trigger: Trigger) async throws -> CalendarStatus {
        if let running { return try await running.value }
        let task = Task { try await self.run(trigger) }
        running = task
        defer { running = nil }
        return try await task.value
    }

    // MARK: 흐름

    private func run(_ trigger: Trigger) async throws -> CalendarStatus {
        var conn = try await fetchConnection()
        let auth = source.authorization()
        var status = CalendarStatus(authorization: auth, connection: conn)
        let stored = conn.storedStatus
        let exists = conn.version > 0

        guard auth == .fullAccess else {
            // 연결한 적 없는 사용자의 앱 활성화는 아무것도 만들지 않는다.
            guard exists || trigger == .connect else { return status }
            let desired: String
            switch auth {
            case .notDetermined: desired = "permission_required"
            case .restricted: desired = "restricted"
            default:
                desired = CalendarTransition.wasAuthorized.contains(stored) && CalendarTransition.allowed(from: stored, to: "revoked") ? "revoked" : "denied"
            }
            if stored != desired, CalendarTransition.allowed(from: stored, to: desired) {
                status.connection = try await put(conn, PutConnectionBody(status: desired))
            }
            return status
        }

        let calendars = source.calendars()
        status.calendars = calendars
        let available = Set(calendars.map(\.id))
        var selected = Set(local.selectedCalendarIDs)
        if !selected.isSubset(of: available) {
            // 선택한 캘린더가 삭제됐거나 접근할 수 없다. 남은 것만 쓴다(전부 사라지면 아래에서 재선택 상태가 된다).
            selected.formIntersection(available)
            var next = local
            next.selectedCalendarIDs = selected.sorted()
            try store.save(next)
            local = next
        }
        status.selectedIDs = selected
        guard exists || trigger != .foreground else { return status }

        let keyer = CalendarKeyer(secret: local.secret)
        let keys = Set(selected.map(keyer.sourceKey))
        let tz = currentTZ()

        if selected.isEmpty {
            status.note = .needsSelection
            if ["ready", "syncing", "error"].contains(stored) {
                // 업로드하던 연결의 선택을 모두 해제했다. ready → selecting은 허용되지 않으므로 재선택 상태로 둔다.
                status.connection = try await put(conn, PutConnectionBody(status: "needs_source_reselection", sources: []))
            } else if stored != "selecting", CalendarTransition.allowed(from: stored, to: "selecting") || !exists {
                status.connection = try await put(conn, PutConnectionBody(status: "selecting", timeZone: tz, sources: conn.enabledKeys.isEmpty ? nil : []))
            }
            return status
        }

        // sync 기기 판정.
        var claim = false
        if !conn.isSyncDevice {
            if let other = conn.syncDeviceID, try await isLiveDevice(other) {
                guard trigger == .connect else {
                    status.note = .otherSyncDevice
                    return status
                }
            }
            claim = true
        }

        let sourcesChanged = keys != conn.enabledKeys
        let needsStatus = !CalendarTransition.uploadable.contains(stored)
        if !exists || sourcesChanged || claim || needsStatus || conn.timeZone != tz {
            var body = PutConnectionBody(timeZone: tz, claimSyncDevice: claim)
            if needsStatus { body.status = "selecting" }
            if sourcesChanged || !exists { body.sources = keys.sorted().map { CalendarConnection.Source(key: $0, enabled: true) } }
            conn = try await put(conn, body)
            status.connection = conn
        } else if trigger == .foreground, let last = local.lastUploadAt, now().timeIntervalSince(last) < minForegroundInterval {
            status.note = .throttled
            return status
        }

        // 읽기와 업로드.
        let window = SnapshotWindow.make(now: now(), timeZone: timeZone())
        let facts = FactBuilder.build(source.occurrences(in: window, calendarIDs: selected), window: window, userTimeZone: timeZone(), keyer: keyer)
        let pages = try SnapshotPager.pages(facts)
        var uploader = CalendarUploader(api: api, revisions: self)
        uploader.sleep = uploaderSleep
        let done = try await uploader.upload(pages: pages, factCount: facts.count, window: window)
        var next = local
        next.lastUploadAt = now()
        try store.save(next)
        local = next
        status.connection = done
        status.uploadedFacts = facts.count
        return status
    }

    private func currentTZ() -> String { FactBuilder.validIdentifier(timeZone().identifier) ?? "UTC" }

    private func fetchConnection() async throws -> CalendarConnection {
        try await api.send(.get("/v1/calendar/connection"), as: CalendarConnectionEnvelope.self).connection
    }

    private func isLiveDevice(_ id: String) async throws -> Bool {
        if id == deviceID { return true }
        let devices = try await api.send(.get("/v1/devices"), as: DevicesResponse.self).devices
        return devices.contains { $0.id == id }
    }

    /// 연결이 없으면(version 0) If-Match 없는 create-only, 있으면 If-Match. create가 409면(그사이 생겼다) 다시 읽고
    /// 한 번 더 보낸다.
    private func put(_ conn: CalendarConnection, _ body: PutConnectionBody) async throws -> CalendarConnection {
        var req = try APIRequest.json("PUT", "/v1/calendar/connection", body: body, ifMatch: conn.version > 0 ? conn.version : nil)
        do {
            return try await api.send(req, as: CalendarConnectionEnvelope.self).connection
        } catch APIError.server(let e) where e.code == "version_conflict" {
            let fresh = try await fetchConnection()
            req.ifMatchVersion = fresh.version > 0 ? fresh.version : nil
            req.idempotencyKey = nil
            return try await api.send(req, as: CalendarConnectionEnvelope.self).connection
        }
    }
}

extension CalendarSync: RevisionStore {
    func lastRevision() -> Int64 { local.lastRevision }

    /// 보내기 전에 저장한다(revision 단조, 서버 last_snapshot_revision).
    func setLastRevision(_ revision: Int64) throws {
        var next = local
        next.lastRevision = revision
        try store.save(next)
        local = next
    }
}
