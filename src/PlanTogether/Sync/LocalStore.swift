import Foundation

/// sync 캐시 저장소.
///
/// 파일 하나(JSON)에 cursor·entity·하한을 함께 쓴다. `Data.write(options: .atomic)`은 임시 파일에 쓴 뒤 rename하므로
/// bootstrap 교체와 페이지 적용이 각각 원자적이다. 중간에 앱이 죽어도 이전 상태나 새 상태 중 하나만 남고, cursor와
/// entity가 서로 다른 시점을 가리키지 않는다.
///
/// 설계 §7.3은 SwiftData를 적었다. I1에서는 캐시 크기(수백 entity)와 질의(Party별 필터)가 단순해 파일 하나가 가장
/// 단순하고 견고하다. 알림 확장(설계 9)이 읽어야 할 때 App Group 컨테이너로 옮긴다.
protocol SyncStore: Sendable {
    func load() throws -> SyncState?
    func save(_ state: SyncState) throws
    func clear() throws
}

struct FileSyncStore: SyncStore {
    let url: URL

    /// Application Support/PlanTogether/sync-cache.json. 백업에서 제외한다(서버에서 다시 받을 수 있는 캐시다).
    static func standard() throws -> FileSyncStore {
        let base = try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
        var dir = base.appendingPathComponent("PlanTogether", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        try? dir.setResourceValues(values)
        return FileSyncStore(url: dir.appendingPathComponent("sync-cache.json"))
    }

    func load() throws -> SyncState? {
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        return try JSONCoding.makeDecoder().decode(SyncState.self, from: Data(contentsOf: url))
    }

    func save(_ state: SyncState) throws {
        try JSONCoding.makeEncoder().encode(state).write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
    }

    func clear() throws {
        if FileManager.default.fileExists(atPath: url.path) { try FileManager.default.removeItem(at: url) }
    }
}

final class InMemorySyncStore: SyncStore, @unchecked Sendable {
    private let lock = NSLock()
    private var state: SyncState?
    private(set) var saveCount = 0

    init(_ state: SyncState? = nil) { self.state = state }

    func load() throws -> SyncState? {
        lock.lock(); defer { lock.unlock() }
        return state
    }

    func save(_ state: SyncState) throws {
        lock.lock(); defer { lock.unlock() }
        self.state = state
        saveCount += 1
    }

    func clear() throws {
        lock.lock(); defer { lock.unlock() }
        state = nil
    }
}

/// 로그인 세션 하나의 디스크 쓰기 허가. 로그아웃·세션 종료가 `close()`하면 그 세션의 진행 중이던 sync·큐가
/// 끝나면서 파일을 되살리지 못한다. 쓰기와 close가 같은 잠금을 잡으므로 close 뒤의 삭제가 마지막 쓰기보다 늦다.
final class SessionGate: @unchecked Sendable {
    struct Closed: Error {}

    private let lock = NSLock()
    private var open = true

    func close() {
        lock.lock(); defer { lock.unlock() }
        open = false
    }

    var isOpen: Bool {
        lock.lock(); defer { lock.unlock() }
        return open
    }

    func write(_ body: () throws -> Void) throws {
        lock.lock(); defer { lock.unlock() }
        guard open else { throw Closed() }
        try body()
    }
}

struct GatedSyncStore: SyncStore {
    let base: SyncStore
    let gate: SessionGate

    func load() throws -> SyncState? { try base.load() }
    func save(_ state: SyncState) throws { try gate.write { try base.save(state) } }
    func clear() throws { try base.clear() }
}
