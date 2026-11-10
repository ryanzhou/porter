import XCTest
@testable import PorterKit

// Checks this implementation against vectors produced by the router's Go
// implementation (daemon/internal/proto/vectors_test.go).

struct Vectors: Decodable {
    struct Message: Decodable {
        let plaintext: String
        let packets: [String]
    }
    struct Frames: Decodable {
        let tag: UInt16
        let mtu: Int
        let packet: String
        let fragments: [String]
    }
    let prologue: String
    let app_static_private: String
    let app_static_public: String
    let app_ephemeral_private: String
    let router_static_public: String
    let handshake: [String]
    let app_to_router: [Message]
    let router_to_app: [Message]
    let fragmentation: Frames
}

extension Data {
    init(hex: String) {
        self.init()
        var index = hex.startIndex
        while index < hex.endIndex {
            let next = hex.index(index, offsetBy: 2)
            append(UInt8(hex[index..<next], radix: 16)!)
            index = next
        }
    }
    var hex: String { map { String(format: "%02x", $0) }.joined() }
}

/// Plays back the router's side of a recorded exchange.
final class ReplayTransport: PacketTransport {
    var incoming: [Data]
    var sent: [Data] = []
    init(incoming: [Data]) { self.incoming = incoming }
    func send(_ packet: Data) async throws { sent.append(packet) }
    func receive() async throws -> Data {
        guard !incoming.isEmpty else { throw SessionError.closed }
        return incoming.removeFirst()
    }
    func close() {}
}

final class VectorTests: XCTestCase {
    func loadVectors() throws -> Vectors {
        let url = try XCTUnwrap(Bundle.module.url(forResource: "vectors", withExtension: "json"))
        return try JSONDecoder().decode(Vectors.self, from: Data(contentsOf: url))
    }

    func testHandshakeAndTransport() async throws {
        let v = try loadVectors()
        XCTAssertEqual(v.prologue, String(decoding: SecureSession.prologue, as: UTF8.self))
        let identity = try Identity(privateKey: Data(hex: v.app_static_private))
        XCTAssertEqual(identity.publicKey.hex, v.app_static_public)

        var incoming = [Data(hex: v.handshake[1])]
        for message in v.router_to_app { incoming += message.packets.map { Data(hex: $0) } }
        let transport = ReplayTransport(incoming: incoming)

        let session = try await SecureSession.open(
            transport: transport, identity: identity,
            expectedRouterKey: Data(hex: v.router_static_public),
            ephemeral: Data(hex: v.app_ephemeral_private))
        XCTAssertEqual(session.routerKey.hex, v.router_static_public)
        XCTAssertEqual(transport.sent.map(\.hex), [v.handshake[0], v.handshake[2]])

        // What we send must be byte-identical to what the Go client sent.
        transport.sent.removeAll()
        for message in v.app_to_router {
            try await session.send(Data(hex: message.plaintext))
        }
        XCTAssertEqual(transport.sent.map(\.hex), v.app_to_router.flatMap(\.packets))

        // And we must read what the Go router sent.
        for message in v.router_to_app {
            let got = try await session.receive()
            XCTAssertEqual(got.hex, message.plaintext)
        }
    }

    func testRouterKeyMismatchIsRejected() async throws {
        let v = try loadVectors()
        let identity = try Identity(privateKey: Data(hex: v.app_static_private))
        let transport = ReplayTransport(incoming: [Data(hex: v.handshake[1])])
        var wrong = Data(hex: v.router_static_public)
        wrong[0] ^= 1
        do {
            _ = try await SecureSession.open(
                transport: transport, identity: identity, expectedRouterKey: wrong,
                ephemeral: Data(hex: v.app_ephemeral_private))
            XCTFail("session opened with the wrong router key")
        } catch let error as SessionError {
            XCTAssertEqual(error, .routerKeyMismatch)
        }
        // The app's static key must not have been revealed to an impostor.
        XCTAssertEqual(transport.sent.count, 1)
    }

    func testTamperedPacketIsRejected() async throws {
        let v = try loadVectors()
        let identity = try Identity(privateKey: Data(hex: v.app_static_private))
        var bad = Data(hex: v.router_to_app[0].packets[0])
        bad[bad.count - 1] ^= 0x01
        let transport = ReplayTransport(incoming: [Data(hex: v.handshake[1]), bad])
        let session = try await SecureSession.open(
            transport: transport, identity: identity, expectedRouterKey: nil,
            ephemeral: Data(hex: v.app_ephemeral_private))
        do {
            _ = try await session.receive()
            XCTFail("tampered packet accepted")
        } catch let error as NoiseError {
            XCTAssertEqual(error, .authenticationFailed)
        }
    }

    func testFragmentation() throws {
        let v = try loadVectors().fragmentation
        let packet = Data(hex: v.packet)
        let fragments = Framing.fragment(tag: v.tag, packet: packet, mtu: v.mtu)
        XCTAssertEqual(fragments.map(\.hex), v.fragments)

        var reassembler = Reassembler()
        var result: Data?
        for fragment in fragments {
            XCTAssertEqual(Framing.tag(of: fragment), v.tag)
            XCTAssertNil(result)
            result = try reassembler.add(fragment)
        }
        XCTAssertEqual(result, packet)

        // Slices of larger buffers (as CoreBluetooth may hand over) work too.
        let padded = Data([0xff, 0xff]) + fragments[0]
        var second = Reassembler()
        XCTAssertNil(try second.add(padded.dropFirst(2)))

        // Empty packets still produce one fragment.
        XCTAssertEqual(Framing.fragment(tag: 1, packet: Data(), mtu: 20), [Data([0, 1, 0xc0])])
        var orphan = Reassembler()
        XCTAssertThrowsError(try orphan.add(Data([0, 1, 0x00, 9])))
    }
}
