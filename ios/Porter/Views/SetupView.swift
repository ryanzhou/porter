import PorterKit
import SwiftUI

/// First-time setup: the minimum the router needs before it turns its
/// radios on.
struct SetupView: View {
    @EnvironmentObject private var model: AppModel

    @State private var country: String = ""
    @State private var routerName: String = ""
    @State private var ssid: String = ""
    @State private var passphrase: String = ""
    @State private var loaded = false
    @State private var saving = false

    private var valid: Bool {
        country.count == 2 && validRouterName(routerName) && validNetworkName(ssid) && validPassphrase(passphrase)
    }

    private var passwordHint: String {
        if passphrase.isEmpty || validPassphrase(passphrase) {
            return "Devices join the router with this name and password. The password needs 8 to 63 characters."
        }
        return "The password needs 8 to 63 characters: letters, digits and common symbols."
    }

    var body: some View {
        Form {
            Section {
                Text("The router keeps its radios off until it knows which country it is in and what its Wi-Fi network is called.")
                    .foregroundStyle(.secondary)
            }
            Section {
                CountryPicker(selection: $country)
            } footer: {
                Text("Decides which Wi-Fi channels and power levels the router may use.")
            }
            Section {
                TextField("Network name", text: $ssid)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                TextField("Password", text: $passphrase)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
            } header: {
                Text("Wi-Fi network")
            } footer: {
                Text(passwordHint)
            }
            Section {
                TextField("Router name", text: $routerName)
            } header: {
                Text("Router name")
            } footer: {
                Text("Shown in this app and over Bluetooth. Up to 20 characters.")
            }
            Section {
                Button {
                    start()
                } label: {
                    HStack(spacing: 12) {
                        Text("Start router")
                        if saving {
                            ProgressView()
                        }
                    }
                }
                .disabled(!valid || saving)
            }
        }
        .navigationTitle("Set up router")
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Disconnect") { model.disconnect() }
            }
        }
        .onAppear { load() }
    }

    private func load() {
        guard !loaded, let config = model.config else { return }
        loaded = true
        country = config.system.country.isEmpty ? CountryChoice.deviceDefault : config.system.country
        routerName = config.system.name
        ssid = config.ap.ssid
        passphrase = config.ap.passphrase
    }

    private func start() {
        let newCountry = country
        let newName = routerName.trimmingCharacters(in: .whitespaces)
        let newSSID = ssid
        let newPassphrase = passphrase
        saving = true
        Task {
            await model.update { cfg in
                cfg.system.country = newCountry
                cfg.system.name = newName
                cfg.ap.ssid = newSSID
                cfg.ap.passphrase = newPassphrase
            }
            saving = false
        }
    }
}
