import PorterKit
import SwiftUI
import UIKit

/// The overview tab: what the router is doing right now.
struct DashboardView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        List {
            if let status = model.status {
                InternetSummarySection(status: status)
                if status.portal.captive {
                    PortalSection(portal: status.portal)
                }
                AccessPointSection(aps: status.aps)
                if !status.stas.isEmpty {
                    UpstreamWifiSection(stas: status.stas)
                }
                if !status.vpns.isEmpty {
                    DashboardVPNSection(vpns: status.vpns)
                }
                RouterInfoSection(status: status)
                RecentActivitySection()
            } else {
                ProgressView()
            }
        }
        .navigationTitle(model.status?.name ?? "Overview")
        .refreshable { await model.refresh() }
    }
}

private struct InternetSummarySection: View {
    let status: RouterStatus

    private var activeUplink: UplinkStatus? {
        guard let active = status.active, !active.isEmpty else { return nil }
        return status.wan.first(where: { $0.iface == active })
    }

    var body: some View {
        Section("Internet") {
            HStack(spacing: 10) {
                StatusDot(color: internetColor(status.internet))
                Text(internetLabel(status.internet))
                    .font(.headline)
            }
            if let uplink = activeUplink {
                LabeledContent("Uplink", value: uplinkLabel(kind: uplink.kind, iface: uplink.iface))
                LabeledContent("IPv4", value: uplink.ipv4 ?? "None")
                LabeledContent("IPv6", value: uplink.hasV6 ? "Available" : "Not available")
            } else {
                Text("No uplink has a connection. Plug in a cable or join a Wi-Fi network on the Internet tab.")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

private struct PortalSection: View {
    @EnvironmentObject private var model: AppModel
    let portal: PortalStatus

    private var currentDevice: String { portal.device ?? "" }

    private var choices: [DeviceInfo] {
        model.devices.filter { $0.online || $0.mac == currentDevice }
    }

    private var explanation: String {
        let lead = "The upstream network wants a sign-in before it gives internet access. "
        if currentDevice.isEmpty {
            return lead + "Connect a device to the router to sign in from it."
        }
        return lead + model.deviceName(currentDevice) + " is let straight through, past VPNs and rules, so the sign-in can be done on it. The rest of the network gets internet once it has."
    }

    private var deviceBinding: Binding<String> {
        Binding(
            get: { currentDevice },
            set: { mac in
                Task { await model.attempt { try await $0.setPortalDevice(mac: mac) } }
            }
        )
    }

    var body: some View {
        Section {
            Label("Sign-in required", systemImage: "exclamationmark.triangle.fill")
                .font(.headline)
                .foregroundStyle(.orange)
            Text(explanation)
                .font(.footnote)
            Picker("Sign-in device", selection: deviceBinding) {
                Text("Automatic").tag("")
                if !currentDevice.isEmpty && !choices.contains(where: { $0.mac == currentDevice }) {
                    Text(model.deviceName(currentDevice)).tag(currentDevice)
                }
                ForEach(choices) { device in
                    Text(device.displayName).tag(device.mac)
                }
            }
            Button("Open sign-in page") { openPortal() }
            Button("Reconnect that device") {
                Task { await model.attempt { try await $0.kickPortalDevice() } }
            }
            .disabled(currentDevice.isEmpty)
            Button("Check again") {
                Task { await model.attempt { try await $0.recheckInternet() } }
            }
        } header: {
            Text("Captive portal")
        } footer: {
            Text("\"Open sign-in page\" works when this phone is the sign-in device and is on the router's Wi-Fi. Reconnecting a device makes it show its own sign-in sheet again.")
        }
    }

    private func openPortal() {
        var text = portal.url ?? ""
        if text.isEmpty {
            text = "http://captive.apple.com"
        }
        if let url = URL(string: text) {
            UIApplication.shared.open(url)
        }
    }
}

private struct AccessPointSection: View {
    @EnvironmentObject private var model: AppModel
    let aps: [AccessPointStatus]

    var body: some View {
        Section("Access point") {
            if aps.isEmpty {
                Text("No radio is running an access point.")
                    .foregroundStyle(.secondary)
            }
            ForEach(aps) { ap in
                AccessPointRow(ap: ap, ssid: model.config?.ap.ssid ?? "")
            }
        }
    }
}

private struct AccessPointRow: View {
    let ap: AccessPointStatus
    let ssid: String

    private var stateColor: Color {
        switch ap.state {
        case "up": return Color.green
        case "starting": return Color.yellow
        case "failed": return Color.red
        default: return Color.gray
        }
    }

    private var stateLabel: String {
        switch ap.state {
        case "up": return "On"
        case "starting": return "Starting"
        case "failed": return "Failed"
        case "stopped": return "Stopped"
        default: return ap.state
        }
    }

    private var detail: String {
        var parts: [String] = [stateLabel]
        if !ap.band.isEmpty { parts.append(ap.band + " GHz") }
        if ap.channel > 0 { parts.append("channel \(ap.channel)") }
        if let mode = ap.mode, !mode.isEmpty { parts.append("802.11" + mode) }
        return parts.joined(separator: " · ")
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 10) {
                StatusDot(color: stateColor)
                Text(ssid.isEmpty ? ap.iface : ssid)
            }
            Text(detail)
                .font(.caption)
                .foregroundStyle(.secondary)
            if let error = ap.error, !error.isEmpty {
                Text(error)
                    .font(.caption)
                    .foregroundStyle(.red)
            }
        }
    }
}

private struct UpstreamWifiSection: View {
    let stas: [StationStatus]

    var body: some View {
        Section("Upstream Wi-Fi") {
            ForEach(stas) { sta in
                StationRow(sta: sta)
            }
        }
    }
}

private struct StationRow: View {
    let sta: StationStatus

    private var title: String {
        if let ssid = sta.ssid, !ssid.isEmpty { return ssid }
        return "No network"
    }

    private var stateLabel: String {
        switch sta.state {
        case "connected": return "Connected"
        case "connecting": return "Connecting"
        default: return "Not connected"
        }
    }

    private var failure: String? {
        guard let failed = sta.failed, !failed.isEmpty else { return nil }
        return failed + ": " + joinFailureLabel(sta.failReason)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(title)
                Spacer()
                Text(stateLabel)
                    .foregroundStyle(.secondary)
                if sta.state == "connected", let signal = sta.signal {
                    SignalBars(rssi: signal)
                }
            }
            if let failure {
                Text(failure)
                    .font(.caption)
                    .foregroundStyle(.red)
            }
        }
    }
}

private struct DashboardVPNSection: View {
    @EnvironmentObject private var model: AppModel
    let vpns: [VPNStatus]

    var body: some View {
        Section("VPN") {
            ForEach(vpns) { vpn in
                Toggle(isOn: model.vpnEnabled(vpn.id)) {
                    HStack(spacing: 10) {
                        StatusDot(color: vpnStateColor(vpn.state))
                        VStack(alignment: .leading, spacing: 2) {
                            Text(vpn.name)
                            Text(vpnStateLabel(vpn))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
    }
}

private struct RouterInfoSection: View {
    let status: RouterStatus

    private var temperature: String {
        String(format: "%.0f °C", status.temp)
    }

    var body: some View {
        Section("Router") {
            LabeledContent("Connected devices", value: String(status.lan.clients))
            if status.temp > 0 {
                LabeledContent("Temperature", value: temperature)
            }
            LabeledContent("Uptime", value: formatDuration(status.uptime))
            LabeledContent("Version", value: status.version)
        }
    }
}

private struct RecentActivitySection: View {
    @EnvironmentObject private var model: AppModel

    private var recent: [ActivityEvent] {
        Array(model.activity.suffix(10).reversed())
    }

    var body: some View {
        Section("Recent activity") {
            if recent.isEmpty {
                Text("Nothing has happened yet.")
                    .foregroundStyle(.secondary)
            }
            ForEach(Array(recent.enumerated()), id: \.offset) { item in
                ActivityRow(event: item.element)
            }
        }
    }
}
