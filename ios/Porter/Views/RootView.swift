import PorterKit
import SwiftUI

struct RootView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        Group {
            switch model.phase {
            case .connected:
                if model.status?.ready == false {
                    NavigationStack { SetupView() }
                } else {
                    MainTabs()
                }
            default:
                NavigationStack { ConnectView() }
            }
        }
        .alert("Porter", isPresented: Binding(
            get: { model.errorMessage != nil },
            set: { if !$0 { model.errorMessage = nil } }
        )) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(model.errorMessage ?? "")
        }
    }
}

struct MainTabs: View {
    var body: some View {
        TabView {
            NavigationStack { DashboardView() }
                .tabItem { Label("Overview", systemImage: "gauge.with.dots.needle.50percent") }
            NavigationStack { InternetView() }
                .tabItem { Label("Internet", systemImage: "globe") }
            NavigationStack { DevicesView() }
                .tabItem { Label("Devices", systemImage: "laptopcomputer.and.iphone") }
            NavigationStack { VPNView() }
                .tabItem { Label("VPN", systemImage: "lock.shield") }
            NavigationStack { SettingsView() }
                .tabItem { Label("Settings", systemImage: "gearshape") }
        }
    }
}
