import Combine
import Foundation
import PorterKit
import UIKit

/// The app's single source of truth: the Bluetooth link, the session with
/// the router, and the router state the views show and edit.
@MainActor
final class AppModel: ObservableObject {
    enum Phase: Equatable {
        /// Looking for a router (none paired, or the paired one is not in range).
        case searching
        case connecting(name: String)
        /// Connected, but the router wants its power button pressed
        /// before it accepts this phone.
        case awaitingPairing(name: String)
        case connected
    }

    /// The router's software and this app are of different generations
    /// and cannot read each other's settings.
    enum Mismatch {
        /// The router has to be updated; the app can still tell it to.
        case routerOlder
        /// The app has to be installed again from newer source.
        case appOlder
    }

    @Published private(set) var phase: Phase = .searching
    /// Set while connected to a router this app cannot work with.
    @Published private(set) var mismatch: Mismatch?
    @Published private(set) var status: RouterStatus?
    @Published private(set) var config: RouterConfig?
    @Published private(set) var devices: [DeviceInfo] = []
    @Published private(set) var radios: [RadioInfo] = []
    @Published private(set) var activity: [ActivityEvent] = []
    @Published private(set) var knownRouters: [KnownRouter] = []
    /// Progress of the speed test currently running, if any.
    @Published private(set) var speedTestProgress: SpeedTestProgress?
    /// The state of a software update being installed, if the router has
    /// reported one in this session.
    @Published private(set) var firmwareProgress: FirmwareProgress?
    /// A message for the user; views present it as an alert and clear it.
    @Published var errorMessage: String?

    let central = BLECentral()
    private let store = KeyStore()
    private var connection: RouterConnection?
    private var link: BLELink?
    /// The configuration as the router last reported it. `config`, which
    /// the views show, is this with the edits applied that have been sent
    /// and not yet confirmed: a control shows its new value the moment it
    /// is touched, not a Bluetooth round trip later.
    private var confirmedConfig: RouterConfig?
    private var pendingEdits: [(id: Int, edit: (inout RouterConfig) -> Void)] = []
    private var nextEditID = 0
    /// The update started last. The next one waits for it to finish, so
    /// that each is written on top of what the one before it left on the
    /// router and none finds the router a revision ahead of it.
    private var updateTail: Task<Bool, Never>?
    private var connectTask: Task<Void, Never>?
    /// Counts the attempts to connect and the disconnects. An attempt
    /// whose number is no longer this one has been cancelled or replaced,
    /// and leaves everything to whatever came after it.
    private var generation = 0
    /// How many attempts nobody asked for have failed in a row.
    private var automaticFailures = 0
    private var eventTask: Task<Void, Never>?
    private var subscriptions: Set<AnyCancellable> = []
    /// Set when the user disconnects on purpose, to stop auto-reconnect.
    private var holdOff = false

    init() {
        knownRouters = store.routers()
        // Forward the central's changes so views observing the model
        // update with the scan list.
        central.objectWillChange
            .sink { [weak self] _ in self?.objectWillChange.send() }
            .store(in: &subscriptions)
        central.$discovered
            .sink { [weak self] routers in self?.autoConnect(routers) }
            .store(in: &subscriptions)
        central.$state
            .sink { [weak self] _ in Task { @MainActor in self?.autoConnect(self?.central.discovered ?? []) } }
            .store(in: &subscriptions)
    }

    /// Demo mode runs the whole app against built-in sample data, with no
    /// router and no Bluetooth: launch with the argument `-demo`.
    let isDemo = ProcessInfo.processInfo.arguments.contains("-demo")

    /// In demo mode, the screen to open directly (`-demoScreen traffic`).
    var demoScreen: String? {
        isDemo ? UserDefaults.standard.string(forKey: "demoScreen") : nil
    }

    func start() {
        if isDemo {
            startDemo()
        } else {
            central.startScanning()
        }
    }

    private func startDemo() {
        guard connection == nil else { return }
        let demo = DemoRouter.makeConnection()
        connection = demo
        Task {
            try? await reload()
            activity = (try? await demo.activity()) ?? []
            phase = .connected
        }
    }

    var isConnected: Bool { phase == .connected }

    // MARK: - Connecting

    /// Connects to the paired router as soon as it shows up.
    private func autoConnect(_ routers: [DiscoveredRouter]) {
        guard !isDemo, phase == .searching, connectTask == nil, !holdOff else { return }
        guard let match = routers.first(where: { seen in knownRouters.contains { $0.peripheral == seen.id } }) else { return }
        connect(to: match, automatic: true)
    }

    /// Connects to a router. `automatic` marks the attempt nobody asked
    /// for, made because the paired router came into view.
    func connect(to router: DiscoveredRouter, automatic: Bool = false) {
        holdOff = false
        if !automatic { automaticFailures = 0 }
        if connectTask != nil {
            // An attempt still under way is replaced, and what it has
            // opened so far goes now rather than when it next looks.
            connectTask?.cancel()
            connectTask = nil
            teardown()
        }
        generation += 1
        let attempt: Int = generation
        connectTask = Task { [weak self] in
            await self?.runConnection(peripheral: router.id, name: router.name, attempt: attempt, automatic: automatic)
            // An attempt that was cancelled or replaced has no task to
            // clear: the one stored by now belongs to a newer attempt.
            guard let self, self.generation == attempt else { return }
            self.connectTask = nil
        }
    }

    /// Whether a connection attempt is still the one under way. One that
    /// is not has been cancelled or replaced: the link it opened itself,
    /// if any, is dropped here, and the caller returns without touching
    /// anything else.
    private func isCurrent(_ attempt: Int, link: BLELink?) -> Bool {
        if attempt == generation && !Task.isCancelled { return true }
        link?.abandon(BLEError.disconnected)
        return false
    }

    private func runConnection(peripheral: UUID, name: String, attempt: Int, automatic: Bool) async {
        guard isCurrent(attempt, link: nil) else { return }
        phase = .connecting(name: name)
        // The link this attempt opens. Cancel may come before the model
        // has it, and then nothing but the attempt itself can drop it.
        var opened: BLELink? = nil
        do {
            let link = try await central.connect(to: peripheral)
            opened = link
            guard isCurrent(attempt, link: link) else { return }
            self.link = link
            let known = knownRouters.first { $0.peripheral == peripheral }
            // The secure handshake and the first exchange get a deadline of
            // their own: a router that accepts the Bluetooth connection but
            // never answers must not leave the app waiting for ever.
            let deadline = Task { [weak link] in
                do {
                    try await Task.sleep(nanoseconds: 15_000_000_000)
                } catch {
                    return // cancelled: the router answered
                }
                link?.abandon(BLEError.stalled("securing the connection"))
            }
            defer { deadline.cancel() }
            let connection = try await RouterConnection.connect(
                transport: link, identity: try store.identity(), expectedRouterKey: known?.publicKey)
            guard isCurrent(attempt, link: link) else { return }
            self.connection = connection
            var hello = try await connection.hello()
            guard isCurrent(attempt, link: link) else { return }
            deadline.cancel()

            // Not paired yet: ask, and keep asking while the user goes to
            // press the button.
            while !hello.paired {
                do {
                    try await connection.pair(name: UIDevice.current.name)
                    guard isCurrent(attempt, link: link) else { return }
                    hello = try await connection.hello()
                    guard isCurrent(attempt, link: link) else { return }
                } catch let error as RouterError where error.code == RouterError.pairingClosed {
                    guard isCurrent(attempt, link: link) else { return }
                    phase = .awaitingPairing(name: hello.name)
                    try await Task.sleep(nanoseconds: 1_500_000_000)
                    guard isCurrent(attempt, link: link) else { return }
                }
            }
            let router = KnownRouter(id: hello.id, name: hello.name, publicKey: connection.routerKey, peripheral: peripheral)
            store.save(router)
            knownRouters = store.routers()

            try? await connection.setTime()
            guard isCurrent(attempt, link: link) else { return }
            // A router of another generation: nothing of it can be shown,
            // but it can still be reached, which is enough to update it.
            let format = try await connection.configFormat()
            guard isCurrent(attempt, link: link) else { return }
            if format != RouterConfig.format {
                mismatch = format < RouterConfig.format ? .routerOlder : .appOlder
                phase = .connected
                automaticFailures = 0
                central.stopScanning()
                listen(to: connection)
                return
            }
            mismatch = nil
            try await reload()
            guard isCurrent(attempt, link: link) else { return }
            let recent: [ActivityEvent] = (try? await connection.activity()) ?? []
            guard isCurrent(attempt, link: link) else { return }
            activity = recent
            phase = .connected
            automaticFailures = 0
            central.stopScanning()
            listen(to: connection)
        } catch {
            // However it ended, an attempt that was cancelled or replaced
            // says nothing: the model is no longer its to clear.
            guard isCurrent(attempt, link: opened) else { return }
            if let sessionError = error as? SessionError, sessionError == .routerKeyMismatch {
                // No automatic retry after a failure: it would fail the same
                // way and put the message up again every few seconds. The
                // user taps the router to try again.
                holdOff = true
                teardown()
                errorMessage = "This router's identity is different from the one this phone paired with. If you have reset the router, forget it under Paired routers and connect again. If you have not, another device may be pretending to be it."
                return
            }
            if automatic {
                // A router restarting after a software update is seen
                // before it answers, and the first tries can run out of
                // time. Four in a row end quietly: scanning goes on, and
                // the next sight of the router starts another. The fifth
                // is told like any other failure.
                automaticFailures += 1
                if automaticFailures < 5 {
                    teardown()
                    return
                }
            }
            // Told once, and no automatic retry after it, as above.
            holdOff = true
            teardown()
            errorMessage = describe(error)
        }
    }

    private func listen(to connection: RouterConnection) {
        eventTask?.cancel()
        eventTask = Task { [weak self] in
            for await event in connection.events {
                guard let self else { return }
                switch event {
                case .status(let status):
                    let revChanged = status.rev != self.config?.rev
                    self.status = status
                    if revChanged {
                        try? await self.reloadConfig()
                        // What each radio is used for is part of what
                        // the radio list says.
                        try? await self.refreshRadios()
                    }
                case .activity(let entry):
                    self.activity.append(entry)
                    if entry.kind == "device" { try? await self.refreshDevices() }
                    if entry.kind == "radio" { try? await self.refreshRadios() }
                case .speedTest(let progress):
                    self.speedTestProgress = progress.phase == "done" ? nil : progress
                case .firmware(let progress):
                    self.firmwareProgress = progress
                }
            }
            // The stream ends when the link is lost.
            guard let self, !Task.isCancelled else { return }
            self.teardown()
        }
    }

    private func teardown() {
        eventTask?.cancel()
        eventTask = nil
        let old = connection
        connection = nil
        // Dropped here and now: an attempt that failed before a session
        // existed has nothing else that would end the Bluetooth connection.
        link?.abandon(BLEError.disconnected)
        link = nil
        Task { await old?.close() }
        status = nil
        mismatch = nil
        pendingEdits.removeAll()
        // Updates still waiting their turn find their edits gone and
        // send nothing.
        updateTail?.cancel()
        updateTail = nil
        setConfirmed(nil)
        devices = []
        radios = []
        speedTestProgress = nil
        firmwareProgress = nil
        phase = .searching
        central.startScanning()
    }

    /// Drops the connection; the app reconnects when the user picks the
    /// router again.
    func disconnect() {
        holdOff = true
        generation += 1
        connectTask?.cancel()
        connectTask = nil
        teardown()
    }

    /// Forgets a paired router on this phone.
    func forget(_ router: KnownRouter) {
        store.remove(routerID: router.id)
        knownRouters = store.routers()
        if connection != nil { disconnect() }
    }

    func cancelConnecting() {
        disconnect()
    }

    // MARK: - Reading

    private func requireConnection() throws -> RouterConnection {
        guard let connection else { throw BLEError.disconnected }
        return connection
    }

    func reload() async throws {
        let connection = try requireConnection()
        let status: RouterStatus = try await connection.status()
        let config: RouterConfig = try await connection.config()
        let devices: [DeviceInfo] = try await connection.devices()
        let radios: [RadioInfo] = try await connection.radios()
        // The connection may have been dropped while the router answered.
        // What it said is then about nothing the model still holds.
        guard self.connection === connection else { throw BLEError.disconnected }
        self.status = status
        setConfirmed(config)
        self.devices = devices
        self.radios = radios
    }

    private func reloadConfig() async throws {
        setConfirmed(try await requireConnection().config())
    }

    func refreshDevices() async throws {
        devices = try await requireConnection().devices()
    }

    func refreshRadios() async throws {
        radios = try await requireConnection().radios()
    }

    /// Asks the router how far an installation is. Progress otherwise
    /// only arrives as it changes, which says nothing to a page opened
    /// in the middle of one. A failure is not announced.
    func refreshFirmwareProgress() async {
        let found: FirmwareProgress? = await fetchQuietly { try await $0.firmwareStatus() }
        if let found {
            firmwareProgress = found
        }
    }

    /// Pull-to-refresh entry point for views.
    func refresh() async {
        await attempt { _ in try await self.reload() }
    }

    // MARK: - Writing

    /// Takes a configuration from the router as the new truth and shows
    /// it, with whatever edits are still on their way applied on top.
    private func setConfirmed(_ fresh: RouterConfig?) {
        confirmedConfig = fresh
        showConfig()
    }

    private func showConfig() {
        guard var shown = confirmedConfig else {
            config = nil
            return
        }
        for item in pendingEdits {
            item.edit(&shown)
        }
        if shown != config {
            config = shown
        }
    }

    /// Applies an edit to the configuration. The edit shows at once and is
    /// taken back if the router refuses it. If the router's copy changed
    /// in the meantime the edit is re-applied to the fresh copy once.
    /// Updates reach the router one at a time, in the order they were
    /// made.
    @discardableResult
    func update(_ edit: @escaping (inout RouterConfig) -> Void) async -> Bool {
        let id = nextEditID
        nextEditID += 1
        pendingEdits.append((id: id, edit: edit))
        showConfig()
        // Written at the same time, two updates would both build on the
        // same copy and the router would refuse the later one; a third
        // could be refused twice and fail.
        let previous: Task<Bool, Never>? = updateTail
        let work: Task<Bool, Never> = Task {
            _ = await previous?.value
            return await self.write(id: id, edit: edit)
        }
        updateTail = work
        return await work.value
    }

    /// Sends one pending edit to the router and takes what the router
    /// then holds as the new truth.
    private func write(id: Int, edit: @escaping (inout RouterConfig) -> Void) async -> Bool {
        // The connection was dropped while the edit waited its turn, and
        // the edit with it: there is nothing to send.
        guard pendingEdits.contains(where: { $0.id == id }) else { return false }
        let ok = await attempt { connection in
            for attemptNumber in 0..<2 {
                // The first try builds on the copy already here, which
                // saves fetching the whole configuration over Bluetooth;
                // if the router has moved on, the second fetches it.
                var next: RouterConfig
                if attemptNumber == 0, let known = self.confirmedConfig {
                    next = known
                } else {
                    next = try await connection.config()
                }
                edit(&next)
                do {
                    _ = try await connection.setConfig(next)
                    break
                } catch let error as RouterError where error.code == RouterError.conflict && attemptNumber == 0 {
                    continue
                }
            }
            let fresh = try await connection.config()
            self.pendingEdits.removeAll(where: { $0.id == id })
            self.setConfirmed(fresh)
            self.status = try await connection.status()
        }
        if !ok {
            pendingEdits.removeAll(where: { $0.id == id })
            showConfig()
        }
        return ok
    }

    /// Asks the router what a change would do, without making it. Nil
    /// when the question could not be asked; that is not worth an alert,
    /// the change itself will say what is wrong when it is applied.
    func check(_ edit: @escaping (inout RouterConfig) -> Void) async -> ConfigCheck? {
        guard let connection, var next = confirmedConfig else { return nil }
        edit(&next)
        return try? await connection.checkConfig(next)
    }

    /// Chooses the radio that joins upstream Wi-Fi networks ("" for none).
    /// The router rearranges the other radios around it in the same step.
    @discardableResult
    func setWifiUplink(radio: String) async -> Bool {
        await attempt { connection in
            try await connection.setWifiUplink(radio: radio)
            self.setConfirmed(try await connection.config())
            self.radios = try await connection.radios()
            self.status = try await connection.status()
        }
    }

    /// Runs a router operation, turning failures into `errorMessage`.
    @discardableResult
    func attempt(_ operation: @escaping (RouterConnection) async throws -> Void) async -> Bool {
        do {
            try await operation(try requireConnection())
            return true
        } catch {
            errorMessage = describe(error)
            return false
        }
    }

    /// Like `attempt` and `fetch`, for what is done without being asked
    /// (loading a list as a screen appears): a failure is not announced.
    @discardableResult
    func quietly(_ operation: @escaping (RouterConnection) async throws -> Void) async -> Bool {
        do {
            try await operation(try requireConnection())
            return true
        } catch {
            return false
        }
    }

    func fetchQuietly<T>(_ operation: @escaping (RouterConnection) async throws -> T) async -> T? {
        try? await operation(try requireConnection())
    }

    /// Runs a router operation that returns a value; nil on failure.
    func fetch<T>(_ operation: @escaping (RouterConnection) async throws -> T) async -> T? {
        do {
            return try await operation(try requireConnection())
        } catch {
            errorMessage = describe(error)
            return nil
        }
    }

    func setDevice(_ settings: DeviceSettings) async {
        // Shown at once, like configuration edits; the router's answer
        // replaces it, or the old state comes back if it refuses.
        let before = devices
        if let index = devices.firstIndex(where: { $0.mac == settings.mac }) {
            devices[index].name = settings.name
            devices[index].route = settings.route
            devices[index].paused = settings.paused
            devices[index].ip = settings.ip
            devices[index].noAdblock = settings.noAdblock
        }
        let ok = await attempt { connection in
            try await connection.setDevice(settings)
            self.devices = try await connection.devices()
            self.setConfirmed(try await connection.config())
        }
        if !ok {
            devices = before
        }
    }

    private func describe(_ error: Error) -> String {
        if let error = error as? RouterError { return error.msg }
        if let error = error as? LocalizedError, let text = error.errorDescription { return text }
        if error is NoiseError || error is SessionError { return "The secure connection to the router failed. Move closer to the router and try again." }
        return error.localizedDescription
    }

    /// Runs a speed test through the given path ("direct" or "vpn:<id>").
    /// `speedTestProgress` updates while it runs.
    func runSpeedTest(via: String) async -> SpeedTestResult? {
        speedTestProgress = SpeedTestProgress(phase: "latency", mbps: 0)
        let result = await fetch { try await $0.runSpeedTest(via: via) }
        speedTestProgress = nil
        return result
    }

    // MARK: - Lookups for views

    func device(_ mac: String) -> DeviceInfo? { devices.first { $0.mac == mac } }

    func deviceName(_ mac: String) -> String { device(mac)?.displayName ?? mac.uppercased() }

    func vpnName(_ id: String) -> String { config?.vpns.first { $0.id == id }?.name ?? "Unknown VPN" }

    func networkName(_ id: String) -> String { config?.networkName(id) ?? id }

    /// What a radio is used for. The configuration says so first: it is
    /// what the app has just changed, where the radio list is only as
    /// fresh as the last time it was fetched.
    func role(of radio: RadioInfo) -> RadioRole {
        config?.radios.first(where: { $0.id == radio.id })?.role ?? radio.role
    }

    /// The radio that joins upstream Wi-Fi networks, if one is set to.
    var uplinkRadio: RadioInfo? {
        radios.first(where: { radio in
            let role = self.role(of: radio)
            return radio.present && (role == .wan || role == .apWan)
        })
    }

    /// More than one network exists, so it is worth saying which one
    /// something belongs to.
    var hasSeveralNetworks: Bool { (config?.networks.count ?? 0) > 1 }

    /// A readable label for an action string ("direct", "block", "vpn:<id>").
    func actionLabel(_ raw: String) -> String {
        switch RouteAction(raw) {
        case .direct: return "Direct"
        case .block: return "Blocked"
        case .vpn(let id): return vpnName(id)
        case nil:
            if raw == "adblock" { return "Ad blocked" }
            return raw.isEmpty ? "Default" : raw
        }
    }

    /// Every action a rule or device can choose, in display order.
    var availableActions: [RouteAction] {
        let tunnels: [RouteAction] = (config?.vpns ?? []).map { RouteAction.vpn($0.id) }
        return [RouteAction.direct] + tunnels + [RouteAction.block]
    }

    /// A short random identifier for new rules and VPN profiles.
    static func newID() -> String {
        let alphabet = Array("abcdefghijklmnopqrstuvwxyz0123456789")
        return String((0..<8).map { _ in alphabet.randomElement()! })
    }
}
