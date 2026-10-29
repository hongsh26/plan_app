import SwiftUI

// Party 목록·생성·상세·초대·수락·공개 수준(설계 4·5). 목록과 상세는 sync 캐시에서 읽는다.

struct PartyListView: View {
    @EnvironmentObject private var session: SessionStore
    @State private var showCreate = false
    @State private var showJoin = false

    var body: some View {
        let parties = session.syncState?.parties() ?? []
        List {
            if parties.isEmpty {
                ContentUnavailableView {
                    Label("아직 Party가 없습니다", systemImage: "person.3")
                } description: {
                    Text("새 Party를 만들거나 받은 초대 코드로 참여하세요.")
                } actions: {
                    Button("새 Party") { showCreate = true }.buttonStyle(.borderedProminent)
                }
            }
            ForEach(parties) { p in
                NavigationLink(value: p.id) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(p.name).font(.headline)
                        Text("멤버 \(p.members.count)명\(p.isOwner ? " · 방장" : "")").font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
        }
        .navigationTitle("내 Party")
        .navigationDestination(for: String.self) { PartyDetailView(partyID: $0) }
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Menu {
                    Button("새 Party") { showCreate = true }
                    Button("초대 코드로 참여") { showJoin = true }
                } label: { Image(systemName: "plus") }
            }
        }
        .refreshable { await session.syncNow() }
        .sheet(isPresented: $showCreate) { CreatePartySheet() }
        .sheet(isPresented: $showJoin) { JoinPartySheet() }
    }
}

private struct CreatePartySheet: View {
    @EnvironmentObject private var session: SessionStore
    @Environment(\.dismiss) private var dismiss
    @State private var name = ""
    @State private var error: String?
    @State private var busy = false

    var body: some View {
        NavigationStack {
            Form {
                TextField("Party 이름", text: $name)
                if let error { Text(error).font(.footnote).foregroundStyle(.red) }
            }
            .navigationTitle("새 Party")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("취소") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("만들기") {
                        Task {
                            busy = true
                            defer { busy = false }
                            do {
                                _ = try await session.partyAPI.createParty(name: name)
                                await session.syncNow()
                                dismiss()
                            } catch { self.error = UserMessage.of(error) }
                        }
                    }
                    .disabled(name.trimmingCharacters(in: .whitespaces).isEmpty || busy)
                }
            }
        }
    }
}

/// 초대 수락(설계 4 §3.2). Universal Link(AASA) 전이라 코드나 링크를 붙여 넣는다. 참여하면 시트를 닫고 목록으로
/// 돌아간다(sync 뒤 목록에 새 Party가 보인다). 이미 멤버여도 같다.
struct JoinPartySheet: View {
    @EnvironmentObject private var session: SessionStore
    @Environment(\.dismiss) private var dismiss
    @StateObject private var box = JoinInviteBox()

    var body: some View {
        NavigationStack {
            Group {
                if let model = box.model { JoinForm(model: model, onDone: dismiss.callAsFunction) } else { ProgressView() }
            }
            .navigationTitle("초대로 참여")
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button("닫기") { dismiss() } } }
        }
        .task {
            if box.model == nil {
                let session = self.session
                box.model = JoinInviteModel(api: session.partyAPI, afterJoin: { await session.syncNow() })
            }
        }
    }
}

@MainActor
final class JoinInviteBox: ObservableObject {
    @Published var model: JoinInviteModel?
}

private struct JoinForm: View {
    @ObservedObject var model: JoinInviteModel
    let onDone: () -> Void

    var body: some View {
        Form {
            Section {
                TextField("초대 코드 또는 링크", text: $model.input, axis: .vertical)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                Button("확인") { Task { await model.loadPreview() } }.disabled(model.input.isEmpty || model.busy)
            } footer: {
                Text("임시 방식: 앱 링크가 준비되기 전까지 받은 코드를 붙여 넣어 참여합니다.")
            }
            if let preview = model.preview {
                // 수락 전에는 Party 이름·인원·초대자 이름만 보인다(멤버 명단·일정 없음).
                Section("초대") {
                    LabeledContent("Party", value: preview.partyName)
                    LabeledContent("멤버", value: "\(preview.memberCount)명")
                    LabeledContent("초대한 사람", value: preview.inviterDisplayName)
                    Button("참여하기") { Task { if await model.accept() { onDone() } } }.disabled(model.busy)
                }
            }
            if let error = model.error { Section { Text(error).font(.footnote).foregroundStyle(.red) } }
        }
    }
}

struct PartyDetailView: View {
    @EnvironmentObject private var session: SessionStore
    let partyID: String
    @State private var invite: CreatedInvite?
    @State private var error: String?
    @State private var busy = false

    var body: some View {
        if let party = session.syncState?.parties().first(where: { $0.id == partyID }) {
            List {
                Section {
                    NavigationLink {
                        SearchView(partyID: partyID)
                    } label: { Label("모두 가능한 시간 찾기", systemImage: "calendar.badge.clock") }
                }
                Section("멤버 \(party.members.count)명") {
                    ForEach(party.members) { m in
                        HStack {
                            Text(m.displayName)
                            Spacer()
                            if m.role == "owner" { Text("방장").font(.caption).foregroundStyle(.secondary) }
                        }
                    }
                }
                if party.isOwner {
                    Section {
                        Button("초대 링크 만들기") {
                            Task { await createInvite() }
                        }
                        .disabled(busy)
                        if let invite {
                            VStack(alignment: .leading, spacing: 6) {
                                Text(invite.token).font(.system(.footnote, design: .monospaced)).textSelection(.enabled)
                                Text("\(invite.expiresAt.formatted(date: .abbreviated, time: .shortened))까지 · 최대 \(invite.maxUses)명")
                                    .font(.caption).foregroundStyle(.secondary)
                            }
                            ShareLink(item: invite.token) { Label("코드 공유", systemImage: "square.and.arrow.up") }
                            Button("코드 복사") { UIPasteboard.general.string = invite.token }
                        }
                    } header: { Text(party.members.count == 1 ? "초대 — 아직 혼자입니다" : "초대") } footer: {
                        Text("임시 방식: 앱 링크가 준비되기 전까지 이 코드를 상대에게 전하고, 상대는 Party 탭의 + → 초대 코드로 참여에 붙여 넣습니다. 코드는 지금 한 번만 보입니다.")
                    }
                }
                Section {
                    NavigationLink { VisibilityView(partyID: partyID) } label: {
                        LabeledContent("내 일정 공개", value: VisibilityView.summary(session.syncState?.myVisibility(party: partyID) ?? "busyOnly"))
                    }
                }
                if let error { Section { Text(error).font(.footnote).foregroundStyle(.red) } }
            }
            .navigationTitle(party.name)
            .refreshable { await session.syncNow() }
        } else {
            ContentUnavailableView("Party를 찾을 수 없습니다", systemImage: "person.3")
        }
    }

    private func createInvite() async {
        busy = true
        defer { busy = false }
        do {
            invite = try await session.partyAPI.createInvite(partyID: partyID)
            error = nil
        } catch {
            self.error = UserMessage.of(error)
        }
    }
}

/// 내 일정 공개(설계 5 §4.1). I3은 시간만 공개·숨김만 고른다. 상세 공개는 제목 업로드(V5)와 함께 붙인다.
struct VisibilityView: View {
    @EnvironmentObject private var session: SessionStore
    let partyID: String
    @State private var current: VisibilityState?
    @State private var confirmHidden = false
    @State private var error: String?
    @State private var busy = false

    static func title(_ level: String) -> String {
        switch level {
        case "busyOnly": return "시간만 공개"
        case "hidden": return "숨김"
        case "details": return "상세 공개"
        default: return "알 수 없음"   // 이 앱이 모르는 값(서버의 새 수준)
        }
    }

    /// Party 상세의 요약 라벨. 이 화면에서 바꿀 수 없는 값이면 그렇게 적는다.
    static func summary(_ level: String) -> String {
        isEditable(level) ? title(level) : "\(title(level)) (이 앱 버전에서는 변경 불가)"
    }

    /// 이 화면에서 바꿀 수 있는 현재 값인가. 다른 기기에서 고른 상세 공개는 이 앱 버전이 다루지 못하므로(제목 업로드는
    /// V5) 읽기 전용으로 두고 덮어쓰지 않는다.
    static func isEditable(_ level: String?) -> Bool {
        guard let level else { return false }
        return level == "busyOnly" || level == "hidden"
    }

    var body: some View {
        Form {
            if let level = current?.level, !Self.isEditable(level) {
                Section {
                    HStack {
                        VStack(alignment: .leading) {
                            Text("\(Self.title(level)) (이 앱 버전에서는 변경 불가)")
                            Text("다른 기기에서 고른 설정입니다. 이 화면에서는 바꾸지 않습니다.").font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer()
                        Image(systemName: "checkmark")
                    }
                }
            }
            Section {
                option("busyOnly", "시간만 일정 있음으로 표시합니다.")
                option("hidden", "일정은 보이지 않지만 모두 가능한 시간과 충돌 검사에는 반영됩니다.")
            } footer: {
                Text("다른 멤버의 공개 수준은 보이지 않습니다. 상세 공개(제목 공유)는 다음 단계에서 추가됩니다.")
            }
            if let error { Section { Text(error).font(.footnote).foregroundStyle(.red) } }
        }
        .navigationTitle("내 일정 공개")
        .task { await load() }
        .alert("숨김으로 바꿀까요?", isPresented: $confirmHidden) {
            Button("숨김", role: .destructive) { Task { await set("hidden") } }
            Button("취소", role: .cancel) {}
        } message: {
            Text("숨겨도 모두 가능한 시간 결과가 달라지는 것으로 일정이 있다는 사실이 간접적으로 드러날 수 있습니다.")
        }
    }

    private func option(_ level: String, _ text: String) -> some View {
        Button {
            if level == "hidden" { confirmHidden = true } else { Task { await set(level) } }
        } label: {
            HStack {
                VStack(alignment: .leading) {
                    Text(Self.title(level)).foregroundStyle(.primary)
                    Text(text).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                if current?.level == level { Image(systemName: "checkmark") }
            }
        }
        .disabled(busy || !Self.isEditable(current?.level) || current?.level == level)
    }

    private func load() async {
        do { current = try await session.partyAPI.visibility(partyID: partyID) } catch { self.error = UserMessage.of(error) }
    }

    /// 오프라인에서 큐에 넣지 않는다(설계 5 §4.2: 서버 적용 여부를 바로 확인한다).
    private func set(_ level: String) async {
        guard let current, Self.isEditable(current.level) else { return }
        busy = true
        defer { busy = false }
        do {
            self.current = try await session.partyAPI.setVisibility(partyID: partyID, level: level, ifMatch: current.version)
            error = nil
            await session.syncNow()
        } catch let e as APIError where e.serverCode == "version_conflict" {
            await load()
            error = UserMessage.of(e)
        } catch {
            self.error = UserMessage.of(error)
        }
    }
}
