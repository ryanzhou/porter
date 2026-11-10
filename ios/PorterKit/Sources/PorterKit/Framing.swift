import Foundation

/// BLE link fragments: 2-byte session tag, 1 flag byte, payload.
public enum Framing {
    public static let first: UInt8 = 0x80
    public static let last: UInt8 = 0x40
    public static let headerSize = 3
    public static let maxPacket = 65535
    /// The longest value a GATT attribute can have, and so the longest
    /// fragment either side may send whatever the MTU.
    public static let maxFragment = 512

    /// Splits a packet into fragments of at most `mtu` bytes each.
    public static func fragment(tag: UInt16, packet: Data, mtu: Int) -> [Data] {
        let room = max(mtu - headerSize, 1)
        var out: [Data] = []
        var offset = packet.startIndex
        var isFirst = true
        repeat {
            let end = min(offset + room, packet.endIndex)
            var flags: UInt8 = 0
            if isFirst { flags |= first }
            if end == packet.endIndex { flags |= last }
            var frag = Data([UInt8(tag >> 8), UInt8(tag & 0xff), flags])
            frag.append(packet[offset..<end])
            out.append(frag)
            offset = end
            isFirst = false
        } while offset < packet.endIndex
        return out
    }

    public static func tag(of fragment: Data) -> UInt16? {
        guard fragment.count >= headerSize else { return nil }
        let f = Data(fragment)
        return UInt16(f[0]) << 8 | UInt16(f[1])
    }
}

public enum FramingError: Error { case shortFragment, missingStart, tooLarge }

/// Rebuilds packets from the fragments of one session.
public struct Reassembler {
    private var buffer = Data()
    private var active = false

    public init() {}

    /// Returns a packet when `fragment` completes one.
    public mutating func add(_ fragment: Data) throws -> Data? {
        let f = Data(fragment)
        guard f.count >= Framing.headerSize else { throw FramingError.shortFragment }
        let flags = f[2]
        if flags & Framing.first != 0 {
            buffer.removeAll(keepingCapacity: true)
            active = true
        } else if !active {
            throw FramingError.missingStart
        }
        buffer.append(f.dropFirst(Framing.headerSize))
        if buffer.count > Framing.maxPacket {
            active = false
            throw FramingError.tooLarge
        }
        if flags & Framing.last != 0 {
            active = false
            return buffer
        }
        return nil
    }
}
