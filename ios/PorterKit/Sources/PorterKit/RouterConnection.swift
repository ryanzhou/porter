import Foundation

/// An error reported by the router.
public struct RouterError: Error, Codable, Equatable, LocalizedError {
    public let code: String
    public let msg: String

    public static let unpaired = "unpaired"
    public static let pairingClosed = "pairing_closed"
    public static let conflict = "conflict"
    public static let noStation = "no_station"

    public var errorDescription: String? { msg }
}

/// An unsolicited message from the router.
public enum RouterEvent {
    case status(RouterStatus)
    case activity(ActivityEvent)
    case speedTest(SpeedTestProgress)
    case firmware(FirmwareProgress)
}

public struct Empty: Codable, Equatable { public init() {} }

/// Something calls to a router can be made on: one connection, or the
/// supervisor that keeps one up over whichever link works. Everything the
/// router can be asked is written once, against this.
public protocol RouterAPI {
    /// Calls a method and decodes its result.
    func call<P: Encodable, R: Decodable>(_ method: String, _ params: P) async throws -> R
}

public extension RouterAPI {
    /// Calls a method without parameters.
    func call<R: Decodable>(_ method: String, as: R.Type = R.self) async throws -> R {
        try await call(method, Empty())
    }

    /// Calls a method whose result is not needed.
    func run<P: Encodable>(_ method: String, _ params: P) async throws {
        let _: Empty = try await call(method, params)
    }

    func run(_ method: String) async throws {
        let _: Empty = try await call(method, Empty())
    }
}

/// Whether a call failed because the link it was made on went, and not
/// because of anything the router said about it. Such a call may or may
/// not have reached the router.
public func isLinkFailure(_ error: Error) -> Bool {
    if error is RouterError || error is DecodingError || error is EncodingError { return false }
    if let error = error as? SupervisorError, error == .notConnected { return false }
    return true
}

/// The RPC layer: requests, responses and events over a secure session.
public actor RouterConnection: RouterAPI {
    private let session: SecureSession?
    /// Answers calls locally instead of over a session (demo mode).
    private let demo: ((String, Data) throws -> Data)?
    private var nextID: UInt32 = 1
    private var pending: [UInt32: CheckedContinuation<Data, Error>] = [:]
    /// The deadline running for each call that is waiting for its reply.
    private var deadlines: [UInt32: Task<Void, Never>] = [:]
    /// How long one second of a deadline lasts, in nanoseconds. Only
    /// tests change it, to not wait as long as a person would.
    private var deadlineUnit: UInt64 = 1_000_000_000
    private var closedError: Error?
    private var sendChain: Task<Void, Never>?
    private var readTask: Task<Void, Never>?

    public nonisolated let routerKey: Data
    public nonisolated let events: AsyncStream<RouterEvent>
    private let eventSink: AsyncStream<RouterEvent>.Continuation
    private var closeHandlers: [(Error) -> Void] = []

    public static func makeEncoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.keyEncodingStrategy = .convertToSnakeCase
        return e
    }

    public static func makeDecoder() -> JSONDecoder {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        return d
    }

    /// A connection with no router behind it: `handler` receives each
    /// method name with its JSON parameters and returns the JSON result.
    /// Used to run the app against sample data.
    public init(demo handler: @escaping (String, Data) throws -> Data) {
        session = nil
        demo = handler
        routerKey = Data(count: 32)
        (events, eventSink) = AsyncStream<RouterEvent>.makeStream(bufferingPolicy: .bufferingNewest(64))
    }

    public init(session: SecureSession) {
        self.session = session
        demo = nil
        routerKey = session.routerKey
        (events, eventSink) = AsyncStream<RouterEvent>.makeStream(bufferingPolicy: .bufferingNewest(64))
    }

    /// Opens a session over `transport` and starts reading.
    public static func connect(transport: PacketTransport, identity: Identity, expectedRouterKey: Data?) async throws -> RouterConnection {
        let session = try await SecureSession.open(transport: transport, identity: identity, expectedRouterKey: expectedRouterKey)
        let connection = RouterConnection(session: session)
        await connection.start()
        return connection
    }

    private func start() {
        readTask = Task { [weak self] in
            guard let self else { return }
            await self.readLoop()
        }
    }

    /// Calls `handler` once when the connection ends.
    public func onClose(_ handler: @escaping (Error) -> Void) {
        if let closedError {
            handler(closedError)
        } else {
            closeHandlers.append(handler)
        }
    }

    private struct Envelope: Decodable {
        let id: UInt32?
        let ev: String?
        let e: RouterError?
    }

    private func readLoop() async {
        guard let session else { return }
        while true {
            do {
                let message = try await session.receive()
                handle(message)
            } catch {
                fail(error)
                return
            }
        }
    }

    private func handle(_ message: Data) {
        // Only the envelope is decoded here; payloads are decoded by
        // whoever asked for them.
        guard let envelope = try? JSONDecoder().decode(Envelope.self, from: message) else { return }
        if let event = envelope.ev {
            let decoder = Self.makeDecoder()
            struct Wrapper<T: Decodable>: Decodable { let d: T }
            switch event {
            case "status":
                if let w = try? decoder.decode(Wrapper<RouterStatus>.self, from: message) { eventSink.yield(.status(w.d)) }
            case "event":
                if let w = try? decoder.decode(Wrapper<ActivityEvent>.self, from: message) { eventSink.yield(.activity(w.d)) }
            case "speedtest":
                if let w = try? decoder.decode(Wrapper<SpeedTestProgress>.self, from: message) { eventSink.yield(.speedTest(w.d)) }
            case "firmware":
                if let w = try? decoder.decode(Wrapper<FirmwareProgress>.self, from: message) { eventSink.yield(.firmware(w.d)) }
            default:
                break
            }
            return
        }
        guard let id = envelope.id, let continuation = pending.removeValue(forKey: id) else { return }
        deadlines.removeValue(forKey: id)?.cancel()
        if let error = envelope.e {
            continuation.resume(throwing: error)
        } else {
            continuation.resume(returning: message)
        }
    }

    private func fail(_ error: Error) {
        guard closedError == nil else { return }
        closedError = error
        for (_, continuation) in pending { continuation.resume(throwing: error) }
        pending.removeAll()
        for (_, deadline) in deadlines { deadline.cancel() }
        deadlines.removeAll()
        eventSink.finish()
        session?.close()
        for handler in closeHandlers { handler(error) }
        closeHandlers.removeAll()
    }

    public func close() {
        fail(SessionError.closed)
        readTask?.cancel()
    }

    /// Whether the connection has ended.
    public var isClosed: Bool { closedError != nil }

    /// How long a call may wait for its reply, in seconds. Most answers
    /// come at once; a few are of work the router does first.
    private static func deadline(for method: String) -> Double {
        switch method {
        case "speedtest.run": return 90
        case "firmware.check", "firmware.install": return 45
        // Answered by the router's control loop, which may be in the
        // middle of restarting a radio when asked.
        case "config.set", "config.check", "wifi.uplink", "wifi.scan": return 45
        default: return 30
        }
    }

    /// The deadline of a call has passed. If its reply is still missing
    /// the router has stopped answering, and a connection like that is
    /// worth nothing: it ends as it does when the link is lost, so that
    /// everything waiting on it hears and the app can connect again.
    private func expire(id: UInt32) {
        // The reply may have arrived just as the deadline passed.
        guard pending[id] != nil else { return }
        fail(SessionError.timeout)
    }

    /// Makes a second of the deadlines last this long, for tests.
    func setDeadlineUnit(nanoseconds: UInt64) {
        deadlineUnit = nanoseconds
    }

    private struct Request<P: Encodable>: Encodable {
        let id: UInt32
        let m: String
        let p: P?
    }

    private struct Response<R: Decodable>: Decodable {
        let r: R
    }

    /// Calls a method and decodes its result.
    public func call<P: Encodable, R: Decodable>(_ method: String, _ params: P) async throws -> R {
        try await call(method, params, within: Self.deadline(for: method))
    }

    /// Calls a method that has to be answered within `seconds`. As with
    /// every deadline, missing it ends the connection.
    public func call<P: Encodable, R: Decodable>(_ method: String, _ params: P, within seconds: Double) async throws -> R {
        if let closedError { throw closedError }
        let id = nextID
        nextID &+= 1
        // The method name and envelope keys are sent verbatim; only the
        // parameters go through the snake_case conversion.
        let paramData = try Self.makeEncoder().encode(params)
        if let demo {
            return try Self.makeDecoder().decode(R.self, from: try demo(method, paramData))
        }
        var payload = Data("{\"id\":\(id),\"m\":".utf8)
        payload.append(try JSONEncoder().encode(method))
        payload.append(Data(",\"p\":".utf8))
        payload.append(paramData)
        payload.append(Data("}".utf8))

        let limit: UInt64 = UInt64(seconds * Double(deadlineUnit))
        let reply: Data = try await withCheckedThrowingContinuation { continuation in
            pending[id] = continuation
            // A reply that never comes must not leave the caller waiting
            // for ever. The deadline is cancelled when the reply arrives.
            deadlines[id] = Task { [weak self] in
                do {
                    try await Task.sleep(nanoseconds: limit)
                } catch {
                    return // cancelled: the call has ended
                }
                await self?.expire(id: id)
            }
            // Sends are chained so that messages never interleave.
            let previous = sendChain
            sendChain = Task {
                await previous?.value
                await self.transmit(id: id, payload: payload)
            }
        }
        return try Self.makeDecoder().decode(Response<R>.self, from: reply).r
    }

    private func transmit(id: UInt32, payload: Data) async {
        do {
            try await session?.send(payload)
        } catch {
            pending.removeValue(forKey: id)?.resume(throwing: error)
            fail(error)
        }
    }
}
