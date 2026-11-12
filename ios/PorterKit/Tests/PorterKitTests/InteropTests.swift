import XCTest
@testable import PorterKit
#if canImport(Glibc)
import Glibc
#elseif canImport(Darwin)
import Darwin
#endif

/// The router's local control socket: packets with a 2-byte length prefix.
final class UnixSocketTransport: PacketTransport {
    private let fd: Int32
    private let queue = DispatchQueue(label: "porter.unix-socket")
    private let writeQueue = DispatchQueue(label: "porter.unix-socket.write")

    init(path: String) throws {
        #if canImport(Glibc)
        fd = socket(AF_UNIX, Int32(SOCK_STREAM.rawValue), 0)
        #else
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        #endif
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        precondition(bytes.count < MemoryLayout.size(ofValue: addr.sun_path))
        withUnsafeMutableBytes(of: &addr.sun_path) { $0.copyBytes(from: bytes) }
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard rc == 0 else { throw SessionError.closed }
    }

    private func readExactly(_ count: Int) throws -> Data {
        var out = Data(count: count)
        var done = 0
        while done < count {
            let n = out.withUnsafeMutableBytes { read(fd, $0.baseAddress! + done, count - done) }
            guard n > 0 else { throw SessionError.closed }
            done += n
        }
        return out
    }

    func send(_ packet: Data) async throws {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
            writeQueue.async {
                var buf = Data([UInt8(packet.count >> 8), UInt8(packet.count & 0xff)])
                buf.append(packet)
                let n = buf.withUnsafeBytes { write(self.fd, $0.baseAddress!, buf.count) }
                if n == buf.count { c.resume() } else { c.resume(throwing: SessionError.closed) }
            }
        }
    }

    func receive() async throws -> Data {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data, Error>) in
            queue.async {
                do {
                    let header = try self.readExactly(2)
                    c.resume(returning: try self.readExactly(Int(header[0]) << 8 | Int(header[1])))
                } catch {
                    c.resume(throwing: error)
                }
            }
        }
    }

    func close() { _ = shutdown(fd, Int32(2)) }
}

/// Exercises the whole Swift stack against a running porterd. Skipped
/// unless PORTER_CONTROL_SOCKET names its control socket.
final class InteropTests: XCTestCase {
    func testAgainstRealRouter() async throws {
        guard let path = ProcessInfo.processInfo.environment["PORTER_CONTROL_SOCKET"] else {
            throw XCTSkip("PORTER_CONTROL_SOCKET not set")
        }
        let router = try await RouterConnection.connect(
            transport: try UnixSocketTransport(path: path), identity: Identity(), expectedRouterKey: nil)
        XCTAssertEqual(router.routerKey.count, 32)

        let hello = try await router.hello()
        XCTAssertTrue(hello.paired) // the local socket is trusted
        try await router.setTime()
        try await router.ping()
        // The router says where it listens on its own networks.
        XCTAssertEqual(hello.control?.port, 7678)

        // Every document the app reads must decode.
        let status = try await router.status()
        XCTAssertEqual(status.name, hello.name)
        XCTAssertEqual(status.control, hello.control)
        XCTAssertEqual(status.control?.addrs.first, "10.83.0.1")
        _ = try await router.radios()
        _ = try await router.devices()
        _ = try await router.activity()
        _ = try await router.logs()
        _ = try await router.pairedPhones()
        let history = try await router.trafficStats()
        XCTAssertEqual(history.minutes.count, 60)
        XCTAssertEqual(history.hours.count, 72)
        _ = try await router.topTalkers()
        _ = try await router.flows()
        _ = try await router.speedTestHistory()

        // Round-trip the configuration through the Swift models: the
        // router validates the whole document on the way back in.
        var config = try await router.config()
        let key = try await router.generateKey()
        let wg = try await router.parseWireGuard(conf: """
            [Interface]
            PrivateKey = \(key.privateKey)
            Address = 10.9.0.2/32, fd00:9::2/128
            DNS = 10.9.0.1

            [Peer]
            PublicKey = \(key.publicKey)
            AllowedIPs = 0.0.0.0/0, ::/0
            Endpoint = vpn.example.net:51820
            """)
        XCTAssertEqual(wg.peers.first?.allowedIps, ["0.0.0.0/0", "::/0"])
        config.system.name = "Swift Test"
        config.system.country = "AU"
        config.wifi[0].ssid = "Porter Tést"
        config.wifi[0].passphrase = "correct horse battery"
        config.wifi[0].security = .wpa2wpa3
        // A second network with a Wi-Fi name of its own; the router picks
        // its addresses.
        config.networks.append(NetworkConfig(id: "guest", name: "Guest", isolated: true, route: RouteAction.direct.raw))
        config.wifi.append(WifiNetworkConfig(id: "guest", ssid: "Porter Guests", passphrase: "welcome aboard", network: "guest"))
        config.wan.order = [RouterConfig.uplinkPhone, RouterConfig.uplinkWifi]
        config.dns.upstreams = ["tls://9.9.9.9#dns.quad9.net"]
        config.dns.blocked = ["Ads.Example.com"]
        config.vpns = [VPNProfile(id: "home", name: "Home", enabled: false, killSwitch: true, wireguard: wg)]
        config.devices = [DeviceSettings(mac: "02:AA:00:00:00:01", name: "Laptop", route: RouteAction.vpn("home").raw)]
        config.policy.rules = [PolicyRule(
            id: "r1", name: "Streaming direct",
            src: RuleSource(devices: ["02:aa:00:00:00:01"]),
            dst: RuleDestination(domains: ["*.video.example"], countries: ["jp"], proto: "tcp", ports: ["443", "8000-8100"]),
            action: RouteAction.direct.raw),
            PolicyRule(id: "r2", name: "Guests", src: RuleSource(networks: ["guest"]), dst: RuleDestination(ports: ["25"]), action: RouteAction.block.raw)]
        config.wan.wifiMac = MACPolicy(mode: .random, address: nil)
        config.speedtest.uploadUrl = "https://speed.example.net/upload"
        XCTAssertFalse(config.dns.adblock.enabled)
        XCTAssertEqual(config.dns.adblock.lists.count, 1) // a default list is ready
        config.dns.adblock.allow = ["Links.Example.com"]
        // Asking first what the change would do changes nothing.
        let check = try await router.checkConfig(config)
        XCTAssertNil(check.error)
        let untouched = try await router.config()
        XCTAssertEqual(untouched.rev, config.rev)
        XCTAssertEqual(untouched.networks.count, 1)
        var overlapping = config
        overlapping.networks[1].address = "10.83.0.129/25"
        let refused = try await router.checkConfig(overlapping)
        XCTAssertNotNil(refused.error)

        // The first wired port: the internet under a VLAN ID, with a
        // PPPoE sign-in. A router answers with both set out for every
        // port, whether it uses them or not.
        XCTAssertEqual(config.ethernet.first?.vlan, 0)
        XCTAssertEqual(config.ethernet.first?.pppoe, PPPoESetup())
        var tagged = config
        tagged.ethernet[0].vlan = 4095
        let outOfRange = try await router.checkConfig(tagged)
        XCTAssertNotNil(outOfRange.error)
        config.ethernet[0].vlan = 35
        config.ethernet[0].pppoe = PPPoESetup(enabled: true, username: "user@provider.example", password: "pa\"ss \\word #1")

        // Networks on one cable, and finding devices across networks:
        // a router answers with a list of tagged networks for every
        // port, and holds a network's VLAN ID and its switch.
        XCTAssertEqual(config.ethernet.first?.tagged, [])
        XCTAssertEqual(config.networks.first?.vlan, 0)
        XCTAssertEqual(config.networks.first?.discovery, false)
        config.networks[1].vlan = 20
        config.networks[1].discovery = true
        var twice = config
        twice.networks[0].vlan = 20
        let sameID = try await router.checkConfig(twice)
        XCTAssertEqual(sameID.error, "Main and Guest have the same VLAN ID, 20. Each network needs one of its own.")
        // Only a port for devices carries networks: on the first port,
        // which is an internet source, the router does not keep them.
        var source = config
        source.ethernet[0].role = .lan
        source.ethernet[0].network = ""
        source.ethernet[0].tagged = ["guest"]
        let trunk = try await router.checkConfig(source)
        XCTAssertNil(trunk.error)
        source.ethernet[0].tagged = ["lan"]
        let untaggable = try await router.checkConfig(source)
        XCTAssertEqual(untaggable.error, "Main has no VLAN ID, so the port \(config.ethernet[0].name) cannot carry it tagged. Give the network a VLAN ID first.")
        config.ethernet[0].tagged = ["guest"]

        // Holding internet sources to a speed: a router answers with the
        // setting for every port and for the phone, switched off, and
        // holds it there and for each saved Wi-Fi network.
        XCTAssertEqual(config.ethernet.first?.shaping, ShapingConfig())
        XCTAssertEqual(config.wan.phone, PhoneSourceConfig())
        XCTAssertEqual(status.shaping, [])
        let wiredKey = RouterConfig.uplinkEthernet(config.ethernet[0].name)
        var halfSet = config
        halfSet.setShaping(ShapingConfig(enabled: true, downKbps: 95_000), source: wiredKey)
        let noUpload = try await router.checkConfig(halfSet)
        XCTAssertEqual(noUpload.error, "Smoothing out heavy use needs both speeds of the internet source, download and upload.")
        halfSet.setShaping(ShapingConfig(enabled: true, downKbps: 95_000, upKbps: ShapingConfig.slowestKbps - 1), source: wiredKey)
        let tooSlow = try await router.checkConfig(halfSet)
        XCTAssertNotNil(tooSlow.error)
        config.setShaping(ShapingConfig(enabled: true, downKbps: 95_000, upKbps: 19_000), source: wiredKey)
        config.setShaping(ShapingConfig(enabled: false, downKbps: 20_000, upKbps: 4_000), source: RouterConfig.uplinkPhone)
        config.wan.networks = [SavedNetwork(ssid: "Hotel", psk: ""), SavedNetwork(ssid: "Home", psk: "password1")]
        config.setShaping(ShapingConfig(enabled: true, downKbps: 8_000, upKbps: 2_500), source: RouterConfig.uplinkWifi, ssid: "Hotel")

        // What comes in from outside. A router answers with its WireGuard
        // server complete (key, ranges, port) and switched off, and holds
        // devices for it, forwarded ports and a name to keep up to date.
        let inbound = try XCTUnwrap(config.inbound)
        XCTAssertFalse(inbound.server.enabled)
        XCTAssertEqual(inbound.server.port, 51820)
        XCTAssertEqual(inbound.server.address, "10.89.0.1/24")
        XCTAssertFalse(inbound.server.privateKey.isEmpty || inbound.server.ulaPrefix.isEmpty || inbound.checkUrl.isEmpty)
        XCTAssertEqual(inbound.forwards, [])
        XCTAssertEqual(inbound.ddns, DynamicDNSConfig())
        XCTAssertEqual(status.inbound?.server.state, "off")
        XCTAssertEqual(status.inbound?.forwards, [])
        XCTAssertEqual(status.inbound?.ddns.state, "off")
        config.inbound?.server.enabled = true
        config.inbound?.server.peers = [HomeDevice(id: "p1", name: "Phone", publicKey: key.publicKey, address: "10.89.0.2", enabled: true, full: true)]
        config.devices.append(DeviceSettings(mac: "02:AA:00:00:00:03", name: "NAS", ip: "10.83.0.10"))
        config.inbound?.forwards = [PortForward(id: "web", name: "Web", ports: "8443", device: "02:AA:00:00:00:03", to: 443, from: ["203.0.113.7"])]
        config.inbound?.ddns = DynamicDNSConfig(provider: "duckdns", hostname: "Porter-Interop", password: "token")
        // The port devices reach home through cannot be forwarded as well.
        var clash = config
        clash.inbound?.forwards[0].proto = "both"
        clash.inbound?.forwards[0].ports = "51820"
        clash.inbound?.forwards[0].to = 0
        let taken = try await router.checkConfig(clash)
        XCTAssertTrue(taken.error?.contains("reach home through") ?? false, taken.error ?? "accepted")

        // The write names itself; sent again, as it is when its answer
        // was lost, it is answered again and made once.
        XCTAssertEqual(config.system.bluetoothOnly, false)
        config.system.bluetoothOnly = true
        let rev = try await router.setConfig(config, id: "interop-1")
        XCTAssertGreaterThan(rev, config.rev)
        let again = try await router.setConfig(config, id: "interop-1")
        XCTAssertEqual(again, rev)

        let stored = try await router.config()
        XCTAssertEqual(stored.rev, rev)
        XCTAssertEqual(stored.system.bluetoothOnly, true)
        XCTAssertEqual(stored.ethernet.first?.vlan, 35)
        XCTAssertEqual(stored.ethernet.first?.pppoe, PPPoESetup(enabled: true, username: "user@provider.example", password: "pa\"ss \\word #1"))
        XCTAssertEqual(stored.system.name, "Swift Test")
        XCTAssertEqual(stored.wifi.map(\.ssid), ["Porter Tést", "Porter Guests"])
        XCTAssertEqual(stored.networks.map(\.id), ["lan", "guest"])
        XCTAssertEqual(stored.networks[1].vlan, 20)
        XCTAssertEqual(stored.networks[1].discovery, true)
        XCTAssertEqual(stored.ethernet.first?.tagged, [])
        XCTAssertEqual(stored.networks[1].address, "10.84.0.1/24") // chosen by the router
        XCTAssertFalse(stored.networks[1].ulaPrefix.isEmpty)
        XCTAssertEqual(stored.policy.rules.last?.src.networks, ["guest"])
        // The order asked for, completed by the router with its wired ports.
        XCTAssertEqual(Array(stored.wan.order.prefix(1)), [RouterConfig.uplinkPhone])
        XCTAssertTrue(stored.wan.order.contains(RouterConfig.uplinkWifi))
        XCTAssertEqual(stored.dns.blocked, ["ads.example.com"]) // normalised by the router
        XCTAssertEqual(stored.devices.first?.mac, "02:aa:00:00:00:01")
        XCTAssertEqual(stored.policy.rules.first?.dst.domains, ["video.example"])
        XCTAssertEqual(stored.policy.rules.first?.dst.countries, ["JP"])
        XCTAssertTrue(stored.speedtest.downloadUrl.hasPrefix("https://"))
        XCTAssertEqual(stored.dns.adblock.allow, ["links.example.com"])
        XCTAssertFalse(status.adblock.enabled)
        try await router.checkForUpdates()
        XCTAssertEqual(stored.vpns.first?.wireguard, wg)
        XCTAssertEqual(stored.wan.wifiMac.mode, .random)
        XCTAssertNotNil(stored.wan.wifiMac.address) // the router picked one

        XCTAssertEqual(stored.shaping(source: wiredKey), ShapingConfig(enabled: true, downKbps: 95_000, upKbps: 19_000))
        XCTAssertEqual(stored.shaping(source: RouterConfig.uplinkPhone), ShapingConfig(enabled: false, downKbps: 20_000, upKbps: 4_000))
        XCTAssertEqual(stored.shaping(source: RouterConfig.uplinkWifi, ssid: "Hotel"), ShapingConfig(enabled: true, downKbps: 8_000, upKbps: 2_500))
        XCTAssertEqual(stored.shaping(source: RouterConfig.uplinkWifi, ssid: "Home"), ShapingConfig())
        // A source that is not connected cannot be measured.
        do {
            _ = try await router.runSpeedTest(via: RouterConfig.speedTestPath(source: RouterConfig.uplinkPhone))
            XCTFail("the speed of a phone that is not plugged in was measured")
        } catch let error as RouterError {
            XCTAssertEqual(error.msg, "That internet source is not connected.")
        }

        XCTAssertEqual(stored.inbound?.server.enabled, true)
        XCTAssertEqual(stored.inbound?.server.peers, config.inbound?.server.peers)
        XCTAssertEqual(stored.inbound?.forwards.first?.device, "02:aa:00:00:00:03") // normalised by the router
        XCTAssertEqual(stored.inbound?.forwards.first?.from, ["203.0.113.7/32"])
        XCTAssertEqual(stored.inbound?.forwards.first?.to, 443)
        XCTAssertEqual(stored.inbound?.ddns.hostname, "porter-interop.duckdns.org")
        let after = try await router.status()
        let reach = try XCTUnwrap(after.inbound)
        XCTAssertEqual(reach.server.peers.map(\.id), ["p1"])
        XCTAssertFalse(reach.server.publicKey.isEmpty)
        XCTAssertEqual(reach.forwards.first?.to, "10.83.0.10")
        XCTAssertEqual(reach.forwards.first?.to6, [])
        try await router.refreshInbound()

        // A stale write is refused, an invalid one explained.
        do {
            _ = try await router.setConfig(config)
            XCTFail("stale configuration accepted")
        } catch let error as RouterError {
            XCTAssertEqual(error.code, RouterError.conflict)
        }
        var invalid = stored
        invalid.wifi[0].passphrase = "short"
        do {
            _ = try await router.setConfig(invalid)
            XCTFail("invalid configuration accepted")
        } catch let error as RouterError {
            XCTAssertEqual(error.code, "invalid")
            XCTAssertTrue(error.msg.contains("password"))
        }

        // Joining a saved network again keeps the speeds set for it.
        try await router.connectWifi(SavedNetwork(ssid: "Hotel", psk: "new password"))
        let rejoined = try await router.config()
        XCTAssertEqual(rejoined.shaping(source: RouterConfig.uplinkWifi, ssid: "Hotel"), ShapingConfig(enabled: true, downKbps: 8_000, upKbps: 2_500))
        XCTAssertEqual(rejoined.wan.networks.first(where: { $0.ssid == "Hotel" })?.psk, "new password")

        // Device settings and events.
        try await router.setDevice(DeviceSettings(mac: "02:aa:00:00:00:02", name: "Phone", paused: true, noAdblock: true))
        let devices = try await router.devices()
        XCTAssertTrue(devices.contains { $0.mac == "02:aa:00:00:00:02" && $0.paused && $0.noAdblock && $0.displayName == "Phone" })

        var sawStatus = false
        let deadline = Date().addingTimeInterval(10)
        try await router.openPairing()
        for await event in router.events {
            if case .status(let s) = event, s.pairing { sawStatus = true; break }
            if Date() > deadline { break }
        }
        XCTAssertTrue(sawStatus, "no status event after opening pairing")

        // Many concurrent calls must each get their own answer.
        try await withThrowingTaskGroup(of: String.self) { group in
            for _ in 0..<20 { group.addTask { try await router.status().name } }
            for try await name in group { XCTAssertEqual(name, "Swift Test") }
        }
        await router.close()
    }
}
