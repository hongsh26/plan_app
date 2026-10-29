import Combine
import Foundation

enum AppConfig {
    /// 서버 주소. 실행 인자·환경 변수·UserDefaults의 `PLAN_API_BASE_URL`로 바꾼다. 기본은 시뮬레이터용 로컬 서버다.
    static var baseURL: URL {
        let raw = ProcessInfo.processInfo.environment["PLAN_API_BASE_URL"]
            ?? UserDefaults.standard.string(forKey: "PLAN_API_BASE_URL")
        return raw.flatMap(URL.init(string:)) ?? URL(string: "http://localhost:8080")!
    }
}

struct SignedInUser: Equatable {
    let userID: String
    let deviceID: String
}

enum SessionState: Equatable {
    case signedOut
    case signedIn(SignedInUser)
}

/// 로그인 상태와 sync·mutation 큐의 수명을 묶는다. 로그아웃(또는 서버가 세션을 끝냄)하면 Keychain·sync 캐시·큐를
/// 모두 지운다.
@MainActor
final class SessionStore: ObservableObject {
    @Published private(set) var state: SessionState = .signedOut
    @Published private(set) var syncState: SyncState?
    @Published private(set) var lastError: String?
    @Published private(set) var isBusy = false
    @Published private(set) var calendarStatus: CalendarStatus?

    let api: APIClient
    private let syncStore: SyncStore
    private let queueStore: MutationQueueStore
    private let calendarSource: CalendarEventSource
    private let calendarStore: CalendarStateStore
    private(set) var syncEngine: SyncEngine?
    private(set) var calendarSync: CalendarSync?
    private(set) var mutationQueue: MutationQueue?
    /// 현재 세션의 디스크 쓰기 허가. 로그아웃·세션 종료가 닫는다.
    private var gate: SessionGate?

    init(api: APIClient, syncStore: SyncStore, queueStore: MutationQueueStore,
         calendarSource: CalendarEventSource = EventKitEventSource(), calendarStore: CalendarStateStore = InMemoryCalendarStateStore()) {
        self.api = api
        self.syncStore = syncStore
        self.queueStore = queueStore
        self.calendarSource = calendarSource
        self.calendarStore = calendarStore
        api.setSessionEndedHandler { [weak self] in await self?.sessionEnded() }
    }

    /// 앱 기본 구성: Keychain token, Application Support 파일 캐시.
    static func live() -> SessionStore {
        let api = APIClient(baseURL: AppConfig.baseURL, tokenStore: KeychainTokenStore())
        let syncStore: SyncStore = (try? FileSyncStore.standard()) ?? InMemorySyncStore()
        let queueStore: MutationQueueStore = (try? FileMutationQueueStore.standard()) ?? InMemoryMutationQueueStore()
        let calendarStore: CalendarStateStore = (try? FileCalendarStateStore.standard()) ?? InMemoryCalendarStateStore()
        return SessionStore(api: api, syncStore: syncStore, queueStore: queueStore, calendarSource: EventKitEventSource(), calendarStore: calendarStore)
    }

    /// 저장된 세션이 있으면 로그인 상태로 시작한다.
    func restore() async {
        guard let tokens = await api.currentTokens else { return }
        startSession(tokens)
        guard let engine = syncEngine else { return }
        let restored = await engine.state
        if syncEngine === engine { syncState = restored }
    }

    /// 개발용 로그인(server DEV_LOGIN_ENABLED, identity token `dev:<이름>`). Sign in with Apple은 이후 단계에서 붙인다.
    func devLogin(name: String, displayName: String) async {
        await run {
            let tokens = try await self.api.signIn(SignInRequest(
                identityToken: "dev:\(name)", authorizationCode: "dev", rawNonce: "dev", displayName: displayName))
            self.startSession(tokens)
            try await self.syncNowThrowing()
        }
    }

    func syncNow() async {
        await run { try await self.syncNowThrowing() }
    }

    // MARK: 캘린더(설계 3)

    /// 사용자가 캘린더 연결을 눌렀다. 처음이면 OS 권한을 묻는다.
    func calendarConnect() async {
        await runCalendar { try await $0.connect() }
    }

    func calendarSetSelection(_ ids: Set<String>) async {
        await runCalendar { try await $0.setSelection(ids) }
    }

    func calendarRefresh(_ trigger: CalendarSync.Trigger) async {
        await runCalendar { try await $0.refresh(trigger) }
    }

    /// 앱 활성화(설계 3 §7.3). 테스트 호스트로 뜬 앱에서는 하지 않는다.
    func appBecameActive() async {
        guard ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] == nil, state != .signedOut else { return }
        await calendarRefresh(.foreground)
        await syncNow()
    }

    #if DEBUG
    func createSampleCalendarEvents() async {
        guard let source = calendarSource as? EventKitEventSource else { return }
        do {
            try source.createSampleEvents()
            await calendarRefresh(.manual)
        } catch {
            lastError = "시연 일정을 만들지 못했습니다: \(error.localizedDescription)"
        }
    }
    #endif

    private func runCalendar(_ op: @escaping (CalendarSync) async throws -> CalendarStatus) async {
        guard let sync = calendarSync else { return }
        await run {
            let status = try await op(sync)
            if self.calendarSync === sync { self.calendarStatus = status }
        }
    }

    func logout() async {
        await api.logout()
        clearLocal()
    }

    private func syncNowThrowing() async throws {
        guard let engine = syncEngine else { return }
        await mutationQueue?.flush()
        let synced = try await engine.sync()
        // 기다리는 사이 로그아웃했으면 반영하지 않는다.
        if syncEngine === engine { syncState = synced }
    }

    private func startSession(_ tokens: AuthTokens) {
        gate?.close()
        let gate = SessionGate()
        self.gate = gate
        syncEngine = SyncEngine(api: api, store: GatedSyncStore(base: syncStore, gate: gate), userID: tokens.userID)
        mutationQueue = MutationQueue(api: api, store: GatedMutationQueueStore(base: queueStore, gate: gate), userID: tokens.userID)
        calendarSync = CalendarSync(api: api, source: calendarSource, store: GatedCalendarStateStore(base: calendarStore, gate: gate),
                                    userID: tokens.userID, deviceID: tokens.deviceID)
        state = .signedIn(SignedInUser(userID: tokens.userID, deviceID: tokens.deviceID))
    }

    /// refresh 401·423으로 서버가 세션을 끝냈다. token은 APIClient가 이미 지웠다.
    private func sessionEnded() {
        clearLocal()
        lastError = "세션이 끝났습니다. 다시 로그인해 주세요."
    }

    private func clearLocal() {
        // 먼저 닫는다. 진행 중인 sync·flush의 쓰기가 끝난 뒤에 지우고, 이후 쓰기는 거부된다.
        gate?.close()
        gate = nil
        try? syncStore.clear()
        try? queueStore.clear()
        try? calendarStore.clear()
        calendarSync = nil
        calendarStatus = nil
        syncEngine = nil
        mutationQueue = nil
        syncState = nil
        state = .signedOut
    }

    private func run(_ work: @escaping () async throws -> Void) async {
        isBusy = true
        lastError = nil
        defer { isBusy = false }
        do {
            try await work()
        } catch {
            // 로그아웃·세션 종료로 끊긴 작업의 오류는 보이지 않는다. 세션 종료 안내는 sessionEnded()가 이미 남겼다.
            let interrupted = error is SessionGate.Closed || (error as? APIError) == .notSignedIn || (error as? APIError) == .sessionEnded
            if gate == nil, interrupted { return }
            lastError = String(describing: error)
        }
    }
}
