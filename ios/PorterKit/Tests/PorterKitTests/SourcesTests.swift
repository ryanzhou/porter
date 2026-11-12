import XCTest
@testable import PorterKit

/// Routes through one internet source, and sources with a data limit.
final class SourcesTests: XCTestCase {
    private func sample() throws -> RouterConfig {
        try RouterConnection.makeDecoder().decode(RouterConfig.self, from: Data(DemoRouter.configJSON.utf8))
    }

    /// A Route that names a source reads and writes as the router has it,
    /// and a picker shows the source however it is used.
    func testRouteActions() {
        XCTAssertEqual(RouteAction("source:phone"), .source("phone", only: false))
        XCTAssertEqual(RouteAction("only:phone"), .source("phone", only: true))
        XCTAssertEqual(RouteAction("source:eth:end0"), .source("eth:end0", only: false))
        XCTAssertEqual(RouteAction("only:wifi"), .source("wifi", only: true))
        XCTAssertEqual(RouteAction("vpn:home"), .vpn("home"))
        XCTAssertEqual(RouteAction("direct"), .direct)
        for raw in ["", "source:", "only:", "sources:phone", "adblock", "phone"] {
            XCTAssertNil(RouteAction(raw), raw)
        }
        for raw in ["direct", "block", "vpn:home", "source:phone", "only:phone", "source:eth:end0", "only:eth:end0", "source:wifi"] {
            XCTAssertEqual(RouteAction(raw)?.raw, raw)
        }
        XCTAssertEqual(RouteAction("only:phone")?.sourceKey, "phone")
        XCTAssertNil(RouteAction("vpn:home")?.sourceKey)
        XCTAssertEqual(RouteAction("only:phone")?.onlySource, true)
        XCTAssertEqual(RouteAction("source:phone")?.onlySource, false)
        XCTAssertEqual(RouteAction("direct")?.onlySource, false)
        XCTAssertEqual(RouteAction("source:phone")?.onlySource(true).raw, "only:phone")
        XCTAssertEqual(RouteAction("only:phone")?.onlySource(false).raw, "source:phone")
        XCTAssertEqual(RouteAction("vpn:home")?.onlySource(true).raw, "vpn:home")

        // One entry in the menu for a source, whichever way it is used.
        XCTAssertEqual(RouteAction.choice(for: "only:phone"), "source:phone")
        XCTAssertEqual(RouteAction.choice(for: "source:phone"), "source:phone")
        XCTAssertEqual(RouteAction.choice(for: "vpn:home"), "vpn:home")
        XCTAssertEqual(RouteAction.choice(for: ""), "")
        // Picking it again keeps the switch; picking another source, or
        // anything else, starts without it.
        XCTAssertEqual(RouteAction.picked("source:phone", current: "only:phone"), "only:phone")
        XCTAssertEqual(RouteAction.picked("source:wifi", current: "only:phone"), "source:wifi")
        XCTAssertEqual(RouteAction.picked("source:phone", current: "direct"), "source:phone")
        XCTAssertEqual(RouteAction.picked("direct", current: "only:phone"), "direct")
        XCTAssertEqual(RouteAction.picked("", current: "only:phone"), "")
    }

    /// The amounts a person types and reads.
    func testAmounts() {
        XCTAssertEqual(DataLimit.mb(gbText: "5"), 5000)
        XCTAssertEqual(DataLimit.mb(gbText: "1.5"), 1500)
        XCTAssertEqual(DataLimit.mb(gbText: " 0,5 "), 500)
        XCTAssertEqual(DataLimit.mb(gbText: "0.001"), 1)
        XCTAssertNil(DataLimit.mb(gbText: "0"))
        XCTAssertNil(DataLimit.mb(gbText: "-2"))
        XCTAssertNil(DataLimit.mb(gbText: "lots"))
        XCTAssertNil(DataLimit.mb(gbText: ""))
        XCTAssertEqual(DataLimit.gbText(mb: 5000), "5")
        XCTAssertEqual(DataLimit.gbText(mb: 1500), "1.5")
        XCTAssertEqual(DataLimit.gbText(mb: 1), "0.001")
        XCTAssertEqual(DataLimit.gbText(mb: 250), "0.25")
        for mb in [1, 250, 500, 1500, 5000, 200_000] {
            XCTAssertEqual(DataLimit.mb(gbText: DataLimit.gbText(mb: mb)), mb)
        }
        XCTAssertTrue(DataLimit.accepts(mb: 1))
        XCTAssertTrue(DataLimit.accepts(mb: DataLimit.largestMB))
        XCTAssertFalse(DataLimit.accepts(mb: 0))
        XCTAssertFalse(DataLimit.accepts(mb: DataLimit.largestMB + 1))
        XCTAssertEqual(DataLimit.amountText(bytes: 0), "0 MB")
        XCTAssertEqual(DataLimit.amountText(bytes: 850_400_000), "850 MB")
        XCTAssertEqual(DataLimit.amountText(bytes: 3_870_000_000), "3.9 GB")
        XCTAssertEqual(DataLimit.amountText(bytes: 5_000_000_000), "5 GB")
        XCTAssertEqual(DataLimit.amountText(bytes: 200_000_000_000), "200 GB")
    }

    /// Where each kind of source keeps its limit, and what the app writes
    /// back of what it read, from routers that know limits and from ones
    /// that do not.
    func testLimitsInTheConfiguration() throws {
        var config = try sample()
        let phone = RouterConfig.uplinkPhone
        XCTAssertEqual(config.dataLimit(source: phone), DataLimit(enabled: true, period: "month", mb: 5000, startDay: 1, liftUntil: 0))
        XCTAssertEqual(config.dataLimit(source: RouterConfig.uplinkWifi, ssid: DemoRouter.joinedNetwork), DataLimit())
        XCTAssertEqual(config.dataLimit(source: RouterConfig.uplinkEthernet("end0")), DataLimit())

        let daily = DataLimit(enabled: true, period: "day", mb: 500)
        config.setDataLimit(daily, source: RouterConfig.uplinkWifi, ssid: DemoRouter.joinedNetwork)
        config.setDataLimit(daily, source: RouterConfig.uplinkWifi, ssid: "Nowhere")
        config.setDataLimit(DataLimit(enabled: true, mb: 200_000, startDay: 15), source: RouterConfig.uplinkEthernet("end0"))
        XCTAssertEqual(config.dataLimit(source: RouterConfig.uplinkWifi, ssid: DemoRouter.joinedNetwork), daily)
        XCTAssertTrue(config.dataLimit(source: RouterConfig.uplinkWifi, ssid: DemoRouter.joinedNetwork).isDaily)
        XCTAssertEqual(config.dataLimit(source: RouterConfig.uplinkWifi, ssid: "Nowhere"), DataLimit())
        XCTAssertEqual(config.dataLimit(source: RouterConfig.uplinkEthernet("end0")).startDay, 15)
        // Neither setting of the phone costs it the other.
        config.setShaping(ShapingConfig(enabled: true, downKbps: 20_000, upKbps: 4_000), source: phone)
        XCTAssertEqual(config.dataLimit(source: phone).mb, 5000)
        config.setDataLimit(DataLimit(enabled: false, mb: 5000), source: phone)
        XCTAssertEqual(config.shaping(source: phone).downKbps, 20_000)

        // As the router writes it, and as the app writes it back.
        let encoded = try RouterConnection.makeEncoder().encode(config)
        let text = String(decoding: encoded, as: UTF8.self)
        for field in [#""start_day":15"#, #""lift_until":0"#, #""period":"day""#, #""mb":500"#] {
            XCTAssertTrue(text.contains(field), "the configuration as written lacks \(field)")
        }
        XCTAssertEqual(try RouterConnection.makeDecoder().decode(RouterConfig.self, from: encoded), config)
        let original = try sample()
        let rewritten = try JSONSerialization.jsonObject(with: RouterConnection.makeEncoder().encode(original)) as? [String: Any]
        let asRead = try JSONSerialization.jsonObject(with: Data(DemoRouter.configJSON.utf8)) as? [String: Any]
        XCTAssertEqual(rewritten?["wan"] as? NSDictionary, asRead?["wan"] as? NSDictionary, "the app would change a data limit by writing the configuration back")
        XCTAssertEqual(rewritten?["devices"] as? NSArray, asRead?["devices"] as? NSArray)

        // A router too old to have limits says nothing of them.
        var older = try XCTUnwrap(asRead)
        var wan = try XCTUnwrap(older["wan"] as? [String: Any])
        wan["phone"] = nil
        older["wan"] = wan
        let decoded = try RouterConnection.makeDecoder().decode(RouterConfig.self, from: JSONSerialization.data(withJSONObject: older))
        XCTAssertEqual(decoded.dataLimit(source: phone), DataLimit())
        XCTAssertNil(decoded.ethernet.first?.data)
    }

    /// The sources a Route can name: the internet ports, Wi-Fi while a
    /// radio joins other networks, and the phone, in the router's order.
    func testRoutableSources() throws {
        var config = try sample()
        let wired: [String] = config.ethernet.filter { $0.role == .wan }.map { RouterConfig.uplinkEthernet($0.name) }
        XCTAssertFalse(wired.isEmpty)
        XCTAssertEqual(config.routableSources, config.wan.order.filter { wired.contains($0) || $0 == "wifi" || $0 == "phone" })
        XCTAssertTrue(config.routableSources.contains("wifi"))
        for index in config.radios.indices where config.radios[index].role == .wan || config.radios[index].role == .apWan {
            config.radios[index].role = .ap
        }
        XCTAssertFalse(config.routableSources.contains("wifi"))
        XCTAssertTrue(config.routableSources.contains("phone"))
        config.wan.order = ["phone"] + config.wan.order.filter { $0 != "phone" }
        XCTAssertEqual(config.routableSources.first, "phone")
    }

    /// How a source stands for a Route that names it, by the same rules
    /// the router goes by.
    func testAvailability() throws {
        func status(wan: String, data: String = "[]", stas: String = "[]") throws -> RouterStatus {
            try RouterConnection.makeDecoder().decode(RouterStatus.self, from: Data("""
                {"name":"P","id":"1","version":"v","time":1,"uptime":1,"temp":1,"ready":true,"rev":1,"pairing":false,"internet":"online",
                 "wan":\(wan),"portal":{"captive":false},"aps":[],"stas":\(stas),"off_air":[],"ports":[],
                 "lan":{"address":"10.83.0.1/24","clients":0,"networks":{}},"vpns":[],"geoip":{"available":false},
                 "adblock":{"enabled":false,"domains":0,"blocked":0,"lists":[]},"data":\(data)}
                """.utf8))
        }
        func uplink(_ key: String, _ kind: String, carrier: Bool = true, internet: String = "online", ipv4: String = "192.0.2.2/24") -> String {
            #"{"if":"if-\#(kind)","kind":"\#(kind)","key":"\#(key)","metric":1,"rank":0,"carrier":\#(carrier),"internet":"\#(internet)","ipv4":"\#(ipv4)","has_v6":false}"#
        }
        let prefer = RouteAction.source("phone", only: false)
        let only = RouteAction.source("phone", only: true)

        var now = try status(wan: "[\(uplink("phone", "phone"))]")
        XCTAssertEqual(now.availability(source: "phone"), .online)
        XCTAssertEqual(now.usesSource(prefer), true)
        XCTAssertEqual(now.usesSource(only), true)
        XCTAssertNil(now.usesSource(.direct))
        XCTAssertNil(now.usesSource(.vpn("home")))
        // Not plugged in.
        XCTAssertEqual(now.availability(source: "wifi"), .away)
        XCTAssertEqual(now.usesSource(.source("wifi", only: true)), false)

        // Connected without internet: only what uses nothing else goes there.
        now = try status(wan: "[\(uplink("phone", "phone", internet: "limited"))]")
        XCTAssertEqual(now.availability(source: "phone"), .connected)
        XCTAssertEqual(now.usesSource(prefer), false)
        XCTAssertEqual(now.usesSource(only), true)
        now = try status(wan: "[\(uplink("phone", "phone", internet: "captive"))]")
        XCTAssertEqual(now.availability(source: "phone"), .connected)

        // No carrier, or no address.
        now = try status(wan: "[\(uplink("phone", "phone", carrier: false, internet: "down"))]")
        XCTAssertEqual(now.availability(source: "phone"), .away)
        now = try status(wan: "[\(uplink("phone", "phone", internet: "connecting", ipv4: ""))]")
        XCTAssertEqual(now.availability(source: "phone"), .away)
        XCTAssertEqual(now.usesSource(only), false)

        // Past its data limit: not used, whatever it reaches; lifted, used.
        let spent = #"[{"key":"phone","period":"month","limit":5000000000,"used":5100000000,"since":1,"resets":99,"reached":true,"lifted":false}]"#
        now = try status(wan: "[\(uplink("phone", "phone"))]", data: spent)
        XCTAssertEqual(now.availability(source: "phone"), .dataSpent)
        XCTAssertEqual(now.usesSource(prefer), false)
        XCTAssertEqual(now.usesSource(only), false)
        XCTAssertEqual(now.data(source: "phone")?.fraction, 1)
        now = try status(wan: "[\(uplink("phone", "phone"))]", data: spent.replacingOccurrences(of: #""lifted":false"#, with: #""lifted":true"#))
        XCTAssertEqual(now.availability(source: "phone"), .online)
        XCTAssertEqual(now.data(source: "phone")?.spent, false)

        // For Wi-Fi the limit is that of the network joined.
        let hotspot = #"[{"key":"wifi","ssid":"Hotspot","period":"day","limit":500000000,"used":600000000,"since":1,"resets":99,"reached":true,"lifted":false}]"#
        let joined = { (ssid: String) in #"[{"radio":"usb","iface":"if-wifi","state":"connected","ssid":"\#(ssid)"}]"# }
        now = try status(wan: "[\(uplink("wifi", "wifi"))]", data: hotspot, stas: joined("Hotspot"))
        XCTAssertEqual(now.joinedNetwork, "Hotspot")
        XCTAssertEqual(now.availability(source: "wifi"), .dataSpent)
        now = try status(wan: "[\(uplink("wifi", "wifi"))]", data: hotspot, stas: joined("Home"))
        XCTAssertEqual(now.availability(source: "wifi"), .online)
        XCTAssertNil(now.data(source: "wifi"))
        XCTAssertEqual(now.data(source: "wifi", ssid: "Hotspot")?.spent, true)

        // A router too old to count says nothing, and nothing is spent.
        let old = try RouterConnection.makeDecoder().decode(RouterStatus.self, from: Data("""
            {"name":"P","id":"1","version":"v","time":1,"uptime":1,"temp":1,"ready":true,"rev":1,"pairing":false,"internet":"online",
             "wan":[\(uplink("phone", "phone"))],"portal":{"captive":false},"aps":[],"stas":[],"off_air":[],"ports":[],
             "lan":{"address":"10.83.0.1/24","clients":0,"networks":{}},"vpns":[],"geoip":{"available":false},
             "adblock":{"enabled":false,"domains":0,"blocked":0,"lists":[]}}
            """.utf8))
        XCTAssertNil(old.data)
        XCTAssertEqual(old.availability(source: "phone"), .online)
    }

    /// The sample router: a phone with a limit and most of it used, a
    /// device routed through the phone, and the limit reached and lifted.
    func testSourcesInDemoMode() async throws {
        let router = DemoRouter.makeConnection()
        let config = try await router.config()
        var status = try await router.status()
        let phone = RouterConfig.uplinkPhone
        let usage = try XCTUnwrap(status.data(source: phone))
        XCTAssertEqual(usage.limit, 5_000_000_000)
        XCTAssertEqual(usage.used, 3_870_000_000)
        XCTAssertFalse(usage.reached)
        XCTAssertGreaterThan(usage.resets, status.time)
        XCTAssertLessThanOrEqual(usage.since, status.time)
        XCTAssertEqual(status.data?.count, 1)
        XCTAssertEqual(status.availability(source: phone), .online)

        let devices = try await router.devices()
        let printer = try XCTUnwrap(devices.first(where: { $0.mac == DemoRouter.printer }))
        XCTAssertEqual(printer.route, "source:phone")
        XCTAssertEqual(status.usesSource(try XCTUnwrap(RouteAction(printer.route))), true)
        XCTAssertTrue(config.routableSources.contains(phone))

        // A smaller limit is one the sample phone is past; lifted until
        // the period ends, it is used all the same.
        var next = config
        next.setDataLimit(DataLimit(enabled: true, period: "month", mb: 3000), source: phone)
        next.rev = try await router.setConfig(next)
        status = try await router.status()
        var now = try XCTUnwrap(status.data(source: phone))
        XCTAssertTrue(now.reached)
        XCTAssertTrue(now.spent)
        XCTAssertEqual(status.availability(source: phone), .dataSpent)
        XCTAssertEqual(status.usesSource(.source(phone, only: true)), false)
        var lifted = next.dataLimit(source: phone)
        lifted.liftUntil = now.resets
        next.setDataLimit(lifted, source: phone)
        next.rev = try await router.setConfig(next)
        status = try await router.status()
        now = try XCTUnwrap(status.data(source: phone))
        XCTAssertTrue(now.reached)
        XCTAssertTrue(now.lifted)
        XCTAssertEqual(status.availability(source: phone), .online)

        // A limit for the network joined shows up under Wi-Fi.
        next.setDataLimit(DataLimit(enabled: true, period: "day", mb: 500), source: RouterConfig.uplinkWifi, ssid: DemoRouter.joinedNetwork)
        _ = try await router.setConfig(next)
        status = try await router.status()
        let hotel = try XCTUnwrap(status.data(source: RouterConfig.uplinkWifi))
        XCTAssertEqual(hotel.ssid, DemoRouter.joinedNetwork)
        XCTAssertEqual(hotel.period, "day")
        XCTAssertEqual(status.joinedNetwork, DemoRouter.joinedNetwork)
    }
}
