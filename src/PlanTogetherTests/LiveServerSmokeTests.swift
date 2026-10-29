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

/// 실서버 캘린더 업로드(선택 실행). EventKit 대신 가짜 source를 넣어 권한 없이 create → page → complete → ready를 본다.
final class LiveCalendarSmokeTests: XCTestCase {
    func testCalendarUploadReachesReady() async throws {
        guard let raw = ProcessInfo.processInfo.environment["PLAN_LIVE_SERVER_URL"], let base = URL(string: raw) else {
            throw XCTSkip("PLAN_LIVE_SERVER_URL이 없다(선택 실행)")
        }
        let api = APIClient(baseURL: base, tokenStore: InMemoryTokenStore())
        let tokens = try await api.signIn(SignInRequest(identityToken: "dev:cal.\(Int(Date().timeIntervalSince1970))", authorizationCode: "dev", rawNonce: "dev", displayName: "캘린더"))
        let cal = CalendarInfo(id: "smoke-calendar", title: "스모크", sourceTitle: "local")
        let now = Date()
        let tz = TimeZone(identifier: "Asia/Seoul")!
        var events: [CalendarEventOccurrence] = (1...300).map { i in
            let start = Calendar.current.startOfDay(for: now).addingTimeInterval(TimeInterval(i) * 3 * 3_600)
            return CalendarEventOccurrence(eventIdentifier: "e\(i)", occurrenceDate: start, start: start, end: start.addingTimeInterval(3_600),
                                           isAllDay: false, timeZoneID: i % 2 == 0 ? "Asia/Seoul" : nil, availability: i % 7 == 0 ? .tentative : .busy, isCanceled: false)
        }
        let dayStart = SnapshotWindow.make(now: now, timeZone: tz).start
        events.append(CalendarEventOccurrence(eventIdentifier: "allday", occurrenceDate: dayStart, start: dayStart.addingTimeInterval(86_400),
                                              end: dayStart.addingTimeInterval(2 * 86_400 - 1), isAllDay: true, timeZoneID: nil, availability: .busy, isCanceled: false))
        let source = FakeEventSource(auth: .notDetermined, cals: [cal], events: [cal.id: events])
        let sync = CalendarSync(api: api, source: source, store: InMemoryCalendarStateStore(), userID: tokens.userID, deviceID: tokens.deviceID,
                                timeZone: { tz })

        var status = try await sync.connect()
        XCTAssertEqual(status.connection?.status, "selecting")
        status = try await sync.setSelection([cal.id])
        XCTAssertEqual(status.connection?.status, "ready")
        XCTAssertEqual(status.connection?.isSyncDevice, true)
        XCTAssertGreaterThan(status.uploadedFacts ?? 0, 250, "두 page 이상")
        XCTAssertNotNil(status.connection?.lastCompletedSyncAt)

        status = try await sync.refresh(.manual)
        XCTAssertEqual(status.connection?.status, "ready", "두 번째 revision")

        status = try await sync.setSelection([])
        XCTAssertEqual(status.connection?.status, "needs_source_reselection")
        await api.logout()
    }
}

/// 실제 EventKit(선택 실행). 시뮬레이터에 권한을 준 뒤 실행한다:
///   xcrun simctl privacy <udid> grant calendar com.example.PlanTogether
///   TEST_RUNNER_PLAN_EVENTKIT_TEST=1 xcodebuild ... -only-testing:PlanTogetherTests/LiveEventKitTests
final class LiveEventKitTests: XCTestCase {
    func testSampleEventsBecomeExpectedFacts() throws {
        guard ProcessInfo.processInfo.environment["PLAN_EVENTKIT_TEST"] != nil else { throw XCTSkip("PLAN_EVENTKIT_TEST가 없다(선택 실행)") }
        #if DEBUG
        let source = EventKitEventSource()
        XCTAssertEqual(source.authorization(), .fullAccess)
        try source.createSampleEvents()
        try source.createSampleEvents()  // 다시 눌러도 일정이 늘지 않는다
        let cal = try XCTUnwrap(source.calendars().first { $0.title == EventKitEventSource.sampleCalendarTitle })
        let tz = TimeZone.current
        let window = SnapshotWindow.make(now: Date(), timeZone: tz)
        let occ = source.occurrences(in: window, calendarIDs: [cal.id])
        XCTAssertEqual(occ.count, 8, "회의 1, 주간 4, 하루 종일 1, 미정 1, free 1")
        let facts = FactBuilder.build(occ, window: window, userTimeZone: tz, keyer: CalendarKeyer(secret: CalendarKeyer.newSecret()))
        if occ.allSatisfy({ $0.availability == .notSupported }) {
            // 로컬("나의 iPhone") 캘린더는 availability를 지원하지 않는다. EventKit이 모두 notSupported로 주므로
            // 안전하게 모두 Busy다(free·tentative를 구별할 수 없다).
            XCTAssertEqual(facts.count, 8)
            XCTAssertTrue(facts.allSatisfy { $0.availability == "busy" })
        } else {
            XCTAssertEqual(facts.count, 7, "free는 제외")
            XCTAssertEqual(facts.filter { $0.availability == "tentative" }.count, 1)
        }
        let allDay = try XCTUnwrap(facts.first { $0.allDay })
        let start = try XCTUnwrap(RFC3339.parse(allDay.startAt)), end = try XCTUnwrap(RFC3339.parse(allDay.endAt))
        XCTAssertEqual(SnapshotWindow.calendar(tz).dateComponents([.day], from: start, to: end).day, 1, "하루 전체")
        XCTAssertEqual(SnapshotWindow.calendar(tz).dateComponents([.hour, .minute], from: start), DateComponents(hour: 0, minute: 0))
        XCTAssertTrue(source.occurrences(in: window, calendarIDs: []).isEmpty, "선택이 비면 읽지 않는다")
        #endif
    }

    /// 실제 EventKit + 실서버(두 변수 모두 있을 때만): 시연 캘린더를 골라 올리면 서버 연결이 ready다.
    func testRealEventKitUploadToLiveServer() async throws {
        guard ProcessInfo.processInfo.environment["PLAN_EVENTKIT_TEST"] != nil,
              let raw = ProcessInfo.processInfo.environment["PLAN_LIVE_SERVER_URL"], let base = URL(string: raw) else {
            throw XCTSkip("PLAN_EVENTKIT_TEST와 PLAN_LIVE_SERVER_URL이 모두 필요하다(선택 실행)")
        }
        #if DEBUG
        let source = EventKitEventSource()
        try source.createSampleEvents()
        let cal = try XCTUnwrap(source.calendars().first { $0.title == EventKitEventSource.sampleCalendarTitle })
        let api = APIClient(baseURL: base, tokenStore: InMemoryTokenStore())
        let tokens = try await api.signIn(SignInRequest(identityToken: "dev:ek.\(Int(Date().timeIntervalSince1970))", authorizationCode: "dev", rawNonce: "dev", displayName: "EK"))
        let sync = CalendarSync(api: api, source: source, store: InMemoryCalendarStateStore(), userID: tokens.userID, deviceID: tokens.deviceID)
        _ = try await sync.connect()
        let status = try await sync.setSelection([cal.id])
        XCTAssertEqual(status.connection?.status, "ready")
        XCTAssertEqual(status.uploadedFacts, 8)
        await api.logout()
        #endif
    }
}

/// 실서버 두 사용자 시연 흐름(선택 실행): 로그인 → Party 생성 → 초대 → 미리보기·수락 → 캘린더 업로드(가짜 source) →
/// 활동 시간 → 검색. 화면이 쓰는 모델(PartyAPI, CalendarSync, PreferencesViewModel, SearchViewModel)을 그대로 지난다.
final class LiveTwoUserDemoTests: XCTestCase {
    @MainActor
    func testTwoUsersFindCommonSlots() async throws {
        guard let raw = ProcessInfo.processInfo.environment["PLAN_LIVE_SERVER_URL"], let base = URL(string: raw) else {
            throw XCTSkip("PLAN_LIVE_SERVER_URL이 없다(선택 실행)")
        }
        let seoul = TimeZone(identifier: "Asia/Seoul")!
        let stamp = Int(Date().timeIntervalSince1970)
        func login(_ name: String) async throws -> (APIClient, AuthTokens) {
            let api = APIClient(baseURL: base, tokenStore: InMemoryTokenStore())
            let t = try await api.signIn(SignInRequest(identityToken: "dev:\(name).\(stamp)", authorizationCode: "dev", rawNonce: "dev", displayName: name))
            return (api, t)
        }
        let (aAPI, aTok) = try await login("alice")
        let (bAPI, bTok) = try await login("bob")

        // Party·초대·수락
        let partyID = try await PartyAPI(api: aAPI).createParty(name: "시연 Party \(stamp)")
        let invite = try await PartyAPI(api: aAPI).createInvite(partyID: partyID)
        XCTAssertEqual(InviteToken.parse(invite.token), invite.token)
        let preview = try await PartyAPI(api: bAPI).preview(token: invite.token)
        XCTAssertEqual(preview.memberCount, 1)
        XCTAssertEqual(preview.inviterDisplayName, "alice")
        let joined = try await PartyAPI(api: bAPI).accept(token: invite.token)
        XCTAssertEqual(joined, partyID)

        // 양쪽 캐시에 Party와 멤버 2명이 보인다.
        for (api, tok) in [(aAPI, aTok), (bAPI, bTok)] {
            let engine = SyncEngine(api: api, store: InMemorySyncStore(), userID: tok.userID)
            var parties: [PartySummary] = []
            for _ in 0..<20 {
                parties = try await engine.sync().parties()
                if parties.first?.members.count == 2 { break }
                try await Task.sleep(nanoseconds: 100_000_000)
            }
            XCTAssertEqual(parties.map(\.id), [partyID])
            XCTAssertEqual(parties.first?.members.count, 2)
            let vis = try await engine.sync().myVisibility(party: partyID)
            XCTAssertEqual(vis, "busyOnly")
        }

        // 아직 활동 시간 미설정: 준비 대기(본인 미설정 CTA)
        let early = SearchViewModel(api: aAPI, partyID: partyID, timeZone: seoul)
        await early.search()
        guard case .pending(_, true, _) = early.state else { return XCTFail("준비 대기여야 한다: \(early.state)") }

        // 캘린더: 내일 서울 10:00~11:00(A), 14:00~15:00(B) busy
        let cal = SnapshotWindow.calendar(seoul)
        let tomorrow = cal.date(byAdding: .day, value: 1, to: cal.startOfDay(for: Date()))!
        func busy(_ id: String, _ hour: Int) -> CalendarEventOccurrence {
            let s = cal.date(byAdding: .hour, value: hour, to: tomorrow)!
            return CalendarEventOccurrence(eventIdentifier: id, occurrenceDate: s, start: s, end: s.addingTimeInterval(3_600),
                                           isAllDay: false, timeZoneID: "Asia/Seoul", availability: .busy, isCanceled: false)
        }
        let info = CalendarInfo(id: "demo", title: "시연", sourceTitle: "local")
        for (api, tok, event) in [(aAPI, aTok, busy("a", 10)), (bAPI, bTok, busy("b", 14))] {
            let sync = CalendarSync(api: api, source: FakeEventSource(auth: .notDetermined, cals: [info], events: [info.id: [event]]),
                                    store: InMemoryCalendarStateStore(), userID: tok.userID, deviceID: tok.deviceID, timeZone: { seoul })
            _ = try await sync.connect()
            let s = try await sync.setSelection([info.id])
            XCTAssertEqual(s.connection?.status, "ready")
        }

        // 활동 시간: 제안값(매일 09:00~22:00)을 그대로 저장
        for api in [aAPI, bAPI] {
            let vm = PreferencesViewModel(api: api, deviceTimeZone: "Asia/Seoul")
            await vm.load()
            XCTAssertTrue(vm.isSuggestion)
            let saved = await vm.save()
            XCTAssertTrue(saved, vm.message ?? "")
        }

        // 검색: 내일 하루, 60분
        let vm = SearchViewModel(api: aAPI, partyID: partyID, timeZone: seoul)
        vm.form.startDate = tomorrow
        vm.form.endDate = tomorrow
        await vm.search()
        guard case .results(let sections, let until) = vm.state else { return XCTFail("결과여야 한다: \(vm.state)") }
        XCTAssertGreaterThan(until, Date())
        let slots = sections.flatMap(\.slots)
        XCTAssertFalse(slots.isEmpty)
        XCTAssertLessThanOrEqual(slots.count, 3, "하루 최대 3개")
        let aBusy = cal.date(byAdding: .hour, value: 10, to: tomorrow)!, bBusy = cal.date(byAdding: .hour, value: 14, to: tomorrow)!
        for slot in slots {
            XCTAssertFalse(slot.start < aBusy.addingTimeInterval(3_600) && slot.end > aBusy, "A의 일정과 겹친다: \(slot.label)")
            XCTAssertFalse(slot.start < bBusy.addingTimeInterval(3_600) && slot.end > bBusy, "B의 일정과 겹친다: \(slot.label)")
            XCTAssertGreaterThanOrEqual(slot.start, cal.date(byAdding: .hour, value: 9, to: tomorrow)!, "활동 시간 밖: \(slot.label)")
            XCTAssertTrue(slot.label.hasSuffix("GMT+9"), slot.label)
        }
        await aAPI.logout()
        await bAPI.logout()
    }
}
