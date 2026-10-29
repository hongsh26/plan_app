import XCTest
@testable import PlanTogether

final class SearchModelTests: XCTestCase {
    private let seoul = TimeZone(identifier: "Asia/Seoul")!
    private let newYork = TimeZone(identifier: "America/New_York")!
    private let now = RFC3339.parse("2026-10-29T03:00:00Z")!  // 서울 10-29 12:00

    func testDefaultsAre14DaysAnd60Minutes() {
        let f = SearchForm.defaults(now: now, timeZone: seoul)
        XCTAssertEqual(SearchForm.day(f.startDate, seoul), "2026-10-29")
        XCTAssertEqual(SearchForm.day(f.endDate, seoul), "2026-11-11", "오늘 포함 14일째")
        XCTAssertEqual(f.durationMinutes, 60)
        XCTAssertNil(f.validate(now: now))
        let body = try! JSONSerialization.jsonObject(with: JSONCoding.makeEncoder().encode(f.body)) as! [String: Any]
        XCTAssertEqual(body["start_date"] as? String, "2026-10-29")
        XCTAssertEqual(body["end_date"] as? String, "2026-11-11")
        XCTAssertEqual(body["duration_minutes"] as? Int, 60)
        XCTAssertEqual(body["time_zone"] as? String, "Asia/Seoul")
        XCTAssertEqual(body["max_results"] as? Int, 20)
        XCTAssertEqual(Set(body.keys), ["start_date", "end_date", "duration_minutes", "time_zone", "max_results"], "멤버·공개 수준은 보내지 않는다")
    }

    func testValidation() {
        let cal = SnapshotWindow.calendar(seoul)
        var f = SearchForm.defaults(now: now, timeZone: seoul)
        f.startDate = cal.date(byAdding: .day, value: -1, to: f.startDate)!
        XCTAssertNotNil(f.validate(now: now), "과거 시작일")
        f = .defaults(now: now, timeZone: seoul)
        f.endDate = cal.date(byAdding: .day, value: -1, to: f.startDate)!
        XCTAssertNotNil(f.validate(now: now), "끝 < 시작")
        f = .defaults(now: now, timeZone: seoul)
        f.endDate = cal.date(byAdding: .day, value: 30, to: f.startDate)!
        XCTAssertNil(f.validate(now: now), "31일은 허용")
        f.endDate = cal.date(byAdding: .day, value: 31, to: f.startDate)!
        XCTAssertNotNil(f.validate(now: now), "32일")
        f = .defaults(now: now, timeZone: seoul)
        f.startDate = cal.date(byAdding: .day, value: 80, to: f.startDate)!
        f.endDate = cal.date(byAdding: .day, value: 90, to: cal.startOfDay(for: now))!
        XCTAssertNotNil(f.validate(now: now), "오늘 + 89일 초과")
        f = .defaults(now: now, timeZone: seoul)
        f.durationMinutes = 45
        XCTAssertNotNil(f.validate(now: now))
        f.durationMinutes = 510
        XCTAssertNotNil(f.validate(now: now))
        f = .defaults(now: now, timeZone: TimeZone(identifier: "GMT")!)
        XCTAssertNotNil(f.validate(now: now), "Area/City가 아닌 시간대")
    }

    func testSectionsGroupByRequestTimeZoneDate() {
        let slots: [(Date, Date)] = [
            ("2026-10-30T01:00:00Z", "2026-10-30T02:00:00Z"),
            ("2026-10-29T16:00:00Z", "2026-10-29T17:00:00Z"),  // 서울 10-30 01:00
            ("2026-10-31T05:00:00Z", "2026-10-31T06:00:00Z"),
        ].map { (RFC3339.parse($0.0)!, RFC3339.parse($0.1)!) }
        let sections = SearchResultFormatter.sections(slots, timeZone: seoul)
        XCTAssertEqual(sections.map(\.id), ["2026-10-30", "2026-10-31"])
        XCTAssertEqual(sections[0].slots.map(\.label), ["01:00–02:00 GMT+9", "10:00–11:00 GMT+9"])
        XCTAssertEqual(sections[0].title, "10월 30일 (금)")
    }

    /// DST fall-back: 뉴욕 11/1 01:00은 두 번 온다. 약식 시간대로 구별한다(rec 1401 표시 계약).
    func testDSTDuplicateWallClockIsDistinguished() {
        let slots: [(Date, Date)] = [
            ("2026-11-01T05:00:00Z", "2026-11-01T05:30:00Z"),  // 01:00 EDT
            ("2026-11-01T05:30:00Z", "2026-11-01T06:00:00Z"),  // 01:30 EDT – 01:00 EST
            ("2026-11-01T06:00:00Z", "2026-11-01T06:30:00Z"),  // 01:00 EST
        ].map { (RFC3339.parse($0.0)!, RFC3339.parse($0.1)!) }
        let labels = SearchResultFormatter.sections(slots, timeZone: newYork).flatMap(\.slots).map(\.label)
        XCTAssertEqual(labels, ["01:00–01:30 EDT", "01:30 EDT–01:00 EST", "01:00–01:30 EST"])
        XCTAssertEqual(Set(labels).count, 3)
    }

    @MainActor
    private func makeVM(_ reply: @escaping (URLRequest) -> MockURLProtocol.Reply, now: Date? = nil) -> SearchViewModel {
        MockURLProtocol.install { req, _ in reply(req) }
        let api = APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                            tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
        let t = now ?? self.now
        let box = DateBox(t)
        let vm = SearchViewModel(api: api, partyID: "p-1", timeZone: seoul, now: { box.value })
        self.clock = box
        return vm
    }
    private var clock: DateBox?

    @MainActor
    func testEmptyResultIsNotPending() async {
        let vm = makeVM { _ in .json(200, ["request_id": "r", "slots": [], "calculation_version": "v", "generated_at": "2026-10-29T03:00:00Z", "valid_until": "2026-10-29T03:05:00Z"]) }
        await vm.search()
        XCTAssertEqual(vm.state, .empty)
        XCTAssertEqual(MockURLProtocol.requests.first?.request.url?.path, "/v1/parties/p-1/availability/search")
    }

    @MainActor
    func testPendingCarriesOnlySelfState() async {
        let vm = makeVM { _ in .json(409, ["code": "availability_setup_pending", "message": "m", "request_id": "r", "self": ["activity": "unconfigured", "calendar": "ready"]]) }
        await vm.search()
        XCTAssertEqual(vm.state, .pending(code: "availability_setup_pending", selfActivityUnconfigured: true, selfCalendarState: "ready"))
        let vm2 = makeVM { _ in .json(409, ["code": "calendar_sync_pending", "message": "m", "request_id": "r", "self": ["activity": "configured", "calendar": "ready"]]) }
        await vm2.search()
        // 본인은 준비됐고 다른 멤버를 기다린다: CTA 없음.
        XCTAssertEqual(vm2.state, .pending(code: "calendar_sync_pending", selfActivityUnconfigured: false, selfCalendarState: "ready"))
    }

    @MainActor
    func testResultsExpireAtValidUntil() async {
        let vm = makeVM { _ in .json(200, ["request_id": "r", "slots": [["start_at": "2026-10-30T01:00:00Z", "end_at": "2026-10-30T02:00:00Z"]],
                                             "calculation_version": "v", "generated_at": "2026-10-29T03:00:00Z", "valid_until": "2026-10-29T03:05:00Z"]) }
        await vm.search()
        guard case .results(let sections, _) = vm.state else { return XCTFail("결과여야 한다: \(vm.state)") }
        XCTAssertEqual(sections.count, 1)
        XCTAssertFalse(vm.isExpired)
        clock?.value = RFC3339.parse("2026-10-29T03:05:00Z")!
        XCTAssertTrue(vm.isExpired)
    }

    @MainActor
    func testValidationFailureSendsNothing() async {
        let vm = makeVM { _ in .json(200, [:]) }
        vm.form.durationMinutes = 45
        await vm.search()
        XCTAssertNotNil(vm.validationMessage)
        XCTAssertTrue(MockURLProtocol.requests.isEmpty)
    }
}

final class DateBox: @unchecked Sendable {
    var value: Date
    init(_ v: Date) { value = v }
}

final class PreferencesModelTests: XCTestCase {
    func testWireNormalizesMidnightCrossing() {
        let days: [Int: [EditableInterval]] = [
            1: [EditableInterval(start: 22 * 60, end: 2 * 60)],   // 월 22:00 ~ 화 02:00
            3: [EditableInterval(start: 18 * 60, end: 20 * 60), EditableInterval(start: 9 * 60, end: 12 * 60)],
            7: [EditableInterval(start: 0, end: 0)],              // 일 24시간
        ]
        XCTAssertEqual(PreferencesRules.wire(days), [
            WeeklyInterval(weekday: 1, startMinute: 1320, endMinute: 1560),
            WeeklyInterval(weekday: 3, startMinute: 540, endMinute: 720),
            WeeklyInterval(weekday: 3, startMinute: 1080, endMinute: 1200),
            WeeklyInterval(weekday: 7, startMinute: 0, endMinute: 1440),
        ])
        // 서버 응답(정규형)을 다시 화면 값으로: 끝 1440은 0:00(다음 날 시작)으로 보인다.
        let back = PreferencesRules.editable([WeeklyInterval(weekday: 1, startMinute: 1320, endMinute: 1440), WeeklyInterval(weekday: 2, startMinute: 0, endMinute: 120)])
        XCTAssertEqual(back[1]?.map { [$0.startMinute, $0.endMinute] }, [[1320, 0]])
        XCTAssertEqual(PreferencesRules.wire(back).map(\.endMinute), [1440, 120])
    }

    func testValidation() {
        XCTAssertNotNil(PreferencesRules.validate([:]), "빈 주")
        XCTAssertNotNil(PreferencesRules.validate([1: (0..<4).map { EditableInterval(start: $0 * 120, end: $0 * 120 + 60) }]), "요일당 4개")
        XCTAssertNotNil(PreferencesRules.validate([1: [EditableInterval(start: 540, end: 720), EditableInterval(start: 600, end: 660)]]), "겹침")
        XCTAssertNil(PreferencesRules.validate([1: [EditableInterval(start: 540, end: 720), EditableInterval(start: 720, end: 780)]]), "맞닿음은 서버가 병합")
    }

    func testRegionZoneRule() {
        for ok in ["Asia/Seoul", "America/New_York", "America/Argentina/Buenos_Aires"] { XCTAssertTrue(PreferencesRules.isRegionZone(ok), ok) }
        for bad in ["GMT", "UTC", "Etc/GMT-9", "etc/UTC", "Local", "asia/seoul", "Asia//Seoul", "GMT+0900"] { XCTAssertFalse(PreferencesRules.isRegionZone(bad), bad) }
        XCTAssertTrue(PreferencesRules.regionZones.contains("Asia/Seoul"))
        XCTAssertFalse(PreferencesRules.regionZones.contains("GMT"))
    }

    private func api() -> APIClient {
        APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                  tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
    }

    private static let unconfigured: [String: Any] = ["request_id": "r", "preferences": ["status": "unconfigured", "time_zone": NSNull(), "version": 0, "weekly_intervals": [], "updated_at": NSNull()]]

    @MainActor
    func testUnconfiguredShowsUnsavedSuggestionAndCityPickerForGMT() async {
        MockURLProtocol.install { _, _ in .json(200, Self.unconfigured) }
        let vm = PreferencesViewModel(api: api(), deviceTimeZone: "GMT")
        await vm.load()
        XCTAssertTrue(vm.isSuggestion)
        XCTAssertEqual(vm.days.count, 7)
        XCTAssertEqual(vm.days[1]?.first.map { [$0.startMinute, $0.endMinute] }, [540, 1320])
        XCTAssertTrue(vm.needsCityPicker)
        let saved = await vm.save()
        XCTAssertFalse(saved, "도시를 고르기 전에는 저장하지 않는다")
        XCTAssertEqual(MockURLProtocol.requests.filter { $0.request.httpMethod == "PUT" }.count, 0)
    }

    /// 첫 저장은 If-Match 없는 create-only이고, 실패 뒤 재시도는 같은 Idempotency-Key다(rec 1404 iOS 계약).
    @MainActor
    func testFirstSaveIsCreateOnlyAndRetryReusesKey() async {
        let attempts = Counter()
        MockURLProtocol.install { req, _ in
            if req.httpMethod == "GET" { return .json(200, Self.unconfigured) }
            attempts.increment()
            if attempts.value == 1 { return .error(503, code: "internal") }
            return .json(200, ["request_id": "r", "preferences": ["status": "configured", "time_zone": "Asia/Seoul", "version": 1,
                                                                  "weekly_intervals": [["weekday": 1, "start_minute": 540, "end_minute": 1320]], "updated_at": "2026-10-29T03:00:00Z"]])
        }
        let vm = PreferencesViewModel(api: api(), deviceTimeZone: "Asia/Seoul")
        await vm.load()
        let first = await vm.save()
        let second = await vm.save()
        XCTAssertFalse(first)
        XCTAssertTrue(second)
        XCTAssertEqual(vm.status, "configured")
        XCTAssertEqual(vm.version, 1)
        XCTAssertFalse(vm.isSuggestion)
        let puts = MockURLProtocol.requests.filter { $0.request.httpMethod == "PUT" }
        XCTAssertEqual(puts.count, 2)
        XCTAssertTrue(puts.allSatisfy { $0.request.value(forHTTPHeaderField: "If-Match") == nil })
        XCTAssertEqual(Set(puts.compactMap { $0.request.value(forHTTPHeaderField: "Idempotency-Key") }).count, 1)
        let body = try! JSONSerialization.jsonObject(with: puts[0].body!) as! [String: Any]
        XCTAssertEqual(body["time_zone"] as? String, "Asia/Seoul")
        XCTAssertEqual((body["weekly_intervals"] as? [Any])?.count, 7)
    }

    @MainActor
    func testConflictReloadsAndTimeZoneInvalidOpensPicker() async {
        let configured: [String: Any] = ["request_id": "r", "preferences": ["status": "configured", "time_zone": "Asia/Seoul", "version": 4,
                                                                            "weekly_intervals": [["weekday": 2, "start_minute": 600, "end_minute": 660]], "updated_at": "2026-10-29T03:00:00Z"]]
        var putCode = "version_conflict"
        MockURLProtocol.install { req, _ in
            if req.httpMethod == "GET" { return .json(200, configured) }
            return .error(putCode == "version_conflict" ? 409 : 400, code: putCode, currentVersion: 4)
        }
        let vm = PreferencesViewModel(api: api(), deviceTimeZone: "Asia/Seoul")
        await vm.load()
        vm.addInterval(3)
        _ = await vm.save()
        XCTAssertEqual(MockURLProtocol.requests.filter { $0.request.httpMethod == "PUT" }.first?.request.value(forHTTPHeaderField: "If-Match"), "\"4\"")
        XCTAssertNil(vm.days[3], "409면 서버 값을 다시 읽는다")
        putCode = "time_zone_invalid"
        _ = await vm.save()
        XCTAssertTrue(vm.needsCityPicker)
    }
}

final class PartyDataTests: XCTestCase {
    func testInviteTokenParsing() {
        let token = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_abcde"
        XCTAssertEqual(InviteToken.parse(token), token)
        XCTAssertEqual(InviteToken.parse("  \(token)\n"), token)
        XCTAssertEqual(InviteToken.parse("https://plantogether.example/i/\(token)"), token)
        XCTAssertNil(InviteToken.parse("https://plantogether.example/x/\(token)"))
        XCTAssertNil(InviteToken.parse("short"))
        XCTAssertNil(InviteToken.parse("has space in the middle of token 12345"))
        XCTAssertNil(InviteToken.parse(String(repeating: "a", count: 129)))
        XCTAssertNil(InviteToken.parse("../../v1/me/aaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "path 문자는 받지 않는다")
    }

    func testPartiesFromSyncCache() {
        var s = SyncState(userID: "me")
        func put(_ type: String, _ id: String, _ payload: [String: JSONValue]) {
            s.entities[EntityKey(type, id)] = StoredEntity(version: 1, payload: .object(payload), scope: "m")
        }
        put("party", "p1", ["name": .string("주말"), "status": .string("active")])
        put("party", "p2", ["name": .string("해산됨"), "status": .string("disbanded")])
        put("party", "p3", ["name": .string("남의 것"), "status": .string("active")])
        put("party_membership", "m1", ["party_id": .string("p1"), "user_id": .string("me"), "display_name": .string("나"), "role": .string("member"), "status": .string("active")])
        put("party_membership", "m2", ["party_id": .string("p1"), "user_id": .string("u2"), "display_name": .string("방장"), "role": .string("owner"), "status": .string("active")])
        put("party_membership", "m3", ["party_id": .string("p1"), "user_id": .string("u3"), "display_name": .string("떠남"), "role": .string("member"), "status": .string("left")])
        put("party_membership", "m4", ["party_id": .string("p3"), "user_id": .string("u4"), "display_name": .string("남"), "role": .string("owner"), "status": .string("active")])
        put("party_visibility_setting", "v1", ["party_id": .string("p1"), "user_id": .string("me"), "visibility_level": .string("hidden")])
        let parties = s.parties()
        XCTAssertEqual(parties.map(\.id), ["p1"], "해산·본인 멤버십 없는 Party는 뺀다")
        XCTAssertEqual(parties[0].members.map(\.displayName), ["방장", "나"], "방장 먼저, 끝난 멤버십 제외")
        XCTAssertFalse(parties[0].isOwner)
        XCTAssertEqual(s.myVisibility(party: "p1"), "hidden")
        XCTAssertNil(s.myVisibility(party: "p3"))
    }

    /// 다른 기기에서 고른 상세 공개는 읽기 전용이다(lead 결정: 이 앱 버전에서 덮어쓰지 않는다).
    func testDetailsVisibilityIsReadOnly() {
        XCTAssertTrue(VisibilityView.isEditable("busyOnly"))
        XCTAssertTrue(VisibilityView.isEditable("hidden"))
        XCTAssertFalse(VisibilityView.isEditable("details"))
        XCTAssertFalse(VisibilityView.isEditable(nil), "불러오기 전에는 바꾸지 않는다")
        XCTAssertEqual(VisibilityView.title("details"), "상세 공개")
        XCTAssertEqual(VisibilityView.title("busyOnly"), "시간만 공개")
        XCTAssertEqual(VisibilityView.title("someFutureLevel"), "알 수 없음", "모르는 값을 시간만 공개로 보이지 않는다")
        XCTAssertFalse(VisibilityView.isEditable("someFutureLevel"))
        XCTAssertEqual(VisibilityView.summary("details"), "상세 공개 (이 앱 버전에서는 변경 불가)")
        XCTAssertEqual(VisibilityView.summary("someFutureLevel"), "알 수 없음 (이 앱 버전에서는 변경 불가)")
        XCTAssertEqual(VisibilityView.summary("hidden"), "숨김")
    }

    func testUserMessagesDoNotExposeOtherMembers() {
        let e = APIError.server(ServerError(status: 409, code: "party_full", message: "", requestID: nil, currentVersion: nil, retryAfter: nil))
        XCTAssertEqual(UserMessage.of(e), "정원이 찼습니다.")
        XCTAssertEqual(UserMessage.of(APIError.transport("x")), "연결할 수 없습니다. 네트워크를 확인하세요.")
    }
}

/// I3 리뷰 반영 회귀.
final class ScreenReviewFixTests: XCTestCase {
    private func api() -> APIClient {
        APIClient(baseURL: URL(string: "http://localhost:8080")!, session: MockURLProtocol.makeSession(),
                  tokenStore: InMemoryTokenStore(TestTokens.make("a-1")), sleep: { _ in })
    }
    private let tokenA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    private let tokenB = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

    /// Medium: 미리본 초대의 token으로만 수락한다. 미리본 뒤 입력을 바꾸면 미리보기가 사라지고 수락하지 않는다.
    @MainActor
    func testAcceptUsesPreviewedTokenOnly() async {
        MockURLProtocol.install { req, _ in
            if req.url!.path.hasSuffix("/preview") {
                let name = req.url!.path.contains(self.tokenA) ? "A Party" : "B Party"
                return .json(200, ["request_id": "r", "preview": ["party_name": name, "member_count": 1, "inviter_display_name": "x"]])
            }
            return .json(201, ["request_id": "r", "party": ["id": "p"], "membership": [:]])
        }
        let joined = Counter()
        let model = JoinInviteModel(api: PartyAPI(api: api()), afterJoin: { joined.increment() })
        model.input = tokenA
        await model.loadPreview()
        XCTAssertEqual(model.preview?.partyName, "A Party")
        model.input = tokenB
        XCTAssertNil(model.preview, "입력이 바뀌면 미리보기를 비운다")
        let refused = await model.accept()
        XCTAssertFalse(refused)
        XCTAssertFalse(MockURLProtocol.requests.contains { $0.request.url!.path.hasSuffix("/accept") })

        await model.loadPreview()
        XCTAssertEqual(model.preview?.partyName, "B Party")
        model.input = "  \(tokenB)\n"   // 같은 token(공백만 다름)은 미리보기를 유지한다
        XCTAssertNotNil(model.preview)
        let ok = await model.accept()
        XCTAssertTrue(ok)
        XCTAssertEqual(joined.value, 1)
        let accepted = MockURLProtocol.requests.filter { $0.request.url!.path.hasSuffix("/accept") }.map { $0.request.url!.path }
        XCTAssertEqual(accepted, ["/v1/invites/\(tokenB)/accept"])
    }

    @MainActor
    func testAlreadyMemberCountsAsJoined() async {
        MockURLProtocol.install { req, _ in
            req.url!.path.hasSuffix("/preview")
                ? .json(200, ["request_id": "r", "preview": ["party_name": "P", "member_count": 2, "inviter_display_name": "x"]])
                : .error(409, code: "already_member")
        }
        let model = JoinInviteModel(api: PartyAPI(api: api()), afterJoin: {})
        model.input = tokenA
        await model.loadPreview()
        let ok = await model.accept()
        XCTAssertTrue(ok)
        XCTAssertNil(model.error)
    }

    /// Medium: 검색 시간대는 저장된 활동 시간의 시간대 → 기기 시간대 → 없으면 도시 선택.
    func testSearchTimeZoneDefault() {
        let seoul = TimeZone(identifier: "Asia/Seoul")!, gmt = TimeZone(identifier: "GMT")!
        XCTAssertEqual(SearchForm.defaultTimeZone(preference: "America/New_York", device: seoul)?.identifier, "America/New_York")
        XCTAssertEqual(SearchForm.defaultTimeZone(preference: nil, device: seoul)?.identifier, "Asia/Seoul")
        XCTAssertEqual(SearchForm.defaultTimeZone(preference: "Asia/Seoul", device: gmt)?.identifier, "Asia/Seoul")
        XCTAssertNil(SearchForm.defaultTimeZone(preference: nil, device: gmt))
        XCTAssertNil(SearchForm.defaultTimeZone(preference: "UTC", device: gmt))
    }

    @MainActor
    func testGMTDeviceUsesSavedPreferenceTimeZone() async {
        MockURLProtocol.install { _, _ in
            .json(200, ["request_id": "r", "preferences": ["status": "configured", "time_zone": "Asia/Seoul", "version": 2,
                                                           "weekly_intervals": [["weekday": 1, "start_minute": 540, "end_minute": 1320]], "updated_at": "2026-10-29T03:00:00Z"]])
        }
        let vm = SearchViewModel(api: api(), partyID: "p", timeZone: TimeZone(identifier: "GMT")!)
        XCTAssertTrue(vm.needsTimeZonePick)
        await vm.prepare()
        XCTAssertFalse(vm.needsTimeZonePick)
        XCTAssertEqual(vm.form.timeZone.identifier, "Asia/Seoul")
        XCTAssertNil(vm.form.validate(now: Date()))
    }

    func testCalendarCTAByState() {
        XCTAssertEqual(CalendarCTA.of("ready"), .none)
        XCTAssertEqual(CalendarCTA.of("not_connected"), .connect)
        XCTAssertEqual(CalendarCTA.of("syncing"), .syncing)
        XCTAssertEqual(CalendarCTA.of("stale"), .refresh)
        XCTAssertEqual(CalendarCTA.of("error"), .refresh)
        XCTAssertEqual(CalendarCTA.of("denied"), .allowInSettings)
        XCTAssertEqual(CalendarCTA.of("revoked"), .allowInSettings)
        XCTAssertEqual(CalendarCTA.of("source_required"), .selectCalendars)
    }

    /// availability_changed_retry는 한 번 다시 찾고, 또 그러면 안내 문구다.
    @MainActor
    func testChangedRetryIsRetriedOnce() async {
        let calls = Counter()
        let ok: [String: Any] = ["request_id": "r", "slots": [], "calculation_version": "v", "generated_at": "2026-10-29T03:00:00Z", "valid_until": "2099-10-29T03:05:00Z"]
        MockURLProtocol.install { _, _ in
            calls.increment()
            return calls.value == 1 ? .error(409, code: "availability_changed_retry") : .json(200, ok)
        }
        let vm = SearchViewModel(api: api(), partyID: "p", timeZone: TimeZone(identifier: "Asia/Seoul")!)
        await vm.search()
        XCTAssertEqual(vm.state, .empty)
        XCTAssertEqual(calls.value, 2)

        MockURLProtocol.install { _, _ in .error(409, code: "availability_changed_retry") }
        await vm.search()
        XCTAssertEqual(vm.state, .failed("검색하는 동안 그룹 정보가 바뀌었습니다. 다시 찾아 주세요."))
        XCTAssertEqual(MockURLProtocol.requests.count, 2)
    }

    /// 첫 저장이 반영됐는데 응답을 잃고 내용을 고쳐 같은 키로 보내면 idempotency_mismatch다. 키를 비우고 다시 읽어 이후
    /// 저장은 If-Match로 간다.
    @MainActor
    func testIdempotencyMismatchReloadsAndDropsKey() async {
        let configured: [String: Any] = ["request_id": "r", "preferences": ["status": "configured", "time_zone": "Asia/Seoul", "version": 1,
                                                                            "weekly_intervals": [["weekday": 1, "start_minute": 540, "end_minute": 1320]], "updated_at": "2026-10-29T03:00:00Z"]]
        let unconfigured: [String: Any] = ["request_id": "r", "preferences": ["status": "unconfigured", "time_zone": NSNull(), "version": 0, "weekly_intervals": [], "updated_at": NSNull()]]
        let puts = Counter()
        MockURLProtocol.install { req, _ in
            if req.httpMethod == "GET" { return .json(200, puts.value == 0 ? unconfigured : configured) }
            puts.increment()
            switch puts.value {
            case 1: throw URLError(.networkConnectionLost)   // 서버는 반영했지만 응답을 잃었다
            case 2: return .error(409, code: "idempotency_mismatch")
            default: return .json(200, configured)
            }
        }
        let vm = PreferencesViewModel(api: api(), deviceTimeZone: "Asia/Seoul")
        await vm.load()
        _ = await vm.save()
        vm.days[2] = []   // 내용을 고친다(화요일 활동 안 함)
        let second = await vm.save()
        XCTAssertFalse(second)
        XCTAssertEqual(vm.version, 1, "서버 값을 다시 읽었다")
        _ = await vm.save()
        let sent = MockURLProtocol.requests.filter { $0.request.httpMethod == "PUT" }
        XCTAssertEqual(sent.count, 3)
        XCTAssertEqual(sent[0].request.value(forHTTPHeaderField: "Idempotency-Key"), sent[1].request.value(forHTTPHeaderField: "Idempotency-Key"))
        XCTAssertEqual(sent[2].request.value(forHTTPHeaderField: "If-Match"), "\"1\"")
    }
}
