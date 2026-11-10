#if canImport(CoreBluetooth)
import CoreBluetooth
import Foundation

public enum PorterUUID {
    public static let service = CBUUID(string: "7E0D0001-5C3A-4B8E-9F1D-706F72746572")
    public static let rx = CBUUID(string: "7E0D0002-5C3A-4B8E-9F1D-706F72746572")
    public static let tx = CBUUID(string: "7E0D0003-5C3A-4B8E-9F1D-706F72746572")
    public static let info = CBUUID(string: "7E0D0004-5C3A-4B8E-9F1D-706F72746572")
}

public enum BLEError: Error, LocalizedError {
    case bluetoothUnavailable(String)
    case connectionFailed(String)
    case serviceMissing
    case disconnected
    case timeout

    public var errorDescription: String? {
        switch self {
        case .bluetoothUnavailable(let why): return why
        case .connectionFailed(let why): return "Could not connect to the router: \(why)"
        case .serviceMissing: return "This device is not a Porter router."
        case .disconnected: return "The Bluetooth connection to the router was lost."
        case .timeout: return "The router did not answer in time."
        }
    }
}

/// A router seen while scanning.
public struct DiscoveredRouter: Identifiable, Equatable {
    public let id: UUID
    public var name: String
    public var rssi: Int
    public var lastSeen: Date
}

/// Scans for routers and opens links to them. All CoreBluetooth work
/// happens on the main queue.
@MainActor
public final class BLECentral: NSObject, ObservableObject {
    @Published public private(set) var state: CBManagerState = .unknown
    @Published public private(set) var discovered: [DiscoveredRouter] = []

    /// Created on first use, so that an app that never scans (demo mode)
    /// never touches Bluetooth or asks for permission.
    private var central: CBCentralManager?
    private var peripherals: [UUID: CBPeripheral] = [:]
    private var links: [UUID: BLELink] = [:]
    private var wantsScan = false

    public override init() {
        super.init()
    }

    private func manager() -> CBCentralManager {
        if let central { return central }
        let created = CBCentralManager(delegate: self, queue: .main)
        central = created
        return created
    }

    /// A sentence explaining why Bluetooth cannot be used, or nil if it can.
    public var unavailableReason: String? {
        switch state {
        case .poweredOn, .unknown, .resetting: return nil
        case .poweredOff: return "Bluetooth is turned off. Turn it on to reach your router."
        case .unauthorized: return "Porter is not allowed to use Bluetooth. Allow it in Settings."
        case .unsupported: return "This device does not support Bluetooth Low Energy."
        @unknown default: return "Bluetooth is not available."
        }
    }

    public func startScanning() {
        wantsScan = true
        let central = manager()
        guard state == .poweredOn else { return }
        central.scanForPeripherals(withServices: [PorterUUID.service], options: [CBCentralManagerScanOptionAllowDuplicatesKey: true])
    }

    public func stopScanning() {
        wantsScan = false
        if state == .poweredOn { central?.stopScan() }
    }

    /// Connects to a router by peripheral identifier and prepares its
    /// GATT link. Works for routers seen in this scan and for ones
    /// remembered from earlier.
    public func connect(to id: UUID, timeout: TimeInterval = 15) async throws -> BLELink {
        let central = manager()
        guard state == .poweredOn else {
            throw BLEError.bluetoothUnavailable(unavailableReason ?? "Bluetooth is not ready yet.")
        }
        let peripheral: CBPeripheral
        if let known = peripherals[id] {
            peripheral = known
        } else if let restored = central.retrievePeripherals(withIdentifiers: [id]).first {
            peripherals[id] = restored
            peripheral = restored
        } else {
            throw BLEError.connectionFailed("the router is not in range")
        }
        // An earlier link to this router has to be torn down now, not
        // later: its teardown cancels the peripheral's connection, and
        // must not land on the attempt that is about to start.
        links[id]?.fail(BLEError.disconnected)
        let link = BLELink(peripheral: peripheral) { [weak central] in
            central?.cancelPeripheralConnection(peripheral)
        }
        links[id] = link
        central.connect(peripheral, options: nil)
        let watchdog = Task { [weak link] in
            do {
                try await Task.sleep(nanoseconds: UInt64(timeout * 1_000_000_000))
            } catch {
                return // cancelled: the link became ready (or failed) in time
            }
            link?.fail(BLEError.timeout)
        }
        defer { watchdog.cancel() }
        try await link.waitUntilReady()
        return link
    }
}

extension BLECentral: CBCentralManagerDelegate {
    public nonisolated func centralManagerDidUpdateState(_ central: CBCentralManager) {
        MainActor.assumeIsolated {
            state = central.state
            if central.state == .poweredOn {
                if wantsScan { startScanning() }
            } else {
                discovered.removeAll()
                for link in links.values { link.fail(BLEError.disconnected) }
                links.removeAll()
            }
        }
    }

    public nonisolated func centralManager(
        _ central: CBCentralManager, didDiscover peripheral: CBPeripheral, advertisementData: [String: Any], rssi RSSI: NSNumber
    ) {
        MainActor.assumeIsolated {
            peripherals[peripheral.identifier] = peripheral
            let name = (advertisementData[CBAdvertisementDataLocalNameKey] as? String) ?? peripheral.name ?? "Porter"
            let seen = DiscoveredRouter(id: peripheral.identifier, name: name, rssi: RSSI.intValue, lastSeen: Date())
            if let index = discovered.firstIndex(where: { $0.id == seen.id }) {
                // Avoid republishing on every advertisement.
                let old = discovered[index]
                if old.name != seen.name || abs(old.rssi - seen.rssi) >= 6 || seen.lastSeen.timeIntervalSince(old.lastSeen) > 5 {
                    discovered[index] = seen
                }
            } else {
                discovered.append(seen)
            }
        }
    }

    public nonisolated func centralManager(_ central: CBCentralManager, didConnect peripheral: CBPeripheral) {
        MainActor.assumeIsolated { links[peripheral.identifier]?.didConnect() }
    }

    public nonisolated func centralManager(_ central: CBCentralManager, didFailToConnect peripheral: CBPeripheral, error: Error?) {
        MainActor.assumeIsolated {
            links[peripheral.identifier]?.fail(BLEError.connectionFailed(error?.localizedDescription ?? "unknown error"))
        }
    }

    public nonisolated func centralManager(_ central: CBCentralManager, didDisconnectPeripheral peripheral: CBPeripheral, error: Error?) {
        MainActor.assumeIsolated {
            guard let link = links[peripheral.identifier] else { return }
            if link.hasFailed {
                // Already given up on; this is its connection ending.
                links[peripheral.identifier] = nil
                return
            }
            guard link.hasConnected else {
                // This is the end of an earlier connection, cancelled just
                // before the current attempt began. The attempt stands;
                // asking again is harmless if it was never dropped.
                central.connect(peripheral, options: nil)
                return
            }
            link.fail(BLEError.disconnected)
            links[peripheral.identifier] = nil
        }
    }
}

/// The GATT link to one router: fragments out through RX, fragments in
/// through TX notifications, reassembled into packets.
@MainActor
public final class BLELink: NSObject, PacketTransport {
    private let peripheral: CBPeripheral
    private let disconnect: () -> Void
    private let tag = UInt16.random(in: 1...UInt16.max)

    private var rx: CBCharacteristic?
    private var tx: CBCharacteristic?
    private var infoCharacteristic: CBCharacteristic?
    private var reassembler = Reassembler()

    private var failure: Error?
    private var isReady = false
    /// Whether this link's own connection was ever established.
    private(set) var hasConnected = false
    var hasFailed: Bool { failure != nil }
    private var readyWaiters: [CheckedContinuation<Void, Error>] = []
    private var packets: [Data] = []
    private var packetWaiters: [CheckedContinuation<Data, Error>] = []
    private var writeWaiters: [CheckedContinuation<Void, Error>] = []
    private var infoWaiters: [CheckedContinuation<Data, Error>] = []
    private var sending = false
    private var sendQueue: [CheckedContinuation<Void, Never>] = []

    init(peripheral: CBPeripheral, disconnect: @escaping () -> Void) {
        self.peripheral = peripheral
        self.disconnect = disconnect
        super.init()
        peripheral.delegate = self
    }

    public var peripheralID: UUID { peripheral.identifier }

    func didConnect() {
        guard failure == nil else { return }
        hasConnected = true
        peripheral.discoverServices([PorterUUID.service])
    }

    func waitUntilReady() async throws {
        if let failure { throw failure }
        if isReady { return }
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in readyWaiters.append(c) }
    }

    /// Ends the link, failing everything that is waiting on it.
    func fail(_ error: Error) {
        guard failure == nil else { return }
        failure = error
        for w in readyWaiters { w.resume(throwing: error) }
        for w in packetWaiters { w.resume(throwing: error) }
        for w in writeWaiters { w.resume(throwing: error) }
        for w in infoWaiters { w.resume(throwing: error) }
        readyWaiters.removeAll()
        packetWaiters.removeAll()
        writeWaiters.removeAll()
        infoWaiters.removeAll()
        for w in sendQueue { w.resume() }
        sendQueue.removeAll()
        disconnect()
    }

    /// Gives up on the link, for a caller whose own deadline has passed.
    public func abandon(_ error: Error = BLEError.timeout) {
        fail(error)
    }

    /// Reads the unauthenticated Info characteristic.
    public func readInfo() async throws -> RouterInfo {
        if let failure { throw failure }
        guard let infoCharacteristic else { throw BLEError.serviceMissing }
        let data: Data = try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data, Error>) in
            infoWaiters.append(c)
            peripheral.readValue(for: infoCharacteristic)
        }
        return try RouterConnection.makeDecoder().decode(RouterInfo.self, from: data)
    }

    // MARK: PacketTransport

    public nonisolated func send(_ packet: Data) async throws {
        try await sendOnMain(packet)
    }

    public nonisolated func receive() async throws -> Data {
        try await receiveOnMain()
    }

    public nonisolated func close() {
        Task { @MainActor in self.fail(BLEError.disconnected) }
    }

    private func sendOnMain(_ packet: Data) async throws {
        // One packet at a time: fragments of two packets must not mix.
        if sending {
            // Woken by the previous sender, which hands the turn over
            // without ever clearing `sending`.
            await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in sendQueue.append(c) }
        } else {
            sending = true
        }
        defer {
            if sendQueue.isEmpty {
                sending = false
            } else {
                sendQueue.removeFirst().resume()
            }
        }
        if let failure { throw failure }
        guard let rx else { throw BLEError.serviceMissing }
        let mtu = peripheral.maximumWriteValueLength(for: .withoutResponse)
        for fragment in Framing.fragment(tag: tag, packet: packet, mtu: mtu) {
            while !peripheral.canSendWriteWithoutResponse {
                if let failure { throw failure }
                try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in writeWaiters.append(c) }
            }
            if let failure { throw failure }
            peripheral.writeValue(fragment, for: rx, type: .withoutResponse)
        }
    }

    private func receiveOnMain() async throws -> Data {
        if !packets.isEmpty { return packets.removeFirst() }
        if let failure { throw failure }
        return try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data, Error>) in packetWaiters.append(c) }
    }
}

extension BLELink: CBPeripheralDelegate {
    public nonisolated func peripheral(_ peripheral: CBPeripheral, didDiscoverServices error: Error?) {
        MainActor.assumeIsolated {
            guard let service = peripheral.services?.first(where: { $0.uuid == PorterUUID.service }) else {
                fail(BLEError.serviceMissing)
                return
            }
            peripheral.discoverCharacteristics([PorterUUID.rx, PorterUUID.tx, PorterUUID.info], for: service)
        }
    }

    public nonisolated func peripheral(_ peripheral: CBPeripheral, didDiscoverCharacteristicsFor service: CBService, error: Error?) {
        MainActor.assumeIsolated {
            for characteristic in service.characteristics ?? [] {
                switch characteristic.uuid {
                case PorterUUID.rx: rx = characteristic
                case PorterUUID.tx: tx = characteristic
                case PorterUUID.info: infoCharacteristic = characteristic
                default: break
                }
            }
            guard rx != nil, let tx else {
                fail(BLEError.serviceMissing)
                return
            }
            peripheral.setNotifyValue(true, for: tx)
        }
    }

    public nonisolated func peripheral(_ peripheral: CBPeripheral, didUpdateNotificationStateFor characteristic: CBCharacteristic, error: Error?) {
        MainActor.assumeIsolated {
            guard characteristic.uuid == PorterUUID.tx else { return }
            if let error {
                fail(BLEError.connectionFailed(error.localizedDescription))
                return
            }
            guard characteristic.isNotifying else { return }
            isReady = true
            for w in readyWaiters { w.resume() }
            readyWaiters.removeAll()
        }
    }

    public nonisolated func peripheral(_ peripheral: CBPeripheral, didUpdateValueFor characteristic: CBCharacteristic, error: Error?) {
        // Copy the value before leaving the callback.
        let value = characteristic.value
        let uuid = characteristic.uuid
        MainActor.assumeIsolated {
            if uuid == PorterUUID.info {
                guard !infoWaiters.isEmpty else { return }
                let waiter = infoWaiters.removeFirst()
                if let value, error == nil {
                    waiter.resume(returning: value)
                } else {
                    waiter.resume(throwing: error ?? BLEError.serviceMissing)
                }
                return
            }
            guard uuid == PorterUUID.tx, let value, Framing.tag(of: value) == tag else { return }
            do {
                guard let packet = try reassembler.add(value) else { return }
                if packetWaiters.isEmpty {
                    packets.append(packet)
                } else {
                    packetWaiters.removeFirst().resume(returning: packet)
                }
            } catch {
                fail(error)
            }
        }
    }

    public nonisolated func peripheralIsReady(toSendWriteWithoutResponse peripheral: CBPeripheral) {
        MainActor.assumeIsolated {
            let waiters = writeWaiters
            writeWaiters.removeAll()
            for w in waiters { w.resume() }
        }
    }
}
#endif
