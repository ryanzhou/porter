import Combine
import Foundation
import PorterKit
import UIKit

/// The app's single source of truth: the connection to the router and the
/// router state the views show and edit.
@MainActor
final class AppModel: ObservableObject {
    enum Phase: Equatable {
        /// Looking for a router (none paired, or the paired one does not answer).
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
    /// Which way the router is reached right now: over one of its own
    /// networks or over Bluetooth. Nil while it is not.
    @Published private(set) var link: LinkKind?
    /// The connection has been gone for a moment and is being brought
    /// back. What is on screen stays; this is for the sign that says so.
    @Published private(set) var reconnecting = false
    /// A message for the user; views present it as an alert and clear it.
    @Published var errorMessage: String?

    let central = BLECentral()
    private let store = KeyStore()
    /// The connection to the router. It keeps itself up over whichever
    /// link works, and for as long as it exists it is trying to; the
    /// model follows what it reports.
    private var supervisor: ConnectionSupervisor?
    /// The router the supervisor is for, as Bluetooth knows it.
    private var target: (peripheral: UUID, name: String)?
    /// Reads what the supervisor reports.
    private var watcher: Task<Void, Never>?
    /// Takes the router's events one at a time.
    private var eventTask: Task<Void, Never>?
    private var eventSink: AsyncStream<RouterEvent>.Continuation?
    /// Brings the model up to date after a link has come up.
    private var syncTask: Task<Void, Never>?
    /// The configuration as the router last reported it. `config`, which
    /// the views show, is this with the edits applied that have been sent
    /// and not yet confirmed: a control shows its new value the moment it
    /// is touched, not a round trip later.
    private var confirmedConfig: RouterConfig?
    private var pendingEdits: [(id: Int, edit: (inout RouterConfig) -> Void)] = []
    private var nextEditID = 0
    /// The update started last. The next one waits for it to finish, so
    /// that each is written on top of what the one before it left on the
    /// router and none finds the router a revision ahead of it.
    private var updateTail: Task<Bool, Never>?
    private var subscriptions: Set<AnyCancellable> = []
    /// Set when the user disconnects on purpose, and after a failure that
    /// was announced, to stop connecting again unasked.
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
        guard supervisor == nil else { return }
        if isDemo {
            watch(ConnectionSupervisor(demo: DemoRouter.makeConnection()))
            return
        }
        central.startScanning()
        // The router used last is tried straight away, over its network
        // and over Bluetooth, without waiting for it to show up in a
        // scan: with Bluetooth off it never would.
        if !holdOff, let router = knownRouters.last {
            open(peripheral: router.peripheral, name: router.name, automatic: true)
        }
    }

    /// The app has come to the front: the moment to see whether the link
    /// still works, and whether there is a better one.
    func foreground() {
        guard !isDemo else { return }
        if let supervisor {
            Task { await supervisor.poke() }
        } else {
            start()
        }
    }

    var isConnected: Bool { phase == .connected }

    // MARK: - Connecting

    /// A paired router has been seen over Bluetooth, or Bluetooth has
    /// just become usable.
    private func autoConnect(_ routers: [DiscoveredRouter]) {
        guard !isDemo, !holdOff, phase == .searching else { return }
        if let supervisor, let target, routers.isEmpty || routers.contains(where: { $0.id == target.peripheral }) {
            // The router the app is already looking for: now is the
            // moment to try.
            Task { await supervisor.poke() }
            return
        }
        guard let match = routers.first(where: { seen in knownRouters.contains { $0.peripheral == seen.id } }) else { return }
        // Another paired router than the one used last is here.
        open(peripheral: match.id, name: match.name, automatic: true)
    }

    /// Connects to a router picked from the list of those in range.
    func connect(to router: DiscoveredRouter) {
        holdOff = false
        open(peripheral: router.id, name: router.name, automatic: false)
    }

    /// Connects to a paired router, which need not be in Bluetooth range:
    /// it may answer on the network the phone is on.
    func connect(to router: KnownRouter) {
        holdOff = false
        open(peripheral: router.peripheral, name: router.name, automatic: false)
    }

    /// Starts a connection to a router in place of whatever there was.
    /// `automatic` marks the one nobody asked for.
    private func open(peripheral: UUID, name: String, automatic: Bool) {
        close()
        let identity: Identity
        do {
            identity = try store.identity()
        } catch {
            holdOff = true
            errorMessage = describe(error)
            return
        }
        let known: KnownRouter? = knownRouters.first { $0.peripheral == peripheral }
        target = (peripheral: peripheral, name: name)
        if !automatic { phase = .connecting(name: name) }
        watch(ConnectionSupervisor(
            identity: identity, routerKey: known?.publicKey, endpoints: known?.endpoints,
            dialer: RouterDialer(central: central, peripheral: peripheral),
            phoneName: UIDevice.current.name, automatic: automatic))
    }

    private func watch(_ supervisor: ConnectionSupervisor) {
        self.supervisor = supervisor
        // The router's events are taken one at a time, each with whatever
        // it makes the model fetch, as they were over a single link.
        let (events, sink) = AsyncStream<RouterEvent>.makeStream()
        eventSink = sink
        eventTask = Task { [weak self] in
            for await event in events {
                await self?.take(event)
            }
        }
        watcher = Task { [weak self] in
            await supervisor.start()
            for await event in supervisor.events {
                guard let self, self.supervisor === supervisor else { return }
                switch event {
                case .state(let state): self.follow(state, of: supervisor)
                case .router(let event): sink.yield(event)
                case .identified(let hello, let routerKey): self.remember(hello, routerKey: routerKey)
                case .endpoints(let endpoints): self.remember(endpoints)
                case .failed(let error): self.failed(error)
                }
            }
        }
    }

    private func follow(_ state: ConnectionSupervisor.State, of supervisor: ConnectionSupervisor) {
        switch state {
        case .searching:
            // Nothing has answered for a while. What is on screen is of a
            // router that is not there; the supervisor goes on looking.
            if phase != .searching {
                clear()
                phase = .searching
            }
            central.startScanning()
        case .connecting:
            if phase == .searching, let target { phase = .connecting(name: target.name) }
        case .awaitingPairing(let name):
            phase = .awaitingPairing(name: name)
        case .connected(let kind):
            link = kind
            reconnecting = false
            if phase == .searching, let target { phase = .connecting(name: target.name) }
            syncTask?.cancel()
            syncTask = Task { [weak self] in await self?.linkCameUp(supervisor) }
        case .reconnecting(let noticeable):
            // The screens stay as they are. Calls wait for the new link.
            reconnecting = noticeable
            if noticeable { link = nil }
        case .stopped:
            break
        }
    }

    private func isCurrent(_ supervisor: ConnectionSupervisor) -> Bool {
        self.supervisor === supervisor && !Task.isCancelled
    }

    /// A link has come up. The first time, everything is fetched and the
    /// screens appear. After a change of link the same is fetched again
    /// without anything showing, because events may have been missed on
    /// the way from one link to the other.
    private func linkCameUp(_ supervisor: ConnectionSupervisor) async {
        let first: Bool = phase != .connected
        do {
            if first { try? await supervisor.setTime() }
            // A router of another generation: nothing of it can be shown,
            // but it can still be reached, which is enough to update it.
            let format: Int = try await supervisor.configFormat()
            guard isCurrent(supervisor) else { return }
            if format != RouterConfig.format {
                mismatch = format < RouterConfig.format ? .routerOlder : .appOlder
                phase = .connected
                central.stopScanning()
                return
            }
            mismatch = nil
            try await reload()
            guard isCurrent(supervisor) else { return }
            let recent: [ActivityEvent] = (try? await supervisor.activity()) ?? []
            guard isCurrent(supervisor) else { return }
            activity = recent
            phase = .connected
            central.stopScanning()
        } catch {
            guard isCurrent(supervisor) else { return }
            // The link went again before everything was read. The
            // supervisor is bringing up another, and this starts over
            // when it has.
            if isLinkFailure(error) || error is SupervisorError { return }
            if first { failed(error) }
        }
    }

    /// Takes one event from the router.
    private func take(_ event: RouterEvent) async {
        switch event {
        case .status(let status):
            let revChanged = status.rev != config?.rev
            self.status = status
            if revChanged {
                try? await reloadConfig()
                // What each radio is used for is part of what
                // the radio list says.
                try? await refreshRadios()
            }
        case .activity(let entry):
            activity.append(entry)
            if entry.kind == "device" { try? await refreshDevices() }
            if entry.kind == "radio" { try? await refreshRadios() }
        case .speedTest(let progress):
            speedTestProgress = progress.phase == "done" ? nil : progress
        case .firmware(let progress):
            firmwareProgress = progress
        }
    }

    /// Keeps what a router said of itself as a link came up: its name,
    /// the key it proved, and where it can be reached over its networks.
    private func remember(_ hello: HelloInfo, routerKey: Data) {
        guard hello.paired, let target else { return }
        store.save(KnownRouter(
            id: hello.id, name: hello.name, publicKey: routerKey, peripheral: target.peripheral,
            endpoints: hello.control ?? ControlEndpoints()))
        knownRouters = store.routers()
    }

    /// The router's addresses have changed, or it has stopped or started
    /// listening on them.
    private func remember(_ endpoints: ControlEndpoints) {
        guard let target, var router = knownRouters.first(where: { $0.peripheral == target.peripheral }) else { return }
        router.endpoints = endpoints
        store.save(router)
        knownRouters = store.routers()
    }

    /// The supervisor has stopped over something worth telling. Told
    /// once, and no connecting again unasked after it: it would fail the
    /// same way and put the message up again every few seconds. The user
    /// picks the router to try again.
    private func failed(_ error: Error) {
        holdOff = true
        close()
        if let sessionError = error as? SessionError, sessionError == .routerKeyMismatch {
            errorMessage = "This router's identity is different from the one this phone paired with. If you have reset the router, forget it under Paired routers and connect again. If you have not, another device may be pretending to be it."
        } else {
            errorMessage = describe(error)
        }
    }

    /// Forgets everything of the router that is on screen.
    private func clear() {
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
        link = nil
        reconnecting = false
    }

    /// Ends the connection and everything that follows it.
    private func close() {
        watcher?.cancel()
        watcher = nil
        syncTask?.cancel()
        syncTask = nil
        eventTask?.cancel()
        eventTask = nil
        eventSink?.finish()
        eventSink = nil
        if let old = supervisor {
            Task { await old.stop() }
        }
        supervisor = nil
        target = nil
        clear()
        phase = .searching
        if !isDemo { central.startScanning() }
    }

    /// Drops the connection; the app reconnects when the user picks the
    /// router again.
    func disconnect() {
        holdOff = true
        close()
    }

    /// Forgets a paired router on this phone.
    func forget(_ router: KnownRouter) {
        store.remove(routerID: router.id)
        knownRouters = store.routers()
        if target?.peripheral == router.peripheral { disconnect() }
    }

    func cancelConnecting() {
        disconnect()
    }

    // MARK: - Reading

    private func requireConnection() throws -> ConnectionSupervisor {
        guard let supervisor else { throw SupervisorError.notConnected }
        return supervisor
    }

    func reload() async throws {
        let connection = try requireConnection()
        let status: RouterStatus = try await connection.status()
        let config: RouterConfig = try await connection.config()
        let devices: [DeviceInfo] = try await connection.devices()
        let radios: [RadioInfo] = try await connection.radios()
        // The connection may have been dropped while the router answered.
        // What it said is then about nothing the model still holds.
        guard self.supervisor === connection else { throw SupervisorError.notConnected }
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

    /// Reads the status again, for what the router does not announce as
    /// it changes: counters. A failure is not announced.
    func refreshStatus() async {
        let found: RouterStatus? = await fetchQuietly { try await $0.status() }
        if let found {
            status = found
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
    /// in the meantime the edit is re-applied to the fresh copy once, and
    /// if the link goes with the write on it the write is seen through
    /// over the next one (`RouterAPI.writeConfig`). Updates reach the
    /// router one at a time, in the order they were made.
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
            // Built on the copy already here, which saves fetching the
            // whole configuration first.
            let fresh: RouterConfig = try await connection.writeConfig(known: self.confirmedConfig, edit)
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
        guard let supervisor, var next = confirmedConfig else { return nil }
        edit(&next)
        return try? await supervisor.checkConfig(next)
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
    func attempt(_ operation: @escaping (ConnectionSupervisor) async throws -> Void) async -> Bool {
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
    func quietly(_ operation: @escaping (ConnectionSupervisor) async throws -> Void) async -> Bool {
        do {
            try await operation(try requireConnection())
            return true
        } catch {
            return false
        }
    }

    func fetchQuietly<T>(_ operation: @escaping (ConnectionSupervisor) async throws -> T) async -> T? {
        try? await operation(try requireConnection())
    }

    /// Runs a router operation that returns a value; nil on failure.
    func fetch<T>(_ operation: @escaping (ConnectionSupervisor) async throws -> T) async -> T? {
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
        if error is NoiseError || error is SessionError { return "The secure connection to the router failed. Try again." }
        return error.localizedDescription
    }

    /// Runs a speed test through the given path ("direct", "vpn:<id>", or
    /// one internet source: `RouterConfig.speedTestPath(source:)`).
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
