import PorterKit
import SwiftUI
import UIKit

struct SettingsView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        List {
            RouterSettingsSection()
            Section("Access and history") {
                NavigationLink("Paired phones") { PairedPhonesPage() }
                NavigationLink("Activity") { ActivityPage() }
                NavigationLink("Logs") { LogsPage() }
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

    var body: some View {
        Section("Router") {
            NavigationLink {
                RouterNamePage()
            } label: {
                LabeledContent("Router name", value: model.config?.system.name ?? "")
            }
            NavigationLink {
                AccessPointPage()
            } label: {
                LabeledContent("Wi-Fi network", value: model.config?.ap.ssid ?? "")
            }
            NavigationLink {
                CountryPage()
            } label: {
                LabeledContent("Country", value: countryName)
            }
            NavigationLink {
                LANPage()
            } label: {
                LabeledContent("Local network", value: model.config?.lan.address ?? "")
            }
            NavigationLink("DNS") { DNSPage() }
            NavigationLink {
                SSHPage()
            } label: {
                LabeledContent("SSH", value: model.config?.system.ssh.enabled == true ? "On" : "Off")
            }
            NavigationLink("Speed test server") { SpeedTestServerPage() }
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
            Text("Shown in this app and used as the router's Bluetooth name. Up to 20 characters.")
        }
    }
}

// MARK: - Wi-Fi network

private func securityLabel(_ security: WifiSecurity) -> String {
    switch security {
    case .wpa2: return "WPA2"
    case .wpa2wpa3: return "WPA2+WPA3"
    case .wpa3: return "WPA3"
    }
}

private struct AccessPointPage: View {
    var body: some View {
        ConfigEditor(
            "Wi-Fi network",
            \RouterConfig.ap,
            valid: { (ap: AccessPointConfig) -> Bool in validNetworkName(ap.ssid) && validPassphrase(ap.passphrase) }
        ) { (ap: Binding<AccessPointConfig>) in
            AccessPointFields(ap: ap)
        }
    }
}

private struct AccessPointFields: View {
    @Binding var ap: AccessPointConfig
    @State private var showPassword = false

    var body: some View {
        Section {
            TextField("Network name", text: $ap.ssid)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            HStack {
                if showPassword {
                    TextField("Password", text: $ap.passphrase)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                } else {
                    SecureField("Password", text: $ap.passphrase)
                }
                Button {
                    showPassword.toggle()
                } label: {
                    Image(systemName: showPassword ? "eye.slash" : "eye")
                }
                .buttonStyle(.borderless)
            }
            Picker("Security", selection: $ap.security) {
                ForEach(WifiSecurity.allCases) { option in
                    Text(securityLabel(option)).tag(option)
                }
            }
        } footer: {
            Text("The password needs 8 to 63 characters. WPA2+WPA3 works with the widest range of devices; WPA3 alone shuts out older ones.")
        }
        Section {
            Toggle("Hide network name", isOn: $ap.hidden)
            Toggle("Isolate clients", isOn: $ap.isolate)
        } footer: {
            Text("Isolated wireless devices can reach the internet but not each other. Saving changes on this page restarts the access point, so devices will need to reconnect; after a new name or password they need the new details.")
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
            Text("Decides which Wi-Fi channels and power levels the router may use. Changing it restarts the radios.")
        }
    }
}

// MARK: - Local network

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

private struct LANPage: View {
    var body: some View {
        ConfigEditor(
            "Local network",
            \RouterConfig.lan,
            valid: { (lan: LANConfig) -> Bool in !lan.address.isEmpty && !lan.domain.isEmpty }
        ) { (lan: Binding<LANConfig>) in
            LANFields(lan: lan)
        }
    }
}

private struct LANFields: View {
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
            LabeledContent("Router address") {
                TextField("10.83.0.1/24", text: $lan.address)
                    .multilineTextAlignment(.trailing)
                    .keyboardType(.numbersAndPunctuation)
                    .autocorrectionDisabled()
            }
            LabeledContent("Domain") {
                TextField("lan", text: $lan.domain)
                    .multilineTextAlignment(.trailing)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
            }
            Picker("Address lease", selection: $lan.leaseMinutes) {
                ForEach(leaseOptions, id: \.self) { minutes in
                    Text(leaseLabel(minutes)).tag(minutes)
                }
            }
        } footer: {
            Text("The router's own address and the size of its network, such as 10.83.0.1/24: a private address with a prefix from /16 to /28. Devices get names under the domain, like laptop.lan, and move to a new address range when they next reconnect.")
        }
        Section {
            Toggle("Move on address conflict", isOn: $lan.autoRenumber)
        } footer: {
            Text("When the upstream network uses the same address range, the router moves its own network to a free range so both keep working.")
        }
        Section {
            Toggle("IPv6", isOn: $lan.ipv6)
            LabeledContent("Prefix", value: lan.ulaPrefix)
        } footer: {
            Text("Devices get IPv6 addresses from this private prefix. IPv6 traffic leaves through address translation (NAT66), so upstream networks see only the router.")
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

    private var upstreamHelp: String {
        [
            "Used for lookups that leave directly. With none listed, the router uses the servers the upstream network hands out. Up to 8, in these forms:",
            "1.1.1.1 (plain DNS)",
            "tls://1.1.1.1#cloudflare-dns.com (DNS over TLS)",
            "https://cloudflare-dns.com/dns-query#1.1.1.1 (DNS over HTTPS)",
        ].joined(separator: "\n")
    }

    var body: some View {
        StringListEditor(
            title: "Upstream servers",
            items: $dns.upstreams,
            placeholder: "1.1.1.1",
            keyboard: .URL,
            footer: upstreamHelp
        )
        Section {
            Toggle("Answer all DNS on the network", isOn: $dns.intercept)
        } footer: {
            Text("Redirects every plain DNS lookup from the network to the router, so a device with its own resolver set cannot get around domain rules and blocked domains.")
        }
        StringListEditor(
            title: "Blocked domains",
            items: $dns.blocked,
            placeholder: "example.com",
            keyboard: .URL,
            footer: "These domains and their subdomains do not resolve for anything on the network."
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
            return "Add at least one authorized key before saving."
        }
        return "Reachable from the router's local network only, with keys only. Needs at least one authorized key."
    }

    var body: some View {
        Section {
            Toggle("Allow SSH", isOn: $ssh.enabled)
        } footer: {
            Text(enableHelp)
        }
        StringListEditor(
            title: "Authorized keys",
            items: $ssh.authorizedKeys,
            placeholder: "ssh-ed25519 AAAA… name",
            keyboard: .asciiCapable,
            footer: "Public keys, one per entry. Up to 8."
        )
        Section {
            Button("Paste key from clipboard") { paste() }
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
            TextField("https://example.com/large-file", text: $speedtest.downloadUrl)
                .keyboardType(.URL)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        } header: {
            Text("Download URL")
        } footer: {
            Text("Has to return a large file. The router fetches it several times over to measure download speed.")
        }
        Section {
            TextField("https://example.com/upload", text: $speedtest.uploadUrl)
                .keyboardType(.URL)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        } header: {
            Text("Upload URL")
        } footer: {
            Text("Has to accept data sent to it with POST. Both addresses start with http:// or https://, and the router contacts them only when a speed test is run from this app.")
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
                                Button("Revoke", role: .destructive) { revoke(phone.key) }
                            }
                        }
                }
            } footer: {
                Text("Swipe to revoke another phone. A revoked phone is cut off at once and has to pair again.")
            }
            Section {
                Button("Allow another phone to pair") {
                    Task { await model.attempt { try await $0.openPairing() } }
                }
                if model.status?.pairing == true {
                    Label("Pairing is open", systemImage: "dot.radiowaves.left.and.right")
                        .foregroundStyle(.green)
                }
            } footer: {
                Text("Pairing stays open for two minutes, or until one phone has paired. A short press of the power button on the Raspberry Pi does the same.")
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
        "Paired " + formatDate(phone.added) + " · last connected " + relativeTime(phone.lastSeen)
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

private struct ActivityPage: View {
    @EnvironmentObject private var model: AppModel

    private var events: [ActivityEvent] { Array(model.activity.reversed()) }

    var body: some View {
        List {
            if events.isEmpty {
                Text("Nothing has happened yet.")
                    .foregroundStyle(.secondary)
            }
            ForEach(Array(events.enumerated()), id: \.offset) { item in
                ActivityRow(event: item.element)
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
        Section("System") {
            ConfirmedButton(
                title: "Reboot",
                message: "The network is down for about a minute while the router restarts.",
                confirmTitle: "Reboot"
            ) {
                Task { await model.attempt { try await $0.reboot() } }
            }
            ConfirmedButton(
                title: "Power off",
                message: "The router shuts down and stays off until its power is unplugged and plugged in again.",
                confirmTitle: "Power off"
            ) {
                Task { await model.attempt { try await $0.powerOff() } }
            }
            ConfirmedButton(
                title: "Factory reset",
                message: "Erases all settings, saved networks, VPN profiles and paired phones, then restarts. The router has to be set up and paired again.",
                confirmTitle: "Erase everything"
            ) {
                factoryReset()
            }
            Button("Disconnect") { model.disconnect() }
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

    var body: some View {
        Section {
            LabeledContent("Name", value: model.status?.name ?? "")
            LabeledContent("Identifier", value: model.status?.id ?? "")
            LabeledContent("Version", value: model.status?.version ?? "")
            LabeledContent("Country database", value: countryDatabase)
        } header: {
            Text("About")
        } footer: {
            Text(verbatim: "IP geolocation by DB-IP (https://db-ip.com)")
        }
    }
}
