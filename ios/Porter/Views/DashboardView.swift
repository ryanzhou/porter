import PorterKit
import SwiftUI
import UIKit

/// The overview tab: what the router is doing right now.
struct DashboardView: View {
    @EnvironmentObject private var model: AppModel
    @State private var lastHour: [TrafficSample] = []
    @State private var sharing = false

    var body: some View {
        ScrollView {
            if let status = model.status {
                VStack(spacing: 12) {
                    PathCard(status: status)
                    if status.portal.captive {
                        PortalCard(portal: status.portal)
                    }
                    WifiCard(aps: status.aps, sharing: $sharing)
                    TrafficCard(samples: lastHour)
                    HStack(spacing: 12) {
                        SpeedTile()
                        BlockedTile(adblock: status.adblock)
                    }
                    if !status.vpns.isEmpty {
                        VPNCard(vpns: status.vpns)
                    }
                    ActivityCard()
                    RouterFootnote(status: status)
                }
                .padding(.horizontal, 16)
                .padding(.bottom, 24)
            } else {
                ProgressView()
                    .padding(.top, 120)
            }
        }
        .background(Surface.page)
        .navigationTitle(model.status?.name ?? "Porter")
        .refreshable {
            await model.refresh()
            await loadTraffic()
        }
        .task { await loadTraffic() }
        .sheet(isPresented: $sharing) {
            WifiShareSheet()
                .environmentObject(model)
        }
    }

    private func loadTraffic() async {
        let found: TrafficStats? = await model.fetch { try await $0.trafficStats(mac: "") }
        if let found {
            lastHour = found.minutes
        }
    }
}

// MARK: - The path

/// Devices, the router, the VPN when everything goes through one, and the
/// internet, joined by lines that show which legs are working.
private struct PathCard: View {
    @EnvironmentObject private var model: AppModel
    let status: RouterStatus

    private var uplink: UplinkStatus? {
        guard let active = status.active, !active.isEmpty else { return nil }
        return status.wan.first(where: { $0.iface == active })
    }

    private var station: StationStatus? {
        guard let uplink, uplink.kind == "wifi" else { return nil }
        return status.stas.first(where: { $0.iface == uplink.iface })
    }

    /// The upstream network by name: the Wi-Fi network, or "Ethernet".
    private var source: String {
        guard let uplink else { return "No uplink" }
        if uplink.kind == "wifi" {
            if let ssid = station?.ssid, !ssid.isEmpty { return ssid }
            return "Wi-Fi"
        }
        return "Ethernet"
    }

    /// A Wi-Fi network that turned the router away.
    private var refusal: String? {
        for sta in status.stas {
            if let failed = sta.failed, !failed.isEmpty {
                return failed + ": " + joinFailureLabel(sta.failReason)
            }
        }
        return nil
    }

    /// The VPN everything goes through by default, if there is one.
    private var tunnel: VPNStatus? {
        guard let action = RouteAction(model.config?.policy.`default` ?? ""), case .vpn(let id) = action else { return nil }
        return status.vpns.first(where: { $0.id == id })
    }

    private var lanUp: Bool {
        status.aps.contains(where: { $0.state == "up" }) || status.lan.clients > 0
    }

    private var deviceCaption: String {
        status.lan.clients == 1 ? "1 device" : "\(status.lan.clients) devices"
    }

    private var stateColor: Color { internetColor(status.internet) }

    var body: some View {
        Card {
            VStack(alignment: .leading, spacing: 20) {
                headline
                diagram
                if let uplink {
                    addresses(uplink)
                }
            }
        }
    }

    private var headline: some View {
        HStack(alignment: .center, spacing: 10) {
            VStack(alignment: .leading, spacing: 2) {
                Text(internetLabel(status.internet))
                    .font(.title.weight(.bold))
                    .fontDesign(.rounded)
                    .foregroundStyle(stateColor)
                Text(source)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                if let refusal {
                    Text(refusal)
                        .font(.footnote)
                        .foregroundStyle(.red)
                }
            }
            Spacer()
            if let signal = station?.signal {
                SignalBars(rssi: signal)
            }
        }
    }

    private var diagram: some View {
        HStack(spacing: 0) {
            PathNode(symbol: "laptopcomputer.and.iphone", caption: deviceCaption, color: Color.accentColor)
            PathLink(color: lanUp ? Color.green : Color.gray, broken: !lanUp)
            PathNode(symbol: "wifi.router.fill", caption: status.name, color: Color.accentColor, filled: true)
            if let tunnel {
                PathLink(color: vpnStateColor(tunnel.state), broken: tunnel.state != "up")
                PathNode(symbol: "lock.shield.fill", caption: tunnel.name, color: vpnStateColor(tunnel.state))
            }
            PathLink(color: stateColor, broken: status.internet != "online")
            PathNode(symbol: "globe", caption: uplink == nil ? "Offline" : "Internet", color: stateColor, filled: status.internet == "online")
        }
        .padding(.horizontal, 24)
        .padding(.bottom, 22)
    }

    private func addresses(_ uplink: UplinkStatus) -> some View {
        HStack(spacing: 8) {
            Text(uplink.ipv4 ?? "No IPv4")
                .font(.footnote.monospaced())
                .foregroundStyle(.secondary)
            if uplink.hasV6 {
                Badge(text: "IPv6", color: Color.accentColor)
            }
            Spacer()
            Text(uplink.iface)
                .font(.footnote.monospaced())
                .foregroundStyle(.tertiary)
        }
    }
}

private struct PathNode: View {
    let symbol: String
    let caption: String
    let color: Color
    var filled: Bool = false

    var body: some View {
        Glyph(symbol: symbol, color: color, size: 46, filled: filled)
            .overlay(alignment: .top) {
                Text(caption)
                    .font(.caption2.weight(.medium))
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .frame(width: 92)
                    .offset(y: 52)
            }
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(Text(caption))
    }
}

private struct PathLine: Shape {
    func path(in rect: CGRect) -> Path {
        var path = Path()
        path.move(to: CGPoint(x: rect.minX, y: rect.midY))
        path.addLine(to: CGPoint(x: rect.maxX, y: rect.midY))
        return path
    }
}

/// One leg of the path: solid when it carries traffic, dotted when not.
private struct PathLink: View {
    let color: Color
    var broken: Bool = false

    private var style: StrokeStyle {
        let dash: [CGFloat] = broken ? [1, 7] : []
        return StrokeStyle(lineWidth: 3, lineCap: .round, dash: dash)
    }

    var body: some View {
        PathLine()
            .stroke(color, style: style)
            .frame(height: 3)
            .frame(maxWidth: .infinity)
            .padding(.horizontal, 7)
            .accessibilityHidden(true)
    }
}

// MARK: - Sign-in

private struct PortalCard: View {
    @EnvironmentObject private var model: AppModel
    let portal: PortalStatus

    private var currentDevice: String { portal.device ?? "" }

    private var choices: [DeviceInfo] {
        model.devices.filter { $0.online || $0.mac == currentDevice }
    }

    private var who: String {
        currentDevice.isEmpty ? "Join the router's Wi-Fi to sign in." : "Sign in on " + model.deviceName(currentDevice) + "."
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
        Card {
            VStack(alignment: .leading, spacing: 12) {
                HStack(spacing: 10) {
                    Glyph(symbol: "person.badge.key.fill", color: Color.orange, filled: true)
                    VStack(alignment: .leading, spacing: 1) {
                        Text("Sign-in required")
                            .font(.headline)
                        Text(who)
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                    }
                }
                Button {
                    openPortal()
                } label: {
                    Text("Open sign-in page")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .tint(Color.orange)
                HStack(spacing: 8) {
                    Picker("Sign-in device", selection: deviceBinding) {
                        Text("Automatic").tag("")
                        if !currentDevice.isEmpty && !choices.contains(where: { $0.mac == currentDevice }) {
                            Text(model.deviceName(currentDevice)).tag(currentDevice)
                        }
                        ForEach(choices) { device in
                            Text(device.displayName).tag(device.mac)
                        }
                    }
                    .labelsHidden()
                    Spacer()
                    Button("Reconnect") {
                        Task { await model.attempt { try await $0.kickPortalDevice() } }
                    }
                    .disabled(currentDevice.isEmpty)
                    Button("Recheck") {
                        Task { await model.attempt { try await $0.recheckInternet() } }
                    }
                }
                .buttonStyle(.bordered)
                .controlSize(.small)
            }
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

// MARK: - The router's own network

private func accessPointColor(_ state: String) -> Color {
    switch state {
    case "up": return Color.green
    case "starting": return Color.yellow
    case "failed": return Color.red
    default: return Color.gray
    }
}

private func accessPointDetail(_ ap: AccessPointStatus) -> String {
    var parts: [String] = []
    switch ap.state {
    case "up": break
    case "starting": parts.append("Starting")
    case "failed": parts.append("Failed")
    default: parts.append("Off")
    }
    if !ap.band.isEmpty { parts.append(ap.band + " GHz") }
    if ap.channel > 0 { parts.append("ch \(ap.channel)") }
    return parts.joined(separator: " · ")
}

private struct WifiCard: View {
    @EnvironmentObject private var model: AppModel
    let aps: [AccessPointStatus]
    @Binding var sharing: Bool

    private var ssid: String { model.config?.ap.ssid ?? "" }

    private var color: Color {
        if aps.contains(where: { $0.state == "up" }) { return Color.green }
        if let first = aps.first { return accessPointColor(first.state) }
        return Color.gray
    }

    var body: some View {
        Card {
            HStack(spacing: 12) {
                Glyph(symbol: "wifi", color: color, filled: color == Color.green)
                VStack(alignment: .leading, spacing: 2) {
                    Text(ssid.isEmpty ? "Wi-Fi" : ssid)
                        .font(.headline)
                        .lineLimit(1)
                    if aps.isEmpty {
                        Text("Off")
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                    }
                    ForEach(aps) { ap in
                        Text(accessPointDetail(ap))
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                        if let error = ap.error, !error.isEmpty {
                            Text(error)
                                .font(.footnote)
                                .foregroundStyle(.red)
                        }
                    }
                }
                Spacer()
                Button {
                    sharing = true
                } label: {
                    Image(systemName: "qrcode")
                        .font(.title3)
                        .frame(width: 44, height: 44)
                }
                .accessibilityLabel(Text("Share Wi-Fi"))
                .disabled(ssid.isEmpty)
            }
        }
    }
}

/// The network's name, password and a code another phone can scan to join.
struct WifiShareSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    private var ap: AccessPointConfig? { model.config?.ap }

    private var code: UIImage? {
        guard let ap else { return nil }
        return qrCode(wifiJoinCode(ssid: ap.ssid, passphrase: ap.passphrase, hidden: ap.hidden))
    }

    var body: some View {
        NavigationStack {
            VStack(spacing: 20) {
                Spacer()
                if let code {
                    Image(uiImage: code)
                        .interpolation(.none)
                        .resizable()
                        .scaledToFit()
                        .frame(maxWidth: 240)
                        .padding(16)
                        .background(Color.white, in: RoundedRectangle(cornerRadius: 24, style: .continuous))
                        .accessibilityLabel(Text("Code to join the network"))
                }
                VStack(spacing: 6) {
                    Text(ap?.ssid ?? "")
                        .font(.title2.weight(.bold))
                        .fontDesign(.rounded)
                    Text(ap?.passphrase ?? "")
                        .font(.body.monospaced())
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
                Button {
                    UIPasteboard.general.string = ap?.passphrase ?? ""
                } label: {
                    Label("Copy password", systemImage: "doc.on.doc")
                }
                .buttonStyle(.bordered)
                Spacer()
                Spacer()
            }
            .frame(maxWidth: .infinity)
            .padding(24)
            .background(Surface.page)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
        .presentationDetents([.large])
    }
}

// MARK: - Traffic

private struct TrafficCard: View {
    let samples: [TrafficSample]

    private var down: UInt64 {
        var total: UInt64 = 0
        for sample in samples {
            total = total &+ sample.down
        }
        return total
    }

    private var up: UInt64 {
        var total: UInt64 = 0
        for sample in samples {
            total = total &+ sample.up
        }
        return total
    }

    private var series: [Double] {
        samples.map { Double($0.down) + Double($0.up) }
    }

    var body: some View {
        NavigationLink {
            TrafficView()
        } label: {
            Card {
                VStack(alignment: .leading, spacing: 12) {
                    HStack(alignment: .firstTextBaseline, spacing: 18) {
                        TransferFigure(symbol: "arrow.down", bytes: down, color: TrafficSeries.downloadColor)
                        TransferFigure(symbol: "arrow.up", bytes: up, color: TrafficSeries.uploadColor)
                        Spacer(minLength: 4)
                        Text("Last hour")
                            .font(.footnote.weight(.medium))
                            .foregroundStyle(.secondary)
                    }
                    Sparkline(values: series)
                        .frame(height: 40)
                        .frame(maxWidth: .infinity)
                }
            }
        }
        .buttonStyle(.plain)
    }
}

/// An arrow in the series colour and the amount moved that way.
struct TransferFigure: View {
    let symbol: String
    let bytes: UInt64
    let color: Color
    var font: Font = Font.title3

    private var parts: (value: String, unit: String) { byteParts(bytes) }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 4) {
            Image(systemName: symbol)
                .font(.caption.weight(.bold))
                .foregroundStyle(color)
            Figure(value: parts.value, unit: parts.unit, font: font)
        }
    }
}

private struct SpeedTile: View {
    var body: some View {
        NavigationLink {
            SpeedTestView()
        } label: {
            Card {
                VStack(alignment: .leading, spacing: 10) {
                    Glyph(symbol: "speedometer")
                    Text("Speed test")
                        .font(.subheadline.weight(.medium))
                }
            }
        }
        .buttonStyle(.plain)
    }
}

private struct BlockedTile: View {
    let adblock: AdblockStatus

    var body: some View {
        NavigationLink {
            AdblockPage()
        } label: {
            Card {
                VStack(alignment: .leading, spacing: 10) {
                    Glyph(symbol: "hand.raised.fill", color: adblock.enabled ? Color.accentColor : Color.gray)
                    if adblock.enabled {
                        Figure(value: adblock.blocked.formatted(), unit: "blocked", font: Font.subheadline)
                    } else {
                        Text("Ad blocking off")
                            .font(.subheadline.weight(.medium))
                            .foregroundStyle(.secondary)
                    }
                }
            }
        }
        .buttonStyle(.plain)
    }
}

// MARK: - VPN

private struct VPNCard: View {
    @EnvironmentObject private var model: AppModel
    let vpns: [VPNStatus]

    var body: some View {
        Card {
            VStack(spacing: 12) {
                ForEach(vpns) { vpn in
                    Toggle(isOn: model.vpnEnabled(vpn.id)) {
                        HStack(spacing: 12) {
                            Glyph(symbol: "lock.shield.fill", color: vpnStateColor(vpn.state), filled: vpn.state == "up")
                            VStack(alignment: .leading, spacing: 1) {
                                Text(vpn.name)
                                    .font(.headline)
                                Text(vpnStateLabel(vpn))
                                    .font(.footnote)
                                    .foregroundStyle(.secondary)
                            }
                        }
                    }
                }
            }
        }
    }
}

// MARK: - Activity

private struct ActivityCard: View {
    @EnvironmentObject private var model: AppModel

    private var recent: [ActivityEvent] {
        Array(model.activity.suffix(4).reversed())
    }

    var body: some View {
        if !recent.isEmpty {
            NavigationLink {
                ActivityPage()
            } label: {
                Card {
                    VStack(alignment: .leading, spacing: 10) {
                        ForEach(Array(recent.enumerated()), id: \.offset) { item in
                            ActivityLine(event: item.element)
                        }
                    }
                }
            }
            .buttonStyle(.plain)
        }
    }
}

/// What happened on the left, when on the right.
struct ActivityLine: View {
    @EnvironmentObject private var model: AppModel
    let event: ActivityEvent

    private var text: String {
        if let mac = event.mac, !mac.isEmpty {
            return model.deviceName(mac) + " · " + event.msg
        }
        return event.msg
    }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(text)
                .font(.subheadline)
                .lineLimit(2)
            Spacer(minLength: 4)
            Text(relativeTime(event.t))
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .layoutPriority(1)
        }
    }
}

private struct RouterFootnote: View {
    let status: RouterStatus

    private var text: String {
        var parts: [String] = [status.version, "up " + formatDuration(status.uptime)]
        if status.temp > 0 {
            parts.append(String(format: "%.0f °C", status.temp))
        }
        return parts.joined(separator: " · ")
    }

    var body: some View {
        Text(text)
            .font(.caption)
            .foregroundStyle(.tertiary)
            .padding(.top, 8)
    }
}
