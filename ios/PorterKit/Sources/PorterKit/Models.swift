import Foundation

// Mirrors of the router's JSON documents. Keys are snake_case on the wire
// and converted by the coders in RouterConnection. RouterConfig mirrors
// daemon/internal/config/config.go field for field: the app writes the
// whole document back, so nothing may be dropped on the way through.

// MARK: - Configuration

public struct RouterConfig: Codable, Equatable {
    public var version: Int
    public var rev: UInt64
    public var system: SystemConfig
    public var radios: [RadioConfig]
    public var ethernet: [EthernetPort]
    /// The Wi-Fi networks the router broadcasts; the first is the main one.
    public var wifi: [WifiNetworkConfig]
    public var wan: WANConfig
    /// What all of the router's own networks have in common.
    public var lan: LANConfig
    /// The router's own networks; the first is the main one.
    public var networks: [NetworkConfig]
    public var dns: DNSConfig
    public var vpns: [VPNProfile]
    public var policy: PolicyConfig
    public var devices: [DeviceSettings]
    public var speedtest: SpeedTestConfig
}

public struct SpeedTestConfig: Codable, Equatable {
    /// Fetched repeatedly to measure download speed.
    public var downloadUrl: String
    /// Receives POSTed data to measure upload speed.
    public var uploadUrl: String
}

public struct SystemConfig: Codable, Equatable {
    public var name: String
    public var country: String
    public var ssh: SSHConfig
    /// The router is controlled over Bluetooth only, and not also over
    /// its own networks. Nil reads as false.
    public var bluetoothOnly: Bool?
}

public struct SSHConfig: Codable, Equatable {
    public var enabled: Bool
    public var authorizedKeys: [String]
}

public enum RadioRole: String, Codable, CaseIterable, Identifiable {
    case off, ap, wan
    case apWan = "ap+wan"
    public var id: String { rawValue }
}

public struct RadioConfig: Codable, Equatable, Identifiable {
    public var id: String
    public var role: RadioRole
    public var band: String
    public var channel: Int
    public var width: Int
}

public enum PortRole: String, Codable, CaseIterable, Identifiable {
    case wan, lan, off
    public var id: String { rawValue }
}

public struct EthernetPort: Codable, Equatable, Identifiable {
    public var name: String
    public var role: PortRole
    /// How the port gets its addresses as an uplink; nil from a router
    /// too old to say, which means automatic.
    public var ip: IPSetup?
    /// The VLAN ID the provider delivers the internet under, 1 to 4094,
    /// while the port is an uplink; 0 or nil for none.
    public var vlan: Int?
    /// The provider's sign-in, in place of automatic or manual IPv4
    /// addresses; nil reads as not signing in.
    public var pppoe: PPPoESetup?
    /// The network a local port belongs to.
    public var network: String
    public var id: String { name }
}

/// A provider's PPPoE sign-in for a wired uplink. The router keeps the
/// user name and password only while it is enabled.
public struct PPPoESetup: Codable, Equatable {
    public var enabled: Bool
    public var username: String
    public var password: String

    public init(enabled: Bool = false, username: String = "", password: String = "") {
        self.enabled = enabled
        self.username = username
        self.password = password
    }
}

/// One address family of an uplink's address setup.
public struct IPFamilySetup: Codable, Equatable {
    /// "auto" or "static".
    public var mode: String
    /// With its prefix length: 192.168.1.50/24, 2001:db8::50/64.
    public var address: String
    public var gateway: String
    public var dns: [String]

    public init(mode: String = "auto", address: String = "", gateway: String = "", dns: [String] = []) {
        self.mode = mode
        self.address = address
        self.gateway = gateway
        self.dns = dns
    }

    public var isStatic: Bool { mode == "static" }
}

/// How an uplink gets its addresses: per family, from the network or
/// typed in.
public struct IPSetup: Codable, Equatable {
    public var v4: IPFamilySetup
    public var v6: IPFamilySetup

    public init(v4: IPFamilySetup = IPFamilySetup(), v6: IPFamilySetup = IPFamilySetup()) {
        self.v4 = v4
        self.v6 = v6
    }

    public var isAutomatic: Bool { !v4.isStatic && !v6.isStatic }
}

public enum WifiSecurity: String, Codable, CaseIterable, Identifiable {
    case wpa2
    case wpa2wpa3 = "wpa2+wpa3"
    case wpa3
    public var id: String { rawValue }
}

/// One Wi-Fi network the router broadcasts.
public struct WifiNetworkConfig: Codable, Equatable, Identifiable {
    public var id: String
    public var ssid: String
    public var passphrase: String
    public var security: WifiSecurity
    public var hidden: Bool
    public var isolate: Bool
    /// The id of the network its devices join.
    public var network: String
    public var enabled: Bool

    public init(id: String, ssid: String = "", passphrase: String = "", security: WifiSecurity = .wpa2, hidden: Bool = false, isolate: Bool = false, network: String = RouterConfig.mainNetwork, enabled: Bool = true) {
        self.id = id
        self.ssid = ssid
        self.passphrase = passphrase
        self.security = security
        self.hidden = hidden
        self.isolate = isolate
        self.network = network
        self.enabled = enabled
    }
}

/// One of the router's own networks: an address range of its own, reached
/// through the Wi-Fi networks and wired ports assigned to it.
public struct NetworkConfig: Codable, Equatable, Identifiable {
    public var id: String
    public var name: String
    /// The router's address with prefix, like 10.83.0.1/24. Left empty
    /// for a new network, the router chooses one.
    public var address: String
    public var ulaPrefix: String
    /// Its devices cannot open connections to the other networks.
    public var isolated: Bool
    /// "", "direct", "block" or "vpn:<id>": where its devices' traffic
    /// goes unless a rule or the device says otherwise.
    public var route: String

    public init(id: String, name: String, address: String = "", ulaPrefix: String = "", isolated: Bool = false, route: String = "") {
        self.id = id
        self.name = name
        self.address = address
        self.ulaPrefix = ulaPrefix
        self.isolated = isolated
        self.route = route
    }
}

public extension RouterConfig {
    /// The format of the configuration document this version of the app
    /// reads and writes (`version` in the document).
    static let format = 3

    /// Ids of the Wi-Fi network and the network that always exist.
    static let mainWifi = "main"
    static let mainNetwork = "lan"
    /// How many of each the router accepts.
    static let maxWifiNetworks = 4
    static let maxNetworks = 4

    /// Uplink keys, as used in `wan.order`.
    static let uplinkWifi = "wifi"
    static let uplinkPhone = "phone"
    static func uplinkEthernet(_ port: String) -> String { "eth:" + port }

    func network(_ id: String) -> NetworkConfig? { networks.first { $0.id == id } }
    func networkName(_ id: String) -> String { network(id)?.name ?? id }
}

public enum MACMode: String, Codable, CaseIterable, Identifiable {
    case hardware, random, custom
    public var id: String { rawValue }
}

public struct MACPolicy: Codable, Equatable {
    public var mode: MACMode
    public var address: String?
}

public struct SavedNetwork: Codable, Equatable, Identifiable {
    public var ssid: String
    public var psk: String
    public var hidden: Bool
    public var priority: Int
    /// How the router gets its addresses on this network. Left out when
    /// joining, which keeps whatever is already set for the network.
    public var ip: IPSetup?
    public var id: String { ssid }

    public init(ssid: String, psk: String, hidden: Bool = false, priority: Int = 0) {
        self.ssid = ssid
        self.psk = psk
        self.hidden = hidden
        self.priority = priority
    }
}

public struct ProbeConfig: Codable, Equatable {
    public var url: String
    public var expect: String
}

public struct WANConfig: Codable, Equatable {
    public var networks: [SavedNetwork]
    public var wifiMac: MACPolicy
    public var ethMac: MACPolicy
    /// The uplinks, best first: "eth:<port>", "wifi", "phone". The router
    /// uses the first that works.
    public var order: [String]
    public var probe: ProbeConfig
}

public struct LANConfig: Codable, Equatable {
    public var autoRenumber: Bool
    public var leaseMinutes: Int
    public var domain: String
    public var ipv6: Bool
}

public struct DNSConfig: Codable, Equatable {
    public var upstreams: [String]
    public var intercept: Bool
    public var blocked: [String]
    public var adblock: AdblockConfig
}

/// DNS-level ad and tracker blocking from downloadable block lists.
public struct AdblockConfig: Codable, Equatable {
    public var enabled: Bool
    /// URLs of block lists (hosts, plain-domain or Adblock format).
    public var lists: [String]
    /// Domains that are never blocked, whatever the lists say.
    public var allow: [String]
}

public struct VPNProfile: Codable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var enabled: Bool
    public var killSwitch: Bool
    /// "wireguard" (also when absent) or "mpquic": which of the two
    /// sections describes the tunnel.
    public var type: String?
    public var wireguard: WireGuardConfig
    public var mpquic: MPQUICConfig?

    public init(id: String, name: String, enabled: Bool, killSwitch: Bool, wireguard: WireGuardConfig) {
        self.id = id
        self.name = name
        self.enabled = enabled
        self.killSwitch = killSwitch
        self.wireguard = wireguard
    }

    /// An MPQUIC profile.
    public init(id: String, name: String, enabled: Bool, killSwitch: Bool, mpquic: MPQUICConfig) {
        self.id = id
        self.name = name
        self.enabled = enabled
        self.killSwitch = killSwitch
        self.type = "mpquic"
        self.wireguard = WireGuardConfig()
        self.mpquic = mpquic
    }

    public var isMPQUIC: Bool { type == "mpquic" }
}

public struct WireGuardConfig: Codable, Equatable {
    public var privateKey: String
    public var addresses: [String]
    public var dns: [String]
    public var mtu: Int
    public var peers: [WireGuardPeer]

    public init(privateKey: String = "", addresses: [String] = [], dns: [String] = [], mtu: Int = 0, peers: [WireGuardPeer] = []) {
        self.privateKey = privateKey
        self.addresses = addresses
        self.dns = dns
        self.mtu = mtu
        self.peers = peers
    }
}

/// A multipath QUIC tunnel to an "mpquic remote".
public struct MPQUICConfig: Codable, Equatable {
    /// The paths to the remote: its own address, then any relays.
    public var servers: [String]
    /// Used for servers given without a port.
    public var port: Int
    public var password: String
    public var dns: [String]
    /// The line's rates if known, like "19mbit"; empty to measure.
    public var up: String
    public var down: String
    public var reliableUdp: Bool

    public init(servers: [String] = [], port: Int = 8388, password: String = "", dns: [String] = [], up: String = "", down: String = "", reliableUdp: Bool = false) {
        self.servers = servers
        self.port = port
        self.password = password
        self.dns = dns
        self.up = up
        self.down = down
        self.reliableUdp = reliableUdp
    }
}

public struct WireGuardPeer: Codable, Equatable {
    public var publicKey: String
    public var presharedKey: String?
    public var endpoint: String
    public var allowedIps: [String]
    public var keepalive: Int
}

/// Where traffic goes: "direct", "block" or "vpn:<profile id>".
public enum RouteAction: Hashable {
    case direct
    case block
    case vpn(String)

    public init?(_ raw: String) {
        switch raw {
        case "direct": self = .direct
        case "block": self = .block
        default:
            guard raw.hasPrefix("vpn:") else { return nil }
            self = .vpn(String(raw.dropFirst(4)))
        }
    }

    public var raw: String {
        switch self {
        case .direct: return "direct"
        case .block: return "block"
        case .vpn(let id): return "vpn:" + id
        }
    }
}

public struct PolicyConfig: Codable, Equatable {
    public var `default`: String
    public var rules: [PolicyRule]
}

public struct PolicyRule: Codable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var enabled: Bool
    public var src: RuleSource
    public var dst: RuleDestination
    public var action: String

    public init(id: String, name: String, enabled: Bool = true, src: RuleSource = .init(), dst: RuleDestination = .init(), action: String = "direct") {
        self.id = id
        self.name = name
        self.enabled = enabled
        self.src = src
        self.dst = dst
        self.action = action
    }
}

public struct RuleSource: Codable, Equatable {
    public var devices: [String]
    public var cidrs: [String]
    /// Ids of networks: every device on them.
    public var networks: [String]
    public init(devices: [String] = [], cidrs: [String] = [], networks: [String] = []) {
        self.devices = devices
        self.cidrs = cidrs
        self.networks = networks
    }
}

public struct RuleDestination: Codable, Equatable {
    public var domains: [String]
    public var cidrs: [String]
    /// ISO 3166-1 alpha-2 codes of destination countries.
    public var countries: [String]
    public var proto: String
    public var ports: [String]
    public init(domains: [String] = [], cidrs: [String] = [], countries: [String] = [], proto: String = "", ports: [String] = []) {
        self.domains = domains
        self.cidrs = cidrs
        self.countries = countries
        self.proto = proto
        self.ports = ports
    }
}

public struct DeviceSettings: Codable, Equatable, Identifiable {
    public var mac: String
    public var name: String
    public var route: String
    public var paused: Bool
    public var ip: String?
    /// Exempts the device from ad blocking.
    public var noAdblock: Bool
    public var id: String { mac }

    public init(mac: String, name: String = "", route: String = "", paused: Bool = false, ip: String? = nil, noAdblock: Bool = false) {
        self.mac = mac
        self.name = name
        self.route = route
        self.paused = paused
        self.ip = ip
        self.noAdblock = noAdblock
    }
}

// MARK: - Status

public struct RouterStatus: Codable, Equatable {
    public var name: String
    public var id: String
    public var version: String
    public var time: Int64
    public var uptime: Int64
    public var temp: Double
    public var ready: Bool
    public var rev: UInt64
    public var pairing: Bool
    public var internet: String
    public var active: String?
    public var wan: [UplinkStatus]
    public var portal: PortalStatus
    public var aps: [AccessPointStatus]
    public var stas: [StationStatus]
    /// Ids of the Wi-Fi networks that are switched on but that no radio
    /// is able to carry.
    public var offAir: [String]
    /// The wired interfaces that are plugged in, uplinks or not.
    public var ports: [PortStatus]
    public var lan: LANStatus
    /// Where the app reaches the router over its own networks; nil from
    /// a router too old to be reached that way.
    public var control: ControlEndpoints?
    public var vpns: [VPNStatus]
    public var geoip: GeoIPStatus
    public var adblock: AdblockStatus
}

/// Where the router listens for the app on its own networks.
public struct ControlEndpoints: Codable, Equatable, Sendable {
    /// 0 while the router does not listen there.
    public var port: Int
    /// The router's addresses in its networks that are not kept apart.
    public var addrs: [String]

    public init(port: Int = 0, addrs: [String] = []) {
        self.port = port
        self.addrs = addrs
    }

    /// Whether there is anywhere to connect to.
    public var isUsable: Bool { port > 0 && port < 65536 && !addrs.isEmpty }
}

public struct AdblockStatus: Codable, Equatable {
    public var enabled: Bool
    /// Domains on the block lists in use.
    public var domains: Int
    /// Lookups blocked since the router started.
    public var blocked: UInt64
    public var lists: [BlockListStatus]
}

public struct BlockListStatus: Codable, Equatable, Identifiable {
    public var url: String
    public var domains: Int
    /// When the copy in use was last downloaded or confirmed current.
    public var updated: Int64?
    public var error: String?
    public var id: String { url }
}

/// The country database country rules and flow listings rely on.
public struct GeoIPStatus: Codable, Equatable {
    public var available: Bool
    /// Month the database was published, e.g. "2026-11".
    public var date: String?
}

/// A wired interface that is plugged in.
public struct PortStatus: Codable, Equatable, Identifiable {
    public var name: String
    /// What a USB device calls itself.
    public var label: String?
    /// A phone sharing its connection over USB.
    public var phone: Bool
    public var carrier: Bool
    public var id: String { name }
}

public struct UplinkStatus: Codable, Equatable, Identifiable {
    public var iface: String
    /// "eth", "wifi" or "phone".
    public var kind: String
    /// Its entry in `wan.order`.
    public var key: String
    public var label: String?
    public var metric: Int
    public var rank: Int
    public var carrier: Bool
    public var internet: String
    public var ipv4: String?
    public var gateway: String?
    public var ipv6: [String]?
    public var hasV6: Bool
    /// The IPv6 prefix the network has delegated to the router for its
    /// own networks, like 2001:db8:1000::/56; nil when it has none.
    public var prefix: String?
    public var dns: [String]?
    /// For a wired uplink that signs in to its provider (PPPoE), how
    /// that stands: "signing_in", "wrong_login", "no_answer" or "up".
    public var pppoe: String?
    public var portalUrl: String?
    public var captiveApi: String?
    public var mac: String?
    public var id: String { iface }

    enum CodingKeys: String, CodingKey {
        case iface = "if"
        case kind, key, label, metric, rank, carrier, internet, ipv4, gateway, ipv6, hasV6, prefix, dns, pppoe, portalUrl, captiveApi, mac
    }
}

public struct PortalStatus: Codable, Equatable {
    public var captive: Bool
    public var url: String?
    public var device: String?
}

public struct AccessPointStatus: Codable, Equatable, Identifiable {
    public var radio: String
    public var iface: String
    public var state: String
    public var band: String
    public var channel: Int
    public var mode: String?
    public var error: String?
    /// Ids of the Wi-Fi networks this radio carries.
    public var ssids: [String]
    public var id: String { radio }
}

public struct StationStatus: Codable, Equatable, Identifiable {
    public var radio: String
    public var iface: String
    /// "connected", "connecting" (joining a particular network),
    /// "scanning" (looking for one it knows) or "disconnected".
    public var state: String
    public var ssid: String?
    public var bssid: String?
    public var freq: Int?
    public var signal: Int?
    public var failed: String?
    public var failReason: String?
    public var id: String { radio }
}

public struct LANStatus: Codable, Equatable {
    public var address: String
    public var ula: String?
    public var clients: Int
    /// Devices online in each network, by network id.
    public var networks: [String: Int]
    /// The /64 each network has of a prefix delegated to the router, by
    /// network id. A network that is not in it has its private prefix
    /// only; nil from a router too old to ask for one.
    public var prefixes: [String: String]?
}

public struct VPNStatus: Codable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var iface: String?
    public var state: String
    public var error: String?
    public var endpoint: String?
    public var handshake: Int64?
    public var rx: Int64
    public var tx: Int64
    public var type: String?
    /// For an MPQUIC tunnel: how many of its paths work, and of how many.
    public var paths: Int?
    public var of: Int?

    enum CodingKeys: String, CodingKey {
        case iface = "if"
        case id, name, state, error, endpoint, handshake, rx, tx, type, paths, of
    }

    public var isMPQUIC: Bool { type == "mpquic" }
}

public struct ActivityEvent: Codable, Equatable, Identifiable {
    public var t: Int64
    public var kind: String
    public var msg: String
    public var mac: String?
    public var id: String { "\(t)-\(kind)-\(msg)" }
}

// MARK: - Other results

public struct HelloInfo: Codable, Equatable {
    public var name: String
    public var id: String
    public var version: String
    public var paired: Bool
    public var claimed: Bool
    public var pairing: Bool
    public var ready: Bool
    /// As in the status; only told to a paired phone.
    public var control: ControlEndpoints?
}

/// The unauthenticated Info characteristic.
public struct RouterInfo: Codable, Equatable {
    public var v: Int
    public var name: String
    public var id: String
    public var claimed: Bool
    public var pairing: Bool
}

public struct RadioChannel: Codable, Equatable {
    public var num: Int
    public var freq: Int
    public var disabled: Bool?
    public var noIr: Bool?
    public var radar: Bool?
    /// Usable for an access point without radar detection.
    public var usable: Bool { !(disabled ?? false) && !(noIr ?? false) && !(radar ?? false) }
}

public struct RadioBand: Codable, Equatable {
    public var name: String
    public var channels: [RadioChannel]
    public var ht: Bool
    public var vht: Bool
    public var he: Bool
}

public struct RadioInfo: Codable, Equatable, Identifiable {
    public var id: String
    public var phy: String
    public var iface: String
    public var driver: String
    public var bus: String
    public var product: String
    public var mac: String
    public var bands: [RadioBand]
    public var ap: Bool
    public var sta: Bool
    public var apSta: Bool
    /// How many Wi-Fi networks it can broadcast at once, and how many
    /// while it also joins an upstream network.
    public var maxAp: Int
    public var maxApSta: Int
    public var present: Bool
    public var role: RadioRole
}

/// What applying a configuration would do; the answer to `checkConfig`.
public struct ConfigCheck: Codable, Equatable {
    /// Why the router would refuse it; nil when it would not.
    public var error: String?
    /// What the hardware cannot do of what is asked.
    public var notes: [String]
    /// What people using the router will notice.
    public var effects: [String]

    public init(error: String? = nil, notes: [String] = [], effects: [String] = []) {
        self.error = error
        self.notes = notes
        self.effects = effects
    }
}

public struct ScanResult: Codable, Equatable, Identifiable {
    public var ssid: String
    public var signal: Int
    public var freq: Int
    public var security: String
    public var known: Bool
    public var id: String { ssid }
}

public struct DeviceInfo: Codable, Equatable, Identifiable {
    public var mac: String
    public var hostname: String?
    public var ipv4: String?
    public var ipv6: [String]?
    public var firstSeen: Int64
    public var lastSeen: Int64
    public var online: Bool
    public var onlineSince: Int64?
    public var link: String?
    /// The id of the network the device is on, while it is online.
    public var network: String?
    public var signal: Int?
    public var up: UInt64
    public var down: UInt64
    public var name: String
    public var route: String
    public var paused: Bool
    public var ip: String?
    public var portal: Bool?
    public var noAdblock: Bool
    public var id: String { mac }

    /// The name to show: the user's, else the device's own, else its MAC.
    public var displayName: String {
        if !name.isEmpty { return name }
        if let hostname, !hostname.isEmpty { return hostname }
        return mac.uppercased()
    }

    public var settings: DeviceSettings {
        DeviceSettings(mac: mac, name: name, route: route, paused: paused, ip: ip, noAdblock: noAdblock)
    }
}

public struct DNSQuery: Codable, Equatable, Identifiable {
    public var t: Int64
    public var mac: String
    public var name: String
    public var type: String
    public var path: String
    public var failed: Bool?
    public var id: String { "\(t)-\(name)-\(type)" }
}

public struct PairedPhone: Codable, Equatable, Identifiable {
    public var key: String
    public var name: String
    public var added: Int64
    public var lastSeen: Int64
    /// True for the phone making the request.
    public var isSelf: Bool
    public var id: String { key }

    enum CodingKeys: String, CodingKey {
        case key, name, added, lastSeen
        case isSelf = "self"
    }
}

public struct KeyPair: Codable, Equatable {
    public var privateKey: String
    public var publicKey: String
}

// MARK: - Traffic

/// Bytes moved during one time bucket starting at `t` (unix seconds).
public struct TrafficSample: Codable, Equatable, Identifiable {
    public var t: Int64
    public var up: UInt64
    public var down: UInt64
    public var id: Int64 { t }
}

public struct TrafficStats: Codable, Equatable {
    /// The last 60 minutes, one sample per minute, oldest first.
    public var minutes: [TrafficSample]
    /// The last 72 hours, one sample per hour, oldest first.
    public var hours: [TrafficSample]
}

public struct TopTalker: Codable, Equatable, Identifiable {
    public var mac: String
    public var up: UInt64
    public var down: UInt64
    public var id: String { mac }
}

/// One connection from a LAN device, current or recently ended.
public struct Flow: Codable, Equatable, Identifiable {
    public var mac: String
    public var proto: String
    public var dst: String
    public var port: Int
    /// The host name the device looked up for this address, if known.
    public var name: String?
    /// ISO country code of the destination, if known.
    public var country: String?
    /// "direct", "vpn:<id>" or "block".
    public var path: String
    public var up: UInt64
    public var down: UInt64
    public var started: Int64
    public var last: Int64
    public var active: Bool
    public var id: String { "\(mac)-\(proto)-\(dst)-\(port)-\(started)" }
}

public struct SpeedTestResult: Codable, Equatable, Identifiable {
    public var t: Int64
    /// "direct" or "vpn:<id>".
    public var via: String
    public var latencyMs: Double
    public var downMbps: Double
    public var upMbps: Double
    public var error: String?
    public var id: Int64 { t }
}

/// Live progress of a running speed test.
public struct SpeedTestProgress: Codable, Equatable {
    /// "latency", "download", "upload" or "done".
    public var phase: String
    public var mbps: Double

    public init(phase: String, mbps: Double) {
        self.phase = phase
        self.mbps = mbps
    }
}

/// What the router knows about its own software and the newest release.
public struct FirmwareInfo: Codable, Equatable {
    public var current: String
    /// False on a card flashed before the router could update itself.
    public var supported: Bool
    public var latest: String?
    public var newer: Bool
    /// Size of the update in bytes.
    public var download: Int64?
    public var built: Int64?
}

/// The state of an installation.
public struct FirmwareProgress: Codable, Equatable {
    /// "idle", "installing" or "failed".
    public var state: String
    /// While installing: "download", "verify" or "restart".
    public var phase: String?
    public var percent: Int
    public var version: String?
    public var error: String?

    public init(state: String, phase: String? = nil, percent: Int = 0, version: String? = nil, error: String? = nil) {
        self.state = state
        self.phase = phase
        self.percent = percent
        self.version = version
        self.error = error
    }
}

// MARK: - Typed calls

// What the calls below send and read that has no use anywhere else.
private struct ConfigHead: Decodable { let version: Int }
private struct ConfigParams: Encodable {
    let config: RouterConfig
    var id: String?
}
private struct RevisionResult: Decodable { let rev: UInt64 }

public extension RouterAPI {
    func hello() async throws -> HelloInfo { try await call("hello") }
    /// Asks for nothing but an answer: a sign that the link works.
    func ping() async throws { try await run("ping") }
    func pair(name: String) async throws { try await run("pair", ["name": name]) }
    func status() async throws -> RouterStatus { try await call("status.get") }
    func config() async throws -> RouterConfig { try await call("config.get") }

    /// The format of the router's configuration document, read without
    /// decoding the rest of it: a router of another generation than the
    /// app has a document the app cannot read, and this is how to tell.
    func configFormat() async throws -> Int {
        let head: ConfigHead = try await call("config.get")
        return head.version
    }

    /// Writes the configuration; returns the new revision. A write that
    /// carries an `id` can be sent again under the same one when its
    /// answer was lost: the router makes it once and answers both times.
    func setConfig(_ config: RouterConfig, id: String? = nil) async throws -> UInt64 {
        let r: RevisionResult = try await call("config.set", ConfigParams(config: config, id: id))
        return r.rev
    }

    /// Applies an edit to the router's configuration and returns what the
    /// router holds afterwards. `known` is the copy already here, which
    /// saves fetching it; if the router has moved on from it, the edit is
    /// applied once more to a fresh copy.
    ///
    /// A write whose answer is lost with the link is sent again, as the
    /// same write, over whatever link there is next. The router knows it
    /// by its id: it makes the change if it had not, and only says so
    /// again if it had. Either way the edit is made exactly once.
    func writeConfig(known: RouterConfig?, _ edit: (inout RouterConfig) -> Void) async throws -> RouterConfig {
        var base: RouterConfig? = known
        for round in 0..<2 {
            let before: RouterConfig
            if let base {
                before = base
            } else {
                before = try await config()
            }
            var next: RouterConfig = before
            edit(&next)
            if round > 0, next == before {
                // The router already holds what the edit asks for: a write
                // of which nothing was heard again has been made after all.
                return before
            }
            let id: String = UUID().uuidString
            do {
                do {
                    _ = try await setConfig(next, id: id)
                } catch where isLinkFailure(error) {
                    _ = try await setConfig(next, id: id)
                }
                break
            } catch let error as RouterError where error.code == RouterError.conflict && round == 0 {
                base = nil
            }
        }
        return try await config()
    }

    /// Asks what applying a configuration would do, without applying it.
    func checkConfig(_ config: RouterConfig) async throws -> ConfigCheck {
        return try await call("config.check", ConfigParams(config: config))
    }

    func radios() async throws -> [RadioInfo] { try await call("radios.list") }
    /// Upstream networks in range. With `quick`, what the last scan found
    /// comes back at once and a new scan starts behind it.
    func scanWifi(quick: Bool = false) async throws -> [ScanResult] { try await call("wifi.scan", ["quick": quick]) }
    /// Chooses the radio that joins upstream Wi-Fi networks ("" for none);
    /// the router rearranges the other radios around it.
    func setWifiUplink(radio: String) async throws { try await run("wifi.uplink", ["radio": radio]) }
    func connectWifi(_ network: SavedNetwork) async throws { try await run("wifi.connect", network) }
    func forgetWifi(ssid: String) async throws { try await run("wifi.forget", ["ssid": ssid]) }
    func newMAC(kind: String) async throws { try await run("wan.new_mac", ["kind": kind]) }
    func recheckInternet() async throws { try await run("wan.recheck") }

    func devices() async throws -> [DeviceInfo] { try await call("devices.list") }
    func setDevice(_ device: DeviceSettings) async throws { try await run("device.set", device) }
    func forgetDevice(mac: String) async throws { try await run("device.forget", ["mac": mac]) }
    func queries(mac: String) async throws -> [DNSQuery] { try await call("device.queries", ["mac": mac]) }

    func parseWireGuard(conf: String) async throws -> WireGuardConfig { try await call("vpn.parse", ["conf": conf]) }
    func generateKey() async throws -> KeyPair { try await call("vpn.genkey") }

    func setPortalDevice(mac: String) async throws { try await run("portal.set_device", ["mac": mac]) }
    func kickPortalDevice() async throws { try await run("portal.kick") }

    func openPairing() async throws { try await run("pairing.open") }
    func pairedPhones() async throws -> [PairedPhone] { try await call("pairing.list") }
    func revokePhone(key: String) async throws { try await run("pairing.revoke", ["key": key]) }

    /// Traffic history for one device, or the whole network when `mac` is empty.
    func trafficStats(mac: String = "") async throws -> TrafficStats { try await call("stats.get", ["mac": mac]) }
    /// Devices ordered by traffic over the last `hours` hours.
    func topTalkers(hours: Int = 24) async throws -> [TopTalker] { try await call("stats.top", ["hours": hours]) }
    /// Connections of one device, or of every device when `mac` is empty.
    func flows(mac: String = "") async throws -> [Flow] { try await call("flows.list", ["mac": mac]) }
    /// Runs a speed test from the router; takes about 20 seconds.
    func runSpeedTest(via: String = "direct") async throws -> SpeedTestResult { try await call("speedtest.run", ["via": via]) }
    func speedTestHistory() async throws -> [SpeedTestResult] { try await call("speedtest.history") }

    /// Looks for newer block lists and a newer country database now.
    func checkForUpdates() async throws { try await run("updates.check") }

    /// Asks the router whether a newer version of its software exists.
    func firmwareCheck() async throws -> FirmwareInfo { try await call("firmware.check") }
    /// Starts installing the newest version; progress arrives as events.
    func firmwareInstall() async throws { try await run("firmware.install") }
    func firmwareStatus() async throws -> FirmwareProgress { try await call("firmware.status") }

    func activity() async throws -> [ActivityEvent] { try await call("events.list") }
    func logs() async throws -> [String] { try await call("system.logs") }
    func setTime(_ date: Date = Date()) async throws {
        try await run("system.set_time", ["unix_ms": Int64(date.timeIntervalSince1970 * 1000)])
    }
    func reboot() async throws { try await run("system.reboot") }
    func powerOff() async throws { try await run("system.poweroff") }
    func factoryReset() async throws { try await run("system.factory_reset") }
}
