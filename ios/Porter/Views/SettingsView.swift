import PorterKit
import SwiftUI
import UIKit

struct SettingsView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        List {
            RouterSettingsSection()
            Section {
                SettingsLink(symbol: "arrow.down.circle.fill", title: "Software", value: model.status?.version ?? "") { SoftwarePage() }
                SettingsLink(symbol: "iphone", title: "Paired phones") { PairedPhonesPage() }
                SettingsLink(symbol: "clock", title: "Activity") { ActivityPage() }
                SettingsLink(symbol: "text.alignleft", title: "Logs") { LogsPage() }
            }
            SystemSection()
            AboutSection()
        }
        .navigationTitle("Settings")
        .refreshable { await model.refresh() }
    }
}

private struct RouterSettingsSection: View {
    @EnvironmentObject private var model: AppModel

    private var countryName: String {
        let code = model.config?.system.country ?? ""
        return CountryChoice.all.first(where: { $0.code == code })?.name ?? code
    }

    private func onOff(_ value: Bool?) -> String {
        value == true ? "On" : "Off"
    }

    var body: some View {
        Section {
            SettingsLink(symbol: "wifi", title: "Wi-Fi & networks", value: model.config?.wifi.first?.ssid ?? "") { NetworkSetupPage() }
            SettingsLink(symbol: "hand.raised.fill", title: "Ad blocking", value: onOff(model.config?.dns.adblock.enabled)) { AdblockPage() }
            SettingsLink(symbol: "server.rack", title: "DNS") { DNSPage() }
            SettingsLink(symbol: "house.fill", title: "From outside", value: inboundSummary(model.config)) { InboundPage() }
        }
        Section {
            SettingsLink(symbol: "wifi.router.fill", title: "Router name", value: model.config?.system.name ?? "") { RouterNamePage() }
            SettingsLink(symbol: "globe.europe.africa.fill", title: "Country", value: countryName) { CountryPage() }
            SettingsLink(symbol: "terminal.fill", title: "SSH", value: onOff(model.config?.system.ssh.enabled)) { SSHPage() }
            SettingsLink(symbol: "speedometer", title: "Speed test server") { SpeedTestServerPage() }
        }
        Section {
            Toggle("Connect over your network too", isOn: model.networkControl)
                .disabled(onlyWayLeft)
        } footer: {
            Text(networkControlNote)
        }
    }

    /// With Bluetooth unusable on this phone the network is the only way
    /// to the router, and switching it off from here would lock the app
    /// out.
    private var onlyWayLeft: Bool {
        model.central.unavailableReason != nil && model.config?.system.bluetoothOnly != true
    }

    private var networkControlNote: String {
        let note: String = "With this on, the app also reaches the router whenever this phone is on one of your networks, which is faster than Bluetooth and works wherever your Wi-Fi does. Pairing a new phone always uses Bluetooth."
        if onlyWayLeft {
            return note + " To switch it off, turn Bluetooth on first: it would be the only way left to the router."
        }
        return note
    }
}

/// A settings row: icon, name, and the current value where one fits.
private struct SettingsLink<Destination: View>: View {
    let symbol: String
    let title: String
    var value: String = ""
    @ViewBuilder var destination: () -> Destination

    var body: some View {
        NavigationLink {
            destination()
        } label: {
            HStack(spacing: 12) {
                Glyph(symbol: symbol, size: 30)
                Text(title)
                Spacer(minLength: 8)
                Text(value)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
        }
    }
}

// MARK: - Editing part of the configuration

/// A form that edits a copy of one part of the configuration and writes
/// it back when Save is tapped. The copy follows the router's version
/// until the user changes something.
private struct ConfigEditor<Value: Equatable, Fields: View>: View {
    @EnvironmentObject private var model: AppModel

    private let title: String
    private let keyPath: WritableKeyPath<RouterConfig, Value>
    private let valid: (Value) -> Bool
    private let fields: (Binding<Value>) -> Fields

    @State private var draft: Value? = nil
    @State private var original: Value? = nil
    @State private var saving = false

    init(
        _ title: String,
        _ keyPath: WritableKeyPath<RouterConfig, Value>,
        valid: @escaping (Value) -> Bool,
        @ViewBuilder fields: @escaping (Binding<Value>) -> Fields
    ) {
        self.title = title
        self.keyPath = keyPath
        self.valid = valid
        self.fields = fields
    }

    private var canSave: Bool {
        guard let draft, !saving else { return false }
        return draft != original && valid(draft)
    }

    private func draftBinding(_ current: Value) -> Binding<Value> {
        Binding(
            get: { draft ?? current },
            set: { draft = $0 }
        )
    }

    var body: some View {
        Form {
            if let current = draft {
                fields(draftBinding(current))
            }
        }
        .navigationTitle(title)
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

    private func sync() {
        guard let config = model.config else { return }
        let current: Value = config[keyPath: keyPath]
        if draft == nil || draft == original {
            draft = current
        }
        original = current
    }

    private func save() {
        guard let value = draft else { return }
        let path = keyPath
        saving = true
        Task {
            let ok = await model.update { cfg in
                cfg[keyPath: path] = value
            }
            saving = false
            if ok {
                draft = nil
                sync()
            }
        }
    }
}

// MARK: - Router name

private struct RouterNamePage: View {
    var body: some View {
        ConfigEditor("Router name", \RouterConfig.system.name, valid: validRouterName) { (name: Binding<String>) in
            RouterNameFields(name: name)
        }
    }
}

private struct RouterNameFields: View {
    @Binding var name: String

    var body: some View {
        Section {
            TextField("Name", text: $name)
        } footer: {
            if let problem = shortNameProblem(name) {
                Text(problem)
                    .foregroundStyle(.red)
            }
        }
    }
}

// MARK: - Country

private struct CountryPage: View {
    var body: some View {
        ConfigEditor(
            "Country",
            \RouterConfig.system.country,
            valid: { (code: String) -> Bool in code.count == 2 }
        ) { (country: Binding<String>) in
            CountryFields(country: country)
        }
    }
}

private struct CountryFields: View {
    @Binding var country: String

    var body: some View {
        Section {
            CountryPicker(selection: $country)
        } footer: {
            Text("The country decides which Wi-Fi channels the router is allowed to use.")
        }
    }
}

// MARK: - DNS

private struct DNSPage: View {
    var body: some View {
        ConfigEditor(
            "DNS",
            \RouterConfig.dns,
            valid: { (dns: DNSConfig) -> Bool in dns.upstreams.count <= 8 }
        ) { (dns: Binding<DNSConfig>) in
            DNSFields(dns: dns)
        }
    }
}

private struct DNSFields: View {
    @Binding var dns: DNSConfig

    /// The router takes eight servers at most: said once there are more.
    private var serversNote: String {
        if dns.upstreams.count > 8 {
            return "The router uses at most 8 DNS servers. Remove some to save."
        }
        return "Devices that go online through a VPN use the DNS servers that come with the VPN, if it has any. An encrypted server is written as tls://1.1.1.1#cloudflare-dns.com or https://cloudflare-dns.com/dns-query#1.1.1.1."
    }

    var body: some View {
        StringListEditor(
            title: "DNS servers",
            items: $dns.upstreams,
            placeholder: "1.1.1.1",
            keyboard: .URL,
            footer: serversNote,
            emptyText: "Using the servers of the network the router joins"
        )
        Section {
            Toggle("Override DNS servers set on devices", isOn: $dns.intercept)
        } footer: {
            Text("A device set to use a DNS server of its own is answered by the router anyway, so ad blocking and rules for domains still apply to it. Encrypted DNS in a browser or an app is not affected.")
        }
        StringListEditor(
            title: "Blocked domains",
            items: $dns.blocked,
            placeholder: "example.com",
            keyboard: .URL,
            footer: "No device can open these domains or their subdomains."
        )
    }
}

// MARK: - SSH

private struct SSHPage: View {
    var body: some View {
        ConfigEditor(
            "SSH",
            \RouterConfig.system.ssh,
            valid: { (ssh: SSHConfig) -> Bool in !ssh.enabled || !ssh.authorizedKeys.isEmpty }
        ) { (ssh: Binding<SSHConfig>) in
            SSHFields(ssh: ssh)
        }
    }
}

private struct SSHFields: View {
    @Binding var ssh: SSHConfig

    private var enableHelp: String {
        if ssh.enabled && ssh.authorizedKeys.isEmpty {
            return "Add a key below before you save. SSH does not accept passwords."
        }
        return "With SSH allowed, you can sign in to the router from your own network with one of the keys below. Passwords are not accepted."
    }

    var body: some View {
        Section {
            Toggle("Allow SSH", isOn: $ssh.enabled)
        } footer: {
            Text(enableHelp)
        }
        StringListEditor(
            title: "Public keys",
            items: $ssh.authorizedKeys,
            placeholder: "ssh-ed25519 AAAA… name",
            keyboard: .asciiCapable,
            footer: ""
        )
        Section {
            Button {
                paste()
            } label: {
                Label("Paste a key from the clipboard", systemImage: "doc.on.clipboard")
            }
        }
    }

    @MainActor
    private func paste() {
        let text = UIPasteboard.general.string ?? ""
        for line in text.split(whereSeparator: { $0.isNewline }) {
            let key = line.trimmingCharacters(in: .whitespaces)
            if !key.isEmpty && !ssh.authorizedKeys.contains(key) {
                ssh.authorizedKeys.append(key)
            }
        }
    }
}

// MARK: - Speed test server

/// What the router accepts: an http or https address with a host, up to
/// 512 characters.
private func validWebURL(_ text: String) -> Bool {
    guard !text.isEmpty, text.utf8.count <= 512, !text.contains(" ") else { return false }
    guard let url = URL(string: text), let scheme = url.scheme?.lowercased() else { return false }
    guard scheme == "http" || scheme == "https" else { return false }
    return !(url.host ?? "").isEmpty
}

private struct SpeedTestServerPage: View {
    var body: some View {
        ConfigEditor(
            "Speed test server",
            \RouterConfig.speedtest,
            valid: { (speedtest: SpeedTestConfig) -> Bool in validWebURL(speedtest.downloadUrl) && validWebURL(speedtest.uploadUrl) }
        ) { (speedtest: Binding<SpeedTestConfig>) in
            SpeedTestServerFields(speedtest: speedtest)
        }
    }
}

private struct SpeedTestServerFields: View {
    @Binding var speedtest: SpeedTestConfig

    var body: some View {
        Section {
            TextField("Download address", text: $speedtest.downloadUrl, prompt: Text(verbatim: "https://example.com/large-file"))
                .keyboardType(.URL)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        } header: {
            Text("Download")
        } footer: {
            Text("The router measures download speed by fetching a large file from this address.")
        }
        Section {
            TextField("Upload address", text: $speedtest.uploadUrl, prompt: Text(verbatim: "https://example.com/upload"))
                .keyboardType(.URL)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        } header: {
            Text("Upload")
        } footer: {
            Text("The router measures upload speed by sending data to this address, which has to accept uploads.")
        }
    }
}

// MARK: - Software

/// The router's version, whether a newer one exists, and installing it.
struct SoftwarePage: View {
    @EnvironmentObject private var model: AppModel
    @State private var info: FirmwareInfo? = nil
    @State private var checking = false
    @State private var asking = false

    private var progress: FirmwareProgress? { model.firmwareProgress }
    private var installing: Bool { progress?.state == "installing" }

    private var version: String {
        info?.current ?? model.status?.version ?? ""
    }

    private var available: Bool { info?.newer == true }

    /// One line under the version: what there is to know right now.
    private var summary: String {
        if installing { return "Installing " + (progress?.version ?? "") }
        if checking && info == nil { return "Checking for updates" }
        guard let info else { return "" }
        if info.newer, let latest = info.latest {
            let size: String = formatBytes(UInt64(max(info.download ?? 0, 0)))
            return latest + " available · " + size
        }
        return "Up to date"
    }

    private var phaseLabel: String {
        switch progress?.phase ?? "" {
        case "download": return "Downloading"
        case "verify": return "Checking the download"
        case "restart": return "Restarting"
        default: return "Working"
        }
    }

    var body: some View {
        List {
            Section {
                VStack(spacing: 10) {
                    Glyph(symbol: available ? "arrow.down.circle.fill" : "checkmark.seal.fill", size: 64, filled: true)
                    Text(version)
                        .font(.title2.weight(.bold))
                        .fontDesign(.rounded)
                    Text(summary)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity)
                .padding(.vertical, 12)
                .listRowBackground(Color.clear)
            }
            if installing {
                Section {
                    VStack(alignment: .leading, spacing: 8) {
                        ProgressView(value: Double(progress?.percent ?? 0), total: 100)
                        HStack {
                            Text(phaseLabel)
                            Spacer()
                            Text("\(progress?.percent ?? 0) %")
                                .monospacedDigit()
                        }
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                    }
                    .padding(.vertical, 4)
                } footer: {
                    Text("Keep the router plugged in. It restarts by itself when the update is in place, and your devices are offline for a minute or two.")
                }
            } else if available {
                installSection
            }
            if progress?.state == "failed", let error = progress?.error {
                Section {
                    Label(error, systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.red)
                        .font(.footnote)
                }
            }
            if !installing {
                Section {
                    Button {
                        Task { await check() }
                    } label: {
                        HStack {
                            Label("Check again", systemImage: "arrow.clockwise")
                            Spacer()
                            if checking { ProgressView() }
                        }
                    }
                    .disabled(checking)
                }
            }
        }
        .navigationTitle("Software")
        .navigationBarTitleDisplayMode(.inline)
        .task {
            // An installation may be under way already, begun before
            // this page was opened or from another phone.
            await model.refreshFirmwareProgress()
            await check()
        }
        .confirmationDialog("Install " + (info?.latest ?? "the update") + "?", isPresented: $asking, titleVisibility: .visible) {
            Button("Install and restart") {
                Task { await model.attempt { try await $0.firmwareInstall() } }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Your settings are kept. While the router restarts, your devices are offline for a minute or two.")
        }
    }

    @ViewBuilder
    private var installSection: some View {
        if info?.supported == true {
            Section {
                Button {
                    asking = true
                } label: {
                    Text("Install")
                        .font(.headline)
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.large)
                .listRowInsets(EdgeInsets())
                .listRowBackground(Color.clear)
            }
        } else {
            Section {
                Label("This SD card was written before the router could update itself. Write the newest image to it once, which also erases the router's settings.", systemImage: "sdcard")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
        }
    }

    private func check() async {
        checking = true
        let found: FirmwareInfo? = await model.fetch { try await $0.firmwareCheck() }
        checking = false
        if let found {
            info = found
        }
    }
}

// MARK: - Paired phones

private struct PairedPhonesPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var phones: [PairedPhone] = []
    @State private var loaded = false

    var body: some View {
        List {
            Section {
                if !loaded {
                    ProgressView()
                }
                ForEach(phones) { phone in
                    PairedPhoneRow(phone: phone)
                        .swipeActions {
                            if !phone.isSelf {
                                Button("Remove", role: .destructive) { revoke(phone.key) }
                            }
                        }
                }
            }
            Section {
                if model.status?.pairing == true {
                    Label("Ready to pair a new phone", systemImage: "dot.radiowaves.left.and.right")
                        .foregroundStyle(.green)
                } else {
                    Button {
                        Task { await model.attempt { try await $0.openPairing() } }
                    } label: {
                        Label("Add a phone", systemImage: "plus")
                    }
                }
            } footer: {
                Text("Adding a phone gives another phone two minutes to pair: open Porter on it near the router and choose this router. A short press of the router's power button does the same.")
            }
        }
        .navigationTitle("Paired phones")
        .navigationBarTitleDisplayMode(.inline)
        .task { await load() }
    }

    private func load() async {
        let found: [PairedPhone]? = await model.fetch { try await $0.pairedPhones() }
        loaded = true
        if let found {
            phones = found
        }
    }

    private func revoke(_ key: String) {
        Task {
            _ = await model.attempt { try await $0.revokePhone(key: key) }
            await load()
        }
    }
}

private struct PairedPhoneRow: View {
    let phone: PairedPhone

    private var detail: String {
        if phone.lastSeen <= 0 { return "Has not connected yet" }
        return "Last connected " + relativeTimeInline(phone.lastSeen)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 8) {
                Text(phone.name)
                if phone.isSelf {
                    Badge(text: "This phone", color: Color.blue)
                }
            }
            Text(detail)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }
}

// MARK: - Activity and logs

struct ActivityPage: View {
    @EnvironmentObject private var model: AppModel

    private var events: [ActivityEvent] { Array(model.activity.reversed()) }

    var body: some View {
        List {
            if events.isEmpty {
                ContentUnavailableView(
                    "No activity yet",
                    systemImage: "clock",
                    description: Text("What the router notices is listed here, such as a new device joining or a network asking for a sign-in.")
                )
                .listRowBackground(Color.clear)
            }
            ForEach(Array(events.enumerated()), id: \.offset) { item in
                ActivityLine(event: item.element)
            }
        }
        .navigationTitle("Activity")
        .navigationBarTitleDisplayMode(.inline)
    }
}

private struct LogsPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var lines: [String] = []
    @State private var loading = false

    private var text: String { lines.joined(separator: "\n") }

    var body: some View {
        ScrollView {
            Text(text)
                .font(.caption2.monospaced())
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding()
        }
        .overlay {
            if loading {
                ProgressView()
            }
        }
        .navigationTitle("Logs")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                Button("Copy") { UIPasteboard.general.string = text }
                    .disabled(lines.isEmpty)
                Button("Reload") {
                    Task { await load() }
                }
                .disabled(loading)
            }
        }
        .task { await load() }
    }

    private func load() async {
        loading = true
        let found: [String]? = await model.fetch { try await $0.logs() }
        loading = false
        if let found {
            lines = found
        }
    }
}

// MARK: - System

private struct SystemSection: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        Section {
            HStack(spacing: 10) {
                PowerButton(
                    symbol: "arrow.clockwise",
                    title: "Restart",
                    question: "Restart the router?",
                    message: "Your devices are offline for about a minute while it restarts."
                ) {
                    Task { await model.attempt { try await $0.reboot() } }
                }
                PowerButton(
                    symbol: "power",
                    title: "Power off",
                    question: "Turn the router off?",
                    message: "Your Wi-Fi goes away and your devices are offline. To start the router again, press its power button, or unplug it and plug it back in."
                ) {
                    Task { await model.attempt { try await $0.powerOff() } }
                }
                PowerButton(
                    symbol: "trash",
                    title: "Reset",
                    question: "Erase the router?",
                    message: "All settings are erased, including your Wi-Fi, saved networks, VPNs, rules and paired phones. The router restarts as new and has to be set up again."
                ) {
                    factoryReset()
                }
            }
            .listRowInsets(EdgeInsets())
            .listRowBackground(Color.clear)
        }
        Section {
            Button("Disconnect from the router") { model.disconnect() }
        }
    }

    /// The reset erases the router's identity, so the key this phone
    /// pinned for it is forgotten too.
    private func factoryReset() {
        let router = model.knownRouters.first(where: { $0.id == model.status?.id })
        Task {
            let ok = await model.attempt { try await $0.factoryReset() }
            if ok, let router {
                model.forget(router)
            }
        }
    }
}

/// A destructive action as a tile: icon above its name, asking first.
/// `question` heads the confirmation, `message` says what will happen.
private struct PowerButton: View {
    let symbol: String
    let title: String
    let question: String
    let message: String
    let action: () -> Void

    @State private var asking = false

    var body: some View {
        Button {
            asking = true
        } label: {
            VStack(spacing: 8) {
                Image(systemName: symbol)
                    .font(.title3.weight(.semibold))
                Text(title)
                    .font(.footnote.weight(.medium))
            }
            .foregroundStyle(.red)
            .frame(maxWidth: .infinity)
            .padding(.vertical, 14)
            .background(Surface.card, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        }
        .buttonStyle(.plain)
        .confirmationDialog(question, isPresented: $asking, titleVisibility: .visible) {
            Button(title, role: .destructive) { action() }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text(message)
        }
    }
}

private struct AboutSection: View {
    @EnvironmentObject private var model: AppModel

    /// The month the database was published, such as "2026-11". The
    /// router may have a database without knowing its date.
    private var countryDatabase: String {
        guard let status = model.status else { return "" }
        if !status.geoip.available { return "Not installed" }
        if let date = status.geoip.date, !date.isEmpty { return date }
        return "Installed"
    }

    /// Which way the app reaches the router at this moment.
    private var connection: String {
        switch model.link {
        case .network: return "Over your network"
        case .bluetooth: return "Over Bluetooth"
        case nil: return "Reconnecting"
        }
    }

    var body: some View {
        Section {
            LabeledContent("ID", value: model.status?.id ?? "")
            LabeledContent("Version", value: model.status?.version ?? "")
            LabeledContent("Connection", value: connection)
            LabeledContent("Country database", value: countryDatabase)
        } footer: {
            Text(verbatim: "IP geolocation by DB-IP (https://db-ip.com)")
        }
    }
}
