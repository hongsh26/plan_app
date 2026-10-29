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

    let api: APIClient
    private let syncStore: SyncStore
    private let queueStore: MutationQueueStore
    private(set) var syncEngine: SyncEngine?
    private(set) var mutationQueue: MutationQueue?

    init(api: APIClient, syncStore: SyncStore, queueStore: MutationQueueStore) {
        self.api = api
        self.syncStore = syncStore
        self.queueStore = queueStore
        Task { await api.setSessionEndedHandler { [weak self] in await self?.sessionEnded() } }
    }

    /// 앱 기본 구성: Keychain token, Application Support 파일 캐시.
    static func live() -> SessionStore {
        let api = APIClient(baseURL: AppConfig.baseURL, tokenStore: KeychainTokenStore())
        let syncStore: SyncStore = (try? FileSyncStore.standard()) ?? InMemorySyncStore()
        let queueStore: MutationQueueStore = (try? FileMutationQueueStore.standard()) ?? InMemoryMutationQueueStore()
        return SessionStore(api: api, syncStore: syncStore, queueStore: queueStore)
    }

    /// 저장된 세션이 있으면 로그인 상태로 시작한다.
    func restore() async {
        guard let tokens = await api.currentTokens else { return }
        startSession(tokens)
        syncState = await syncEngine?.state
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

    func logout() async {
        await api.logout()
        clearLocal()
    }

    private func syncNowThrowing() async throws {
        guard let engine = syncEngine else { return }
        await mutationQueue?.flush()
        syncState = try await engine.sync()
    }

    private func startSession(_ tokens: AuthTokens) {
        syncEngine = SyncEngine(api: api, store: syncStore, userID: tokens.userID)
        mutationQueue = MutationQueue(api: api, store: queueStore)
        state = .signedIn(SignedInUser(userID: tokens.userID, deviceID: tokens.deviceID))
    }

    /// refresh 401·423으로 서버가 세션을 끝냈다. token은 APIClient가 이미 지웠다.
    private func sessionEnded() {
        clearLocal()
        lastError = "세션이 끝났습니다. 다시 로그인해 주세요."
    }

    private func clearLocal() {
        try? syncStore.clear()
        try? queueStore.clear()
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
            lastError = String(describing: error)
        }
    }
}
