import PorterKit
import SwiftUI

struct RootView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        Group {
            switch model.phase {
            case .connected:
                if let mismatch = model.mismatch {
                    NavigationStack { MismatchView(mismatch: mismatch) }
                } else if model.status?.ready == false {
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
        .overlay(alignment: .top) {
            if model.reconnecting {
                ReconnectingSign()
                    .transition(.opacity)
            }
        }
        .animation(.default, value: model.reconnecting)
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { model.foreground() }
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

/// Shown above whatever is on screen while a connection that was lost is
/// being brought back. Everything under it stays as it is and can still
/// be used; what is asked of the router waits for the connection.
private struct ReconnectingSign: View {
    var body: some View {
        HStack(spacing: 8) {
            ProgressView()
                .controlSize(.small)
            Text("Reconnecting to the router")
                .font(.footnote)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
        .background(.regularMaterial, in: Capsule())
        .padding(.top, 4)
        .allowsHitTesting(false)
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

    /// The Wi-Fi network the router has joined, as the internet source
    /// whose smoothing page is shown.
    private var joinedSource: SmoothingTarget {
        let stations: [StationStatus] = model.status?.stas ?? []
        let joined: String = stations.first(where: { $0.state == "connected" })?.ssid ?? ""
        return SmoothingTarget(key: RouterConfig.uplinkWifi, ssid: joined)
    }

    var body: some View {
        switch name {
        case "traffic":
            NavigationStack { TrafficView() }
        case "flows":
            NavigationStack { FlowsView(mac: "") }
        case "speedtest":
            NavigationStack { SpeedTestView() }
        case "adblock":
            NavigationStack { AdblockPage() }
        case "device":
            NavigationStack { DeviceDetailView(mac: model.devices.first?.mac ?? "") }
        case "software":
            NavigationStack { SoftwarePage() }
        case "networks":
            NavigationStack { NetworkSetupPage() }
        case "outside":
            NavigationStack { InboundPage() }
        case "smoothing":
            NavigationStack { SmoothingPage(title: "Smooth out heavy use", heading: false, target: joinedSource) }
        case "setup":
            NavigationStack { SetupView() }
        case "connect":
            NavigationStack { ConnectView() }
        case "share":
            WifiShareSheet()
        default:
            MainTabs(initial: name)
        }
    }
}

/// Shown in place of everything else when the router's software and the
/// app are of different generations. An older router can be updated from
/// here; a newer one needs a newer app.
struct MismatchView: View {
    @EnvironmentObject private var model: AppModel
    let mismatch: AppModel.Mismatch

    private var title: String {
        mismatch == .routerOlder ? "The router needs an update" : "This app is out of date"
    }

    private var explanation: String {
        if mismatch == .routerOlder {
            return "The router's software is older than this app, so the app cannot show its settings yet. Update the router here, and the app carries on once it has restarted."
        }
        return "The router's software is newer than this app, so the app cannot show its settings. Download the current version of Porter and install the app from it again."
    }

    var body: some View {
        List {
            Section {
                VStack(alignment: .leading, spacing: 8) {
                    Text(title)
                        .font(.headline)
                    Text(explanation)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                .padding(.vertical, 4)
            }
            if mismatch == .routerOlder {
                Section {
                    NavigationLink("Update the router") { SoftwarePage() }
                }
            }
            Section {
                Button("Disconnect from the router") { model.disconnect() }
            }
        }
        .navigationTitle("Porter")
    }
}
