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
                } else if let screen = model.demoScreen {
                    DemoScreen(name: screen)
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
    @State private var selection: String

    init(initial: String = "overview") {
        _selection = State(initialValue: initial)
    }

    var body: some View {
        TabView(selection: $selection) {
            NavigationStack { DashboardView() }
                .tabItem { Label("Overview", systemImage: "gauge.with.dots.needle.50percent") }
                .tag("overview")
            NavigationStack { InternetView() }
                .tabItem { Label("Internet", systemImage: "globe") }
                .tag("internet")
            NavigationStack { DevicesView() }
                .tabItem { Label("Devices", systemImage: "laptopcomputer.and.iphone") }
                .tag("devices")
            NavigationStack { VPNView() }
                .tabItem { Label("VPN", systemImage: "lock.shield") }
                .tag("vpn")
            NavigationStack { SettingsView() }
                .tabItem { Label("Settings", systemImage: "gearshape") }
                .tag("settings")
        }
    }
}

/// Opens one screen directly in demo mode (for screenshots and UI work).
struct DemoScreen: View {
    @EnvironmentObject private var model: AppModel
    let name: String

    var body: some View {
        switch name {
        case "traffic":
            NavigationStack { TrafficView() }
        case "flows":
            NavigationStack { FlowsView(mac: "") }
        case "speedtest":
            NavigationStack { SpeedTestView() }
        case "device":
            NavigationStack { DeviceDetailView(mac: model.devices.first?.mac ?? "") }
        default:
            MainTabs(initial: name)
        }
    }
}
