import Foundation
#if canImport(Security)
import Security
#endif

/// A router this app has paired with.
public struct KnownRouter: Codable, Equatable, Identifiable {
    /// The router's identifier (first bytes of its public key, hex).
    public var id: String
    public var name: String
    /// The static public key pinned at pairing time.
    public var publicKey: Data
    /// CoreBluetooth's identifier for the router on this phone.
    public var peripheral: UUID
    /// Where the router last said it can be reached over its own
    /// networks, for trying that way before it has been asked again.
    public var endpoints: ControlEndpoints

    public init(id: String, name: String, publicKey: Data, peripheral: UUID, endpoints: ControlEndpoints = ControlEndpoints()) {
        self.id = id
        self.name = name
        self.publicKey = publicKey
        self.peripheral = peripheral
        self.endpoints = endpoints
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decode(String.self, forKey: .name)
        publicKey = try c.decode(Data.self, forKey: .publicKey)
        peripheral = try c.decode(UUID.self, forKey: .peripheral)
        // Records from before routers could be reached over the network
        // have none.
        endpoints = try c.decodeIfPresent(ControlEndpoints.self, forKey: .endpoints) ?? ControlEndpoints()
    }
}

/// Persists the app's identity key (Keychain) and its paired routers.
public final class KeyStore {
    private let defaults: UserDefaults
    private static let service = "porter.identity"
    private static let routersKey = "porter.routers"

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    // MARK: Paired routers

    public func routers() -> [KnownRouter] {
        guard let data = defaults.data(forKey: Self.routersKey) else { return [] }
        return (try? JSONDecoder().decode([KnownRouter].self, from: data)) ?? []
    }

    public func save(_ router: KnownRouter) {
        var list = routers().filter { $0.id != router.id }
        list.append(router)
        defaults.set(try? JSONEncoder().encode(list), forKey: Self.routersKey)
    }

    public func remove(routerID: String) {
        let list = routers().filter { $0.id != routerID }
        defaults.set(try? JSONEncoder().encode(list), forKey: Self.routersKey)
    }

    // MARK: Identity

    #if canImport(Security)
    /// The app's identity, created on first use. It stays on this device:
    /// the Keychain item is not synchronised and not included in backups
    /// restored to other devices.
    public func identity() throws -> Identity {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: Self.service,
            kSecAttrAccount as String: "static-key",
            kSecReturnData as String: true,
        ]
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecSuccess, let data = item as? Data {
            return try Identity(privateKey: data)
        }
        let identity = Identity()
        let add: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: Self.service,
            kSecAttrAccount as String: "static-key",
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
            kSecValueData as String: identity.privateKey,
        ]
        let addStatus = SecItemAdd(add as CFDictionary, nil)
        guard addStatus == errSecSuccess else {
            throw NSError(domain: NSOSStatusErrorDomain, code: Int(addStatus), userInfo: [
                NSLocalizedDescriptionKey: "Could not store this phone's key in the Keychain.",
            ])
        }
        return identity
    }
    #endif
}
