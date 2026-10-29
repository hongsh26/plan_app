import SwiftUI

/// 시연 화면(I3, 디자인 전 표준 SwiftUI). 로그인 전에는 로그인, 뒤에는 Party·캘린더·활동 시간·설정 탭.
struct RootView: View {
    @EnvironmentObject private var session: SessionStore

    var body: some View {
        switch session.state {
        case .signedOut:
            LoginView()
        case .signedIn:
            TabView {
                NavigationStack { PartyListView() }
                    .tabItem { Label("Party", systemImage: "person.3") }
                NavigationStack { Form { CalendarSection() }.navigationTitle("캘린더") }
                    .tabItem { Label("캘린더", systemImage: "calendar") }
                NavigationStack { PreferencesView() }
                    .tabItem { Label("활동 시간", systemImage: "clock") }
                NavigationStack { SettingsView() }
                    .tabItem { Label("설정", systemImage: "gearshape") }
            }
        }
    }
}

// MARK: 로그인

private struct LoginView: View {
    @EnvironmentObject private var session: SessionStore
    @State private var name = ""
    @State private var displayName = ""

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Button {} label: { Label("Apple로 로그인 (준비 중)", systemImage: "apple.logo") }
                        .disabled(true)
                } footer: { Text("Sign in with Apple은 다음 단계에서 붙입니다.") }
                Section("개발용 로그인") {
                    TextField("이름 (영문 소문자·숫자·. _ -)", text: $name)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    TextField("표시 이름", text: $displayName)
                    Button("로그인") { Task { await session.devLogin(name: name, displayName: displayName.isEmpty ? name : displayName) } }
                        .disabled(name.isEmpty || session.isBusy)
                }
                Section { LabeledContent("서버", value: AppConfig.baseURL.absoluteString) }
                if let error = session.lastError {
                    Section { Text(error).font(.footnote).foregroundStyle(.red) }
                }
            }
            .navigationTitle("PlanTogether")
        }
    }
}

// MARK: 설정

private struct SettingsView: View {
    @EnvironmentObject private var session: SessionStore

    var body: some View {
        Form {
            if case .signedIn(let user) = session.state {
                Section("세션") {
                    LabeledContent("user_id", value: user.userID).font(.caption)
                    LabeledContent("device_id", value: user.deviceID).font(.caption)
                }
            }
            Section("동기화") {
                Button("지금 sync") { Task { await session.syncNow() } }.disabled(session.isBusy)
                if let s = session.syncState {
                    LabeledContent("캐시 entity", value: "\(s.entities.count)")
                }
            }
            Section { LabeledContent("서버", value: AppConfig.baseURL.absoluteString) }
            Section {
                Button("로그아웃", role: .destructive) { Task { await session.logout() } }
            }
            if let error = session.lastError {
                Section("오류") { Text(error).font(.footnote).foregroundStyle(.red) }
            }
        }
        .navigationTitle("설정")
    }
}
