import XCTest
@testable import PlanTogether

final class MemoryRevisionStore: RevisionStore, @unchecked Sendable {
    private let lock = NSLock()
    private var value: Int64
    init(_ v: Int64 = 0) { value = v }
    func lastRevision() -> Int64 { lock.lock(); defer { lock.unlock() }; return value }
    func setLastRevision(_ r: Int64) throws { lock.lock(); value = r; lock.unlock() }
}

enum CalendarMock {
    static func connection(_ status: String = "ready", version: Int64 = 3, isSyncDevice: Bool = true, syncDevice: String? = "22222222-2222-2222-2222-222222222222",
                           sources: [[String: Any]] = [], tz: String? = "Asia/Seoul") -> [String: Any] {
        ["status": status, "sync_device_id": syncDevice as Any? ?? NSNull(), "is_sync_device": isSyncDevice, "active_generation": 1,
         "last_completed_sync_at": "2026-10-29T03:00:00Z", "time_zone": tz as Any? ?? NSNull(), "sources": sources, "version": version]
    }

    static func snapshot(id: String = "s-1", revision: Int64, status: String = "open", connection: [String: Any] = connection("syncing")) -> [String: Any] {
        ["request_id": "r", "snapshot": ["id": id, "revision": revision, "window_start": "x", "window_end": "y",
                                         "expected_pages": 1, "status": status, "fact_count": NSNull(), "generation": NSNull()],
         "connection": connection]
    }
}

final class CalendarUploaderTests: XCTestCase {
    private let window = SnapshotWindow(start: RFC3339.parse("2026-10-28T15:00:00Z")!, end: RFC3339.parse("2027-01-27T15:00:00Z")!)

    private func api() -> APIClient {
        APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                  tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
    }

    private func uploader(_ store: RevisionStore, sleeps: SleepLog = SleepLog()) -> CalendarUploader {
        var u = CalendarUploader(api: api(), revisions: store)
        u.sleep = { sleeps.append($0) }
        return u
    }

    private func pages(_ n: Int) -> [Data] { (0..<n).map { Data(#"{"facts":[{"n":\#($0)}]}"#.utf8) } }

    private func json(_ data: Data?) -> [String: Any] {
        (data.flatMap { try? JSONSerialization.jsonObject(with: $0) } as? [String: Any]) ?? [:]
    }

    func testHappyPathCreatesPagesCompletes() async throws {
        let store = MemoryRevisionStore(4)
        MockURLProtocol.install { req, _ in
            switch (req.httpMethod!, req.url!.path) {
            case ("POST", "/v1/calendar/snapshots"):
                XCTAssertEqual(store.lastRevision(), 5, "보내기 전에 revision을 저장한다")
                return .json(201, CalendarMock.snapshot(revision: 5))
            case ("PUT", let p) where p.hasPrefix("/v1/calendar/snapshots/s-1/pages/"): return .json(200, ["ok": true])
            case ("POST", "/v1/calendar/snapshots/s-1/complete"):
                return .json(200, CalendarMock.snapshot(revision: 5, status: "completed", connection: CalendarMock.connection("ready", version: 9)))
            default: return .error(404, code: "not_found")
            }
        }
        let conn = try await uploader(store).upload(pages: pages(2), factCount: 2, window: window)
        XCTAssertEqual(conn.status, "ready")
        XCTAssertEqual(conn.version, 9)
        let reqs = MockURLProtocol.requests
        XCTAssertEqual(reqs.map { "\($0.request.httpMethod!) \($0.request.url!.path)" }, [
            "POST /v1/calendar/snapshots", "PUT /v1/calendar/snapshots/s-1/pages/0", "PUT /v1/calendar/snapshots/s-1/pages/1",
            "POST /v1/calendar/snapshots/s-1/complete",
        ])
        let create = json(reqs[0].body)
        XCTAssertEqual(create["revision"] as? Int, 5)
        XCTAssertEqual(create["expected_pages"] as? Int, 2)
        XCTAssertEqual(create["details_included"] as? Bool, false)
        XCTAssertEqual(create["window_start"] as? String, "2026-10-28T15:00:00Z")
        XCTAssertEqual(reqs[1].body, pages(2)[0], "page 본문은 그대로 보낸다")
        XCTAssertEqual(String(data: reqs[3].body!, encoding: .utf8), #"{"fact_count":2}"#)
    }

    func testRevisionConflictUsesServerRevisionPlusOne() async throws {
        let store = MemoryRevisionStore(0)
        MockURLProtocol.install { req, body in
            if req.url!.path == "/v1/calendar/snapshots" {
                let rev = (try? JSONSerialization.jsonObject(with: body!) as? [String: Any])?["revision"] as? Int
                return rev == 1 ? .error(409, code: "version_conflict", currentVersion: 41) : .json(201, CalendarMock.snapshot(revision: 42))
            }
            if req.url!.path.hasSuffix("/complete") { return .json(200, CalendarMock.snapshot(revision: 42, status: "completed")) }
            return .json(200, [:])
        }
        _ = try await uploader(store).upload(pages: [], factCount: 0, window: window)
        XCTAssertEqual(store.lastRevision(), 42)
        let revs = MockURLProtocol.requests.filter { $0.request.url!.path == "/v1/calendar/snapshots" }.map { json($0.body)["revision"] as? Int }
        XCTAssertEqual(revs, [1, 42])
    }

    func testReusedSessionStartsNewRevision() async throws {
        let store = MemoryRevisionStore(6)
        MockURLProtocol.install { req, body in
            if req.url!.path == "/v1/calendar/snapshots" {
                let rev = (try? JSONSerialization.jsonObject(with: body!) as? [String: Any])?["revision"] as? Int
                return rev == 7 ? .json(200, CalendarMock.snapshot(id: "old", revision: 7, status: "aborted")) : .json(201, CalendarMock.snapshot(id: "s-1", revision: 8))
            }
            if req.url!.path.hasSuffix("/complete") { return .json(200, CalendarMock.snapshot(revision: 8, status: "completed")) }
            return .json(200, [:])
        }
        _ = try await uploader(store).upload(pages: pages(1), factCount: 1, window: window)
        XCTAssertEqual(store.lastRevision(), 8)
        XCTAssertTrue(MockURLProtocol.requests.contains { $0.request.url!.path == "/v1/calendar/snapshots/s-1/pages/0" })
    }

    func testPageTransportFailureIsRetriedWithSameKeyAndBody() async throws {
        let attempts = Counter()
        MockURLProtocol.install { req, _ in
            switch req.url!.path {
            case "/v1/calendar/snapshots": return .json(201, CalendarMock.snapshot(revision: 1))
            case "/v1/calendar/snapshots/s-1/pages/0":
                attempts.increment()
                if attempts.value == 1 { throw URLError(.networkConnectionLost) }
                if attempts.value == 2 { return .error(429, code: "rate_limited", headers: ["Retry-After": "4"]) }
                return .json(200, [:])
            default: return .json(200, CalendarMock.snapshot(revision: 1, status: "completed"))
            }
        }
        let sleeps = SleepLog()
        _ = try await uploader(MemoryRevisionStore(), sleeps: sleeps).upload(pages: pages(1), factCount: 1, window: window)
        XCTAssertEqual(attempts.value, 3)
        XCTAssertEqual(sleeps.values, [1, 4])
        let sent = MockURLProtocol.requests.filter { $0.request.url!.path.hasSuffix("/pages/0") }
        XCTAssertEqual(Set(sent.compactMap { $0.request.value(forHTTPHeaderField: "Idempotency-Key") }).count, 1)
        XCTAssertEqual(Set(sent.compactMap(\.body)).count, 1)
    }

    func testRejectedPageAbortsSession() async throws {
        MockURLProtocol.install { req, _ in
            switch req.url!.path {
            case "/v1/calendar/snapshots": return .json(201, CalendarMock.snapshot(revision: 1))
            case "/v1/calendar/snapshots/s-1/pages/0": return .error(400, code: "invalid_request")
            case "/v1/calendar/snapshots/s-1/abort": return .json(200, CalendarMock.snapshot(revision: 1, status: "aborted"))
            default: return .error(500, code: "internal")
            }
        }
        do {
            _ = try await uploader(MemoryRevisionStore()).upload(pages: pages(2), factCount: 2, window: window)
            XCTFail("오류여야 한다")
        } catch let e as APIError {
            XCTAssertEqual(e.serverCode, "invalid_request")
        }
        let paths = MockURLProtocol.requests.map { $0.request.url!.path }
        XCTAssertEqual(paths, ["/v1/calendar/snapshots", "/v1/calendar/snapshots/s-1/pages/0", "/v1/calendar/snapshots/s-1/abort"])
        let abort = MockURLProtocol.requests.last!
        XCTAssertNil(abort.body, "abort는 본문이 없다")
        XCTAssertNotNil(abort.request.value(forHTTPHeaderField: "Idempotency-Key"))
    }

    func testIncompleteCompleteAbortsAndClosedSessionDoesNot() async throws {
        for code in ["calendar_snapshot_incomplete", "calendar_snapshot_state"] {
            MockURLProtocol.install { req, _ in
                switch req.url!.path {
                case "/v1/calendar/snapshots": return .json(201, CalendarMock.snapshot(revision: 1))
                case "/v1/calendar/snapshots/s-1/complete": return .error(409, code: code)
                default: return .json(200, CalendarMock.snapshot(revision: 1, status: "aborted"))
                }
            }
            _ = try? await uploader(MemoryRevisionStore()).upload(pages: [], factCount: 0, window: window)
            let aborted = MockURLProtocol.requests.contains { $0.request.url!.path.hasSuffix("/abort") }
            XCTAssertEqual(aborted, code == "calendar_snapshot_incomplete", code)
        }
    }

    /// Low: 매 시도의 409와 닫힌 session(200)을 처리한다. 409(서버 5) → 6이 중단된 기존 session(200) → 7 생성.
    func testRepeatedConflictAndClosedSessionAreHandled() async throws {
        let store = MemoryRevisionStore(0)
        MockURLProtocol.install { req, body in
            if req.url!.path == "/v1/calendar/snapshots" {
                let rev = (try? JSONSerialization.jsonObject(with: body!) as? [String: Any])?["revision"] as? Int
                switch rev {
                case 1: return .error(409, code: "version_conflict", currentVersion: 5)
                case 6: return .json(200, CalendarMock.snapshot(id: "old", revision: 6, status: "completed"))
                default: return .json(201, CalendarMock.snapshot(id: "s-1", revision: 7))
                }
            }
            if req.url!.path.hasSuffix("/complete") { return .json(200, CalendarMock.snapshot(revision: 7, status: "completed")) }
            return .json(200, [:])
        }
        _ = try await uploader(store).upload(pages: pages(1), factCount: 1, window: window)
        XCTAssertEqual(store.lastRevision(), 7)
        XCTAssertFalse(MockURLProtocol.requests.contains { $0.request.url!.path.contains("/old/") })
    }

    /// 같은 revision의 열린 session(200)은 이어 쓴다.
    func testReusedOpenSessionIsContinued() async throws {
        MockURLProtocol.install { req, _ in
            if req.url!.path == "/v1/calendar/snapshots" { return .json(200, CalendarMock.snapshot(id: "open-1", revision: 1, status: "open")) }
            if req.url!.path.hasSuffix("/complete") { return .json(200, CalendarMock.snapshot(revision: 1, status: "completed")) }
            return .json(200, [:])
        }
        _ = try await uploader(MemoryRevisionStore()).upload(pages: pages(1), factCount: 1, window: window)
        XCTAssertTrue(MockURLProtocol.requests.contains { $0.request.url!.path == "/v1/calendar/snapshots/open-1/pages/0" })
    }
}
