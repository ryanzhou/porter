#if canImport(Network)
import Foundation
import Network

public enum TCPError: Error, LocalizedError {
    /// The connection could not be opened: nothing at that address, no
    /// route there, or the phone does not let the app onto local networks.
    case unreachable
    case closed

    public var errorDescription: String? {
        switch self {
        case .unreachable: return "The router did not answer on this network."
        case .closed: return "The connection to the router over the network was lost."
        }
    }
}

/// The control protocol's link over the router's own networks: a TCP
/// connection carrying packets with a 2-byte length prefix, big endian
/// (see docs/PROTOCOL.md).
public final class TCPTransport: PacketTransport, @unchecked Sendable {
    private let connection: NWConnection
    /// Everything of the connection happens on this queue, and so does
    /// every access to the two properties below.
    private let queue = DispatchQueue(label: "porter.tcp")
    private var opening: CheckedContinuation<Void, Error>?
    private var ready = false
    private var closed = false

    private init(host: String, port: Int) throws {
        guard port > 0, let endpointPort = NWEndpoint.Port(rawValue: UInt16(clamping: port)) else { throw TCPError.unreachable }
        let tcp = NWProtocolTCP.Options()
        tcp.noDelay = true
        // A connection whose other end has gone quiet (the phone has left
        // the network) is found out by the system as well as by the
        // heartbeat above it.
        tcp.enableKeepalive = true
        tcp.keepaliveIdle = 10
        tcp.keepaliveInterval = 3
        tcp.keepaliveCount = 3
        let parameters = NWParameters(tls: nil, tcp: tcp)
        // The router's own networks are never reached through the mobile
        // network. (Through a WireGuard tunnel home they are, wherever
        // the phone is: the tunnel is an interface of its own.)
        parameters.prohibitedInterfaceTypes = [.cellular]
        connection = NWConnection(host: NWEndpoint.Host(host), port: endpointPort, using: parameters)
    }

    /// Opens a connection, giving up after `timeout` seconds or when the
    /// task is cancelled.
    public static func connect(host: String, port: Int, timeout: TimeInterval) async throws -> TCPTransport {
        let transport = try TCPTransport(host: host, port: port)
        try await withTaskCancellationHandler {
            try await transport.open(timeout: timeout)
        } onCancel: {
            transport.close()
        }
        return transport
    }

    private func open(timeout: TimeInterval) async throws {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
            queue.async {
                // Closed before it was opened: the attempt was cancelled.
                if self.closed {
                    c.resume(throwing: TCPError.unreachable)
                    return
                }
                self.opening = c
                self.connection.stateUpdateHandler = { [weak self] state in
                    self?.stateChanged(state)
                }
                // A path that stops being usable (Wi-Fi left) ends the
                // connection at once instead of leaving it to time out.
                self.connection.viabilityUpdateHandler = { [weak self] viable in
                    if !viable { self?.connection.cancel() }
                }
                self.connection.start(queue: self.queue)
                self.queue.asyncAfter(deadline: .now() + timeout) { [weak self] in
                    guard let self, !self.ready else { return }
                    self.connection.cancel()
                }
            }
        }
    }

    /// Runs on the queue.
    private func stateChanged(_ state: NWConnection.State) {
        switch state {
        case .ready:
            ready = true
            opening?.resume()
            opening = nil
        case .waiting:
            // No way there right now. While opening that is an answer:
            // the router is on this network or it is not.
            if !ready { connection.cancel() }
        case .failed, .cancelled:
            opening?.resume(throwing: TCPError.unreachable)
            opening = nil
        default:
            break
        }
    }

    public func send(_ packet: Data) async throws {
        guard packet.count <= Framing.maxPacket else { throw SessionError.messageTooLarge }
        var framed = Data([UInt8(packet.count >> 8), UInt8(packet.count & 0xff)])
        framed.append(packet)
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
            connection.send(content: framed, completion: .contentProcessed { error in
                if let error {
                    c.resume(throwing: error)
                } else {
                    c.resume()
                }
            })
        }
    }

    public func receive() async throws -> Data {
        let header: Data = try await read(2)
        let length: Int = Int(header[header.startIndex]) << 8 | Int(header[header.startIndex + 1])
        if length == 0 { return Data() }
        return try await read(length)
    }

    private func read(_ count: Int) async throws -> Data {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data, Error>) in
            connection.receive(minimumIncompleteLength: count, maximumLength: count) { data, _, _, error in
                if let data, data.count == count {
                    c.resume(returning: data)
                } else if let error {
                    c.resume(throwing: error)
                } else {
                    c.resume(throwing: TCPError.closed)
                }
            }
        }
    }

    public func close() {
        queue.async {
            self.closed = true
            self.opening?.resume(throwing: TCPError.unreachable)
            self.opening = nil
            self.connection.cancel()
        }
    }
}
#endif
