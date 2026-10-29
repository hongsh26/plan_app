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
        /// 서버가 일정 시간대 이름을 받지 않아 모든 시간대를 UTC로 바꿔 올렸다.
        case timeZoneFallback
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

    /// 이 세션이 아직 살아 있는지(SessionGate). 로그아웃 뒤 같은 APIClient의 새 token으로 옛 세션의 요청이 나가지 않게
    /// 모든 요청 전에 본다.
    private let isActive: @Sendable () -> Bool

    init(api: APIClient, source: CalendarEventSource, store: CalendarStateStore, userID: String, deviceID: String,
         now: @escaping @Sendable () -> Date = { Date() }, timeZone: @escaping @Sendable () -> TimeZone = { TimeZone.current },
         isActive: @escaping @Sendable () -> Bool = { true }) {
        self.api = api
        self.source = source
        self.store = store
        self.userID = userID
        self.deviceID = deviceID
        self.now = now
        self.timeZone = timeZone
        self.isActive = isActive
        let saved = (try? store.load()).flatMap { $0 }
        local = saved?.userID == userID ? saved! : CalendarLocalState(userID: userID, secret: CalendarKeyer.newSecret())
    }

    var selectedIDs: Set<String> { Set(local.selectedCalendarIDs) }

    func setForegroundInterval(_ v: TimeInterval) { minForegroundInterval = v }
    func setUploaderSleep(_ s: @escaping CalendarUploader.Sleeper) { uploaderSleep = s }

    /// 사용자가 이 기기에서 연결을 눌렀다. 권한을 아직 묻지 않았으면 permission_required를 기록하고 OS 권한을 요청한다.
    /// 이 기기가 sync 기기를 맡는다(다른 기기가 맡고 있어도 가져온다).
    func connect() async throws -> CalendarStatus {
        if source.authorization() == .notDetermined {
            try checkActive()
            let conn = try await fetchConnection()
            if conn.storedStatus == "disconnected" {
                _ = try? await put(conn, PutConnectionBody(status: "permission_required", timeZone: currentTZ()))
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

    /// 한 번에 하나만 돈다. 앱 활성화는 진행 중인 실행 결과를 함께 쓴다. 연결·선택 변경·수동 새로고침은 진행 중인
    /// 실행이 끝나기를 기다린 뒤 새로 돈다(그 실행은 이 의도를 모른다).
    func refresh(_ trigger: Trigger) async throws -> CalendarStatus {
        if trigger == .foreground, let current = running { return try await current.value }
        // 새 실행은 앞 실행이 끝난 뒤에 돈다(사슬). 앞 실행의 결과·오류는 이 실행과 무관하다.
        let previous = running
        let task = Task { () async throws -> CalendarStatus in
            if let previous { _ = try? await previous.value }
            return try await self.run(trigger)
        }
        running = task
        defer { if running == task { running = nil } }
        return try await task.value
    }

    // MARK: 흐름

    /// 연결 PUT이 409(version_conflict)면 그사이 다른 기기가 연결을 바꾼 것이다. 같은 본문을 새 version으로 다시 보내면
    /// 낡은 판단으로 덮어쓰므로, 최신 연결을 다시 읽고 처음부터 한 번 더 판단한다.
    private func run(_ trigger: Trigger) async throws -> CalendarStatus {
        do {
            return try await decide(trigger)
        } catch CalendarSyncError.connectionChanged {
            return try await decide(trigger)
        }
    }

    private func decide(_ trigger: Trigger) async throws -> CalendarStatus {
        try checkActive()
        var conn = try await fetchConnection()
        let auth = source.authorization()
        var status = CalendarStatus(authorization: auth, connection: conn)
        let stored = conn.storedStatus
        let explicit = trigger == .connect
        // 연결이 없거나 사용자가 끊었으면(disconnected) 사용자가 이 기기에서 연결을 누를 때만 다시 시작한다. 앱 활성화·
        // 선택 변경이 끊은 연결을 되살려 서버가 지운 일정을 다시 올리지 않게 한다.
        let active = conn.version > 0 && stored != "disconnected"

        var selected = Set(local.selectedCalendarIDs)
        if auth == .fullAccess {
            let calendars = source.calendars()
            status.calendars = calendars
            let available = Set(calendars.map(\.id))
            if !selected.isSubset(of: available) {
                // 선택한 캘린더가 삭제됐거나 접근할 수 없다. 남은 것만 쓴다(전부 사라지면 재선택 상태가 된다).
                selected.formIntersection(available)
                var next = local
                next.selectedCalendarIDs = selected.sorted()
                try store.save(next)
                local = next
            }
            status.selectedIDs = selected
        }
        guard active || explicit else { return status }

        // sync 기기 판정은 상태를 바꾸는 어떤 PUT보다 먼저다. 연결 상태는 사용자 하나에 하나라, sync 기기가 아닌 기기가
        // 자기 권한·선택으로 PUT하면 sync 기기의 projection을 숨기고 두 기기가 번갈아 상태를 뒤집는다.
        var claim = !active
        if active, !conn.isSyncDevice {
            if let other = conn.syncDeviceID, try await isLiveDevice(other), !explicit {
                status.note = .otherSyncDevice
                return status
            }
            claim = true
        }

        guard auth == .fullAccess else {
            let desired: String
            switch auth {
            case .notDetermined: desired = "permission_required"
            case .restricted: desired = "restricted"
            default:
                desired = CalendarTransition.wasAuthorized.contains(stored) && CalendarTransition.allowed(from: stored, to: "revoked") ? "revoked" : "denied"
            }
            if stored != desired || claim, CalendarTransition.allowed(from: stored, to: desired) {
                status.connection = try await put(conn, PutConnectionBody(status: desired, claimSyncDevice: claim))
            }
            return status
        }

        let keyer = CalendarKeyer(secret: local.secret)
        let keys = Set(selected.map(keyer.sourceKey))
        let tz = currentTZ()

        if selected.isEmpty {
            status.note = .needsSelection
            if ["ready", "syncing", "error"].contains(stored) {
                // 업로드하던 연결의 선택을 모두 해제했다. ready → selecting은 허용되지 않으므로 재선택 상태로 둔다.
                status.connection = try await put(conn, PutConnectionBody(status: "needs_source_reselection", sources: [], claimSyncDevice: claim))
            } else if stored == "needs_source_reselection" {
                // 사용자가 다시 고를 때까지 그대로 둔다(설계 3 §12).
                if claim { status.connection = try await put(conn, PutConnectionBody(claimSyncDevice: true)) }
            } else if stored != "selecting" || claim, CalendarTransition.allowed(from: stored, to: "selecting") {
                status.connection = try await put(conn, PutConnectionBody(status: "selecting", timeZone: tz,
                                                                          sources: conn.enabledKeys.isEmpty ? nil : [], claimSyncDevice: claim))
            }
            return status
        }

        let sourcesChanged = keys != conn.enabledKeys
        let needsStatus = !CalendarTransition.uploadable.contains(stored)
        if conn.version == 0 || sourcesChanged || claim || needsStatus || conn.timeZone != tz {
            var body = PutConnectionBody(timeZone: tz, claimSyncDevice: claim)
            if needsStatus { body.status = "selecting" }
            if sourcesChanged || conn.version == 0 { body.sources = keys.sorted().map { CalendarConnection.Source(key: $0, enabled: true) } }
            conn = try await put(conn, body)
            status.connection = conn
        } else if trigger == .foreground, let last = local.lastUploadAt, now().timeIntervalSince(last) < minForegroundInterval {
            status.note = .throttled
            return status
        }

        // 읽기와 업로드.
        let window = SnapshotWindow.make(now: now(), timeZone: timeZone())
        var facts = FactBuilder.build(source.occurrences(in: window, calendarIDs: selected), window: window, userTimeZone: timeZone(), keyer: keyer)
        let done: CalendarConnection
        do {
            done = try await upload(facts, window: window)
        } catch APIError.server(let e) where e.status == 400 && e.code == "invalid_request" {
            // 서버는 시간대를 자기 tzdata(time.LoadLocation)로 검사한다. 기기가 아는 IANA 이름을 서버가 모르면 page 전체가
            // 400이 되어 업로드가 계속 실패한다. 한 번은 모든 fact의 시간대를 UTC로 바꿔 다시 올린다. 시작·끝은 이미 UTC라
            // 바쁜 시간은 그대로이고 원래 시간대 표시만 잃는다.
            facts = facts.map { BusyFact(sourceEventKey: $0.sourceEventKey, startAt: $0.startAt, endAt: $0.endAt, allDay: $0.allDay, timeZone: "UTC", availability: $0.availability) }
            done = try await upload(facts, window: window)
            status.note = .timeZoneFallback
        }
        var next = local
        next.lastUploadAt = now()
        try store.save(next)
        local = next
        status.connection = done
        status.uploadedFacts = facts.count
        return status
    }

    private func upload(_ facts: [BusyFact], window: SnapshotWindow) async throws -> CalendarConnection {
        let pages = try SnapshotPager.pages(facts)
        var uploader = CalendarUploader(api: api, revisions: self)
        uploader.sleep = uploaderSleep
        uploader.isActive = isActive
        return try await uploader.upload(pages: pages, factCount: facts.count, window: window)
    }

    private func checkActive() throws {
        guard isActive() else { throw SessionGate.Closed() }
    }

    private func currentTZ() -> String { FactBuilder.validIdentifier(timeZone().identifier) ?? "UTC" }

    private func fetchConnection() async throws -> CalendarConnection {
        try checkActive()
        return try await api.send(.get("/v1/calendar/connection"), as: CalendarConnectionEnvelope.self).connection
    }

    private func isLiveDevice(_ id: String) async throws -> Bool {
        if id == deviceID { return true }
        try checkActive()
        let devices = try await api.send(.get("/v1/devices"), as: DevicesResponse.self).devices
        return devices.contains { $0.id == id }
    }

    /// 연결이 없으면(version 0) If-Match 없는 create-only, 있으면 If-Match. 409면 다시 판단하도록 알린다.
    private func put(_ conn: CalendarConnection, _ body: PutConnectionBody) async throws -> CalendarConnection {
        try checkActive()
        let req = try APIRequest.json("PUT", "/v1/calendar/connection", body: body, ifMatch: conn.version > 0 ? conn.version : nil)
        do {
            return try await api.send(req, as: CalendarConnectionEnvelope.self).connection
        } catch APIError.server(let e) where e.code == "version_conflict" {
            throw CalendarSyncError.connectionChanged
        }
    }
}

enum CalendarSyncError: Error, Equatable {
    /// 연결 PUT이 409였다. 다른 기기가 그사이 연결을 바꿨다.
    case connectionChanged
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
