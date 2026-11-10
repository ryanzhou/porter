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

    /// Said only once the password is being typed and is not yet usable.
    private var passwordProblem: Bool {
        !passphrase.isEmpty && !validPassphrase(passphrase)
    }

    var body: some View {
        Form {
            Section {
                VStack(spacing: 10) {
                    Glyph(symbol: "wifi.router.fill", size: 64, filled: true)
                    Text("Name your network")
                        .font(.title2.weight(.bold))
                        .fontDesign(.rounded)
                }
                .frame(maxWidth: .infinity)
                .padding(.vertical, 8)
                .listRowBackground(Color.clear)
            }
            Section {
                TextField("Wi-Fi name", text: $ssid)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                TextField("Password", text: $passphrase)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
            } footer: {
                Text("8 to 63 characters")
                    .foregroundStyle(passwordProblem ? Color.red : Color.secondary)
            }
            Section {
                CountryPicker(selection: $country)
                LabeledContent("Router") {
                    TextField("Name", text: $routerName)
                        .multilineTextAlignment(.trailing)
                }
            } footer: {
                Text("The country sets the legal Wi-Fi channels.")
            }
            Section {
                Button {
                    start()
                } label: {
                    HStack(spacing: 10) {
                        Text("Start")
                            .font(.headline)
                        if saving {
                            ProgressView()
                        }
                    }
                    .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.large)
                .disabled(!valid || saving)
                .listRowInsets(EdgeInsets())
                .listRowBackground(Color.clear)
            }
        }
        .navigationBarTitleDisplayMode(.inline)
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
