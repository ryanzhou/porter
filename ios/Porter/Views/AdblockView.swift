import PorterKit
import SwiftUI

/// Well-known block lists offered as one-tap choices.
private struct BlockListPreset: Identifiable {
    let name: String
    let detail: String
    let url: String
    var id: String { url }

    static let all: [BlockListPreset] = [
        BlockListPreset(
            name: "StevenBlack",
            detail: "Ads and malware. Conservative; rarely breaks sites.",
            url: "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"),
        BlockListPreset(
            name: "HaGeZi Light",
            detail: "Ads, tracking and telemetry.",
            url: "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/light.txt"),
        BlockListPreset(
            name: "OISD Small",
            detail: "Ads and tracking, tuned to avoid breakage.",
            url: "https://small.oisd.nl/"),
    ]
}

/// Settings → Ad blocking.
struct AdblockPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var customURL: String = ""
    @State private var checking = false

    private var enabled: Bool { model.config?.dns.adblock.enabled ?? false }
    private var lists: [String] { model.config?.dns.adblock.lists ?? [] }
    private var status: AdblockStatus? { model.status?.adblock }

    private var customLists: [String] {
        let presets = Set(BlockListPreset.all.map(\.url))
        return lists.filter { !presets.contains($0) }
    }

    private var cleanCustomURL: String {
        customURL.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private var customURLValid: Bool {
        let text = cleanCustomURL
        guard let url = URL(string: text), let scheme = url.scheme, url.host != nil else { return false }
        return (scheme == "http" || scheme == "https") && !text.contains(" ") && !lists.contains(text)
    }

    var body: some View {
        List {
            Section {
                Toggle("Block ads and trackers", isOn: model.binding(\.dns.adblock.enabled, fallback: false))
            } footer: {
                Text("The router refuses to look up names that are on the block lists below, for every device on its network. It downloads the lists itself and refreshes them daily. Ads served from the same address as the content they sit in cannot be blocked this way.")
            }
            if enabled {
                statusSection
            }
            presetSection
            customSection
            StringListEditor(
                title: "Always allow",
                items: allowBinding,
                placeholder: "example.com",
                keyboard: .URL,
                footer: "Domains listed here, and their subdomains, are never blocked. To switch blocking off for one device, open the device and turn on Skip ad blocking."
            )
        }
        .navigationTitle("Ad blocking")
        .navigationBarTitleDisplayMode(.inline)
    }

    private var statusSection: some View {
        Section("Status") {
            LabeledContent("Domains blocked", value: (status?.domains ?? 0).formatted())
            LabeledContent("Lookups blocked since start", value: (status?.blocked ?? 0).formatted())
            ForEach(status?.lists ?? []) { list in
                BlockListStatusRow(list: list)
            }
            Button {
                checkNow()
            } label: {
                HStack {
                    Text("Update now")
                    Spacer()
                    if checking { ProgressView() }
                }
            }
            .disabled(checking)
        }
    }

    private var presetSection: some View {
        Section {
            ForEach(BlockListPreset.all) { preset in
                Button {
                    toggle(preset.url)
                } label: {
                    HStack {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(preset.name)
                                .foregroundStyle(.primary)
                            Text(preset.detail)
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                        }
                        Spacer()
                        if lists.contains(preset.url) {
                            Image(systemName: "checkmark")
                        }
                    }
                }
                .disabled(isLastList(preset.url))
            }
        } header: {
            Text("Block lists")
        } footer: {
            Text("More lists block more, use more of the router's memory, and are more likely to break something. One is usually enough.")
        }
    }

    private var customSection: some View {
        Section {
            ForEach(customLists, id: \.self) { url in
                Text(url)
                    .font(.footnote)
                    .lineLimit(2)
                    .swipeActions {
                        if !isLastList(url) {
                            Button("Remove", role: .destructive) { toggle(url) }
                        }
                    }
            }
            HStack {
                TextField("https://example.com/list.txt", text: $customURL)
                    .keyboardType(.URL)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                Button("Add") {
                    let url = cleanCustomURL
                    customURL = ""
                    toggle(url)
                }
                .disabled(!customURLValid)
            }
        } header: {
            Text("Other lists")
        } footer: {
            Text("Any list in hosts, plain domain or Adblock format.")
        }
    }

    /// While blocking is on there has to be at least one list.
    private func isLastList(_ url: String) -> Bool {
        enabled && lists == [url]
    }

    private var allowBinding: Binding<[String]> {
        Binding(
            get: { model.config?.dns.adblock.allow ?? [] },
            set: { value in
                Task { await model.update { cfg in cfg.dns.adblock.allow = value } }
            }
        )
    }

    private func toggle(_ url: String) {
        Task {
            await model.update { cfg in
                if let index = cfg.dns.adblock.lists.firstIndex(of: url) {
                    cfg.dns.adblock.lists.remove(at: index)
                } else {
                    cfg.dns.adblock.lists.append(url)
                }
            }
        }
    }

    private func checkNow() {
        checking = true
        Task {
            await model.attempt { try await $0.checkForUpdates() }
            // Downloads take a moment; show what came of them.
            try? await Task.sleep(nanoseconds: 4_000_000_000)
            await model.refresh()
            checking = false
        }
    }
}

private struct BlockListStatusRow: View {
    let list: BlockListStatus

    private var name: String {
        BlockListPreset.all.first(where: { $0.url == list.url })?.name ?? (URL(string: list.url)?.host ?? list.url)
    }

    private var detail: String {
        if let updated = list.updated {
            return "\(list.domains.formatted()) domains · checked \(relativeTime(updated))"
        }
        return "Not downloaded yet"
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(name)
            Text(detail)
                .font(.footnote)
                .foregroundStyle(.secondary)
            if let error = list.error, !error.isEmpty {
                Text("Last update failed: \(error)")
                    .font(.footnote)
                    .foregroundStyle(.red)
            }
        }
    }
}
