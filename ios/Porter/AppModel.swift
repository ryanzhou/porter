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

    @Published private(set) var phase: Phase = .searching
    @Published private(set) var status: RouterStatus?
    @Published private(set) var config: RouterConfig?
    @Published private(set) var devices: [DeviceInfo] = []
    @Published private(set) var radios: [RadioInfo] = []
    @Published private(set) var activity: [ActivityEvent] = []
    @Published private(set) var knownRouters: [KnownRouter] = []
    /// Progress of the speed test currently running, if any.
    @Published private(set) var speedTestProgress: SpeedTestProgress?
    /// A message for the user; views present it as an alert and clear it.
    @Published var errorMessage: String?

    let central = BLECentral()
    private let store = KeyStore()
    private var connection: RouterConnection?
    private var link: BLELink?
    private var connectTask: Task<Void, Never>?
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
        connect(to: match)
    }

    func connect(to router: DiscoveredRouter) {
        holdOff = false
        connectTask?.cancel()
        connectTask = Task { [weak self] in
            await self?.runConnection(peripheral: router.id, name: router.name)
            self?.connectTask = nil
        }
    }

    private func runConnection(peripheral: UUID, name: String) async {
        phase = .connecting(name: name)
        do {
            let link = try await central.connect(to: peripheral)
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
                link?.abandon()
            }
            defer { deadline.cancel() }
            let connection = try await RouterConnection.connect(
                transport: link, identity: try store.identity(), expectedRouterKey: known?.publicKey)
            self.connection = connection
            var hello = try await connection.hello()
            deadline.cancel()

            // Not paired yet: ask, and keep asking while the user goes to
            // press the button.
            while !hello.paired {
                do {
                    try await connection.pair(name: UIDevice.current.name)
                    hello = try await connection.hello()
                } catch let error as RouterError where error.code == RouterError.pairingClosed {
                    phase = .awaitingPairing(name: hello.name)
                    try await Task.sleep(nanoseconds: 1_500_000_000)
                }
            }
            let router = KnownRouter(id: hello.id, name: hello.name, publicKey: connection.routerKey, peripheral: peripheral)
            store.save(router)
            knownRouters = store.routers()

            try? await connection.setTime()
            try await reload()
            activity = (try? await connection.activity()) ?? []
            phase = .connected
            central.stopScanning()
            listen(to: connection)
        } catch is CancellationError {
            teardown()
        } catch SessionError.routerKeyMismatch {
            teardown()
            errorMessage = "This router's identity is different from the one this phone paired with. If you reset the router, forget it here and pair again. Otherwise something may be impersonating it."
        } catch {
            teardown()
            if !Task.isCancelled { errorMessage = describe(error) }
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
                    if revChanged { try? await self.reloadConfig() }
                case .activity(let entry):
                    self.activity.append(entry)
                    if entry.kind == "device" { try? await self.refreshDevices() }
                case .speedTest(let progress):
                    self.speedTestProgress = progress.phase == "done" ? nil : progress
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
        config = nil
        devices = []
        radios = []
        speedTestProgress = nil
        phase = .searching
        central.startScanning()
    }

    /// Drops the connection; the app reconnects when the user picks the
    /// router again.
    func disconnect() {
        holdOff = true
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
        status = try await connection.status()
        config = try await connection.config()
        devices = try await connection.devices()
        radios = try await connection.radios()
    }

    private func reloadConfig() async throws {
        config = try await requireConnection().config()
    }

    func refreshDevices() async throws {
        devices = try await requireConnection().devices()
    }

    func refreshRadios() async throws {
        radios = try await requireConnection().radios()
    }

    /// Pull-to-refresh entry point for views.
    func refresh() async {
        await attempt { _ in try await self.reload() }
    }

    // MARK: - Writing

    /// Applies an edit to the configuration. If the router's copy changed
    /// in the meantime the edit is re-applied to the fresh copy once.
    @discardableResult
    func update(_ edit: @escaping (inout RouterConfig) -> Void) async -> Bool {
        await attempt { connection in
            for attemptNumber in 0..<2 {
                var next = try await connection.config()
                edit(&next)
                do {
                    _ = try await connection.setConfig(next)
                    break
                } catch let error as RouterError where error.code == RouterError.conflict && attemptNumber == 0 {
                    continue
                }
            }
            self.config = try await connection.config()
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
        await attempt { connection in
            try await connection.setDevice(settings)
            self.devices = try await connection.devices()
            self.config = try await connection.config()
        }
    }

    private func describe(_ error: Error) -> String {
        if let error = error as? RouterError { return error.msg }
        if let error = error as? LocalizedError, let text = error.errorDescription { return text }
        if error is NoiseError || error is SessionError { return "The secure connection to the router failed." }
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
