import PorterKit
import SwiftUI
import UIKit
import UniformTypeIdentifiers

/// VPN profiles and the rules that decide which traffic uses them.
struct VPNView: View {
    @EnvironmentObject private var model: AppModel
    @State private var adding = false

    var body: some View {
        List {
            VPNProfilesSection(adding: $adding)
            RulesSection()
        }
        .navigationTitle("VPN")
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                EditButton()
            }
        }
        .refreshable { await model.refresh() }
        .sheet(isPresented: $adding) {
            NavigationStack {
                AddVPNSheet()
            }
            .modifier(SheetErrorAlert())
            .environmentObject(model)
        }
    }
}

// MARK: - Profiles

private struct VPNProfilesSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var adding: Bool

    private var profiles: [VPNProfile] { model.config?.vpns ?? [] }

    private func status(_ id: String) -> VPNStatus? {
        model.status?.vpns.first(where: { $0.id == id })
    }

    var body: some View {
        Section {
            ForEach(profiles) { profile in
                NavigationLink {
                    VPNDetailView(profileID: profile.id)
                } label: {
                    VPNProfileRow(profile: profile, status: status(profile.id))
                }
            }
            Button {
                adding = true
            } label: {
                Label("Add VPN", systemImage: "plus")
            }
        }
    }
}

private struct VPNProfileRow: View {
    @EnvironmentObject private var model: AppModel
    let profile: VPNProfile
    let status: VPNStatus?

    private var state: String { status?.state ?? "off" }

    /// The state, and once the tunnel is up what it has carried.
    private var detail: String {
        guard let status else { return "Off" }
        var parts: [String] = [vpnStateLabel(status)]
        if status.isMPQUIC, let total = status.of, total > 0 {
            parts.append("\(status.paths ?? 0)/\(total) paths")
        }
        if status.state == "up" {
            parts.append("↓ " + formatBytes(UInt64(clamping: status.rx)))
            parts.append("↑ " + formatBytes(UInt64(clamping: status.tx)))
        }
        return parts.joined(separator: "  ")
    }

    var body: some View {
        HStack(spacing: 12) {
            Glyph(symbol: "lock.shield.fill", color: vpnStateColor(state), filled: state == "up")
            VStack(alignment: .leading, spacing: 2) {
                Text(profile.name)
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
            }
            Spacer()
            Toggle("Enabled", isOn: model.vpnEnabled(profile.id))
                .labelsHidden()
        }
    }
}

struct VPNDetailView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let profileID: String

    @State private var name: String = ""
    @State private var loaded = false

    private var profile: VPNProfile? {
        model.config?.vpns.first(where: { $0.id == profileID })
    }

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }

    private var canSave: Bool {
        guard let profile else { return false }
        return !cleanName.isEmpty && cleanName != profile.name
    }

    private var killSwitchBinding: Binding<Bool> {
        let id = profileID
        return Binding(
            get: { profile?.killSwitch ?? true },
            set: { value in
                Task {
                    await model.update { cfg in
                        if let index = cfg.vpns.firstIndex(where: { $0.id == id }) {
                            cfg.vpns[index].killSwitch = value
                        }
                    }
                }
            }
        )
    }

    var body: some View {
        Form {
            if let profile {
                Section("Name") {
                    TextField("Name", text: $name)
                }
                Section {
                    Toggle("Enabled", isOn: model.vpnEnabled(profileID))
                    Toggle("Kill switch", isOn: killSwitchBinding)
                } footer: {
                    Text("The kill switch stops this VPN's traffic while the tunnel is down.")
                }
                VPNLiveSection(status: model.status?.vpns.first(where: { $0.id == profileID }))
                if profile.isMPQUIC {
                    MPQUICSummarySection(profileID: profileID, mpquic: profile.mpquic ?? MPQUICConfig())
                } else {
                    WireGuardSummarySection(wireguard: profile.wireguard)
                }
                Section {
                    ConfirmedButton(
                        title: "Delete VPN",
                        message: "Whatever used it goes direct.",
                        confirmTitle: "Delete"
                    ) {
                        delete()
                    }
                }
            }
        }
        .navigationTitle(profile?.name ?? "VPN")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
                    .disabled(!canSave)
            }
        }
        .onAppear { load() }
    }

    private func load() {
        guard !loaded, let profile else { return }
        loaded = true
        name = profile.name
    }

    private func save() {
        let id = profileID
        let newName = cleanName
        name = newName
        Task {
            await model.update { cfg in
                if let index = cfg.vpns.firstIndex(where: { $0.id == id }) {
                    cfg.vpns[index].name = newName
                }
            }
        }
    }

    /// Removes the profile and everything that points at it; the router
    /// rejects a configuration that refers to a missing VPN.
    private func delete() {
        let id = profileID
        let action = RouteAction.vpn(id).raw
        Task {
            let ok = await model.update { cfg in
                cfg.vpns.removeAll(where: { $0.id == id })
                if cfg.policy.`default` == action {
                    cfg.policy.`default` = "direct"
                }
                for index in cfg.devices.indices where cfg.devices[index].route == action {
                    cfg.devices[index].route = ""
                }
                for index in cfg.policy.rules.indices where cfg.policy.rules[index].action == action {
                    cfg.policy.rules[index].action = "direct"
                }
            }
            if ok {
                dismiss()
            }
        }
    }
}

/// What the tunnel is doing now; shown only while it is doing something.
private struct VPNLiveSection: View {
    let status: VPNStatus?

    var body: some View {
        if let status, status.state == "up" || status.state == "connecting" {
            Section {
                HStack(spacing: 24) {
                    TransferFigure(symbol: "arrow.down", bytes: UInt64(clamping: status.rx), color: TrafficSeries.downloadColor)
                    TransferFigure(symbol: "arrow.up", bytes: UInt64(clamping: status.tx), color: TrafficSeries.uploadColor)
                    Spacer()
                }
                .padding(.vertical, 4)
                if status.isMPQUIC {
                    LabeledContent("Paths", value: "\(status.paths ?? 0) of \(status.of ?? 0)")
                } else {
                    if let endpoint = status.endpoint, !endpoint.isEmpty {
                        LabeledContent("Endpoint", value: endpoint)
                    }
                    if let handshake = status.handshake, handshake > 0 {
                        LabeledContent("Handshake", value: relativeTime(handshake))
                    }
                }
            }
        }
    }
}

private struct WireGuardSummarySection: View {
    let wireguard: WireGuardConfig

    private func list(_ values: [String]) -> String {
        values.isEmpty ? "None" : values.joined(separator: ", ")
    }

    var body: some View {
        Section("WireGuard") {
            LabeledContent("Addresses", value: list(wireguard.addresses))
            LabeledContent("DNS", value: list(wireguard.dns))
            ForEach(Array(wireguard.peers.enumerated()), id: \.offset) { item in
                LabeledContent("Peer", value: item.element.endpoint.isEmpty ? "None" : item.element.endpoint)
                LabeledContent("Allowed IPs", value: list(item.element.allowedIps))
            }
        }
    }
}

/// What an MPQUIC profile is made of, as form sections. Used when adding
/// one and when changing one.
private struct MPQUICFields: View {
    @Binding var config: MPQUICConfig

    var body: some View {
        StringListEditor(
            title: "Servers",
            items: $config.servers,
            placeholder: "server.example.com:8388",
            keyboard: .URL,
            footer: "The server, then any relays. One path each."
        )
        Section("Password") {
            TextField("Password", text: $config.password)
                .font(.body.monospaced())
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        }
        Section {
            RateField(label: "Download", text: $config.down, example: "95mbit")
            RateField(label: "Upload", text: $config.up, example: "19mbit")
        } header: {
            Text("This line")
        } footer: {
            Text("A little under the real rates. Empty measures them.")
        }
        Section {
            Toggle("Reliable UDP", isOn: $config.reliableUdp)
        }
    }
}

private struct RateField: View {
    let label: String
    @Binding var text: String
    let example: String

    var body: some View {
        LabeledContent(label) {
            TextField(label, text: $text, prompt: Text(verbatim: example))
                .multilineTextAlignment(.trailing)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        }
    }
}

private func usable(_ config: MPQUICConfig) -> Bool {
    !config.servers.isEmpty && config.password.count >= 8
}

/// An MPQUIC profile's settings, with a way to change them.
private struct MPQUICSummarySection: View {
    let profileID: String
    let mpquic: MPQUICConfig

    var body: some View {
        Section("MPQUIC") {
            ForEach(Array(mpquic.servers.enumerated()), id: \.offset) { item in
                LabeledContent(item.offset == 0 ? "Server" : "Relay", value: item.element)
            }
            if !mpquic.down.isEmpty || !mpquic.up.isEmpty {
                LabeledContent("Line", value: "↓ " + (mpquic.down.isEmpty ? "–" : mpquic.down) + "  ↑ " + (mpquic.up.isEmpty ? "–" : mpquic.up))
            }
            NavigationLink("Change") {
                MPQUICEditPage(profileID: profileID, initial: mpquic)
            }
        }
    }
}

private struct MPQUICEditPage: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let profileID: String
    let initial: MPQUICConfig

    @State private var draft = MPQUICConfig()
    @State private var loaded = false
    @State private var saving = false

    var body: some View {
        Form {
            MPQUICFields(config: $draft)
        }
        .navigationTitle("MPQUIC")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
                    .disabled(!loaded || saving || draft == initial || !usable(draft))
            }
        }
        .onAppear {
            if !loaded {
                loaded = true
                draft = initial
            }
        }
    }

    private func save() {
        let id = profileID
        let next = draft
        saving = true
        Task {
            let ok = await model.update { cfg in
                if let index = cfg.vpns.firstIndex(where: { $0.id == id }) {
                    cfg.vpns[index].mpquic = next
                }
            }
            saving = false
            if ok {
                dismiss()
            }
        }
    }
}

private struct AddVPNSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var kind: String = "wireguard"
    @State private var name: String = ""
    @State private var conf: String = ""
    @State private var mpquic = MPQUICConfig()
    @State private var importing = false
    @State private var importProblem: String = ""
    @State private var working = false

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }

    private var canAdd: Bool {
        guard !working, !cleanName.isEmpty else { return false }
        if kind == "mpquic" { return usable(mpquic) }
        return !conf.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    var body: some View {
        Form {
            Section {
                Picker("Kind", selection: $kind) {
                    Text("WireGuard").tag("wireguard")
                    Text("MPQUIC").tag("mpquic")
                }
                .pickerStyle(.segmented)
                .listRowInsets(EdgeInsets())
                .listRowBackground(Color.clear)
            }
            Section("Name") {
                TextField("Name", text: $name)
            }
            if kind == "mpquic" {
                MPQUICFields(config: $mpquic)
            } else {
                wireguardSection
            }
        }
        .navigationTitle("Add VPN")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel") { dismiss() }
            }
            ToolbarItem(placement: .confirmationAction) {
                Button("Add") { add() }
                    .disabled(!canAdd)
            }
        }
        .fileImporter(
            isPresented: $importing,
            allowedContentTypes: [UTType.data, UTType.text],
            onCompletion: { result in handleImport(result) }
        )
    }

    private var wireguardSection: some View {
            Section {
                TextEditor(text: $conf)
                    .font(.footnote.monospaced())
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .frame(minHeight: 180)
                HStack(spacing: 12) {
                    Button {
                        conf = UIPasteboard.general.string ?? conf
                    } label: {
                        Label("Paste", systemImage: "doc.on.clipboard")
                            .frame(maxWidth: .infinity)
                    }
                    Button {
                        importing = true
                    } label: {
                        Label("Import", systemImage: "folder")
                            .frame(maxWidth: .infinity)
                    }
                }
                .buttonStyle(.bordered)
                if !importProblem.isEmpty {
                    Text(importProblem)
                        .font(.footnote)
                        .foregroundStyle(.red)
                }
            } header: {
                Text("WireGuard .conf")
            }
    }

    private func handleImport(_ result: Result<URL, Error>) {
        switch result {
        case .success(let url):
            let scoped = url.startAccessingSecurityScopedResource()
            defer {
                if scoped { url.stopAccessingSecurityScopedResource() }
            }
            if let text = try? String(contentsOf: url, encoding: .utf8) {
                conf = text
                importProblem = ""
                if cleanName.isEmpty {
                    name = url.deletingPathExtension().lastPathComponent
                }
            } else {
                importProblem = "That file could not be read as text."
            }
        case .failure:
            importProblem = "The file could not be opened."
        }
    }

    private func add() {
        let text = conf
        let profileName = cleanName
        working = true
        if kind == "mpquic" {
            let profile = VPNProfile(id: AppModel.newID(), name: profileName, enabled: true, killSwitch: true, mpquic: mpquic)
            Task {
                let ok = await model.update { cfg in
                    cfg.vpns.append(profile)
                }
                working = false
                if ok {
                    dismiss()
                }
            }
            return
        }
        Task {
            let parsed: WireGuardConfig? = await model.fetch { try await $0.parseWireGuard(conf: text) }
            guard let wireguard = parsed else {
                working = false
                return
            }
            let profile = VPNProfile(id: AppModel.newID(), name: profileName, enabled: true, killSwitch: true, wireguard: wireguard)
            let ok = await model.update { cfg in
                cfg.vpns.append(profile)
            }
            working = false
            if ok {
                dismiss()
            }
        }
    }
}

// MARK: - Rules

/// The rules as a table read from the top: numbered in the order they are
/// tried, each ending in where its traffic goes, and below them the row
/// for everything no rule matched.
private struct RulesSection: View {
    @EnvironmentObject private var model: AppModel
    /// The order just chosen by dragging, shown until the router confirms it.
    @State private var pendingOrder: [PolicyRule]? = nil

    private var rules: [PolicyRule] {
        pendingOrder ?? model.config?.policy.rules ?? []
    }

    var body: some View {
        Section {
            ForEach(Array(rules.enumerated()), id: \.element.id) { item in
                NavigationLink {
                    RuleEditorView(ruleID: item.element.id)
                } label: {
                    RuleRow(rule: item.element, number: item.offset + 1)
                }
                .swipeActions(edge: .leading) {
                    Button(item.element.enabled ? "Turn off" : "Turn on") {
                        setEnabled(item.element.id, !item.element.enabled)
                    }
                    .tint(item.element.enabled ? Color.gray : Color.green)
                }
            }
            .onMove { source, destination in
                move(from: source, to: destination)
            }
            .onDelete { offsets in
                remove(at: offsets)
            }
            HStack(spacing: 12) {
                RuleNumber(text: "∗")
                Text("Everything else")
                Spacer()
                Picker("Everything else", selection: model.binding(\.policy.`default`, fallback: "direct")) {
                    ForEach(model.availableActions, id: \.self) { action in
                        Text(model.actionLabel(action.raw)).tag(action.raw)
                    }
                }
                .labelsHidden()
            }
            Button {
                addRule()
            } label: {
                Label("Add rule", systemImage: "plus")
            }
        } header: {
            Text("Rules")
        } footer: {
            Text("First match wins.")
        }
    }

    private func setEnabled(_ id: String, _ value: Bool) {
        Task {
            await model.update { cfg in
                if let index = cfg.policy.rules.firstIndex(where: { $0.id == id }) {
                    cfg.policy.rules[index].enabled = value
                }
            }
        }
    }

    private func move(from source: IndexSet, to destination: Int) {
        var reordered = rules
        reordered.move(fromOffsets: source, toOffset: destination)
        let order: [String] = reordered.map { $0.id }
        pendingOrder = reordered
        Task {
            await model.update { cfg in
                var next: [PolicyRule] = []
                for id in order {
                    if let rule = cfg.policy.rules.first(where: { $0.id == id }) {
                        next.append(rule)
                    }
                }
                for rule in cfg.policy.rules where !order.contains(rule.id) {
                    next.append(rule)
                }
                cfg.policy.rules = next
            }
            pendingOrder = nil
        }
    }

    private func remove(at offsets: IndexSet) {
        let current = rules
        let ids: [String] = offsets.map { current[$0].id }
        Task {
            await model.update { cfg in
                cfg.policy.rules.removeAll(where: { ids.contains($0.id) })
            }
        }
    }

    private func addRule() {
        let rule = PolicyRule(id: AppModel.newID(), name: "New rule")
        Task {
            await model.update { cfg in
                cfg.policy.rules.append(rule)
            }
        }
    }
}

/// A rule's place in the order.
private struct RuleNumber: View {
    let text: String

    var body: some View {
        Text(text)
            .font(.caption.weight(.bold))
            .monospacedDigit()
            .foregroundStyle(.secondary)
            .frame(width: 24, height: 24)
            .background(Color.secondary.opacity(0.14), in: Circle())
    }
}

/// Where traffic goes, coloured by kind: grey direct, blue VPN, red blocked.
struct ActionBadge: View {
    @EnvironmentObject private var model: AppModel
    let action: String

    private var color: Color {
        guard let parsed = RouteAction(action) else { return Color.secondary }
        switch parsed {
        case .direct: return Color.secondary
        case .block: return Color.red
        case .vpn: return Color.blue
        }
    }

    var body: some View {
        Badge(text: model.actionLabel(action), color: color)
            .lineLimit(1)
            .layoutPriority(1)
    }
}

private struct RuleRow: View {
    @EnvironmentObject private var model: AppModel
    let rule: PolicyRule
    let number: Int

    /// Who: "All", one device's name, or the first of several.
    private var source: String {
        var names: [String] = rule.src.networks.map { model.networkName($0) }
        names.append(contentsOf: rule.src.devices.map { model.deviceName($0) })
        names.append(contentsOf: rule.src.cidrs)
        guard let first = names.first else { return "All" }
        return names.count == 1 ? first : first + " +\(names.count - 1)"
    }

    /// Where to: flags for countries, then the first name or range, then
    /// protocol and ports.
    private var destination: String {
        var parts: [String] = []
        let flags: String = rule.dst.countries.prefix(4).map { countryFlag($0) }.joined()
        if !flags.isEmpty {
            parts.append(rule.dst.countries.count > 4 ? flags + "…" : flags)
        }
        let named: [String] = rule.dst.domains + rule.dst.cidrs
        if let first = named.first {
            parts.append(named.count == 1 ? first : first + " +\(named.count - 1)")
        }
        var service: String = rule.dst.proto.uppercased()
        if !rule.dst.ports.isEmpty {
            service += ":" + rule.dst.ports.joined(separator: ",")
        }
        if !service.isEmpty {
            parts.append(service)
        }
        return parts.isEmpty ? "Anywhere" : parts.joined(separator: " ")
    }

    var body: some View {
        HStack(spacing: 12) {
            RuleNumber(text: String(number))
            VStack(alignment: .leading, spacing: 2) {
                Text(rule.name.isEmpty ? "Unnamed" : rule.name)
                    .lineLimit(1)
                HStack(spacing: 4) {
                    Text(source)
                    Image(systemName: "arrow.right")
                        .font(.caption2.weight(.semibold))
                    Text(destination)
                }
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(1)
            }
            Spacer(minLength: 4)
            if rule.enabled {
                ActionBadge(action: rule.action)
            } else {
                Badge(text: "Off")
            }
        }
        .opacity(rule.enabled ? 1 : 0.5)
    }
}

// MARK: - Rule editor

struct RuleEditorView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let ruleID: String

    @State private var draft = PolicyRule(id: "", name: "")
    @State private var original: PolicyRule? = nil

    private var stored: PolicyRule? {
        model.config?.policy.rules.first(where: { $0.id == ruleID })
    }

    private var canSave: Bool {
        guard let original else { return false }
        return draft != original
    }

    private var deviceChoices: [String] {
        let known = model.devices.sorted {
            $0.displayName.localizedCaseInsensitiveCompare($1.displayName) == .orderedAscending
        }
        var macs: [String] = known.map { $0.mac }
        for mac in draft.src.devices where !macs.contains(mac) {
            macs.append(mac)
        }
        return macs
    }

    var body: some View {
        Form {
            generalSection
            if model.hasSeveralNetworks || !draft.src.networks.isEmpty {
                sourceNetworksSection
            }
            sourceDevicesSection
            StringListEditor(
                title: "From addresses",
                items: $draft.src.cidrs,
                placeholder: "10.83.0.50 or 10.83.0.64/28",
                keyboard: .numbersAndPunctuation,
                footer: ""
            )
            StringListEditor(
                title: "To domains",
                items: $draft.dst.domains,
                placeholder: "example.com",
                keyboard: .URL,
                footer: "Includes subdomains."
            )
            StringListEditor(
                title: "To addresses",
                items: $draft.dst.cidrs,
                placeholder: "203.0.113.0/24",
                keyboard: .numbersAndPunctuation,
                footer: ""
            )
            countriesSection
            protocolSection
            StringListEditor(
                title: "Ports",
                items: $draft.dst.ports,
                placeholder: "443 or 8000-8100",
                keyboard: .numbersAndPunctuation,
                footer: ""
            )
        }
        .navigationTitle(draft.name.isEmpty ? "Rule" : draft.name)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
                    .disabled(!canSave)
            }
        }
        .onAppear { sync() }
        .onChange(of: model.config?.rev) { _, _ in sync() }
    }

    private var generalSection: some View {
        Section {
            TextField("Name", text: $draft.name)
            Toggle("Enabled", isOn: $draft.enabled)
            Picker("Send to", selection: $draft.action) {
                ForEach(model.availableActions, id: \.self) { action in
                    Text(model.actionLabel(action.raw)).tag(action.raw)
                }
            }
        }
    }

    /// Whole networks as the source: every device on them.
    private var sourceNetworksSection: some View {
        Section {
            ForEach(model.config?.networks ?? []) { network in
                Button {
                    toggleNetwork(network.id)
                } label: {
                    HStack {
                        Text(network.name)
                        Spacer()
                        if draft.src.networks.contains(network.id) {
                            Image(systemName: "checkmark")
                                .foregroundStyle(.tint)
                        }
                    }
                }
                .foregroundStyle(.primary)
            }
        } header: {
            Text("From networks")
        }
    }

    private var sourceDevicesSection: some View {
        Section {
            if deviceChoices.isEmpty {
                Text("All devices")
                    .foregroundStyle(.secondary)
            }
            ForEach(deviceChoices, id: \.self) { mac in
                Button {
                    toggleDevice(mac)
                } label: {
                    HStack {
                        Text(model.deviceName(mac))
                        Spacer()
                        if draft.src.devices.contains(mac) {
                            Image(systemName: "checkmark")
                                .foregroundStyle(.tint)
                        }
                    }
                }
                .foregroundStyle(.primary)
            }
        } header: {
            Text("From devices")
        } footer: {
            Text("Nothing ticked means all.")
        }
    }

    private var countriesMissing: Bool {
        model.status?.geoip.available == false
    }

    private var countriesSection: some View {
        Section {
            if countriesMissing {
                Label("This router has no country database.", systemImage: "exclamationmark.triangle.fill")
                    .font(.footnote)
                    .foregroundStyle(.orange)
            }
            ForEach(Array(draft.dst.countries.enumerated()), id: \.offset) { item in
                CountryRow(code: item.element)
            }
            .onDelete { offsets in
                draft.dst.countries.remove(atOffsets: offsets)
            }
            NavigationLink("Add country") {
                CountrySelectionList(selection: $draft.dst.countries)
            }
        } header: {
            Text("To countries")
        }
    }

    private var protocolSection: some View {
        Section {
            Picker("Protocol", selection: $draft.dst.proto) {
                Text("Any").tag("")
                Text("TCP").tag("tcp")
                Text("UDP").tag("udp")
            }
            .pickerStyle(.segmented)
        }
    }

    private func toggleNetwork(_ id: String) {
        if let index = draft.src.networks.firstIndex(of: id) {
            draft.src.networks.remove(at: index)
        } else {
            draft.src.networks.append(id)
        }
    }

    private func toggleDevice(_ mac: String) {
        if let index = draft.src.devices.firstIndex(of: mac) {
            draft.src.devices.remove(at: index)
        } else {
            draft.src.devices.append(mac)
        }
    }

    private func sync() {
        guard let current = stored else { return }
        if original == nil || draft == original {
            draft = current
        }
        original = current
    }

    private func save() {
        var cleaned = draft
        cleaned.name = cleaned.name.trimmingCharacters(in: .whitespaces)
        // Country codes are stored in upper case, each one once.
        var codes: [String] = []
        for code in cleaned.dst.countries {
            let upper = code.uppercased()
            if !codes.contains(upper) {
                codes.append(upper)
            }
        }
        cleaned.dst.countries = codes
        let rule = cleaned
        Task {
            let ok = await model.update { cfg in
                if let index = cfg.policy.rules.firstIndex(where: { $0.id == rule.id }) {
                    cfg.policy.rules[index] = rule
                }
            }
            if ok {
                dismiss()
            }
        }
    }
}

/// A country as a row: flag, name in the phone's language, and code.
private struct CountryRow: View {
    let code: String

    var body: some View {
        HStack(spacing: 8) {
            Text(countryFlag(code))
            Text(countryName(code.uppercased()))
            Spacer()
            Text(code.uppercased())
                .foregroundStyle(.secondary)
        }
    }
}

/// Every country, to tick the ones a rule should match.
private struct CountrySelectionList: View {
    @Binding var selection: [String]
    @State private var search: String = ""

    private var matches: [CountryChoice] {
        let text = search.trimmingCharacters(in: .whitespaces)
        if text.isEmpty { return CountryChoice.all }
        return CountryChoice.all.filter { choice in
            choice.name.localizedCaseInsensitiveContains(text) || choice.code.localizedCaseInsensitiveContains(text)
        }
    }

    var body: some View {
        List {
            if matches.isEmpty {
                Text("No match")
                    .foregroundStyle(.secondary)
            }
            ForEach(matches) { choice in
                Button {
                    toggle(choice.code)
                } label: {
                    HStack(spacing: 8) {
                        CountryRow(code: choice.code)
                        if isSelected(choice.code) {
                            Image(systemName: "checkmark")
                                .foregroundStyle(.tint)
                        }
                    }
                }
                .foregroundStyle(.primary)
            }
        }
        .navigationTitle("Countries")
        .navigationBarTitleDisplayMode(.inline)
        .searchable(text: $search, prompt: "Country")
    }

    private func isSelected(_ code: String) -> Bool {
        selection.contains(where: { $0.uppercased() == code })
    }

    private func toggle(_ code: String) {
        if isSelected(code) {
            selection.removeAll(where: { $0.uppercased() == code })
        } else {
            selection.append(code)
        }
    }
}
