import Foundation
@testable import PorterKit
#if canImport(CryptoKit)
import CryptoKit
#else
import Crypto
#endif

// A router for the supervisor's tests: the router's side of the Noise
// handshake and of the session, a handful of calls answered the way the
// real router answers them, and links to it that can be made to fail in
// the ways real links do. Everything above the transport runs for real.

/// The responder's side of one XX handshake (see Noise.swift for the
/// initiator's).
struct ResponderHandshake {
    private var state = SymmetricState(protocolName: "Noise_XX_25519_ChaChaPoly_SHA256")
    private let s: Curve25519.KeyAgreement.PrivateKey
    private let e = Curve25519.KeyAgreement.PrivateKey()
    private var re: Curve25519.KeyAgreement.PublicKey?

    init(identity: Identity) throws {
        s = try Curve25519.KeyAgreement.PrivateKey(rawRepresentation: identity.privateKey)
        state.mixHash(SecureSession.prologue)
    }

    private func dh(_ priv: Curve25519.KeyAgreement.PrivateKey, _ pub: Curve25519.KeyAgreement.PublicKey) throws -> Data {
        try priv.sharedSecretFromKeyAgreement(with: pub).withUnsafeBytes { Data($0) }
    }

    /// -> e
    mutating func readMessage1(_ message: Data) throws {
        let m = Data(message)
        guard m.count == 32 else { throw NoiseError.malformedMessage }
        re = try Curve25519.KeyAgreement.PublicKey(rawRepresentation: m)
        state.mixHash(m)
        _ = try state.decryptAndHash(Data())
    }

    /// <- e, ee, s, es
    mutating func writeMessage2() throws -> Data {
        guard let re else { throw NoiseError.outOfOrder }
        let pub = e.publicKey.rawRepresentation
        state.mixHash(pub)
        state.mixKey(try dh(e, re))
        var out = pub + (try state.encryptAndHash(s.publicKey.rawRepresentation))
        state.mixKey(try dh(s, re))
        out += try state.encryptAndHash(Data())
        return out
    }

    /// -> s, se. Returns the app's static key and the transport ciphers.
    mutating func readMessage3(_ message: Data) throws -> (remote: Data, send: CipherState, receive: CipherState) {
        let m = Data(message)
        guard m.count == 48 + 16 else { throw NoiseError.malformedMessage }
        let rs = try state.decryptAndHash(m.subdata(in: 0..<48))
        state.mixKey(try dh(e, try Curve25519.KeyAgreement.PublicKey(rawRepresentation: rs)))
        _ = try state.decryptAndHash(m.subdata(in: 48..<64))
        let (fromApp, toApp) = state.split()
        return (rs, toApp, fromApp)
    }
}

enum FakeError: Error, Equatable {
    /// The link ended: closed by either side, or cut.
    case linkDown
    case outOfRange
    case noRoute
    case bluetoothOff
    case stuck
}

/// A link between the app and a fake router. The app's end is the
/// transport; the router's end is the `router…` calls.
final class FakeLink: PacketTransport, @unchecked Sendable {
    let kind: LinkKind
    let host: String

    private let lock = NSLock()
    private var toApp: [Data] = []
    private var appWaiter: CheckedContinuation<Data, Error>?
    private var toRouter: [Data] = []
    private var routerWaiter: CheckedContinuation<Data, Error>?
    private var down = false
    private var silent = false
    private var closedByApp = false

    init(kind: LinkKind, host: String = "") {
        self.kind = kind
        self.host = host
    }

    /// Whether the app's side has closed the link.
    var wasClosedByApp: Bool { lock.withLock { closedByApp } }
    var isDown: Bool { lock.withLock { down } }

    // The app's end.

    func send(_ packet: Data) async throws {
        let waiter: CheckedContinuation<Data, Error>? = try lock.withLock {
            if down { throw FakeError.linkDown }
            if silent { return nil }
            if let waiting = routerWaiter {
                routerWaiter = nil
                return waiting
            }
            toRouter.append(packet)
            return nil
        }
        waiter?.resume(returning: packet)
    }

    func receive() async throws -> Data {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data, Error>) in
            let ready: Result<Data, Error>? = lock.withLock {
                if !toApp.isEmpty { return .success(toApp.removeFirst()) }
                if down { return .failure(FakeError.linkDown) }
                appWaiter = c
                return nil
            }
            if let ready { c.resume(with: ready) }
        }
    }

    func close() {
        lock.withLock { closedByApp = true }
        end()
    }

    // Ways for a link to fail.

    /// The link breaks and both ends are told at once, as when a TCP
    /// connection is reset or Bluetooth disconnects.
    func cut() { end() }

    /// Nothing gets through any more and nobody is told, as when the
    /// phone has walked out of range.
    func goSilent() { lock.withLock { silent = true } }

    private func end() {
        let waiters: [CheckedContinuation<Data, Error>] = lock.withLock {
            down = true
            let waiting = [appWaiter, routerWaiter].compactMap { $0 }
            appWaiter = nil
            routerWaiter = nil
            return waiting
        }
        for waiter in waiters { waiter.resume(throwing: FakeError.linkDown) }
    }

    // The router's end.

    func routerReceive() async throws -> Data {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data, Error>) in
            let ready: Result<Data, Error>? = lock.withLock {
                if !toRouter.isEmpty { return .success(toRouter.removeFirst()) }
                if down { return .failure(FakeError.linkDown) }
                routerWaiter = c
                return nil
            }
            if let ready { c.resume(with: ready) }
        }
    }

    func routerSend(_ packet: Data) {
        let waiter: CheckedContinuation<Data, Error>? = lock.withLock {
            if down || silent { return nil }
            if let waiting = appWaiter {
                appWaiter = nil
                return waiting
            }
            toApp.append(packet)
            return nil
        }
        waiter?.resume(returning: packet)
    }
}

/// Lets a test hold something up until it says go.
final class Gate: @unchecked Sendable {
    private let lock = NSLock()
    private var open = false
    private var waiters: [CheckedContinuation<Void, Never>] = []
    private var arrived = 0

    /// How many have come to the gate so far, waiting or let through.
    var arrivals: Int { lock.withLock { arrived } }

    func wait() async {
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
            let through: Bool = lock.withLock {
                arrived += 1
                if open { return true }
                waiters.append(c)
                return false
            }
            if through { c.resume() }
        }
    }

    func release() {
        let waiting: [CheckedContinuation<Void, Never>] = lock.withLock {
            open = true
            let all = waiters
            waiters = []
            return all
        }
        for waiter in waiting { waiter.resume() }
    }
}

/// A router with one configuration, a list of paired phones and the calls
/// the tests make.
final class FakeRouter: @unchecked Sendable {
    /// One call as the router saw it.
    struct Call: Equatable {
        let method: String
        let over: LinkKind
    }

    /// What becomes of a call, decided by a test.
    enum Disposition {
        case answer
        /// The link breaks before the call reaches the router.
        case cutBeforeRequest
        /// The router carries the call out, and the link breaks before
        /// its answer gets back.
        case cutBeforeReply
    }

    let identity = Identity()
    let name: String

    private let lock = NSLock()
    private var config: RouterConfig
    private var paired: Set<Data> = []
    private var pairingOpen = false
    private var applied: [String: UInt64] = [:]
    private var seen: [Call] = []
    private var sessions: [Session] = []
    private var control: ControlEndpoints?
    private var hook: (@Sendable (String, FakeLink) async -> Disposition)?

    private final class Session: @unchecked Sendable {
        let link: FakeLink
        let remote: Data
        private let lock = NSLock()
        private var sendCipher: CipherState
        private var receiveCipher: CipherState

        init(link: FakeLink, remote: Data, send: CipherState, receive: CipherState) {
            self.link = link
            self.remote = remote
            sendCipher = send
            receiveCipher = receive
        }

        func send(_ message: Data) {
            // Encrypted and handed over under one lock: the order of the
            // counters is the order on the link.
            lock.withLock {
                var rest = Data(message)
                repeat {
                    let chunk = rest.prefix(8000)
                    rest = Data(rest.dropFirst(chunk.count))
                    var record = Data([rest.isEmpty ? 0 : 1])
                    record.append(chunk)
                    if let packet = try? sendCipher.encrypt(record, ad: Data()) { link.routerSend(packet) }
                } while !rest.isEmpty
            }
        }

        func receive() async throws -> Data {
            var message = Data()
            while true {
                let packet = try await link.routerReceive()
                let record: Data = try lock.withLock { try receiveCipher.decrypt(packet, ad: Data()) }
                guard let more = record.first else { throw NoiseError.malformedMessage }
                message.append(record.dropFirst())
                if more == 0 { return message }
            }
        }
    }

    init(name: String = "Porter", control: ControlEndpoints? = ControlEndpoints(port: 7678, addrs: ["10.83.0.1"])) {
        self.name = name
        self.control = control
        config = try! RouterConnection.makeDecoder().decode(RouterConfig.self, from: Data(DemoRouter.configJSON.utf8))
    }

    // What tests set up and look at.

    func pair(_ phone: Identity) { lock.withLock { _ = paired.insert(phone.publicKey) } }
    func openPairing() { lock.withLock { pairingOpen = true } }
    /// Decides what becomes of each call; nil answers them all.
    func onCall(_ hook: (@Sendable (String, FakeLink) async -> Disposition)?) { lock.withLock { self.hook = hook } }

    var calls: [Call] { lock.withLock { seen } }
    func count(_ method: String, over kind: LinkKind? = nil) -> Int {
        calls.filter { $0.method == method && (kind == nil || $0.over == kind) }.count
    }

    var currentConfig: RouterConfig { lock.withLock { config } }
    /// Changes the configuration as another phone would.
    func edit(_ change: (inout RouterConfig) -> Void) {
        lock.withLock {
            change(&config)
            config.rev += 1
        }
    }

    /// As after a restart: the writes made so far are not remembered.
    func forgetWrites() { lock.withLock { applied.removeAll() } }

    /// Changes where the router says it can be reached, and says so to
    /// every phone connected.
    func move(to endpoints: ControlEndpoints?) {
        lock.withLock { control = endpoints }
        pushStatus()
    }

    func pushStatus() {
        let (status, live): (String, [Session]) = lock.withLock { (statusJSON(), sessions) }
        for session in live { session.send(Data("{\"ev\":\"status\",\"d\":\(status)}".utf8)) }
    }

    /// The links to sessions that are up.
    var liveLinks: [FakeLink] { lock.withLock { sessions.map(\.link).filter { !$0.isDown } } }

    // The router's side of a link.

    func attach(_ link: FakeLink) {
        Task.detached { await self.serve(link) }
    }

    private func serve(_ link: FakeLink) async {
        do {
            var handshake = try ResponderHandshake(identity: identity)
            try handshake.readMessage1(try await link.routerReceive())
            link.routerSend(try handshake.writeMessage2())
            let keys = try handshake.readMessage3(try await link.routerReceive())
            let session = Session(link: link, remote: keys.remote, send: keys.send, receive: keys.receive)
            // Over the network, paired phones only: anyone else is cut
            // off as soon as the handshake has shown who they are.
            let known: Bool = lock.withLock { paired.contains(keys.remote) }
            if link.kind == .network && !known {
                link.cut()
                return
            }
            lock.withLock { sessions.append(session) }
            defer { lock.withLock { sessions.removeAll(where: { $0 === session }) } }
            while true {
                let message = try await session.receive()
                Task.detached { await self.answer(message, session) }
            }
        } catch {
            link.cut()
        }
    }

    private struct Request: Decodable {
        let id: UInt32
        let m: String
    }

    private func answer(_ message: Data, _ session: Session) async {
        guard let request = try? JSONDecoder().decode(Request.self, from: message) else { return }
        let hook = lock.withLock { self.hook }
        let disposition: Disposition = await hook?(request.m, session.link) ?? .answer
        if case .cutBeforeRequest = disposition {
            session.link.cut()
            return
        }
        lock.withLock { seen.append(Call(method: request.m, over: session.link.kind)) }
        var reply = Data("{\"id\":\(request.id),".utf8)
        do {
            let result: Data = try lock.withLock { try handle(request.m, message, session) }
            reply.append(Data("\"r\":".utf8))
            reply.append(result)
        } catch let error as RouterError {
            reply.append(Data("\"e\":".utf8))
            reply.append((try? JSONEncoder().encode(error)) ?? Data("{}".utf8))
        } catch {
            return
        }
        reply.append(Data("}".utf8))
        if case .cutBeforeReply = disposition {
            session.link.cut()
            return
        }
        session.send(reply)
    }

    private func statusJSON() -> String {
        var endpoints = "null"
        if let control, let data = try? JSONEncoder().encode(control) { endpoints = String(decoding: data, as: UTF8.self) }
        let now = Int64(Date().timeIntervalSince1970)
        let groups: String = (try? DemoRouter.groups(config, now: now)) ?? #""groups":[],"held":[]"#
        return DemoRouter.status(now: now, rev: config.rev, name: config.system.name, control: endpoints, inbound: DemoRouter.inbound(config, now: now), shaping: DemoRouter.shaping(config), groups: groups)
    }

    /// Called with the lock held.
    private func handle(_ method: String, _ message: Data, _ session: Session) throws -> Data {
        let isPaired: Bool = paired.contains(session.remote)
        switch method {
        case "hello":
            var hello = "{\"name\":\"\(name)\",\"id\":\"3fa9c210\",\"version\":\"v1.0.0\",\"paired\":\(isPaired),\"claimed\":true,\"pairing\":\(pairingOpen),\"ready\":true"
            if isPaired, let control, let data = try? JSONEncoder().encode(control) {
                hello += ",\"control\":" + String(decoding: data, as: UTF8.self)
            }
            return Data((hello + "}").utf8)
        case "pair":
            if isPaired { return Data("{}".utf8) }
            guard pairingOpen else {
                throw RouterError(code: RouterError.pairingClosed, msg: "press the power button on the router to allow pairing")
            }
            paired.insert(session.remote)
            pairingOpen = false
            return Data("{}".utf8)
        default:
            break
        }
        guard isPaired else { throw RouterError(code: RouterError.unpaired, msg: "this phone is not paired with the router") }
        switch method {
        case "status.get":
            return Data(statusJSON().utf8)
        case "config.get":
            return try RouterConnection.makeEncoder().encode(config)
        case "config.set":
            struct Envelope: Decodable {
                struct Params: Decodable {
                    let config: RouterConfig
                    let id: String?
                }
                let p: Params
            }
            let params = try RouterConnection.makeDecoder().decode(Envelope.self, from: message).p
            if let id = params.id, let rev = applied[id] { return Data("{\"rev\":\(rev)}".utf8) }
            guard params.config.rev == config.rev else {
                throw RouterError(code: RouterError.conflict, msg: "the configuration changed; reload and try again")
            }
            config = params.config
            config.rev += 1
            if let id = params.id { applied[id] = config.rev }
            return Data("{\"rev\":\(config.rev)}".utf8)
        case "devices.list":
            return try RouterConnection.makeEncoder().encode(try DemoRouter.devices(config, now: Int64(Date().timeIntervalSince1970)))
        case "radios.list":
            return Data(DemoRouter.radiosJSON.utf8)
        default:
            return Data("{}".utf8)
        }
    }
}

/// Opens links to fake routers, or fails to, as a test arranges.
final class FakeDialer: LinkDialer, @unchecked Sendable {
    enum Way {
        /// Fails at once: no such host, Bluetooth off, out of range.
        case absent(Error)
        /// Answers after a delay, in seconds.
        case router(FakeRouter, after: TimeInterval)
        /// Never answers; the attempt ends when it is cancelled or, over
        /// the network, when its time is up.
        case hangs
    }

    private let lock = NSLock()
    private var bluetooth: Way = .absent(RouterNotFound(FakeError.outOfRange))
    private var hosts: [String: Way] = [:]
    private var opened: [FakeLink] = []
    private var tried: [LinkKind: Int] = [:]

    func setBluetooth(_ way: Way) { lock.withLock { bluetooth = way } }
    func setHost(_ host: String, _ way: Way?) { lock.withLock { hosts[host] = way } }

    /// How often a link of a kind has been asked for.
    func attempts(_ kind: LinkKind) -> Int { lock.withLock { tried[kind] ?? 0 } }
    /// Every link that was opened, oldest first.
    func links(_ kind: LinkKind) -> [FakeLink] { lock.withLock { opened.filter { $0.kind == kind } } }

    func openBluetooth() async throws -> PacketTransport {
        let way: Way = lock.withLock {
            tried[.bluetooth, default: 0] += 1
            return bluetooth
        }
        return try await open(way, kind: .bluetooth, host: "", timeout: nil)
    }

    func openNetwork(host: String, port: Int, timeout: TimeInterval) async throws -> PacketTransport {
        let way: Way = lock.withLock {
            tried[.network, default: 0] += 1
            return hosts[host] ?? .absent(FakeError.noRoute)
        }
        return try await open(way, kind: .network, host: host, timeout: timeout)
    }

    private func open(_ way: Way, kind: LinkKind, host: String, timeout: TimeInterval?) async throws -> PacketTransport {
        switch way {
        case .absent(let error):
            throw error
        case .hangs:
            try await Task.sleep(nanoseconds: UInt64((timeout ?? 3600) * 1_000_000_000))
            throw FakeError.stuck
        case .router(let router, let delay):
            try await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
            let link = FakeLink(kind: kind, host: host)
            lock.withLock { opened.append(link) }
            router.attach(link)
            return link
        }
    }
}
