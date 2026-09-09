import Combine
import Foundation

@MainActor
final class AppStore: ObservableObject {
    @Published var parties: [Party]
    @Published var proposals: [UUID: [EventProposal]] = [:]
    @Published var calendarMessage: String?

    private let calendarService: CalendarServing
    let currentUser: Member

    init(currentUser: Member, parties: [Party], calendarService: CalendarServing) {
        self.currentUser = currentUser
        self.parties = parties
        self.calendarService = calendarService
    }

    func availableSlots(for party: Party) -> [AvailabilitySlot] {
        let now = Date()
        let dayStart = Calendar.current.date(bySettingHour: 9, minute: 0, second: 0, of: now) ?? now
        let start = max(now, dayStart)
        let end = Calendar.current.date(bySettingHour: 22, minute: 0, second: 0, of: now) ?? now.addingTimeInterval(46_800)
        guard start.addingTimeInterval(3_600) <= end else { return [] }
        return AvailabilityCalculator.commonSlots(
            members: party.members,
            syncedMemberIDs: party.syncedMemberIDs,
            busyIntervals: party.busyIntervals,
            searchRange: DateInterval(start: start, end: end),
            duration: 3_600
        )
    }

    func propose(partyID: UUID, slot: AvailabilitySlot) {
        guard let party = parties.first(where: { $0.id == partyID }) else { return }
        let proposal = EventProposal(
            id: UUID(), title: "새 약속", start: slot.start, end: slot.end,
            responses: Dictionary(uniqueKeysWithValues: party.members.map { ($0.id, $0.id == currentUser.id ? .accepted : .pending) })
        )
        proposals[partyID, default: []].append(proposal)
    }

    func connectCalendar() async {
        do {
            guard try await calendarService.requestAccess() else {
                invalidateCurrentUserCalendar()
                calendarMessage = "캘린더 접근이 허용되지 않았습니다."
                return
            }
            let now = Date()
            let dayStart = Calendar.current.startOfDay(for: now)
            let range = DateInterval(start: dayStart, end: dayStart.addingTimeInterval(8 * 86_400))
            let intervals = try await calendarService.busyIntervals(in: range, ownerID: currentUser.id)
            for index in parties.indices {
                parties[index].busyIntervals.removeAll { $0.ownerID == currentUser.id }
                let visibility = parties[index].visibilityByMember[currentUser.id] ?? .busyOnly
                if visibility != .hidden {
                    parties[index].busyIntervals.append(contentsOf: intervals.map {
                        BusyInterval(
                            id: $0.id, ownerID: $0.ownerID, start: $0.start, end: $0.end,
                            title: visibility == .details ? $0.title : nil
                        )
                    })
                }
                parties[index].syncedMemberIDs.insert(currentUser.id)
            }
            calendarMessage = "앞으로 7일의 일정을 불러왔습니다."
        } catch {
            invalidateCurrentUserCalendar()
            calendarMessage = "캘린더를 연결하지 못했습니다: \(error.localizedDescription)"
        }
    }

    private func invalidateCurrentUserCalendar() {
        for index in parties.indices {
            parties[index].busyIntervals.removeAll { $0.ownerID == currentUser.id }
            parties[index].syncedMemberIDs.remove(currentUser.id)
        }
    }

    static var demo: AppStore {
        let me = Member(id: UUID(), name: "나")
        let friend = Member(id: UUID(), name: "민지")
        let now = Date()
        let calendar = Calendar.current
        let busyStart = calendar.date(bySettingHour: 13, minute: 0, second: 0, of: now) ?? now
        let busyEnd = calendar.date(byAdding: .hour, value: 2, to: busyStart) ?? busyStart.addingTimeInterval(7_200)
        let party = Party(
            id: UUID(), name: "대학 친구", members: [me, friend],
            visibilityByMember: [me.id: .busyOnly, friend.id: .busyOnly],
            syncedMemberIDs: [me.id, friend.id],
            busyIntervals: [BusyInterval(id: UUID(), ownerID: friend.id, start: busyStart, end: busyEnd, title: nil)]
        )
        return AppStore(currentUser: me, parties: [party], calendarService: EventKitCalendarService())
    }
}
