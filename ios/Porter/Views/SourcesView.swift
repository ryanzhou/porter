import PorterKit
import SwiftUI

// A Route can name one internet source, and a source can have a data
// limit. What is here is shared by every place a Route is chosen (a rule,
// a device, a group, a network, "Everything else"), and by the pages of
// the sources on the Internet tab.

extension AppModel {
    /// What an internet source is called where a Route names it.
    func sourceName(_ key: String) -> String {
        if key == RouterConfig.uplinkWifi { return "Wi-Fi" }
        if key == RouterConfig.uplinkPhone { return "Phone on USB" }
        let name: String = key.hasPrefix("eth:") ? String(key.dropFirst(4)) : key
        return severalWiredSources ? "Ethernet (\(name))" : "Ethernet"
    }

    /// The same inside a sentence.
    func sourcePhrase(_ key: String) -> String {
        if key == RouterConfig.uplinkWifi { return "the Wi-Fi network the router joins" }
        if key == RouterConfig.uplinkPhone { return "the phone on USB" }
        let name: String = key.hasPrefix("eth:") ? String(key.dropFirst(4)) : key
        return severalWiredSources ? "the wired internet port \(name)" : "the wired internet port"
    }

    func sourceSymbol(_ key: String) -> String {
        if key == RouterConfig.uplinkWifi { return uplinkSymbol(kind: "wifi") }
        if key == RouterConfig.uplinkPhone { return uplinkSymbol(kind: "phone") }
        return uplinkSymbol(kind: "eth")
    }

    /// More than one wired port is an internet port, so a name has to
    /// say which.
    private var severalWiredSources: Bool {
        let ports: [EthernetPort] = config?.ethernet ?? []
        return ports.filter { $0.role == .wan }.count > 1
    }

    /// What happens to traffic with a Route that names an internet
    /// source: where it goes now if the source is not available, and
    /// otherwise what it will do when the source is not.
    func routeNote(_ action: RouteAction, for subject: RouteSubject) -> String {
        guard case .source(let key, let only) = action else { return "" }
        let name: String = sourcePhrase(key)
        let named: String = name.prefix(1).uppercased() + String(name.dropFirst())
        let then: String = only ? subject.offline : subject.fallsBack
        let state: SourceAvailability = status?.availability(source: key) ?? .away
        switch state {
        case .dataSpent:
            if only {
                return "\(named) has reached its data limit, so \(then). The limit can be lifted on the Internet tab."
            }
            return "\(named) has reached its data limit, so \(then) for now."
        case .away:
            return only ? "\(named) is not connected, so \(then)." : "\(named) is not connected, so \(then) for now."
        case .connected:
            if only {
                return "\(named) is connected but does not reach the internet right now."
            }
            return "\(named) does not reach the internet right now, so \(then) for now."
        case .online:
            let limited: Bool = config?.dataLimit(source: key, ssid: status?.joinedNetwork).enabled ?? false
            let limit: String = limited ? ", or has reached its data limit" : ""
            if only {
                return "While \(name) is not connected\(limit), \(then)."
            }
            return "While \(name) is not available\(limit), \(then)."
        }
    }
}

/// What a Route is being chosen for, for the sentence that says what
/// happens while its internet source is not available.
enum RouteSubject {
    case device, group, network, rule, everythingElse

    var offline: String {
        switch self {
        case .device: return "this device is offline"
        case .group: return "the devices in this group are offline"
        case .network: return "devices on this network are offline"
        case .rule: return "what this rule applies to does not go online"
        case .everythingElse: return "everything else is offline"
        }
    }

    var fallsBack: String {
        switch self {
        case .device: return "this device uses whichever internet source is in use"
        case .group: return "the devices in this group use whichever internet source is in use"
        case .network: return "devices on this network use whichever internet source is in use"
        case .rule: return "what this rule applies to uses whichever internet source is in use"
        case .everythingElse: return "everything else uses whichever internet source is in use"
        }
    }
}

/// The Route menu: Direct, the internet sources, the VPNs, Blocked. A
/// source is one entry however it is used; whether it is the only one is
/// the switch in `RouteSourceRows`.
struct RoutePicker: View {
    @EnvironmentObject private var model: AppModel
    let title: String
    @Binding var selection: String
    /// The entry for no Route of its own, where there is such a thing.
    var defaultLabel: String? = nil

    private var choice: Binding<String> {
        Binding(
            get: { RouteAction.choice(for: selection) },
            set: { picked in selection = RouteAction.picked(picked, current: selection) }
        )
    }

    /// Every Route there is to choose, and the one chosen if it is none
    /// of those: a source that is no longer one.
    private var choices: [RouteAction] {
        var list: [RouteAction] = model.availableActions
        if let current = RouteAction(RouteAction.choice(for: selection)), !list.contains(current) {
            list.insert(current, at: min(1, list.count))
        }
        return list
    }

    var body: some View {
        Picker(title, selection: choice) {
            if let defaultLabel {
                Text(defaultLabel).tag("")
            }
            ForEach(choices, id: \.self) { action in
                RouteChoiceLabel(action: action).tag(action.raw)
            }
        }
    }
}

/// One entry of the Route menu: an internet source has its symbol.
private struct RouteChoiceLabel: View {
    @EnvironmentObject private var model: AppModel
    let action: RouteAction

    var body: some View {
        if let key = action.sourceKey {
            Label(model.sourceName(key), systemImage: model.sourceSymbol(key))
        } else {
            Text(model.actionLabel(action.raw))
        }
    }
}

/// The rows under a Route that names an internet source: the switch that
/// makes it the only one, and what that means. Nothing for any other
/// Route.
struct RouteSourceRows: View {
    @EnvironmentObject private var model: AppModel
    @Binding var selection: String
    let subject: RouteSubject

    private var only: Binding<Bool> {
        Binding(
            get: { RouteAction(selection)?.onlySource ?? false },
            set: { on in
                if let action = RouteAction(selection) {
                    selection = action.onlySource(on).raw
                }
            }
        )
    }

    var body: some View {
        if let action = RouteAction(selection), action.sourceKey != nil {
            Toggle("Only this source", isOn: only)
            Text(model.routeNote(action, for: subject))
                .font(.footnote)
                .foregroundStyle(.secondary)
        }
    }
}

/// How a data limit reads in one line: "5 GB a month", "500 MB a day".
func dataLimitSummary(_ limit: DataLimit) -> String {
    let amount: String = DataLimit.amountText(bytes: UInt64(max(limit.mb, 0)) * 1_000_000)
    return amount + (limit.isDaily ? " a day" : " a month")
}

/// A day, as the end of a period is named: "1 December".
func dayAndMonth(_ unix: Int64) -> String {
    let formatter = DateFormatter()
    formatter.setLocalizedDateFormatFromTemplate("dMMMM")
    return formatter.string(from: Date(timeIntervalSince1970: TimeInterval(unix)))
}

/// The row on a source's page that leads to its data limit.
struct DataLimitLink: View {
    @EnvironmentObject private var model: AppModel
    let target: SmoothingTarget

    private var limit: DataLimit {
        model.config?.dataLimit(source: target.key, ssid: target.ssid) ?? DataLimit()
    }

    var body: some View {
        NavigationLink {
            DataLimitPage(target: target)
        } label: {
            LabeledContent("Data limit", value: limit.enabled ? dataLimitSummary(limit) : "Off")
        }
    }
}

/// What is set for a phone sharing its connection over USB, whichever
/// phone it is.
struct PhoneSourcePage: View {
    private let target = SmoothingTarget(key: RouterConfig.uplinkPhone)

    var body: some View {
        Form {
            Section {
                DataLimitLink(target: target)
                SmoothingLink(target: target)
            } footer: {
                Text("These apply to whichever phone is plugged in.")
            }
        }
        .navigationTitle("Phone on USB")
        .navigationBarTitleDisplayMode(.inline)
    }
}

/// The data limit of one internet source: how much it may use in a day
/// or a month, what it has used, and using it anyway once that is spent.
struct DataLimitPage: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let target: SmoothingTarget

    @State private var enabled = false
    @State private var daily = false
    @State private var amount: String = ""
    @State private var startDay: Int = 1
    @State private var loaded = false
    @State private var saving = false

    private var stored: DataLimit {
        model.config?.dataLimit(source: target.key, ssid: target.ssid) ?? DataLimit()
    }

    private var usage: DataStatus? {
        guard let ssid = target.ssid else { return model.status?.data(source: target.key) }
        return model.status?.data(source: target.key, ssid: ssid)
    }

    private var amountMB: Int? {
        if amount.trimmingCharacters(in: .whitespaces).isEmpty { return 0 }
        return DataLimit.mb(gbText: amount)
    }

    private var amountOK: Bool {
        guard let amountMB else { return false }
        return amountMB == 0 || DataLimit.accepts(mb: amountMB)
    }

    /// What Save would write; nil while the amount is not one.
    private var result: DataLimit? {
        guard let amountMB, amountOK else { return nil }
        var next: DataLimit = stored
        next.enabled = enabled
        next.period = daily ? "day" : "month"
        next.mb = amountMB
        next.startDay = startDay
        return next
    }

    private var canSave: Bool {
        guard loaded, !saving, let next = result else { return false }
        if next == stored { return false }
        return !next.enabled || next.mb > 0
    }

    var body: some View {
        Form {
            Section {
                Toggle("Enabled", isOn: $enabled)
            } footer: {
                Text("The router counts the data this source carries. Once the amount is used, it stops using the source until the next day or month begins, and goes online through the next source in the list.")
            }
            if enabled {
                amountSection
            }
            if stored.enabled, let usage {
                usageSection(usage)
            }
        }
        .navigationTitle("Data limit")
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

    private var amountSection: some View {
        Section {
            LabeledContent("Amount") {
                HStack(spacing: 6) {
                    TextField("Amount", text: $amount, prompt: Text(verbatim: "0"))
                        .multilineTextAlignment(.trailing)
                        .keyboardType(.decimalPad)
                    Text("GB")
                        .foregroundStyle(.secondary)
                }
            }
            Picker("Counted for", selection: $daily) {
                Text("Each month").tag(false)
                Text("Each day").tag(true)
            }
            if !daily {
                Picker("Month starts on day", selection: $startDay) {
                    ForEach(1...31, id: \.self) { day in
                        Text(String(day)).tag(day)
                    }
                }
            }
        } footer: {
            if !amountOK {
                Text("An amount is a number from 0.001 to 10000 GB.")
                    .foregroundStyle(.red)
            } else if !daily && startDay > 28 {
                Text("In a month without that day, the limit starts over on its last day.")
            }
        }
    }

    @ViewBuilder
    private func usageSection(_ usage: DataStatus) -> some View {
        Section {
            VStack(alignment: .leading, spacing: 8) {
                ProgressView(value: usage.fraction)
                    .tint(usage.reached ? Color.orange : Color.accentColor)
                Text("\(DataLimit.amountText(bytes: usage.used)) of \(DataLimit.amountText(bytes: usage.limit)) used")
                    .font(.subheadline)
            }
            LabeledContent("Starts over", value: usage.period == "day" ? "Tomorrow" : dayAndMonth(usage.resets))
            if usage.spent {
                Button("Use anyway until then") { lift(until: usage.resets) }
                    .disabled(saving)
            } else if usage.lifted {
                Button("Stop using it until then") { lift(until: 0) }
                    .disabled(saving)
            }
        } header: {
            Text(usage.period == "day" ? "Today" : "This month")
        } footer: {
            if usage.spent {
                Text("The limit is reached. The router does not use this source until it starts over. Devices with a Route through only this source are offline until then.")
            } else if usage.lifted {
                Text("The limit is reached, and the source is used anyway until it starts over.")
            }
        }
    }

    private func load() {
        guard !loaded else { return }
        loaded = true
        let initial: DataLimit = stored
        enabled = initial.enabled
        daily = initial.isDaily
        amount = initial.mb > 0 ? DataLimit.gbText(mb: initial.mb) : ""
        startDay = min(max(initial.startDay, 1), 31)
    }

    /// What has been used is not announced by the router as it grows: it
    /// is read again every few seconds while the page shows it.
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

    private func lift(until: Int64) {
        let key: String = target.key
        let ssid: String? = target.ssid
        saving = true
        Task {
            _ = await model.update { cfg in
                var limit: DataLimit = cfg.dataLimit(source: key, ssid: ssid)
                limit.liftUntil = until
                cfg.setDataLimit(limit, source: key, ssid: ssid)
            }
            saving = false
        }
    }

    private func commit() {
        guard let next = result else { return }
        let key: String = target.key
        let ssid: String? = target.ssid
        saving = true
        Task {
            let ok = await model.update { cfg in
                cfg.setDataLimit(next, source: key, ssid: ssid)
            }
            saving = false
            if ok {
                dismiss()
            }
        }
    }
}
