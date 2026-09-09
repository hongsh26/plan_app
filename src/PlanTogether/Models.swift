import Foundation

struct Member: Identifiable, Hashable {
    let id: UUID
    var name: String
}

enum VisibilityLevel: String, CaseIterable, Identifiable {
    case details = "상세 공개"
    case busyOnly = "시간만 공개"
    case hidden = "숨김"

    var id: Self { self }
}

struct BusyInterval: Identifiable, Hashable {
    let id: UUID
    let ownerID: UUID
    let start: Date
    let end: Date
    let title: String?
}

struct Party: Identifiable {
    let id: UUID
    var name: String
    var members: [Member]
    var visibilityByMember: [UUID: VisibilityLevel]
    var syncedMemberIDs: Set<UUID>
    var busyIntervals: [BusyInterval]
}

struct AvailabilitySlot: Identifiable, Hashable {
    let id = UUID()
    let start: Date
    let end: Date
    let availableMemberCount: Int
    let totalMemberCount: Int
}

enum ProposalResponse: String {
    case pending, accepted, declined
}

struct EventProposal: Identifiable {
    let id: UUID
    var title: String
    var start: Date
    var end: Date
    var responses: [UUID: ProposalResponse]

    var isConfirmed: Bool {
        !responses.isEmpty && responses.values.allSatisfy { $0 == .accepted }
    }
}
