import Foundation

enum SyncError: Error, Equatable {
    /// 모르는 bootstrap schema_version. 앱 업데이트가 필요하다(§7.2).
    case unsupportedSchema(Int)
}

/// 서버 변경을 로컬 캐시에 적용한다(account_backend_design.md §7.1·§7.2, 2026-10-28 개정).
/// 기준 구현은 server/internal/syncfeed/party_sync_test.go의 `client.apply`와 `fetchBootstrapFloors`다.
///
/// - upsert는 version이 로컬 version과 하한보다 클 때만 적용한다. 부모 Party보다 먼저 온 하위 entity도 보관한다.
/// - version 있는 tombstone은 지우고 하한을 기록한다. version 없는 tombstone은 지우기만 한다.
/// - 규칙 (a): `scope_membership_id`가 하한이 기록된(끝난) 멤버십인 upsert는 버린다.
/// - 규칙 (b): version 없는 tombstone은 그 entity의 Party에 scope가 아닌 본인 활성 멤버십이 로컬에 있으면 무시한다.
/// - 기기 폐기는 `revoked: true` upsert로 온다. 목록에서 빼고 그 version을 하한으로 둔다.
/// - 본인 멤버십이 끝나면(version 있는 본인 멤버십 tombstone) 그 Party의 캐시를 모두 지운다(V3 캐시 삭제). 단 같은
///   Party에 다른 본인 활성 멤버십이 있으면(재가입 부트스트랩이 먼저 온 경우) 지우지 않는다.
/// - availability_preference는 저장하지 않고 다시 읽으라는 표시만 남긴다(payload에 규칙이 없다).
struct SyncApplier {
    /// 계약 테스트의 대조군 스위치. 앱에서는 기본값(모두 켬)만 쓴다.
    struct Options: Equatable {
        /// 규칙 (a)(b).
        var rules = true
        /// version 하한(bootstrap 하한 항목, version 있는 tombstone, 기기 폐기).
        var floors = true
        /// 부모 Party가 없는 하위 entity upsert를 보관한다.
        var keepOrphans = true
    }

    static let supportedSchemaVersion = 1

    private(set) var state: SyncState
    var options: Options

    init(state: SyncState, options: Options = Options()) {
        self.state = state
        self.options = options
    }

    private var selfUserID: String { state.userID }

    // MARK: bootstrap

    /// 기존 서버 투영과 하한을 버리고 snapshot으로 교체한다. 하한 항목(version 있는 tombstone, 같은 entity에 여러 개면
    /// 최대값, scope는 null일 수 있다)을 먼저 기록하고 하한보다 큰 upsert만 넣는다. cursor는 watermark가 된다.
    mutating func applyBootstrap(_ response: BootstrapResponse) throws {
        guard response.schemaVersion == Self.supportedSchemaVersion else {
            throw SyncError.unsupportedSchema(response.schemaVersion)
        }
        var fresh = SyncState(userID: state.userID, cursor: response.cursorWatermark)
        if options.floors {
            for c in response.snapshot where c.operation == "tombstone" {
                guard let v = c.version else { continue }
                let key = EntityKey(c.entityType, c.entityID)
                fresh.floors[key] = max(fresh.floors[key] ?? v, v)
            }
        }
        for c in response.snapshot where c.operation == "upsert" {
            guard let v = c.version, let payload = c.payload else { continue }
            let key = EntityKey(c.entityType, c.entityID)
            if let f = fresh.floors[key], v <= f { continue }
            fresh.entities[key] = StoredEntity(version: v, payload: payload, scope: c.scopeMembershipID)
        }
        // availability_preference는 snapshot에 없다. watermark 아래의 최신 신호는 다시 오지 않으므로 다시 읽는다.
        fresh.needsPreferenceRefresh = true
        state = fresh
    }

    // MARK: 증분

    mutating func apply(page: SyncPage) {
        for change in page.changes { apply(change) }
        state.cursor = page.cursor
    }

    mutating func apply(_ ch: SyncChange) {
        if ch.entityType == SyncEntityType.availabilityPreference {
            state.needsPreferenceRefresh = true
            return
        }
        let key = EntityKey(ch.entityType, ch.entityID)
        switch ch.operation {
        case "upsert":
            applyUpsert(ch, key)
        case "tombstone":
            applyTombstone(ch, key)
        default:
            return  // 모르는 operation은 무시한다(새 서버 기능).
        }
    }

    private mutating func applyUpsert(_ ch: SyncChange, _ key: EntityKey) {
        guard let v = ch.version, let payload = ch.payload else { return }
        if options.rules, let scope = ch.scopeMembershipID,
           state.floors[EntityKey(SyncEntityType.partyMembership, scope)] != nil {
            return  // 규칙 (a): 끝난 멤버십으로 발행된 upsert다.
        }
        if let f = state.floors[key], v <= f { return }
        if let cur = state.entities[key], v <= cur.version { return }
        if !options.keepOrphans, let pid = payload["party_id"]?.stringValue,
           state.entities[EntityKey(SyncEntityType.party, pid)] == nil {
            return
        }
        if key.type == SyncEntityType.device, payload["revoked"]?.boolValue == true {
            state.entities[key] = nil
            recordFloor(key, v)
            return
        }
        state.entities[key] = StoredEntity(version: v, payload: payload, scope: ch.scopeMembershipID)
    }

    private mutating func applyTombstone(_ ch: SyncChange, _ key: EntityKey) {
        if let v = ch.version {
            if key.type == SyncEntityType.partyMembership { wipePartyIfOwnMembershipEnded(ch, key) }
            recordFloor(key, v)
            state.entities[key] = nil
            return
        }
        if options.rules, let cur = state.entities[key], let scope = ch.scopeMembershipID,
           let pid = state.partyID(of: key, cur), hasOtherOwnActiveMembership(party: pid, excluding: scope) {
            return  // 규칙 (b): 다른(새) 본인 멤버십으로 다시 받은 사본이다.
        }
        state.entities[key] = nil
    }

    private mutating func recordFloor(_ key: EntityKey, _ v: Int64) {
        guard options.floors else { return }
        state.floors[key] = max(state.floors[key] ?? v, v)
    }

    /// 본인 멤버십 tombstone은 scope가 그 멤버십 자신이다(서버 emitScoped는 끝난 수신자에게 끝난 멤버십을 싣는다).
    /// 로컬 사본이 있으면 user_id로도 확인한다.
    private mutating func wipePartyIfOwnMembershipEnded(_ ch: SyncChange, _ key: EntityKey) {
        let local = state.entities[key]
        let own = ch.scopeMembershipID == key.id || local?.payload["user_id"]?.stringValue == selfUserID
        guard own else { return }
        let partyID = local?.payload["party_id"]?.stringValue
            ?? state.entities.first { $0.value.scope == key.id }.flatMap { state.partyID(of: $0.key, $0.value) }
        guard let partyID, !hasOtherOwnActiveMembership(party: partyID, excluding: key.id) else { return }
        state.entities = state.entities.filter { state.partyID(of: $0.key, $0.value) != partyID }
    }

    private func hasOtherOwnActiveMembership(party partyID: String, excluding scope: String) -> Bool {
        state.entities.contains { key, e in
            key.type == SyncEntityType.partyMembership && key.id != scope
                && e.payload["party_id"]?.stringValue == partyID
                && e.payload["user_id"]?.stringValue == selfUserID
                && e.payload["status"]?.stringValue == "active"
        }
    }
}
