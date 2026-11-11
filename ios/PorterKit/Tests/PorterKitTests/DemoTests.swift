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

        var config = try await router.config()
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

        let devices = try await router.devices()
        XCTAssertEqual(devices.count, 5)
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

        // Calls with nothing to return succeed.
        try await router.setTime()
        try await router.openPairing()
        try await router.setDevice(DeviceSettings(mac: devices[0].mac, name: "Renamed"))
        _ = try await router.generateKey()
        _ = try await router.parseWireGuard(conf: "ignored")
    }
}
