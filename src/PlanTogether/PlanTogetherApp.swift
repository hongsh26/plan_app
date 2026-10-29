import SwiftUI

@main
struct PlanTogetherApp: App {
    @StateObject private var store = AppStore.demo
    @StateObject private var session = SessionStore.live()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(store)
                .environmentObject(session)
                .task { await session.restore() }
        }
    }
}
