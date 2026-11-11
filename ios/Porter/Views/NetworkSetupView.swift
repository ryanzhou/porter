import PorterKit
import SwiftUI

/// The parts of the configuration that describe the router's own side:
/// its Wi-Fi networks, its networks, what each radio and wired port is
/// used for. They depend on each other, so they are edited together as a
/// draft and reach the router in one step.
struct NetworkSetup: Equatable {
    var wifi: [WifiNetworkConfig]
    var networks: [NetworkConfig]
    var radios: [RadioConfig]
    var ethernet: [EthernetPort]
    var lan: LANConfig

    init(_ config: RouterConfig) {
        wifi = config.wifi
        networks = config.networks
        radios = config.radios
        ethernet = config.ethernet
        lan = config.lan
    }

    func apply(to config: inout RouterConfig) {
        config.wifi = wifi
        config.networks = networks
        config.radios = radios
        config.ethernet = ethernet
        config.lan = lan
    }

    /// Carries what was changed since `base` over to a newer copy from the
    /// router, item by item and field by field: what was not touched here
    /// follows the router. Taking a whole part from the draft because one
    /// thing in it was touched would undo what was changed elsewhere in
    /// the meantime, such as a radio made the Wi-Fi uplink under Internet.
    func rebased(from base: NetworkSetup, onto fresh: NetworkSetup) -> NetworkSetup {
        var out = fresh
        out.wifi = NetworkSetup.rebasedItems(wifi, from: base.wifi, onto: fresh.wifi)
        out.networks = NetworkSetup.rebasedItems(networks, from: base.networks, onto: fresh.networks)
        out.radios = fresh.radios.map { radio in rebasedRadio(radio, from: base) }
        out.ethernet = fresh.ethernet.map { port in rebasedPort(port, from: base) }
        if lan != base.lan { out.lan = lan }
        // A network removed here may have been given a Wi-Fi network or
        // a port elsewhere since; they go to the main one, as the ones it
        // had when it was removed did.
        for old in base.networks where !networks.contains(where: { $0.id == old.id }) {
            out.removeNetwork(old.id)
        }
        return out
    }

    /// One list of Wi-Fi networks or of networks, rebased. An item changed
    /// here keeps the draft's version and an untouched one follows the
    /// router; one added here is kept and one removed here stays removed;
    /// one the router has gained since is added. The router's order is
    /// kept, so its first item, the main one, stays first.
    private static func rebasedItems<Item: Identifiable & Equatable>(_ draft: [Item], from base: [Item], onto fresh: [Item]) -> [Item] where Item.ID == String {
        var out: [Item] = []
        for item in fresh {
            guard let old = base.first(where: { $0.id == item.id }) else {
                // New on the router since the draft was begun.
                out.append(item)
                continue
            }
            guard let mine = draft.first(where: { $0.id == item.id }) else {
                // Removed here.
                continue
            }
            out.append(mine != old ? mine : item)
        }
        for mine in draft where !fresh.contains(where: { $0.id == mine.id }) {
            if let old = base.first(where: { $0.id == mine.id }) {
                // Gone from the router since. Untouched here, it goes
                // from the draft too; changed here, the draft keeps it.
                if mine != old { out.append(mine) }
            } else {
                // Added here.
                out.append(mine)
            }
        }
        return out
    }

    /// One radio of the fresh copy with what was changed here. This page
    /// decides whether a radio broadcasts and never whether it is the
    /// uplink, so of the two things its role says the second is always
    /// the router's.
    private func rebasedRadio(_ radio: RadioConfig, from base: NetworkSetup) -> RadioConfig {
        guard let mine = radios.first(where: { $0.id == radio.id }),
              let old = base.radios.first(where: { $0.id == radio.id }) else { return radio }
        var out: RadioConfig = radio
        if mine.band != old.band {
            // A channel is one of its band's: choosing a band here also
            // chose the channel, if only by leaving it automatic.
            out.band = mine.band
            out.channel = mine.channel
        } else if mine.channel != old.channel {
            out.channel = mine.channel
        }
        if mine.width != old.width { out.width = mine.width }
        let uplink: Bool = radio.role == .wan || radio.role == .apWan
        var broadcasts: Bool = radio.role == .ap || radio.role == .apWan
        let mineBroadcasts: Bool = mine.role == .ap || mine.role == .apWan
        let oldBroadcasts: Bool = old.role == .ap || old.role == .apWan
        if mineBroadcasts != oldBroadcasts { broadcasts = mineBroadcasts }
        if uplink {
            out.role = broadcasts ? .apWan : .wan
        } else {
            out.role = broadcasts ? .ap : .off
        }
        return out
    }

    /// One wired port of the fresh copy with what was changed here. How
    /// it gets its addresses is not set on this page.
    private func rebasedPort(_ port: EthernetPort, from base: NetworkSetup) -> EthernetPort {
        guard let mine = ethernet.first(where: { $0.name == port.name }),
              let old = base.ethernet.first(where: { $0.name == port.name }) else { return port }
        var out: EthernetPort = port
        if mine.role != old.role {
            out.role = mine.role
            // A port given to a network here was given to that one, also
            // when it is the one the port had been left with before.
            if mine.role == .lan { out.network = mine.network }
        }
        if mine.network != old.network { out.network = mine.network }
        return out
    }

    func networkName(_ id: String) -> String {
        networks.first(where: { $0.id == id })?.name ?? id
    }

    /// Removes a network; what belonged to it goes to the main one.
    mutating func removeNetwork(_ id: String) {
        guard id != RouterConfig.mainNetwork else { return }
        networks.removeAll(where: { $0.id == id })
        for index in wifi.indices where wifi[index].network == id {
            wifi[index].network = RouterConfig.mainNetwork
        }
        for index in ethernet.indices where ethernet[index].network == id {
            ethernet[index].network = RouterConfig.mainNetwork
        }
    }
}

/// What the add sheet is adding.
private enum AddKind: String, Identifiable {
    case wifi, network
    var id: String { rawValue }
}

/// Wi-Fi names, networks, radios and ports. Nothing here changes the
/// router until Apply, which sends everything at once; Discard puts it
/// back as it is.
struct NetworkSetupPage: View {
    @EnvironmentObject private var model: AppModel

    @State private var base: NetworkSetup? = nil
    @State private var draft: NetworkSetup? = nil
    @State private var check: ConfigCheck? = nil
    @State private var checking = false
    @State private var applying = false
    @State private var confirming = false
    @State private var adding: AddKind? = nil

    private var dirty: Bool {
        guard let draft, let base else { return false }
        return draft != base
    }

    private var canApply: Bool {
        dirty && !applying && !checking && check?.error == nil
    }

    private var effects: String {
        let lines: [String] = check?.effects ?? []
        return lines.isEmpty ? "The changes take effect together." : lines.joined(separator: "\n")
    }

    private func binding(_ current: NetworkSetup) -> Binding<NetworkSetup> {
        Binding(
            get: { draft ?? current },
            set: { draft = $0 }
        )
    }

    var body: some View {
        Form {
            if let current = draft {
                if dirty {
                    ChangesSection(check: check, checking: checking)
                }
                WifiSection(setup: binding(current)) { adding = .wifi }
                NetworksSection(setup: binding(current)) { adding = .network }
                RadiosSection(setup: binding(current))
                PortsSection(setup: binding(current))
                CommonSection(lan: binding(current).lan)
            }
        }
        // A change made while Apply is on its way would be thrown away
        // when the answer comes.
        .disabled(applying)
        .navigationTitle("Wi-Fi & networks")
        .navigationBarTitleDisplayMode(.inline)
        .navigationBarBackButtonHidden(dirty)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                if dirty {
                    Button("Discard") { discard() }
                        .disabled(applying)
                }
            }
            ToolbarItem(placement: .confirmationAction) {
                Button("Apply") { confirming = true }
                    .disabled(!canApply)
            }
        }
        .confirmationDialog("Apply the changes?", isPresented: $confirming, titleVisibility: .visible) {
            Button("Apply") { apply() }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text(effects)
        }
        .sheet(item: $adding) { kind in
            NavigationStack {
                if let current = draft {
                    AddSheet(kind: kind, setup: binding(current))
                }
            }
        }
        .onAppear { sync() }
        .onChange(of: model.config) { _, _ in sync() }
        .task(id: draft) { await runCheck() }
    }

    /// Follows the router while nothing has been changed here, and keeps
    /// what has been changed when the router's copy moves on.
    private func sync() {
        guard !applying, let config = model.config else { return }
        let fresh = NetworkSetup(config)
        if let base, let draft, draft != base {
            self.draft = draft.rebased(from: base, onto: fresh)
        } else {
            draft = fresh
        }
        base = fresh
    }

    private func discard() {
        draft = base
        check = nil
    }

    /// Asks the router what the draft would do, a moment after the last
    /// change to it.
    private func runCheck() async {
        guard let setup = draft, let base, setup != base else {
            check = nil
            checking = false
            return
        }
        checking = true
        try? await Task.sleep(nanoseconds: 400_000_000)
        if Task.isCancelled { return }
        // Asked about what Apply would send: the draft's changes on the
        // copy the router has at that moment.
        let start: NetworkSetup = base
        let result: ConfigCheck? = await model.check { cfg in
            setup.rebased(from: start, onto: NetworkSetup(cfg)).apply(to: &cfg)
        }
        if Task.isCancelled { return }
        check = result
        checking = false
    }

    private func apply() {
        guard let setup = draft, let start = base else { return }
        applying = true
        Task {
            // The copy the write is built on may be newer than the one
            // the draft was made from (the router moved on, and the write
            // is tried again on what it has now). Only what was changed
            // here goes onto it; the rest of it stays as the router has it.
            let ok = await model.update { cfg in
                setup.rebased(from: start, onto: NetworkSetup(cfg)).apply(to: &cfg)
            }
            applying = false
            if ok {
                base = nil
                draft = nil
                check = nil
                sync()
            } else {
                // Refused: the draft stays, to be put right, on top of
                // what the router has now.
                sync()
            }
        }
    }
}

// MARK: - What Apply will do

private struct NoticeRow: View {
    let symbol: String
    let color: Color
    let text: String

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Image(systemName: symbol)
                .foregroundStyle(color)
            Text(text)
                .font(.footnote)
        }
    }
}

/// Shown while there are changes: why they cannot be applied, what the
/// hardware will not do of them, what will be noticed.
private struct ChangesSection: View {
    let check: ConfigCheck?
    let checking: Bool

    private var notes: [String] { check?.notes ?? [] }
    private var effects: [String] { check?.effects ?? [] }

    private var quiet: Bool {
        check?.error == nil && notes.isEmpty && effects.isEmpty
    }

    var body: some View {
        Section {
            if let error = check?.error {
                NoticeRow(symbol: "xmark.octagon.fill", color: Color.red, text: error)
            }
            ForEach(notes, id: \.self) { note in
                NoticeRow(symbol: "exclamationmark.triangle.fill", color: Color.orange, text: note)
            }
            ForEach(effects, id: \.self) { effect in
                NoticeRow(symbol: "arrow.triangle.2.circlepath", color: Color.secondary, text: effect)
            }
            if quiet {
                NoticeRow(symbol: "pencil", color: Color.secondary, text: checking ? "Checking the changes" : "Nothing changes on the router until you tap Apply.")
            }
        }
    }
}

// MARK: - Wi-Fi networks

private func securityLabel(_ security: WifiSecurity) -> String {
    switch security {
    case .wpa2: return "WPA2"
    case .wpa2wpa3: return "WPA2+WPA3"
    case .wpa3: return "WPA3"
    }
}

private struct WifiSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var setup: NetworkSetup
    let add: () -> Void

    private var offAir: [String] { model.status?.offAir ?? [] }

    var body: some View {
        Section {
            ForEach(setup.wifi) { wifi in
                NavigationLink {
                    WifiNetworkEditor(setup: $setup, id: wifi.id)
                } label: {
                    HStack(spacing: 8) {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(wifi.ssid.isEmpty ? "No name yet" : wifi.ssid)
                                .lineLimit(1)
                            if setup.networks.count > 1 {
                                Text(setup.networkName(wifi.network))
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                            }
                        }
                        Spacer(minLength: 8)
                        if !wifi.enabled {
                            Text("Off")
                                .foregroundStyle(.secondary)
                        } else if offAir.contains(wifi.id) {
                            Badge(text: "Not broadcast", color: Color.orange)
                        }
                    }
                }
            }
            if setup.wifi.count < RouterConfig.maxWifiNetworks {
                Button("Add Wi-Fi network") { add() }
            }
        } header: {
            Text("Your Wi-Fi")
        }
    }
}

private struct WifiNetworkEditor: View {
    @Environment(\.dismiss) private var dismiss
    @Binding var setup: NetworkSetup
    let id: String

    @State private var showPassword = false

    private var isMain: Bool { id == RouterConfig.mainWifi }

    /// A binding to one field of this Wi-Fi network. Once the network has
    /// been removed (the page is still closing) it reads the fallback.
    private func field<Value>(_ keyPath: WritableKeyPath<WifiNetworkConfig, Value>, _ fallback: Value) -> Binding<Value> {
        let id = self.id
        return Binding(
            get: {
                guard let index = setup.wifi.firstIndex(where: { $0.id == id }) else { return fallback }
                return setup.wifi[index][keyPath: keyPath]
            },
            set: { value in
                guard let index = setup.wifi.firstIndex(where: { $0.id == id }) else { return }
                setup.wifi[index][keyPath: keyPath] = value
            }
        )
    }

    private var title: String {
        let name: String = setup.wifi.first(where: { $0.id == id })?.ssid ?? ""
        return name.isEmpty ? "Wi-Fi" : name
    }

    /// What is wrong with the name or the password as typed, if anything.
    private var problem: String? {
        guard let current = setup.wifi.first(where: { $0.id == id }) else { return nil }
        return networkNameProblem(current.ssid) ?? passphraseProblem(current.passphrase)
    }

    var body: some View {
        Form {
            Section {
                TextField("Name", text: field(\.ssid, ""))
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                HStack {
                    if showPassword {
                        TextField("Password", text: field(\.passphrase, ""))
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                    } else {
                        SecureField("Password", text: field(\.passphrase, ""))
                    }
                    Button {
                        showPassword.toggle()
                    } label: {
                        Image(systemName: showPassword ? "eye.slash" : "eye")
                    }
                    .buttonStyle(.borderless)
                    .accessibilityLabel(Text(showPassword ? "Hide password" : "Show password"))
                }
                Picker("Security", selection: field(\.security, WifiSecurity.wpa2)) {
                    ForEach(WifiSecurity.allCases) { option in
                        Text(securityLabel(option)).tag(option)
                    }
                }
            } footer: {
                if let problem {
                    Text(problem)
                        .foregroundStyle(.red)
                }
            }
            if setup.networks.count > 1 {
                Section {
                    Picker("Network", selection: field(\.network, RouterConfig.mainNetwork)) {
                        ForEach(setup.networks) { network in
                            Text(network.name).tag(network.id)
                        }
                    }
                }
            }
            Section {
                Toggle("Hidden network", isOn: field(\.hidden, false))
                Toggle("Keep its devices apart from each other", isOn: field(\.isolate, false))
            }
            if !isMain {
                Section {
                    Toggle("Enabled", isOn: field(\.enabled, true))
                }
                Section {
                    Button("Remove this Wi-Fi network", role: .destructive) { remove() }
                }
            }
        }
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
    }

    private func remove() {
        let id = self.id
        setup.wifi.removeAll(where: { $0.id == id })
        dismiss()
    }
}

// MARK: - Networks

private struct NetworksSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var setup: NetworkSetup
    let add: () -> Void

    private func online(_ id: String) -> Int {
        model.status?.lan.networks[id] ?? 0
    }

    private func caption(_ network: NetworkConfig) -> String {
        var parts: [String] = []
        if !network.address.isEmpty { parts.append(network.address) }
        if network.isolated { parts.append("kept apart") }
        if !network.route.isEmpty { parts.append(model.actionLabel(network.route)) }
        return parts.joined(separator: " · ")
    }

    var body: some View {
        Section {
            ForEach(setup.networks) { network in
                NavigationLink {
                    NetworkEditor(setup: $setup, id: network.id)
                } label: {
                    HStack(spacing: 8) {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(network.name)
                                .lineLimit(1)
                            Text(caption(network))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                                .lineLimit(1)
                        }
                        Spacer(minLength: 8)
                        Text("\(online(network.id)) online")
                            .foregroundStyle(.secondary)
                            .monospacedDigit()
                    }
                }
            }
            if setup.networks.count < RouterConfig.maxNetworks {
                Button("Add network") { add() }
            }
        } header: {
            Text("Your networks")
        } footer: {
            Text("A network is a group of your devices with addresses of its own, such as one for guests. Each Wi-Fi network and wired port puts its devices on one of them.")
        }
    }
}

/// What is wrong with a network's address as typed; nil while it is empty
/// (the router then chooses one) or looks usable.
private func networkAddressProblem(_ text: String) -> String? {
    if text.isEmpty { return nil }
    let hint: String = "Use a private address followed by its size, /16 to /28, such as 10.84.0.1/24."
    let halves: [Substring] = text.split(separator: "/", omittingEmptySubsequences: false)
    guard halves.count == 2, let bits = Int(halves[1]), bits >= 16, bits <= 28 else { return hint }
    let parts: [Substring] = halves[0].split(separator: ".", omittingEmptySubsequences: false)
    guard parts.count == 4 else { return hint }
    var octets: [Int] = []
    for part in parts {
        guard let value = Int(part), value >= 0, value <= 255 else { return hint }
        octets.append(value)
    }
    let first: Int = octets[0]
    let second: Int = octets[1]
    if first == 10 { return nil }
    if first == 172 && second >= 16 && second <= 31 { return nil }
    if first == 192 && second == 168 { return nil }
    return hint
}

private struct NetworkEditor: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @Binding var setup: NetworkSetup
    let id: String

    private var isMain: Bool { id == RouterConfig.mainNetwork }

    private var current: NetworkConfig? { setup.networks.first(where: { $0.id == id }) }

    private func field<Value>(_ keyPath: WritableKeyPath<NetworkConfig, Value>, _ fallback: Value) -> Binding<Value> {
        let id = self.id
        return Binding(
            get: {
                guard let index = setup.networks.firstIndex(where: { $0.id == id }) else { return fallback }
                return setup.networks[index][keyPath: keyPath]
            },
            set: { value in
                guard let index = setup.networks.firstIndex(where: { $0.id == id }) else { return }
                setup.networks[index][keyPath: keyPath] = value
            }
        )
    }

    private var wifiNames: String {
        let names: [String] = setup.wifi.filter { $0.network == id && !$0.ssid.isEmpty }.map { $0.ssid }
        return names.isEmpty ? "None" : names.joined(separator: ", ")
    }

    private var portNames: String {
        let names: [String] = setup.ethernet.filter { $0.role == .lan && $0.network == id }.map { $0.name }
        return names.isEmpty ? "None" : names.joined(separator: ", ")
    }

    /// What is wrong with the name or the address as typed, if anything.
    /// The router checks both fully before anything is applied.
    private var problem: String? {
        guard let current else { return nil }
        return shortNameProblem(current.name) ?? networkAddressProblem(current.address)
    }

    /// What the network's devices do when it has no route of its own.
    private var defaultRouteLabel: String {
        "Default (" + model.actionLabel(model.config?.policy.`default` ?? "direct") + ")"
    }

    private var mainName: String { setup.networkName(RouterConfig.mainNetwork) }

    var body: some View {
        Form {
            Section {
                TextField("Name", text: field(\.name, ""))
                LabeledContent("Address") {
                    TextField("Chosen by the router", text: field(\.address, ""))
                        .multilineTextAlignment(.trailing)
                        .font(.body.monospaced())
                        .keyboardType(.numbersAndPunctuation)
                        .autocorrectionDisabled()
                }
                if let prefix = current?.ulaPrefix, !prefix.isEmpty, setup.lan.ipv6 {
                    LabeledContent("IPv6", value: prefix)
                }
            } footer: {
                if let problem {
                    Text(problem)
                        .foregroundStyle(.red)
                }
            }
            Section {
                Toggle("Keep apart from your other networks", isOn: field(\.isolated, false))
            } footer: {
                Text("Devices on your other networks can still reach the ones on this network.")
            }
            Section {
                Picker("Route", selection: field(\.route, "")) {
                    Text(defaultRouteLabel).tag("")
                    ForEach(model.availableActions, id: \.self) { action in
                        Text(model.actionLabel(action.raw)).tag(action.raw)
                    }
                }
            } footer: {
                Text("Route sets how devices on this network go online. A device's own route, and a rule on the VPN tab that fits its traffic, take priority.")
            }
            Section {
                LabeledContent("Wi-Fi networks", value: wifiNames)
                LabeledContent("Wired ports", value: portNames)
            } footer: {
                Text("Devices join this network through these. To change them, choose the network on a Wi-Fi network's page or under Wired ports.")
            }
            if !isMain {
                Section {
                    Button("Remove this network", role: .destructive) { remove() }
                } footer: {
                    Text("Its Wi-Fi networks and wired ports move to \(mainName), and their devices with them.")
                }
            }
        }
        .navigationTitle(current?.name ?? "Network")
        .navigationBarTitleDisplayMode(.inline)
    }

    private func remove() {
        setup.removeNetwork(id)
        dismiss()
    }
}

// MARK: - Adding

private struct AddSheet: View {
    @Environment(\.dismiss) private var dismiss
    let kind: AddKind
    @Binding var setup: NetworkSetup

    @State private var name: String = ""
    @State private var password: String = ""
    /// The network a new Wi-Fi network belongs to; "+" for one of its own.
    @State private var home: String = RouterConfig.mainNetwork
    @State private var networkName: String = "Guest"
    @State private var apart = true

    private var canAddNetwork: Bool { setup.networks.count < RouterConfig.maxNetworks }

    private var valid: Bool {
        switch kind {
        case .wifi:
            guard validNetworkName(name), validPassphrase(password) else { return false }
            return home != "+" || validRouterName(networkName)
        case .network:
            return validRouterName(name)
        }
    }

    var body: some View {
        Form {
            if kind == .wifi {
                Section {
                    TextField("Name", text: $name)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    TextField("Password", text: $password)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                } footer: {
                    if let problem = networkNameProblem(name) ?? passphraseProblem(password) {
                        Text(problem)
                            .foregroundStyle(.red)
                    }
                }
                Section {
                    Picker("Network", selection: $home) {
                        ForEach(setup.networks) { network in
                            Text(network.name).tag(network.id)
                        }
                        if canAddNetwork {
                            Text("A new network").tag("+")
                        }
                    }
                    if home == "+" {
                        TextField("Network name", text: $networkName)
                        Toggle("Keep apart from your other networks", isOn: $apart)
                    }
                } footer: {
                    if home == "+", let problem = shortNameProblem(networkName) {
                        Text(problem)
                            .foregroundStyle(.red)
                    } else if home == "+" && apart {
                        Text("This suits guests. Their devices get online but cannot reach the devices on your other networks.")
                    }
                }
            } else {
                Section {
                    TextField("Name", text: $name)
                    Toggle("Keep apart from your other networks", isOn: $apart)
                } footer: {
                    if let problem = shortNameProblem(name) {
                        Text(problem)
                            .foregroundStyle(.red)
                    } else {
                        Text("Devices join a network through a Wi-Fi network or a wired port. Give this one either of them after adding it.")
                    }
                }
            }
        }
        .navigationTitle(kind == .wifi ? "New Wi-Fi network" : "New network")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel") { dismiss() }
            }
            ToolbarItem(placement: .confirmationAction) {
                Button("Add") { add() }
                    .disabled(!valid)
            }
        }
        .onAppear {
            // The usual reason for a second Wi-Fi network is guests.
            if kind == .wifi && setup.networks.count == 1 && canAddNetwork {
                home = "+"
            }
        }
    }

    private func add() {
        switch kind {
        case .wifi:
            var network = home
            if home == "+" {
                network = AppModel.newID()
                setup.networks.append(NetworkConfig(id: network, name: networkName.trimmingCharacters(in: .whitespaces), isolated: apart))
            }
            setup.wifi.append(WifiNetworkConfig(id: AppModel.newID(), ssid: name, passphrase: password, network: network))
        case .network:
            setup.networks.append(NetworkConfig(id: AppModel.newID(), name: name.trimmingCharacters(in: .whitespaces), isolated: apart))
        }
        dismiss()
    }
}

// MARK: - Radios

private func bandLabel(_ band: String) -> String {
    band.isEmpty ? "Automatic" : band + " GHz"
}

private func channelLabel(_ channel: Int) -> String {
    channel == 0 ? "Automatic" : String(channel)
}

private func widthLabel(_ width: Int) -> String {
    width == 0 ? "Automatic" : "\(width) MHz"
}

/// One section for each radio: whether it carries the router's Wi-Fi and,
/// when it does nothing else, where on the air.
private struct RadiosSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var setup: NetworkSetup

    private var present: [RadioInfo] { model.radios.filter { $0.present } }

    var body: some View {
        if present.isEmpty {
            Section("Radios") {
                Text("The router has no Wi-Fi radio.")
                    .foregroundStyle(.secondary)
            }
        }
        ForEach(present) { radio in
            RadioSetupRows(setup: $setup, radio: radio)
        }
    }
}

private struct RadioSetupRows: View {
    @Binding var setup: NetworkSetup
    let radio: RadioInfo

    private var setting: RadioConfig? { setup.radios.first(where: { $0.id == radio.id }) }

    private var role: RadioRole { setting?.role ?? .off }

    private var isUplink: Bool { role == .wan || role == .apWan }

    private var broadcasts: Bool { role == .ap || role == .apWan }

    private func edit(_ change: (inout RadioConfig) -> Void) {
        let id = radio.id
        guard let index = setup.radios.firstIndex(where: { $0.id == id }) else { return }
        change(&setup.radios[index])
    }

    private var broadcastBinding: Binding<Bool> {
        Binding(
            get: { broadcasts },
            set: { on in
                let uplink = isUplink
                edit { entry in
                    if uplink {
                        entry.role = on ? .apWan : .wan
                    } else {
                        entry.role = on ? .ap : .off
                    }
                }
            }
        )
    }

    /// The radio cannot carry a network at all, or not while it is also
    /// the uplink.
    private var cannotBroadcast: Bool {
        !radio.ap || (isUplink && !radio.apSta)
    }

    private var bandOptions: [String] {
        var options: [String] = [""]
        for band in radio.bands where band.name == "2.4" || band.name == "5" {
            options.append(band.name)
        }
        let current = setting?.band ?? ""
        if !options.contains(current) { options.append(current) }
        return options
    }

    private var channelOptions: [Int] {
        var options: [Int] = [0]
        let band = setting?.band ?? ""
        if let match = radio.bands.first(where: { $0.name == band }) {
            for channel in match.channels where channel.usable {
                options.append(channel.num)
            }
        }
        let current = setting?.channel ?? 0
        if !options.contains(current) { options.append(current) }
        return options
    }

    private var widthOptions: [Int] { [0, 20, 40, 80] }

    private var bandBinding: Binding<String> {
        Binding(
            get: { setting?.band ?? "" },
            set: { value in
                edit { entry in
                    entry.band = value
                    entry.channel = 0
                }
            }
        )
    }

    private var channelBinding: Binding<Int> {
        Binding(
            get: { setting?.channel ?? 0 },
            set: { value in edit { $0.channel = value } }
        )
    }

    private var widthBinding: Binding<Int> {
        Binding(
            get: { setting?.width ?? 0 },
            set: { value in edit { $0.width = value } }
        )
    }

    /// How many Wi-Fi networks the radio carries in the job it has.
    private var capacity: Int {
        max(1, isUplink ? radio.maxApSta : radio.maxAp)
    }

    private var wanted: Int { setup.wifi.filter { $0.enabled }.count }

    /// Why the switch or the channel settings are not there to use, and
    /// how many Wi-Fi networks fit when more are wanted.
    private var note: String {
        var lines: [String] = []
        if !radio.ap {
            lines.append("This radio cannot broadcast Wi-Fi. It can only join a network.")
        } else if isUplink {
            if broadcasts {
                lines.append("This radio also joins a Wi-Fi network for internet, so your Wi-Fi uses the same channel as that network.")
            } else if radio.apSta {
                lines.append("This radio joins a Wi-Fi network for internet. Which radio does that is chosen on the Internet tab.")
            } else {
                lines.append("This radio joins a Wi-Fi network for internet and cannot broadcast yours at the same time. Which radio joins is chosen on the Internet tab.")
            }
        }
        if broadcasts && wanted > capacity {
            lines.append(capacity == 1 ? "This radio can broadcast one Wi-Fi network at a time." : "This radio can broadcast \(capacity) Wi-Fi networks at a time.")
        }
        return lines.joined(separator: " ")
    }

    var body: some View {
        Section {
            Toggle("Broadcasts your Wi-Fi", isOn: broadcastBinding)
                .disabled(cannotBroadcast && !broadcasts)
            if role == .ap {
                Picker("Band", selection: bandBinding) {
                    ForEach(bandOptions, id: \.self) { band in
                        Text(bandLabel(band)).tag(band)
                    }
                }
                Picker("Channel", selection: channelBinding) {
                    ForEach(channelOptions, id: \.self) { channel in
                        Text(channelLabel(channel)).tag(channel)
                    }
                }
                Picker("Channel width", selection: widthBinding) {
                    ForEach(widthOptions, id: \.self) { width in
                        Text(widthLabel(width)).tag(width)
                    }
                }
            }
        } header: {
            Text(radioTitle(radio))
        } footer: {
            if !note.isEmpty {
                Text(note)
            }
        }
    }
}

// MARK: - Wired ports

/// What each wired port that is there is used for: the internet, one of
/// the networks, or nothing.
private struct PortsSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var setup: NetworkSetup

    private var ports: [EthernetPort] {
        let plugged: [PortStatus] = model.status?.ports ?? []
        return setup.ethernet.filter { port in plugged.contains(where: { $0.name == port.name && !$0.phone }) }
    }

    private func title(_ name: String) -> String {
        let plugged: [PortStatus] = model.status?.ports ?? []
        if let text = plugged.first(where: { $0.name == name })?.label, !text.isEmpty { return text }
        return name
    }

    private func choice(_ name: String) -> Binding<String> {
        Binding(
            get: {
                guard let port = setup.ethernet.first(where: { $0.name == name }) else { return "off" }
                switch port.role {
                case .wan: return "wan"
                case .off: return "off"
                case .lan: return "lan:" + port.network
                }
            },
            set: { value in
                guard let index = setup.ethernet.firstIndex(where: { $0.name == name }) else { return }
                if value == "wan" {
                    setup.ethernet[index].role = .wan
                } else if value == "off" {
                    setup.ethernet[index].role = .off
                } else {
                    setup.ethernet[index].role = .lan
                    setup.ethernet[index].network = String(value.dropFirst(4))
                }
            }
        )
    }

    var body: some View {
        if !ports.isEmpty {
            Section {
                ForEach(ports) { port in
                    Picker(selection: choice(port.name)) {
                        Text("Internet source").tag("wan")
                        ForEach(setup.networks) { network in
                            Text("Devices on " + network.name).tag("lan:" + network.id)
                        }
                        Text("Not used").tag("off")
                    } label: {
                        Label(title(port.name), systemImage: "cable.connector")
                    }
                }
            } header: {
                Text("Wired ports")
            }
        }
    }
}

// MARK: - What the networks share

private func leaseLabel(_ minutes: Int) -> String {
    if minutes % 1440 == 0 {
        let days = minutes / 1440
        return days == 1 ? "1 day" : "\(days) days"
    }
    if minutes % 60 == 0 {
        let hours = minutes / 60
        return hours == 1 ? "1 hour" : "\(hours) hours"
    }
    return "\(minutes) minutes"
}

private struct CommonSection: View {
    @Binding var lan: LANConfig

    private var leaseOptions: [Int] {
        var options: [Int] = [30, 60, 120, 360, 720, 1440, 2880, 10080]
        if !options.contains(lan.leaseMinutes) {
            options.append(lan.leaseMinutes)
            options.sort()
        }
        return options
    }

    var body: some View {
        Section {
            Picker("Devices keep an address for", selection: $lan.leaseMinutes) {
                ForEach(leaseOptions, id: \.self) { minutes in
                    Text(leaseLabel(minutes)).tag(minutes)
                }
            }
            LabeledContent("Local domain") {
                TextField("lan", text: $lan.domain)
                    .multilineTextAlignment(.trailing)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
            }
            Toggle("IPv6", isOn: $lan.ipv6)
            Toggle("Change addresses to avoid a clash", isOn: $lan.autoRenumber)
        } header: {
            Text("All your networks")
        } footer: {
            Text("When the network the router joins uses the same addresses as one of yours, the router moves yours to different ones and its devices get new addresses.")
        }
    }
}
