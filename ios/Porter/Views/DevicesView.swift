import PorterKit
import SwiftUI

/// Everything that has joined the router's network.
struct DevicesView: View {
    @EnvironmentObject private var model: AppModel

    private var online: [DeviceInfo] {
        model.devices
            .filter { $0.online }
            .sorted { $0.displayName.localizedCaseInsensitiveCompare($1.displayName) == .orderedAscending }
    }

    private var offline: [DeviceInfo] {
        model.devices
            .filter { !$0.online }
            .sorted { $0.lastSeen > $1.lastSeen }
    }

    var body: some View {
        List {
            if model.devices.isEmpty {
                ContentUnavailableView("No devices yet", systemImage: "laptopcomputer.and.iphone")
                    .listRowBackground(Color.clear)
            }
            if !online.isEmpty {
                Section("Online") {
                    ForEach(online) { device in
                        NavigationLink {
                            DeviceDetailView(mac: device.mac)
                        } label: {
                            DeviceRow(device: device)
                        }
                    }
                }
            }
            if !offline.isEmpty {
                Section("Offline") {
                    ForEach(offline) { device in
                        NavigationLink {
                            DeviceDetailView(mac: device.mac)
                        } label: {
                            DeviceRow(device: device)
                        }
                    }
                }
            }
        }
        .navigationTitle("Devices")
        .refreshable { _ = await reload() }
        .task { await poll() }
    }

    @discardableResult
    private func reload() async -> Bool {
        await model.attempt { _ in try await model.refreshDevices() }
    }

    /// Refreshes every five seconds until the view goes away. Stops after
    /// a failure so that one lost link does not produce a stream of alerts.
    private func poll() async {
        while !Task.isCancelled {
            let ok = await reload()
            if !ok { return }
            do {
                try await Task.sleep(nanoseconds: 5_000_000_000)
            } catch {
                return
            }
        }
    }
}

private struct DeviceRow: View {
    @EnvironmentObject private var model: AppModel
    let device: DeviceInfo

    private var symbol: String {
        if device.paused { return "pause.fill" }
        return device.link == "wired" ? "cable.connector" : "wifi"
    }

    private var color: Color {
        if device.paused { return Color.orange }
        return device.online ? Color.accentColor : Color.gray
    }

    /// Online: the address. Offline: when it was last here.
    private var subtitle: String {
        if !device.online { return relativeTime(device.lastSeen) }
        if let address = device.ipv4, !address.isEmpty { return address }
        return device.mac.uppercased()
    }

    var body: some View {
        HStack(spacing: 12) {
            Glyph(symbol: symbol, color: color)
            VStack(alignment: .leading, spacing: 2) {
                Text(device.displayName)
                    .lineLimit(1)
                HStack(spacing: 6) {
                    Text(subtitle)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    if !device.route.isEmpty {
                        Badge(text: model.actionLabel(device.route), color: Color.blue)
                    }
                    if device.portal == true {
                        Badge(text: "Sign-in", color: Color.purple)
                    }
                }
            }
            Spacer(minLength: 8)
            VStack(alignment: .trailing, spacing: 4) {
                Text(formatBytes(device.up + device.down))
                    .font(.subheadline)
                    .monospacedDigit()
                    .foregroundStyle(.secondary)
                if device.online, device.link != "wired", let signal = device.signal {
                    SignalBars(rssi: signal)
                }
            }
        }
        .opacity(device.online ? 1 : 0.6)
    }
}

struct DeviceDetailView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let mac: String

    @State private var name: String = ""
    @State private var reservedIP: String = ""
    @State private var original: DeviceSettings? = nil
    @State private var queries: [DNSQuery] = []
    @State private var loadingQueries = false
    @State private var usage: [TrafficSample] = []
    @State private var usageLoaded = false

    private var device: DeviceInfo? { model.device(mac) }

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }
    private var cleanIP: String { reservedIP.trimmingCharacters(in: .whitespaces) }

    private var dirty: Bool {
        guard let original else { return false }
        return cleanName != original.name || cleanIP != (original.ip ?? "")
    }

    private var defaultRouteLabel: String {
        "Default (" + model.actionLabel(model.config?.policy.`default` ?? "direct") + ")"
    }

    var body: some View {
        Form {
            if let device {
                settingsSection(device)
                DeviceFactsSection(device: device)
                usageSection
                lookupsSection
                if !device.online {
                    Section {
                        ConfirmedButton(
                            title: "Forget device",
                            message: "Its name, settings and history are removed.",
                            confirmTitle: "Forget"
                        ) {
                            forget()
                        }
                    }
                }
            } else {
                ContentUnavailableView("Device forgotten", systemImage: "questionmark.circle")
            }
        }
        .navigationTitle(device?.displayName ?? "Device")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
                    .disabled(!dirty)
            }
        }
        .onAppear { sync() }
        .onChange(of: device?.settings) { _, _ in sync() }
        .task { await loadQueries() }
        .task { await loadUsage() }
    }

    private func settingsSection(_ device: DeviceInfo) -> some View {
        Section {
            TextField(device.hostname ?? "Name", text: $name)
            Toggle("Pause", isOn: pausedBinding)
            if model.config?.dns.adblock.enabled == true {
                Toggle("Skip ad blocking", isOn: noAdblockBinding)
            }
            Picker("Route", selection: routeBinding) {
                Text(defaultRouteLabel).tag("")
                ForEach(model.availableActions, id: \.self) { action in
                    Text(model.actionLabel(action.raw)).tag(action.raw)
                }
            }
            LabeledContent("Fixed address") {
                TextField(device.ipv4 ?? "Automatic", text: $reservedIP)
                    .multilineTextAlignment(.trailing)
                    .keyboardType(.numbersAndPunctuation)
                    .autocorrectionDisabled()
            }
        }
    }

    private var usageSection: some View {
        Section {
            if usageLoaded {
                TrafficChart(samples: usage, unit: Calendar.Component.minute, height: 140)
            } else {
                ProgressView()
            }
            NavigationLink("Connections") {
                FlowsView(mac: mac)
            }
        } header: {
            Text("Last hour")
        }
    }

    private var lookupsSection: some View {
        Section {
            if loadingQueries {
                ProgressView()
            } else if queries.isEmpty {
                Text("None")
                    .foregroundStyle(.secondary)
            }
            ForEach(Array(queries.enumerated()), id: \.offset) { item in
                QueryRow(query: item.element)
            }
        } header: {
            HStack {
                Text("Lookups")
                Spacer()
                Button {
                    Task { await loadQueries() }
                } label: {
                    Image(systemName: "arrow.clockwise")
                }
                .disabled(loadingQueries)
            }
        }
    }

    private var pausedBinding: Binding<Bool> {
        Binding(
            get: { device?.paused ?? false },
            set: { value in applyNow { $0.paused = value } }
        )
    }

    private var noAdblockBinding: Binding<Bool> {
        Binding(
            get: { device?.noAdblock ?? false },
            set: { value in applyNow { $0.noAdblock = value } }
        )
    }

    private var routeBinding: Binding<String> {
        Binding(
            get: { device?.route ?? "" },
            set: { value in applyNow { $0.route = value } }
        )
    }

    /// Saves a change to the stored settings, leaving unsaved text edits alone.
    private func applyNow(_ change: (inout DeviceSettings) -> Void) {
        guard var settings = device?.settings else { return }
        change(&settings)
        let updated = settings
        Task { await model.setDevice(updated) }
    }

    private func sync() {
        guard let current = device?.settings else { return }
        if original == nil || !dirty {
            name = current.name
            reservedIP = current.ip ?? ""
        }
        original = current
    }

    private func save() {
        guard let current = device?.settings else { return }
        let newName = cleanName
        let newIP = cleanIP
        name = newName
        reservedIP = newIP
        let settings = DeviceSettings(
            mac: current.mac,
            name: newName,
            route: current.route,
            paused: current.paused,
            ip: newIP.isEmpty ? nil : newIP,
            noAdblock: current.noAdblock
        )
        Task { await model.setDevice(settings) }
    }

    private func loadQueries() async {
        let target = mac
        loadingQueries = true
        let found: [DNSQuery]? = await model.fetch { try await $0.queries(mac: target) }
        loadingQueries = false
        if let found {
            queries = Array(found.suffix(100).reversed())
        }
    }

    private func loadUsage() async {
        let target = mac
        let found: TrafficStats? = await model.fetch { try await $0.trafficStats(mac: target) }
        usageLoaded = true
        if let found {
            usage = found.minutes
        }
    }

    private func forget() {
        let target = mac
        Task {
            let ok = await model.attempt { connection in
                try await connection.forgetDevice(mac: target)
                try await model.refreshDevices()
            }
            if ok {
                dismiss()
            }
        }
    }
}

private struct DeviceFactsSection: View {
    let device: DeviceInfo

    private var addresses6: [String] { device.ipv6 ?? [] }

    private var seen: String {
        device.online ? "Now" : relativeTime(device.lastSeen)
    }

    var body: some View {
        Section {
            HStack(spacing: 24) {
                TransferFigure(symbol: "arrow.down", bytes: device.down, color: TrafficSeries.downloadColor)
                TransferFigure(symbol: "arrow.up", bytes: device.up, color: TrafficSeries.uploadColor)
                Spacer()
            }
            .padding(.vertical, 4)
            LabeledContent("MAC", value: device.mac.uppercased())
            if let address = device.ipv4, !address.isEmpty {
                LabeledContent("IPv4", value: address)
            }
            ForEach(addresses6, id: \.self) { address in
                LabeledContent("IPv6", value: address)
            }
            LabeledContent("Seen", value: seen)
            LabeledContent("Since", value: formatDate(device.firstSeen))
        }
    }
}

private struct QueryRow: View {
    @EnvironmentObject private var model: AppModel
    let query: DNSQuery

    private var pathLabel: String {
        switch query.path {
        case "direct", "wan": return "Direct"
        case "block": return "Blocked"
        case "local": return "Router"
        case "refuse": return "Refused"
        default:
            if let action = RouteAction(query.path), case .vpn(let id) = action {
                return model.vpnName(id)
            }
            return query.path
        }
    }

    private var color: Color {
        switch query.path {
        case "block", "refuse": return Color.red
        case "direct", "wan", "local": return Color.secondary
        default: return Color.blue
        }
    }

    var body: some View {
        HStack(spacing: 8) {
            VStack(alignment: .leading, spacing: 2) {
                Text(query.name)
                    .font(.footnote.monospaced())
                    .lineLimit(1)
                    .truncationMode(.middle)
                Text(query.type + " · " + relativeTime(query.t))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer(minLength: 4)
            Badge(text: query.failed == true ? "Failed" : pathLabel, color: query.failed == true ? Color.red : color)
                .lineLimit(1)
                .layoutPriority(1)
        }
    }
}
