import Foundation

/// bootstrap·증분 sync를 돌리고 결과를 저장한다(§7.1·§7.2).
///
/// - cursor가 없으면 bootstrap한다. 있으면 `has_more`가 false가 될 때까지 페이지를 읽고, 페이지마다 cursor와 함께 저장한다.
/// - `410 sync_cursor_expired`(code로 판정한다. party_disbanded도 410이다)면 bootstrap으로 원자적으로 교체한 뒤 이어 읽는다.
/// - 동시에 부르면 진행 중인 한 번을 함께 기다린다.
actor SyncEngine {
    private let api: APIClient
    private let store: SyncStore
    private let pageLimit: Int
    private var applier: SyncApplier
    private var running: Task<SyncState, Error>?

    init(api: APIClient, store: SyncStore, userID: String, pageLimit: Int = 500) {
        self.api = api
        self.store = store
        self.pageLimit = pageLimit
        // 다른 사용자의 캐시는 쓰지 않는다(로그아웃이 지우지 못한 경우의 방어).
        let saved = (try? store.load()).flatMap { $0?.userID == userID ? $0 : nil }
        self.applier = SyncApplier(state: saved ?? SyncState(userID: userID))
    }

    var state: SyncState { applier.state }

    @discardableResult
    func sync() async throws -> SyncState {
        if let running { return try await running.value }
        let task = Task { try await self.run() }
        running = task
        defer { running = nil }
        return try await task.value
    }

    /// availability_preference를 다시 읽은 뒤 부른다.
    func markPreferencesRefreshed() throws {
        var s = applier.state
        s.needsPreferenceRefresh = false
        applier = SyncApplier(state: s, options: applier.options)
        try store.save(s)
    }

    private func run() async throws -> SyncState {
        if applier.state.cursor == nil { try await bootstrap() }
        var rebootstrapped = false
        while true {
            guard let cursor = applier.state.cursor else { return applier.state }
            let page: SyncPage
            do {
                page = try await api.send(.get("/v1/sync", query: [
                    URLQueryItem(name: "cursor", value: cursor),
                    URLQueryItem(name: "limit", value: String(pageLimit)),
                ]), as: SyncPage.self)
            } catch let error as APIError where error.serverCode == "sync_cursor_expired" && !rebootstrapped {
                rebootstrapped = true
                try await bootstrap()
                continue
            }
            var next = applier
            next.apply(page: page)
            try store.save(next.state)
            applier = next
            if !page.hasMore { return applier.state }
        }
    }

    private func bootstrap() async throws {
        let response = try await api.send(.get("/v1/sync/bootstrap"), as: BootstrapResponse.self)
        var next = applier
        try next.applyBootstrap(response)
        try store.save(next.state)  // 저장이 실패하면 메모리 상태도 바꾸지 않는다.
        applier = next
    }
}
