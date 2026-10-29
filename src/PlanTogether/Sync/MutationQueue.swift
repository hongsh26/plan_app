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
/// userID는 주인이다. 다른 사용자(또는 주인이 없는 옛 파일)의 큐는 읽지 않고 버린다.
struct MutationQueueSnapshot: Codable, Equatable, Sendable {
    var userID: String?
    var pending: [QueuedMutation] = []
    var failed: [QueuedMutation] = []

    enum CodingKeys: String, CodingKey {
        case pending, failed
        case userID = "user_id"
    }
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

struct GatedMutationQueueStore: MutationQueueStore {
    let base: MutationQueueStore
    let gate: SessionGate

    func load() throws -> MutationQueueSnapshot? { try base.load() }
    func save(_ snapshot: MutationQueueSnapshot) throws { try gate.write { try base.save(snapshot) } }
    func clear() throws { try base.clear() }
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
/// - 성공(2xx, 탈퇴는 410 party_disbanded도)이면 지운다. 서버 성공 응답 뒤에만 완료로 본다(§7.3).
/// - 네트워크 오류·5xx·429·401이면 남기고 멈춘다(순서 보존). 다음 flush가 같은 키로 다시 보낸다.
/// - 세션이 끝났으면(로그아웃·세션 종료) 아무것도 저장하지 않고 멈춘다. 큐는 SessionStore가 지운다.
/// - 저장이 실패하면 멈춘다. 한 flush는 시작할 때 있던 mutation만큼만 돈다.
/// - 그 밖의 4xx(version_conflict 등)는 다시 보내도 같으므로 failed로 옮긴다. 화면이 현재 서버 상태를 보여 주고
///   사용자가 다시 적용하게 한다(§7.4).
actor MutationQueue {
    private let api: APIClient
    private let store: MutationQueueStore
    private var snapshot: MutationQueueSnapshot
    private var flushing: Task<Int, Never>?

    init(api: APIClient, store: MutationQueueStore, userID: String) {
        self.api = api
        self.store = store
        let saved = (try? store.load()).flatMap { $0 }
        self.snapshot = saved?.userID == userID ? saved! : MutationQueueSnapshot(userID: userID)
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

    /// 보낼 수 있는 만큼 보낸다. 남은 개수를 돌려준다. 동시에 부르면 진행 중인 한 번을 함께 기다린다. 겹쳐 돌면
    /// 같은 mutation을 같은 키로 동시에 보내고 두 결과를 따로 처리하게 된다.
    @discardableResult
    func flush() async -> Int {
        if let flushing { return await flushing.value }
        let task = Task { await self.drain() }
        flushing = task
        defer { flushing = nil }
        return await task.value
    }

    private func drain() async -> Int {
        var budget = snapshot.pending.count
        while budget > 0, let m = snapshot.pending.first {
            budget -= 1
            var sent = m
            sent.attemptCount += 1
            let outcome: Outcome
            do {
                _ = try await api.send(m.request)
                outcome = .done
            } catch let e as APIError {
                outcome = Self.classify(e, kind: m.kind)
                sent.lastError = e.serverCode ?? String(describing: e)
            } catch {
                outcome = .retryLater
                sent.lastError = String(describing: error)
            }
            do {
                switch outcome {
                case .done: try finish(m.id, failed: nil)
                case .rejected: try finish(m.id, failed: sent)
                case .retryLater:
                    try replaceHead(sent)
                    return snapshot.pending.count
                case .stop:
                    return snapshot.pending.count
                }
            } catch {
                // 저장 실패(세션이 닫힌 경우 포함). 같은 mutation을 이번 flush에서 다시 보내지 않고 멈춘다. 서버가 이미
                // 처리했다면 다음 재전송은 같은 키라 원래 결과를 돌려받는다.
                return snapshot.pending.count
            }
        }
        return snapshot.pending.count
    }

    func clear() throws {
        try store.clear()
        snapshot = MutationQueueSnapshot(userID: snapshot.userID)
    }

    enum Outcome: Equatable { case done, retryLater, rejected, stop }

    static func classify(_ error: APIError, kind: QueuedMutation.Kind) -> Outcome {
        switch error {
        case .server(let e):
            // code로 판정한다(§6). 410은 sync_cursor_expired 등 다른 뜻도 있다.
            if kind == .leaveParty, e.status == 410, e.code == "party_disbanded" { return .done }
            // 401은 refresh 뒤 재전송도 거절된 경우다. 세션 문제이지 mutation 문제가 아니므로 남긴다.
            if e.status == 401 || e.status == 429 || e.status >= 500 { return .retryLater }
            return .rejected
        case .notSignedIn, .sessionEnded:
            return .stop
        case .transport, .decoding:
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
