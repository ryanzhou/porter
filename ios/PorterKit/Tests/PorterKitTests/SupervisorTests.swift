import XCTest
@testable import PorterKit

/// Collects what a supervisor reports, in order.
final class Recorder: @unchecked Sendable {
    private let lock = NSLock()
    private var seenStates: [ConnectionSupervisor.State] = []
    private var seenStatuses = 0
    private var seenIdentities: [(hello: HelloInfo, key: Data)] = []
    private var seenEndpoints: [ControlEndpoints] = []
    private var seenFailures: [Error] = []

    init(_ supervisor: ConnectionSupervisor) {
        Task.detached {
            for await event in supervisor.events {
                self.lock.withLock {
                    switch event {
                    case .state(let state): self.seenStates.append(state)
                    case .router(.status): self.seenStatuses += 1
                    case .router: break
                    case .identified(let hello, let key): self.seenIdentities.append((hello, key))
                    case .endpoints(let endpoints): self.seenEndpoints.append(endpoints)
                    case .failed(let error): self.seenFailures.append(error)
                    }
                }
            }
        }
    }

    var states: [ConnectionSupervisor.State] { lock.withLock { seenStates } }
    var statuses: Int { lock.withLock { seenStatuses } }
    var identities: [(hello: HelloInfo, key: Data)] { lock.withLock { seenIdentities } }
    var endpoints: [ControlEndpoints] { lock.withLock { seenEndpoints } }
    var failures: [Error] { lock.withLock { seenFailures } }
}

/// Waits until `condition` holds; false if it does not within `timeout`.
func eventually(_ timeout: TimeInterval = 5, _ condition: @escaping () async -> Bool) async -> Bool {
    let end = Date().addingTimeInterval(timeout)
    while Date() < end {
        if await condition() { return true }
        try? await Task.sleep(nanoseconds: 3_000_000)
    }
    return await condition()
}

func pause(_ seconds: TimeInterval) async {
    try? await Task.sleep(nanoseconds: UInt64(seconds * 1_000_000_000))
}

/// The supervisor's behaviour, against a router that speaks the real
/// protocol over links that fail the way real links do. Intervals are a
/// fraction of the app's so that the tests take seconds.
final class SupervisorTests: XCTestCase {
    typealias State = ConnectionSupervisor.State

    static let home = "10.83.0.1"

    /// The app's timing, shrunk. A test changes what it is about.
    static var quick: ConnectionSupervisor.Timing {
        var t = ConnectionSupervisor.Timing()
        t.networkConnect = 0.3
        t.networkSecure = 0.5
        t.bluetoothSecure = 1
        t.upgradeEvery = 30
        t.heartbeatEvery = 0.05
        t.heartbeatWithin = 0.25
        t.callWait = 2
        t.drain = 5
        t.noticeAfter = 1
        t.giveUpAfter = 3
        t.retryEvery = 0.05
        t.searchEvery = 0.1
        t.pairingEvery = 0.05
        return t
    }

    /// A router, a phone paired with it, and the ways between them.
    final class Rig {
        let router: FakeRouter
        let dialer = FakeDialer()
        let phone = Identity()
        var supervisor: ConnectionSupervisor!
        var recorder: Recorder!

        init(paired: Bool = true, router: FakeRouter = FakeRouter()) {
            self.router = router
            if paired { router.pair(phone) }
        }

        /// The phone leaves the router's network.
        func leaveNetwork() {
            dialer.setHost(SupervisorTests.home, nil)
        }

        /// Both ways work; the network is the faster, as it is.
        func bothWork() {
            dialer.setHost(SupervisorTests.home, .router(router, after: 0.01))
            dialer.setBluetooth(.router(router, after: 0.08))
        }

        func start(
            knownKey: Bool = true, endpoints: ControlEndpoints? = ControlEndpoints(port: 7678, addrs: [SupervisorTests.home]),
            automatic: Bool = true, timing: ConnectionSupervisor.Timing = SupervisorTests.quick
        ) async {
            supervisor = ConnectionSupervisor(
                identity: phone, routerKey: knownKey ? router.identity.publicKey : nil, endpoints: endpoints,
                dialer: dialer, phoneName: "Test phone", automatic: automatic, timing: timing)
            recorder = Recorder(supervisor)
            await supervisor.start()
        }

        func reaches(_ state: ConnectionSupervisor.State, within timeout: TimeInterval = 5) async -> Bool {
            await eventually(timeout) { self.recorder.states.last == state }
        }

        func stop() async {
            await supervisor.stop()
        }
    }

    // MARK: 1. The start

    func testStartUsesTheLinkThatAnswersFirst() async throws {
        // The network answers first: it is used, and the Bluetooth attempt
        // is not left behind.
        let rig = Rig()
        rig.bothWork()
        await rig.start()
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)
        XCTAssertEqual(rig.recorder.states, [.connected(.network)])
        let status = try await rig.supervisor.status()
        XCTAssertEqual(status.name, "Porter")
        XCTAssertEqual(rig.router.count("status.get", over: .network), 1)
        let alone = await eventually { rig.router.liveLinks.count == 1 && rig.dialer.links(.bluetooth).allSatisfy(\.wasClosedByApp) }
        XCTAssertTrue(alone, "the attempt that lost the race is still open")
        XCTAssertEqual(rig.dialer.attempts(.bluetooth), 1)
        XCTAssertEqual(rig.dialer.attempts(.network), 1)
        await rig.stop()
    }

    func testStartOverBluetoothWhenTheNetworkDoesNotAnswer() async throws {
        let rig = Rig()
        rig.dialer.setBluetooth(.router(rig.router, after: 0.02))
        await rig.start()
        let up = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(up)
        XCTAssertEqual(rig.recorder.states, [.connecting, .connected(.bluetooth)])
        _ = try await rig.supervisor.status()
        XCTAssertEqual(rig.router.count("status.get", over: .bluetooth), 1)
        XCTAssertTrue(rig.recorder.failures.isEmpty)
        await rig.stop()
    }

    func testBluetoothFirstWithTheNetworkOnItsWayMovesOverAtOnce() async throws {
        // Bluetooth wins while the network attempt is still under way.
        // That is not the network failing: it is tried again straight
        // away, not after the half minute between looks.
        let rig = Rig()
        rig.dialer.setBluetooth(.router(rig.router, after: 0.01))
        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.25))
        await rig.start()
        let up = await rig.reaches(.connected(.network), within: 3)
        XCTAssertTrue(up)
        XCTAssertEqual(rig.recorder.states, [.connecting, .connected(.bluetooth), .connected(.network)])
        await rig.stop()
    }

    // MARK: 2. From Bluetooth to the network

    func testMovesToTheNetworkWithoutAGap() async throws {
        let rig = Rig()
        rig.dialer.setBluetooth(.router(rig.router, after: 0.01))
        var timing = Self.quick
        timing.upgradeEvery = 0.1
        await rig.start(timing: timing)
        let onBluetooth = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(onBluetooth)
        let bluetooth = try XCTUnwrap(rig.dialer.links(.bluetooth).last)

        // One call is under way on Bluetooth and stays so...
        let gate = Gate()
        rig.router.onCall { method, link in
            if method == "devices.list" && link.kind == .bluetooth { await gate.wait() }
            return .answer
        }
        let supervisor: ConnectionSupervisor = rig.supervisor
        let held = Task { try await supervisor.devices() }
        let waiting = await eventually { gate.arrivals == 1 }
        XCTAssertTrue(waiting)
        // ...and calls keep being made the whole time.
        let stream = Task { () -> (made: Int, failed: Int) in
            var made = 0
            var failed = 0
            while !Task.isCancelled {
                do {
                    _ = try await supervisor.status()
                    made += 1
                } catch {
                    failed += 1
                }
                await pause(0.002)
            }
            return (made, failed)
        }
        await pause(0.05)

        // The phone joins the router's network. Found on the timer.
        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.01))
        let moved = await rig.reaches(.connected(.network))
        XCTAssertTrue(moved)

        // New calls go over the network from here on.
        await pause(0.05)
        let overBluetooth: Int = rig.router.count("status.get", over: .bluetooth)
        let overNetwork: Int = rig.router.count("status.get", over: .network)
        await pause(0.1)
        XCTAssertEqual(rig.router.count("status.get", over: .bluetooth), overBluetooth, "calls still go over Bluetooth")
        XCTAssertGreaterThan(rig.router.count("status.get", over: .network), overNetwork)

        // The old link is kept for the call that is still on it, which
        // finishes there and is not made a second time...
        XCTAssertFalse(bluetooth.isDown, "the old link was dropped under a call")
        // While both links are up, an event the router sends to both is
        // taken once.
        let heard: Int = rig.recorder.statuses
        rig.router.pushStatus()
        let once = await eventually { rig.recorder.statuses == heard + 1 }
        XCTAssertTrue(once)
        await pause(0.05)
        XCTAssertEqual(rig.recorder.statuses, heard + 1, "an event was taken from both links")
        gate.release()
        let devices = try await held.value
        XCTAssertEqual(devices.count, 5)
        XCTAssertEqual(rig.router.count("devices.list", over: .bluetooth), 1)
        XCTAssertEqual(rig.router.count("devices.list", over: .network), 0)
        // ...and is let go once that is done.
        let released = await eventually { bluetooth.wasClosedByApp }
        XCTAssertTrue(released, "the old link was never let go")

        stream.cancel()
        let calls = await stream.value
        XCTAssertEqual(calls.failed, 0, "calls failed while the link changed")
        XCTAssertGreaterThan(calls.made, 20)
        // Nothing but "connected" was ever reported.
        XCTAssertEqual(rig.recorder.states, [.connecting, .connected(.bluetooth), .connected(.network)])
        XCTAssertTrue(rig.recorder.failures.isEmpty)
        await rig.stop()
    }

    func testTheNetworkIsTriedWhenTheAppComesToTheFront() async throws {
        let rig = Rig()
        rig.dialer.setBluetooth(.router(rig.router, after: 0.01))
        await rig.start() // looks again every 30 s
        let onBluetooth = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(onBluetooth)
        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.01))
        await pause(0.2)
        XCTAssertEqual(rig.recorder.states.last, .connected(.bluetooth))
        await rig.supervisor.poke()
        let moved = await rig.reaches(.connected(.network), within: 2)
        XCTAssertTrue(moved)
        await rig.stop()
    }

    func testNewAddressesAreTriedAtOnce() async throws {
        // The router's network was renumbered: the address remembered is
        // dead, and the status it sends names the new one.
        let rig = Rig()
        rig.dialer.setBluetooth(.router(rig.router, after: 0.01))
        await rig.start() // looks again every 30 s
        let onBluetooth = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(onBluetooth)
        XCTAssertTrue(rig.recorder.endpoints.isEmpty)
        let moved = ControlEndpoints(port: 7678, addrs: ["10.90.0.1"])
        rig.dialer.setHost("10.90.0.1", .router(rig.router, after: 0.01))
        rig.router.move(to: moved)
        let up = await rig.reaches(.connected(.network), within: 2)
        XCTAssertTrue(up)
        XCTAssertEqual(rig.recorder.endpoints, [moved])
        XCTAssertEqual(rig.dialer.links(.network).last?.host, "10.90.0.1")
        await rig.stop()
    }

    // MARK: 3. Losing the network link

    func testLostNetworkLinkIsReplacedUnderTheCalls() async throws {
        let rig = Rig()
        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.01))
        rig.dialer.setBluetooth(.router(rig.router, after: 0.3))
        await rig.start()
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)
        let network = try XCTUnwrap(rig.dialer.links(.network).last)

        // A read and an action are under way when the link breaks.
        let gate = Gate()
        rig.router.onCall { method, link in
            if link.kind == .network && (method == "devices.list" || method == "system.reboot") { await gate.wait() }
            return .answer
        }
        let supervisor: ConnectionSupervisor = rig.supervisor
        let read = Task { try await supervisor.devices() }
        let action = Task { try await supervisor.reboot() }
        let waiting = await eventually { gate.arrivals == 2 }
        XCTAssertTrue(waiting)

        // Out of Wi-Fi range: the network is gone, Bluetooth takes a
        // moment to come up.
        rig.dialer.setHost(Self.home, nil)
        network.cut()
        let noticed = await rig.reaches(.reconnecting(noticeable: false), within: 1)
        XCTAssertTrue(noticed)
        // A call made now, with no link at all, waits for the next one.
        let during = Task { try await supervisor.status() }

        let devices = try await read.value
        XCTAssertEqual(devices.count, 5, "the read that died with the link was not made again")
        let status = try await during.value
        XCTAssertEqual(status.name, "Porter")
        do {
            try await action.value
            XCTFail("an action that died with the link was reported as done")
        } catch {
            XCTAssertTrue(isLinkFailure(error), "\(error)")
        }
        gate.release()
        await pause(0.1)
        // The read went out again once, over Bluetooth; the action did
        // not go out a second time.
        XCTAssertEqual(rig.router.count("devices.list", over: .bluetooth), 1)
        XCTAssertEqual(rig.router.count("status.get", over: .bluetooth), 1)
        XCTAssertEqual(rig.router.count("system.reboot"), 1)
        XCTAssertEqual(rig.router.count("system.reboot", over: .bluetooth), 0)
        // Never "searching", never stopped: the screen stays.
        XCTAssertEqual(rig.recorder.states, [.connected(.network), .reconnecting(noticeable: false), .connected(.bluetooth)])
        XCTAssertTrue(rig.recorder.failures.isEmpty)
        await rig.stop()
    }

    func testSilentNetworkLinkIsNoticedByTheHeartbeat() async throws {
        let rig = Rig()
        rig.bothWork()
        await rig.start()
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)
        let network = try XCTUnwrap(rig.dialer.links(.network).last)

        // A link that works is asked regularly and left alone.
        await pause(0.4)
        XCTAssertGreaterThanOrEqual(rig.router.count("ping", over: .network), 3)
        XCTAssertEqual(rig.recorder.states, [.connected(.network)])

        // Nothing arrives any more, and no error says so.
        rig.dialer.setHost(Self.home, nil)
        let started = Date()
        network.goSilent()
        let noticed = await eventually(3) { rig.recorder.states.contains(.reconnecting(noticeable: false)) }
        XCTAssertTrue(noticed)
        // Within one interval and one timeout (0.05 s + 0.25 s), with
        // room for a busy machine.
        XCTAssertLessThan(Date().timeIntervalSince(started), 1.5)
        let back = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(back)
        _ = try await rig.supervisor.status()
        await rig.stop()
    }

    func testReconnectsOverTheNetworkWhenItAnswersAgain() async throws {
        let rig = Rig()
        rig.bothWork()
        await rig.start()
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)
        // The connection breaks; the network itself is still there.
        try XCTUnwrap(rig.dialer.links(.network).last).cut()
        let back = await eventually { rig.recorder.states == [.connected(.network), .reconnecting(noticeable: false), .connected(.network)] }
        XCTAssertTrue(back, "\(rig.recorder.states)")
        XCTAssertEqual(rig.dialer.links(.network).count, 2)
        _ = try await rig.supervisor.status()
        await rig.stop()
    }

    func testNetworkIsTriedAgainWhileBluetoothIsStillTrying() async throws {
        // The router is out of Bluetooth range, as it is in a cupboard:
        // an attempt over Bluetooth neither succeeds nor fails for a long
        // time. That must not hold up the network, which is the link that
        // comes back.
        let rig = Rig()
        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.01))
        rig.dialer.setBluetooth(.hangs)
        await rig.start()
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)
        // Wi-Fi drops for a moment.
        rig.leaveNetwork()
        try XCTUnwrap(rig.dialer.links(.network).last).cut()
        let lost = await rig.reaches(.reconnecting(noticeable: false), within: 1)
        XCTAssertTrue(lost)
        await pause(0.3)
        let before: Int = rig.dialer.attempts(.network)
        XCTAssertGreaterThanOrEqual(before, 3, "the network was not tried again while Bluetooth was still trying")
        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.01))
        let started = Date()
        let back = await rig.reaches(.connected(.network), within: 2)
        XCTAssertTrue(back)
        XCTAssertLessThan(Date().timeIntervalSince(started), 1)
        XCTAssertEqual(rig.recorder.states, [.connected(.network), .reconnecting(noticeable: false), .connected(.network)])
        // One attempt over Bluetooth at the start and one after the loss,
        // each ended when the network won.
        XCTAssertEqual(rig.dialer.attempts(.bluetooth), 2)
        await rig.stop()
    }

    // MARK: 4. Giving up

    func testGivesUpOnlyAfterNothingAnswersForAWhile() async throws {
        let rig = Rig()
        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.01))
        var timing = Self.quick
        timing.noticeAfter = 0.2
        timing.giveUpAfter = 0.8
        timing.callWait = 0.3
        await rig.start(timing: timing)
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)

        // The router is unplugged.
        rig.dialer.setHost(Self.home, nil)
        let started = Date()
        try XCTUnwrap(rig.dialer.links(.network).last).cut()

        // First nothing shows, then the quiet sign, and only after the
        // whole bound "searching".
        let noticeable = await rig.reaches(.reconnecting(noticeable: true), within: 2)
        XCTAssertTrue(noticeable)
        XCTAssertGreaterThanOrEqual(Date().timeIntervalSince(started), 0.19)
        // A call made meanwhile waits its time for a link, then fails.
        let asked = Date()
        do {
            _ = try await rig.supervisor.status()
            XCTFail("a call was answered with no link")
        } catch {
            XCTAssertEqual(error as? SupervisorError, .notConnected)
            XCTAssertGreaterThanOrEqual(Date().timeIntervalSince(asked), 0.25)
        }
        let gaveUp = await rig.reaches(.searching, within: 3)
        XCTAssertTrue(gaveUp)
        XCTAssertGreaterThanOrEqual(Date().timeIntervalSince(started), 0.79)
        XCTAssertEqual(rig.recorder.states, [
            .connected(.network), .reconnecting(noticeable: false), .reconnecting(noticeable: true), .searching,
        ])
        // Now there is nothing to wait for.
        let again = Date()
        do {
            _ = try await rig.supervisor.status()
            XCTFail("a call was answered with no link")
        } catch {
            XCTAssertEqual(error as? SupervisorError, .notConnected)
            XCTAssertLessThan(Date().timeIntervalSince(again), 0.2)
        }
        // It is not an error, and the supervisor goes on looking: when
        // the router is back it is found without anyone asking.
        XCTAssertTrue(rig.recorder.failures.isEmpty)
        rig.dialer.setBluetooth(.router(rig.router, after: 0.01))
        let found = await rig.reaches(.connected(.bluetooth), within: 3)
        XCTAssertTrue(found)
        XCTAssertEqual(Array(rig.recorder.states.suffix(3)), [.searching, .connecting, .connected(.bluetooth)])
        await rig.stop()
    }

    // MARK: 5. Without Bluetooth

    func testWithBluetoothOffTheNetworkIsEnough() async throws {
        let rig = Rig()
        rig.dialer.setBluetooth(.absent(RouterNotFound(FakeError.bluetoothOff)))
        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.01))
        await rig.start()
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)
        _ = try await rig.supervisor.status()
        // And a lost link comes back the same way.
        try XCTUnwrap(rig.dialer.links(.network).last).cut()
        let back = await eventually { rig.recorder.states == [.connected(.network), .reconnecting(noticeable: false), .connected(.network)] }
        XCTAssertTrue(back, "\(rig.recorder.states)")
        _ = try await rig.supervisor.status()
        XCTAssertTrue(rig.recorder.failures.isEmpty)
        await rig.stop()
    }

    // MARK: 6. After a change of link

    func testEventsComeFromTheLinkInUse() async throws {
        let rig = Rig()
        rig.dialer.setBluetooth(.router(rig.router, after: 0.01))
        await rig.start()
        let onBluetooth = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(onBluetooth)
        rig.router.pushStatus()
        let first = await eventually { rig.recorder.statuses == 1 }
        XCTAssertTrue(first)

        rig.dialer.setHost(Self.home, .router(rig.router, after: 0.01))
        await rig.supervisor.poke()
        let moved = await rig.reaches(.connected(.network))
        XCTAssertTrue(moved)
        // Every "connected" is a new link, which is what tells the app to
        // bring its picture up to date; each came with the router's name
        // and the key it proved.
        XCTAssertEqual(rig.recorder.states.filter { $0 == .connected(.bluetooth) || $0 == .connected(.network) }.count, 2)
        XCTAssertEqual(rig.recorder.identities.map(\.key), [rig.router.identity.publicKey, rig.router.identity.publicKey])
        XCTAssertEqual(rig.recorder.identities.map(\.hello.name), ["Porter", "Porter"])
        // An event now arrives once: over the new link, and not also over
        // the old one.
        rig.router.pushStatus()
        let second = await eventually { rig.recorder.statuses == 2 }
        XCTAssertTrue(second)
        await pause(0.1)
        XCTAssertEqual(rig.recorder.statuses, 2)
        await rig.stop()
    }

    // MARK: 3 (writes). A write whose answer was lost

    private func writeRig() async throws -> (Rig, RouterConfig) {
        let rig = Rig()
        rig.bothWork()
        await rig.start()
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)
        return (rig, try await rig.supervisor.config())
    }

    private static let extraRule = PolicyRule(id: "extra", name: "Extra", src: RuleSource(), dst: RuleDestination(ports: ["25"]), action: RouteAction.block.raw)

    func testWriteThatLandedBeforeTheLinkWentIsNotMadeTwice() async throws {
        let (rig, known) = try await writeRig()
        // The router makes the change; the link breaks before it can say so.
        rig.router.onCall { method, link in
            guard method == "config.set" && link.kind == .network else { return .answer }
            rig.leaveNetwork()
            return .cutBeforeReply
        }
        // An edit that would show if it were made twice.
        let fresh = try await rig.supervisor.writeConfig(known: known) { $0.policy.rules.append(Self.extraRule) }
        XCTAssertEqual(fresh.policy.rules.filter { $0.id == "extra" }.count, 1)
        XCTAssertEqual(fresh.policy.rules.count, known.policy.rules.count + 1)
        XCTAssertEqual(fresh.rev, known.rev + 1)
        XCTAssertEqual(rig.router.currentConfig, fresh)
        // Asked twice, over one link and then the other; made once.
        XCTAssertEqual(rig.router.count("config.set", over: .network), 1)
        XCTAssertEqual(rig.router.count("config.set", over: .bluetooth), 1)
        XCTAssertEqual(rig.recorder.states, [.connected(.network), .reconnecting(noticeable: false), .connected(.bluetooth)])
        await rig.stop()
    }

    func testWriteThatDidNotLandIsMadeOnce() async throws {
        let (rig, known) = try await writeRig()
        // The link breaks with the write on its way; the router never
        // sees it.
        rig.router.onCall { method, link in
            guard method == "config.set" && link.kind == .network else { return .answer }
            rig.leaveNetwork()
            return .cutBeforeRequest
        }
        let fresh = try await rig.supervisor.writeConfig(known: known) { $0.policy.rules.append(Self.extraRule) }
        XCTAssertEqual(fresh.policy.rules.filter { $0.id == "extra" }.count, 1)
        XCTAssertEqual(fresh.rev, known.rev + 1)
        XCTAssertEqual(rig.router.currentConfig, fresh)
        XCTAssertEqual(rig.router.count("config.set"), 1)
        await rig.stop()
    }

    func testWriteThatDidNotLandIsMadeOnTopOfSomeoneElsesChange() async throws {
        let (rig, known) = try await writeRig()
        // The write is lost, and before it is sent again another phone
        // changes something else.
        let router: FakeRouter = rig.router
        rig.router.onCall { method, link in
            guard method == "config.set" && link.kind == .network else { return .answer }
            rig.leaveNetwork()
            router.edit { $0.lan.domain = "home" }
            return .cutBeforeRequest
        }
        let fresh = try await rig.supervisor.writeConfig(known: known) { $0.system.name = "Renamed" }
        XCTAssertEqual(fresh.system.name, "Renamed")
        XCTAssertEqual(fresh.lan.domain, "home", "the other phone's change was overwritten")
        XCTAssertEqual(fresh.rev, known.rev + 2)
        await rig.stop()
    }

    func testWriteThatLandedIsRecognisedEvenIfTheRouterForgot() async throws {
        let (rig, known) = try await writeRig()
        // The router makes the change, the answer is lost, and the router
        // restarts before the write is sent again: it no longer knows the
        // write by its id. What it holds shows that the change is there.
        let router: FakeRouter = rig.router
        rig.router.onCall { method, link in
            guard method == "config.set" else { return .answer }
            if link.kind == .network {
                rig.leaveNetwork()
                return .cutBeforeReply
            }
            router.forgetWrites()
            return .answer
        }
        let fresh = try await rig.supervisor.writeConfig(known: known) { $0.system.name = "Renamed" }
        XCTAssertEqual(fresh.system.name, "Renamed")
        XCTAssertEqual(fresh.rev, known.rev + 1, "the change was written a second time")
        await rig.stop()
    }

    func testWriteFailsWhenNoLinkComesBack() async throws {
        let (rig, known) = try await writeRig()
        rig.router.onCall { method, _ in method == "config.set" ? .cutBeforeRequest : .answer }
        rig.leaveNetwork()
        rig.dialer.setBluetooth(.absent(RouterNotFound(FakeError.outOfRange)))
        do {
            _ = try await rig.supervisor.writeConfig(known: known) { $0.system.name = "Renamed" }
            XCTFail("a write that reached nobody was reported as made")
        } catch {
            XCTAssertEqual(error as? SupervisorError, .notConnected)
        }
        XCTAssertEqual(rig.router.currentConfig, known)
        await rig.stop()
    }

    func testWriteOnAStaleCopyIsMadeOnAFreshOne() async throws {
        // No link trouble at all: the router has moved on from the copy
        // the app holds.
        let (rig, known) = try await writeRig()
        rig.router.edit { $0.lan.domain = "home" }
        let fresh = try await rig.supervisor.writeConfig(known: known) { $0.system.name = "Renamed" }
        XCTAssertEqual(fresh.system.name, "Renamed")
        XCTAssertEqual(fresh.lan.domain, "home")
        XCTAssertEqual(rig.router.count("config.set"), 2)
        await rig.stop()
    }

    // MARK: The pinned key

    func testAnotherRouterOnTheNetworkIsRefusedAndBluetoothStays() async throws {
        // At the address remembered for the router something else answers
        // that speaks the protocol: another Porter on a network numbered
        // the same, or an impostor. It even knows this phone.
        let rig = Rig()
        let other = FakeRouter(name: "Other")
        other.pair(rig.phone)
        rig.dialer.setHost(Self.home, .router(other, after: 0.01))
        rig.dialer.setBluetooth(.router(rig.router, after: 0.05))
        var timing = Self.quick
        timing.upgradeEvery = 0.08
        await rig.start(timing: timing)
        let up = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(up)
        let bluetooth = try XCTUnwrap(rig.dialer.links(.bluetooth).last)

        // The network is tried again and again, and refused each time...
        let tried = await eventually { rig.dialer.attempts(.network) >= 4 }
        XCTAssertTrue(tried)
        // ...without the session over Bluetooth noticing.
        XCTAssertEqual(rig.recorder.states, [.connecting, .connected(.bluetooth)])
        XCTAssertFalse(bluetooth.isDown)
        let status = try await rig.supervisor.status()
        XCTAssertEqual(status.name, "Porter")
        XCTAssertTrue(rig.recorder.failures.isEmpty)
        // The other router was never asked anything, nor shown this
        // phone's key: the handshake stops when its key is seen.
        XCTAssertTrue(other.calls.isEmpty)
        XCTAssertTrue(other.liveLinks.isEmpty)
        let dropped = await eventually { rig.dialer.links(.network).allSatisfy(\.isDown) }
        XCTAssertTrue(dropped)
        await rig.stop()
    }

    func testAnotherRouterOverBluetoothEndsTheAttempt() async throws {
        let rig = Rig()
        let other = FakeRouter(name: "Other")
        other.pair(rig.phone)
        rig.dialer.setBluetooth(.router(other, after: 0.01))
        await rig.start()
        let stopped = await rig.reaches(.stopped)
        XCTAssertTrue(stopped)
        XCTAssertEqual(rig.recorder.failures.count, 1)
        XCTAssertEqual(rig.recorder.failures.first as? SessionError, .routerKeyMismatch)
        XCTAssertTrue(other.calls.isEmpty)
    }

    // MARK: Pairing, and routers that cannot be reached over the network

    func testPairingIsOverBluetoothAndTheKeyIsPinnedFromThere() async throws {
        let rig = Rig(paired: false)
        rig.bothWork()
        // A router this phone has never met: no key, no addresses.
        await rig.start(knownKey: false, endpoints: nil, automatic: false)
        let waiting = await rig.reaches(.awaitingPairing(name: "Porter"))
        XCTAssertTrue(waiting)
        XCTAssertEqual(rig.dialer.attempts(.network), 0)
        await pause(0.2)
        XCTAssertGreaterThanOrEqual(rig.router.count("pair", over: .bluetooth), 2)

        // The button is pressed.
        rig.router.openPairing()
        // Paired over Bluetooth; and now that the router has said where
        // it is, the app moves to the network by itself.
        let moved = await rig.reaches(.connected(.network))
        XCTAssertTrue(moved)
        XCTAssertEqual(rig.recorder.states, [
            .connecting, .awaitingPairing(name: "Porter"), .connected(.bluetooth), .connected(.network),
        ])
        XCTAssertEqual(rig.router.count("pair", over: .network), 0)
        let first = try XCTUnwrap(rig.recorder.identities.first)
        XCTAssertTrue(first.hello.paired)
        XCTAssertEqual(first.key, rig.router.identity.publicKey)
        XCTAssertEqual(rig.recorder.endpoints, [ControlEndpoints(port: 7678, addrs: [Self.home])])
        await rig.stop()
    }

    func testPhoneThatIsNotPairedGetsNowhereOverTheNetwork() async throws {
        // The router has removed this phone. Over the network it is cut
        // off after the handshake; over Bluetooth it may ask to pair.
        let rig = Rig(paired: false)
        rig.bothWork()
        await rig.start()
        let waiting = await rig.reaches(.awaitingPairing(name: "Porter"))
        XCTAssertTrue(waiting)
        XCTAssertEqual(rig.router.calls.filter { $0.over == .network }, [])
        await rig.stop()
    }

    func testOlderRouterIsUsedOverBluetoothAsBefore() async throws {
        // A router from before any of this: its hello and its status say
        // nothing about the network.
        let old = FakeRouter(control: nil)
        let rig = Rig(router: old)
        rig.dialer.setBluetooth(.router(old, after: 0.01))
        var timing = Self.quick
        timing.upgradeEvery = 0.05
        await rig.start(endpoints: nil, timing: timing)
        let up = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(up)
        old.pushStatus()
        await pause(0.3)
        let status = try await rig.supervisor.status()
        XCTAssertNil(status.control)
        XCTAssertEqual(rig.dialer.attempts(.network), 0)
        XCTAssertEqual(rig.recorder.states, [.connecting, .connected(.bluetooth)])
        XCTAssertTrue(rig.recorder.failures.isEmpty)
        XCTAssertTrue(rig.recorder.endpoints.isEmpty)
        await rig.stop()
    }

    func testRouterThatStopsListeningIsNoLongerTried() async throws {
        // Set to Bluetooth only since the phone last saw it.
        let rig = Rig(router: FakeRouter(control: ControlEndpoints()))
        rig.dialer.setBluetooth(.router(rig.router, after: 0.01))
        var timing = Self.quick
        timing.upgradeEvery = 0.05
        await rig.start(timing: timing)
        let up = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(up)
        await pause(0.3)
        // Tried once, at the start, at the address remembered; then the
        // router said there is none, which the app is told to remember.
        XCTAssertEqual(rig.dialer.attempts(.network), 1)
        XCTAssertEqual(rig.recorder.endpoints, [ControlEndpoints()])
        await rig.stop()
    }

    // MARK: What is told, and what is not

    func testAnAttemptThatWasAskedForSaysWhenTheRouterIsNotThere() async throws {
        let rig = Rig()
        await rig.start(automatic: false)
        let stopped = await rig.reaches(.stopped)
        XCTAssertTrue(stopped)
        XCTAssertEqual(rig.recorder.failures.count, 1)
        XCTAssertTrue(rig.recorder.failures.first is RouterNotFound)
    }

    func testAnAttemptNobodyAskedForKeepsQuietAndKeepsLooking() async throws {
        let rig = Rig()
        await rig.start()
        await pause(0.5)
        XCTAssertTrue(rig.recorder.states.isEmpty, "\(rig.recorder.states)")
        XCTAssertTrue(rig.recorder.failures.isEmpty)
        XCTAssertGreaterThanOrEqual(rig.dialer.attempts(.bluetooth), 3)
        // Found as soon as it is there.
        rig.dialer.setBluetooth(.router(rig.router, after: 0.01))
        let up = await rig.reaches(.connected(.bluetooth))
        XCTAssertTrue(up)
        await rig.stop()
    }

    func testRouterThatIsThereButWillNotConnectIsToldAfterFiveTries() async throws {
        let rig = Rig()
        rig.dialer.setBluetooth(.absent(FakeError.stuck))
        await rig.start()
        let stopped = await rig.reaches(.stopped)
        XCTAssertTrue(stopped)
        XCTAssertEqual(rig.dialer.attempts(.bluetooth), 5)
        XCTAssertEqual(rig.recorder.failures.first as? FakeError, .stuck)
    }

    func testStopEndsEverything() async throws {
        let rig = Rig()
        rig.bothWork()
        await rig.start()
        let up = await rig.reaches(.connected(.network))
        XCTAssertTrue(up)
        await rig.stop()
        let stopped = await rig.reaches(.stopped)
        XCTAssertTrue(stopped)
        do {
            _ = try await rig.supervisor.status()
            XCTFail("a call was answered after the end")
        } catch {
            XCTAssertEqual(error as? SupervisorError, .notConnected)
        }
        let closed = await eventually { rig.dialer.links(.network).allSatisfy(\.wasClosedByApp) && rig.router.liveLinks.isEmpty }
        XCTAssertTrue(closed)
    }

    // MARK: 7. Demo mode

    func testDemoRouterBehindASupervisor() async throws {
        let supervisor = ConnectionSupervisor(demo: DemoRouter.makeConnection())
        let recorder = Recorder(supervisor)
        await supervisor.start()
        let up = await eventually { recorder.states == [.connected(.network)] }
        XCTAssertTrue(up)
        let hello = try await supervisor.hello()
        XCTAssertEqual(hello.control, ControlEndpoints(port: 7678, addrs: ["10.83.0.1", "fd4c:91a7:20be:1::1"]))
        try await supervisor.ping()
        let known = try await supervisor.config()
        XCTAssertEqual(known.system.bluetoothOnly, false)
        // The switch in Settings, as the app makes it.
        let fresh = try await supervisor.writeConfig(known: known) { $0.system.bluetoothOnly = true }
        XCTAssertEqual(fresh.system.bluetoothOnly, true)
        let status = try await supervisor.status()
        XCTAssertEqual(status.control, ControlEndpoints())
    }
}

/// What the app keeps and reads has to survive the versions before this
/// one.
final class NetworkControlModelTests: XCTestCase {
    func testRouterRememberedBeforeNetworkControlStillDecodes() throws {
        let peripheral = UUID()
        let old = """
        [{"id":"3fa9c210","name":"Porter","publicKey":"\(Data(repeating: 7, count: 32).base64EncodedString())","peripheral":"\(peripheral.uuidString)"}]
        """
        let routers = try JSONDecoder().decode([KnownRouter].self, from: Data(old.utf8))
        XCTAssertEqual(routers.count, 1)
        XCTAssertEqual(routers[0].peripheral, peripheral)
        XCTAssertEqual(routers[0].endpoints, ControlEndpoints())
        XCTAssertFalse(routers[0].endpoints.isUsable)

        // And what is stored now comes back whole.
        var router = routers[0]
        router.endpoints = ControlEndpoints(port: 7678, addrs: ["10.83.0.1", "fd00::1"])
        let again = try JSONDecoder().decode(KnownRouter.self, from: try JSONEncoder().encode(router))
        XCTAssertEqual(again, router)
        XCTAssertTrue(again.endpoints.isUsable)
    }

    func testKeyStoreKeepsEndpoints() throws {
        let suite = "porter.tests.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let store = KeyStore(defaults: defaults)
        var router = KnownRouter(id: "3fa9c210", name: "Porter", publicKey: Data(repeating: 7, count: 32), peripheral: UUID())
        store.save(router)
        XCTAssertEqual(store.routers(), [router])
        router.endpoints = ControlEndpoints(port: 7678, addrs: ["10.83.0.1"])
        store.save(router)
        XCTAssertEqual(store.routers(), [router])
    }

    func testAnswersWithoutTheNewFieldsDecode() throws {
        let decoder = RouterConnection.makeDecoder()
        let hello = try decoder.decode(HelloInfo.self, from: Data(#"{"name":"Porter","id":"3fa9c210","version":"v0.9","paired":true,"claimed":true,"pairing":false,"ready":true}"#.utf8))
        XCTAssertNil(hello.control)
        let system = try decoder.decode(SystemConfig.self, from: Data(#"{"name":"Porter","country":"AU","ssh":{"enabled":false,"authorized_keys":[]}}"#.utf8))
        XCTAssertNil(system.bluetoothOnly)
        let current = try decoder.decode(SystemConfig.self, from: Data(#"{"name":"Porter","country":"AU","ssh":{"enabled":false,"authorized_keys":[]},"bluetooth_only":true}"#.utf8))
        XCTAssertEqual(current.bluetoothOnly, true)
        // Written back, the field keeps its name.
        let written = String(decoding: try RouterConnection.makeEncoder().encode(current), as: UTF8.self)
        XCTAssertTrue(written.contains("\"bluetooth_only\":true"), written)
        XCTAssertEqual(RouterConfig.format, 3)
    }
}
