import PorterKit
import SwiftUI

/// The internet source a smoothing page is about, and with it where the
/// setting is kept: with the wired port, with the phone, or, for Wi-Fi,
/// with the saved network (a hotel's line and the one at home differ).
struct SmoothingTarget: Equatable {
    /// The source's key in the order of sources.
    var key: String
    /// For Wi-Fi, the saved network.
    var ssid: String? = nil
}

/// The row on a source's page that leads to its smoothing page.
struct SmoothingLink: View {
    @EnvironmentObject private var model: AppModel
    let target: SmoothingTarget

    private var on: Bool {
        model.config?.shaping(source: target.key, ssid: target.ssid).enabled ?? false
    }

    var body: some View {
        NavigationLink {
            SmoothingPage(title: "Smooth out heavy use", heading: false, target: target)
        } label: {
            LabeledContent("Smooth out heavy use", value: on ? "On" : "Off")
        }
    }
}

/// Holding one internet source a little below its real speed, so that a
/// big upload or download does not make everything else wait: on or off,
/// the two speeds, and a button that has the router measure them.
struct SmoothingPage: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let title: String
    /// The page is the source's own, so the setting gets its name as a
    /// heading; where the page is named after the setting it does not.
    let heading: Bool
    let target: SmoothingTarget

    @State private var enabled = false
    @State private var down: String = ""
    @State private var up: String = ""
    @State private var loaded = false
    @State private var saving = false
    @State private var measuring = false
    @State private var measureProblem: String? = nil

    private var stored: ShapingConfig {
        model.config?.shaping(source: target.key, ssid: target.ssid) ?? ShapingConfig()
    }

    private var uplink: UplinkStatus? {
        let sources: [UplinkStatus] = model.status?.wan ?? []
        return sources.first(where: { $0.key == target.key })
    }

    /// For Wi-Fi the setting is one network's: what the router says of
    /// its Wi-Fi source is about this page only while it is on that
    /// network.
    private var onThisNetwork: Bool {
        guard let ssid = target.ssid else { return true }
        let stations: [StationStatus] = model.status?.stas ?? []
        return stations.contains(where: { $0.state == "connected" && $0.ssid == ssid })
    }

    /// The source is there to be measured.
    private var connected: Bool {
        guard onThisNetwork, let uplink, uplink.carrier else { return false }
        let address: String = uplink.ipv4 ?? ""
        return !address.isEmpty || uplink.hasV6
    }

    private var held: ShapingStatus? {
        guard onThisNetwork else { return nil }
        let entries: [ShapingStatus] = model.status?.shaping ?? []
        return entries.first(where: { $0.key == target.key })
    }

    private var downKbps: Int? { ShapingConfig.kbps(mbpsText: down) }
    private var upKbps: Int? { ShapingConfig.kbps(mbpsText: up) }

    private func acceptable(_ kbps: Int?) -> Bool {
        guard let kbps else { return false }
        return kbps == 0 || ShapingConfig.accepts(kbps: kbps)
    }

    /// What Save would write; nil while a speed is not one.
    private var result: ShapingConfig? {
        guard let downKbps, let upKbps, acceptable(downKbps), acceptable(upKbps) else { return nil }
        return ShapingConfig(enabled: enabled, downKbps: downKbps, upKbps: upKbps)
    }

    private var canSave: Bool {
        guard loaded, !saving, !measuring, let next = result else { return false }
        if next == stored { return false }
        return !next.enabled || (next.downKbps > 0 && next.upKbps > 0)
    }

    private var canMeasure: Bool {
        connected && !saving && model.speedTestProgress == nil
    }

    /// Under the speeds: what is wrong with them, or else why they
    /// matter and where they can come from.
    private var speedsNote: String {
        var note: String = "The speeds have to be what this source really delivers. Set too high, this does nothing. Set too low, the connection is slower than it could be."
        if connected {
            note += " Tap Measure to have the router test this source, which takes about 20 seconds, and fill in a little less than the result."
        } else if target.ssid != nil {
            note += " The router can measure them while it has joined this network."
        } else {
            note += " The router can measure them while this source is connected."
        }
        return note
    }

    private var measuringLabel: String {
        switch model.speedTestProgress?.phase ?? "" {
        case "download": return "Measuring download"
        case "upload": return "Measuring upload"
        default: return "Measuring"
        }
    }

    var body: some View {
        Form {
            Section {
                Toggle("Enabled", isOn: $enabled)
            } header: {
                if heading {
                    Text("Smooth out heavy use")
                }
            } footer: {
                Text("A big upload or download no longer makes calls stutter and pages hang on your other devices. It costs a few per cent of this source's top speed.")
            }
            if enabled {
                speedsSection
            }
            if stored.enabled, let held {
                SmoothingStatusSection(held: held)
            }
        }
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { commit() }
                    .disabled(!canSave)
            }
        }
        .onAppear { load() }
        .task { await poll() }
    }

    private var speedsSection: some View {
        Section {
            SpeedField(label: "Download", text: $down)
            SpeedField(label: "Upload", text: $up)
            if measuring {
                HStack {
                    Text(measuringLabel)
                        .foregroundStyle(.secondary)
                    Spacer()
                    ProgressView()
                }
            } else {
                Button("Measure") { measure() }
                    .disabled(!canMeasure)
            }
        } header: {
            Text("Speeds")
        } footer: {
            if !acceptable(downKbps) || !acceptable(upKbps) {
                Text("A speed is a number from 0.1 to 10000 Mbit/s.")
                    .foregroundStyle(.red)
            } else if let problem = measureProblem {
                Text(problem)
                    .foregroundStyle(.red)
            } else {
                Text(speedsNote)
            }
        }
        .disabled(measuring)
    }

    private func load() {
        guard !loaded else { return }
        loaded = true
        let initial: ShapingConfig = stored
        enabled = initial.enabled
        down = ShapingConfig.mbpsText(kbps: initial.downKbps)
        up = ShapingConfig.mbpsText(kbps: initial.upKbps)
    }

    /// The counters are not announced by the router as they move: they
    /// are read again every few seconds while the page shows them.
    private func poll() async {
        while !Task.isCancelled {
            do {
                try await Task.sleep(nanoseconds: 5_000_000_000)
            } catch {
                return
            }
            if stored.enabled {
                await model.refreshStatus()
            }
        }
    }

    private func measure() {
        let path: String = RouterConfig.speedTestPath(source: target.key)
        measuring = true
        measureProblem = nil
        Task {
            let found: SpeedTestResult? = await model.runSpeedTest(via: path)
            measuring = false
            guard let found else { return }
            // Whatever was measured before a test failed is still worth
            // having; the rest is left as it was.
            let downSpeed: Int = ShapingConfig.speed(measuredMbps: found.downMbps)
            let upSpeed: Int = ShapingConfig.speed(measuredMbps: found.upMbps)
            if downSpeed > 0 {
                down = ShapingConfig.mbpsText(kbps: downSpeed)
            }
            if upSpeed > 0 {
                up = ShapingConfig.mbpsText(kbps: upSpeed)
            }
            if let problem = found.error, !problem.isEmpty {
                measureProblem = "The speed test did not finish: \(problem)"
            }
        }
    }

    private func commit() {
        guard let next = result else { return }
        let key: String = target.key
        let ssid: String? = target.ssid
        saving = true
        Task {
            let ok = await model.update { cfg in
                cfg.setShaping(next, source: key, ssid: ssid)
            }
            saving = false
            if ok {
                dismiss()
            }
        }
    }
}

/// A speed, typed in Mbit/s.
private struct SpeedField: View {
    let label: String
    @Binding var text: String

    var body: some View {
        LabeledContent(label) {
            HStack(spacing: 6) {
                TextField(label, text: $text, prompt: Text(verbatim: "0"))
                    .multilineTextAlignment(.trailing)
                    .keyboardType(.decimalPad)
                Text("Mbit/s")
                    .foregroundStyle(.secondary)
            }
        }
    }
}

/// How smoothing stands for a source that has it switched on: that it is
/// at work, with the counts that show it, or why it is not.
private struct SmoothingStatusSection: View {
    let held: ShapingStatus

    private func times(_ count: UInt64) -> String {
        count == 1 ? "1 time" : "\(count.formatted()) times"
    }

    var body: some View {
        switch held.state {
        case "active":
            Section {
                LabeledContent("Downloads", value: times(held.down.slowed))
                LabeledContent("Uploads", value: times(held.up.slowed))
            } header: {
                Text("Slowed down")
            } footer: {
                Text("How often the router has made a transfer slow down since this source connected. If these stay at zero while something big is downloading or uploading, the speeds are set too high.")
            }
        case "measuring":
            Section {
                Text("Paused while the router measures this source.")
                    .foregroundStyle(.secondary)
            }
        case "error":
            Section {
                Text("The router could not switch this on.")
                    .foregroundStyle(.red)
            }
        default:
            Section {
                Text("Starts when this source connects.")
                    .foregroundStyle(.secondary)
            }
        }
    }
}
