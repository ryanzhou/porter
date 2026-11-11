import Charts
import PorterKit
import SwiftUI
import UIKit

// MARK: - Shared pieces

/// The two series every traffic chart shows, and their colours. The pair
/// stays distinguishable with the common forms of colour blindness and
/// has its own steps for the dark appearance.
enum TrafficSeries {
    static let download: String = "Download"
    static let upload: String = "Upload"

    static let downloadColor: Color = adaptiveColor(
        light: UIColor(red: 0.165, green: 0.471, blue: 0.839, alpha: 1),
        dark: UIColor(red: 0.224, green: 0.529, blue: 0.898, alpha: 1)
    )

    static let uploadColor: Color = adaptiveColor(
        light: UIColor(red: 0.922, green: 0.408, blue: 0.204, alpha: 1),
        dark: UIColor(red: 0.851, green: 0.349, blue: 0.149, alpha: 1)
    )

    private static func adaptiveColor(light: UIColor, dark: UIColor) -> Color {
        let dynamic = UIColor(dynamicProvider: { (traits: UITraitCollection) -> UIColor in
            if traits.userInterfaceStyle == .dark {
                return dark
            }
            return light
        })
        return Color(uiColor: dynamic)
    }
}

/// Adds byte counts without trapping on overflow.
private func sumBytes(_ values: [UInt64]) -> UInt64 {
    var total: UInt64 = 0
    for value in values {
        let (sum, overflow) = total.addingReportingOverflow(value)
        total = overflow ? UInt64.max : sum
    }
    return total
}

private func formatLatency(_ milliseconds: Double) -> String {
    String(format: "%.0f ms", milliseconds)
}

/// Tick values for a byte axis: round numbers of KB, MB or GB, at most
/// four above zero. The last one is at or above `peak`.
private func byteAxisTicks(peak: Double) -> [Double] {
    let target: Double = max(peak, 1024)
    var unit: Double = 1024
    while unit * 1024 <= target {
        unit *= 1024
    }
    let multipliers: [Double] = [0.25, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500]
    var step: Double = unit * 500
    for multiplier in multipliers {
        if target / (unit * multiplier) <= 4 {
            step = unit * multiplier
            break
        }
    }
    let count = Int((target / step).rounded(.up))
    var ticks: [Double] = [0]
    for index in 1...max(count, 1) {
        ticks.append(step * Double(index))
    }
    return ticks
}

private func byteAxisLabel(_ value: Double) -> String {
    if value < 1 { return "0" }
    return formatBytes(UInt64(exactly: value.rounded()) ?? UInt64.max)
}

// MARK: - Chart

private struct TrafficPoint: Identifiable {
    let id: Int
    let date: Date
    let direction: String
    let bytes: Double
}

/// Download and upload per time bucket, as stacked bars. `unit` is the
/// width of one bucket: `.minute` or `.hour`.
struct TrafficChart: View {
    let samples: [TrafficSample]
    let unit: Calendar.Component
    var height: CGFloat = 200

    private var isEmpty: Bool {
        samples.allSatisfy { $0.up == 0 && $0.down == 0 }
    }

    private var points: [TrafficPoint] {
        var result: [TrafficPoint] = []
        result.reserveCapacity(samples.count * 2)
        for (index, sample) in samples.enumerated() {
            let date = Date(timeIntervalSince1970: TimeInterval(sample.t))
            result.append(TrafficPoint(id: index * 2, date: date, direction: TrafficSeries.download, bytes: Double(sample.down)))
            result.append(TrafficPoint(id: index * 2 + 1, date: date, direction: TrafficSeries.upload, bytes: Double(sample.up)))
        }
        return result
    }

    /// The tallest bar: both directions are stacked.
    private var peak: Double {
        var highest: Double = 0
        for sample in samples {
            highest = max(highest, Double(sample.down) + Double(sample.up))
        }
        return highest
    }

    var body: some View {
        if isEmpty {
            Text("No traffic in this period")
                .foregroundStyle(.secondary)
        } else {
            chart
        }
    }

    private var chart: some View {
        let bucket: Calendar.Component = unit
        let ticks: [Double] = byteAxisTicks(peak: peak)
        let top: Double = ticks.last ?? 1024
        return Chart(points) { point in
            BarMark(
                x: .value("Time", point.date, unit: bucket),
                y: .value("Bytes", point.bytes)
            )
            .foregroundStyle(by: .value("Direction", point.direction))
        }
        .chartForegroundStyleScale([
            TrafficSeries.download: TrafficSeries.downloadColor,
            TrafficSeries.upload: TrafficSeries.uploadColor,
        ])
        .chartLegend(.hidden)
        .chartYScale(domain: 0.0...top)
        .chartYAxis {
            AxisMarks(values: ticks) { value in
                AxisGridLine()
                AxisValueLabel {
                    if let bytes = value.as(Double.self) {
                        Text(byteAxisLabel(bytes))
                    }
                }
            }
        }
        .frame(height: height)
    }
}

// MARK: - Traffic

private enum TrafficRange: String, CaseIterable, Identifiable {
    case hour
    case days

    var id: String { rawValue }

    var title: String {
        switch self {
        case .hour: return "Last hour"
        case .days: return "Last 3 days"
        }
    }
}

/// How much the network has moved, which devices moved it, and a way into
/// the list of connections.
struct TrafficView: View {
    @EnvironmentObject private var model: AppModel

    @State private var stats: TrafficStats? = nil
    @State private var talkers: [TopTalker] = []
    @State private var loaded = false
    @State private var range: TrafficRange = .hour

    var body: some View {
        List {
            ThroughputSection(stats: stats, loaded: loaded, range: $range)
            TopDevicesSection(talkers: talkers, loaded: loaded)
            Section {
                NavigationLink {
                    FlowsView(mac: "")
                } label: {
                    Label("Connections", systemImage: "arrow.left.arrow.right")
                }
            }
        }
        .navigationTitle("Traffic")
        .refreshable { _ = await reload() }
        .task { await poll() }
    }

    @discardableResult
    private func reload() async -> Bool {
        let newStats: TrafficStats? = await model.fetch { try await $0.trafficStats(mac: "") }
        guard let newStats else {
            loaded = true
            return false
        }
        stats = newStats
        let newTalkers: [TopTalker]? = await model.fetch { try await $0.topTalkers(hours: 24) }
        loaded = true
        guard let newTalkers else { return false }
        talkers = newTalkers
        return true
    }

    /// Refreshes every ten seconds until the view goes away. Stops after
    /// a failure so that one lost link does not produce a stream of alerts.
    private func poll() async {
        while !Task.isCancelled {
            let ok = await reload()
            if !ok { return }
            do {
                try await Task.sleep(nanoseconds: 10_000_000_000)
            } catch {
                return
            }
        }
    }
}

private struct ThroughputSection: View {
    let stats: TrafficStats?
    let loaded: Bool
    @Binding var range: TrafficRange

    private var samples: [TrafficSample] {
        guard let stats else { return [] }
        switch range {
        case .hour: return stats.minutes
        case .days: return stats.hours
        }
    }

    private var unit: Calendar.Component {
        switch range {
        case .hour: return Calendar.Component.minute
        case .days: return Calendar.Component.hour
        }
    }

    private var downloaded: UInt64 { sumBytes(samples.map { $0.down }) }
    private var uploaded: UInt64 { sumBytes(samples.map { $0.up }) }

    var body: some View {
        Section {
            Picker("Range", selection: $range) {
                ForEach(TrafficRange.allCases) { option in
                    Text(option.title).tag(option)
                }
            }
            .pickerStyle(.segmented)
            .listRowSeparator(.hidden)
            if loaded {
                HStack(spacing: 24) {
                    TransferFigure(symbol: "arrow.down", bytes: downloaded, color: TrafficSeries.downloadColor, font: Font.title2)
                    TransferFigure(symbol: "arrow.up", bytes: uploaded, color: TrafficSeries.uploadColor, font: Font.title2)
                    Spacer()
                }
                .listRowSeparator(.hidden)
                TrafficChart(samples: samples, unit: unit)
            } else {
                ProgressView()
            }
        }
    }
}

private struct TopDevicesSection: View {
    @EnvironmentObject private var model: AppModel
    let talkers: [TopTalker]
    let loaded: Bool

    /// The router sends them ordered by traffic; the ten busiest are shown.
    private var shown: [TopTalker] {
        let busy = talkers.filter { $0.up > 0 || $0.down > 0 }
        return Array(busy.prefix(10))
    }

    private var largest: Double {
        var highest: Double = 0
        for talker in shown {
            highest = max(highest, Double(talker.down) + Double(talker.up))
        }
        return highest
    }

    var body: some View {
        Section("Busiest devices, last 24 hours") {
            if loaded && shown.isEmpty {
                Text("The devices that use the most data are listed here.")
                    .foregroundStyle(.secondary)
            }
            ForEach(shown) { talker in
                NavigationLink {
                    DeviceDetailView(mac: talker.mac)
                } label: {
                    TopTalkerRow(name: model.deviceName(talker.mac), talker: talker, largest: largest)
                }
            }
        }
    }
}

private struct TopTalkerRow: View {
    let name: String
    let talker: TopTalker
    let largest: Double

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text(name)
                    .lineLimit(1)
                Spacer()
                Text(formatBytes(talker.down &+ talker.up))
                    .font(.subheadline)
                    .monospacedDigit()
                    .foregroundStyle(.secondary)
            }
            UsageBar(down: Double(talker.down), up: Double(talker.up), largest: largest)
        }
    }
}

/// A thin bar as long as this device's share of the busiest device's
/// traffic, split into download and upload in the chart's colours.
private struct UsageBar: View {
    let down: Double
    let up: Double
    let largest: Double

    var body: some View {
        GeometryReader { proxy in
            HStack(spacing: 2) {
                Rectangle()
                    .fill(TrafficSeries.downloadColor)
                    .frame(width: segmentWidth(down, available: proxy.size.width))
                Rectangle()
                    .fill(TrafficSeries.uploadColor)
                    .frame(width: segmentWidth(up, available: proxy.size.width))
            }
        }
        .frame(height: 4)
        .background(Color.secondary.opacity(0.15))
        .clipShape(Capsule())
        .accessibilityHidden(true)
    }

    private func segmentWidth(_ value: Double, available: CGFloat) -> CGFloat {
        guard largest > 0 else { return 0 }
        let usable: CGFloat = max(available - 2, 0)
        let share: Double = min(max(value / largest, 0), 1)
        return usable * CGFloat(share)
    }
}

// MARK: - Connections

private enum FlowFilter: String, CaseIterable, Identifiable {
    case active
    case recent

    var id: String { rawValue }

    var title: String {
        switch self {
        case .active: return "Active"
        case .recent: return "Recent"
        }
    }
}

/// Connections from one device, or from every device when `mac` is empty.
struct FlowsView: View {
    @EnvironmentObject private var model: AppModel
    let mac: String

    @State private var flows: [Flow] = []
    @State private var loaded = false
    @State private var filter: FlowFilter = .active
    @State private var search: String = ""

    private var query: String {
        search.trimmingCharacters(in: .whitespaces)
    }

    private var visible: [Flow] {
        let wantActive: Bool = filter == .active
        let text: String = query
        var rows: [Flow] = flows.filter { $0.active == wantActive }
        if !text.isEmpty {
            rows = rows.filter { matches($0, text) }
        }
        rows.sort { first, second in
            if first.last != second.last { return first.last > second.last }
            return first.id < second.id
        }
        return rows
    }

    private var emptyText: String {
        if !query.isEmpty { return "No connection matches your search." }
        switch filter {
        case .active: return "No connections are open right now."
        case .recent: return "Connections that ended recently are listed here."
        }
    }

    private var countriesMissing: Bool {
        model.status?.geoip.available == false
    }

    var body: some View {
        List {
            Section {
                Picker("Show", selection: $filter) {
                    ForEach(FlowFilter.allCases) { option in
                        Text(option.title).tag(option)
                    }
                }
                .pickerStyle(.segmented)
                .listRowInsets(EdgeInsets())
                .listRowBackground(Color.clear)
            }
            FlowListSection(
                rows: visible,
                loaded: loaded,
                emptyText: emptyText,
                showDevice: mac.isEmpty,
                countriesMissing: countriesMissing
            )
        }
        .navigationTitle("Connections")
        .navigationBarTitleDisplayMode(.inline)
        .searchable(text: $search, prompt: "Name, address or device")
        .refreshable { _ = await reload() }
        .task { await poll() }
    }

    private func matches(_ flow: Flow, _ text: String) -> Bool {
        if let name = flow.name, name.localizedCaseInsensitiveContains(text) { return true }
        if flow.dst.localizedCaseInsensitiveContains(text) { return true }
        return model.deviceName(flow.mac).localizedCaseInsensitiveContains(text)
    }

    @discardableResult
    private func reload() async -> Bool {
        let target = mac
        let found: [Flow]? = await model.fetch { try await $0.flows(mac: target) }
        loaded = true
        guard let found else { return false }
        flows = found
        return true
    }

    /// Refreshes every five seconds until the view goes away. Stops after
    /// a failure so that one lost link does not produce a stream of alerts.
    private func poll() async {
        while !Task.isCancelled {
            let ok = await reload()
            if !ok { return }
            do {
                try await Task.sleep(nanoseconds: 5_000_000_000)
            } catch {
                return
            }
        }
    }
}

private struct FlowListSection: View {
    let rows: [Flow]
    let loaded: Bool
    let emptyText: String
    let showDevice: Bool
    let countriesMissing: Bool

    var body: some View {
        Section {
            if !loaded {
                ProgressView()
            } else if rows.isEmpty {
                Text(emptyText)
                    .foregroundStyle(.secondary)
            }
            ForEach(Array(rows.enumerated()), id: \.offset) { item in
                FlowRow(flow: item.element, showDevice: showDevice)
            }
        } footer: {
            if countriesMissing {
                Text("The router has no country database, so connections are shown without a country.")
            }
        }
    }
}

private struct FlowRow: View {
    @EnvironmentObject private var model: AppModel
    let flow: Flow
    let showDevice: Bool

    private var destination: String {
        if let name = flow.name, !name.isEmpty { return name }
        return flow.dst
    }

    private var flag: String { countryFlag(flow.country) }

    /// "TCP 443", then the device when the list covers all of them.
    private var detail: String {
        var parts: [String] = []
        parts.append(flow.port > 0 ? flow.proto.uppercased() + " " + String(flow.port) : flow.proto.uppercased())
        if showDevice {
            parts.append(model.deviceName(flow.mac))
        }
        if !flow.active {
            parts.append(relativeTime(flow.last))
        }
        return parts.joined(separator: " · ")
    }

    var body: some View {
        HStack(alignment: .center, spacing: 10) {
            Text(flag.isEmpty ? "🌐" : flag)
                .font(.title3)
            VStack(alignment: .leading, spacing: 2) {
                Text(destination)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            Spacer(minLength: 4)
            VStack(alignment: .trailing, spacing: 4) {
                PathBadge(path: flow.path)
                Text(formatBytes(flow.down &+ flow.up))
                    .font(.caption)
                    .monospacedDigit()
                    .foregroundStyle(.secondary)
            }
        }
    }
}

/// Where a connection went: grey for direct, blue for a VPN, red for
/// blocked. The text carries the same information as the colour.
private struct PathBadge: View {
    @EnvironmentObject private var model: AppModel
    let path: String

    private var color: Color {
        guard let action = RouteAction(path) else { return Color.secondary }
        switch action {
        case .direct: return Color.secondary
        case .block: return Color.red
        case .vpn: return Color.blue
        }
    }

    var body: some View {
        Badge(text: model.actionLabel(path), color: color)
            .lineLimit(1)
            .layoutPriority(1)
    }
}

// MARK: - Speed test

/// Runs a speed test from the router and lists earlier results.
struct SpeedTestView: View {
    @EnvironmentObject private var model: AppModel

    @State private var via: String = "direct"
    @State private var latest: SpeedTestResult? = nil
    @State private var history: [SpeedTestResult] = []
    @State private var historyLoaded = false
    /// True while a test started from this screen is running.
    @State private var startedHere = false

    private var running: Bool { model.speedTestProgress != nil }

    /// "direct", then one action string for each enabled VPN profile.
    private var paths: [String] {
        let profiles: [VPNProfile] = model.config?.vpns ?? []
        let tunnels: [String] = profiles.filter { $0.enabled }.map { RouteAction.vpn($0.id).raw }
        return ["direct"] + tunnels
    }

    /// What the top of the screen shows: this session's result, or else
    /// the most recent one the router remembers.
    private var shown: SpeedTestResult? {
        latest ?? history.first
    }

    var body: some View {
        List {
            Section {
                if let progress = model.speedTestProgress {
                    SpeedTestProgressPanel(progress: progress)
                } else if let shown {
                    SpeedTestResultPanel(result: shown)
                } else {
                    SpeedTestResultPanel(result: nil)
                }
            }
            .listRowInsets(EdgeInsets())
            .listRowBackground(Color.clear)
            Section {
                Picker("Through", selection: $via) {
                    ForEach(paths, id: \.self) { path in
                        Text(model.actionLabel(path)).tag(path)
                    }
                }
                .disabled(running)
            } footer: {
                Text("The test runs on the router. It measures the internet connection itself, not the Wi-Fi between your devices and the router.")
            }
            Section {
                Button {
                    run()
                } label: {
                    Text(running ? "Testing" : "Start")
                        .font(.headline)
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.large)
                .disabled(running)
                .listRowInsets(EdgeInsets())
                .listRowBackground(Color.clear)
            }
            SpeedTestHistorySection(history: history, loaded: historyLoaded)
        }
        .navigationTitle("Speed test")
        .task { await loadHistory() }
        .onChange(of: running) { _, isRunning in
            // A test started before this screen was opened has finished.
            if !isRunning && !startedHere {
                Task { await loadHistory() }
            }
        }
        .onChange(of: paths) { _, available in
            if !available.contains(via) {
                via = "direct"
            }
        }
    }

    private func run() {
        let path: String = paths.contains(via) ? via : "direct"
        startedHere = true
        Task {
            let result: SpeedTestResult? = await model.runSpeedTest(via: path)
            startedHere = false
            guard let result else { return }
            latest = result
            history.removeAll(where: { $0.t == result.t })
            history.insert(result, at: 0)
        }
    }

    private func loadHistory() async {
        let found: [SpeedTestResult]? = await model.fetch { try await $0.speedTestHistory() }
        historyLoaded = true
        guard let found else { return }
        var merged: [SpeedTestResult] = found.sorted { $0.t > $1.t }
        if let latest, !merged.contains(where: { $0.t == latest.t }) {
            merged.insert(latest, at: 0)
        }
        history = merged
    }
}

/// One measurement: what it is above, the number below.
private struct SpeedFigure: View {
    let symbol: String
    let color: Color
    let value: String
    let unit: String
    var active: Bool = false

    var body: some View {
        VStack(spacing: 6) {
            Image(systemName: symbol)
                .font(.footnote.weight(.bold))
                .foregroundStyle(color)
                .symbolEffect(.pulse, isActive: active)
            Text(value)
                .font(.system(size: 34, weight: .semibold, design: .rounded))
                .monospacedDigit()
                .lineLimit(1)
                .minimumScaleFactor(0.6)
            Text(unit)
                .font(.caption.weight(.medium))
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity)
    }
}

private func speedNumber(_ value: Double) -> String {
    guard value > 0 else { return "–" }
    return value >= 100 ? String(format: "%.0f", value) : String(format: "%.1f", value)
}

/// Download, upload and latency side by side.
private struct SpeedTestResultPanel: View {
    @EnvironmentObject private var model: AppModel
    let result: SpeedTestResult?

    private var failure: String? {
        guard let error = result?.error, !error.isEmpty else { return nil }
        return error
    }

    private var caption: String {
        guard let result else { return "" }
        return formatDate(result.t) + " · " + model.actionLabel(result.via)
    }

    private var latency: String {
        let value: Double = result?.latencyMs ?? 0
        return value > 0 ? String(format: "%.0f", value) : "–"
    }

    var body: some View {
        VStack(spacing: 14) {
            HStack(spacing: 0) {
                SpeedFigure(symbol: "arrow.down", color: TrafficSeries.downloadColor, value: speedNumber(result?.downMbps ?? 0), unit: "Mbps")
                SpeedFigure(symbol: "arrow.up", color: TrafficSeries.uploadColor, value: speedNumber(result?.upMbps ?? 0), unit: "Mbps")
                SpeedFigure(symbol: "stopwatch", color: Color.secondary, value: latency, unit: "ms")
            }
            if let failure {
                Text(failure)
                    .font(.footnote)
                    .foregroundStyle(.red)
                    .multilineTextAlignment(.center)
            } else if !caption.isEmpty {
                Text(caption)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, 20)
        .padding(.horizontal, 12)
        .frame(maxWidth: .infinity)
        .background(Surface.card, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
    }
}

/// The same three columns while a test runs: the one being measured is
/// live, the others wait.
private struct SpeedTestProgressPanel: View {
    let progress: SpeedTestProgress

    private func figure(_ phase: String) -> String {
        progress.phase == phase ? speedNumber(progress.mbps) : "–"
    }

    var body: some View {
        VStack(spacing: 14) {
            HStack(spacing: 0) {
                SpeedFigure(symbol: "arrow.down", color: TrafficSeries.downloadColor, value: figure("download"), unit: "Mbps", active: progress.phase == "download")
                SpeedFigure(symbol: "arrow.up", color: TrafficSeries.uploadColor, value: figure("upload"), unit: "Mbps", active: progress.phase == "upload")
                SpeedFigure(symbol: "stopwatch", color: Color.secondary, value: "–", unit: "ms", active: progress.phase == "latency")
            }
            ProgressView()
        }
        .padding(.vertical, 20)
        .padding(.horizontal, 12)
        .frame(maxWidth: .infinity)
        .background(Surface.card, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
    }
}

private struct SpeedTestHistorySection: View {
    let history: [SpeedTestResult]
    let loaded: Bool

    var body: some View {
        if !loaded {
            Section {
                ProgressView()
            }
        } else if !history.isEmpty {
            Section("Earlier results") {
                ForEach(Array(history.enumerated()), id: \.offset) { item in
                    SpeedTestHistoryRow(result: item.element)
                }
            }
        }
    }
}

private struct SpeedTestHistoryRow: View {
    @EnvironmentObject private var model: AppModel
    let result: SpeedTestResult

    private var failed: Bool {
        guard let error = result.error else { return false }
        return !error.isEmpty
    }

    private var figures: String {
        "↓ " + speedNumber(result.downMbps) + "  ↑ " + speedNumber(result.upMbps)
    }

    var body: some View {
        HStack(spacing: 8) {
            VStack(alignment: .leading, spacing: 2) {
                Text(formatDate(result.t))
                    .font(.subheadline)
                if result.via != "direct" {
                    ActionBadge(action: result.via)
                }
            }
            Spacer(minLength: 8)
            if failed {
                Badge(text: "Failed", color: Color.red)
            } else {
                VStack(alignment: .trailing, spacing: 2) {
                    Text(figures)
                        .font(.subheadline)
                        .monospacedDigit()
                    Text(formatLatency(result.latencyMs))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }
}
