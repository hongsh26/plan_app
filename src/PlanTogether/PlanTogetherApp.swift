import SwiftUI

@main
struct PlanTogetherApp: App {
    @StateObject private var store = AppStore.demo

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(store)
        }
    }
}
