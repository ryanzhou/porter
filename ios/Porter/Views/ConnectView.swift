import PorterKit
import SwiftUI

/// Finding, connecting to and pairing with a router.
struct ConnectView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        List {
            switch model.phase {
            case .connecting(let name):
                Section {
                    HStack(spacing: 12) {
                        ProgressView()
                        Text("Connecting to \(name)…")
                    }
                    Button("Cancel", role: .cancel) { model.cancelConnecting() }
                }
            case .awaitingPairing(let name):
                Section {
                    VStack(alignment: .leading, spacing: 12) {
                        Label("Press the power button", systemImage: "power.circle")
                            .font(.headline)
                        Text("\(name) already belongs to another phone. Briefly press the power button on the Raspberry Pi to let this phone pair; its green light blinks quickly while pairing is open.")
                            .foregroundStyle(.secondary)
                        HStack(spacing: 12) {
                            ProgressView()
                            Text("Waiting for the button…")
                        }
                    }
                    .padding(.vertical, 4)
                    Button("Cancel", role: .cancel) { model.cancelConnecting() }
                }
            default:
                if let reason = model.central.unavailableReason {
                    Section {
                        Label(reason, systemImage: "exclamationmark.triangle")
                    }
                }
                Section {
                    if model.central.discovered.isEmpty {
                        HStack(spacing: 12) {
                            ProgressView()
                            Text("Looking for routers nearby…")
                                .foregroundStyle(.secondary)
                        }
                    }
                    ForEach(model.central.discovered) { router in
                        Button {
                            model.connect(to: router)
                        } label: {
                            HStack {
                                Image(systemName: "wifi.router")
                                VStack(alignment: .leading) {
                                    Text(router.name)
                                    if model.knownRouters.contains(where: { $0.peripheral == router.id }) {
                                        Text("Paired").font(.caption).foregroundStyle(.secondary)
                                    }
                                }
                                Spacer()
                                SignalBars(rssi: router.rssi)
                            }
                        }
                        .foregroundStyle(.primary)
                    }
                } header: {
                    Text("Routers")
                } footer: {
                    Text("Porter is controlled over Bluetooth, so this works whether or not the router, or this phone, has an internet connection.")
                }
                if !model.knownRouters.isEmpty {
                    Section("Paired with this phone") {
                        ForEach(model.knownRouters) { router in
                            HStack {
                                Text(router.name)
                                Spacer()
                                Text(router.id).font(.caption.monospaced()).foregroundStyle(.secondary)
                            }
                            .swipeActions {
                                Button("Forget", role: .destructive) { model.forget(router) }
                            }
                        }
                    }
                }
            }
        }
        .navigationTitle("Porter")
    }
}
