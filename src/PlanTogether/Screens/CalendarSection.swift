import SwiftUI

/// 캘린더 연결 최소 화면(I2): 연결(권한 요청), 읽을 캘린더 선택(기본 비움), 새로고침, 서버 상태.
struct CalendarSection: View {
    @EnvironmentObject private var session: SessionStore

    var body: some View {
        Section("캘린더") {
            let status = session.calendarStatus
            LabeledContent("서버 상태", value: status?.connection?.status ?? "-")
            if let last = status?.connection?.lastCompletedSyncAt {
                LabeledContent("마지막 업로드", value: last.formatted(date: .abbreviated, time: .shortened))
            }
            if let n = status?.uploadedFacts { LabeledContent("올린 일정", value: "\(n)") }
            switch status?.note {
            case .otherSyncDevice?: Text("다른 기기가 캘린더를 동기화하고 있습니다. 연결을 누르면 이 기기로 가져옵니다.").font(.footnote)
            case .needsSelection?: Text("읽을 캘린더를 하나 이상 선택하세요.").font(.footnote)
            default: EmptyView()
            }
            switch status?.authorization {
            case .denied?, .writeOnly?: Text("캘린더 접근이 거부됐습니다. 설정 앱에서 전체 접근을 허용하세요.").font(.footnote)
            case .restricted?: Text("이 기기에서는 캘린더 접근이 제한돼 있습니다.").font(.footnote)
            default: EmptyView()
            }
            Button("캘린더 연결") { Task { await session.calendarConnect() } }
                .disabled(session.isBusy)
            if let status, status.authorization == .fullAccess {
                ForEach(status.calendars) { cal in
                    Toggle(isOn: Binding(
                        get: { status.selectedIDs.contains(cal.id) },
                        set: { on in
                            var ids = status.selectedIDs
                            if on { ids.insert(cal.id) } else { ids.remove(cal.id) }
                            Task { await session.calendarSetSelection(ids) }
                        })) {
                        VStack(alignment: .leading) {
                            Text(cal.title)
                            Text(cal.sourceTitle).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    .disabled(session.isBusy)
                }
                Button("지금 업로드") { Task { await session.calendarRefresh(.manual) } }
                    .disabled(session.isBusy || status.selectedIDs.isEmpty)
                #if DEBUG
                Button("시연 일정 만들기 (DEBUG)") { Task { await session.createSampleCalendarEvents() } }
                    .disabled(session.isBusy)
                #endif
            }
        }
    }
}
