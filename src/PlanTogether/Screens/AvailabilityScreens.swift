import SwiftUI

// 활동 시간 설정과 공통 가능 시간 검색(설계 6 §3·§8).

struct PreferencesView: View {
    @EnvironmentObject private var session: SessionStore
    @StateObject private var model: PreferencesViewModelBox = PreferencesViewModelBox()

    var body: some View {
        Group {
            if let vm = model.vm { PreferencesForm(vm: vm) } else { ProgressView() }
        }
        .navigationTitle("활동 시간")
        .task {
            // 한 번 읽은 뒤에는 탭을 오가도 다시 읽지 않는다(저장하지 않은 편집을 지키려고). 첫 읽기가 실패했으면 다시 읽는다.
            let vm = model.vm ?? PreferencesViewModel(api: session.api)
            model.vm = vm
            guard !vm.loaded else { return }
            if await vm.load() { await session.preferencesRefreshed() }
        }
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button("다시 불러오기") {
                    guard let vm = model.vm else { return }
                    Task { if await vm.load() { await session.preferencesRefreshed() } }
                }
                .disabled(model.vm?.isBusy ?? true)
            }
        }
        // 다른 기기에서 바꾸면 sync가 무효화 신호만 준다. 다시 읽고, 읽기에 성공했을 때만 표시를 지운다.
        .onChange(of: session.syncState?.needsPreferenceRefresh ?? false) { _, needs in
            guard needs, let vm = model.vm else { return }
            Task { if await vm.load() { await session.preferencesRefreshed() } }
        }
    }
}

@MainActor
final class PreferencesViewModelBox: ObservableObject {
    @Published var vm: PreferencesViewModel?
}

private struct PreferencesForm: View {
    @ObservedObject var vm: PreferencesViewModel
    private let steps = Array(stride(from: 0, to: 1440, by: 30))

    var body: some View {
        Form {
            if vm.isSuggestion {
                Section {
                    Text("아직 설정하지 않았습니다. 아래 제안값(매일 09:00~22:00)은 저장해야 적용됩니다. 저장하기 전에는 모두 가능한 시간을 찾을 수 없습니다.")
                        .font(.footnote)
                }
            }
            Section("시간대") {
                if vm.needsCityPicker {
                    Picker("도시", selection: $vm.timeZone) {
                        ForEach(PreferencesRules.regionZones, id: \.self) { Text($0).tag($0) }
                    }
                    Text("기기 시간대(\(TimeZone.current.identifier))는 쓸 수 없어 도시를 골라야 합니다.").font(.caption)
                } else {
                    LabeledContent("시간대", value: vm.timeZone)
                }
            }
            ForEach(PreferencesRules.weekdays, id: \.self) { day in
                Section("\(PreferencesRules.weekdayNames[day] ?? "")요일") {
                    let list = vm.days[day] ?? []
                    if list.isEmpty { Text("활동 안 함").foregroundStyle(.secondary) }
                    ForEach(list) { interval in
                        HStack {
                            Picker("시작", selection: binding(day, interval.id, \.startMinute)) {
                                ForEach(steps, id: \.self) { Text(PreferencesRules.label($0)).tag($0) }
                            }
                            .labelsHidden()
                            Text("~")
                            Picker("끝", selection: binding(day, interval.id, \.endMinute)) {
                                ForEach(steps, id: \.self) { Text(PreferencesRules.label($0)).tag($0) }
                            }
                            .labelsHidden()
                            if (vm.days[day]?.first { $0.id == interval.id })?.crossesMidnight == true {
                                Text("다음 날").font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Button(role: .destructive) { vm.removeInterval(day, interval.id) } label: { Image(systemName: "minus.circle") }
                                .buttonStyle(.borderless)
                        }
                    }
                    if list.count < PreferencesRules.maxPerDay {
                        Button("구간 추가") { vm.addInterval(day) }
                    }
                }
            }
            Section {
                Button("저장") { Task { await vm.save() } }.disabled(vm.isBusy)
                if let m = vm.message { Text(m).font(.footnote) }
            }
        }
    }

    private func binding(_ day: Int, _ id: UUID, _ key: WritableKeyPath<EditableInterval, Int>) -> Binding<Int> {
        Binding(
            get: { vm.days[day]?.first { $0.id == id }?[keyPath: key] ?? 0 },
            set: { v in
                guard let i = vm.days[day]?.firstIndex(where: { $0.id == id }) else { return }
                vm.days[day]![i][keyPath: key] = v
            })
    }
}

struct SearchView: View {
    @EnvironmentObject private var session: SessionStore
    let partyID: String
    @StateObject private var box = SearchViewModelBox()

    var body: some View {
        Group {
            if let vm = box.vm { SearchContent(vm: vm) } else { ProgressView() }
        }
        .navigationTitle("모두 가능한 시간")
        .task {
            guard box.vm == nil else { return }
            let vm = SearchViewModel(api: session.api, partyID: partyID)
            box.vm = vm
            await vm.prepare()
        }
    }
}

@MainActor
final class SearchViewModelBox: ObservableObject {
    @Published var vm: SearchViewModel?
}

struct SearchContent: View {
    @EnvironmentObject private var session: SessionStore
    @ObservedObject var vm: SearchViewModel

    var body: some View {
        Form {
            Section("조건") {
                DatePicker("시작", selection: $vm.form.startDate, displayedComponents: .date)
                DatePicker("끝", selection: $vm.form.endDate, displayedComponents: .date)
                Picker("약속 길이", selection: $vm.form.durationMinutes) {
                    ForEach(SearchForm.durations, id: \.self) { m in Text(m % 60 == 0 ? "\(m / 60)시간" : "\(m / 60)시간 \(m % 60)분").tag(m) }
                }
                Picker("시간대", selection: Binding(get: { vm.form.timeZone.identifier },
                                                   set: { if let tz = TimeZone(identifier: $0) { vm.setTimeZone(tz) } })) {
                    if vm.needsTimeZonePick { Text("도시를 고르세요").tag(vm.form.timeZone.identifier) }
                    ForEach(PreferencesRules.regionZones, id: \.self) { Text($0).tag($0) }
                }
                Button("찾기") { Task { await vm.search() } }.disabled(vm.needsTimeZonePick)
                if let m = vm.validationMessage { Text(m).font(.footnote).foregroundStyle(.red) }
            }
            switch vm.state {
            case .idle:
                EmptyView()
            case .loading:
                Section { ProgressView() }
            case .results(let sections, let until):
                ForEach(sections) { day in
                    Section(day.title) {
                        ForEach(day.slots) { Text($0.label) }
                    }
                }
                Section {
                    // valid_until이 지나면 화면을 열어 둔 채로도 만료 표시가 나오게 이 문구만 주기적으로 다시 그린다.
                    TimelineView(.periodic(from: .now, by: 10)) { _ in
                        if vm.isExpired {
                            Text("결과가 만료됐습니다. 다시 찾아 주세요.").font(.footnote).foregroundStyle(.red)
                        } else {
                            Text("\(until.formatted(date: .omitted, time: .shortened))까지 유효한 결과입니다.").font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }
            case .empty:
                Section {
                    Text("선택한 기간과 활동 시간 안에 모두 가능한 시간이 없습니다.")
                    Text("기간을 늘리거나 약속 길이를 줄여 보세요.").font(.caption).foregroundStyle(.secondary)
                }
            case .pending(_, let activityUnconfigured, let calendarState):
                Section {
                    Text("그룹 준비를 기다리는 중")
                    // 본인 문제일 때만 CTA. 다른 멤버의 수·원인은 보여 주지 않는다.
                    if activityUnconfigured { Text("내 활동 시간을 먼저 설정하세요. (활동 시간 탭)").font(.footnote) }
                    switch CalendarCTA.of(calendarState) {
                    case .none: EmptyView()
                    case .connect:
                        Text("내 캘린더가 연결되지 않았습니다.").font(.footnote)
                        Button("캘린더 연결") { Task { await session.calendarConnect() } }
                    case .syncing:
                        HStack { ProgressView(); Text("내 캘린더를 올리는 중입니다.").font(.footnote) }
                    case .refresh:
                        Text("내 캘린더 정보가 오래됐습니다.").font(.footnote)
                        Button("내 캘린더 새로고침") { Task { await session.calendarRefresh(.manual) } }
                    case .allowInSettings:
                        Text("캘린더 접근이 꺼져 있습니다. 설정 앱에서 전체 접근을 허용하세요.").font(.footnote)
                    case .selectCalendars:
                        Text("읽을 캘린더를 골라 주세요. (캘린더 탭)").font(.footnote)
                    }
                }
            case .failed(let message):
                Section { Text(message).font(.footnote).foregroundStyle(.red) }
            }
        }
    }
}
