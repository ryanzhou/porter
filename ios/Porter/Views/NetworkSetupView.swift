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
    /// router, part by part: a part that was not touched follows the
    /// router.
    func rebased(from base: NetworkSetup, onto fresh: NetworkSetup) -> NetworkSetup {
        var out = fresh
        if wifi != base.wifi { out.wifi = wifi }
        if networks != base.networks { out.networks = networks }
        if radios != base.radios { out.radios = radios }
        if ethernet != base.ethernet { out.ethernet = ethernet }
        if lan != base.lan { out.lan = lan }
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
        let result: ConfigCheck? = await model.check { cfg in setup.apply(to: &cfg) }
        if Task.isCancelled { return }
        check = result
        checking = false
    }

    private func apply() {
        guard let setup = draft else { return }
        applying = true
        Task {
            let ok = await model.update { cfg in setup.apply(to: &cfg) }
            applying = false
            if ok {
                base = nil
                draft = nil
                check = nil
                sync()
            } else if let config = model.config {
                // Refused: the draft stays, to be put right.
                base = NetworkSetup(config)
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
                NoticeRow(symbol: "pencil", color: Color.secondary, text: checking ? "Checking" : "Not applied yet")
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
                            Text(wifi.ssid.isEmpty ? "Not set" : wifi.ssid)
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
                            Badge(text: "Off air", color: Color.orange)
                        }
                    }
                }
            }
            if setup.wifi.count < RouterConfig.maxWifiNetworks {
                Button("Add Wi-Fi network") { add() }
            }
        } header: {
            Text("Wi-Fi")
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
                }
                Picker("Security", selection: field(\.security, WifiSecurity.wpa2)) {
                    ForEach(WifiSecurity.allCases) { option in
                        Text(securityLabel(option)).tag(option)
                    }
                }
            } footer: {
                Text("Password: 8 to 63 characters.")
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
                Toggle("Hidden", isOn: field(\.hidden, false))
                Toggle("Keep devices apart", isOn: field(\.isolate, false))
            }
            if !isMain {
                Section {
                    Toggle("On", isOn: field(\.enabled, true))
                }
                Section {
                    Button("Remove", role: .destructive) { remove() }
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
                        Text("\(online(network.id))")
                            .foregroundStyle(.secondary)
                            .monospacedDigit()
                    }
                }
            }
            if setup.networks.count < RouterConfig.maxNetworks {
                Button("Add network") { add() }
            }
        } header: {
            Text("Networks")
        } footer: {
            Text("Each has its own addresses. Rules and routes can apply to one.")
        }
    }
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
                Text("A private address, /16 to /28, like 10.84.0.1/24.")
            }
            Section {
                Toggle("Keep apart", isOn: field(\.isolated, false))
            } footer: {
                Text("Its devices cannot reach the other networks. They can still be reached from them.")
            }
            Section {
                Picker("Route", selection: field(\.route, "")) {
                    Text("Default").tag("")
                    ForEach(model.availableActions, id: \.self) { action in
                        Text(model.actionLabel(action.raw)).tag(action.raw)
                    }
                }
            } footer: {
                Text("For devices with no route of their own.")
            }
            Section {
                LabeledContent("Wi-Fi", value: wifiNames)
                LabeledContent("Ports", value: portNames)
            }
            if !isMain {
                Section {
                    Button("Remove", role: .destructive) { remove() }
                } footer: {
                    Text("Its Wi-Fi networks and ports go to the main network.")
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
                    Text("Password: 8 to 63 characters.")
                }
                Section {
                    Picker("Network", selection: $home) {
                        ForEach(setup.networks) { network in
                            Text(network.name).tag(network.id)
                        }
                        if canAddNetwork {
                            Text("A new one").tag("+")
                        }
                    }
                    if home == "+" {
                        TextField("Network name", text: $networkName)
                        Toggle("Keep apart", isOn: $apart)
                    }
                } footer: {
                    Text(home == "+" && apart ? "For guests: online, but not able to reach your other devices." : "")
                }
            } else {
                Section {
                    TextField("Name", text: $name)
                    Toggle("Keep apart", isOn: $apart)
                } footer: {
                    Text("The router chooses its addresses. Give it a Wi-Fi network or a port afterwards.")
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
    band.isEmpty ? "Auto" : band + " GHz"
}

private func channelLabel(_ channel: Int) -> String {
    channel == 0 ? "Auto" : String(channel)
}

private func widthLabel(_ width: Int) -> String {
    width == 0 ? "Auto" : "\(width) MHz"
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
                Text("No Wi-Fi radio")
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

    private var note: String {
        var lines: [String] = []
        if isUplink {
            lines.append(broadcasts ? "Also the Wi-Fi uplink: your network follows the uplink's channel." : "The Wi-Fi uplink. Change that under Internet.")
        }
        if broadcasts && wanted > capacity {
            lines.append(capacity == 1 ? "Carries one Wi-Fi network at a time." : "Carries \(capacity) Wi-Fi networks at a time.")
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
                Picker("Width", selection: widthBinding) {
                    ForEach(widthOptions, id: \.self) { width in
                        Text(widthLabel(width)).tag(width)
                    }
                }
            }
        } header: {
            Text(radioTitle(radio))
        } footer: {
            Text(note)
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
                        Text("Internet").tag("wan")
                        ForEach(setup.networks) { network in
                            Text(network.name).tag("lan:" + network.id)
                        }
                        Text("Off").tag("off")
                    } label: {
                        Label(title(port.name), systemImage: "cable.connector")
                    }
                }
            } header: {
                Text("Wired ports")
            } footer: {
                Text("Internet: an uplink. A network: devices plugged in there join it.")
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
            Picker("Lease", selection: $lan.leaseMinutes) {
                ForEach(leaseOptions, id: \.self) { minutes in
                    Text(leaseLabel(minutes)).tag(minutes)
                }
            }
            LabeledContent("Domain") {
                TextField("lan", text: $lan.domain)
                    .multilineTextAlignment(.trailing)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
            }
            Toggle("IPv6", isOn: $lan.ipv6)
            Toggle("Move if upstream uses the same range", isOn: $lan.autoRenumber)
        } header: {
            Text("All networks")
        }
    }
}
