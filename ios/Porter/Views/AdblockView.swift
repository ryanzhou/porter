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
            detail: "Ads, malware",
            url: "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"),
        BlockListPreset(
            name: "HaGeZi Light",
            detail: "Ads, tracking, telemetry",
            url: "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/light.txt"),
        BlockListPreset(
            name: "OISD Small",
            detail: "Ads, tracking",
            url: "https://small.oisd.nl/"),
    ]
}

/// Settings → Ad blocking.
struct AdblockPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var customURL: String = ""
    @State private var checking = false
    /// Set by a tap on the only list left, which cannot be turned off.
    @State private var triedLastList = false

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
                Toggle(isOn: model.binding(\.dns.adblock.enabled, fallback: false)) {
                    HStack(spacing: 12) {
                        Glyph(symbol: "hand.raised.fill", color: enabled ? Color.accentColor : Color.gray, filled: enabled)
                        Text("Block ads and trackers")
                    }
                }
            } footer: {
                Text("This applies to every device on your networks. Ads that come from the same place as the content, as on YouTube, still get through.")
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
                footer: "Add a domain here if a site stops working with ad blocking on. Its subdomains are allowed too."
            )
        }
        .navigationTitle("Ad blocking")
        .navigationBarTitleDisplayMode(.inline)
    }

    private var statusSection: some View {
        Section {
            HStack(spacing: 0) {
                CountFigure(value: status?.blocked ?? 0, caption: "lookups blocked")
                CountFigure(value: UInt64(status?.domains ?? 0), caption: "domains on the lists")
            }
            .padding(.vertical, 4)
            ForEach(status?.lists ?? []) { list in
                BlockListStatusRow(list: list)
            }
            Button {
                checkNow()
            } label: {
                HStack {
                    Label("Update the lists now", systemImage: "arrow.clockwise")
                    Spacer()
                    if checking { ProgressView() }
                }
            }
            .disabled(checking)
        } footer: {
            Text("The router updates the lists by itself once a day.")
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
                                .foregroundStyle(.tint)
                        }
                    }
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
            }
        } header: {
            Text("Lists")
        } footer: {
            Text(listsNote)
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
                TextField("List address", text: $customURL, prompt: Text(verbatim: "https://example.com/list.txt"))
                    .keyboardType(.URL)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                Button("Add") {
                    let url = cleanCustomURL
                    customURL = ""
                    toggle(url)
                }
                .buttonStyle(.borderless)
                .disabled(!customURLValid)
            }
        } header: {
            Text("Other lists")
        }
    }

    /// While blocking is on there has to be at least one list.
    private func isLastList(_ url: String) -> Bool {
        enabled && lists == [url]
    }

    /// Under the lists: how many are worth having, or, after a tap on
    /// the only one left, why that one stays.
    private var listsNote: String {
        if triedLastList && enabled && lists.count == 1 {
            return "Ad blocking needs at least one list, so the last one stays on."
        }
        return "One list is usually enough. Each one added makes it more likely that a site stops working."
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
        // The last list cannot be removed while blocking is on.
        guard !isLastList(url) else {
            triedLastList = true
            return
        }
        triedLastList = false
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
            return "\(list.domains.formatted()) domains · updated \(relativeTimeInline(updated))"
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
                Text(error)
                    .font(.footnote)
                    .foregroundStyle(.red)
            }
        }
    }
}

/// A count with what it counts underneath.
private struct CountFigure: View {
    let value: UInt64
    let caption: String

    var body: some View {
        VStack(spacing: 2) {
            Figure(value: value.formatted(), font: Font.title2)
            Text(caption)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity)
    }
}
