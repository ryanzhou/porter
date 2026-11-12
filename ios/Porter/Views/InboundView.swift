import PorterKit
import SwiftUI
import UIKit

/// What comes in from outside: your own devices reaching home through
/// the router's WireGuard server, ports forwarded to devices, and the
/// name both are found by. The VPN tab is the other direction, where the
/// router connects out.
struct InboundPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var adding = false
    @State private var editing: PortForward? = nil

    var body: some View {
        List {
            ReachSection()
            ReachHomeSection(adding: $adding)
            ForwardsSection(editing: $editing)
            DynamicDNSSection()
        }
        .navigationTitle("From outside")
        .refreshable {
            await model.quietly { connection in try await connection.refreshInbound() }
            await model.refresh()
        }
        .sheet(isPresented: $adding) {
            NavigationStack {
                AddHomeDeviceSheet()
            }
            .modifier(SheetErrorAlert())
            .environmentObject(model)
        }
        .sheet(item: $editing) { forward in
            NavigationStack {
                PortForwardEditor(forward: forward)
            }
            .modifier(SheetErrorAlert())
            .environmentObject(model)
        }
    }
}

/// What the Settings row says about all of this: whether anything is let
/// in at all.
func inboundSummary(_ config: RouterConfig?) -> String {
    guard let inbound = config?.inbound else { return "" }
    let forwards: Int = inbound.forwards.filter(\.enabled).count
    if inbound.server.enabled || forwards > 0 { return "On" }
    return "Off"
}

// MARK: - Whether the router can be reached

/// Why nothing from outside gets to the router as things stand, and what
/// would change that; empty when it is reachable.
func reachProblem(_ inbound: InboundStatus, port: Int) -> String {
    switch inbound.reach {
    case "nat":
        let address: String = inbound.address ?? "this router"
        return "The router is connected behind another router, which has the public address. Nothing from outside gets here until that router passes it on. In its settings, forward UDP port \(port) and the ports forwarded below to \(address), or switch it to what it calls bridge mode, which hands the public address to this router."
    case "cgnat":
        var text: String = "Your provider shares one public address among several customers, so nothing can reach the router from outside over IPv4. Providers usually hand out a public address of your own when asked."
        if inbound.publicAddress6 != nil {
            text += " Over IPv6 the router can be reached: set up dynamic DNS below with IPv6 included, and use that name."
        }
        return text
    case "offline":
        return "The router is not online at the moment."
    default:
        return ""
    }
}

private struct ReachSection: View {
    @EnvironmentObject private var model: AppModel

    private var port: Int { model.config?.inbound?.server.port ?? 51820 }
    private var host: String { model.status?.inbound?.host ?? "" }

    private var problem: String {
        guard let inbound = model.status?.inbound else { return "" }
        return reachProblem(inbound, port: port)
    }

    var body: some View {
        if !host.isEmpty || !problem.isEmpty {
            Section {
                if !host.isEmpty {
                    LabeledContent("Reached at") {
                        Text(host)
                            .textSelection(.enabled)
                    }
                }
                if !problem.isEmpty {
                    Label {
                        Text(problem)
                            .font(.callout)
                    } icon: {
                        Image(systemName: "exclamationmark.triangle.fill")
                            .foregroundStyle(.orange)
                    }
                }
            }
        }
    }
}

// MARK: - Reach home

private struct ReachHomeSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var adding: Bool

    private var server: HomeServerConfig? { model.config?.inbound?.server }
    private var status: HomeServerStatus? { model.status?.inbound?.server }

    private var enabled: Binding<Bool> {
        Binding(
            get: { server?.enabled ?? false },
            set: { value in
                Task { await model.update { config in config.inbound?.server.enabled = value } }
            }
        )
    }

    /// A device can only be set up while there is something to tell it
    /// to connect to.
    private var reachable: Bool {
        let host: String = model.status?.inbound?.host ?? ""
        return !host.isEmpty
    }

    var body: some View {
        Section {
            Toggle("Enabled", isOn: enabled)
            if let server, server.enabled {
                if let problem = status?.error, !problem.isEmpty {
                    Text(problem)
                        .font(.callout)
                        .foregroundStyle(.red)
                }
                ForEach(server.peers) { peer in
                    NavigationLink {
                        HomeDeviceDetail(deviceID: peer.id)
                    } label: {
                        HomeDeviceRow(device: peer, status: status?.peers.first(where: { $0.id == peer.id }))
                    }
                }
                Button {
                    adding = true
                } label: {
                    Label("Add a device", systemImage: "plus")
                }
                .disabled(!reachable)
                NavigationLink {
                    HomeServerPortPage()
                } label: {
                    LabeledContent("Port", value: String(server.port))
                }
            }
        } header: {
            Text("Reach home")
        } footer: {
            Text("Your own phones and computers connect to the router with the WireGuard app while they are away, and are then on your networks as they are at home. The VPN tab is the other direction: there the router connects out to a VPN.")
        }
    }
}

/// How a device's connection stands, in a line.
private func homeDeviceDetail(_ device: HomeDevice, _ status: HomeDeviceStatus?) -> String {
    if !device.enabled { return "Off" }
    guard let status else { return "Has not connected yet" }
    if status.connected {
        return "Connected  ↓ " + formatBytes(UInt64(clamping: status.tx)) + "  ↑ " + formatBytes(UInt64(clamping: status.rx))
    }
    if let last = status.handshake, last > 0 {
        return "Last connected " + relativeTimeInline(last)
    }
    return "Has not connected yet"
}

private struct HomeDeviceRow: View {
    let device: HomeDevice
    let status: HomeDeviceStatus?

    private var connected: Bool { device.enabled && (status?.connected ?? false) }

    var body: some View {
        HStack(spacing: 12) {
            Glyph(symbol: "house.fill", color: connected ? Color.green : Color.secondary, filled: connected)
            VStack(alignment: .leading, spacing: 2) {
                Text(device.name)
                Text(homeDeviceDetail(device, status))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

private struct HomeDeviceDetail: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let deviceID: String

    @State private var name: String = ""
    @State private var loaded = false

    private var device: HomeDevice? {
        model.config?.inbound?.server.peers.first(where: { $0.id == deviceID })
    }

    private var status: HomeDeviceStatus? {
        model.status?.inbound?.server.peers.first(where: { $0.id == deviceID })
    }

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }

    private var enabled: Binding<Bool> {
        Binding(
            get: { device?.enabled ?? false },
            set: { value in
                let id: String = deviceID
                Task {
                    await model.update { config in
                        guard let index = config.inbound?.server.peers.firstIndex(where: { $0.id == id }) else { return }
                        config.inbound?.server.peers[index].enabled = value
                    }
                }
            }
        )
    }

    var body: some View {
        Form {
            if let device {
                Section {
                    TextField("Name", text: $name)
                    Toggle("Enabled", isOn: enabled)
                } footer: {
                    Text("Switched off, the device is cut off at once and keeps its configuration for when it is switched on again.")
                }
                Section {
                    LabeledContent("Status", value: homeDeviceDetail(device, status))
                    if let endpoint = status?.endpoint, !endpoint.isEmpty {
                        LabeledContent("Connects from", value: endpoint)
                    }
                    LabeledContent("Address at home", value: device.address)
                    LabeledContent("Sends through home", value: device.full ? "Everything" : "Only your networks")
                }
                Section {
                    ConfirmedButton(
                        title: "Remove device",
                        message: "Its configuration stops working at once. To let the device in again, add it anew and set it up with the new configuration.",
                        confirmTitle: "Remove"
                    ) {
                        remove()
                    }
                } footer: {
                    Text("The configuration a device was given cannot be shown again. A device that has lost it is removed here and added again.")
                }
            } else {
                ContentUnavailableView("Device removed", systemImage: "questionmark.circle")
            }
        }
        .navigationTitle(device?.name ?? "Device")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { rename() }
                    .disabled(cleanName.isEmpty || cleanName == (device?.name ?? ""))
            }
        }
        .onAppear {
            if !loaded {
                name = device?.name ?? ""
                loaded = true
            }
        }
    }

    private func rename() {
        let id: String = deviceID
        let value: String = cleanName
        Task {
            await model.update { config in
                guard let index = config.inbound?.server.peers.firstIndex(where: { $0.id == id }) else { return }
                config.inbound?.server.peers[index].name = value
            }
        }
    }

    private func remove() {
        let id: String = deviceID
        Task {
            let ok: Bool = await model.update { config in
                config.inbound?.server.peers.removeAll(where: { $0.id == id })
            }
            if ok { dismiss() }
        }
    }
}

private struct HomeServerPortPage: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var text: String = ""
    @State private var loaded = false

    private var current: Int { model.config?.inbound?.server.port ?? 51820 }
    private var port: Int? { Int(text.trimmingCharacters(in: .whitespaces)) }

    var body: some View {
        Form {
            Section {
                TextField("Port", text: $text)
                    .keyboardType(.numberPad)
            } footer: {
                Text("Devices that were added before the port is changed go on trying the old one, and have to be removed and added again.")
            }
        }
        .navigationTitle("Port")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
                    .disabled(port == nil || port == current)
            }
        }
        .onAppear {
            if !loaded {
                text = String(current)
                loaded = true
            }
        }
    }

    private func save() {
        guard let port else { return }
        Task {
            let ok: Bool = await model.update { config in config.inbound?.server.port = port }
            if ok { dismiss() }
        }
    }
}

/// Adds a device and then shows its configuration, which exists at that
/// moment only.
private struct AddHomeDeviceSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var name: String = ""
    @State private var full = true
    @State private var working = false
    @State private var added: HomeDeviceAdded? = nil

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }

    var body: some View {
        if let added {
            HomeDeviceConfigView(name: cleanName, added: added)
        } else {
            Form {
                Section {
                    TextField("Name", text: $name)
                }
                Section {
                    Picker("Through home", selection: $full) {
                        Text("Everything").tag(true)
                        Text("Only your networks").tag(false)
                    }
                    .pickerStyle(.inline)
                    .labelsHidden()
                } header: {
                    Text("Through home")
                } footer: {
                    Text("With Everything, all the device does online goes through the router while it is connected, and follows the same routes and rules as at home. With Only your networks, the device reaches what is at home and uses its own connection for the rest.")
                }
            }
            .navigationTitle("Add a device")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Add") { add() }
                        .disabled(working || cleanName.isEmpty)
                }
            }
        }
    }

    private func add() {
        let wantedName: String = cleanName
        let wantedFull: Bool = full
        working = true
        Task {
            let result: HomeDeviceAdded? = await model.fetch { connection in
                try await connection.addHomeDevice(name: wantedName, full: wantedFull)
            }
            // The router has a device more than the copy here knows of.
            await model.refresh()
            working = false
            added = result
        }
    }
}

/// A new device's configuration: as a code for the WireGuard app to scan,
/// and as text or a file for a device that cannot.
private struct HomeDeviceConfigView: View {
    @Environment(\.dismiss) private var dismiss
    let name: String
    let added: HomeDeviceAdded

    @State private var file: URL? = nil

    var body: some View {
        ScrollView {
            VStack(spacing: 20) {
                if let code = qrCode(added.conf) {
                    Image(uiImage: code)
                        .interpolation(.none)
                        .resizable()
                        .scaledToFit()
                        .frame(maxWidth: 280)
                        .padding(16)
                        .background(Color.white, in: RoundedRectangle(cornerRadius: 24, style: .continuous))
                        .accessibilityLabel(Text("QR code with the device's WireGuard configuration"))
                }
                Text("In the WireGuard app on the device, tap the plus and choose Create from QR code. The device then connects to \(added.host).")
                    .multilineTextAlignment(.center)
                Text("This is the only time the configuration is shown. The router does not keep the device's key: if the configuration is lost, remove the device and add it again.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                HStack(spacing: 12) {
                    Button {
                        UIPasteboard.general.string = added.conf
                    } label: {
                        Label("Copy", systemImage: "doc.on.doc")
                    }
                    .buttonStyle(.bordered)
                    if let file {
                        ShareLink(item: file) {
                            Label("Share", systemImage: "square.and.arrow.up")
                        }
                        .buttonStyle(.bordered)
                    }
                }
            }
            .frame(maxWidth: .infinity)
            .padding(24)
        }
        .background(Surface.page)
        .navigationTitle(name)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Done") { dismiss() }
            }
        }
        .onAppear { writeFile() }
        .onDisappear { removeFile() }
    }

    /// The configuration as a file, which is what the WireGuard apps on
    /// computers import. It holds a private key, and is gone again when
    /// this screen is.
    private func writeFile() {
        let url: URL = FileManager.default.temporaryDirectory.appendingPathComponent("porter-home.conf")
        do {
            try Data(added.conf.utf8).write(to: url, options: [.atomic, .completeFileProtection])
            file = url
        } catch {
            file = nil
        }
    }

    private func removeFile() {
        if let file {
            try? FileManager.default.removeItem(at: file)
        }
    }
}

// MARK: - Forwarded ports

private func protocolLabel(_ proto: String) -> String {
    switch proto {
    case "udp": return "UDP"
    case "both": return "TCP and UDP"
    default: return "TCP"
    }
}

private struct ForwardsSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var editing: PortForward?

    private var forwards: [PortForward] { model.config?.inbound?.forwards ?? [] }

    private func detail(_ forward: PortForward) -> String {
        var text: String = protocolLabel(forward.proto) + " " + forward.ports + " to " + model.deviceName(forward.device)
        if forward.to != 0 {
            text += ", port " + String(forward.to)
        }
        if !forward.enabled {
            text += "  Off"
        }
        return text
    }

    var body: some View {
        if model.config?.inbound != nil {
            Section {
                ForEach(forwards) { forward in
                    Button {
                        editing = forward
                    } label: {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(forward.name)
                                .foregroundStyle(Color.primary)
                            Text(detail(forward))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
                Button {
                    editing = PortForward(id: AppModel.newID())
                } label: {
                    Label("Forward a port", systemImage: "plus")
                }
            } header: {
                Text("Forwarded ports")
            } footer: {
                Text("A forwarded port lets anyone on the internet connect to one device at home. The device keeps a fixed address for as long as a port is forwarded to it.")
            }
        }
    }
}

private struct PortForwardEditor: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var draft: PortForward
    @State private var insidePort: String
    @State private var saving = false

    init(forward: PortForward) {
        _draft = State(initialValue: forward)
        _insidePort = State(initialValue: forward.to == 0 ? "" : String(forward.to))
    }

    private var existing: Bool {
        let id: String = draft.id
        return model.config?.inbound?.forwards.contains(where: { $0.id == id }) ?? false
    }

    /// The devices to choose from: every one the router knows, by name.
    private var devices: [DeviceInfo] {
        model.devices.sorted { $0.displayName.localizedCaseInsensitiveCompare($1.displayName) == .orderedAscending }
    }

    private var cleanName: String { draft.name.trimmingCharacters(in: .whitespaces) }
    private var cleanPorts: String { draft.ports.replacingOccurrences(of: " ", with: "") }
    private var cleanInside: String { insidePort.trimmingCharacters(in: .whitespaces) }

    private var canSave: Bool {
        if saving || cleanName.isEmpty || cleanPorts.isEmpty || draft.device.isEmpty { return false }
        return cleanInside.isEmpty || Int(cleanInside) != nil
    }

    /// Where the port is open over IPv6, when the device has an address
    /// of its own there.
    private var overIPv6: String {
        let id: String = draft.id
        guard let status = model.status?.inbound?.forwards.first(where: { $0.id == id }), let address = status.to6.first else { return "" }
        return "Over IPv6 nothing is translated: the device is reached at its own address, \(address), on the port it listens on."
    }

    var body: some View {
        Form {
            Section {
                TextField("Name", text: $draft.name)
                Toggle("Enabled", isOn: $draft.enabled)
            }
            Section {
                Picker("Protocol", selection: $draft.proto) {
                    Text("TCP").tag("tcp")
                    Text("UDP").tag("udp")
                    Text("Both").tag("both")
                }
                TextField("Port or range, like 8080 or 5000-5010", text: $draft.ports)
                    .keyboardType(.numbersAndPunctuation)
                    .autocorrectionDisabled()
            } header: {
                Text("From outside")
            }
            Section {
                Picker("Device", selection: $draft.device) {
                    Text("Choose").tag("")
                    ForEach(devices) { device in
                        Text(device.displayName).tag(device.mac)
                    }
                }
                .pickerStyle(.navigationLink)
                TextField("Port on the device", text: $insidePort, prompt: Text("Same port"))
                    .keyboardType(.numberPad)
            } header: {
                Text("To")
            } footer: {
                Text(overIPv6)
            }
            StringListEditor(
                title: "Only from",
                items: $draft.from,
                placeholder: "203.0.113.0/24",
                keyboard: .numbersAndPunctuation,
                footer: "With addresses listed here, the port is open to them and to nobody else.",
                emptyText: "Open to everyone"
            )
            if existing {
                Section {
                    ConfirmedButton(
                        title: "Remove",
                        message: "Connections that came in through this port end at once.",
                        confirmTitle: "Remove"
                    ) {
                        remove()
                    }
                }
            }
        }
        .navigationTitle(existing ? "Forwarded port" : "Forward a port")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel") { dismiss() }
            }
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
                    .disabled(!canSave)
            }
        }
    }

    private func save() {
        var forward: PortForward = draft
        forward.name = cleanName
        forward.ports = cleanPorts
        forward.to = Int(cleanInside) ?? 0
        saving = true
        Task {
            let ok: Bool = await model.update { config in
                if let index = config.inbound?.forwards.firstIndex(where: { $0.id == forward.id }) {
                    config.inbound?.forwards[index] = forward
                } else {
                    config.inbound?.forwards.append(forward)
                }
            }
            saving = false
            if ok { dismiss() }
        }
    }

    private func remove() {
        let id: String = draft.id
        Task {
            let ok: Bool = await model.update { config in
                config.inbound?.forwards.removeAll(where: { $0.id == id })
            }
            if ok { dismiss() }
        }
    }
}

// MARK: - Dynamic DNS

private func providerLabel(_ provider: String) -> String {
    switch provider {
    case "duckdns": return "DuckDNS"
    case "cloudflare": return "Cloudflare"
    case "dyndns2": return "Another provider"
    case "custom": return "An address to fetch"
    default: return "Choose"
    }
}

private struct DynamicDNSSection: View {
    @EnvironmentObject private var model: AppModel

    private var ddns: DynamicDNSConfig? { model.config?.inbound?.ddns }
    private var status: DynamicDNSStatus? { model.status?.inbound?.ddns }

    private var value: String {
        guard let ddns, ddns.enabled else { return "Off" }
        return ddns.hostname
    }

    var body: some View {
        if ddns != nil {
            Section {
                NavigationLink {
                    DynamicDNSPage()
                } label: {
                    LabeledContent("Dynamic DNS", value: value)
                }
                if let status, ddns?.enabled == true {
                    DynamicDNSStatusRow(status: status)
                }
            } header: {
                Text("Name")
            } footer: {
                Text("Most home connections get a new address from time to time. Dynamic DNS keeps a name pointing at the current one, and devices that reach home are then given the name to connect to.")
            }
        }
    }
}

private struct DynamicDNSStatusRow: View {
    let status: DynamicDNSStatus

    private var updated: String {
        var text: String = "Updated " + relativeTimeInline(status.updated ?? 0)
        if let address = status.address, !address.isEmpty {
            text += " with " + address
        }
        return text
    }

    var body: some View {
        if status.state == "error" || status.state == "waiting" {
            Text(status.error ?? "The name has not been updated yet.")
                .font(.callout)
                .foregroundStyle(status.state == "error" ? Color.red : Color.secondary)
        } else if status.state == "ok" {
            Text(updated)
                .font(.callout)
                .foregroundStyle(.secondary)
        }
    }
}

private struct DynamicDNSPage: View {
    @EnvironmentObject private var model: AppModel

    @State private var draft = DynamicDNSConfig()
    @State private var loaded = false
    @State private var saving = false

    private var stored: DynamicDNSConfig { model.config?.inbound?.ddns ?? DynamicDNSConfig() }
    private var status: DynamicDNSStatus? { model.status?.inbound?.ddns }

    private var namePrompt: String {
        draft.provider == "duckdns" ? "example.duckdns.org" : "home.example.com"
    }

    var body: some View {
        Form {
            Section {
                Toggle("Enabled", isOn: $draft.enabled)
                Picker("Provider", selection: $draft.provider) {
                    if draft.provider.isEmpty {
                        Text("Choose").tag("")
                    }
                    Text(providerLabel("duckdns")).tag("duckdns")
                    Text(providerLabel("cloudflare")).tag("cloudflare")
                    Text(providerLabel("dyndns2")).tag("dyndns2")
                    Text(providerLabel("custom")).tag("custom")
                }
            }
            if !draft.provider.isEmpty {
                Section {
                    TextField("Name", text: $draft.hostname, prompt: Text(namePrompt))
                        .keyboardType(.URL)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    providerFields
                } footer: {
                    Text(providerNote)
                }
                Section {
                    Toggle("Include IPv6", isOn: $draft.ipv6)
                } footer: {
                    Text("With this on, the name also leads to the router over IPv6, which is the way in when your provider gives you no public IPv4 address of your own.")
                }
            }
            if stored.enabled, let status {
                Section {
                    DynamicDNSStatusRow(status: status)
                    Button("Update now") {
                        Task {
                            await model.attempt { connection in try await connection.refreshInbound() }
                        }
                    }
                }
            }
        }
        .navigationTitle("Dynamic DNS")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
                    .disabled(saving || draft == stored)
            }
        }
        .onAppear {
            if !loaded {
                draft = stored
                loaded = true
            }
        }
    }

    @ViewBuilder private var providerFields: some View {
        switch draft.provider {
        case "duckdns":
            SecureField("Token", text: $draft.password)
        case "cloudflare":
            SecureField("API token", text: $draft.password)
        case "dyndns2":
            TextField("Update server", text: $draft.server, prompt: Text("members.dyndns.org"))
                .keyboardType(.URL)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            TextField("User name", text: $draft.username)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            SecureField("Password", text: $draft.password)
        default:
            TextField("Address", text: $draft.url, prompt: Text("https://dns.example.com/update?ip={ip}"))
                .keyboardType(.URL)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        }
    }

    private var providerNote: String {
        switch draft.provider {
        case "cloudflare":
            return "The token has to be allowed to edit the DNS records of the domain the name is in."
        case "dyndns2":
            return "This works with the many providers that take updates the way DynDNS does. Your provider's instructions name the update server."
        case "custom":
            return "The router fetches this address whenever its own changes, with {ip} replaced by its IPv4 address and {ip6} by its IPv6 address."
        default:
            return ""
        }
    }

    private func save() {
        let next: DynamicDNSConfig = draft
        saving = true
        Task {
            let ok: Bool = await model.update { config in config.inbound?.ddns = next }
            saving = false
            if ok {
                draft = stored
            }
        }
    }
}
