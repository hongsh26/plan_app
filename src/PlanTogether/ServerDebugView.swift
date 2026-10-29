import SwiftUI

/// 서버 연동 확인용 최소 화면(I1). 개발용 로그인, sync, 캐시 요약, 로그아웃. 실제 화면은 I3에서 만든다.
struct ServerDebugView: View {
    @EnvironmentObject private var session: SessionStore
    @State private var name = "alice"
    @State private var displayName = "앨리스"

    var body: some View {
        Form {
            Section("서버") {
                LabeledContent("주소", value: AppConfig.baseURL.absoluteString)
            }
            switch session.state {
            case .signedOut:
                Section("개발용 로그인") {
                    TextField("이름 (a-z, 0-9, . _ -)", text: $name)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    TextField("표시 이름", text: $displayName)
                    Button("로그인") { Task { await session.devLogin(name: name, displayName: displayName) } }
                        .disabled(name.isEmpty || session.isBusy)
                }
            case .signedIn(let user):
                Section("세션") {
                    LabeledContent("user_id", value: user.userID)
                    LabeledContent("device_id", value: user.deviceID)
                    Button("지금 sync") { Task { await session.syncNow() } }
                        .disabled(session.isBusy)
                    Button("로그아웃", role: .destructive) { Task { await session.logout() } }
                }
                if let state = session.syncState {
                    Section("캐시") {
                        ForEach(entityCounts(state), id: \.0) { type, count in
                            LabeledContent(type, value: "\(count)")
                        }
                        LabeledContent("하한", value: "\(state.floors.count)")
                    }
                    Section("Party") {
                        let parties = state.entities(ofType: SyncEntityType.party)
                        if parties.isEmpty { Text("없음").foregroundStyle(.secondary) }
                        ForEach(parties, id: \.id) { party in
                            Text(party.entity.payload["name"]?.stringValue ?? party.id)
                        }
                    }
                }
            }
            if let error = session.lastError {
                Section("오류") { Text(error).font(.footnote).foregroundStyle(.red) }
            }
        }
        .navigationTitle("서버 연동")
    }

    private func entityCounts(_ state: SyncState) -> [(String, Int)] {
        Dictionary(grouping: state.entities.keys, by: \.type).map { ($0.key, $0.value.count) }.sorted { $0.0 < $1.0 }
    }
}
