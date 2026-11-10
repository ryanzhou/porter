import PorterKit
import SwiftUI

/// Finding, connecting to and pairing with a router.
struct ConnectView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        ScrollView {
            VStack(spacing: 12) {
                switch model.phase {
                case .connecting(let name):
                    WaitingCard(symbol: "wifi.router.fill", title: name, detail: "Connecting")
                case .awaitingPairing(let name):
                    WaitingCard(symbol: "power", title: "Press the power button", detail: name + " belongs to another phone. A short press lets this one in.")
                default:
                    searching
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 24)
        }
        .background(Surface.page)
        .navigationTitle("Porter")
    }

    @ViewBuilder
    private var searching: some View {
        if let reason = model.central.unavailableReason {
            Card {
                Label(reason, systemImage: "exclamationmark.triangle.fill")
                    .foregroundStyle(.orange)
            }
        }
        if model.central.discovered.isEmpty {
            SearchingCard()
        }
        ForEach(model.central.discovered) { router in
            Button {
                model.connect(to: router)
            } label: {
                RouterCard(router: router, paired: isPaired(router))
            }
            .buttonStyle(.plain)
        }
        if !forgettable.isEmpty {
            Heading(text: "Paired")
            Card(padding: 0) {
                VStack(spacing: 0) {
                    ForEach(forgettable) { router in
                        PairedRow(router: router)
                    }
                }
            }
        }
    }

    private func isPaired(_ router: DiscoveredRouter) -> Bool {
        model.knownRouters.contains(where: { $0.peripheral == router.id })
    }

    private var forgettable: [KnownRouter] { model.knownRouters }
}

/// Shown while nothing has been found yet.
private struct SearchingCard: View {
    var body: some View {
        VStack(spacing: 14) {
            Image(systemName: "dot.radiowaves.left.and.right")
                .font(.system(size: 54, weight: .medium))
                .foregroundStyle(.tint)
                .symbolEffect(.variableColor.iterative, options: .repeating)
            Text("Looking for a router")
                .font(.headline)
            Text("Stay within a few metres.")
                .font(.subheadline)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 72)
    }
}

private struct RouterCard: View {
    let router: DiscoveredRouter
    let paired: Bool

    var body: some View {
        Card {
            HStack(spacing: 14) {
                Glyph(symbol: "wifi.router.fill", size: 46, filled: true)
                VStack(alignment: .leading, spacing: 2) {
                    Text(router.name)
                        .font(.headline)
                    Text(paired ? "Paired" : "New")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
                Spacer()
                SignalBars(rssi: router.rssi)
                Image(systemName: "chevron.right")
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(.tertiary)
            }
        }
    }
}

private struct PairedRow: View {
    @EnvironmentObject private var model: AppModel
    let router: KnownRouter

    var body: some View {
        HStack {
            VStack(alignment: .leading, spacing: 1) {
                Text(router.name)
                Text(router.id)
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button("Forget", role: .destructive) { model.forget(router) }
                .buttonStyle(.bordered)
                .controlSize(.small)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
    }
}

/// One thing is in progress and the only choice is to stop it.
private struct WaitingCard: View {
    @EnvironmentObject private var model: AppModel
    let symbol: String
    let title: String
    let detail: String

    var body: some View {
        VStack(spacing: 16) {
            Glyph(symbol: symbol, size: 72, filled: true)
            Text(title)
                .font(.title3.weight(.semibold))
                .multilineTextAlignment(.center)
            Text(detail)
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
            ProgressView()
                .padding(.top, 4)
            Button("Cancel") { model.cancelConnecting() }
                .buttonStyle(.bordered)
                .padding(.top, 8)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 56)
        .padding(.horizontal, 24)
    }
}
