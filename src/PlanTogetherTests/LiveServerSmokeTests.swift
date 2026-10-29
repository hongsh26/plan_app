import XCTest
@testable import PlanTogether

/// 실제 로컬 서버를 대상으로 하는 선택 실행 테스트. 서버가 없으면 건너뛴다(xcodebuild test를 깨지 않는다).
///
/// 실행: server/scripts/run_api_dev.sh로 서버를 띄운 뒤
///   TEST_RUNNER_PLAN_LIVE_SERVER_URL=http://localhost:8080 xcodebuild ... test -only-testing:PlanTogetherTests/LiveServerSmokeTests
final class LiveServerSmokeTests: XCTestCase {
    func testDevLoginBootstrapAndIncrementalSync() async throws {
        guard let raw = ProcessInfo.processInfo.environment["PLAN_LIVE_SERVER_URL"], let base = URL(string: raw) else {
            throw XCTSkip("PLAN_LIVE_SERVER_URL이 없다(선택 실행)")
        }
        let api = APIClient(baseURL: base, tokenStore: InMemoryTokenStore())
        let name = "smoke.\(Int(Date().timeIntervalSince1970))"
        let tokens = try await api.signIn(SignInRequest(identityToken: "dev:\(name)", authorizationCode: "dev", rawNonce: "dev", displayName: "스모크"))

        let me = try await api.send(.get("/v1/me"), as: MeResponse.self)
        XCTAssertEqual(me.user.id, tokens.userID)

        let engine = SyncEngine(api: api, store: InMemorySyncStore(), userID: tokens.userID)
        var state = try await engine.sync()
        XCTAssertEqual(state.entities[EntityKey(SyncEntityType.user, tokens.userID)]?.payload["display_name"], .string("스모크"))
        XCTAssertNotNil(state.entities[EntityKey(SyncEntityType.device, tokens.deviceID)])
        XCTAssertNotNil(state.cursor)

        // Party 생성은 증분으로 와야 한다. horizon이 잠시 늦을 수 있어 몇 번 읽는다.
        _ = try await api.send(try .json("POST", "/v1/parties", body: ["name": "스모크 Party"]))
        for _ in 0..<20 {
            state = try await engine.sync()
            if !state.entities(ofType: SyncEntityType.party).isEmpty { break }
            try await Task.sleep(nanoseconds: 100_000_000)
        }
        let parties = state.entities(ofType: SyncEntityType.party)
        XCTAssertEqual(parties.map { $0.entity.payload["name"] }, [.string("스모크 Party")])
        XCTAssertEqual(state.entities(ofType: SyncEntityType.partyMembership).count, 1)

        await api.logout()
        let after = await api.currentTokens
        XCTAssertNil(after)
    }
}
