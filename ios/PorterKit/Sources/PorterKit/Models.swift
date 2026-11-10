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
    public var ap: AccessPointConfig
    public var wan: WANConfig
    public var lan: LANConfig
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
    public var id: String { name }
}

public enum WifiSecurity: String, Codable, CaseIterable, Identifiable {
    case wpa2
    case wpa2wpa3 = "wpa2+wpa3"
    case wpa3
    public var id: String { rawValue }
}

public struct AccessPointConfig: Codable, Equatable {
    public var ssid: String
    public var passphrase: String
    public var security: WifiSecurity
    public var hidden: Bool
    public var isolate: Bool
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
    public var preferWifi: Bool
    public var probe: ProbeConfig
}

public struct LANConfig: Codable, Equatable {
    public var address: String
    public var autoRenumber: Bool
    public var leaseMinutes: Int
    public var domain: String
    public var ipv6: Bool
    public var ulaPrefix: String
}

public struct DNSConfig: Codable, Equatable {
    public var upstreams: [String]
    public var intercept: Bool
    public var blocked: [String]
}

public struct VPNProfile: Codable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var enabled: Bool
    public var killSwitch: Bool
    public var wireguard: WireGuardConfig

    public init(id: String, name: String, enabled: Bool, killSwitch: Bool, wireguard: WireGuardConfig) {
        self.id = id
        self.name = name
        self.enabled = enabled
        self.killSwitch = killSwitch
        self.wireguard = wireguard
    }
}

public struct WireGuardConfig: Codable, Equatable {
    public var privateKey: String
    public var addresses: [String]
    public var dns: [String]
    public var mtu: Int
    public var peers: [WireGuardPeer]
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
    public init(devices: [String] = [], cidrs: [String] = []) {
        self.devices = devices
        self.cidrs = cidrs
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
    public var id: String { mac }

    public init(mac: String, name: String = "", route: String = "", paused: Bool = false, ip: String? = nil) {
        self.mac = mac
        self.name = name
        self.route = route
        self.paused = paused
        self.ip = ip
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
    public var lan: LANStatus
    public var vpns: [VPNStatus]
    public var geoip: GeoIPStatus
}

/// The country database country rules and flow listings rely on.
public struct GeoIPStatus: Codable, Equatable {
    public var available: Bool
    /// Month the database was published, e.g. "2026-11".
    public var date: String?
}

public struct UplinkStatus: Codable, Equatable, Identifiable {
    public var iface: String
    public var kind: String
    public var metric: Int
    public var carrier: Bool
    public var internet: String
    public var ipv4: String?
    public var gateway: String?
    public var ipv6: [String]?
    public var hasV6: Bool
    public var dns: [String]?
    public var portalUrl: String?
    public var captiveApi: String?
    public var mac: String?
    public var id: String { iface }

    enum CodingKeys: String, CodingKey {
        case iface = "if"
        case kind, metric, carrier, internet, ipv4, gateway, ipv6, hasV6, dns, portalUrl, captiveApi, mac
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
    public var id: String { radio }
}

public struct StationStatus: Codable, Equatable, Identifiable {
    public var radio: String
    public var iface: String
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

    enum CodingKeys: String, CodingKey {
        case iface = "if"
        case id, name, state, error, endpoint, handshake, rx, tx
    }
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
    public var present: Bool
    public var role: RadioRole
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
    public var signal: Int?
    public var up: UInt64
    public var down: UInt64
    public var name: String
    public var route: String
    public var paused: Bool
    public var ip: String?
    public var portal: Bool?
    public var id: String { mac }

    /// The name to show: the user's, else the device's own, else its MAC.
    public var displayName: String {
        if !name.isEmpty { return name }
        if let hostname, !hostname.isEmpty { return hostname }
        return mac.uppercased()
    }

    public var settings: DeviceSettings {
        DeviceSettings(mac: mac, name: name, route: route, paused: paused, ip: ip)
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

// MARK: - Typed calls

public extension RouterConnection {
    func hello() async throws -> HelloInfo { try await call("hello") }
    func pair(name: String) async throws { try await run("pair", ["name": name]) }
    func status() async throws -> RouterStatus { try await call("status.get") }
    func config() async throws -> RouterConfig { try await call("config.get") }

    /// Writes the configuration; returns the new revision.
    func setConfig(_ config: RouterConfig) async throws -> UInt64 {
        struct Params: Encodable { let config: RouterConfig }
        struct Result: Decodable { let rev: UInt64 }
        let r: Result = try await call("config.set", Params(config: config))
        return r.rev
    }

    func radios() async throws -> [RadioInfo] { try await call("radios.list") }
    func scanWifi() async throws -> [ScanResult] { try await call("wifi.scan") }
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

    func activity() async throws -> [ActivityEvent] { try await call("events.list") }
    func logs() async throws -> [String] { try await call("system.logs") }
    func setTime(_ date: Date = Date()) async throws {
        try await run("system.set_time", ["unix_ms": Int64(date.timeIntervalSince1970 * 1000)])
    }
    func reboot() async throws { try await run("system.reboot") }
    func powerOff() async throws { try await run("system.poweroff") }
    func factoryReset() async throws { try await run("system.factory_reset") }
}
