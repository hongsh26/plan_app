import Foundation

/// 오프라인 mutation 하나(§7.3). 재전송은 같은 Idempotency-Key와 같은 본문 **바이트**로 한다. 서버는 본문 바이트로
/// 요청 지문을 만들므로 모델에서 다시 인코딩하면 `idempotency_mismatch`가 날 수 있다.
struct QueuedMutation: Codable, Equatable, Identifiable, Sendable {
    enum Kind: String, Codable, Sendable {
        case generic
        /// Party 탈퇴. 200(이미 탈퇴 포함)과 410 party_disbanded를 모두 완료로 본다(progressing.md iOS 계약).
        case leaveParty = "leave_party"
    }

    let id: UUID
    let idempotencyKey: String
    let kind: Kind
    let method: String
    let path: String
    let body: Data?
    let ifMatchVersion: Int64?
    let createdAt: Date
    var attemptCount: Int
    var lastError: String?

    enum CodingKeys: String, CodingKey {
        case id, kind, method, path, body
        case idempotencyKey = "idempotency_key"
        case ifMatchVersion = "if_match_version"
        case createdAt = "created_at"
        case attemptCount = "attempt_count"
        case lastError = "last_error"
    }

    var request: APIRequest {
        APIRequest(method: method, path: path, body: body, idempotencyKey: idempotencyKey, ifMatchVersion: ifMatchVersion)
    }
}

/// 큐 파일 내용. pending은 보낼 것(생성 순서), failed는 서버가 거절해 사용자에게 보여 줄 것이다.
struct MutationQueueSnapshot: Codable, Equatable, Sendable {
    var pending: [QueuedMutation] = []
    var failed: [QueuedMutation] = []
}

protocol MutationQueueStore: Sendable {
    func load() throws -> MutationQueueSnapshot?
    func save(_ snapshot: MutationQueueSnapshot) throws
    func clear() throws
}

struct FileMutationQueueStore: MutationQueueStore {
    let url: URL

    static func standard() throws -> FileMutationQueueStore {
        FileMutationQueueStore(url: try FileSyncStore.standard().url.deletingLastPathComponent().appendingPathComponent("mutation-queue.json"))
    }

    func load() throws -> MutationQueueSnapshot? {
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        return try JSONCoding.makeDecoder().decode(MutationQueueSnapshot.self, from: Data(contentsOf: url))
    }

    func save(_ snapshot: MutationQueueSnapshot) throws {
        try JSONCoding.makeEncoder().encode(snapshot).write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
    }

    func clear() throws {
        if FileManager.default.fileExists(atPath: url.path) { try FileManager.default.removeItem(at: url) }
    }
}

final class InMemoryMutationQueueStore: MutationQueueStore, @unchecked Sendable {
    private let lock = NSLock()
    private var snapshot: MutationQueueSnapshot?

    init(_ snapshot: MutationQueueSnapshot? = nil) { self.snapshot = snapshot }

    func load() throws -> MutationQueueSnapshot? {
        lock.lock(); defer { lock.unlock() }
        return snapshot
    }

    func save(_ snapshot: MutationQueueSnapshot) throws {
        lock.lock(); defer { lock.unlock() }
        self.snapshot = snapshot
    }

    func clear() throws {
        lock.lock(); defer { lock.unlock() }
        snapshot = nil
    }
}

/// 최소 오프라인 mutation 큐. 생성 순서대로 하나씩 보낸다.
///
/// - 성공(2xx, 탈퇴는 410도)이면 지운다. 서버 성공 응답 뒤에만 완료로 본다(§7.3).
/// - 네트워크 오류·5xx·429·세션 없음이면 남기고 멈춘다(순서 보존). 다음 flush가 같은 키로 다시 보낸다.
/// - 그 밖의 4xx(version_conflict 등)는 다시 보내도 같으므로 failed로 옮긴다. 화면이 현재 서버 상태를 보여 주고
///   사용자가 다시 적용하게 한다(§7.4).
actor MutationQueue {
    private let api: APIClient
    private let store: MutationQueueStore
    private var snapshot: MutationQueueSnapshot

    init(api: APIClient, store: MutationQueueStore) {
        self.api = api
        self.store = store
        self.snapshot = (try? store.load()).flatMap { $0 } ?? MutationQueueSnapshot()
    }

    var pending: [QueuedMutation] { snapshot.pending }
    var failed: [QueuedMutation] { snapshot.failed }

    @discardableResult
    func enqueue(_ request: APIRequest, kind: QueuedMutation.Kind = .generic) throws -> QueuedMutation {
        let m = QueuedMutation(id: UUID(), idempotencyKey: request.idempotencyKey ?? UUID().uuidString, kind: kind,
                               method: request.method, path: request.path, body: request.body,
                               ifMatchVersion: request.ifMatchVersion, createdAt: Date(), attemptCount: 0, lastError: nil)
        var next = snapshot
        next.pending.append(m)
        try store.save(next)
        snapshot = next
        return m
    }

    @discardableResult
    func enqueueLeaveParty(_ partyID: String) throws -> QueuedMutation {
        try enqueue(APIRequest(method: "DELETE", path: "/v1/parties/\(partyID)/memberships/me"), kind: .leaveParty)
    }

    /// 보낼 수 있는 만큼 보낸다. 남은 개수를 돌려준다.
    @discardableResult
    func flush() async -> Int {
        while let m = snapshot.pending.first {
            var sent = m
            sent.attemptCount += 1
            do {
                _ = try await api.send(m.request)
                try? finish(m.id, failed: nil)
            } catch let e as APIError {
                switch Self.classify(e, kind: m.kind) {
                case .done:
                    try? finish(m.id, failed: nil)
                case .retryLater:
                    sent.lastError = String(describing: e)
                    try? replaceHead(sent)
                    return snapshot.pending.count
                case .rejected:
                    sent.lastError = e.serverCode ?? String(describing: e)
                    try? finish(m.id, failed: sent)
                }
            } catch {
                sent.lastError = String(describing: error)
                try? replaceHead(sent)
                return snapshot.pending.count
            }
        }
        return 0
    }

    func clear() throws {
        try store.clear()
        snapshot = MutationQueueSnapshot()
    }

    enum Outcome: Equatable { case done, retryLater, rejected }

    static func classify(_ error: APIError, kind: QueuedMutation.Kind) -> Outcome {
        switch error {
        case .server(let e):
            if kind == .leaveParty, e.status == 410 { return .done }
            if e.status == 429 || e.status >= 500 { return .retryLater }
            return .rejected
        case .transport, .notSignedIn, .sessionEnded, .decoding:
            // decoding은 2xx 본문을 해석하지 못한 경우가 아니다(send는 본문을 해석하지 않는다). 안전하게 다시 보낸다.
            return .retryLater
        }
    }

    private func replaceHead(_ m: QueuedMutation) throws {
        var next = snapshot
        guard next.pending.first?.id == m.id else { return }
        next.pending[0] = m
        try store.save(next)
        snapshot = next
    }

    private func finish(_ id: UUID, failed: QueuedMutation?) throws {
        var next = snapshot
        next.pending.removeAll { $0.id == id }
        if let failed { next.failed.append(failed) }
        try store.save(next)
        snapshot = next
    }
}
