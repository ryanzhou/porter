import PorterKit
import SwiftUI

/// Uplinks, upstream Wi-Fi, and what each radio and wired port is used for.
struct InternetView: View {
    @EnvironmentObject private var model: AppModel
    @State private var joinTarget: JoinTarget?

    var body: some View {
        List {
            UplinksSection()
            WifiScanSection(joinTarget: $joinTarget)
            SavedNetworksSection()
            RadiosSection()
            WiredPortsSection()
            UpstreamIdentitySection()
        }
        .navigationTitle("Internet")
        .refreshable { await model.refresh() }
        .task { await loadRadios() }
        .sheet(item: $joinTarget) { target in
            NavigationStack {
                JoinNetworkSheet(target: target)
            }
            .modifier(SheetErrorAlert())
            .environmentObject(model)
        }
    }

    private func loadRadios() async {
        _ = await model.attempt { _ in try await model.refreshRadios() }
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

// MARK: - Uplinks

private struct UplinksSection: View {
    @EnvironmentObject private var model: AppModel

    private var uplinks: [UplinkStatus] { model.status?.wan ?? [] }

    var body: some View {
        Section("Uplinks") {
            if uplinks.isEmpty {
                Text("No wired port or radio is set up as an uplink.")
                    .foregroundStyle(.secondary)
            }
            ForEach(uplinks) { uplink in
                UplinkRow(uplink: uplink, active: uplink.iface == model.status?.active)
            }
        }
    }
}

private struct UplinkRow: View {
    let uplink: UplinkStatus
    let active: Bool

    private var detail: String {
        if let address = uplink.ipv4, !address.isEmpty { return address }
        return uplink.carrier ? "No address yet" : "No link"
    }

    var body: some View {
        HStack(spacing: 10) {
            StatusDot(color: internetColor(uplink.internet))
            VStack(alignment: .leading, spacing: 2) {
                Text(uplinkLabel(kind: uplink.kind, iface: uplink.iface))
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            VStack(alignment: .trailing, spacing: 2) {
                Text(internetLabel(uplink.internet))
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                if active {
                    Text("In use")
                        .font(.caption)
                        .foregroundStyle(.green)
                }
            }
        }
    }
}

// MARK: - Upstream Wi-Fi

private struct WifiScanSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var joinTarget: JoinTarget?

    @State private var scanning = false
    @State private var results: [ScanResult] = []

    private var hasStation: Bool {
        let radios: [RadioConfig] = model.config?.radios ?? []
        return radios.contains(where: { $0.role == .wan || $0.role == .apWan })
    }

    private var savedNames: [String] {
        let networks: [SavedNetwork] = model.config?.wan.networks ?? []
        return networks.map { $0.ssid }
    }

    var body: some View {
        Section {
            if hasStation {
                scanButton
                ForEach(results) { result in
                    ScanResultRow(result: result, saved: savedNames.contains(result.ssid)) {
                        joinTarget = JoinTarget(ssid: result.ssid, secured: result.security != "open", other: false)
                    }
                }
                Button("Other…") {
                    joinTarget = JoinTarget(ssid: "", secured: true, other: true)
                }
            } else {
                Text("No radio is set to connect to Wi-Fi networks. Under Radios below, give one the upstream role.")
                    .foregroundStyle(.secondary)
            }
        } header: {
            Text("Wi-Fi networks")
        } footer: {
            Text(hasStation ? "A scan takes about 10 seconds." : "")
        }
    }

    private var scanButton: some View {
        Button {
            scan()
        } label: {
            HStack(spacing: 12) {
                Text(scanning ? "Scanning…" : "Scan")
                if scanning {
                    ProgressView()
                }
            }
        }
        .disabled(scanning)
    }

    private func scan() {
        scanning = true
        Task {
            let found: [ScanResult]? = await model.fetch { try await $0.scanWifi() }
            scanning = false
            if let found {
                results = strongestPerName(found)
            }
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

private struct ScanResultRow: View {
    let result: ScanResult
    let saved: Bool
    let onTap: () -> Void

    private var unsupported: Bool {
        result.security == "enterprise" || result.security == "wep"
    }

    private var note: String {
        if result.security == "enterprise" { return "Enterprise sign-in is not supported" }
        if result.security == "wep" { return "WEP is not supported" }
        return saved ? "Saved" : ""
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
        Section {
            if networks.isEmpty {
                Text("None")
                    .foregroundStyle(.secondary)
            }
            ForEach(networks) { network in
                HStack {
                    Text(network.ssid)
                    Spacer()
                    Text(note(for: network.ssid))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                .swipeActions {
                    Button("Forget", role: .destructive) { forget(network.ssid) }
                }
            }
        } header: {
            Text("Saved networks")
        } footer: {
            Text("The router joins saved networks on its own, the most recently added first. Swipe to forget one.")
        }
    }

    private func forget(_ ssid: String) {
        Task { await model.attempt { try await $0.forgetWifi(ssid: ssid) } }
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
                }
            }
            if target.secured {
                Section {
                    SecureField("Password", text: $password)
                } footer: {
                    Text(target.other ? "Leave the password empty for an open network. Otherwise 8 to 63 characters." : "8 to 63 characters.")
                }
            } else {
                Section {
                    Text("This network has no password. Others nearby can see traffic that is not encrypted by a VPN or by the site itself.")
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

// MARK: - Radios

private func radioRoleLabel(_ role: RadioRole) -> String {
    switch role {
    case .off: return "Off"
    case .ap: return "Access point"
    case .wan: return "Upstream Wi-Fi"
    case .apWan: return "Both"
    }
}

private func bandLabel(_ band: String) -> String {
    band.isEmpty ? "Automatic" : band + " GHz"
}

private func channelLabel(_ channel: Int) -> String {
    channel == 0 ? "Auto" : String(channel)
}

private func widthLabel(_ width: Int) -> String {
    width == 0 ? "Auto" : "\(width) MHz"
}

private struct RadiosSection: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        Section {
            if model.radios.isEmpty {
                Text("No Wi-Fi radio found.")
                    .foregroundStyle(.secondary)
            }
            ForEach(model.radios) { radio in
                RadioRows(radio: radio)
            }
        } header: {
            Text("Radios")
        } footer: {
            Text("With one radio doing both jobs, the access point has to use the upstream network's channel, so band and channel cannot be chosen. A second adapter lets each radio do one job.")
        }
    }
}

private struct RadioRows: View {
    @EnvironmentObject private var model: AppModel
    let radio: RadioInfo

    private var setting: RadioConfig? {
        model.config?.radios.first(where: { $0.id == radio.id })
    }

    private var role: RadioRole { setting?.role ?? .off }

    private var subtitle: String {
        radio.present ? radio.id : radio.id + " · not plugged in"
    }

    private var roleOptions: [RadioRole] {
        var options: [RadioRole] = [.off]
        if radio.ap { options.append(.ap) }
        if radio.sta { options.append(.wan) }
        if radio.apSta { options.append(.apWan) }
        if !options.contains(role) { options.append(role) }
        return options
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

    private func edit(_ change: @escaping (inout RadioConfig) -> Void) {
        let id = radio.id
        Task {
            await model.update { cfg in
                if let index = cfg.radios.firstIndex(where: { $0.id == id }) {
                    change(&cfg.radios[index])
                }
            }
        }
    }

    private var roleBinding: Binding<RadioRole> {
        Binding(
            get: { role },
            set: { value in edit { $0.role = value } }
        )
    }

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

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(radio.product.isEmpty ? "Wi-Fi radio" : radio.product)
            Text(subtitle)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        Picker("Role", selection: roleBinding) {
            ForEach(roleOptions) { option in
                Text(radioRoleLabel(option)).tag(option)
            }
        }
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
    }
}

// MARK: - Wired ports

private func portRoleLabel(_ role: PortRole) -> String {
    switch role {
    case .wan: return "Internet"
    case .lan: return "Local network"
    case .off: return "Off"
    }
}

private struct WiredPortsSection: View {
    @EnvironmentObject private var model: AppModel

    private var ports: [EthernetPort] { model.config?.ethernet ?? [] }

    private func roleBinding(_ name: String) -> Binding<PortRole> {
        Binding(
            get: { ports.first(where: { $0.name == name })?.role ?? .off },
            set: { value in
                Task {
                    await model.update { cfg in
                        if let index = cfg.ethernet.firstIndex(where: { $0.name == name }) {
                            cfg.ethernet[index].role = value
                        }
                    }
                }
            }
        )
    }

    var body: some View {
        Section {
            if ports.isEmpty {
                Text("No wired port found.")
                    .foregroundStyle(.secondary)
            }
            ForEach(ports) { port in
                Picker(port.name, selection: roleBinding(port.name)) {
                    ForEach(PortRole.allCases) { option in
                        Text(portRoleLabel(option)).tag(option)
                    }
                }
            }
        } header: {
            Text("Wired ports")
        } footer: {
            Text("\"Internet\" takes the connection from whatever the port is plugged into. \"Local network\" shares the router's connection with what is plugged in.")
        }
    }
}

// MARK: - Upstream identity

private func macModeLabel(_ mode: MACMode) -> String {
    switch mode {
    case .hardware: return "Hardware"
    case .random: return "Random"
    case .custom: return "Custom"
    }
}

private struct UpstreamIdentitySection: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        Section {
            MACPolicyRows(title: "Wi-Fi address", kind: "wifi", policyPath: \.wan.wifiMac)
            MACPolicyRows(title: "Wired address", kind: "eth", policyPath: \.wan.ethMac)
            Toggle("Prefer Wi-Fi over wired", isOn: model.binding(\.wan.preferWifi, fallback: false))
        } header: {
            Text("Upstream identity")
        } footer: {
            Text("The hardware (MAC) address the router shows to upstream networks. A random address stays the same until you ask for a new one; a new one makes the router look like a different device. The last switch decides which uplink is used when both are connected.")
        }
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
