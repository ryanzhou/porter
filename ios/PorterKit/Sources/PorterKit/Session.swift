import Foundation

/// Carries whole packets, in order. Implemented by the BLE link.
public protocol PacketTransport: AnyObject {
    func send(_ packet: Data) async throws
    /// The next packet from the router; throws when the link is gone.
    func receive() async throws -> Data
    func close()
}

public enum SessionError: Error, Equatable {
    /// The router presented a different key than the one this app paired
    /// with. Never proceed: it is either another router or an attack.
    case routerKeyMismatch
    case messageTooLarge
    case closed
    /// A request went unanswered past its deadline; the connection is
    /// given up, as when the link is lost.
    case timeout
}

extension SessionError: LocalizedError {
    /// Only the timeout has words of its own. The others are for the app
    /// to describe, which it does all in the same way.
    public var errorDescription: String? {
        switch self {
        case .timeout: return "The router stopped answering."
        case .routerKeyMismatch, .messageTooLarge, .closed: return nil
        }
    }
}

/// An authenticated, encrypted channel to a router.
public final class SecureSession {
    public static let prologue = Data("porter/1".utf8)
    static let maxChunk = 8000
    static let maxMessage = 1 << 20

    private let transport: PacketTransport
    private var sendCipher: CipherState
    private var receiveCipher: CipherState
    /// The router's static public key, authenticated by the handshake.
    public let routerKey: Data

    private init(transport: PacketTransport, send: CipherState, receive: CipherState, routerKey: Data) {
        self.transport = transport
        self.sendCipher = send
        self.receiveCipher = receive
        self.routerKey = routerKey
    }

    /// Runs the handshake. If `expectedRouterKey` is given the router
    /// must prove possession of exactly that key.
    public static func open(
        transport: PacketTransport, identity: Identity, expectedRouterKey: Data?, ephemeral: Data? = nil
    ) async throws -> SecureSession {
        var hs = try Handshake(identity: identity, prologue: prologue, ephemeral: ephemeral)
        try await transport.send(try hs.writeMessage1())
        try hs.readMessage2(try await transport.receive())
        guard let routerKey = hs.remoteStatic else { throw NoiseError.outOfOrder }
        if let expectedRouterKey, expectedRouterKey != routerKey {
            transport.close()
            throw SessionError.routerKeyMismatch
        }
        let (message, send, receive) = try hs.writeMessage3()
        try await transport.send(message)
        return SecureSession(transport: transport, send: send, receive: receive, routerKey: routerKey)
    }

    /// Sends one message. Must not be called concurrently with itself.
    public func send(_ message: Data) async throws {
        guard message.count <= Self.maxMessage else { throw SessionError.messageTooLarge }
        var rest = Data(message)
        repeat {
            let chunk = rest.prefix(Self.maxChunk)
            rest = Data(rest.dropFirst(chunk.count))
            var record = Data([rest.isEmpty ? 0 : 1])
            record.append(chunk)
            try await transport.send(try sendCipher.encrypt(record, ad: Data()))
        } while !rest.isEmpty
    }

    /// Receives one message. Must not be called concurrently with itself.
    public func receive() async throws -> Data {
        var message = Data()
        while true {
            let record = try receiveCipher.decrypt(try await transport.receive(), ad: Data())
            guard let more = record.first else { throw NoiseError.malformedMessage }
            message.append(record.dropFirst())
            guard message.count <= Self.maxMessage else { throw SessionError.messageTooLarge }
            if more == 0 { return message }
        }
    }

    public func close() { transport.close() }
}
