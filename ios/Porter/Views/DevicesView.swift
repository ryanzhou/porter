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
                Text("No device has joined the router yet.")
                    .foregroundStyle(.secondary)
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
        .refreshable { await reload() }
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

    private var subtitle: String {
        var parts: [String] = []
        if let address = device.ipv4, !address.isEmpty { parts.append(address) }
        parts.append(formatBytes(device.up + device.down))
        if !device.online { parts.append("seen " + relativeTime(device.lastSeen)) }
        return parts.joined(separator: " · ")
    }

    private var hasBadges: Bool {
        device.paused || !device.route.isEmpty || device.portal == true
    }

    var body: some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 3) {
                Text(device.displayName)
                Text(subtitle)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if hasBadges {
                    badges
                }
            }
            Spacer()
            if device.online {
                linkIndicator
            }
        }
    }

    private var badges: some View {
        HStack(spacing: 6) {
            if device.paused {
                Badge(text: "Paused", color: Color.orange)
            }
            if !device.route.isEmpty {
                Badge(text: model.actionLabel(device.route), color: Color.blue)
            }
            if device.portal == true {
                Badge(text: "Sign-in device", color: Color.purple)
            }
        }
    }

    @ViewBuilder
    private var linkIndicator: some View {
        if device.link == "wired" {
            Text("Wired")
                .font(.caption)
                .foregroundStyle(.secondary)
        } else if let signal = device.signal {
            SignalBars(rssi: signal)
        }
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
                lookupsSection
                if !device.online {
                    Section {
                        ConfirmedButton(
                            title: "Forget device",
                            message: "Removes this device's name, settings and history. It can join again like any new device.",
                            confirmTitle: "Forget"
                        ) {
                            forget()
                        }
                    }
                }
            } else {
                Text("The router no longer knows this device.")
                    .foregroundStyle(.secondary)
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
    }

    private func settingsSection(_ device: DeviceInfo) -> some View {
        Section {
            TextField(device.hostname ?? "Name", text: $name)
            Toggle("Pause internet", isOn: pausedBinding)
            Picker("Route", selection: routeBinding) {
                Text(defaultRouteLabel).tag("")
                ForEach(model.availableActions, id: \.self) { action in
                    Text(model.actionLabel(action.raw)).tag(action.raw)
                }
            }
            TextField("Reserved IPv4 address (optional)", text: $reservedIP)
                .keyboardType(.numbersAndPunctuation)
                .autocorrectionDisabled()
        } header: {
            Text("Settings")
        } footer: {
            Text("The route overrides the default for this device; rules on the VPN tab still come first. A reserved address has to be inside the router's local network.")
        }
    }

    private var lookupsSection: some View {
        Section {
            if loadingQueries {
                ProgressView()
            } else if queries.isEmpty {
                Text("None recorded.")
                    .foregroundStyle(.secondary)
            }
            ForEach(Array(queries.enumerated()), id: \.offset) { item in
                QueryRow(query: item.element)
            }
            Button("Reload") {
                Task { await loadQueries() }
            }
            .disabled(loadingQueries)
        } header: {
            Text("Recent lookups")
        } footer: {
            Text("Names this device asked the router to resolve, newest first, and where each question was sent.")
        }
    }

    private var pausedBinding: Binding<Bool> {
        Binding(
            get: { device?.paused ?? false },
            set: { value in
                guard var settings = device?.settings else { return }
                settings.paused = value
                Task { await model.setDevice(settings) }
            }
        )
    }

    private var routeBinding: Binding<String> {
        Binding(
            get: { device?.route ?? "" },
            set: { value in
                guard var settings = device?.settings else { return }
                settings.route = value
                Task { await model.setDevice(settings) }
            }
        )
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
            ip: newIP.isEmpty ? nil : newIP
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

    var body: some View {
        Section("Details") {
            LabeledContent("MAC address", value: device.mac.uppercased())
            LabeledContent("IPv4", value: device.ipv4 ?? "None")
            ForEach(addresses6, id: \.self) { address in
                LabeledContent("IPv6", value: address)
            }
            LabeledContent("First seen", value: formatDate(device.firstSeen))
            LabeledContent("Last seen", value: device.online ? "Now" : formatDate(device.lastSeen))
            LabeledContent("Sent", value: formatBytes(device.up))
            LabeledContent("Received", value: formatBytes(device.down))
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
        case "local": return "Answered by the router"
        case "refuse": return "Refused"
        default:
            if let action = RouteAction(query.path), case .vpn(let id) = action {
                return model.vpnName(id)
            }
            return query.path
        }
    }

    private var detail: String {
        query.type + " · " + pathLabel + " · " + relativeTime(query.t)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(query.name)
                .font(.footnote.monospaced())
                .lineLimit(1)
            HStack(spacing: 6) {
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if query.failed == true {
                    Text("Failed")
                        .font(.caption)
                        .foregroundStyle(.red)
                }
            }
        }
    }
}
