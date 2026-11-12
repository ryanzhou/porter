import PorterKit
import SwiftUI

// Groups of devices: the kids' devices, work, the things that are off at
// night. A group has what makes sense for several devices at once (Route,
// pausing, skipping ad blocking) and schedules, the times its devices
// have no internet access.

// MARK: - Pictures

/// The pictures a group can have. The router keeps the key and nothing
/// more.
struct GroupIcon: Identifiable {
    let key: String
    let symbol: String
    let color: Color
    var id: String { key }

    static let all: [GroupIcon] = [
        GroupIcon(key: "people", symbol: "person.2.fill", color: Color.accentColor),
        GroupIcon(key: "child", symbol: "figure.and.child.holdinghands", color: Color.orange),
        GroupIcon(key: "work", symbol: "briefcase.fill", color: Color.brown),
        GroupIcon(key: "games", symbol: "gamecontroller.fill", color: Color.purple),
        GroupIcon(key: "tv", symbol: "tv.fill", color: Color.indigo),
        GroupIcon(key: "home", symbol: "house.fill", color: Color.green),
        GroupIcon(key: "school", symbol: "book.fill", color: Color.teal),
        GroupIcon(key: "things", symbol: "lightbulb.fill", color: Color.yellow),
    ]

    /// The picture for a key; the first for one this version of the app
    /// does not know.
    static func named(_ key: String) -> GroupIcon {
        all.first(where: { $0.key == key }) ?? all[0]
    }
}

// MARK: - Times

extension AppModel {
    /// The time zone the router keeps schedules in; the phone's while
    /// the router has none.
    var routerTimeZone: TimeZone {
        TimeZone(identifier: config?.system.timezone ?? "") ?? TimeZone.current
    }

    /// A moment as a clock where the router is shows it: "07:00", and
    /// with the day in front when that is not within the next day.
    func clockText(_ unix: Int64) -> String {
        let date = Date(timeIntervalSince1970: TimeInterval(unix))
        let formatter = DateFormatter()
        formatter.timeZone = routerTimeZone
        let far: Bool = date.timeIntervalSinceNow > 23 * 3600
        formatter.setLocalizedDateFormatFromTemplate(far ? "EEEjmm" : "jmm")
        return formatter.string(from: date)
    }

    /// What pauses a device's internet access right now, if anything.
    func held(_ mac: String) -> HeldDevice? { status?.held(mac) }

    /// How a group stands, in a few words for a list: "Paused until
    /// 07:00", "Allowed until 22:00", "Bedtime at 21:00". Empty when
    /// there is nothing to say.
    func groupStateText(_ group: DeviceGroup) -> String {
        guard let state = status?.group(group.id) else { return group.paused ? "Paused" : "" }
        if state.offline {
            if let until = state.until { return "Paused until " + clockText(until) }
            return "Paused"
        }
        if let until = state.allowedUntil { return "Allowed until " + clockText(until) }
        if let next = state.next {
            return scheduleName(group, state.nextSchedule) + " at " + clockText(next)
        }
        return ""
    }

    /// What a schedule is called: its name, or "Schedule" without one.
    func scheduleName(_ group: DeviceGroup, _ id: String?) -> String {
        let name: String = group.schedules.first(where: { $0.id == id })?.name ?? ""
        return name.isEmpty ? "Schedule" : name
    }
}

/// A time of day as the router writes it ("21:00"), the way this phone
/// shows times.
func timeOfDayText(_ time: String) -> String {
    guard let date = timeOfDay(time) else { return time }
    return date.formatted(date: .omitted, time: .shortened)
}

/// A time of day as a date the time pickers can work on. Only its hour
/// and minute mean anything.
func timeOfDay(_ time: String) -> Date? {
    guard let parts = GroupSchedule.parts(of: time) else { return nil }
    return Calendar.current.date(bySettingHour: parts.hour, minute: parts.minute, second: 0, of: Date(timeIntervalSince1970: 86_400 * 365))
}

/// The time of day of a date from a time picker, as the router writes it.
func timeOfDay(_ date: Date) -> String {
    let parts: DateComponents = Calendar.current.dateComponents([.hour, .minute], from: date)
    return GroupSchedule.time(hour: parts.hour ?? 0, minute: parts.minute ?? 0)
}

/// A schedule in one line: "Sunday to Thursday, 21:00 to 07:00".
func scheduleText(_ schedule: GroupSchedule) -> String {
    schedule.daysText + ", " + timeOfDayText(schedule.from) + " to " + timeOfDayText(schedule.to)
}

// MARK: - The list on the Devices tab

/// The groups, first on the Devices tab: each with how many of its
/// devices are online and whether it is paused right now.
struct GroupsSection: View {
    @EnvironmentObject private var model: AppModel
    @Binding var adding: Bool

    private var groups: [DeviceGroup] { model.config?.groupList ?? [] }

    var body: some View {
        Section {
            ForEach(groups) { group in
                NavigationLink {
                    GroupDetailView(groupID: group.id)
                } label: {
                    GroupRow(group: group)
                }
            }
            if groups.count < RouterConfig.maxGroups {
                Button {
                    adding = true
                } label: {
                    Label("New group", systemImage: "plus")
                }
            }
        } header: {
            Text("Groups")
        } footer: {
            if groups.isEmpty {
                Text("A group gives several devices the same settings, and can pause their internet access at set times, such as bedtime.")
            }
        }
    }
}

private struct GroupRow: View {
    @EnvironmentObject private var model: AppModel
    let group: DeviceGroup

    private var state: GroupStatus? { model.status?.group(group.id) }

    private var paused: Bool { state?.offline ?? group.paused }

    private var members: String {
        let count: Int = group.members.count
        if count == 0 { return "No devices yet" }
        let online: Int = state?.connected ?? 0
        return "\(online) of \(count) online"
    }

    var body: some View {
        let icon: GroupIcon = GroupIcon.named(group.icon)
        let stateText: String = model.groupStateText(group)
        HStack(spacing: 12) {
            Glyph(symbol: paused ? "pause.fill" : icon.symbol, color: paused ? Color.orange : icon.color)
            VStack(alignment: .leading, spacing: 2) {
                Text(group.name)
                    .lineLimit(1)
                HStack(spacing: 6) {
                    Text(members)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    if !stateText.isEmpty {
                        Badge(text: stateText, color: paused ? Color.orange : Color.secondary)
                    }
                    if !group.route.isEmpty {
                        Badge(text: model.actionLabel(group.route), color: Color.blue)
                    }
                }
            }
        }
    }
}

/// Names a new group and gives it a picture.
struct NewGroupSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var name: String = ""
    @State private var icon: String = GroupIcon.all[0].key
    @State private var saving = false

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }

    var body: some View {
        Form {
            Section {
                TextField("Name", text: $name)
                GroupIconPicker(selection: $icon)
            } footer: {
                if let problem = groupNameProblem(name) {
                    Text(problem)
                        .foregroundStyle(.red)
                }
            }
        }
        .navigationTitle("New group")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel") { dismiss() }
            }
            ToolbarItem(placement: .confirmationAction) {
                Button("Add") { add() }
                    .disabled(cleanName.isEmpty || groupNameProblem(name) != nil || saving)
            }
        }
    }

    private func add() {
        let group = DeviceGroup(id: AppModel.newID(), name: cleanName, icon: icon)
        saving = true
        Task {
            let ok: Bool = await model.update { cfg in
                cfg.groups = cfg.groupList + [group]
            }
            saving = false
            if ok { dismiss() }
        }
    }
}

/// The router takes 40 characters for a group's or a schedule's name:
/// said once what was typed is longer.
func groupNameProblem(_ text: String) -> String? {
    if text.trimmingCharacters(in: .whitespaces).utf8.count <= 40 { return nil }
    return "This name is too long. Use at most 40 characters."
}

private struct GroupIconPicker: View {
    @Binding var selection: String

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 10) {
                ForEach(GroupIcon.all) { icon in
                    Button {
                        selection = icon.key
                    } label: {
                        Glyph(symbol: icon.symbol, color: icon.color, size: 38, filled: selection == icon.key)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(Text(icon.key))
                }
            }
            .padding(.vertical, 2)
        }
    }
}

// MARK: - One group

struct GroupDetailView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let groupID: String

    @State private var name: String = ""
    @State private var original: String? = nil
    @State private var editing: GroupSchedule? = nil

    private var group: DeviceGroup? { model.config?.group(groupID) }
    private var state: GroupStatus? { model.status?.group(groupID) }

    private var cleanName: String { name.trimmingCharacters(in: .whitespaces) }

    private var dirty: Bool {
        guard let original else { return false }
        return cleanName != original && !cleanName.isEmpty && groupNameProblem(name) == nil
    }

    var body: some View {
        Form {
            if let group {
                nameSection(group)
                nowSection(group)
                schedulesSection(group)
                settingsSection(group)
                membersSection(group)
                Section {
                    ConfirmedButton(title: "Delete group", message: deleteMessage, confirmTitle: "Delete") {
                        delete()
                    }
                }
            } else {
                ContentUnavailableView("Group deleted", systemImage: "questionmark.circle")
            }
        }
        .navigationTitle(group?.name ?? "Group")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
                    .disabled(!dirty)
            }
        }
        .onAppear { sync() }
        .onChange(of: group?.name) { _, _ in sync() }
        .sheet(item: $editing) { schedule in
            NavigationStack {
                ScheduleEditor(groupID: groupID, schedule: schedule)
            }
            .modifier(SheetErrorAlert())
            .environmentObject(model)
        }
    }

    /// What deleting takes with it.
    private var deleteMessage: String {
        var text: String = "Its schedules are removed with it. Its devices stay, each with its own settings."
        let rules: [PolicyRule] = model.config?.rulesOnly(forGroup: groupID) ?? []
        if let first = rules.first {
            let named: String = first.name.isEmpty ? "A rule" : "The rule “" + first.name + "”"
            if rules.count == 1 {
                text += " " + named + " on the VPN tab applies to this group only and is removed too."
            } else {
                text += " \(rules.count) rules on the VPN tab apply to this group only and are removed too."
            }
        }
        return text
    }

    // MARK: Sections

    @ViewBuilder
    private func nameSection(_ group: DeviceGroup) -> some View {
        Section {
            TextField("Name", text: $name)
            GroupIconPicker(selection: iconBinding)
        } footer: {
            if let problem = groupNameProblem(name) {
                Text(problem)
                    .foregroundStyle(.red)
            }
        }
    }

    /// How the group stands right now, and what can be done about it
    /// for now.
    @ViewBuilder
    private func nowSection(_ group: DeviceGroup) -> some View {
        Section {
            if let state, state.offline, state.why != "paused" {
                pausedRows(group, state)
            } else if let state, let until = state.allowedUntil {
                let time: String = model.clockText(until)
                LabeledContent("Allowed until", value: time)
                Button("Stop allowing") { setForNow(GroupOverride()) }
            }
            Toggle("Pause internet access", isOn: pausedBinding)
            if state?.offline != true {
                Menu("Pause for a while") {
                    Button("30 minutes") { pause(minutes: 30) }
                    Button("1 hour") { pause(minutes: 60) }
                    Button("2 hours") { pause(minutes: 120) }
                }
            }
        } footer: {
            Text(nowNote(group))
        }
    }

    /// A schedule or a timer has the group paused: until when, and the
    /// ways to let it online before then.
    @ViewBuilder
    private func pausedRows(_ group: DeviceGroup, _ state: GroupStatus) -> some View {
        let bySchedule: Bool = state.why == "schedule"
        let what: String = bySchedule ? model.scheduleName(group, state.schedule) : "Paused"
        if let until = state.until {
            let time: String = model.clockText(until)
            LabeledContent(what, value: "Until \(time)")
        } else {
            LabeledContent(what, value: "In force")
        }
        if bySchedule {
            if let until = state.until {
                let time: String = model.clockText(until)
                Button("Allow until \(time)") { setForNow(GroupOverride.allow(until: until)) }
            }
            Menu("Allow for a while") {
                Button("30 minutes") { setForNow(GroupOverride.allow(until: later(minutes: 30))) }
                Button("1 hour") { setForNow(GroupOverride.allow(until: later(minutes: 60))) }
                Button("2 hours") { setForNow(GroupOverride.allow(until: later(minutes: 120))) }
            }
        } else {
            Button("Resume now") { setForNow(GroupOverride()) }
        }
    }

    /// What is worth knowing under the switch: when a schedule next
    /// pauses the group, and that the router is unsure of the time.
    private func nowNote(_ group: DeviceGroup) -> String {
        var notes: [String] = []
        if let state, !state.offline, state.allowedUntil == nil, let next = state.next {
            notes.append(model.scheduleName(group, state.nextSchedule) + " pauses this group at " + model.clockText(next) + ".")
        }
        if let clock = model.status?.clock, !clock.trusted, group.schedules.contains(where: { $0.enabled }), let time = model.status?.time {
            notes.append("The router has not been able to check the time since it started. Until it has, schedules go by the time it last knew, " + model.clockText(time) + ".")
        }
        return notes.joined(separator: " ")
    }

    @ViewBuilder
    private func schedulesSection(_ group: DeviceGroup) -> some View {
        let zone: String = model.config?.system.timezone ?? ""
        Section {
            ForEach(group.schedules) { schedule in
                Button {
                    editing = schedule
                } label: {
                    HStack {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(schedule.name.isEmpty ? "Schedule" : schedule.name)
                                .foregroundStyle(Color.primary)
                            Text(scheduleText(schedule))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        Spacer(minLength: 4)
                        if !schedule.enabled {
                            Badge(text: "Off")
                        }
                    }
                }
            }
            if group.schedules.count < RouterConfig.maxSchedules && !zone.isEmpty {
                Button {
                    editing = GroupSchedule.bedtime(id: AppModel.newID())
                } label: {
                    Label("Add a schedule", systemImage: "plus")
                }
            }
        } header: {
            Text("Schedules")
        } footer: {
            if zone.isEmpty {
                Text("Schedules need to know what time it is where you are. Choose your time zone in Settings first.")
            } else {
                Text("During a schedule the devices in this group have no internet access. The times are those of the router's time zone, \(zoneLabel(zone)).")
            }
        }
    }

    @ViewBuilder
    private func settingsSection(_ group: DeviceGroup) -> some View {
        Section {
            Picker("Route", selection: routeBinding) {
                Text("Default").tag("")
                ForEach(model.availableActions, id: \.self) { action in
                    Text(model.actionLabel(action.raw)).tag(action.raw)
                }
            }
            if model.config?.dns.adblock.enabled == true {
                Toggle("Skip ad blocking", isOn: noAdblockBinding)
            }
        } footer: {
            Text("A device that has a route of its own keeps it. A rule on the VPN tab that fits the traffic takes priority over both.")
        }
    }

    @ViewBuilder
    private func membersSection(_ group: DeviceGroup) -> some View {
        Section {
            ForEach(group.members, id: \.self) { mac in
                NavigationLink {
                    DeviceDetailView(mac: mac)
                } label: {
                    MemberRow(mac: mac)
                }
            }
            .onDelete { offsets in
                let leaving: [String] = offsets.map { group.members[$0] }
                edit { changed in changed.members.removeAll(where: { leaving.contains($0) }) }
            }
            NavigationLink("Choose devices") {
                GroupMembersPage(groupID: groupID)
            }
        } header: {
            Text("Devices")
        }
    }

    // MARK: Bindings and edits

    /// Applies a change to the group at once.
    private func edit(_ change: @escaping (inout DeviceGroup) -> Void) {
        let id: String = groupID
        Task { await model.update { cfg in cfg.editGroup(id, change) } }
    }

    private func setForNow(_ value: GroupOverride) {
        edit { $0.forNow = value }
    }

    private func pause(minutes: Int) {
        let id: String = groupID
        let until: Int64 = later(minutes: minutes)
        Task { await model.update { cfg in cfg.pauseGroup(id, until: until) } }
    }

    /// A while from now, by the router's clock where it has said what
    /// time it has.
    private func later(minutes: Int) -> Int64 {
        let now: Int64 = model.status?.time ?? Int64(Date().timeIntervalSince1970)
        return now + Int64(minutes) * 60
    }

    private var iconBinding: Binding<String> {
        Binding(
            get: { group?.icon ?? "" },
            set: { value in edit { $0.icon = value } }
        )
    }

    private var pausedBinding: Binding<Bool> {
        Binding(
            get: { group?.paused ?? false },
            set: { value in edit { $0.paused = value } }
        )
    }

    private var routeBinding: Binding<String> {
        Binding(
            get: { group?.route ?? "" },
            set: { value in edit { $0.route = value } }
        )
    }

    private var noAdblockBinding: Binding<Bool> {
        Binding(
            get: { group?.noAdblock ?? false },
            set: { value in edit { $0.noAdblock = value } }
        )
    }

    private func sync() {
        guard let current = group?.name else { return }
        if original == nil || cleanName == original {
            name = current
        }
        original = current
    }

    private func save() {
        let newName: String = cleanName
        name = newName
        edit { $0.name = newName }
    }

    private func delete() {
        let id: String = groupID
        Task {
            let ok: Bool = await model.update { cfg in cfg.removeGroup(id) }
            if ok { dismiss() }
        }
    }
}

/// A time zone's name as it is shown: "Europe/Berlin" as it is, less the
/// underscores of "America/New_York".
func zoneLabel(_ zone: String) -> String {
    zone.replacingOccurrences(of: "_", with: " ")
}

private struct MemberRow: View {
    @EnvironmentObject private var model: AppModel
    let mac: String

    /// The device has been let online while its group is paused, until
    /// then.
    private var allowedUntil: Int64? {
        guard let device = model.device(mac), let until = device.allowUntil, let status = model.status,
              until > status.time, let state = status.group(device.group ?? ""), state.offline, state.why != "paused" else { return nil }
        return until
    }

    var body: some View {
        let device: DeviceInfo? = model.device(mac)
        let online: Bool = device?.online ?? false
        HStack(spacing: 8) {
            StatusDot(color: online ? Color.green : Color.gray.opacity(0.4))
            Text(model.deviceName(mac))
                .lineLimit(1)
            Spacer(minLength: 4)
            if let held = model.held(mac), !held.byGroup {
                Badge(text: "Paused", color: Color.orange)
            } else if let until = allowedUntil {
                let time: String = model.clockText(until)
                Badge(text: "Allowed until \(time)")
            }
        }
    }
}

/// Every device, to tick the ones that belong to the group. A device is
/// in one group at most, so ticking one that is in another moves it.
private struct GroupMembersPage: View {
    @EnvironmentObject private var model: AppModel
    let groupID: String

    private var devices: [DeviceInfo] {
        model.devices.sorted { $0.displayName.localizedCaseInsensitiveCompare($1.displayName) == .orderedAscending }
    }

    var body: some View {
        List {
            Section {
                ForEach(devices) { device in
                    Button {
                        toggle(device.mac)
                    } label: {
                        row(device)
                    }
                    .foregroundStyle(.primary)
                }
            } footer: {
                Text("A device is in one group at most. Choosing one that is in another group moves it here.")
            }
        }
        .navigationTitle("Devices")
        .navigationBarTitleDisplayMode(.inline)
    }

    @ViewBuilder
    private func row(_ device: DeviceInfo) -> some View {
        let home: DeviceGroup? = model.config?.group(of: device.mac)
        HStack {
            Text(device.displayName)
                .lineLimit(1)
            Spacer(minLength: 4)
            if let home, home.id != groupID {
                Text(home.name)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else if home != nil {
                Image(systemName: "checkmark")
                    .foregroundStyle(.tint)
            }
        }
    }

    private func toggle(_ mac: String) {
        let id: String = groupID
        let inside: Bool = model.config?.group(of: mac)?.id == id
        Task {
            await model.update { cfg in cfg.setGroup(of: mac, to: inside ? nil : id) }
        }
    }
}

// MARK: - One schedule

/// Edits a schedule, or adds one. A new one starts out as what most are:
/// bedtime on school nights.
private struct ScheduleEditor: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let groupID: String

    @State private var draft: GroupSchedule
    @State private var saving = false

    init(groupID: String, schedule: GroupSchedule) {
        self.groupID = groupID
        _draft = State(initialValue: schedule)
    }

    private var existing: Bool {
        let id: String = draft.id
        return model.config?.group(groupID)?.schedules.contains(where: { $0.id == id }) ?? false
    }

    /// The days in the order this phone's calendar starts the week.
    private var week: [Int] {
        let first: Int = Calendar.current.firstWeekday - 1
        return (0..<7).map { ($0 + first) % 7 }
    }

    private var summary: String {
        if draft.days.isEmpty { return "Choose at least one day." }
        var text: String = scheduleText(draft) + "."
        if draft.endsNextDay {
            text += " It ends on the following day."
        }
        return text
    }

    var body: some View {
        Form {
            Section {
                TextField("Name", text: $draft.name)
                Toggle("Enabled", isOn: $draft.enabled)
            } footer: {
                if let problem = groupNameProblem(draft.name) {
                    Text(problem)
                        .foregroundStyle(.red)
                }
            }
            Section {
                HStack(spacing: 6) {
                    ForEach(week, id: \.self) { day in
                        DayButton(day: day, chosen: draft.days.contains(day)) { toggle(day) }
                    }
                }
                .padding(.vertical, 2)
                DatePicker("From", selection: fromBinding, displayedComponents: .hourAndMinute)
                DatePicker("To", selection: toBinding, displayedComponents: .hourAndMinute)
            } footer: {
                Text(summary)
            }
            if existing {
                Section {
                    Button("Remove", role: .destructive) { remove() }
                }
            }
        }
        .navigationTitle(existing ? "Schedule" : "New schedule")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel") { dismiss() }
            }
            ToolbarItem(placement: .confirmationAction) {
                Button(existing ? "Save" : "Add") { save() }
                    .disabled(draft.days.isEmpty || groupNameProblem(draft.name) != nil || saving)
            }
        }
    }

    private var fromBinding: Binding<Date> {
        Binding(
            get: { timeOfDay(draft.from) ?? Date() },
            set: { value in draft.from = timeOfDay(value) }
        )
    }

    private var toBinding: Binding<Date> {
        Binding(
            get: { timeOfDay(draft.to) ?? Date() },
            set: { value in draft.to = timeOfDay(value) }
        )
    }

    private func toggle(_ day: Int) {
        if let index = draft.days.firstIndex(of: day) {
            draft.days.remove(at: index)
        } else {
            draft.days.append(day)
            draft.days.sort()
        }
    }

    private func save() {
        var schedule: GroupSchedule = draft
        schedule.name = schedule.name.trimmingCharacters(in: .whitespaces)
        let id: String = groupID
        saving = true
        Task {
            let ok: Bool = await model.update { cfg in
                cfg.editGroup(id) { group in
                    if let index = group.schedules.firstIndex(where: { $0.id == schedule.id }) {
                        group.schedules[index] = schedule
                    } else {
                        group.schedules.append(schedule)
                    }
                }
            }
            saving = false
            if ok { dismiss() }
        }
    }

    private func remove() {
        let id: String = groupID
        let scheduleID: String = draft.id
        Task {
            let ok: Bool = await model.update { cfg in
                cfg.editGroup(id) { group in
                    group.schedules.removeAll(where: { $0.id == scheduleID })
                }
            }
            if ok { dismiss() }
        }
    }
}

/// One day of the week, to tick.
private struct DayButton: View {
    let day: Int
    let chosen: Bool
    let action: () -> Void

    private static let letters: [String] = ["S", "M", "T", "W", "T", "F", "S"]
    private static let names: [String] = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"]

    var body: some View {
        Button(action: action) {
            Text(DayButton.letters[day])
                .font(.subheadline.weight(.semibold))
                .frame(maxWidth: .infinity, minHeight: 34)
                .foregroundStyle(chosen ? Color.white : Color.primary)
                .background(chosen ? Color.accentColor : Color.secondary.opacity(0.15), in: Circle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(Text(DayButton.names[day]))
        .accessibilityAddTraits(chosen ? .isSelected : [])
    }
}
