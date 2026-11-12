import Foundation

/// The two ways to a router.
public enum LinkKind: String, Sendable, Equatable {
    case bluetooth
    /// TCP to one of the router's addresses on its own networks.
    case network
}

/// Opens links for a `ConnectionSupervisor`. Both calls give up when their
/// task is cancelled, which is how an attempt that lost to another ends.
public protocol LinkDialer {
    func openBluetooth() async throws -> PacketTransport
    func openNetwork(host: String, port: Int, timeout: TimeInterval) async throws -> PacketTransport
}

/// What a dialer throws when the router is not there to connect to, as
/// opposed to there and failing: out of range, or Bluetooth switched off.
/// A router that is not there is nothing to tell anyone about.
public struct RouterNotFound: Error, LocalizedError {
    public let underlying: Error

    public init(_ underlying: Error) {
        self.underlying = underlying
    }

    public var errorDescription: String? {
        (underlying as? LocalizedError)?.errorDescription ?? underlying.localizedDescription
    }
}

public enum SupervisorError: Error, Equatable, LocalizedError {
    /// There is no link to the router, and none came up in the time a
    /// call waits for one.
    case notConnected

    public var errorDescription: String? { "The router is not answering at the moment." }
}

/// Keeps one connection to a router up over whichever of two links works:
/// the router's own network when the phone is on it, Bluetooth otherwise.
/// To everything above it this is the connection: calls are made on it and
/// events come from it, whichever link is carrying them and however often
/// that changes.
///
/// - At the start every way to the router is tried at once, and the first
///   to answer is used.
/// - On Bluetooth it keeps looking for the network, which is much faster,
///   and moves over without a gap: the new link is up before the old one
///   is let go, and calls under way finish where they started.
/// - A link that is lost is replaced at once by whatever answers. Calls
///   made meanwhile wait for it, and calls that only read are made again
///   if they were lost with the link.
/// - Only when nothing has answered for a while does it report that the
///   router is gone, and it goes on looking.
public actor ConnectionSupervisor: RouterAPI {
    /// Every interval the supervisor works to, in seconds. The defaults
    /// are what the app runs with; tests shorten them.
    public struct Timing: Sendable {
        /// How long a TCP connection may take to open. The router is on
        /// the same network or it is not; there is nothing to wait for.
        public var networkConnect: TimeInterval = 2
        /// How long the handshake and the first exchange may take on a
        /// link that has opened.
        public var networkSecure: TimeInterval = 5
        public var bluetoothSecure: TimeInterval = 15
        /// How often, on Bluetooth, the network is tried again.
        public var upgradeEvery: TimeInterval = 30
        /// How often the network link is asked whether it still works, and
        /// how long the answer may take. Together they bound how long a
        /// dead link goes unnoticed.
        public var heartbeatEvery: TimeInterval = 2
        public var heartbeatWithin: TimeInterval = 3
        /// How long a call made without a link waits for one.
        public var callWait: TimeInterval = 10
        /// How long a link that has been moved away from may take to
        /// finish the calls still on it. Each call has a deadline of its
        /// own, and this is past the longest of them.
        public var drain: TimeInterval = 100
        /// How long the link may be gone before it is worth a sign.
        public var noticeAfter: TimeInterval = 2
        /// How long the link may be gone before the router counts as gone.
        public var giveUpAfter: TimeInterval = 20
        /// The pause between attempts while a lost link is being replaced,
        /// and while the router is gone.
        public var retryEvery: TimeInterval = 1
        public var searchEvery: TimeInterval = 5
        /// How often pairing is asked for again while the person goes to
        /// press the router's button.
        public var pairingEvery: TimeInterval = 1.5

        public init() {}
    }

    public enum State: Equatable, Sendable {
        /// Nothing answers. The supervisor keeps trying.
        case searching
        /// The router has been reached and the first exchange is under way.
        case connecting
        /// The router wants its power button pressed before it accepts
        /// this phone.
        case awaitingPairing(name: String)
        /// Calls go over this link.
        case connected(LinkKind)
        /// The link is gone and another is being brought up. Calls wait.
        /// `noticeable` once that has gone on long enough to show.
        case reconnecting(noticeable: Bool)
        /// Over, by `stop()` or after `Event.failed`.
        case stopped
    }

    public enum Event {
        case state(State)
        /// From the router, over the link in use.
        case router(RouterEvent)
        /// A link has come up: what the router says of itself, and the key
        /// it proved to hold.
        case identified(HelloInfo, routerKey: Data)
        /// Where the router says it can be reached over its networks.
        case endpoints(ControlEndpoints)
        /// The supervisor has stopped over something the person should
        /// hear about.
        case failed(Error)
    }

    /// States, router events and the rest, in order. A `connected` state
    /// always means a link other than the one before: events may have
    /// been missed in between.
    public nonisolated let events: AsyncStream<Event>
    private let sink: AsyncStream<Event>.Continuation

    private final class Link {
        let id: Int
        let kind: LinkKind
        let connection: RouterConnection
        /// Calls made on the link that have not come back.
        var inFlight = 0
        var pump: Task<Void, Never>?
        var heartbeat: Task<Void, Never>?

        init(id: Int, kind: LinkKind, connection: RouterConnection) {
            self.id = id
            self.kind = kind
            self.connection = connection
        }
    }

    private struct Candidate {
        let kind: LinkKind
        var host: String = ""
        var port: Int = 0
    }

    /// A link that has opened, been secured and said hello.
    private struct Opened {
        let kind: LinkKind
        let connection: RouterConnection
        let hello: HelloInfo
    }

    private enum Outcome {
        case up(Opened)
        case failed(LinkKind, Error)
    }

    /// The transport of an attempt, where whoever ends the attempt (its
    /// deadline, its cancellation) can reach it from any thread.
    private final class TransportBox: @unchecked Sendable {
        private let lock = NSLock()
        private var transport: PacketTransport?
        private var closed = false

        /// Takes the transport; false if the attempt is already over, in
        /// which case the transport is closed here.
        func hold(_ transport: PacketTransport) -> Bool {
            lock.lock()
            let over: Bool = closed
            if !over { self.transport = transport }
            lock.unlock()
            if over { transport.close() }
            return !over
        }

        func close() {
            lock.lock()
            closed = true
            let held: PacketTransport? = transport
            transport = nil
            lock.unlock()
            held?.close()
        }
    }

    private let identity: Identity
    private let dialer: LinkDialer?
    private let phoneName: String
    /// Nobody asked for this connection: the app makes it because it
    /// knows the router. Its failures are not announced until they repeat.
    private let automatic: Bool
    private let timing: Timing
    /// The key the router has to prove. Nil only until the first session
    /// with a router that is new to this phone.
    private var expectedKey: Data?
    private var endpoints: ControlEndpoints

    private var state: State = .searching
    private var active: Link?
    private var draining: [Link] = []
    private var nextLinkID = 0
    private var stopped = false
    private var runner: Task<Void, Never>?
    /// When the link was lost, in uptime nanoseconds; nil while there is
    /// one, and before the first.
    private var lostAt: UInt64?
    private var everConnected = false
    /// The network is worth trying now: it has not been since the
    /// addresses changed, or the time for another look has come.
    private var probeNow = true
    /// A session that is waiting to be paired.
    private var pairing: RouterConnection?
    /// The attempts under way, and those that have ended and wait for the
    /// main loop to look at them.
    private var flights: [Int: (kind: LinkKind, task: Task<Void, Never>)] = [:]
    private var landed: [(id: Int, outcome: Outcome)] = []
    /// When each kind of link is next due for an attempt while there is
    /// no link, in uptime nanoseconds.
    private var nextBluetooth: UInt64 = 0
    private var nextNetwork: UInt64 = 0
    /// Bluetooth attempts in a row that found the router and failed all
    /// the same.
    private var failures = 0
    private var lastBluetoothError: Error?
    private var lastNetworkError: Error?

    /// The main loop, asleep until a timer or `wake()`.
    private var sleeper: (token: Int, continuation: CheckedContinuation<Void, Never>)?
    private var wakePending = false
    private var nextToken = 0
    /// Calls waiting for a link.
    private var linkWaiters: [Int: CheckedContinuation<Bool, Never>] = [:]

    /// - Parameters:
    ///   - routerKey: the key pinned for the router, nil for one this
    ///     phone has not paired with (which only Bluetooth can reach).
    ///   - endpoints: where the router was last known to be on its
    ///     networks.
    ///   - phoneName: what the router calls this phone if it pairs.
    ///   - automatic: see the property.
    public init(
        identity: Identity, routerKey: Data?, endpoints: ControlEndpoints?, dialer: LinkDialer,
        phoneName: String, automatic: Bool, timing: Timing = Timing()
    ) {
        self.identity = identity
        self.expectedKey = routerKey
        self.endpoints = endpoints ?? ControlEndpoints()
        self.dialer = dialer
        self.phoneName = phoneName
        self.automatic = automatic
        self.timing = timing
        (events, sink) = AsyncStream<Event>.makeStream()
    }

    /// A supervisor over a connection that needs none: the sample router
    /// of demo mode.
    public init(demo connection: RouterConnection) {
        identity = Identity()
        expectedKey = nil
        endpoints = ControlEndpoints()
        dialer = nil
        phoneName = ""
        automatic = true
        timing = Timing()
        (events, sink) = AsyncStream<Event>.makeStream()
        active = Link(id: 0, kind: .network, connection: connection)
        nextLinkID = 1
        everConnected = true
        state = .connected(.network)
        sink.yield(.state(.connected(.network)))
    }

    /// Starts connecting. Does nothing the second time.
    public func start() {
        guard runner == nil, !stopped, dialer != nil else { return }
        runner = Task { await self.run() }
    }

    /// Ends everything: links, waiting calls, the stream of events.
    public func stop() {
        shutdown()
    }

    /// Something has happened that makes now a good moment to look: the
    /// app has come to the front, the router has been seen over Bluetooth.
    /// A network link is asked whether it still works; on Bluetooth the
    /// network is tried; without a link the next attempt starts now.
    public func poke() {
        guard !stopped else { return }
        probeNow = true
        nextBluetooth = 0
        if let link = active, link.kind == .network {
            let id: Int = link.id
            let connection: RouterConnection = link.connection
            let within: TimeInterval = timing.heartbeatWithin
            Task {
                if let error = await Self.beat(connection, within: within) {
                    self.linkClosed(id, error)
                }
            }
        }
        wake()
    }

    // MARK: - Calls

    /// Calls that change nothing on the router, and so can be made a
    /// second time when the first was lost with its link.
    private static let repeatable: Set<String> = [
        "hello", "ping", "status.get", "config.get", "config.check", "events.list", "radios.list", "wifi.scan",
        "devices.list", "device.queries", "stats.get", "stats.top", "flows.list", "speedtest.history",
        "pairing.list", "firmware.check", "firmware.status", "system.logs", "system.set_time",
        "vpn.parse", "vpn.genkey", "vpn.pubkey",
    ]

    public func call<P: Encodable, R: Decodable>(_ method: String, _ params: P) async throws -> R {
        var repeated = false
        while true {
            let link: Link = try await linkForCall()
            link.inFlight += 1
            do {
                let result: R = try await link.connection.call(method, params)
                finished(link)
                return result
            } catch {
                finished(link)
                // Anything else than the link having gone is the call's
                // own failure, on a link that works.
                guard isLinkFailure(error), await link.connection.isClosed else { throw error }
                linkClosed(link.id, error)
                if repeated || stopped || !Self.repeatable.contains(method) { throw error }
                repeated = true
            }
        }
    }

    /// The link to make a call on. Without one, waits for the one being
    /// brought up, for as long as a call may wait.
    private func linkForCall() async throws -> Link {
        while true {
            if stopped { throw SupervisorError.notConnected }
            if let link = active {
                // A link that has ended without the supervisor having
                // heard yet: nothing was sent on it, so nothing is lost
                // by looking first.
                guard await link.connection.isClosed else { return link }
                linkClosed(link.id, SessionError.closed)
                continue
            }
            guard case .reconnecting = state else { throw SupervisorError.notConnected }
            let token: Int = nextToken
            nextToken += 1
            let limit: UInt64 = Self.nanoseconds(timing.callWait)
            let timer = Task {
                do {
                    try await Task.sleep(nanoseconds: limit)
                } catch {
                    return // cancelled: a link came up, or none will
                }
                self.releaseWaiter(token, false)
            }
            let up: Bool = await withCheckedContinuation { (c: CheckedContinuation<Bool, Never>) in linkWaiters[token] = c }
            timer.cancel()
            if !up { throw SupervisorError.notConnected }
        }
    }

    private func releaseWaiter(_ token: Int, _ up: Bool) {
        linkWaiters.removeValue(forKey: token)?.resume(returning: up)
    }

    private func releaseWaiters(_ up: Bool) {
        let waiting = linkWaiters
        linkWaiters.removeAll()
        for (_, waiter) in waiting { waiter.resume(returning: up) }
    }

    /// A call has come back on a link. One that was only kept for its
    /// calls goes with the last of them.
    private func finished(_ link: Link) {
        link.inFlight -= 1
        guard link.inFlight == 0, let index = draining.firstIndex(where: { $0 === link }) else { return }
        draining.remove(at: index)
        let connection: RouterConnection = link.connection
        Task { await connection.close() }
    }

    // MARK: - The main loop

    /// One pass: take in the attempts that have ended, start the ones
    /// that are due, sleep until something happens or is due again.
    ///
    /// Attempts are not made in rounds that have to end before the next
    /// begins. A Bluetooth attempt at a router that is out of range stays
    /// open for a long time, and the network must be tried again every
    /// second meanwhile: it is the one that comes back.
    private func run() async {
        while !stopped {
            // What has ended since the last pass.
            var winner: Opened?
            let ended = landed
            landed = []
            for (id, outcome) in ended {
                guard flights.removeValue(forKey: id) != nil else {
                    // Told to stop before it ended.
                    if case .up(let opened) = outcome { await opened.connection.close() }
                    continue
                }
                switch outcome {
                case .up(let opened):
                    if let other = winner {
                        // Two at once: the network is the one to have.
                        if opened.kind == .network && other.kind != .network {
                            await other.connection.close()
                            winner = opened
                        } else {
                            await opened.connection.close()
                        }
                    } else {
                        winner = opened
                    }
                case .failed(let kind, let error):
                    if kind == .bluetooth {
                        if state == .connecting { setState(.searching) }
                        bluetoothFailed(error)
                    } else {
                        lastNetworkError = error
                    }
                }
            }
            if stopped {
                if let winner { await winner.connection.close() }
                return
            }
            if let opened = winner {
                // The network was still on its way when Bluetooth won:
                // that is not the network having failed.
                let networkPending: Bool = flights.values.contains { $0.kind == .network }
                cancelFlights()
                if await adopt(opened) {
                    failures = 0
                    if opened.kind == .bluetooth, networkPending { probeNow = true }
                }
                continue
            }

            let networkIdle: Bool = !flights.values.contains { $0.kind == .network }
            if let link = active {
                // Bluetooth is the slow way. The network is tried at once
                // when there is reason to, and otherwise every so often.
                guard link.kind == .bluetooth, endpoints.isUsable, networkIdle else {
                    await idle(nil)
                    continue
                }
                if probeNow {
                    probeNow = false
                    launch(networkCandidates())
                    await idle(nil)
                } else if await idle(timing.upgradeEvery) {
                    probeNow = true
                }
                continue
            }

            // No link. How long it has been decides how hard to try, and
            // whether anyone is still to be kept waiting.
            var pause: TimeInterval = timing.searchEvery
            if let lostAt {
                if DispatchTime.now().uptimeNanoseconds - lostAt < Self.nanoseconds(timing.giveUpAfter) {
                    pause = timing.retryEvery
                } else {
                    // Gone, as far as anyone waiting is concerned.
                    self.lostAt = nil
                    setState(.searching)
                    releaseWaiters(false)
                }
            }
            let now: UInt64 = DispatchTime.now().uptimeNanoseconds
            if !flights.values.contains(where: { $0.kind == .bluetooth }), now >= nextBluetooth {
                nextBluetooth = now + Self.nanoseconds(pause)
                launch([Candidate(kind: .bluetooth)])
            }
            if networkIdle, probeNow || now >= nextNetwork {
                probeNow = false
                nextNetwork = now + Self.nanoseconds(pause)
                launch(networkCandidates())
            }
            // An attempt the person asked for says how it went as soon as
            // every way has been tried.
            if !automatic, !everConnected, flights.isEmpty, let error = lastBluetoothError ?? lastNetworkError {
                fail(error)
                return
            }
            await idle(pause)
        }
    }

    /// A Bluetooth attempt has failed. Whether that is worth telling
    /// depends on how: the network not answering, or the router being out
    /// of range, is what happens whenever the phone is somewhere else.
    private func bluetoothFailed(_ error: Error) {
        lastBluetoothError = error
        if let mismatch = error as? SessionError, mismatch == .routerKeyMismatch {
            fail(mismatch)
            return
        }
        guard !(error is RouterNotFound), !(error is CancellationError) else { return }
        // An attempt the person asked for says how it went. One nobody
        // asked for stays quiet unless the router is there and will not
        // connect, five times running: a router that is starting up is
        // seen before it answers. While a lost link is being replaced,
        // attempts come too fast to count.
        if lostAt == nil { failures += 1 }
        if (!automatic && !everConnected) || failures >= 5 { fail(error) }
    }

    private func networkCandidates() -> [Candidate] {
        guard endpoints.isUsable, expectedKey != nil else { return [] }
        return endpoints.addrs.map { Candidate(kind: .network, host: $0, port: endpoints.port) }
    }

    /// Starts an attempt for each candidate. Each reports back by itself.
    private func launch(_ candidates: [Candidate]) {
        for candidate in candidates {
            let id: Int = nextToken
            nextToken += 1
            let task = Task {
                let outcome: Outcome = await self.attempt(candidate, id)
                self.land(id, outcome)
            }
            flights[id] = (kind: candidate.kind, task: task)
        }
    }

    /// An attempt has ended. The main loop takes it from here.
    private func land(_ id: Int, _ outcome: Outcome) {
        landed.append((id: id, outcome: outcome))
        wake()
    }

    /// Tells every attempt under way to stop. What they have opened they
    /// close themselves.
    private func cancelFlights() {
        for (_, flight) in flights { flight.task.cancel() }
        flights.removeAll()
    }

    /// One attempt: open the link, secure it against the pinned key, say
    /// hello.
    private func attempt(_ candidate: Candidate, _ id: Int) async -> Outcome {
        guard let dialer else { return .failed(candidate.kind, SupervisorError.notConnected) }
        let box = TransportBox()
        do {
            let opened: Opened = try await withTaskCancellationHandler {
                let transport: PacketTransport
                switch candidate.kind {
                case .bluetooth:
                    transport = try await dialer.openBluetooth()
                case .network:
                    transport = try await dialer.openNetwork(host: candidate.host, port: candidate.port, timeout: timing.networkConnect)
                }
                guard box.hold(transport) else { throw CancellationError() }
                // A link that opens and then says nothing must not hold
                // the attempt up for ever.
                let limit: UInt64 = Self.nanoseconds(candidate.kind == .network ? timing.networkSecure : timing.bluetoothSecure)
                let deadline = Task {
                    do {
                        try await Task.sleep(nanoseconds: limit)
                    } catch {
                        return // cancelled: the router answered
                    }
                    box.close()
                }
                defer { deadline.cancel() }
                if candidate.kind == .bluetooth { reached() }
                let connection = try await RouterConnection.connect(transport: transport, identity: identity, expectedRouterKey: expectedKey)
                do {
                    let hello: HelloInfo = try await connection.hello()
                    return Opened(kind: candidate.kind, connection: connection, hello: hello)
                } catch {
                    await connection.close()
                    throw error
                }
            } onCancel: {
                box.close()
            }
            if stopped || Task.isCancelled || flights[id] == nil {
                await opened.connection.close()
                return .failed(candidate.kind, CancellationError())
            }
            if candidate.kind == .network && !opened.hello.paired {
                // Over the network the router only talks to paired phones.
                await opened.connection.close()
                return .failed(candidate.kind, RouterError(code: RouterError.unpaired, msg: "This phone is not paired with the router."))
            }
            return .up(opened)
        } catch {
            box.close()
            return .failed(candidate.kind, error)
        }
    }

    /// The router has been reached over Bluetooth for the first time, or
    /// the first time since it was given up on.
    private func reached() {
        if state == .searching { setState(.connecting) }
    }

    /// Takes a link that has come up into use, pairing first if the
    /// router does not know this phone. False if the link did not last.
    private func adopt(_ opened: Opened) async -> Bool {
        var hello: HelloInfo = opened.hello
        if !hello.paired {
            pairing = opened.connection
            defer { pairing = nil }
            // Ask, and keep asking while the person goes to press the
            // button.
            while !hello.paired {
                do {
                    try await opened.connection.pair(name: phoneName)
                    hello = try await opened.connection.hello()
                } catch let error as RouterError where error.code == RouterError.pairingClosed {
                    if stopped { return false }
                    setState(.awaitingPairing(name: hello.name))
                    await idle(timing.pairingEvery)
                } catch {
                    await opened.connection.close()
                    // Back to looking, if it had got as far as showing.
                    if !stopped, state == .connecting || state == .awaitingPairing(name: hello.name) { setState(.searching) }
                    return false
                }
                if stopped {
                    await opened.connection.close()
                    return false
                }
            }
        }
        install(opened, hello: hello)
        return true
    }

    /// Makes a link the one calls go over. The link before it, if there
    /// is one, keeps the calls already on it and goes when they are done.
    private func install(_ opened: Opened, hello: HelloInfo) {
        let link = Link(id: nextLinkID, kind: opened.kind, connection: opened.connection)
        nextLinkID += 1
        let previous: Link? = active
        active = link
        lostAt = nil
        everConnected = true
        // From the first session on, every link has to be to this router.
        expectedKey = opened.connection.routerKey
        sink.yield(.identified(hello, routerKey: opened.connection.routerKey))
        learn(hello.control)

        let id: Int = link.id
        let connection: RouterConnection = link.connection
        link.pump = Task { [weak self] in
            for await event in connection.events {
                await self?.forward(event, from: id)
            }
        }
        if link.kind == .network {
            let every: UInt64 = Self.nanoseconds(timing.heartbeatEvery)
            let within: TimeInterval = timing.heartbeatWithin
            link.heartbeat = Task { [weak self] in
                while true {
                    do {
                        try await Task.sleep(nanoseconds: every)
                    } catch {
                        return // cancelled: the link is no longer the one in use
                    }
                    if let error = await Self.beat(connection, within: within) {
                        await self?.linkClosed(id, error)
                        return
                    }
                }
            }
        }
        Task { [weak self] in
            await connection.onClose { error in
                Task { await self?.linkClosed(id, error) }
            }
        }

        setState(.connected(link.kind))
        releaseWaiters(true)
        if let previous { retire(previous) }
    }

    /// Asks a link for a sign of life. Nil if one came; otherwise why not.
    private static func beat(_ connection: RouterConnection, within seconds: TimeInterval) async -> Error? {
        do {
            let _: Empty = try await connection.call("ping", Empty(), within: seconds)
            return nil
        } catch is RouterError {
            // An answer, whatever it says.
            return nil
        } catch {
            return error
        }
    }

    private func forward(_ event: RouterEvent, from id: Int) {
        guard !stopped, active?.id == id else { return }
        if case .status(let status) = event { learn(status.control) }
        sink.yield(.router(event))
    }

    /// Takes in what the router says about where it can be reached.
    private func learn(_ control: ControlEndpoints?) {
        let next: ControlEndpoints = control ?? ControlEndpoints()
        guard next != endpoints else { return }
        endpoints = next
        sink.yield(.endpoints(next))
        probeNow = true
        wake()
    }

    /// Lets go of a link that calls no longer go over.
    private func retire(_ link: Link) {
        link.pump?.cancel()
        link.heartbeat?.cancel()
        let connection: RouterConnection = link.connection
        if link.inFlight == 0 {
            Task { await connection.close() }
            return
        }
        draining.append(link)
        let id: Int = link.id
        let limit: UInt64 = Self.nanoseconds(timing.drain)
        Task { [weak self] in
            try? await Task.sleep(nanoseconds: limit)
            await self?.drained(id)
        }
    }

    private func drained(_ id: Int) {
        guard let index = draining.firstIndex(where: { $0.id == id }) else { return }
        let connection: RouterConnection = draining.remove(at: index).connection
        Task { await connection.close() }
    }

    /// A link has ended. If it was the one in use, another is brought up
    /// at once; nothing above the supervisor is torn down for it.
    private func linkClosed(_ id: Int, _ error: Error) {
        if let index = draining.firstIndex(where: { $0.id == id }) {
            draining.remove(at: index)
            return
        }
        guard let link = active, link.id == id, !stopped else { return }
        active = nil
        link.pump?.cancel()
        link.heartbeat?.cancel()
        let connection: RouterConnection = link.connection
        Task { await connection.close() }
        lostAt = DispatchTime.now().uptimeNanoseconds
        // Every way is due for an attempt now.
        nextBluetooth = 0
        nextNetwork = 0
        setState(.reconnecting(noticeable: false))
        let after: UInt64 = Self.nanoseconds(timing.noticeAfter)
        Task { [weak self] in
            try? await Task.sleep(nanoseconds: after)
            await self?.notice()
        }
        wake()
    }

    private func notice() {
        if state == .reconnecting(noticeable: false) { setState(.reconnecting(noticeable: true)) }
    }

    // MARK: - Ending

    private func fail(_ error: Error) {
        guard !stopped else { return }
        sink.yield(.failed(error))
        shutdown()
    }

    private func shutdown() {
        guard !stopped else { return }
        stopped = true
        runner?.cancel()
        cancelFlights()
        var connections: [RouterConnection] = draining.map(\.connection)
        if let link = active {
            link.pump?.cancel()
            link.heartbeat?.cancel()
            connections.append(link.connection)
        }
        if let pairing { connections.append(pairing) }
        for (_, outcome) in landed {
            if case .up(let opened) = outcome { connections.append(opened.connection) }
        }
        landed = []
        active = nil
        draining.removeAll()
        Task { [connections] in
            for connection in connections { await connection.close() }
        }
        setState(.stopped)
        releaseWaiters(false)
        wake()
        sink.finish()
    }

    // MARK: - Plumbing

    private func setState(_ next: State) {
        guard next != state else { return }
        state = next
        sink.yield(.state(next))
    }

    /// Sleeps until `wake()` or for `seconds`, whichever is first. True
    /// if the time ran out.
    @discardableResult
    private func idle(_ seconds: TimeInterval?) async -> Bool {
        if wakePending || stopped {
            wakePending = false
            return false
        }
        let token: Int = nextToken
        nextToken += 1
        var timer: Task<Void, Never>?
        if let seconds {
            let limit: UInt64 = Self.nanoseconds(seconds)
            timer = Task {
                do {
                    try await Task.sleep(nanoseconds: limit)
                } catch {
                    return // cancelled: woken first
                }
                self.timerFired(token)
            }
        }
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in sleeper = (token: token, continuation: c) }
        timer?.cancel()
        return firedToken == token
    }

    /// The token of the sleep whose time ran out last.
    private var firedToken = -1

    private func timerFired(_ token: Int) {
        guard let current = sleeper, current.token == token else { return }
        firedToken = token
        sleeper = nil
        current.continuation.resume()
    }

    private func wake() {
        if let current = sleeper {
            sleeper = nil
            current.continuation.resume()
        } else {
            wakePending = true
        }
    }

    private static func nanoseconds(_ seconds: TimeInterval) -> UInt64 {
        UInt64(max(seconds, 0) * 1_000_000_000)
    }
}
