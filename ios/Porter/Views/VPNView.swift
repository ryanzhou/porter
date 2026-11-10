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
            RoutingSection()
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
            Button("Add VPN") { adding = true }
        } header: {
            Text("Profiles")
        } footer: {
            Text("WireGuard tunnels. A profile carries traffic only when the default route, a device or a rule sends traffic to it.")
        }
    }
}

private struct VPNProfileRow: View {
    @EnvironmentObject private var model: AppModel
    let profile: VPNProfile
    let status: VPNStatus?

    private var stateLine: String {
        guard let status else { return "Off" }
        return vpnStateLabel(status)
    }

    private var detailLine: String {
        guard let status, status.state == "up" || status.state == "connecting" else { return "" }
        var parts: [String] = []
        if let endpoint = status.endpoint, !endpoint.isEmpty { parts.append(endpoint) }
        if let handshake = status.handshake, handshake > 0 {
            parts.append("handshake " + relativeTime(handshake))
        }
        let received = formatBytes(UInt64(clamping: status.rx))
        let sent = formatBytes(UInt64(clamping: status.tx))
        parts.append("↓ " + received + " ↑ " + sent)
        return parts.joined(separator: " · ")
    }

    var body: some View {
        HStack(spacing: 10) {
            StatusDot(color: vpnStateColor(status?.state ?? "off"))
            VStack(alignment: .leading, spacing: 2) {
                Text(profile.name)
                Text(stateLine)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if !detailLine.isEmpty {
                    Text(detailLine)
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
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
                    Text("Traffic assigned to this VPN is blocked, not sent directly, whenever the tunnel is down.")
                }
                WireGuardSummarySection(wireguard: profile.wireguard)
                Section {
                    ConfirmedButton(
                        title: "Delete VPN",
                        message: "Devices that use this VPN go back to the default route, and rules or a default route that use it switch to direct.",
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
                LabeledContent("Endpoint", value: item.element.endpoint.isEmpty ? "None" : item.element.endpoint)
                LabeledContent("Allowed IPs", value: list(item.element.allowedIps))
            }
        }
    }
}

private struct AddVPNSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var name: String = ""
    @State private var conf: String = ""
    @State private var importing = false
    @State private var importProblem: String = ""
    @State private var working = false

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }

    private var canAdd: Bool {
        !working && !cleanName.isEmpty && !conf.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    var body: some View {
        Form {
            Section("Name") {
                TextField("Name", text: $name)
            }
            Section {
                TextEditor(text: $conf)
                    .font(.footnote.monospaced())
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .frame(minHeight: 180)
                Button("Import file") { importing = true }
                Button("Paste from clipboard") {
                    conf = UIPasteboard.general.string ?? conf
                }
                if !importProblem.isEmpty {
                    Text(importProblem)
                        .font(.footnote)
                        .foregroundStyle(.red)
                }
            } header: {
                Text("WireGuard configuration")
            } footer: {
                Text("The contents of the .conf file from your VPN provider or server. It contains a private key, which is sent to the router over the encrypted Bluetooth link and stored there.")
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

// MARK: - Routing

private struct RoutingSection: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        Section {
            Picker("Default route", selection: model.binding(\.policy.`default`, fallback: "direct")) {
                ForEach(model.availableActions, id: \.self) { action in
                    Text(model.actionLabel(action.raw)).tag(action.raw)
                }
            }
        } header: {
            Text("Routing")
        } footer: {
            Text("Where traffic goes when no rule matches it and its device has no route of its own.")
        }
    }
}

private struct RulesSection: View {
    @EnvironmentObject private var model: AppModel
    /// The order just chosen by dragging, shown until the router confirms it.
    @State private var pendingOrder: [PolicyRule]? = nil

    private var rules: [PolicyRule] {
        pendingOrder ?? model.config?.policy.rules ?? []
    }

    private func enabledBinding(_ id: String) -> Binding<Bool> {
        Binding(
            get: { rules.first(where: { $0.id == id })?.enabled ?? false },
            set: { value in
                Task {
                    await model.update { cfg in
                        if let index = cfg.policy.rules.firstIndex(where: { $0.id == id }) {
                            cfg.policy.rules[index].enabled = value
                        }
                    }
                }
            }
        )
    }

    var body: some View {
        Section {
            ForEach(rules) { rule in
                NavigationLink {
                    RuleEditorView(ruleID: rule.id)
                } label: {
                    RuleRow(rule: rule, enabled: enabledBinding(rule.id))
                }
            }
            .onMove { source, destination in
                move(from: source, to: destination)
            }
            .onDelete { offsets in
                remove(at: offsets)
            }
            Button("Add rule") { addRule() }
        } header: {
            Text("Rules")
        } footer: {
            Text("Rules are checked from top to bottom and the first one that matches wins. Traffic that no rule matches follows its device's route setting, and after that the default route. Tap Edit to reorder.")
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

private struct RuleRow: View {
    @EnvironmentObject private var model: AppModel
    let rule: PolicyRule
    let enabled: Binding<Bool>

    private var sourceSummary: String {
        let count = rule.src.devices.count + rule.src.cidrs.count
        if count == 0 { return "Everything" }
        return count == 1 ? "1 source" : "\(count) sources"
    }

    private var destinationSummary: String {
        let count = rule.dst.domains.count + rule.dst.cidrs.count + rule.dst.countries.count + rule.dst.ports.count
        if count == 0 {
            return rule.dst.proto.isEmpty ? "anywhere" : "any " + rule.dst.proto.uppercased()
        }
        return count == 1 ? "1 destination" : "\(count) destinations"
    }

    private var summary: String {
        sourceSummary + " → " + destinationSummary + " · " + model.actionLabel(rule.action)
    }

    var body: some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text(rule.name.isEmpty ? "Unnamed rule" : rule.name)
                Text(summary)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Toggle("Enabled", isOn: enabled)
                .labelsHidden()
        }
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
            sourceDevicesSection
            StringListEditor(
                title: "Source addresses",
                items: $draft.src.cidrs,
                placeholder: "10.83.0.50 or 10.83.0.64/28",
                keyboard: .numbersAndPunctuation,
                footer: "Addresses or ranges on the router's network. With no devices and no addresses, the rule covers everything on the network."
            )
            StringListEditor(
                title: "Destination domains",
                items: $draft.dst.domains,
                placeholder: "example.com",
                keyboard: .URL,
                footer: "Subdomains are included."
            )
            StringListEditor(
                title: "Destination addresses",
                items: $draft.dst.cidrs,
                placeholder: "203.0.113.0/24",
                keyboard: .numbersAndPunctuation,
                footer: "Single addresses or ranges."
            )
            countriesSection
            protocolSection
            StringListEditor(
                title: "Destination ports",
                items: $draft.dst.ports,
                placeholder: "443 or 8000-8100",
                keyboard: .numbersAndPunctuation,
                footer: "Protocol and ports narrow the match further. With every destination field empty, the rule matches traffic to anywhere."
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
            Picker("Action", selection: $draft.action) {
                ForEach(model.availableActions, id: \.self) { action in
                    Text(model.actionLabel(action.raw)).tag(action.raw)
                }
            }
        } footer: {
            Text("Rules are checked in order and the first match wins. Device settings and the default route apply to traffic that no rule matches.")
        }
    }

    private var sourceDevicesSection: some View {
        Section {
            if deviceChoices.isEmpty {
                Text("No devices known yet.")
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
            Text("Source devices")
        } footer: {
            Text("Tick the devices this rule applies to. None ticked means every device, unless source addresses are listed below.")
        }
    }

    private var countriesMissing: Bool {
        model.status?.geoip.available == false
    }

    private var countriesSection: some View {
        Section {
            if countriesMissing {
                Label("Country rules need the country database, which this router image does not have, so they will not match anything.", systemImage: "exclamationmark.triangle.fill")
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
            Text("Destination countries")
        } footer: {
            Text("A destination matches if it is in any listed domain, address range or country. The country is the one the router's country database gives for the destination address. Up to 64 countries.")
        }
    }

    private var protocolSection: some View {
        Section("Protocol") {
            Picker("Protocol", selection: $draft.dst.proto) {
                Text("Any").tag("")
                Text("TCP").tag("tcp")
                Text("UDP").tag("udp")
            }
            .pickerStyle(.segmented)
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
                Text("No country matches the search.")
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
