import PorterKit
import SwiftUI

/// Everything that has joined the router's network: the groups first,
/// then the devices that are in none.
struct DevicesView: View {
    @EnvironmentObject private var model: AppModel
    @State private var addingGroup = false

    /// The devices listed one by one: those that are in no group. The
    /// others are on their group's page.
    private var single: [DeviceInfo] {
        model.devices.filter { model.config?.group(of: $0.mac) == nil }
    }

    private var online: [DeviceInfo] {
        single
            .filter { $0.online }
            .sorted { $0.displayName.localizedCaseInsensitiveCompare($1.displayName) == .orderedAscending }
    }

    private var offline: [DeviceInfo] {
        single
            .filter { !$0.online }
            .sorted { $0.lastSeen > $1.lastSeen }
    }

    var body: some View {
        List {
            if model.devices.isEmpty {
                ContentUnavailableView(
                    "No devices yet",
                    systemImage: "laptopcomputer.and.iphone",
                    description: Text("Devices are listed here once they join your Wi-Fi or are plugged into a wired port.")
                )
                .listRowBackground(Color.clear)
            } else {
                GroupsSection(adding: $addingGroup)
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
        .sheet(isPresented: $addingGroup) {
            NavigationStack {
                NewGroupSheet()
            }
            .modifier(SheetErrorAlert())
            .environmentObject(model)
        }
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

    /// Paused by its own switch, or with its group.
    private var paused: Bool { device.paused || model.held(device.mac) != nil }

    private var symbol: String {
        if paused { return "pause.fill" }
        return device.link == "wired" ? "cable.connector" : "wifi"
    }

    private var color: Color {
        if paused { return Color.orange }
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
                    if model.hasSeveralNetworks, let network = device.network, !network.isEmpty, network != RouterConfig.mainNetwork {
                        Badge(text: model.networkName(network))
                    }
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
    @State private var askingToAllow = false

    private var device: DeviceInfo? { model.device(mac) }

    /// The group the device is in, and what pauses its internet access
    /// right now: its own switch, or its group.
    private var group: DeviceGroup? { model.config?.group(of: mac) }
    private var held: HeldDevice? { model.held(mac) }

    /// The device has been let online while a schedule or a timer has
    /// its group paused, until then.
    private var allowedUntil: Int64? {
        guard let group, let state = model.status?.group(group.id), state.offline, state.why != "paused",
              let until = device?.allowUntil, until > (model.status?.time ?? 0), device?.paused != true else { return nil }
        return until
    }

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }
    private var cleanIP: String { reservedIP.trimmingCharacters(in: .whitespaces) }

    private var dirty: Bool {
        guard let original else { return false }
        return cleanName != original.name || cleanIP != (original.ip ?? "")
    }

    /// The route the device has when it has none of its own: its
    /// group's, else its network's, or else the default.
    private var inheritedRoute: String {
        if let group, !group.route.isEmpty { return group.route }
        if let id = device?.network, let route = model.config?.network(id)?.route, !route.isEmpty {
            return route
        }
        return model.config?.policy.`default` ?? "direct"
    }

    /// Where the device's traffic goes when it has no route of its own.
    private var defaultRouteLabel: String {
        if let group, !group.route.isEmpty {
            return group.name + " (" + model.actionLabel(group.route) + ")"
        }
        return "Default (" + model.actionLabel(inheritedRoute) + ")"
    }

    /// For a device without a route of its own whose inherited route is
    /// through an internet source that is not carrying it right now: why
    /// it is offline, or going out another way.
    private var inheritedRouteNote: String? {
        let own: String = device?.route ?? ""
        return own.isEmpty ? inheritedNote : nil
    }

    private var inheritedNote: String? {
        guard let action = RouteAction(inheritedRoute), model.status?.usesSource(action) == false else { return nil }
        return model.routeNote(action, for: .device)
    }

    /// What the switches do not say by themselves: that the device is
    /// paused with its group and until when, or let through for now, and
    /// that it skips ad blocking because its group does.
    private var groupNote: String {
        guard let group else { return "" }
        var notes: [String] = []
        if let held, held.byGroup {
            var text: String = group.name + " is paused"
            if let until = held.until {
                text += " until " + model.clockText(until)
            }
            if held.why == "schedule" {
                text += " by " + model.scheduleName(group, model.status?.group(group.id)?.schedule)
            }
            notes.append(text + ", and this device with it.")
        } else if let allowedUntil {
            notes.append(group.name + " is paused. This device is allowed online until " + model.clockText(allowedUntil) + ".")
        }
        if group.noAdblock && model.config?.dns.adblock.enabled == true {
            notes.append(group.name + " skips ad blocking, and this device with it.")
        }
        return notes.joined(separator: " ")
    }

    /// What to say when asked to unpause a device that is paused with
    /// its group.
    private var allowMessage: String {
        guard let group, let held else { return "" }
        guard let until = held.until else {
            return group.name + " is paused. To let only this device online, take it out of the group."
        }
        return group.name + " is paused until " + model.clockText(until) + "."
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
                            message: "Its name, settings and traffic history are erased. If it joins again, it is listed as a new device.",
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
        .confirmationDialog("Paused with its group", isPresented: $askingToAllow, titleVisibility: .visible) {
            allowChoices
        } message: {
            Text(allowMessage)
        }
    }

    /// What can be done for a device that is paused with its group: let
    /// it alone online, or the whole group, until the group would be
    /// back anyway; or, where nothing ends the pause, end it.
    @ViewBuilder
    private var allowChoices: some View {
        if let group, let held {
            let groupID: String = group.id
            if let until = held.until {
                let time: String = model.clockText(until)
                Button("Allow this device until \(time)") {
                    applyNow { $0.allowUntil = until }
                }
                Button("Allow all of \(group.name) until \(time)") {
                    editGroup(groupID) { $0.forNow = GroupOverride.allow(until: until) }
                }
            } else {
                Button("Resume all of \(group.name)") {
                    editGroup(groupID) { $0.paused = false }
                }
            }
        }
        Button("Cancel", role: .cancel) {}
    }

    /// The two fields Save is for, then the switches that apply at once,
    /// then the route with what can overrule it.
    @ViewBuilder
    private func settingsSection(_ device: DeviceInfo) -> some View {
        Section {
            TextField(device.hostname ?? "Name", text: $name)
            LabeledContent("Fixed address") {
                TextField(device.ipv4 ?? "Automatic", text: $reservedIP)
                    .multilineTextAlignment(.trailing)
                    .keyboardType(.numbersAndPunctuation)
                    .autocorrectionDisabled()
            }
        }
        Section {
            Toggle("Pause internet access", isOn: pausedBinding)
            if allowedUntil != nil {
                Button("Stop allowing") { applyNow { $0.allowUntil = nil } }
            }
            if model.config?.dns.adblock.enabled == true {
                Toggle("Skip ad blocking", isOn: noAdblockBinding)
                    .disabled(group?.noAdblock == true)
            }
            if !(model.config?.groupList ?? []).isEmpty {
                Picker("Group", selection: groupBinding) {
                    Text("None").tag("")
                    ForEach(model.config?.groupList ?? []) { choice in
                        Text(choice.name).tag(choice.id)
                    }
                }
            }
        } footer: {
            Text(groupNote)
        }
        Section {
            RoutePicker(title: "Route", selection: routeBinding, defaultLabel: defaultRouteLabel)
            RouteSourceRows(selection: routeBinding, subject: .device)
            if let note = inheritedRouteNote {
                Text(note)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
        } footer: {
            Text("Route sets how this device goes online. A rule on the VPN tab that fits its traffic takes priority.")
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
                Text("The names this device looks up are listed here.")
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
                .accessibilityLabel(Text("Reload lookups"))
                .disabled(loadingQueries)
            }
        }
    }

    /// The switch shows whether the device is paused in effect: by
    /// itself or with its group. Switching it off unpauses the device
    /// itself; a device that is paused with its group is asked about.
    private var pausedBinding: Binding<Bool> {
        Binding(
            get: { (device?.paused ?? false) || held?.byGroup == true },
            set: { value in
                if value {
                    applyNow { $0.paused = true }
                } else if device?.paused == true {
                    applyNow { $0.paused = false }
                } else if held?.byGroup == true {
                    askingToAllow = true
                }
            }
        )
    }

    /// Shows what holds in effect: the device skips ad blocking if it or
    /// its group is set to.
    private var noAdblockBinding: Binding<Bool> {
        Binding(
            get: { (device?.noAdblock ?? false) || group?.noAdblock == true },
            set: { value in applyNow { $0.noAdblock = value } }
        )
    }

    private var groupBinding: Binding<String> {
        Binding(
            get: { group?.id ?? "" },
            set: { value in
                let target: String = mac
                Task { await model.update { cfg in cfg.setGroup(of: target, to: value.isEmpty ? nil : value) } }
            }
        )
    }

    /// Applies a change to a group at once.
    private func editGroup(_ id: String, _ change: @escaping (inout DeviceGroup) -> Void) {
        Task { await model.update { cfg in cfg.editGroup(id, change) } }
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
            noAdblock: current.noAdblock,
            allowUntil: current.allowUntil
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
    @EnvironmentObject private var model: AppModel
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
            if model.hasSeveralNetworks, let network = device.network, !network.isEmpty {
                LabeledContent("Network", value: model.networkName(network))
            }
            if let address = device.ipv4, !address.isEmpty {
                LabeledContent("IPv4", value: address)
            }
            ForEach(addresses6, id: \.self) { address in
                LabeledContent("IPv6", value: address)
            }
            LabeledContent("Last seen", value: seen)
            LabeledContent("First seen", value: formatDate(device.firstSeen))
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
            // A VPN by its name, a lookup the block lists stopped as
            // "Ad blocked".
            return model.actionLabel(query.path)
        }
    }

    private var color: Color {
        switch query.path {
        case "block", "refuse", "adblock": return Color.red
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
