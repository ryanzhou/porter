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
                    WaitingCard(symbol: "power", title: "Press the power button", detail: name + " is already paired with another phone. Press its power button briefly to let this phone pair as well.")
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
                VStack(alignment: .leading, spacing: 8) {
                    Label(reason, systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.orange)
                    Text(withoutBluetooth)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
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
            Heading(text: "Paired routers")
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

    /// What the app still does, and cannot do, while Bluetooth is not
    /// usable.
    private var withoutBluetooth: String {
        if model.knownRouters.isEmpty {
            return "Pairing with a router needs Bluetooth."
        }
        return "Porter is still looking for your router on the network this phone is on. Pairing with another router needs Bluetooth."
    }
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
            Text("Stay within a few metres. A router takes about a minute to appear after it is plugged in.")
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 72)
        .padding(.horizontal, 24)
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
                    Text(paired ? "Paired" : "New to this phone")
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
            Button("Connect") { model.connect(to: router) }
                .buttonStyle(.bordered)
                .controlSize(.small)
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
