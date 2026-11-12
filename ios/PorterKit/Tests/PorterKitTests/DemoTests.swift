import XCTest
@testable import PorterKit

/// Everything the app shows in demo mode must decode with the real models.
final class DemoTests: XCTestCase {
    func testEveryCallDecodes() async throws {
        let router = DemoRouter.makeConnection()
        let hello = try await router.hello()
        XCTAssertTrue(hello.paired && hello.ready)

        let format = try await router.configFormat()
        XCTAssertEqual(format, RouterConfig.format)

        let status = try await router.status()
        XCTAssertEqual(status.internet, "online")
        XCTAssertEqual(status.wan.map(\.key), ["eth:end0", "wifi", "phone"])
        // A second wired port, for devices: the main network as it is
        // and the one for things under its VLAN ID, where a printer is
        // that the other networks can find.
        let sample = try await router.config()
        let trunk = try XCTUnwrap(sample.ethernet.last)
        XCTAssertEqual(trunk.role, .lan)
        XCTAssertEqual(trunk.network, RouterConfig.mainNetwork)
        XCTAssertEqual(trunk.taggedNetworks, ["things"])
        XCTAssertTrue(status.ports.contains { $0.name == trunk.name && !$0.phone })
        XCTAssertTrue(sample.wan.order.contains(RouterConfig.uplinkEthernet(trunk.name)))
        XCTAssertEqual(sample.ethernet.first?.taggedNetworks, [])
        let things = try XCTUnwrap(sample.network("things"))
        XCTAssertEqual(things.vlanID, 20)
        XCTAssertEqual(things.discovery, true)
        XCTAssertEqual(sample.networks.filter { $0.vlanID != 0 }.count, 1)
        XCTAssertEqual(status.lan.networks["things"], 1)
        XCTAssertEqual(status.offAir, ["guest"])
        // Looking at a change tells what it would do and changes nothing.
        var draft = try await router.config()
        draft.wifi[1].enabled = false
        let quiet = try await router.checkConfig(draft)
        XCTAssertNil(quiet.error)
        XCTAssertTrue(quiet.notes.isEmpty)
        let asIs = try await router.checkConfig(try await router.config())
        XCTAssertEqual(asIs.notes.count, 1)
        XCTAssertTrue(asIs.effects.isEmpty)
        XCTAssertEqual(status.ports.filter(\.phone).count, 1)
        XCTAssertEqual(status.vpns.first?.state, "up")
        XCTAssertTrue(status.geoip.available)

        // A wired source that signs in to its provider under a VLAN ID,
        // at home; on the road its cable is out.
        let wired = try XCTUnwrap(status.wan.first)
        XCTAssertEqual(wired.pppoe, "signing_in")
        XCTAssertNil(wired.prefix)
        XCTAssertEqual(status.lan.prefixes, [:])

        var config = try await router.config()
        XCTAssertEqual(config.ethernet.first?.vlan, 7)
        XCTAssertEqual(config.ethernet.first?.pppoe?.enabled, true)
        XCTAssertEqual(config.ethernet.first?.pppoe?.username, "porter@fibre.example")
        XCTAssertEqual(config.vpns.count, 2)
        XCTAssertEqual(config.policy.rules.count, 3)
        XCTAssertEqual(config.policy.rules[1].dst.countries, ["JP"])
        // Edits are kept, as with a real router.
        config.wifi[0].ssid = "Renamed"
        let rev = try await router.setConfig(config)
        XCTAssertEqual(rev, config.rev + 1)
        let stored = try await router.config()
        XCTAssertEqual(stored.wifi[0].ssid, "Renamed")
        XCTAssertEqual(stored.rev, rev)
        // A write built on the copy from before is refused, as it is by
        // a real router, and changes nothing.
        config.wifi[0].ssid = "Stale"
        do {
            _ = try await router.setConfig(config)
            XCTFail("stale configuration accepted")
        } catch let error as RouterError {
            XCTAssertEqual(error.code, RouterError.conflict)
        }
        let kept = try await router.config()
        XCTAssertEqual(kept, stored)

        let devices = try await router.devices()
        XCTAssertEqual(devices.count, 6)
        XCTAssertEqual(devices.filter(\.online).count, status.lan.clients)
        // Settings shown on devices agree with the configuration.
        for setting in stored.devices {
            XCTAssertTrue(devices.contains { $0.mac == setting.mac }, "configured device \(setting.mac) missing from the list")
        }
        let radios = try await router.radios()
        XCTAssertEqual(radios.map(\.id), stored.radios.map(\.id))
        let scan = try await router.scanWifi()
        let activity = try await router.activity()
        let logs = try await router.logs()
        let phones = try await router.pairedPhones()
        let queries = try await router.queries(mac: devices[0].mac)
        XCTAssertFalse(scan.isEmpty || activity.isEmpty || logs.isEmpty || queries.isEmpty)
        XCTAssertEqual(phones.filter(\.isSelf).count, 1)

        let stats = try await router.trafficStats()
        XCTAssertEqual(stats.minutes.count, 60)
        XCTAssertEqual(stats.hours.count, 72)
        XCTAssertTrue(stats.minutes.contains { $0.down > 0 })
        XCTAssertEqual(stats.minutes.map(\.t), stats.minutes.map(\.t).sorted())
        let top = try await router.topTalkers()
        XCTAssertTrue(top.allSatisfy { talker in devices.contains { $0.mac == talker.mac } })
        let flows = try await router.flows()
        XCTAssertTrue(flows.contains { $0.active } && flows.contains { !$0.active })
        XCTAssertTrue(flows.allSatisfy { flow in devices.contains { $0.mac == flow.mac } })
        let own = try await router.flows(mac: devices[0].mac)
        XCTAssertTrue(!own.isEmpty && own.allSatisfy { $0.mac == devices[0].mac })
        let speed = try await router.runSpeedTest()
        let history = try await router.speedTestHistory()
        XCTAssertNil(speed.error)
        XCTAssertEqual(history.count, 3)

        // Coming in from outside: a WireGuard server with two devices of
        // which one is connected, a forwarded port, a name kept up to
        // date. All of it survives being read and written back.
        let fresh = try await router.config()
        let inbound = try XCTUnwrap(fresh.inbound)
        let reach = try XCTUnwrap(status.inbound)
        XCTAssertTrue(inbound.server.enabled)
        XCTAssertEqual(inbound.server.peers.map(\.id), reach.server.peers.map(\.id))
        XCTAssertEqual(reach.server.peers.filter(\.connected).count, 1)
        XCTAssertEqual(reach.reach, "nat")
        XCTAssertEqual(reach.publicAddress, "203.0.113.150")
        XCTAssertNil(reach.publicAddress6)
        XCTAssertEqual(reach.host, inbound.ddns.hostname)
        XCTAssertEqual(reach.ddns.state, "ok")
        XCTAssertEqual(inbound.forwards.map(\.id), reach.forwards.map(\.id))
        let target = try XCTUnwrap(inbound.forwards.first)
        XCTAssertTrue(devices.contains { $0.mac == target.device && $0.ip == reach.forwards.first?.to })
        XCTAssertTrue(status.control?.addrs.contains("10.89.0.1") ?? false)
        let rewritten = try JSONSerialization.jsonObject(with: RouterConnection.makeEncoder().encode(fresh)) as? [String: Any]
        let original = try JSONSerialization.jsonObject(with: Data(DemoRouter.configJSON.utf8)) as? [String: Any]
        XCTAssertEqual(rewritten?["inbound"] as? NSDictionary, original?["inbound"] as? NSDictionary, "the app would change what comes in by writing the configuration back")
        // Adding a device gives its configuration, once, and the device
        // is then one of the router's.
        let added = try await router.addHomeDevice(name: "Tablet", full: false)
        XCTAssertTrue(added.conf.contains("[Interface]") && added.conf.contains("PrivateKey = ") && added.conf.contains("Endpoint = \(added.host):51820"))
        XCTAssertFalse(added.conf.contains("0.0.0.0/0"))
        let after = try await router.config()
        XCTAssertEqual(after.inbound?.server.peers.last?.id, added.id)
        XCTAssertEqual(after.inbound?.server.peers.last?.name, "Tablet")
        let seen = try await router.status()
        XCTAssertEqual(seen.inbound?.server.peers.count, 3)
        try await router.refreshInbound()
        // A router that has none of this yet is read all the same.
        var older = try XCTUnwrap(original)
        older["inbound"] = nil
        let decoded = try RouterConnection.makeDecoder().decode(RouterConfig.self, from: JSONSerialization.data(withJSONObject: older))
        XCTAssertNil(decoded.inbound)

        // Calls with nothing to return succeed.
        try await router.setTime()
        try await router.openPairing()
        try await router.setDevice(DeviceSettings(mac: devices[0].mac, name: "Renamed"))
        _ = try await router.generateKey()
        _ = try await router.parseWireGuard(conf: "ignored")
        // Reading a pasted VPN configuration does not depend on a VPN
        // profile being there.
        var bare = try await router.config()
        bare.vpns = []
        _ = try await router.setConfig(bare)
        let parsed = try await router.parseWireGuard(conf: "ignored")
        XCTAssertEqual(parsed.peers.count, 1)
    }

    /// Holding an internet source to a speed, in demo mode: the sample
    /// router is on a hotel's network and holds that to speeds measured
    /// there; the wired port's are kept for when it is back home.
    func testSmoothingInDemoMode() async throws {
        let router = DemoRouter.makeConnection()
        let config = try await router.config()
        let status = try await router.status()
        let wired = RouterConfig.uplinkEthernet("end0")
        XCTAssertEqual(config.shaping(source: wired), ShapingConfig(enabled: true, downKbps: 237_500, upKbps: 38_000))
        XCTAssertEqual(config.shaping(source: RouterConfig.uplinkPhone), ShapingConfig())
        XCTAssertEqual(config.shaping(source: RouterConfig.uplinkWifi, ssid: "Airport Free WiFi"), ShapingConfig())
        XCTAssertEqual(config.shaping(source: RouterConfig.uplinkWifi, ssid: "Nowhere"), ShapingConfig())
        let hotel = config.shaping(source: RouterConfig.uplinkWifi, ssid: DemoRouter.joinedNetwork)
        XCTAssertTrue(hotel.enabled)

        let shaped = try XCTUnwrap(status.shaping)
        XCTAssertEqual(shaped.map(\.key), [wired, RouterConfig.uplinkWifi])
        XCTAssertEqual(shaped.map(\.state), ["waiting", "active"])
        // Each is the status of a source the router lists, by interface.
        XCTAssertTrue(shaped.allSatisfy { entry in status.wan.contains { $0.iface == entry.iface && $0.key == entry.key } })
        XCTAssertEqual(shaped[1].down.kbps, hotel.downKbps)
        XCTAssertEqual(shaped[1].up.kbps, hotel.upKbps)
        XCTAssertEqual(shaped[1].down.slowed, 2046)
        XCTAssertEqual(shaped[0].down.slowed, 0)

        // Measured through the source, the speeds to hold it to are a
        // little below what was measured: the ones the sample has.
        let measured = try await router.runSpeedTest(via: RouterConfig.speedTestPath(source: RouterConfig.uplinkWifi))
        XCTAssertEqual(measured.via, "source:wifi")
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: measured.downMbps), hotel.downKbps)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: measured.upMbps), hotel.upKbps)
        let plain = try await router.runSpeedTest()
        XCTAssertEqual(plain.via, "direct")

        // Set for the phone, it is kept, and the phone that is plugged
        // in is held to it; switched off for the hotel, that goes.
        var next = config
        next.setShaping(ShapingConfig(enabled: true, downKbps: 20_000, upKbps: 4_000), source: RouterConfig.uplinkPhone)
        next.setShaping(ShapingConfig(enabled: false, downKbps: hotel.downKbps, upKbps: hotel.upKbps), source: RouterConfig.uplinkWifi, ssid: DemoRouter.joinedNetwork)
        next.setShaping(ShapingConfig(enabled: true, downKbps: 1, upKbps: 1), source: RouterConfig.uplinkWifi, ssid: "Nowhere")
        _ = try await router.setConfig(next)
        let stored = try await router.config()
        XCTAssertEqual(stored.wan.phone?.shaping, ShapingConfig(enabled: true, downKbps: 20_000, upKbps: 4_000))
        XCTAssertEqual(stored.wan.networks.map(\.ssid), config.wan.networks.map(\.ssid))
        let after = try await router.status()
        XCTAssertEqual(after.shaping?.map(\.key), [wired, RouterConfig.uplinkPhone])
        XCTAssertEqual(after.shaping?.last?.iface, DemoRouter.phoneInterface)

        // The app writes the whole document back: what it read of this
        // has to come out as it went in.
        let rewritten = try JSONSerialization.jsonObject(with: RouterConnection.makeEncoder().encode(config)) as? [String: Any]
        let original = try JSONSerialization.jsonObject(with: Data(DemoRouter.configJSON.utf8)) as? [String: Any]
        XCTAssertEqual(rewritten?["ethernet"] as? NSArray, original?["ethernet"] as? NSArray)
        XCTAssertEqual(rewritten?["wan"] as? NSDictionary, original?["wan"] as? NSDictionary)
    }

    /// The setting as routers of different ages say it, and the figures
    /// the app makes of a measurement and of what is typed.
    func testShapingModel() throws {
        let decoder = RouterConnection.makeDecoder()
        let encoder = RouterConnection.makeEncoder()
        // A router that knows nothing of it.
        let port = try decoder.decode(EthernetPort.self, from: Data(#"{"name":"eth0","role":"wan","network":"lan"}"#.utf8))
        XCTAssertNil(port.shaping)
        let saved = try decoder.decode(SavedNetwork.self, from: Data(#"{"ssid":"Hotel","psk":"","hidden":false,"priority":1}"#.utf8))
        XCTAssertNil(saved.shaping)
        let wan = try decoder.decode(WANConfig.self, from: Data(#"{"networks":[],"wifi_mac":{"mode":"hardware"},"eth_mac":{"mode":"hardware"},"order":["wifi"],"probe":{"url":"http://example.com/","expect":"ok"}}"#.utf8))
        XCTAssertNil(wan.phone)
        // Joining a network says nothing about its speeds, so that the
        // router keeps the ones it has.
        let join = String(decoding: try encoder.encode(SavedNetwork(ssid: "Hotel", psk: "")), as: UTF8.self)
        XCTAssertFalse(join.contains("shaping"), join)

        // One that does: the names on the wire, there and back.
        let held = try decoder.decode(EthernetPort.self, from: Data(#"{"name":"eth0","role":"wan","network":"lan","shaping":{"enabled":true,"down_kbps":95000,"up_kbps":19000}}"#.utf8))
        XCTAssertEqual(held.shaping, ShapingConfig(enabled: true, downKbps: 95_000, upKbps: 19_000))
        let written = String(decoding: try encoder.encode(held), as: UTF8.self)
        XCTAssertTrue(written.contains("\"shaping\":{") && written.contains("\"down_kbps\":95000") && written.contains("\"up_kbps\":19000"), written)
        XCTAssertEqual(try decoder.decode(EthernetPort.self, from: Data(written.utf8)), held)
        let phone = String(decoding: try encoder.encode(PhoneSourceConfig(shaping: ShapingConfig(enabled: true, downKbps: 20_000, upKbps: 4_000))), as: UTF8.self)
        XCTAssertTrue(phone.contains("\"shaping\":{") && phone.contains("\"down_kbps\":20000"), phone)
        let entry = try decoder.decode(ShapingStatus.self, from: Data(#"{"key":"eth:eth0","if":"ppp-eth0","state":"error","down":{"kbps":95000,"dropped":7,"marked":5},"up":{"kbps":19000,"dropped":0,"marked":1},"error":"Error: Specified qdisc kind is unknown."}"#.utf8))
        XCTAssertEqual(entry.iface, "ppp-eth0")
        XCTAssertEqual(entry.down.slowed, 12)
        XCTAssertEqual(entry.up.slowed, 1)
        XCTAssertNotNil(entry.error)

        // 95 % of what was measured, rounded down to a figure that reads
        // well, and never outside what the router takes.
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: 93.7), 89_000)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: 41.2), 39_100)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: 9.3), 8_830)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: 250), 237_500)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: 0.05), ShapingConfig.slowestKbps)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: 50_000), ShapingConfig.fastestKbps)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: 0), 0)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: -3), 0)
        XCTAssertEqual(ShapingConfig.speed(measuredMbps: .nan), 0)
        for mbps in [0.3, 1.0, 7.77, 12.5, 99.99, 940.0] {
            let kbps = ShapingConfig.speed(measuredMbps: mbps)
            XCTAssertTrue(ShapingConfig.accepts(kbps: kbps))
            XCTAssertLessThanOrEqual(Double(kbps), mbps * 950 + 0.001)
            XCTAssertGreaterThan(Double(kbps), mbps * 900)
        }

        // Shown and typed in Mbit/s.
        XCTAssertEqual(ShapingConfig.mbpsText(kbps: 89_000), "89")
        XCTAssertEqual(ShapingConfig.mbpsText(kbps: 39_100), "39.1")
        XCTAssertEqual(ShapingConfig.mbpsText(kbps: 8_830), "8.83")
        XCTAssertEqual(ShapingConfig.mbpsText(kbps: 500), "0.5")
        XCTAssertEqual(ShapingConfig.mbpsText(kbps: 10_000_000), "10000")
        XCTAssertEqual(ShapingConfig.mbpsText(kbps: 0), "")
        XCTAssertEqual(ShapingConfig.kbps(mbpsText: "39.1"), 39_100)
        XCTAssertEqual(ShapingConfig.kbps(mbpsText: " 39,1 "), 39_100)
        XCTAssertEqual(ShapingConfig.kbps(mbpsText: "250"), 250_000)
        XCTAssertEqual(ShapingConfig.kbps(mbpsText: "0.5"), 500)
        XCTAssertEqual(ShapingConfig.kbps(mbpsText: ""), 0)
        XCTAssertNil(ShapingConfig.kbps(mbpsText: "fast"))
        XCTAssertNil(ShapingConfig.kbps(mbpsText: "-5"))
        XCTAssertNil(ShapingConfig.kbps(mbpsText: "1e12"))
        for kbps in [100, 990, 8_830, 39_100, 237_500, 10_000_000] {
            XCTAssertEqual(ShapingConfig.kbps(mbpsText: ShapingConfig.mbpsText(kbps: kbps)), kbps)
        }
        XCTAssertFalse(ShapingConfig.accepts(kbps: 99))
        XCTAssertFalse(ShapingConfig.accepts(kbps: 10_000_001))
    }
}
