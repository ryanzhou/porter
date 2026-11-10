import CoreImage.CIFilterBuiltins
import PorterKit
import SwiftUI
import UIKit

// MARK: - Design
//
// The app's own screens are built from a few pieces: cards on the grouped
// background, an icon tile that says what a row is about, and figures set
// large enough to be read without a label next to them.

enum Surface {
    static let page: Color = Color(uiColor: UIColor.systemGroupedBackground)
    static let card: Color = Color(uiColor: UIColor.secondarySystemGroupedBackground)
}

/// A rounded panel.
struct Card<Content: View>: View {
    var padding: CGFloat = 16
    @ViewBuilder var content: () -> Content

    var body: some View {
        content()
            .padding(padding)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Surface.card, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
    }
}

/// An icon on a tinted rounded square.
struct Glyph: View {
    let symbol: String
    var color: Color = Color.accentColor
    var size: CGFloat = 34
    var filled: Bool = false

    private var background: Color {
        filled ? color : color.opacity(0.14)
    }

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: size * 0.46, weight: .semibold))
            .foregroundStyle(filled ? Color.white : color)
            .frame(width: size, height: size)
            .background(background, in: RoundedRectangle(cornerRadius: size * 0.28, style: .continuous))
            .accessibilityHidden(true)
    }
}

/// A number that is the point of the card it is on, with its unit small
/// beside it.
struct Figure: View {
    let value: String
    var unit: String = ""
    var font: Font = Font.title2

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 3) {
            Text(value)
                .font(font.weight(.semibold))
                .monospacedDigit()
            if !unit.isEmpty {
                Text(unit)
                    .font(.footnote.weight(.medium))
                    .foregroundStyle(.secondary)
            }
        }
        .fontDesign(.rounded)
        .lineLimit(1)
    }
}

/// A small heading above a card.
struct Heading: View {
    let text: String

    var body: some View {
        Text(text)
            .font(.footnote.weight(.semibold))
            .foregroundStyle(.secondary)
            .textCase(.uppercase)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 4)
            .padding(.top, 8)
    }
}

/// A byte count as number and unit, for `Figure`.
func byteParts(_ n: UInt64) -> (value: String, unit: String) {
    let text: String = formatBytes(n)
    let pieces: [Substring] = text.split(separator: " ")
    guard pieces.count == 2 else { return (text, "") }
    return (String(pieces[0]), String(pieces[1]))
}

private struct SparkShape: Shape {
    let values: [Double]
    let closed: Bool

    func path(in rect: CGRect) -> Path {
        var path = Path()
        guard values.count > 1 else { return path }
        let peak: Double = max(values.max() ?? 0, 1)
        let step: CGFloat = rect.width / CGFloat(values.count - 1)
        for (index, value) in values.enumerated() {
            let x: CGFloat = rect.minX + CGFloat(index) * step
            let y: CGFloat = rect.maxY - CGFloat(value / peak) * rect.height
            let point = CGPoint(x: x, y: y)
            if index == 0 {
                path.move(to: point)
            } else {
                path.addLine(to: point)
            }
        }
        if closed {
            path.addLine(to: CGPoint(x: rect.maxX, y: rect.maxY))
            path.addLine(to: CGPoint(x: rect.minX, y: rect.maxY))
            path.closeSubpath()
        }
        return path
    }
}

/// The shape of a series, without axes.
struct Sparkline: View {
    let values: [Double]
    var color: Color = Color.accentColor

    var body: some View {
        ZStack {
            SparkShape(values: values, closed: true)
                .fill(color.opacity(0.16))
            SparkShape(values: values, closed: false)
                .stroke(color, style: StrokeStyle(lineWidth: 1.5, lineCap: .round, lineJoin: .round))
        }
        .accessibilityHidden(true)
    }
}

/// The text a camera turns into "Join this network".
func wifiJoinCode(ssid: String, passphrase: String, hidden: Bool) -> String {
    func escaped(_ text: String) -> String {
        var out = ""
        for character in text {
            if "\\;,:\"".contains(character) {
                out.append("\\")
            }
            out.append(character)
        }
        return out
    }
    var code: String = "WIFI:T:WPA;S:" + escaped(ssid) + ";P:" + escaped(passphrase) + ";"
    if hidden {
        code += "H:true;"
    }
    return code + ";"
}

func qrCode(_ text: String) -> UIImage? {
    let filter = CIFilter.qrCodeGenerator()
    filter.message = Data(text.utf8)
    filter.correctionLevel = "M"
    guard let output = filter.outputImage else { return nil }
    let scaled: CIImage = output.transformed(by: CGAffineTransform(scaleX: 12, y: 12))
    guard let image = CIContext().createCGImage(scaled, from: scaled.extent) else { return nil }
    return UIImage(cgImage: image)
}

// MARK: - Indicators

/// Four bars for a signal strength in dBm.
struct SignalBars: View {
    let rssi: Int

    private var level: Int {
        if rssi >= -55 { return 4 }
        if rssi >= -67 { return 3 }
        if rssi >= -78 { return 2 }
        if rssi >= -89 { return 1 }
        return 0
    }

    var body: some View {
        HStack(alignment: .bottom, spacing: 2) {
            ForEach(0..<4, id: \.self) { index in
                RoundedRectangle(cornerRadius: 1)
                    .fill(index < level ? Color.primary : Color.secondary.opacity(0.3))
                    .frame(width: 3, height: CGFloat(5 + index * 3))
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(verbatim: "Signal \(level) of 4"))
    }
}

struct StatusDot: View {
    let color: Color

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 10, height: 10)
    }
}

struct Badge: View {
    let text: String
    var color: Color = Color.secondary

    var body: some View {
        Text(text)
            .font(.caption2)
            .foregroundStyle(color)
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(color.opacity(0.15), in: Capsule())
    }
}

// MARK: - Formatting

func formatBytes(_ n: UInt64) -> String {
    ByteCountFormatter.string(fromByteCount: Int64(clamping: n), countStyle: .binary)
}

/// "5 min. ago" for a unix timestamp in seconds; "Never" for zero.
func relativeTime(_ unix: Int64) -> String {
    guard unix > 0 else { return "Never" }
    let date = Date(timeIntervalSince1970: TimeInterval(unix))
    if abs(date.timeIntervalSinceNow) < 5 { return "Just now" }
    let formatter = RelativeDateTimeFormatter()
    formatter.unitsStyle = .short
    return formatter.localizedString(for: date, relativeTo: Date())
}

func formatDate(_ unix: Int64) -> String {
    guard unix > 0 else { return "Never" }
    let date = Date(timeIntervalSince1970: TimeInterval(unix))
    return date.formatted(date: .abbreviated, time: .shortened)
}

func formatDuration(_ seconds: Int64) -> String {
    let total = max(seconds, 0)
    let days = total / 86400
    let hours = (total % 86400) / 3600
    let minutes = (total % 3600) / 60
    if days > 0 { return "\(days) d \(hours) h" }
    if hours > 0 { return "\(hours) h \(minutes) min" }
    if minutes > 0 { return "\(minutes) min" }
    return "\(total) s"
}

/// The flag emoji for a two-letter country code. Empty when there is no
/// code or it is not two ASCII letters.
func countryFlag(_ code: String?) -> String {
    guard let code else { return "" }
    let scalars = Array(code.unicodeScalars)
    guard scalars.count == 2 else { return "" }
    var flag = ""
    for scalar in scalars {
        var value: UInt32 = scalar.value
        if value >= 97 && value <= 122 {
            value -= 32
        }
        // A flag is the two letters as regional indicator symbols.
        guard value >= 65, value <= 90, let indicator = Unicode.Scalar(0x1F1E6 + value - 65) else { return "" }
        flag.unicodeScalars.append(indicator)
    }
    return flag
}

/// The name of a country in the phone's language; the code itself when
/// the phone does not know it.
func countryName(_ code: String) -> String {
    Locale.current.localizedString(forRegionCode: code) ?? code
}

// MARK: - Status wording

func internetLabel(_ state: String) -> String {
    switch state {
    case "down": return "Not connected"
    case "connecting": return "Connecting"
    case "limited": return "Connected, no internet"
    case "captive": return "Sign-in required"
    case "online": return "Online"
    default: return "Connected, not checked"
    }
}

func internetColor(_ state: String) -> Color {
    switch state {
    case "down": return Color.red
    case "connecting": return Color.yellow
    case "limited": return Color.orange
    case "captive": return Color.orange
    case "online": return Color.green
    default: return Color.gray
    }
}

func uplinkLabel(kind: String, iface: String) -> String {
    switch kind {
    case "wifi": return "Wi-Fi (\(iface))"
    case "eth": return "Wired (\(iface))"
    default: return iface
    }
}

func joinFailureLabel(_ reason: String?) -> String {
    switch reason ?? "" {
    case "wrong_password": return "Wrong password"
    case "rejected": return "Rejected by the network"
    default: return "Could not join"
    }
}

func vpnStateLabel(_ status: VPNStatus) -> String {
    switch status.state {
    case "off": return "Off"
    case "connecting": return "Connecting"
    case "up": return "Connected"
    case "error":
        if let error = status.error, !error.isEmpty { return error }
        return "Error"
    default: return status.state
    }
}

func vpnStateColor(_ state: String) -> Color {
    switch state {
    case "up": return Color.green
    case "connecting": return Color.yellow
    case "error": return Color.red
    default: return Color.gray
    }
}

// MARK: - Validation

/// The router accepts Wi-Fi passwords of 8 to 63 printable ASCII characters.
func validPassphrase(_ text: String) -> Bool {
    let bytes = Array(text.utf8)
    guard bytes.count >= 8, bytes.count <= 63 else { return false }
    return bytes.allSatisfy { $0 >= 0x20 && $0 <= 0x7e }
}

func validNetworkName(_ text: String) -> Bool {
    !text.isEmpty && text.utf8.count <= 32
}

func validRouterName(_ text: String) -> Bool {
    let trimmed = text.trimmingCharacters(in: .whitespaces)
    return !trimmed.isEmpty && trimmed.utf8.count <= 20
}

/// Six colon-separated hex bytes that do not form a multicast address.
func validUnicastMAC(_ text: String) -> Bool {
    let parts = text.split(separator: ":", omittingEmptySubsequences: false)
    guard parts.count == 6 else { return false }
    var bytes: [UInt8] = []
    for part in parts {
        guard part.count == 2, let byte = UInt8(String(part), radix: 16) else { return false }
        bytes.append(byte)
    }
    return bytes[0] & 1 == 0
}

// MARK: - Bindings into the configuration

extension AppModel {
    /// A binding for controls that apply at once: reads the current
    /// configuration, writes through `update`.
    func binding<Value>(_ keyPath: WritableKeyPath<RouterConfig, Value>, fallback: Value) -> Binding<Value> {
        Binding(
            get: { self.config?[keyPath: keyPath] ?? fallback },
            set: { value in
                Task { await self.update { cfg in cfg[keyPath: keyPath] = value } }
            }
        )
    }

    func vpnEnabled(_ id: String) -> Binding<Bool> {
        Binding(
            get: { self.config?.vpns.first(where: { $0.id == id })?.enabled ?? false },
            set: { value in
                Task {
                    await self.update { cfg in
                        if let index = cfg.vpns.firstIndex(where: { $0.id == id }) {
                            cfg.vpns[index].enabled = value
                        }
                    }
                }
            }
        )
    }
}

// MARK: - Shared views

/// A list section that edits an array of strings: rows that can be swiped
/// away, and a field to add one.
struct StringListEditor: View {
    let title: String
    @Binding var items: [String]
    var placeholder: String = ""
    var keyboard: UIKeyboardType = .default
    var footer: String = ""

    @State private var draft: String = ""

    private var trimmed: String {
        draft.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    var body: some View {
        Section {
            ForEach(Array(items.enumerated()), id: \.offset) { item in
                Text(item.element)
                    .lineLimit(2)
            }
            .onDelete { offsets in
                items.remove(atOffsets: offsets)
            }
            HStack {
                TextField(placeholder, text: $draft)
                    .keyboardType(keyboard)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .onSubmit { add() }
                Button {
                    add()
                } label: {
                    Image(systemName: "plus.circle.fill")
                }
                .buttonStyle(.borderless)
                .disabled(trimmed.isEmpty)
            }
        } header: {
            Text(title)
        } footer: {
            if !footer.isEmpty {
                Text(footer)
            }
        }
    }

    private func add() {
        let value = trimmed
        guard !value.isEmpty else { return }
        if !items.contains(value) {
            items.append(value)
        }
        draft = ""
    }
}

struct CountryChoice: Identifiable {
    let code: String
    let name: String
    var id: String { code }

    /// Two-letter ISO regions, without the groupings that are not
    /// regulatory domains.
    static let all: [CountryChoice] = {
        let groupings: Set<String> = ["EU", "EZ", "UN", "QO", "ZZ"]
        let locale = Locale.current
        var choices: [CountryChoice] = []
        for region in Locale.Region.isoRegions {
            let code = region.identifier
            guard code.count == 2, !groupings.contains(code) else { continue }
            let name = locale.localizedString(forRegionCode: code) ?? code
            choices.append(CountryChoice(code: code, name: name))
        }
        choices.sort { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
        return choices
    }()

    static var deviceDefault: String {
        let code = Locale.current.region?.identifier ?? ""
        return all.contains(where: { $0.code == code }) ? code : ""
    }
}

struct CountryPicker: View {
    @Binding var selection: String

    private var listed: Bool {
        CountryChoice.all.contains(where: { $0.code == selection })
    }

    var body: some View {
        Picker("Country", selection: $selection) {
            if selection.isEmpty {
                Text("Not set").tag("")
            } else if !listed {
                Text(selection).tag(selection)
            }
            ForEach(CountryChoice.all) { choice in
                Text(choice.name).tag(choice.code)
            }
        }
        .pickerStyle(.navigationLink)
    }
}

/// A destructive button that asks before it acts.
struct ConfirmedButton: View {
    let title: String
    let message: String
    let confirmTitle: String
    let action: () -> Void

    @State private var asking = false

    var body: some View {
        Button(title, role: .destructive) {
            asking = true
        }
        .confirmationDialog(title, isPresented: $asking, titleVisibility: .visible) {
            Button(confirmTitle, role: .destructive) { action() }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text(message)
        }
    }
}

/// Presents the model's error message from inside a sheet. The alert on
/// the root view cannot appear while a sheet covers it.
struct SheetErrorAlert: ViewModifier {
    @EnvironmentObject private var model: AppModel

    func body(content: Content) -> some View {
        content.alert("Porter", isPresented: Binding(
            get: { model.errorMessage != nil },
            set: { if !$0 { model.errorMessage = nil } }
        )) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(model.errorMessage ?? "")
        }
    }
}
