import SwiftUI

@main
struct PlanTogetherApp: App {
    @StateObject private var session = SessionStore.live()
    @Environment(\.scenePhase) private var scenePhase

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(session)
                .task { await session.restore() }
                .onChange(of: scenePhase) { _, phase in
                    if phase == .active { Task { await session.appBecameActive() } }
                }
        }
    }
}
