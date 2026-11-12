import Foundation
#if canImport(CryptoKit)
import CryptoKit
#else
import Crypto
#endif

// Noise_XX_25519_ChaChaPoly_SHA256, initiator side, as specified in
// docs/PROTOCOL.md. The router is always the responder.

public enum NoiseError: Error, Equatable {
    case malformedMessage
    case authenticationFailed
    case outOfOrder
}

/// A long-term X25519 key pair identifying this app to routers.
public struct Identity: Sendable {
    public let privateKey: Data
    public let publicKey: Data

    public init() {
        let key = Curve25519.KeyAgreement.PrivateKey()
        privateKey = key.rawRepresentation
        publicKey = key.publicKey.rawRepresentation
    }

    public init(privateKey: Data) throws {
        let key = try Curve25519.KeyAgreement.PrivateKey(rawRepresentation: privateKey)
        self.privateKey = key.rawRepresentation
        publicKey = key.publicKey.rawRepresentation
    }
}

struct CipherState {
    private var key: SymmetricKey?
    private var counter: UInt64 = 0

    init(key: Data? = nil) {
        self.key = key.map { SymmetricKey(data: $0) }
    }

    private func nonce() throws -> ChaChaPoly.Nonce {
        // 32 zero bits followed by the 64-bit counter, little endian.
        var bytes = Data(count: 4)
        withUnsafeBytes(of: counter.littleEndian) { bytes.append(contentsOf: $0) }
        return try ChaChaPoly.Nonce(data: bytes)
    }

    mutating func encrypt(_ plaintext: Data, ad: Data) throws -> Data {
        guard let key else { return plaintext }
        let box = try ChaChaPoly.seal(plaintext, using: key, nonce: nonce(), authenticating: ad)
        counter += 1
        return box.ciphertext + box.tag
    }

    mutating func decrypt(_ ciphertext: Data, ad: Data) throws -> Data {
        guard let key else { return ciphertext }
        guard ciphertext.count >= 16 else { throw NoiseError.malformedMessage }
        let split = ciphertext.index(ciphertext.endIndex, offsetBy: -16)
        do {
            let box = try ChaChaPoly.SealedBox(nonce: nonce(), ciphertext: ciphertext[..<split], tag: ciphertext[split...])
            let plaintext = try ChaChaPoly.open(box, using: key, authenticating: ad)
            counter += 1
            return plaintext
        } catch {
            throw NoiseError.authenticationFailed
        }
    }
}

private func hmac(_ key: Data, _ data: Data) -> Data {
    Data(HMAC<SHA256>.authenticationCode(for: data, using: SymmetricKey(data: key)))
}

/// Noise's HKDF with two outputs.
private func hkdf(_ chainingKey: Data, _ material: Data) -> (Data, Data) {
    let temp = hmac(chainingKey, material)
    let out1 = hmac(temp, Data([1]))
    let out2 = hmac(temp, out1 + Data([2]))
    return (out1, out2)
}

/// Not private to this file: the tests play the router's side of the
/// handshake with it.
struct SymmetricState {
    var chainingKey: Data
    var hash: Data
    var cipher = CipherState()

    init(protocolName: String) {
        var name = Data(protocolName.utf8)
        if name.count <= 32 {
            name.append(Data(count: 32 - name.count))
            hash = name
        } else {
            hash = Data(SHA256.hash(data: name))
        }
        chainingKey = hash
    }

    mutating func mixHash(_ data: Data) {
        hash = Data(SHA256.hash(data: hash + data))
    }

    mutating func mixKey(_ material: Data) {
        let (ck, key) = hkdf(chainingKey, material)
        chainingKey = ck
        cipher = CipherState(key: key)
    }

    mutating func encryptAndHash(_ plaintext: Data) throws -> Data {
        let out = try cipher.encrypt(plaintext, ad: hash)
        mixHash(out)
        return out
    }

    mutating func decryptAndHash(_ ciphertext: Data) throws -> Data {
        let out = try cipher.decrypt(ciphertext, ad: hash)
        mixHash(ciphertext)
        return out
    }

    func split() -> (CipherState, CipherState) {
        let (k1, k2) = hkdf(chainingKey, Data())
        return (CipherState(key: k1), CipherState(key: k2))
    }
}

/// The initiator's side of one XX handshake.
struct Handshake {
    private var state = SymmetricState(protocolName: "Noise_XX_25519_ChaChaPoly_SHA256")
    private let s: Curve25519.KeyAgreement.PrivateKey
    private let e: Curve25519.KeyAgreement.PrivateKey
    private var re: Curve25519.KeyAgreement.PublicKey?
    private var step = 0
    /// The router's static key, known after `readMessage2`.
    private(set) var remoteStatic: Data?

    /// `ephemeral` is injectable so tests can reproduce known vectors.
    init(identity: Identity, prologue: Data, ephemeral: Data? = nil) throws {
        s = try Curve25519.KeyAgreement.PrivateKey(rawRepresentation: identity.privateKey)
        if let ephemeral {
            e = try Curve25519.KeyAgreement.PrivateKey(rawRepresentation: ephemeral)
        } else {
            e = Curve25519.KeyAgreement.PrivateKey()
        }
        state.mixHash(prologue)
    }

    private func dh(_ priv: Curve25519.KeyAgreement.PrivateKey, _ pub: Curve25519.KeyAgreement.PublicKey) throws -> Data {
        try priv.sharedSecretFromKeyAgreement(with: pub).withUnsafeBytes { Data($0) }
    }

    /// -> e
    mutating func writeMessage1() throws -> Data {
        guard step == 0 else { throw NoiseError.outOfOrder }
        step = 1
        let pub = e.publicKey.rawRepresentation
        state.mixHash(pub)
        return pub + (try state.encryptAndHash(Data()))
    }

    /// <- e, ee, s, es
    mutating func readMessage2(_ message: Data) throws {
        guard step == 1 else { throw NoiseError.outOfOrder }
        step = 2
        guard message.count == 32 + 48 + 16 else { throw NoiseError.malformedMessage }
        let m = Data(message)
        let remoteEphemeral = try Curve25519.KeyAgreement.PublicKey(rawRepresentation: m.subdata(in: 0..<32))
        re = remoteEphemeral
        state.mixHash(m.subdata(in: 0..<32))
        state.mixKey(try dh(e, remoteEphemeral))
        let rs = try state.decryptAndHash(m.subdata(in: 32..<80))
        let remote = try Curve25519.KeyAgreement.PublicKey(rawRepresentation: rs)
        state.mixKey(try dh(e, remote))
        _ = try state.decryptAndHash(m.subdata(in: 80..<96))
        remoteStatic = rs
    }

    /// -> s, se. Returns the message and the transport ciphers.
    mutating func writeMessage3() throws -> (message: Data, send: CipherState, receive: CipherState) {
        guard step == 2, let re else { throw NoiseError.outOfOrder }
        step = 3
        var out = try state.encryptAndHash(s.publicKey.rawRepresentation)
        state.mixKey(try dh(s, re))
        out += try state.encryptAndHash(Data())
        let (c1, c2) = state.split()
        return (out, c1, c2)
    }
}
