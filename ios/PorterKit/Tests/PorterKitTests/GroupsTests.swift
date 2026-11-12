import XCTest
@testable import PorterKit

/// Groups of devices and their schedules, as the app models them.
final class GroupsTests: XCTestCase {
    private func sample() throws -> RouterConfig {
        try RouterConnection.makeDecoder().decode(RouterConfig.self, from: Data(DemoRouter.configJSON.utf8))
    }

    func testDaysInWords() {
        func text(_ days: [Int]) -> String { GroupSchedule(id: "s", days: days).daysText }
        XCTAssertEqual(text([0, 1, 2, 3, 4, 5, 6]), "Every day")
        XCTAssertEqual(text([1, 2, 3, 4, 5]), "Weekdays")
        XCTAssertEqual(text([6, 0]), "Weekends")
        XCTAssertEqual(text([0, 1, 2, 3, 4]), "Sunday to Thursday")
        XCTAssertEqual(text([4, 3, 2, 1, 0]), "Sunday to Thursday")
        // A run that goes round the end of the week.
        XCTAssertEqual(text([5, 6, 0]), "Friday to Sunday")
        XCTAssertEqual(text([6, 0, 1, 2]), "Saturday to Tuesday")
        XCTAssertEqual(text([1, 3, 5]), "Mon, Wed and Fri")
        XCTAssertEqual(text([0, 3]), "Wed and Sun")
        XCTAssertEqual(text([5, 6]), "Fri and Sat")
        XCTAssertEqual(text([1, 2, 4, 5]), "Mon, Tue, Thu and Fri")
        XCTAssertEqual(text([6]), "Saturdays")
        XCTAssertEqual(text([]), "No days")
        XCTAssertEqual(text([9, -1]), "No days")
        XCTAssertEqual(GroupSchedule.bedtime(id: "b").daysText, "Sunday to Thursday")
    }

    func testTimesOfDay() {
        XCTAssertEqual(GroupSchedule.time(hour: 7, minute: 5), "07:05")
        XCTAssertEqual(GroupSchedule.time(hour: 21, minute: 0), "21:00")
        XCTAssertEqual(GroupSchedule.parts(of: "21:00")?.hour, 21)
        XCTAssertEqual(GroupSchedule.parts(of: "07:05")?.minute, 5)
        for bad in ["", "7:05", "24:00", "12:60", "12.30", "12:3", "ab:cd", "12:30:00"] {
            XCTAssertNil(GroupSchedule.parts(of: bad), bad)
        }
        XCTAssertTrue(GroupSchedule(id: "s", from: "21:00", to: "07:00").endsNextDay)
        XCTAssertTrue(GroupSchedule(id: "s", from: "00:00", to: "00:00").endsNextDay)
        XCTAssertFalse(GroupSchedule(id: "s", from: "16:30", to: "18:00").endsNextDay)
    }

    /// A device is in one group at most: putting it into one takes it
    /// out of the other.
    func testMembership() throws {
        var config = try sample()
        XCTAssertEqual(config.group(of: DemoRouter.tablet)?.id, "kids")
        XCTAssertEqual(config.group(of: DemoRouter.laptop)?.id, "work")
        XCTAssertNil(config.group(of: DemoRouter.phone))
        config.setGroup(of: DemoRouter.tablet, to: "work")
        XCTAssertEqual(config.group("work")?.members, [DemoRouter.laptop, DemoRouter.tablet])
        XCTAssertEqual(config.group("kids")?.members, [DemoRouter.console, DemoRouter.reader])
        config.setGroup(of: DemoRouter.tablet, to: "work")
        XCTAssertEqual(config.group("work")?.members, [DemoRouter.laptop, DemoRouter.tablet])
        config.setGroup(of: DemoRouter.phone, to: "kids")
        XCTAssertEqual(config.group(of: DemoRouter.phone)?.id, "kids")
        config.setGroup(of: DemoRouter.tablet, to: nil)
        XCTAssertNil(config.group(of: DemoRouter.tablet))
        XCTAssertEqual(config.groupList.reduce(0) { $0 + $1.members.filter { $0 == DemoRouter.tablet }.count }, 0)
        config.editGroup("kids") { $0.paused = true }
        XCTAssertEqual(config.group("kids")?.paused, true)
        config.editGroup("nobody") { $0.paused = true }
        XCTAssertEqual(config.groupList.count, 2)
    }

    /// Removing a group must not turn a rule that was for it into a rule
    /// for every device.
    func testRemovingAGroup() throws {
        var config = try sample()
        let before: Int = config.policy.rules.count
        config.policy.rules.append(PolicyRule(id: "k1", name: "Kids only", src: RuleSource(groups: ["kids"]), action: "block"))
        config.policy.rules.append(PolicyRule(id: "k2", name: "Kids and work", src: RuleSource(groups: ["kids", "work"]), action: "block"))
        config.policy.rules.append(PolicyRule(id: "k3", name: "Kids and a laptop", src: RuleSource(devices: [DemoRouter.laptop], groups: ["kids"]), action: "block"))
        config.policy.rules.append(PolicyRule(id: "w1", name: "Work only", src: RuleSource(groups: ["work"]), action: "direct"))
        XCTAssertEqual(config.rulesOnly(forGroup: "kids").map(\.id), ["k1"])
        XCTAssertFalse(config.policy.rules[before].src.isEveryone)
        config.removeGroup("kids")
        XCTAssertNil(config.group("kids"))
        XCTAssertEqual(config.groupList.map(\.id), ["work"])
        XCTAssertEqual(config.policy.rules.count, before + 3)
        XCTAssertEqual(config.policy.rules.suffix(3).map(\.id), ["k2", "k3", "w1"])
        XCTAssertEqual(config.policy.rules.suffix(3).map(\.src.groupIDs), [["work"], [], ["work"]])
        XCTAssertFalse(config.policy.rules.contains { $0.src.groupIDs.contains("kids") })
        // The rules that were for everybody still are, and are still there.
        XCTAssertEqual(config.policy.rules.prefix(before).filter(\.src.isEveryone).count, before)
    }

    /// What the app writes back is what the router sent, and a router
    /// that has none of this yet is read all the same.
    func testRoundTripAndOlderRouters() throws {
        let config = try sample()
        let kids = try XCTUnwrap(config.group("kids"))
        XCTAssertEqual(kids.schedules.map(\.id), ["bed", "hw"])
        XCTAssertEqual(kids.schedules[0].days, [0, 1, 2, 3, 4])
        XCTAssertEqual(kids.forNow, GroupOverride())
        XCTAssertEqual(config.system.timezone, DemoRouter.timeZone)
        let rewritten = try JSONSerialization.jsonObject(with: RouterConnection.makeEncoder().encode(config)) as? [String: Any]
        let original = try JSONSerialization.jsonObject(with: Data(DemoRouter.configJSON.utf8)) as? [String: Any]
        XCTAssertEqual(rewritten?["groups"] as? NSArray, original?["groups"] as? NSArray, "the app would change the groups by writing the configuration back")
        XCTAssertEqual(rewritten?["policy"] as? NSDictionary, original?["policy"] as? NSDictionary)
        XCTAssertEqual((rewritten?["system"] as? NSDictionary)?["timezone"] as? String, DemoRouter.timeZone)

        var older = try XCTUnwrap(original)
        older["groups"] = nil
        var system = try XCTUnwrap(older["system"] as? [String: Any])
        system["timezone"] = nil
        older["system"] = system
        var policy = try XCTUnwrap(older["policy"] as? [String: Any])
        policy["rules"] = [["id": "r", "name": "R", "enabled": true, "action": "block",
                            "src": ["devices": [], "cidrs": [], "networks": []] as [String: Any],
                            "dst": ["domains": [], "cidrs": [], "countries": [], "proto": "", "ports": []] as [String: Any]] as [String: Any]]
        older["policy"] = policy
        let decoded = try RouterConnection.makeDecoder().decode(RouterConfig.self, from: JSONSerialization.data(withJSONObject: older))
        XCTAssertNil(decoded.groups)
        XCTAssertEqual(decoded.groupList, [])
        XCTAssertNil(decoded.system.timezone)
        XCTAssertNil(decoded.policy.rules[0].src.groups)
        XCTAssertTrue(decoded.policy.rules[0].src.isEveryone)
        XCTAssertNil(decoded.group(of: DemoRouter.tablet))

        // A status and a device list without the new fields.
        let status = try RouterConnection.makeDecoder().decode(RouterStatus.self, from: Data("""
            {"name":"P","id":"1","version":"v","time":1,"uptime":1,"temp":1,"ready":true,"rev":1,"pairing":false,"internet":"down",
             "wan":[],"portal":{"captive":false},"aps":[],"stas":[],"off_air":[],"ports":[],
             "lan":{"address":"10.83.0.1/24","clients":0,"networks":{}},"vpns":[],"geoip":{"available":false},
             "adblock":{"enabled":false,"domains":0,"blocked":0,"lists":[]}}
            """.utf8))
        XCTAssertNil(status.groups)
        XCTAssertNil(status.clock)
        XCTAssertNil(status.held("02:00:00:00:00:01"))
        XCTAssertNil(status.group("kids"))
        let device = try RouterConnection.makeDecoder().decode(DeviceInfo.self, from: Data("""
            {"mac":"02:00:00:00:00:01","first_seen":1,"last_seen":1,"online":false,"up":0,"down":0,"name":"","route":"","paused":false,"no_adblock":false}
            """.utf8))
        XCTAssertNil(device.group)
        XCTAssertNil(device.settings.allowUntil)
    }

    /// The sample router works its groups out from the configuration at
    /// whatever time it is asked for: bedtime where the sample
    /// household lives, daylight saving included.
    func testDemoSchedulesAtFixedTimes() throws {
        struct Fields: Decodable {
            let clock: ClockStatus
            let groups: [GroupStatus]
            let held: [HeldDevice]
        }
        var config = try sample()
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = try XCTUnwrap(TimeZone(identifier: DemoRouter.timeZone))
        func at(_ month: Int, _ day: Int, _ hour: Int, _ minute: Int = 0) throws -> Int64 {
            let date = try XCTUnwrap(calendar.date(from: DateComponents(year: 2026, month: month, day: day, hour: hour, minute: minute)))
            return Int64(date.timeIntervalSince1970)
        }
        func kids(_ now: Int64) throws -> (state: GroupStatus, held: [String]) {
            let json: String = "{" + (try DemoRouter.groups(config, now: now)) + "}"
            let fields = try RouterConnection.makeDecoder().decode(Fields.self, from: Data(json.utf8))
            XCTAssertEqual(fields.clock, ClockStatus(zone: DemoRouter.timeZone, trusted: true))
            let state = try XCTUnwrap(fields.groups.first(where: { $0.id == "kids" }))
            return (state, fields.held.filter { $0.group == "kids" }.map(\.mac))
        }
        // Thursday 12 November 2026, at noon: bedtime is next, at nine.
        var seen = try kids(try at(11, 12, 12))
        XCTAssertFalse(seen.state.offline)
        XCTAssertEqual(seen.state.next, try at(11, 12, 21))
        XCTAssertEqual(seen.state.nextSchedule, "bed")
        XCTAssertEqual(seen.held, [])
        // At ten that night, and at half past six the morning after.
        for now in [try at(11, 12, 22), try at(11, 13, 6, 30)] {
            seen = try kids(now)
            XCTAssertEqual(seen.state.why, "schedule")
            XCTAssertEqual(seen.state.schedule, "bed")
            XCTAssertEqual(seen.state.until, try at(11, 13, 7))
            XCTAssertEqual(seen.held, [DemoRouter.tablet, DemoRouter.reader])
        }
        // Friday and Saturday have no bedtime: the next is Sunday's.
        seen = try kids(try at(11, 13, 22))
        XCTAssertFalse(seen.state.offline)
        XCTAssertEqual(seen.state.next, try at(11, 15, 21))
        // The night the clocks go forward in Sydney (4 October 2026):
        // bedtime still ends at seven by the clock, nine hours on.
        seen = try kids(try at(10, 3, 12))
        XCTAssertFalse(seen.state.offline)
        seen = try kids(try at(10, 4, 21))
        XCTAssertEqual(seen.state.until, try at(10, 5, 7))
        seen = try kids(try at(10, 1, 21))
        XCTAssertEqual(seen.state.until, try at(10, 2, 7))
        XCTAssertEqual((seen.state.until ?? 0) - (try at(10, 1, 21)), 10 * 3_600)
        // Allowed for half an hour during bedtime: online, and paused
        // again when that is over.
        let night: Int64 = try at(11, 12, 22)
        config.editGroup("kids") { $0.forNow = GroupOverride.allow(until: night + 1_800) }
        seen = try kids(night)
        XCTAssertFalse(seen.state.offline)
        XCTAssertEqual(seen.state.allowedUntil, night + 1_800)
        XCTAssertEqual(seen.state.next, night + 1_800)
        XCTAssertEqual(seen.held, [])
        seen = try kids(night + 1_800)
        XCTAssertEqual(seen.state.why, "schedule")
    }

    /// The sample router's groups, through the calls the app makes.
    func testGroupsInDemoMode() async throws {
        let router = DemoRouter.makeConnection()
        let now = Int64(Date().timeIntervalSince1970)
        var config = try await router.config()
        var status = try await router.status()
        XCTAssertEqual(status.clock, ClockStatus(zone: DemoRouter.timeZone, trusted: true))
        XCTAssertEqual(status.groups?.map(\.id), config.groupList.map(\.id))
        // The kids' devices were paused for a while: until a full hour
        // that is an hour or two away.
        let kids = try XCTUnwrap(status.group("kids"))
        XCTAssertTrue(kids.offline)
        XCTAssertEqual(kids.connected, 2)
        XCTAssertEqual(kids.why, "timer")
        let until = try XCTUnwrap(kids.until)
        XCTAssertEqual(until, config.group("kids")?.forNow.until)
        XCTAssertTrue(until > now + 3_500 && until <= now + 7_200 && until % 3_600 == 0)
        let work = try XCTUnwrap(status.group("work"))
        XCTAssertFalse(work.offline)
        XCTAssertNil(work.next)
        XCTAssertEqual(work.connected, 1)
        // Its three devices are paused with it, and the console by its
        // own switch as well, which is what it says.
        XCTAssertEqual(Set(status.held?.map(\.mac) ?? []), [DemoRouter.tablet, DemoRouter.console, DemoRouter.reader])
        XCTAssertEqual(status.held(DemoRouter.tablet), HeldDevice(mac: DemoRouter.tablet, why: "timer", group: "kids", until: until))
        XCTAssertEqual(status.held(DemoRouter.console)?.why, "device")
        XCTAssertEqual(status.held(DemoRouter.console)?.byGroup, false)
        XCTAssertNil(status.held(DemoRouter.laptop))
        var devices = try await router.devices()
        XCTAssertEqual(devices.first(where: { $0.mac == DemoRouter.tablet })?.group, "kids")
        XCTAssertEqual(devices.first(where: { $0.mac == DemoRouter.laptop })?.group, "work")
        XCTAssertNil(devices.first(where: { $0.mac == DemoRouter.phone })?.group)

        // Resumed: the timer is cleared, and what the schedules say is
        // what holds. Outside bedtime the group is online with bedtime
        // next; inside it, paused by the schedule until it ends.
        config.editGroup("kids") { $0.forNow = GroupOverride() }
        _ = try await router.setConfig(config)
        status = try await router.status()
        let resumed = try XCTUnwrap(status.group("kids"))
        if resumed.offline {
            XCTAssertEqual(resumed.why, "schedule")
            XCTAssertEqual(resumed.schedule, "bed")
            XCTAssertTrue((resumed.until ?? 0) > now && (resumed.until ?? 0) <= now + 10 * 3_600)
            XCTAssertNil(resumed.next)
        } else {
            XCTAssertEqual(resumed.nextSchedule, "bed")
            XCTAssertTrue((resumed.next ?? 0) > now && (resumed.next ?? 0) <= now + 3 * 86_400)
            XCTAssertEqual(status.held?.map(\.mac), [DemoRouter.console])
        }
        // The next start is at nine in the evening, on a school night,
        // by the clock where the sample router is.
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = try XCTUnwrap(TimeZone(identifier: DemoRouter.timeZone))
        if let next = resumed.next {
            let parts = calendar.dateComponents([.hour, .minute, .weekday], from: Date(timeIntervalSince1970: TimeInterval(next)))
            XCTAssertEqual(parts.hour, 21)
            XCTAssertEqual(parts.minute, 0)
            XCTAssertTrue((1...5).contains(parts.weekday ?? 0), "bedtime next starts on weekday \(parts.weekday ?? 0)")
        }

        // Paused for a while again, and one device let through.
        config = try await router.config()
        config.pauseGroup("kids", until: now + 1_800)
        _ = try await router.setConfig(config)
        status = try await router.status()
        XCTAssertEqual(status.held(DemoRouter.reader), HeldDevice(mac: DemoRouter.reader, why: "timer", group: "kids", until: now + 1_800))
        try await router.setDevice(DeviceSettings(mac: DemoRouter.reader, allowUntil: now + 1_800))
        status = try await router.status()
        XCTAssertNil(status.held(DemoRouter.reader))
        XCTAssertNotNil(status.held(DemoRouter.tablet))
        devices = try await router.devices()
        XCTAssertEqual(devices.first(where: { $0.mac == DemoRouter.reader })?.allowUntil, now + 1_800)
        // Pausing the group once more takes back what the one device was
        // allowed: the later wish holds.
        config = try await router.config()
        config.pauseGroup("kids", until: now + 3_600)
        XCTAssertNil(config.devices.first(where: { $0.mac == DemoRouter.reader })?.allowUntil)
        XCTAssertEqual(config.devices.first(where: { $0.mac == DemoRouter.laptop })?.route, "vpn:home")
        try await router.setDevice(DeviceSettings(mac: DemoRouter.reader, allowUntil: now + 3_600))
        // The group's own switch pauses every device in it, let through
        // or not, and has no end.
        config = try await router.config()
        config.editGroup("kids") { $0.paused = true }
        _ = try await router.setConfig(config)
        status = try await router.status()
        XCTAssertEqual(status.group("kids")?.why, "paused")
        XCTAssertNil(status.group("kids")?.until)
        XCTAssertEqual(status.held(DemoRouter.reader), HeldDevice(mac: DemoRouter.reader, why: "paused", group: "kids"))
        XCTAssertEqual(status.held(DemoRouter.tablet), HeldDevice(mac: DemoRouter.tablet, why: "paused", group: "kids"))
        // A device's settings are kept like a real router keeps them,
        // and one with nothing set is not kept at all.
        var stored = try await router.config()
        XCTAssertEqual(stored.devices.first(where: { $0.mac == DemoRouter.reader })?.allowUntil, now + 3_600)
        try await router.setDevice(DeviceSettings(mac: DemoRouter.reader))
        stored = try await router.config()
        XCTAssertNil(stored.devices.first(where: { $0.mac == DemoRouter.reader }))
        stored.editGroup("kids") { $0.paused = false }

        // A group added, with a device moved into it, and removed again.
        stored.groups = stored.groupList + [DeviceGroup(id: "media", name: "Media", icon: "tv")]
        stored.setGroup(of: DemoRouter.reader, to: "media")
        _ = try await router.setConfig(stored)
        status = try await router.status()
        XCTAssertEqual(status.groups?.map(\.id), ["kids", "work", "media"])
        XCTAssertNotNil(status.held(DemoRouter.tablet))
        XCTAssertNil(status.held(DemoRouter.reader))
        devices = try await router.devices()
        XCTAssertEqual(devices.first(where: { $0.mac == DemoRouter.reader })?.group, "media")
        stored = try await router.config()
        stored.removeGroup("media")
        _ = try await router.setConfig(stored)
        devices = try await router.devices()
        XCTAssertNil(devices.first(where: { $0.mac == DemoRouter.reader })?.group)
    }
}
