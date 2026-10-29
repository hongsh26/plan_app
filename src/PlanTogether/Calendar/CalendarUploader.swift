import Foundation

/// `GET/PUT /v1/calendar/connection` 응답의 연결(server calendar.connectionResponse).
struct CalendarConnection: Decodable, Equatable, Sendable {
    struct Source: Codable, Equatable, Sendable {
        let key: String
        let enabled: Bool
    }

    let status: String
    let syncDeviceID: String?
    let isSyncDevice: Bool
    let activeGeneration: Int64
    let lastCompletedSyncAt: Date?
    let timeZone: String?
    let sources: [Source]
    /// 연결이 없으면 0이다(이때 PUT은 If-Match 없는 create-only).
    let version: Int64

    enum CodingKeys: String, CodingKey {
        case status, sources, version
        case syncDeviceID = "sync_device_id"
        case isSyncDevice = "is_sync_device"
        case activeGeneration = "active_generation"
        case lastCompletedSyncAt = "last_completed_sync_at"
        case timeZone = "time_zone"
    }

    /// GET은 6시간 지난 ready를 stale로 보여 준다. 저장된 상태(전이 판정 기준)는 ready다.
    var storedStatus: String { status == "stale" ? "ready" : status }

    var enabledKeys: Set<String> { Set(sources.filter(\.enabled).map(\.key)) }
}

struct CalendarConnectionEnvelope: Decodable {
    let connection: CalendarConnection
}

private struct SnapshotEnvelope: Decodable {
    struct Snapshot: Decodable {
        let id: String
        let revision: Int64
        let status: String
    }

    let snapshot: Snapshot
    let connection: CalendarConnection
}

private struct CreateSnapshotBody: Encodable {
    let revision: Int64
    let windowStart: String
    let windowEnd: String
    let expectedPages: Int
    /// 제목·장소(details)는 V5에서 올린다. I2는 언제나 false다.
    let detailsIncluded = false

    enum CodingKeys: String, CodingKey {
        case revision
        case windowStart = "window_start"
        case windowEnd = "window_end"
        case expectedPages = "expected_pages"
        case detailsIncluded = "details_included"
    }
}

private struct CompleteBody: Encodable {
    let factCount: Int

    enum CodingKeys: String, CodingKey { case factCount = "fact_count" }
}

enum CalendarUploadError: Error, Equatable {
    /// page를 서버가 400으로 거부했다(fact 검증 실패). session은 abort했다.
    case pageRejected(ServerError)
}

/// revision은 연결 안에서 단조 증가해야 한다(서버 last_snapshot_revision). 보내기 전에 저장한다.
protocol RevisionStore: Sendable {
    func lastRevision() async -> Int64
    func setLastRevision(_ revision: Int64) async throws
}

/// snapshot 하나를 올린다(설계 3 §7.2): create → page 전부 → complete.
///
/// - revision은 저장 뒤 보낸다. 409 version_conflict면 서버의 마지막 revision + 1로 한 번 다시 만든다. 같은 revision의
///   기존 session을 돌려받은(200) 경우도 같게 처리한다(이미 중단된 session일 수 있다).
/// - page·complete는 네트워크 오류·5xx·429면 같은 본문(같은 Idempotency-Key)으로 몇 번 다시 보낸다.
/// - 그 밖의 실패면 열린 session을 abort한다(기존 ready generation은 그대로다). 다음 업로드는 새 revision으로 처음부터 한다.
struct CalendarUploader {
    typealias Sleeper = @Sendable (TimeInterval) async throws -> Void

    let api: APIClient
    let revisions: RevisionStore
    var maxAttempts = 3
    /// 세션이 끝났으면(SessionGate 닫힘) 요청을 보내지 않는다.
    var isActive: @Sendable () -> Bool = { true }
    var sleep: Sleeper = { try await Task.sleep(nanoseconds: UInt64(max(0, $0) * 1_000_000_000)) }

    func upload(pages: [Data], factCount: Int, window: SnapshotWindow) async throws -> CalendarConnection {
        let sessionID = try await createSession(expectedPages: pages.count, window: window)
        do {
            for (n, body) in pages.enumerated() {
                let req = APIRequest(method: "PUT", path: "/v1/calendar/snapshots/\(sessionID)/pages/\(n)", body: body,
                                     idempotencyKey: UUID().uuidString)
                try checkActive()
                do {
                    _ = try await withRetry { try await api.send(req) }
                } catch APIError.server(let e) where e.status == 400 {
                    // page 내용(fact 검증)을 서버가 거부했다. 호출자가 원인별로 다시 시도할 수 있게 구별한다.
                    throw CalendarUploadError.pageRejected(e)
                }
            }
            var complete = try APIRequest.json("POST", "/v1/calendar/snapshots/\(sessionID)/complete", body: CompleteBody(factCount: factCount))
            complete.idempotencyKey = UUID().uuidString
            try checkActive()
            let done = try await withRetry { try await api.send(complete, as: SnapshotEnvelope.self) }
            return done.connection
        } catch {
            if !Self.sessionGone(error) { await abort(sessionID) }
            throw error
        }
    }

    /// 같은 revision의 기존 session(200)을 돌려받으면 열려 있어도 이어 쓰지 않고 revision을 올린다. 옛 session의 다른
    /// page에 남은 staging과 새 page의 key가 겹칠 수 있고, 새 revision의 create가 서버에서 옛 열린 session을 abort한다.
    /// 409면 서버의 마지막 revision + 1. 최대 세 번.
    private func createSession(expectedPages: Int, window: SnapshotWindow) async throws -> String {
        var revision = await revisions.lastRevision() + 1
        for _ in 0..<3 {
            try await revisions.setLastRevision(revision)
            var req = try APIRequest.json("POST", "/v1/calendar/snapshots", body: CreateSnapshotBody(
                revision: revision, windowStart: RFC3339.format(window.start), windowEnd: RFC3339.format(window.end),
                expectedPages: expectedPages))
            req.idempotencyKey = UUID().uuidString
            try checkActive()
            do {
                let response = try await withRetry { try await api.send(req) }
                let snapshot = try decodeSnapshot(response.data)
                if response.status == 201 { return snapshot.id }
                revision += 1
            } catch APIError.server(let e) where e.code == "version_conflict" {
                revision = max(revision, e.currentVersion ?? revision) + 1
            }
        }
        throw APIError.decoding("snapshot session을 만들지 못했다")
    }

    private func checkActive() throws {
        guard isActive() else { throw SessionGate.Closed() }
    }

    private func decodeSnapshot(_ data: Data) throws -> SnapshotEnvelope.Snapshot {
        do { return try JSONCoding.makeDecoder().decode(SnapshotEnvelope.self, from: data).snapshot }
        catch { throw APIError.decoding(String(describing: error)) }
    }

    private func abort(_ sessionID: String) async {
        var req = APIRequest(method: "POST", path: "/v1/calendar/snapshots/\(sessionID)/abort")
        req.idempotencyKey = UUID().uuidString
        _ = try? await api.send(req)
    }

    /// 세션이 끝났거나 이 기기 것이 아니면 abort해도 소용없다.
    private static func sessionGone(_ error: Error) -> Bool {
        switch error as? APIError {
        case .sessionEnded?, .notSignedIn?: return true
        case nil:
            if case CalendarUploadError.pageRejected? = error as? CalendarUploadError { return false }
            return error is SessionGate.Closed
        case .server(let e)?: return e.status == 403 || e.code == "calendar_snapshot_state"
        default: return false
        }
    }

    private func withRetry<T>(_ op: () async throws -> T) async throws -> T {
        var attempt = 0
        while true {
            attempt += 1
            do {
                return try await op()
            } catch let error as APIError {
                guard attempt < maxAttempts, let wait = Self.retryDelay(error) else { throw error }
                try await sleep(wait)
            }
        }
    }

    /// 다시 보낼 오류면 기다릴 시간, 아니면 nil.
    static func retryDelay(_ error: APIError) -> TimeInterval? {
        switch error {
        case .transport: return 1
        case .server(let e) where e.status == 429 || e.status >= 500: return e.retryAfter ?? 1
        default: return nil
        }
    }
}
