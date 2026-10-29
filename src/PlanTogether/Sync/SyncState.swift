import Foundation

/// 로컬 서버 투영의 key.
struct EntityKey: Hashable, Codable, Sendable, CustomStringConvertible {
    let type: String
    let id: String

    init(_ type: String, _ id: String) {
        self.type = type
        self.id = id
    }

    var description: String { "\(type)/\(id)" }
}

/// 서버 entity 하나의 로컬 사본. payload는 서버의 전체 투영을 그대로 둔다(§7.1).
struct StoredEntity: Codable, Equatable, Sendable {
    var version: Int64
    var payload: JSONValue
    /// 받을 때의 `scope_membership_id`(Party 범위 entity만).
    var scope: String?
}

/// 로그인한 사용자 한 명의 sync 캐시. 서버 데이터의 읽기 캐시이며 기준 데이터가 아니다(§7.3).
struct SyncState: Codable, Equatable, Sendable {
    /// 이 캐시의 주인. 다른 사용자의 캐시를 읽지 않게 확인한다.
    var userID: String
    /// 서버가 준 opaque cursor. nil이면 bootstrap부터 한다.
    var cursor: String?
    var entities: [EntityKey: StoredEntity] = [:]
    /// version 있는 tombstone이 남긴 하한(§7.1). 하한 이하의 upsert는 버린다.
    var floors: [EntityKey: Int64] = [:]
    /// availability_preference 무효화 신호를 받았거나 bootstrap했다. `GET /v1/availability/preferences/me`로 다시 읽는다.
    var needsPreferenceRefresh = false

    init(userID: String, cursor: String? = nil) {
        self.userID = userID
        self.cursor = cursor
    }

    // Dictionary<EntityKey, _>의 기본 Codable은 [k, v, k, v] 배열이라 읽기 어렵다. 항목 배열로 쓴다.
    private struct EntityRecord: Codable {
        let type: String
        let id: String
        let entity: StoredEntity
    }

    private struct FloorRecord: Codable {
        let type: String
        let id: String
        let version: Int64
    }

    enum CodingKeys: String, CodingKey {
        case userID = "user_id"
        case cursor, entities, floors
        case needsPreferenceRefresh = "needs_preference_refresh"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        userID = try c.decode(String.self, forKey: .userID)
        cursor = try c.decodeIfPresent(String.self, forKey: .cursor)
        needsPreferenceRefresh = try c.decode(Bool.self, forKey: .needsPreferenceRefresh)
        for r in try c.decode([EntityRecord].self, forKey: .entities) { entities[EntityKey(r.type, r.id)] = r.entity }
        for r in try c.decode([FloorRecord].self, forKey: .floors) { floors[EntityKey(r.type, r.id)] = r.version }
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(userID, forKey: .userID)
        try c.encodeIfPresent(cursor, forKey: .cursor)
        try c.encode(needsPreferenceRefresh, forKey: .needsPreferenceRefresh)
        let sortedEntities = entities.sorted { $0.key.description < $1.key.description }
        try c.encode(sortedEntities.map { EntityRecord(type: $0.key.type, id: $0.key.id, entity: $0.value) }, forKey: .entities)
        let sortedFloors = floors.sorted { $0.key.description < $1.key.description }
        try c.encode(sortedFloors.map { FloorRecord(type: $0.key.type, id: $0.key.id, version: $0.value) }, forKey: .floors)
    }

    /// entity가 속한 Party ID. party는 자기 ID, 하위 entity는 payload의 party_id다.
    func partyID(of key: EntityKey, _ entity: StoredEntity) -> String? {
        key.type == SyncEntityType.party ? key.id : entity.payload["party_id"]?.stringValue
    }

    func entities(ofType type: String) -> [(id: String, entity: StoredEntity)] {
        entities.filter { $0.key.type == type }.map { ($0.key.id, $0.value) }.sorted { $0.id < $1.id }
    }
}

enum SyncEntityType {
    static let user = "user"
    static let device = "device"
    static let availabilityPreference = "availability_preference"
    static let party = "party"
    static let partyMembership = "party_membership"
    static let partyInvite = "party_invite"
    static let partyVisibilitySetting = "party_visibility_setting"
    static let partyScheduleProjection = "party_schedule_projection"

    /// scope_membership_id를 싣는 Party 범위 type(서버 party.ScopedEntityTypes).
    static let partyScoped: Set<String> = [party, partyMembership, partyInvite, partyVisibilitySetting, partyScheduleProjection]
}
