import PorterKit
import SwiftUI

/// Where the internet comes from: the sources in the order the router
/// tries them, and the upstream Wi-Fi networks to join.
struct InternetView: View {
    @EnvironmentObject private var model: AppModel
    @State private var joinTarget: JoinTarget?
    @State private var results: [ScanResult] = []
    @State private var scanning = false
    @State private var switching = false

    private var uplinkRadio: RadioInfo? { model.uplinkRadio }

    /// The radio that would join, were Wi-Fi turned on as a source: a
    /// spare one, else an adapter rather than the built-in radio.
    private var candidate: RadioInfo? {
        let able: [RadioInfo] = model.radios.filter { $0.present && $0.sta }
        if let spare = able.first(where: { model.role(of: $0) == .off }) { return spare }
        if let adapter = able.first(where: { $0.id != "builtin" }) { return adapter }
        return able.first
    }

    private var offNote: String {
        guard let radio = candidate else { return "The router has no Wi-Fi radio that can join a network." }
        let others: [RadioInfo] = model.radios.filter { $0.present && $0.id != radio.id && model.role(of: $0) == .ap }
        if let other = others.first {
            return "\(radioTitle(radio)) will join it. Your Wi-Fi stays on \(radioTitle(other))."
        }
        return "\(radioTitle(radio)) will join it and keep broadcasting your Wi-Fi. Your devices drop off for a moment while it changes over."
    }

    private var stations: [StationStatus] { model.status?.stas ?? [] }

    private var savedNames: [String] {
        let networks: [SavedNetwork] = model.config?.wan.networks ?? []
        return networks.map { $0.ssid }
    }

    private func note(for result: ScanResult) -> String {
        if result.security == "enterprise" { return "Networks that need a username are not supported" }
        if result.security == "wep" { return "WEP networks are not supported" }
        if stations.contains(where: { $0.state == "connected" && $0.ssid == result.ssid }) { return "Connected" }
        if stations.contains(where: { $0.state == "connecting" && $0.ssid == result.ssid }) { return "Joining" }
        if let refused = stations.first(where: { $0.failed == result.ssid }) { return joinFailureLabel(refused.failReason) }
        return savedNames.contains(result.ssid) ? "Saved" : ""
    }

    var body: some View {
        List {
            SourcesSection()
            if uplinkRadio != nil {
                nearbySection
                SavedNetworksSection()
                Section {
                    NavigationLink("Wi-Fi source") { WifiUplinkPage() }
                }
            } else {
                offSection
            }
            Section {
                NavigationLink("Hardware addresses") { UpstreamIdentityPage() }
            }
        }
        .navigationTitle("Internet")
        .toolbar { EditButton() }
        .refreshable {
            await model.refresh()
            await scan(quick: false)
        }
        .task(id: uplinkRadio?.id) { await firstScan() }
        .sheet(item: $joinTarget) { target in
            NavigationStack {
                JoinNetworkSheet(target: target)
            }
            .modifier(SheetErrorAlert())
            .environmentObject(model)
        }
    }

    private var nearbySection: some View {
        Section {
            ForEach(results) { result in
                ScanResultRow(result: result, note: note(for: result)) {
                    joinTarget = JoinTarget(ssid: result.ssid, secured: result.security != "open", other: false)
                }
            }
            if results.isEmpty {
                Text(scanning ? "Looking for networks" : "No Wi-Fi networks in range")
                    .foregroundStyle(.secondary)
            }
            Button("Other…") {
                joinTarget = JoinTarget(ssid: "", secured: true, other: true)
            }
        } header: {
            HStack {
                Text("Wi-Fi networks nearby")
                Spacer()
                if scanning {
                    ProgressView()
                } else {
                    Button("Scan") {
                        Task { await scan(quick: false) }
                    }
                    .font(.footnote)
                }
            }
        }
    }

    /// Wi-Fi is not a source yet: one tap makes it one.
    private var offSection: some View {
        Section {
            Button {
                turnOn()
            } label: {
                HStack(spacing: 12) {
                    Label("Join a Wi-Fi network", systemImage: "wifi")
                    Spacer()
                    if switching {
                        ProgressView()
                    }
                }
            }
            .disabled(switching || candidate == nil)
        } footer: {
            Text(offNote)
        }
    }

    private func turnOn() {
        guard let radio = candidate else { return }
        switching = true
        Task {
            await model.setWifiUplink(radio: radio.id)
            switching = false
        }
    }

    /// What the router already knows is shown at once; a fresh scan
    /// follows. A radio that has only just become the uplink needs a
    /// moment before it answers at all.
    private func firstScan() async {
        if model.radios.isEmpty {
            await model.quietly { _ in try await model.refreshRadios() }
        }
        guard uplinkRadio != nil else {
            results = []
            return
        }
        for attemptNumber in 0..<4 {
            if attemptNumber > 0 {
                try? await Task.sleep(nanoseconds: 1_500_000_000)
            }
            if Task.isCancelled { return }
            let found: [ScanResult]? = await model.fetchQuietly { try await $0.scanWifi(quick: true) }
            if let found {
                if !found.isEmpty { results = strongestPerName(found) }
                break
            }
        }
        await scan(quick: false)
    }

    private func scan(quick: Bool) async {
        guard uplinkRadio != nil, !scanning else { return }
        scanning = true
        let found: [ScanResult]? = await model.fetchQuietly { try await $0.scanWifi(quick: quick) }
        scanning = false
        if let found {
            results = strongestPerName(found)
        }
    }

    private func strongestPerName(_ found: [ScanResult]) -> [ScanResult] {
        var best: [String: ScanResult] = [:]
        for result in found where !result.ssid.isEmpty {
            if let existing = best[result.ssid], existing.signal >= result.signal { continue }
            best[result.ssid] = result
        }
        return best.values.sorted { $0.signal > $1.signal }
    }
}

/// The network the join sheet is asking about.
private struct JoinTarget: Identifiable {
    let id = UUID()
    var ssid: String
    var secured: Bool
    /// The name is typed in rather than picked from a scan.
    var other: Bool
}

// MARK: - Sources

/// One way to the internet, as it stands in the order.
private struct Source: Identifiable {
    var key: String
    /// "eth", "wifi" or "phone".
    var kind: String
    var uplink: UplinkStatus?
    /// The wired port, for "eth".
    var port: String?
    var id: String { key }
}

/// The ways to the internet in the order the router tries them. Dragging
/// one changes the order at once.
private struct SourcesSection: View {
    @EnvironmentObject private var model: AppModel

    private var sources: [Source] {
        guard let config = model.config else { return [] }
        let uplinks: [UplinkStatus] = model.status?.wan ?? []
        let ports: [PortStatus] = model.status?.ports ?? []
        var out: [Source] = []
        for key in config.wan.order {
            let uplink: UplinkStatus? = uplinks.first(where: { $0.key == key })
            if key == RouterConfig.uplinkWifi {
                out.append(Source(key: key, kind: "wifi", uplink: uplink, port: nil))
            } else if key == RouterConfig.uplinkPhone {
                out.append(Source(key: key, kind: "phone", uplink: uplink, port: nil))
            } else if key.hasPrefix("eth:") {
                // A wired port counts while it is plugged into the router
                // and set to be an uplink.
                let name = String(key.dropFirst(4))
                let isUplink: Bool = config.ethernet.contains(where: { $0.name == name && $0.role == .wan })
                let present: Bool = ports.contains(where: { $0.name == name })
                if isUplink && present {
                    out.append(Source(key: key, kind: "eth", uplink: uplink, port: name))
                }
            }
        }
        return out
    }

    private func isActive(_ source: Source) -> Bool {
        guard let uplink = source.uplink, let active = model.status?.active else { return false }
        return uplink.iface == active
    }

    var body: some View {
        Section {
            ForEach(sources) { source in
                if let port = source.port {
                    NavigationLink {
                        WiredSourcePage(port: port)
                    } label: {
                        SourceRow(source: source, active: isActive(source))
                    }
                } else {
                    SourceRow(source: source, active: isActive(source))
                }
            }
            .onMove { from, to in move(from, to) }
        } header: {
            Text("Internet sources")
        } footer: {
            Text("The router gets online through the first source in this list that works, and moves to the next when it stops working. Tap Edit to change the order.")
        }
    }

    private func move(_ from: IndexSet, _ to: Int) {
        var keys: [String] = sources.map { $0.key }
        keys.move(fromOffsets: from, toOffset: to)
        // Sources that are not shown keep a place, after the ones that are.
        let all: [String] = model.config?.wan.order ?? []
        let hidden: [String] = all.filter { !keys.contains($0) }
        let order: [String] = keys + hidden
        Task {
            await model.update { cfg in cfg.wan.order = order }
        }
    }
}

/// One way out. The one in use has the filled icon.
private struct SourceRow: View {
    @EnvironmentObject private var model: AppModel
    let source: Source
    let active: Bool

    private var station: StationStatus? {
        guard source.kind == "wifi" else { return nil }
        let stations: [StationStatus] = model.status?.stas ?? []
        if let uplink = source.uplink, let match = stations.first(where: { $0.iface == uplink.iface }) {
            return match
        }
        return stations.first
    }

    private var uplink: UplinkStatus? { source.uplink }

    private var linked: Bool { uplink?.carrier ?? false }

    private var title: String {
        switch source.kind {
        case "wifi":
            if let network = station?.ssid, !network.isEmpty { return network }
            return "Wi-Fi"
        case "phone":
            if let label = uplink?.label, !label.isEmpty { return label }
            return "Phone on USB"
        default:
            return "Ethernet"
        }
    }

    /// The line under the name: the address, or what is missing.
    private var detail: String {
        if linked {
            if let address = uplink?.ipv4, !address.isEmpty { return address }
            return "Waiting for an address"
        }
        switch source.kind {
        case "wifi":
            if let station {
                if let failed = station.failed, !failed.isEmpty {
                    return joinFailureText(ssid: failed, reason: station.failReason)
                }
                switch station.state {
                case "connecting": return "Joining"
                case "scanning": return "Looking for a saved network"
                default: return "No network joined"
                }
            }
            return "Off"
        case "phone":
            if uplink == nil { return "Not plugged in" }
            return "Turn on the phone's hotspot. If an iPhone asks whether to trust the router, tap Trust."
        default:
            return "No cable"
        }
    }

    /// Nothing plugged in is not a fault: grey, and no second label.
    private var color: Color {
        guard linked, let uplink else { return Color.gray }
        return internetColor(uplink.internet)
    }

    var body: some View {
        HStack(spacing: 12) {
            Glyph(symbol: uplinkSymbol(kind: source.kind), color: color, filled: active)
            VStack(alignment: .leading, spacing: 2) {
                Text(title)
                    .lineLimit(1)
                HStack(spacing: 6) {
                    Text(detail)
                        .font(linked ? Font.caption.monospaced() : Font.caption)
                        .foregroundStyle(.secondary)
                    if uplink?.hasV6 == true {
                        Badge(text: "IPv6", color: Color.accentColor)
                    }
                    if linked, let freq = station?.freq, freq > 0 {
                        BandBadge(freq: freq)
                    }
                }
            }
            Spacer(minLength: 8)
            if linked, let uplink {
                VStack(alignment: .trailing, spacing: 4) {
                    Text(internetLabel(uplink.internet))
                        .font(.subheadline)
                        .foregroundStyle(color)
                    if station?.state == "connected", let signal = station?.signal {
                        SignalBars(rssi: signal)
                    }
                }
            }
        }
    }
}

/// A wired uplink: how it gets its addresses.
private struct WiredSourcePage: View {
    @EnvironmentObject private var model: AppModel
    let port: String

    private var setup: IPSetup {
        let ports: [EthernetPort] = model.config?.ethernet ?? []
        return ports.first(where: { $0.name == port })?.ip ?? IPSetup()
    }

    var body: some View {
        IPSetupPage(title: "Ethernet", initial: setup) { next in
            let name = port
            return await model.update { cfg in
                if let index = cfg.ethernet.firstIndex(where: { $0.name == name }) {
                    cfg.ethernet[index].ip = next
                }
            }
        }
    }
}

// MARK: - Upstream Wi-Fi

private struct ScanResultRow: View {
    let result: ScanResult
    let note: String
    let onTap: () -> Void

    private var unsupported: Bool {
        result.security == "enterprise" || result.security == "wep"
    }

    var body: some View {
        Button {
            onTap()
        } label: {
            HStack(spacing: 10) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(result.ssid)
                    if !note.isEmpty {
                        Text(note)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                Spacer()
                BandBadge(freq: result.freq)
                if result.security != "open" {
                    Image(systemName: "lock.fill")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                SignalBars(rssi: result.signal)
            }
        }
        .foregroundStyle(.primary)
        .disabled(unsupported)
    }
}

/// Which radio joins upstream networks, and turning that off.
private struct WifiUplinkPage: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var working = false

    private var able: [RadioInfo] { model.radios.filter { $0.present && $0.sta } }

    private var current: RadioInfo? { model.uplinkRadio }

    private var selection: Binding<String> {
        Binding(
            get: { current?.id ?? "" },
            set: { id in choose(id) }
        )
    }

    private var note: String {
        guard let current else { return "" }
        if model.role(of: current) == .apWan {
            return "This radio also broadcasts your Wi-Fi. Your devices drop off for a moment whenever it joins a different network, and the two share its speed. A second radio avoids both."
        }
        return ""
    }

    var body: some View {
        Form {
            Section {
                Picker("Radio", selection: selection) {
                    ForEach(able) { radio in
                        Text(radioTitle(radio)).tag(radio.id)
                    }
                }
                .pickerStyle(.inline)
                .labelsHidden()
            } header: {
                Text("Radio that joins Wi-Fi")
            } footer: {
                if !note.isEmpty {
                    Text(note)
                }
            }
            Section("Hardware address") {
                MACPolicyRows(title: "Address", kind: "wifi", policyPath: \.wan.wifiMac)
            }
            Section {
                Button("Stop using Wi-Fi", role: .destructive) { choose("") }
            } footer: {
                Text("The router leaves the Wi-Fi network it has joined and gets online through its other sources only. Saved networks are kept.")
            }
        }
        .disabled(working)
        .navigationTitle("Wi-Fi source")
        .navigationBarTitleDisplayMode(.inline)
    }

    private func choose(_ id: String) {
        guard id != (current?.id ?? "") else { return }
        working = true
        Task {
            let ok = await model.setWifiUplink(radio: id)
            working = false
            if ok && id.isEmpty {
                dismiss()
            }
        }
    }
}

private struct SavedNetworksSection: View {
    @EnvironmentObject private var model: AppModel

    private var networks: [SavedNetwork] {
        let saved: [SavedNetwork] = model.config?.wan.networks ?? []
        return saved.sorted { $0.priority > $1.priority }
    }

    private var stations: [StationStatus] { model.status?.stas ?? [] }

    private func note(for ssid: String) -> String {
        if stations.contains(where: { $0.state == "connected" && $0.ssid == ssid }) { return "Connected" }
        if let refused = stations.first(where: { $0.failed == ssid }) { return joinFailureLabel(refused.failReason) }
        return ""
    }

    var body: some View {
        if !networks.isEmpty {
            savedSection
        }
    }

    private var savedSection: some View {
        Section {
            ForEach(networks) { network in
                NavigationLink {
                    IPSetupPage(title: network.ssid, initial: network.ip ?? IPSetup()) { setup in
                        await saveAddresses(network.ssid, setup)
                    }
                } label: {
                    HStack {
                        Text(network.ssid)
                        if network.ip?.isAutomatic == false {
                            Badge(text: "Manual address")
                        }
                        Spacer()
                        Text(note(for: network.ssid))
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                    }
                }
                .swipeActions {
                    Button("Forget", role: .destructive) { forget(network.ssid) }
                }
            }
        } header: {
            Text("Saved Wi-Fi networks")
        } footer: {
            Text("The router joins these again by itself when they are in range.")
        }
    }

    private func forget(_ ssid: String) {
        Task { await model.attempt { try await $0.forgetWifi(ssid: ssid) } }
    }

    private func saveAddresses(_ ssid: String, _ setup: IPSetup) async -> Bool {
        await model.update { cfg in
            if let index = cfg.wan.networks.firstIndex(where: { $0.ssid == ssid }) {
                cfg.wan.networks[index].ip = setup
            }
        }
    }
}

// MARK: - Addresses

/// How an uplink gets its addresses: from the network, or typed in, for
/// IPv4 and IPv6 separately.
struct IPSetupPage: View {
    @Environment(\.dismiss) private var dismiss
    let title: String
    let initial: IPSetup
    let save: (IPSetup) async -> Bool

    @State private var draft = IPSetup()
    @State private var dns4: String = ""
    @State private var dns6: String = ""
    @State private var loaded = false
    @State private var saving = false

    /// The draft with the DNS fields folded in.
    private var result: IPSetup {
        var out = draft
        out.v4.dns = servers(dns4)
        out.v6.dns = servers(dns6)
        return out
    }

    private func servers(_ text: String) -> [String] {
        text.split(whereSeparator: { $0 == "," || $0 == " " || $0.isNewline }).map { String($0) }
    }

    private func complete(_ family: IPFamilySetup) -> Bool {
        !family.isStatic || (!family.address.isEmpty && !family.gateway.isEmpty)
    }

    private var canSave: Bool {
        loaded && !saving && result != initial && complete(draft.v4) && complete(draft.v6)
    }

    var body: some View {
        Form {
            IPFamilySection(name: "IPv4", family: $draft.v4, dns: $dns4, address: "192.168.1.50/24", gateway: "192.168.1.1", server: "1.1.1.1")
            IPFamilySection(name: "IPv6", family: $draft.v6, dns: $dns6, address: "2001:db8::50/64", gateway: "fe80::1", server: "2606:4700:4700::1111")
        }
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { commit() }
                    .disabled(!canSave)
            }
        }
        .onAppear { load() }
    }

    private func load() {
        guard !loaded else { return }
        loaded = true
        draft = initial
        dns4 = initial.v4.dns.joined(separator: ", ")
        dns6 = initial.v6.dns.joined(separator: ", ")
    }

    private func commit() {
        let setup = result
        saving = true
        Task {
            let ok = await save(setup)
            saving = false
            if ok {
                dismiss()
            }
        }
    }
}

private struct IPFamilySection: View {
    let name: String
    @Binding var family: IPFamilySetup
    @Binding var dns: String
    let address: String
    let gateway: String
    let server: String

    var body: some View {
        Section(name) {
            Picker(name, selection: $family.mode) {
                Text("Automatic").tag("auto")
                Text("Manual").tag("static")
            }
            .pickerStyle(.segmented)
            .listRowInsets(EdgeInsets(top: 10, leading: 12, bottom: 10, trailing: 12))
            if family.isStatic {
                AddressField(label: "Address", text: $family.address, example: address)
                AddressField(label: "Gateway", text: $family.gateway, example: gateway)
                AddressField(label: "DNS", text: $dns, example: server)
            }
        }
    }
}

/// A labelled field for an address, with an example where the value goes.
private struct AddressField: View {
    let label: String
    @Binding var text: String
    let example: String

    var body: some View {
        LabeledContent(label) {
            TextField(label, text: $text, prompt: Text(verbatim: example))
                .multilineTextAlignment(.trailing)
                .font(.body.monospaced())
                .keyboardType(.numbersAndPunctuation)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        }
    }
}

private struct JoinNetworkSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let target: JoinTarget

    @State private var typedName: String = ""
    @State private var password: String = ""
    @State private var hidden = false
    @State private var joining = false

    private var name: String { target.other ? typedName : target.ssid }

    private var valid: Bool {
        guard validNetworkName(name) else { return false }
        if target.other { return password.isEmpty || validPassphrase(password) }
        return !target.secured || validPassphrase(password)
    }

    var body: some View {
        Form {
            if target.other {
                Section {
                    TextField("Network name", text: $typedName)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    Toggle("Hidden network", isOn: $hidden)
                } footer: {
                    if let problem = networkNameProblem(typedName) {
                        Text(problem)
                            .foregroundStyle(.red)
                    }
                }
            }
            if target.secured {
                Section {
                    SecureField("Password", text: $password, prompt: Text(target.other ? "Password, if it has one" : "Password"))
                } footer: {
                    if let problem = passphraseProblem(password) {
                        Text(problem)
                            .foregroundStyle(.red)
                    }
                }
            } else {
                Section {
                    Label("This network has no password. Others nearby can see any of your traffic that is not encrypted.", systemImage: "lock.open")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .navigationTitle(target.other ? "Other network" : target.ssid)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel") { dismiss() }
            }
            ToolbarItem(placement: .confirmationAction) {
                Button("Join") { join() }
                    .disabled(!valid || joining)
            }
        }
        .onAppear { prefill() }
    }

    private func prefill() {
        let saved: [SavedNetwork] = model.config?.wan.networks ?? []
        if let known = saved.first(where: { $0.ssid == target.ssid }) {
            password = known.psk
        }
    }

    private func join() {
        let network = SavedNetwork(ssid: name, psk: target.secured ? password : "", hidden: target.other && hidden)
        joining = true
        Task {
            let ok = await model.attempt { try await $0.connectWifi(network) }
            joining = false
            if ok {
                dismiss()
            }
        }
    }
}

// MARK: - Upstream identity

private func macModeLabel(_ mode: MACMode) -> String {
    switch mode {
    case .hardware: return "Built-in"
    case .random: return "Random"
    case .custom: return "Custom"
    }
}

/// The hardware (MAC) address the router shows upstream, per kind of uplink.
private struct UpstreamIdentityPage: View {
    var body: some View {
        Form {
            Section("Wi-Fi") {
                MACPolicyRows(title: "Address", kind: "wifi", policyPath: \.wan.wifiMac)
            }
            Section {
                MACPolicyRows(title: "Address", kind: "eth", policyPath: \.wan.ethMac)
            } header: {
                Text("Wired")
            } footer: {
                Text("The hardware address is how the network the router joins tells devices apart. With a new one the router looks like a device it has not seen before, so you may have to sign in there again.")
            }
        }
        .navigationTitle("Hardware addresses")
        .navigationBarTitleDisplayMode(.inline)
    }
}

private struct MACPolicyRows: View {
    @EnvironmentObject private var model: AppModel
    let title: String
    let kind: String
    let policyPath: WritableKeyPath<RouterConfig, MACPolicy>

    @State private var enteringCustom = false
    @State private var custom: String = ""

    private var policy: MACPolicy? { model.config?[keyPath: policyPath] }

    private var shownMode: MACMode {
        enteringCustom ? .custom : (policy?.mode ?? .hardware)
    }

    private var customReady: Bool {
        validUnicastMAC(custom) && custom.lowercased() != (policy?.address ?? "")
    }

    private var modeBinding: Binding<MACMode> {
        Binding(
            get: { shownMode },
            set: { mode in choose(mode) }
        )
    }

    var body: some View {
        Picker(title, selection: modeBinding) {
            ForEach(MACMode.allCases) { mode in
                Text(macModeLabel(mode)).tag(mode)
            }
        }
        if shownMode == .random {
            LabeledContent("Current address", value: policy?.address ?? "")
            Button("New random address") { newRandom() }
        }
        if shownMode == .custom {
            TextField("02:00:00:00:00:01", text: $custom)
                .font(.body.monospaced())
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            Button("Use this address") { apply(mode: .custom, address: custom.lowercased()) }
                .disabled(!customReady)
        }
    }

    private func choose(_ mode: MACMode) {
        switch mode {
        case .hardware:
            enteringCustom = false
            apply(mode: .hardware, address: nil)
        case .random:
            enteringCustom = false
            newRandom()
        case .custom:
            custom = policy?.address ?? ""
            enteringCustom = true
        }
    }

    private func newRandom() {
        let kind = self.kind
        Task { await model.attempt { try await $0.newMAC(kind: kind) } }
    }

    private func apply(mode: MACMode, address: String?) {
        let path = policyPath
        Task {
            let ok = await model.update { cfg in
                cfg[keyPath: path].mode = mode
                cfg[keyPath: path].address = address
            }
            if ok && mode == .custom {
                enteringCustom = false
            }
        }
    }
}
