import XCTest
@testable import PlanTogether

/// 서버 Go 테스트가 기록한 실제 응답(Fixtures/sync_*.json)을 적용기에 넣어 증분 결과가 새 bootstrap과 같은지 본다.
/// 생성 방법은 server/internal/syncfeed/swift_fixture_test.go 머리말에 있다.
///
/// 비교: Party 범위 entity는 version과 payload, user·device는 version만 본다(device last_seen_at은 version 없이 바뀐다).
final class SyncFixtureTests: XCTestCase {
    private struct Fixture: Decodable {
        let scenario: String
        let divergesWithout: [String]
        let sessions: [Session]

        enum CodingKeys: String, CodingKey {
            case scenario, sessions
            case divergesWithout = "diverges_without"
        }
    }

    private struct Session: Decodable {
        let name: String
        let selfUserID: String
        let bootstrap: BootstrapResponse
        let pages: [SyncPage]
        let checkpoints: [Checkpoint]
        let expected: [ExpectedEntity]

        enum CodingKeys: String, CodingKey {
            case name, bootstrap, pages, checkpoints, expected
            case selfUserID = "self_user_id"
        }
    }

    private struct Checkpoint: Decodable {
        let afterPages: Int
        let entities: [ExpectedEntity]

        enum CodingKeys: String, CodingKey {
            case entities
            case afterPages = "after_pages"
        }
    }

    private struct ExpectedEntity: Decodable {
        let entityType: String
        let entityID: String
        let version: Int64
        let payload: JSONValue

        enum CodingKeys: String, CodingKey {
            case version, payload
            case entityType = "entity_type"
            case entityID = "entity_id"
        }
    }

    static let expectedScenarios: Set<String> = [
        "sequential_acceptance", "leave_vs_rename", "transfer_vs_accept", "rejoin_vs_leave", "rename_vs_join",
        "bootstrap_ended_membership_floor", "bootstrap_tombstone_floor", "device_revoke_floor", "user_rename_reorder",
    ]

    private func loadFixtures() throws -> [Fixture] {
        let urls = Bundle(for: Self.self).urls(forResourcesWithExtension: "json", subdirectory: nil) ?? []
        let fixtures = try urls.filter { $0.lastPathComponent.hasPrefix("sync_") }.map {
            try JSONCoding.makeDecoder().decode(Fixture.self, from: Data(contentsOf: $0))
        }
        XCTAssertEqual(Set(fixtures.map(\.scenario)), Self.expectedScenarios, "fixture가 test bundle에 모두 있어야 한다")
        return fixtures
    }

    /// 세션 하나를 재생하고 checkpoint·최종 비교 결과(차이 목록)를 돌려준다.
    private func replay(_ s: Session, options: SyncApplier.Options) throws -> [String] {
        var applier = SyncApplier(state: SyncState(userID: s.selfUserID), options: options)
        try applier.applyBootstrap(s.bootstrap)
        var diffs: [String] = []
        var checkpoints = s.checkpoints[...]
        func check(_ applied: Int) {
            while let cp = checkpoints.first, cp.afterPages == applied {
                checkpoints.removeFirst()
                diffs += diff(applier.state, cp.entities, partyOnly: true).map { "[checkpoint \(applied)] \($0)" }
            }
        }
        check(0)
        for (i, page) in s.pages.enumerated() {
            applier.apply(page: page)
            check(i + 1)
        }
        XCTAssertTrue(checkpoints.isEmpty, "\(s.name): 쓰지 않은 checkpoint")
        XCTAssertEqual(applier.state.cursor, s.pages.last?.cursor ?? s.bootstrap.cursorWatermark)
        // 이 세션이 bootstrap·페이지로 한 번이라도 받은 기기. 받은 적 없는 기기만 누락을 허용한다(diff 참고).
        let seenDevices = Set((s.bootstrap.snapshot + s.pages.flatMap(\.changes))
            .filter { $0.entityType == SyncEntityType.device }.map(\.entityID))
        diffs += diff(applier.state, s.expected, partyOnly: false, seenDevices: seenDevices).map { "[final] \($0)" }
        return diffs
    }

    private func diff(_ state: SyncState, _ expected: [ExpectedEntity], partyOnly: Bool, seenDevices: Set<String> = []) -> [String] {
        var out: [String] = []
        var want: [EntityKey: ExpectedEntity] = [:]
        for e in expected { want[EntityKey(e.entityType, e.entityID)] = e }
        let got = state.entities.filter { !partyOnly || SyncEntityType.partyScoped.contains($0.key.type) }
        for (k, w) in want {
            guard let g = got[k] else {
                // 서버 로그인은 새 기기의 sync 변경을 발행하지 않는다(auth signInTx). 다른 세션은 새 기기를 bootstrap이나
                // GET /v1/devices로만 안다. 그래서 이 세션이 한 번도 받지 못한 기기의 누락만 허용한다. 받은 기기가
                // 빠지면(잘못된 폐기 처리) 실패다.
                if k.type != SyncEntityType.device || seenDevices.contains(k.id) { out.append("누락 \(k) v\(w.version)") }
                continue
            }
            if g.version != w.version {
                out.append("version \(k) 증분 v\(g.version) / bootstrap v\(w.version)")
            } else if SyncEntityType.partyScoped.contains(k.type), g.payload != w.payload {
                out.append("payload \(k) 증분 \(g.payload) / bootstrap \(w.payload)")
            }
        }
        for k in got.keys where want[k] == nil { out.append("남음 \(k)") }
        return out.sorted()
    }

    func testIncrementalReplayMatchesBootstrap() throws {
        for f in try loadFixtures() {
            for s in f.sessions {
                let diffs = try replay(s, options: SyncApplier.Options())
                XCTAssertEqual(diffs, [], "\(f.scenario)/\(s.name)")
                XCTAssertFalse(s.expected.isEmpty, "\(f.scenario)/\(s.name): 기대 모델이 비었다")
            }
        }
    }

    /// 대조군: 시나리오가 실제로 규칙을 시험한다는 증거다. 해당 스위치를 끈 적용기는 bootstrap과 달라져야 한다.
    func testControlsDivergeWhereRulesMatter() throws {
        var exercised: Set<String> = []
        for f in try loadFixtures() {
            for control in f.divergesWithout {
                var options = SyncApplier.Options()
                switch control {
                case "rules": options.rules = false
                case "floors": options.floors = false
                case "orphans": options.keepOrphans = false
                default: XCTFail("모르는 대조군 \(control)")
                }
                let diverged = try f.sessions.contains { try !replay($0, options: options).isEmpty }
                XCTAssertTrue(diverged, "\(f.scenario): \(control)를 꺼도 bootstrap과 같다. fixture가 규칙을 시험하지 못한다")
                exercised.insert(control)
            }
        }
        XCTAssertEqual(exercised, ["rules", "floors", "orphans"])
    }

    /// 순차 시나리오는 작은 페이지(limit=2)와 늦은 bootstrap 세션을 포함해야 한다.
    func testSequentialFixtureCoversPagingAndLateSession() throws {
        let f = try XCTUnwrap(loadFixtures().first { $0.scenario == "sequential_acceptance" })
        XCTAssertEqual(Set(f.sessions.map(\.name)), ["A1", "A2", "B1", "B2", "C1"])
        let a2 = try XCTUnwrap(f.sessions.first { $0.name == "A2" })
        XCTAssertTrue(a2.pages.contains { $0.hasMore }, "A2는 has_more 페이지를 지나야 한다")
    }
}
