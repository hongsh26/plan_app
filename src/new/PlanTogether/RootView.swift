import SwiftUI

struct RootView: View {
    var body: some View {
        TabView {
            NavigationStack { PartyListView() }
                .tabItem { Label("Party", systemImage: "person.3") }
            NavigationStack { CalendarConnectionView() }
                .tabItem { Label("캘린더", systemImage: "calendar") }
            NavigationStack { SettingsView() }
                .tabItem { Label("설정", systemImage: "gearshape") }
        }
    }
}

private struct PartyListView: View {
    @EnvironmentObject private var store: AppStore

    var body: some View {
        List(store.parties) { party in
            NavigationLink(value: party.id) {
                VStack(alignment: .leading, spacing: 4) {
                    Text(party.name).font(.headline)
                    let visibility = party.visibilityByMember[store.currentUser.id] ?? .busyOnly
                    Text("멤버 \(party.members.count)명 · \(visibility.rawValue)")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        }
        .navigationTitle("내 Party")
        .navigationDestination(for: UUID.self) { partyID in PartyDetailView(partyID: partyID) }
    }
}

private struct PartyDetailView: View {
    @EnvironmentObject private var store: AppStore
    let partyID: UUID

    var party: Party? { store.parties.first { $0.id == partyID } }

    var body: some View {
        Group {
            if let party {
                List {
                    Section("멤버") {
                        ForEach(party.members) { Text($0.name) }
                    }
                    Section("모두 가능한 시간") {
                        let slots = store.availableSlots(for: party)
                        if slots.isEmpty { Text("조건에 맞는 시간이 없습니다.") }
                        ForEach(slots) { slot in
                            Button {
                                store.propose(partyID: partyID, slot: slot)
                            } label: {
                                VStack(alignment: .leading) {
                                    Text(slot.start.formatted(date: .omitted, time: .shortened) + " – " + slot.end.formatted(date: .omitted, time: .shortened))
                                    Text("\(slot.availableMemberCount)/\(slot.totalMemberCount) 가능 · 탭해서 제안")
                                        .font(.caption).foregroundStyle(.secondary)
                                }
                            }
                        }
                    }
                    Section("제안") {
                        let proposals = store.proposals[partyID, default: []]
                        if proposals.isEmpty { Text("아직 제안이 없습니다.") }
                        ForEach(proposals) { proposal in
                            Text("\(proposal.title) · \(proposal.isConfirmed ? "확정" : "응답 대기")")
                        }
                    }
                }
                .navigationTitle(party.name)
            } else {
                ContentUnavailableView("Party를 찾을 수 없습니다", systemImage: "person.3")
            }
        }
    }
}

private struct CalendarConnectionView: View {
    @EnvironmentObject private var store: AppStore

    var body: some View {
        VStack(spacing: 20) {
            Image(systemName: "calendar.badge.plus").font(.system(size: 52)).foregroundStyle(.blue)
            Text("Apple 캘린더 연결").font(.title2.bold())
            Text("일정은 가능한 시간을 계산할 때 사용됩니다.").foregroundStyle(.secondary)
            Button("캘린더 접근 허용") { Task { await store.connectCalendar() } }
                .buttonStyle(.borderedProminent)
            if let message = store.calendarMessage { Text(message).font(.footnote) }
        }
        .padding()
        .navigationTitle("캘린더")
    }
}

private struct SettingsView: View {
    @EnvironmentObject private var store: AppStore
    var body: some View {
        Form {
            Section("프로필") { LabeledContent("이름", value: store.currentUser.name) }
            Section("요금제") { LabeledContent("현재", value: "Free") }
        }
        .navigationTitle("설정")
    }
}
