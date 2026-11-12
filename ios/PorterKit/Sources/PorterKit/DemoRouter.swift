import Foundation

/// A stand-in router with sample data, for running the app without
/// hardware: UI work, screenshots, App Review. It answers the same calls
/// as a real router, in the router's own JSON, so everything above the
/// transport is exercised for real. Configuration changes are kept in
/// memory; nothing else changes.
public enum DemoRouter {
    public static func makeConnection() -> RouterConnection {
        let state = State()
        return RouterConnection(demo: { method, params in try state.answer(method, params) })
    }

    private final class State {
        private let lock = NSLock()
        private var config: RouterConfig

        init() {
            // The sample configuration is known to decode (see DemoTests).
            config = try! RouterConnection.makeDecoder().decode(RouterConfig.self, from: Data(DemoRouter.configJSON.utf8))
            // Whatever the time of day the app is opened at, one group
            // has something to show: its devices were paused for a
            // while, until the hour after next.
            let now = Int64(Date().timeIntervalSince1970)
            config.editGroup(DemoRouter.kidsGroup) { $0.forNow = GroupOverride.pause(until: (now / 3_600 + 2) * 3_600) }
        }

        func answer(_ method: String, _ params: Data) throws -> Data {
            lock.lock()
            defer { lock.unlock() }
            let now = Int64(Date().timeIntervalSince1970)
            switch method {
            case "config.get":
                return try RouterConnection.makeEncoder().encode(config)
            case "config.set":
                struct Params: Decodable { let config: RouterConfig }
                var next = try RouterConnection.makeDecoder().decode(Params.self, from: params).config
                // As with a real router: a write built on a copy that
                // is no longer the current one is refused.
                guard next.rev == config.rev else {
                    throw RouterError(code: RouterError.conflict, msg: "the configuration changed; reload and try again")
                }
                next.rev = config.rev + 1
                config = next
                return Data("{\"rev\":\(next.rev)}".utf8)
            case "hello":
                return Data(#"{"name":"Porter","id":"3fa9c210","version":"v1.0.0","paired":true,"claimed":true,"pairing":false,"ready":true,"control":\#(DemoRouter.control(config))}"#.utf8)
            case "status.get":
                return Data(DemoRouter.status(now: now, rev: config.rev, name: config.system.name, control: DemoRouter.control(config), inbound: DemoRouter.inbound(config, now: now), shaping: DemoRouter.shaping(config), data: try DemoRouter.data(config, now: now), groups: try DemoRouter.groups(config, now: now)).utf8)
            case "devices.list":
                return try RouterConnection.makeEncoder().encode(try DemoRouter.devices(config, now: now))
            case "device.set":
                // Kept with the configuration, as a real router keeps a
                // device's settings.
                let wanted = try RouterConnection.makeDecoder().decode(DeviceSettings.self, from: params)
                config.devices.removeAll { $0.mac == wanted.mac }
                if wanted != DeviceSettings(mac: wanted.mac) {
                    config.devices.append(wanted)
                }
                config.rev += 1
                return Data("{}".utf8)
            case "config.check":
                // The demo has the built-in radio carry the network, which
                // broadcasts one Wi-Fi network at a time.
                struct Params: Decodable { let config: RouterConfig }
                let next = try RouterConnection.makeDecoder().decode(Params.self, from: params).config
                var check = ConfigCheck()
                let extra = next.wifi.dropFirst().filter(\.enabled)
                if let first = extra.first {
                    check.notes = ["“\(first.ssid)” will not be broadcast: this radio can broadcast one Wi-Fi network at a time. A second radio, or a USB Wi-Fi adapter that can broadcast several, would carry it."]
                }
                if next.wifi != config.wifi || next.radios != config.radios {
                    check.effects = ["Your Wi-Fi restarts. Devices reconnect by themselves within a few seconds."]
                }
                return try RouterConnection.makeEncoder().encode(check)
            case "wifi.uplink":
                struct Params: Decodable { let radio: String }
                let radio = try RouterConnection.makeDecoder().decode(Params.self, from: params).radio
                for index in config.radios.indices {
                    config.radios[index].role = config.radios[index].id == radio ? .wan : .ap
                }
                if !radio.isEmpty, config.radios.count == 1 {
                    config.radios[0].role = .apWan
                }
                config.rev += 1
                return Data("{}".utf8)
            case "radios.list":
                return Data(DemoRouter.radiosJSON.utf8)
            case "wifi.scan":
                return Data(DemoRouter.scanJSON.utf8)
            case "events.list":
                return Data(DemoRouter.events(now: now).utf8)
            case "stats.get":
                struct Params: Decodable { let mac: String? }
                let mac = (try? JSONDecoder().decode(Params.self, from: params))?.mac ?? ""
                return Data(DemoRouter.stats(now: now, scale: mac.isEmpty ? 1.0 : 0.45).utf8)
            case "stats.top":
                return Data(DemoRouter.topJSON.utf8)
            case "flows.list":
                struct Params: Decodable { let mac: String? }
                let mac = (try? JSONDecoder().decode(Params.self, from: params))?.mac ?? ""
                return Data(DemoRouter.flows(now: now, mac: mac).utf8)
            case "device.queries":
                return Data(DemoRouter.queries(now: now).utf8)
            case "speedtest.run":
                // Through one internet source, what is measured is the
                // line: a little more than the speeds the sample router
                // holds that source to.
                struct Params: Decodable { let via: String? }
                let via = (try? JSONDecoder().decode(Params.self, from: params))?.via ?? "direct"
                if via.hasPrefix("source:") {
                    return try RouterConnection.makeEncoder().encode(SpeedTestResult(t: now, via: via, latencyMs: 21.6, downMbps: 38.4, upMbps: 9.3, error: nil))
                }
                return Data("{\"t\":\(now),\"via\":\"direct\",\"latency_ms\":18.4,\"down_mbps\":93.7,\"up_mbps\":41.2}".utf8)
            case "speedtest.history":
                return Data("""
                [{"t":\(now - 3_600),"via":"direct","latency_ms":17.9,"down_mbps":94.6,"up_mbps":40.8},
                 {"t":\(now - 3_480),"via":"vpn:home","latency_ms":42.3,"down_mbps":61.2,"up_mbps":33.5},
                 {"t":\(now - 86_400),"via":"direct","latency_ms":23.1,"down_mbps":12.4,"up_mbps":4.9}]
                """.utf8)
            case "pairing.list":
                return Data("""
                [{"key":"qk3vN2xQxq0S1f8W8l7m2cQ8m3w1vR0b4yZk9a5uT2E=","name":"iPhone","added":\(now - 40 * 86_400),"last_seen":\(now),"self":true},
                 {"key":"Zm9vYmFyYmF6cXV4Zm9vYmFyYmF6cXV4Zm9vYmFyYmE=","name":"iPad","added":\(now - 12 * 86_400),"last_seen":\(now - 2 * 86_400),"self":false}]
                """.utf8)
            case "firmware.check":
                return Data(#"{"current":"v1.0.0","supported":true,"latest":"v1.1.0","newer":true,"download":268435456,"built":\#(now - 86_400)}"#.utf8)
            case "firmware.status":
                return Data(#"{"state":"idle","percent":0}"#.utf8)
            case "system.logs":
                return Data(#"["2026-11-10T08:14:02Z INFO porterd starting version=1.0","2026-11-10T08:14:09Z INFO access point up iface=wlan0 channel=36 mode=ac/80MHz","2026-11-10T08:14:21Z INFO uplink reachability if=wlan1 state=captive","2026-11-10T08:15:40Z INFO uplink reachability if=wlan1 state=online","2026-11-10T08:15:44Z INFO vpn state vpn=Home state=up"]"#.utf8)
            case "vpn.genkey":
                return Data(#"{"private_key":"YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU=","public_key":"MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXY="}"#.utf8)
            case "vpn.parse":
                // Always the same sample, whatever was pasted: the VPN
                // profiles in the configuration may all have been deleted.
                return Data(DemoRouter.wireGuardJSON.utf8)
            case "server.peer_add":
                // A device is added for real, with sample keys: nothing
                // will ever connect with them.
                struct Params: Decodable {
                    let name: String
                    let full: Bool
                }
                let wanted = try RouterConnection.makeDecoder().decode(Params.self, from: params)
                guard var inbound = config.inbound else {
                    throw RouterError(code: "invalid", msg: "Switch on reaching home first.")
                }
                let host = 2 + inbound.server.peers.count
                let id = "d\(host)"
                inbound.server.peers.append(HomeDevice(id: id, name: wanted.name, publicKey: "c2FtcGxlLXB1YmxpYy1rZXktZm9yLWRlbW8tbW9kZTA=", address: "10.89.0.\(host)", enabled: true, full: wanted.full))
                config.inbound = inbound
                config.rev += 1
                let allowed = wanted.full ? "0.0.0.0/0, ::/0" : "10.89.0.0/24, 10.83.0.0/24, 10.84.0.0/24, 10.85.0.0/24"
                let conf = "[Interface]\nPrivateKey = c2FtcGxlLXByaXZhdGUta2V5LWZvci1kZW1vLW1vZGU=\nAddress = 10.89.0.\(host)/32\nDNS = 10.89.0.1\n\n[Peer]\nPublicKey = \(DemoRouter.serverKey)\nEndpoint = \(DemoRouter.homeName):51820\nAllowedIPs = \(allowed)\nPersistentKeepalive = 25\n"
                return try RouterConnection.makeEncoder().encode(HomeDeviceAdded(id: id, conf: conf, host: DemoRouter.homeName))
            case "system.set_time":
                return Data(#"{"adjusted":false}"#.utf8)
            default:
                return Data("{}".utf8)
            }
        }
    }

    // MARK: - Sample data

    static let phone = "a6:3c:11:5e:90:21"
    static let laptop = "3c:22:fb:18:7a:c4"
    static let tablet = "f2:9b:40:d1:66:0e"
    static let reader = "28:ef:01:a4:3b:77"
    static let console = "98:b6:e9:52:1c:d0"
    static let printer = "00:1e:8f:3a:5b:c2"
    /// A USB Ethernet adapter: the cable to a switch that takes the main
    /// network as it is and the one for things under its VLAN ID.
    static let adapter = "enx00e04c6801a7"

    static let configJSON = """
    {"version":3,"rev":12,
     "system":{"name":"Porter","country":"AU","ssh":{"enabled":false,"authorized_keys":[]},"bluetooth_only":false,"timezone":"\(timeZone)"},
     "radios":[{"id":"builtin","role":"ap","band":"5","channel":0,"width":0},
               {"id":"usb:9c:ef:d5:12:34:56","role":"wan","band":"","channel":0,"width":0}],
     "ethernet":[{"name":"end0","role":"wan","network":"lan","tagged":[],"vlan":7,
                  "ip":{"v4":{"mode":"auto","address":"","gateway":"","dns":[]},"v6":{"mode":"auto","address":"","gateway":"","dns":[]}},
                  "pppoe":{"enabled":true,"username":"porter@fibre.example","password":"not-a-real-password"},
                  "shaping":{"enabled":true,"down_kbps":237500,"up_kbps":38000}},
                 {"name":"\(adapter)","role":"lan","network":"lan","tagged":["things"],"vlan":0,
                  "ip":{"v4":{"mode":"auto","address":"","gateway":"","dns":[]},"v6":{"mode":"auto","address":"","gateway":"","dns":[]}},
                  "pppoe":{"enabled":false,"username":"","password":""},
                  "shaping":{"enabled":false,"down_kbps":0,"up_kbps":0}}],
     "wifi":[{"id":"main","ssid":"Porter","passphrase":"correct horse battery","security":"wpa2","hidden":false,"isolate":false,"network":"lan","enabled":true},
             {"id":"guest","ssid":"Porter Guest","passphrase":"welcome aboard","security":"wpa2","hidden":false,"isolate":true,"network":"guest","enabled":true}],
     "wan":{"networks":[{"ssid":"Harbour Hotel Guest","psk":"","hidden":false,"priority":3,"shaping":{"enabled":true,"down_kbps":36400,"up_kbps":8830}},
                        {"ssid":"Home","psk":"not-a-real-password","hidden":false,"priority":2,"shaping":{"enabled":false,"down_kbps":0,"up_kbps":0}},
                        {"ssid":"Airport Free WiFi","psk":"","hidden":false,"priority":1}],
            "phone":{"shaping":{"enabled":false,"down_kbps":0,"up_kbps":0},
                     "data":{"enabled":true,"period":"month","mb":5000,"start_day":1,"lift_until":0}},
            "wifi_mac":{"mode":"random","address":"06:5a:91:c3:7e:12"},"eth_mac":{"mode":"hardware"},
            "order":["eth:end0","eth:\(adapter)","wifi","phone"],
            "probe":{"url":"http://captive.apple.com/hotspot-detect.html","expect":"Success"}},
     "lan":{"auto_renumber":true,"lease_minutes":720,"domain":"lan","ipv6":true},
     "networks":[{"id":"lan","name":"Main","address":"10.83.0.1/24","ula_prefix":"fd4c:91a7:20be:1::/64","isolated":false,"route":"","vlan":0,"discovery":false},
                 {"id":"guest","name":"Guest","address":"10.84.0.1/24","ula_prefix":"fd4c:91a7:20be:2::/64","isolated":true,"route":"","vlan":0,"discovery":false},
                 {"id":"things","name":"Things","address":"10.85.0.1/24","ula_prefix":"fd4c:91a7:20be:3::/64","isolated":true,"route":"","vlan":20,"discovery":true}],
     "dns":{"upstreams":["tls://9.9.9.9#dns.quad9.net"],"intercept":true,"blocked":["ads.example.net"],
            "adblock":{"enabled":true,"lists":["https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"],"allow":["links.example.com"]}},
     "vpns":[{"id":"home","name":"Home","enabled":true,"kill_switch":true,
              "wireguard":{"private_key":"YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU=","addresses":["10.8.0.2/32","fd08::2/128"],"dns":["10.8.0.1"],"mtu":0,
                           "peers":[{"public_key":"MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXY=","endpoint":"home.example.net:51820","allowed_ips":["0.0.0.0/0","::/0"],"keepalive":25}]}},
             {"id":"us","name":"Streaming (US)","enabled":false,"kill_switch":false,
              "wireguard":{"private_key":"enl4d3Z1dHNycXBvbm1sa2ppaGdmZWRjYmE5ODc2NTQ=","addresses":["10.64.0.7/32"],"dns":["10.64.0.1"],"mtu":0,
                           "peers":[{"public_key":"dnV0c3JxcG9ubWxramloZ2ZlZGNiYTk4NzY1NDMyMTA=","endpoint":"us-east.vpn.example.com:51820","allowed_ips":["0.0.0.0/0"],"keepalive":25}]}}],
     "policy":{"default":"direct","rules":[
        {"id":"bank","name":"Banking stays direct","enabled":true,"src":{"devices":[],"cidrs":[],"networks":[],"groups":[]},
         "dst":{"domains":["mybank.example.com","payments.example.org"],"cidrs":[],"countries":[],"proto":"","ports":[]},"action":"direct"},
        {"id":"jp","name":"Japan through Home","enabled":true,"src":{"devices":[],"cidrs":[],"networks":[],"groups":[]},
         "dst":{"domains":[],"cidrs":[],"countries":["JP"],"proto":"","ports":[]},"action":"vpn:home"},
        {"id":"smtp","name":"No outgoing mail","enabled":true,"src":{"devices":[],"cidrs":[],"networks":[],"groups":[]},
         "dst":{"domains":[],"cidrs":[],"countries":[],"proto":"tcp","ports":["25"]},"action":"block"}]},
     "devices":[{"mac":"3c:22:fb:18:7a:c4","name":"Work laptop","route":"vpn:home","paused":false,"ip":"10.83.0.57","no_adblock":true},
                {"mac":"98:b6:e9:52:1c:d0","name":"Games console","route":"","paused":true,"no_adblock":false},
                {"mac":"a6:3c:11:5e:90:21","name":"","route":"","paused":false,"no_adblock":false},
                {"mac":"\(printer)","name":"Printer","route":"source:phone","paused":false,"no_adblock":false}],
     "groups":[{"id":"\(kidsGroup)","name":"Kids","icon":"child","members":["\(tablet)","\(console)","\(reader)"],"route":"","paused":false,"no_adblock":false,
                "schedules":[{"id":"bed","name":"Bedtime","enabled":true,"days":[0,1,2,3,4],"from":"21:00","to":"07:00"},
                             {"id":"hw","name":"Homework","enabled":false,"days":[1,2,3,4,5],"from":"16:30","to":"18:00"}],
                "override":{"mode":"","until":0}},
               {"id":"work","name":"Work","icon":"work","members":["\(laptop)"],"route":"vpn:home","paused":false,"no_adblock":true,
                "schedules":[],"override":{"mode":"","until":0}}],
     "speedtest":{"download_url":"https://speed.cloudflare.com/__down?bytes=50000000","upload_url":"https://speed.cloudflare.com/__up"},
     "inbound":{"server":{"enabled":true,"port":51820,"private_key":"c2FtcGxlLXNlcnZlci1rZXktZm9yLWRlbW8tbW9kZTA=","address":"10.89.0.1/24","ula_prefix":"fd4c:91a7:20be:89::/64",
                          "peers":[{"id":"d2","name":"iPhone","public_key":"aVBob25lLXB1YmxpYy1rZXktZm9yLWRlbW8tbW9kZTA=","address":"10.89.0.2","enabled":true,"full":true},
                                   {"id":"d3","name":"Work laptop","public_key":"bGFwdG9wLXB1YmxpYy1rZXktZm9yLWRlbW8tbW9kZTA=","address":"10.89.0.3","enabled":true,"full":false}]},
                "forwards":[{"id":"ssh","name":"Remote login","enabled":true,"proto":"tcp","ports":"2222","device":"3c:22:fb:18:7a:c4","to":22,"from":[]}],
                "ddns":{"enabled":true,"provider":"duckdns","hostname":"\(homeName)","server":"","username":"","password":"not-a-real-token","url":"","ipv6":false},
                "check_url":"https://cloudflare.com/cdn-cgi/trace"}}
    """

    /// The sample household's time zone, and the group whose devices
    /// have schedules.
    static let timeZone = "Australia/Sydney"
    static let kidsGroup = "kids"

    /// How the sample router's groups stand at `now`, and which devices
    /// that pauses: the fields `clock`, `groups` and `held` of the
    /// status. Worked out from the configuration as a real router does,
    /// less carefully: a schedule's end is not looked beyond for another
    /// that follows it.
    static func groups(_ config: RouterConfig, now: Int64) throws -> String {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: config.system.timezone ?? "") ?? calendar.timeZone
        let today: Date = calendar.startOfDay(for: Date(timeIntervalSince1970: TimeInterval(now)))
        // When a schedule that starts `ahead` days from today is in
        // force: nil if it does not start on that day of the week.
        func span(_ schedule: GroupSchedule, ahead: Int) -> (start: Int64, end: Int64)? {
            guard let day = calendar.date(byAdding: .day, value: ahead, to: today),
                  schedule.days.contains(calendar.component(.weekday, from: day) - 1),
                  let from = GroupSchedule.parts(of: schedule.from), let to = GroupSchedule.parts(of: schedule.to),
                  let start = calendar.date(bySettingHour: from.hour, minute: from.minute, second: 0, of: day),
                  let last = calendar.date(byAdding: .day, value: schedule.endsNextDay ? 1 : 0, to: day),
                  let end = calendar.date(bySettingHour: to.hour, minute: to.minute, second: 0, of: last) else { return nil }
            return (Int64(start.timeIntervalSince1970), Int64(end.timeIntervalSince1970))
        }
        var states: [GroupStatus] = []
        var held: [HeldDevice] = []
        for group in config.groupList {
            var state = GroupStatus(id: group.id, connected: group.members.filter { onlineDevices.contains($0) }.count, offline: false)
            let timed: Bool = group.forNow.until > now
            var inForce: (id: String, end: Int64)?
            var next: (id: String, start: Int64)?
            for schedule in group.schedules where schedule.enabled {
                for ahead in -1...7 {
                    guard let time = span(schedule, ahead: ahead) else { continue }
                    if time.start <= now, time.end > now, time.end > (inForce?.end ?? 0) {
                        inForce = (schedule.id, time.end)
                    }
                    if time.start > now, (next?.start ?? Int64.max) > time.start {
                        next = (schedule.id, time.start)
                    }
                }
            }
            if group.paused {
                state.offline = true
                state.why = "paused"
            } else if timed, group.forNow.mode == "pause" {
                state.offline = true
                state.why = "timer"
                state.until = group.forNow.until
            } else if let inForce, timed, group.forNow.mode == "allow" {
                state.allowedUntil = group.forNow.until
                if group.forNow.until < inForce.end {
                    state.next = group.forNow.until
                    state.nextSchedule = inForce.id
                }
            } else if let inForce {
                state.offline = true
                state.why = "schedule"
                state.schedule = inForce.id
                state.until = inForce.end
            }
            if !state.offline, state.next == nil, let next {
                state.next = next.start
                state.nextSchedule = next.id
            }
            states.append(state)
            for mac in group.members where state.offline {
                let allowed: Int64 = config.devices.first(where: { $0.mac == mac })?.allowUntil ?? 0
                if allowed <= now || state.why == "paused" {
                    held.append(HeldDevice(mac: mac, why: state.why ?? "", group: group.id, until: state.until))
                }
            }
        }
        for device in config.devices where device.paused {
            held.removeAll { $0.mac == device.mac }
            held.append(HeldDevice(mac: device.mac, why: "device"))
        }
        let encoder = RouterConnection.makeEncoder()
        let zone: String = config.system.timezone ?? ""
        return "\"clock\":" + String(decoding: try encoder.encode(ClockStatus(zone: zone, trusted: true)), as: UTF8.self)
            + ",\"groups\":" + String(decoding: try encoder.encode(states), as: UTF8.self)
            + ",\"held\":" + String(decoding: try encoder.encode(held), as: UTF8.self)
    }

    /// The name the sample router keeps pointing at itself, and its
    /// WireGuard server's public key.
    static let homeName = "porter-demo.duckdns.org"
    static let serverKey = "c2VydmVyLXB1YmxpYy1rZXktZm9yLWRlbW8tbW9kZTA="

    /// What the sample router says about coming in from outside. It is
    /// on the road, joined to a hotel's network: behind another router.
    static func inbound(_ config: RouterConfig, now: Int64) -> String {
        guard let inbound = config.inbound else {
            return #"{"reach":"offline","server":{"state":"off","public_key":"","peers":[]},"forwards":[],"ddns":{"state":"off"}}"#
        }
        let peers = inbound.server.peers.enumerated().map { index, peer -> String in
            if index == 0, peer.enabled, inbound.server.enabled {
                return "{\"id\":\"\(peer.id)\",\"connected\":true,\"endpoint\":\"198.51.100.61:49152\",\"handshake\":\(now - 38),\"rx\":48234496,\"tx\":391118848}"
            }
            return "{\"id\":\"\(peer.id)\",\"connected\":false,\"rx\":0,\"tx\":0}"
        }.joined(separator: ",")
        let forwards = inbound.forwards.filter(\.enabled).map { "{\"id\":\"\($0.id)\",\"to\":\"10.83.0.57\",\"to6\":[]}" }.joined(separator: ",")
        let named = inbound.ddns.enabled && !inbound.ddns.hostname.isEmpty
        let host = named ? inbound.ddns.hostname : "203.0.113.150"
        let ddns = named ? "{\"state\":\"ok\",\"updated\":\(now - 7_200),\"address\":\"203.0.113.150\"}" : "{\"state\":\"off\"}"
        return """
        {"reach":"nat","address":"172.20.14.87","public":"203.0.113.150","host":"\(host)",
         "server":{"state":"\(inbound.server.enabled ? "up" : "off")","public_key":"\(serverKey)","peers":[\(peers)]},
         "forwards":[\(forwards)],"ddns":\(ddns)}
        """
    }

    /// What reading a pasted WireGuard configuration gives.
    static let wireGuardJSON = """
    {"private_key":"YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU=","addresses":["10.8.0.2/32","fd08::2/128"],"dns":["10.8.0.1"],"mtu":0,
     "peers":[{"public_key":"MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXY=","endpoint":"home.example.net:51820","allowed_ips":["0.0.0.0/0","::/0"],"keepalive":25}]}
    """

    /// Where the sample router says the app reaches it over its networks:
    /// at its addresses in the main one (the guest network is kept
    /// apart), unless it is set to Bluetooth only.
    static func control(_ config: RouterConfig) -> String {
        if config.system.bluetoothOnly == true { return #"{"port":0,"addrs":[]}"# }
        if config.inbound?.server.enabled == true {
            // And in its WireGuard server's range, for a phone that is
            // connected to that from somewhere else.
            return #"{"port":7678,"addrs":["10.83.0.1","fd4c:91a7:20be:1::1","10.89.0.1","fd4c:91a7:20be:89::1"]}"#
        }
        return #"{"port":7678,"addrs":["10.83.0.1","fd4c:91a7:20be:1::1"]}"#
    }

    /// The network the sample router has joined, the interface it joined
    /// it with, and the phone that is plugged in.
    static let joinedNetwork = "Harbour Hotel Guest"
    static let wifiInterface = "wlan1"
    static let phoneInterface = "enx3a1f0c2299d4"

    /// How holding the sample router's internet sources to their speeds
    /// stands: the hotel's network is the one in use, the cable for the
    /// wired port is at home.
    static func shaping(_ config: RouterConfig) -> String {
        var out: [String] = []
        func entry(_ key: String, _ iface: String, _ state: String, _ set: ShapingConfig, _ down: (Int, Int), _ up: (Int, Int)) -> String {
            "{\"key\":\"\(key)\",\"if\":\"\(iface)\",\"state\":\"\(state)\",\"down\":{\"kbps\":\(set.downKbps),\"dropped\":\(down.0),\"marked\":\(down.1)},\"up\":{\"kbps\":\(set.upKbps),\"dropped\":\(up.0),\"marked\":\(up.1)}}"
        }
        for port in config.ethernet where port.role == .wan {
            if let set = port.shaping, set.enabled {
                out.append(entry(RouterConfig.uplinkEthernet(port.name), "ppp-" + port.name, "waiting", set, (0, 0), (0, 0)))
            }
        }
        let hotel = config.shaping(source: RouterConfig.uplinkWifi, ssid: joinedNetwork)
        if hotel.enabled {
            out.append(entry(RouterConfig.uplinkWifi, wifiInterface, "active", hotel, (1834, 212), (96, 41)))
        }
        let phone = config.shaping(source: RouterConfig.uplinkPhone)
        if phone.enabled {
            out.append(entry(RouterConfig.uplinkPhone, phoneInterface, "active", phone, (57, 0), (12, 0)))
        }
        return "[" + out.joined(separator: ",") + "]"
    }

    /// What the sources with a data limit have used of it: the phone most
    /// of its month's, any other a tenth, each as far into its period as
    /// a third of it.
    static func data(_ config: RouterConfig, now: Int64) throws -> String {
        var out: [DataStatus] = []
        func add(_ key: String, _ ssid: String?, _ limit: DataLimit, usedMB: Int) {
            guard limit.enabled else { return }
            let length: Int64 = limit.isDaily ? 86_400 : 30 * 86_400
            let since: Int64 = now - length / 3
            let used = UInt64(max(usedMB, 0)) * 1_000_000
            let amount = UInt64(max(limit.mb, 0)) * 1_000_000
            let reached: Bool = used >= amount
            out.append(DataStatus(key: key, ssid: ssid, period: limit.period, limit: amount, used: used, since: since, resets: since + length,
                                  reached: reached, lifted: reached && limit.liftUntil > now))
        }
        for port in config.ethernet where port.role == .wan {
            let limit: DataLimit = port.data ?? DataLimit()
            add(RouterConfig.uplinkEthernet(port.name), nil, limit, usedMB: limit.mb / 10)
        }
        for network in config.wan.networks {
            let limit: DataLimit = network.data ?? DataLimit()
            add(RouterConfig.uplinkWifi, network.ssid, limit, usedMB: limit.mb / 10)
        }
        add(RouterConfig.uplinkPhone, nil, config.dataLimit(source: RouterConfig.uplinkPhone), usedMB: 3_870)
        return String(decoding: try RouterConnection.makeEncoder().encode(out), as: UTF8.self)
    }

    static func status(now: Int64, rev: UInt64, name: String, control: String, inbound: String, shaping: String, data: String, groups: String) -> String {
        let safeName = name.replacingOccurrences(of: "\\", with: "").replacingOccurrences(of: "\"", with: "'")
        return """
        {"name":"\(safeName)","id":"3fa9c210","version":"v1.0.0","time":\(now),"uptime":93784,"temp":51.3,"ready":true,"rev":\(rev),"pairing":false,
         "internet":"online","active":"wlan1",
         "wan":[{"if":"ppp-end0","kind":"eth","key":"eth:end0","metric":100,"rank":0,"carrier":false,"internet":"down","has_v6":false,"mac":"2c:cf:67:1a:44:90","pppoe":"signing_in"},
                {"if":"wlan1","kind":"wifi","key":"wifi","metric":200,"rank":1,"carrier":true,"internet":"online","ipv4":"172.20.14.87/22","gateway":"172.20.12.1",
                 "ipv6":["2001:db8:41:7::9c2f/64"],"has_v6":true,"dns":["172.20.12.1"],"mac":"06:5a:91:c3:7e:12"},
                {"if":"enx3a1f0c2299d4","kind":"phone","key":"phone","label":"iPhone","metric":300,"rank":2,"carrier":true,"internet":"online","ipv4":"172.20.10.2/28","gateway":"172.20.10.1",
                 "has_v6":false,"dns":["172.20.10.1"]}],
         "portal":{"captive":false},
         "aps":[{"radio":"builtin","iface":"wlan0","state":"up","band":"5","channel":36,"mode":"ac/80MHz","ssids":["main"]}],
         "stas":[{"radio":"usb:9c:ef:d5:12:34:56","iface":"wlan1","state":"connected","ssid":"Harbour Hotel Guest","bssid":"7c:10:c9:2b:55:e0","freq":5220,"signal":-58}],
         "off_air":["guest"],
         "ports":[{"name":"end0","phone":false,"carrier":false},{"name":"\(adapter)","label":"USB 10/100/1000 LAN","phone":false,"carrier":true},
                  {"name":"enx3a1f0c2299d4","label":"iPhone","phone":true,"carrier":true}],
         "lan":{"address":"10.83.0.1/24","ula":"fd4c:91a7:20be:1::/64","clients":5,"networks":{"lan":4,"things":1},"prefixes":{}},
         "control":\(control),
         "vpns":[{"id":"home","name":"Home","if":"wg-home","state":"up","endpoint":"203.0.113.24:51820","handshake":\(now - 47),"rx":1843200512,"tx":211812352},
                 {"id":"us","name":"Streaming (US)","state":"off","rx":0,"tx":0}],
         "inbound":\(inbound),
         "shaping":\(shaping),
         "data":\(data),
         \(groups),
         "geoip":{"available":true,"date":"2026-11"},
         "adblock":{"enabled":true,"domains":78215,"blocked":1284,
                    "lists":[{"url":"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts","domains":78215,"updated":\(now - 21_600)}]}}
        """
    }

    /// The devices that are on the sample router's networks.
    static let onlineDevices: Set<String> = [phone, laptop, tablet, reader, printer]

    /// The sample devices with what the configuration says of each: the
    /// settings made for it and the group it is in.
    static func devices(_ config: RouterConfig, now: Int64) throws -> [DeviceInfo] {
        var list = try RouterConnection.makeDecoder().decode([DeviceInfo].self, from: Data(seen(now: now).utf8))
        for index in list.indices {
            let mac: String = list[index].mac
            let settings: DeviceSettings = config.devices.first(where: { $0.mac == mac }) ?? DeviceSettings(mac: mac)
            list[index].name = settings.name
            list[index].route = settings.route
            list[index].paused = settings.paused
            list[index].ip = settings.ip
            list[index].noAdblock = settings.noAdblock
            list[index].allowUntil = settings.allowUntil
            list[index].group = config.group(of: mac)?.id
        }
        return list
    }

    /// What the sample router has seen of each device. The settings in
    /// it are filled in from the configuration (`devices`).
    static func seen(now: Int64) -> String {
        """
        [{"mac":"\(phone)","hostname":"iPhone","ipv4":"10.83.0.52","ipv6":["fd4c:91a7:20be:1:1c3a:77ff:fe20:9b01"],"first_seen":\(now - 41 * 86_400),"last_seen":\(now),"online":true,"online_since":\(now - 5_400),"link":"wlan0","network":"lan","signal":-47,"up":412300288,"down":3865470566,"name":"","route":"","paused":false,"no_adblock":false},
         {"mac":"\(laptop)","hostname":"MacBook-Pro","ipv4":"10.83.0.57","ipv6":["fd4c:91a7:20be:1:8d2:41ff:fe7a:c4e2"],"first_seen":\(now - 41 * 86_400),"last_seen":\(now),"online":true,"online_since":\(now - 5_100),"link":"wlan0","network":"lan","signal":-53,"up":1288490188,"down":9126805504,"name":"Work laptop","route":"vpn:home","paused":false,"ip":"10.83.0.57","no_adblock":true},
         {"mac":"\(tablet)","hostname":"iPad","ipv4":"10.83.0.61","first_seen":\(now - 12 * 86_400),"last_seen":\(now),"online":true,"online_since":\(now - 2_700),"link":"wlan0","network":"lan","signal":-61,"up":96468992,"down":2254857830,"name":"","route":"","paused":false,"no_adblock":false},
         {"mac":"\(reader)","hostname":"kindle","ipv4":"10.83.0.66","first_seen":\(now - 9 * 86_400),"last_seen":\(now),"online":true,"online_since":\(now - 900),"link":"wlan0","network":"lan","signal":-70,"up":1258291,"down":48234496,"name":"","route":"","paused":false,"no_adblock":false},
         {"mac":"\(printer)","hostname":"Printer","ipv4":"10.85.0.60","first_seen":\(now - 20 * 86_400),"last_seen":\(now),"online":true,"online_since":\(now - 86_000),"link":"wired","network":"things","up":3145728,"down":20971520,"name":"","route":"","paused":false,"no_adblock":false},
         {"mac":"\(console)","hostname":"Switch","first_seen":\(now - 30 * 86_400),"last_seen":\(now - 3 * 86_400),"online":false,"up":734003200,"down":18146236006,"name":"Games console","route":"","paused":true,"no_adblock":false}]
        """
    }

    static let radiosJSON: String = {
        let ch24 = [1, 6, 11].map { "{\"num\":\($0),\"freq\":\(2407 + 5 * $0)}" }.joined(separator: ",")
        let ch5 = [36, 40, 44, 48, 149, 153, 157, 161].map { "{\"num\":\($0),\"freq\":\(5000 + 5 * $0)}" }.joined(separator: ",")
        let bands = "[{\"name\":\"2.4\",\"channels\":[\(ch24)],\"ht\":true,\"vht\":false,\"he\":false},{\"name\":\"5\",\"channels\":[\(ch5)],\"ht\":true,\"vht\":true,\"he\":false}]"
        return """
        [{"id":"builtin","phy":"phy0","iface":"wlan0","driver":"brcmfmac","bus":"sdio","product":"Built-in Wi-Fi","mac":"2c:cf:67:1a:44:91","bands":\(bands),"ap":true,"sta":true,"ap_sta":true,"max_ap":1,"max_ap_sta":1,"present":true,"role":"ap"},
         {"id":"usb:9c:ef:d5:12:34:56","phy":"phy1","iface":"wlan1","driver":"mt7921u","bus":"usb","product":"Wireless_Device","mac":"9c:ef:d5:12:34:56","bands":\(bands),"ap":true,"sta":true,"ap_sta":true,"max_ap":4,"max_ap_sta":3,"present":true,"role":"wan"}]
        """
    }()

    static let scanJSON = """
    [{"ssid":"Harbour Hotel Guest","signal":-58,"freq":5220,"security":"open","known":true},
     {"ssid":"Harbour Hotel Staff","signal":-60,"freq":5220,"security":"enterprise","known":false},
     {"ssid":"Cafe Lumen","signal":-71,"freq":2437,"security":"psk","known":false},
     {"ssid":"Airport Free WiFi","signal":-79,"freq":2412,"security":"open","known":true},
     {"ssid":"DIRECT-7F-Printer","signal":-83,"freq":2462,"security":"psk","known":false}]
    """

    static func events(now: Int64) -> String {
        """
        [{"t":\(now - 5_520),"kind":"portal","msg":"The network the router joined asks for a sign-in before it lets anything online","mac":"\(phone)"},
         {"t":\(now - 5_440),"kind":"portal","msg":"Signed in. The router is online."},
         {"t":\(now - 5_100),"kind":"device","msg":"New device joined","mac":"\(reader)"},
         {"t":\(now - 3_600),"kind":"lan","msg":"Main now uses the addresses 10.83.0.0/24. The network the router joined uses 192.168.8.0/24 as well."},
         {"t":\(now - 900),"kind":"pairing","msg":"Paired with iPad"}]
        """
    }

    /// An evening's worth of plausible traffic: quiet, with a few bursts.
    static func stats(now: Int64, scale: Double) -> String {
        func series(count: Int, step: Int64, base: Double) -> String {
            let last = now / step * step
            return (0..<count).map { i -> String in
                let x = Double(i)
                let wave = 0.55 + 0.45 * sin(x / 4.1) * cos(x / 9.3)
                let burst = (i % 17 == 5 || i % 23 == 11) ? 3.2 : 1.0
                let down = max(0, base * scale * wave * burst)
                let up = down * (0.06 + 0.05 * abs(sin(x / 2.7)))
                return "{\"t\":\(last - Int64(count - 1 - i) * step),\"up\":\(UInt64(up)),\"down\":\(UInt64(down))}"
            }.joined(separator: ",")
        }
        return "{\"minutes\":[\(series(count: 60, step: 60, base: 38_000_000))],\"hours\":[\(series(count: 72, step: 3_600, base: 900_000_000))]}"
    }

    static let topJSON = """
    [{"mac":"\(laptop)","up":412316860,"down":5368709120},
     {"mac":"\(phone)","up":96468992,"down":1932735283},
     {"mac":"\(tablet)","up":31457280,"down":1181116006},
     {"mac":"\(reader)","up":524288,"down":17825792}]
    """

    static func flows(now: Int64, mac: String) -> String {
        let all: [(String, String)] = [
            (laptop, "{\"mac\":\"\(laptop)\",\"proto\":\"udp\",\"dst\":\"198.51.100.44\",\"port\":443,\"name\":\"meet.example.com\",\"country\":\"US\",\"path\":\"vpn:home\",\"up\":48234496,\"down\":391118848,\"started\":\(now - 1_840),\"last\":\(now),\"active\":true}"),
            (phone, "{\"mac\":\"\(phone)\",\"proto\":\"tcp\",\"dst\":\"203.0.113.80\",\"port\":443,\"name\":\"video.cdn.example.net\",\"country\":\"SG\",\"path\":\"direct\",\"up\":2202009,\"down\":268435456,\"started\":\(now - 620),\"last\":\(now),\"active\":true}"),
            (tablet, "{\"mac\":\"\(tablet)\",\"proto\":\"tcp\",\"dst\":\"192.0.2.133\",\"port\":443,\"name\":\"stream.tv.example.jp\",\"country\":\"JP\",\"path\":\"vpn:home\",\"up\":1048576,\"down\":157286400,\"started\":\(now - 410),\"last\":\(now),\"active\":true}"),
            (laptop, "{\"mac\":\"\(laptop)\",\"proto\":\"tcp\",\"dst\":\"198.51.100.9\",\"port\":993,\"name\":\"imap.mail.example.com\",\"country\":\"DE\",\"path\":\"vpn:home\",\"up\":86016,\"down\":4194304,\"started\":\(now - 5_000),\"last\":\(now),\"active\":true}"),
            (phone, "{\"mac\":\"\(phone)\",\"proto\":\"tcp\",\"dst\":\"203.0.113.12\",\"port\":443,\"name\":\"mybank.example.com\",\"country\":\"AU\",\"path\":\"direct\",\"up\":20480,\"down\":163840,\"started\":\(now - 75),\"last\":\(now),\"active\":true}"),
            (reader, "{\"mac\":\"\(reader)\",\"proto\":\"tcp\",\"dst\":\"198.51.100.201\",\"port\":443,\"name\":\"books.example.com\",\"country\":\"IE\",\"path\":\"direct\",\"up\":12288,\"down\":2621440,\"started\":\(now - 240),\"last\":\(now),\"active\":true}"),
            (phone, "{\"mac\":\"\(phone)\",\"proto\":\"udp\",\"dst\":\"2001:db8:9::5\",\"port\":443,\"name\":\"photos.example.com\",\"country\":\"US\",\"path\":\"direct\",\"up\":73400320,\"down\":1048576,\"started\":\(now - 900),\"last\":\(now - 130),\"active\":false}"),
            (tablet, "{\"mac\":\"\(tablet)\",\"proto\":\"tcp\",\"dst\":\"192.0.2.60\",\"port\":80,\"name\":\"captive.apple.com\",\"country\":\"US\",\"path\":\"direct\",\"up\":820,\"down\":1240,\"started\":\(now - 2_700),\"last\":\(now - 2_690),\"active\":false}"),
            (laptop, "{\"mac\":\"\(laptop)\",\"proto\":\"tcp\",\"dst\":\"198.51.100.77\",\"port\":22,\"country\":\"NL\",\"path\":\"vpn:home\",\"up\":524288,\"down\":786432,\"started\":\(now - 4_000),\"last\":\(now - 1_500),\"active\":false}"),
        ]
        return "[" + all.filter { mac.isEmpty || $0.0 == mac }.map(\.1).joined(separator: ",") + "]"
    }

    static func queries(now: Int64) -> String {
        """
        [{"t":\(now - 300),"mac":"\(phone)","name":"mybank.example.com","type":"A","path":"direct"},
         {"t":\(now - 240),"mac":"\(phone)","name":"video.cdn.example.net","type":"AAAA","path":"direct"},
         {"t":\(now - 120),"mac":"\(phone)","name":"ads.example.net","type":"A","path":"block"},
         {"t":\(now - 90),"mac":"\(phone)","name":"metrics.tracker.example","type":"A","path":"adblock"},
         {"t":\(now - 60),"mac":"\(phone)","name":"photos.example.com","type":"A","path":"direct"}]
        """
    }
}
