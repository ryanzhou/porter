# Developing Porter

Porter is its own router software on a minimal Debian base, not a
distribution of an existing router OS. The router and the app are the
only two components: there is no server anywhere, so no cloud control
plane, no account, no central management and no automatic software
updates.

| Part | Where | What |
|------|-------|------|
| Router daemon | [`daemon/`](../daemon) | `porterd` (Go): radios, uplinks, LAN, firewall, policy routing, DNS, VPN, BLE control |
| OS image | [`image/`](../image) | Build script and root file system overlay for the SD card image |
| iOS app | [`ios/`](../ios) | SwiftUI app and `PorterKit`, the Swift protocol implementation |
| Protocol | [`PROTOCOL.md`](PROTOCOL.md) | The BLE control protocol both sides implement |

## How the pieces fit

```
 iPhone app ── BLE GATT ── bluetoothd ── porterd ─┬─ hostapd          (access points)
                                                  ├─ wpa_supplicant   (Wi-Fi uplink)
                                                  ├─ dhcpcd           (uplink DHCP / RA / DHCPv6)
                                                  ├─ dnsmasq          (LAN DHCP and router advertisements)
                                                  ├─ nftables         (firewall, NAT44/NAT66, classification)
                                                  ├─ policy routing   (one table per VPN profile)
                                                  ├─ WireGuard        (kernel)
                                                  └─ DNS forwarder    (built in)
```

`porterd` owns one configuration document and continuously reconciles
every subsystem against it and against what the network is doing.

- **Classification.** Each connection from the LAN is classified once in
  nftables and marked; the mark selects a routing table (the main table,
  or one per VPN profile). The mark is kept in connection tracking.
  Whenever the inputs to classification change, connection tracking is
  flushed so no flow stays on a path that no longer applies to it.
- **Kill switch.** A VPN's table carries an `unreachable` backstop route
  below the tunnel's routes, and the forward chain independently refuses
  packets whose mark and output interface disagree. An IPv4-only tunnel
  gets an IPv6 backstop so IPv6 cannot go around it.
- **DNS follows the traffic.** The forwarder picks the upstream for each
  query the way the classifier would route a connection to that name, and
  teaches nftables the addresses behind domain rules before releasing the
  answer.
- **Captive portals.** While the active uplink is captive, one device is
  marked direct and resolved through the upstream network's own DNS;
  everything else keeps its policy.
- **IPv6.** The LAN gets a ULA /64; the uplink takes whatever RA or
  DHCPv6 offers; traffic is masqueraded (NAT66). No prefix delegation is
  needed.
- **Control.** A Noise XX channel above GATT, independent of BLE
  pairing; see [PROTOCOL.md](PROTOCOL.md). Pairing is trust-on-first-use,
  gated by the power button.
- **Firewall replacement** is atomic, and the ruleset is left in place
  when the daemon stops, so a restarting daemon never leaves the router
  open.

## The image

On Debian or Ubuntu (x86-64 or arm64), as root:

```bash
sudo apt-get install mmdebstrap qemu-user-static binfmt-support arch-test dosfstools mtools fdisk e2fsprogs xz-utils gpg curl
```

```bash
sudo image/build.sh
```

The result is `build/porter-<version>.img.xz` with its SHA-256. The
script needs Go (the version in `daemon/go.mod`) and downloads Debian
*trixie* packages, Raspberry Pi's kernel and firmware, and the DB-IP
country database (`PORTER_NO_GEOIP=1` builds without it). Nothing is
loop-mounted, so it also runs in containers. The *Image* workflow runs
the same script.

The image is deliberately appliance-like:

- The root file system is mounted read-only; `/var` and `/tmp` are in
  RAM. Pulling the power cannot corrupt the system.
- Settings, pairings, cached block lists and the refreshed country
  database live on a small third partition (`/data`). If it is ever
  unrecoverable it is recreated and the router returns to factory state
  rather than failing to start.
- There is no password login. SSH is off unless enabled in the app
  (LAN-side only, key-only). An empty file named `porter-debug` on the
  boot partition gives a root shell on HDMI and on the serial console
  (on a Pi 5, the 3-pin debug connector, 115200 baud).
- Time: the Pi 5 has no clock battery by default. Besides NTP, the app
  sets the clock at the start of every session and the last known time
  is restored at boot, so that WireGuard never sees time run backwards.

### Built-in radio client limit

Raspberry Pi ships two firmware builds for the on-board radio; the
default one allows only a handful of simultaneous access point clients.
The image keeps the default. To switch, on the router (with the root
file system remounted read-write):

```bash
update-alternatives --set cyfmac43455-sdio.bin /lib/firmware/cypress/cyfmac43455-sdio-minimal.bin
```

## The daemon

```bash
cd daemon && go test ./...
```

The end-to-end tests run the real daemon in network namespaces. They
need root and `ip`, `nft`, `dnsmasq`, `dhcpcd` and `wg`:

```bash
cd daemon && sudo go test -tags e2e -count=1 -v ./e2e/
```

- `TestRouter` simulates an upstream network (DHCP, router
  advertisements, DNS, a captive portal, a WireGuard server, block lists
  and a country database to download) and two client devices, and checks
  what actually crosses the router: NAT44, NAT66, DNS interception,
  pause and block, VPN splits by device, domain, country and port,
  per-path DNS, the kill switch, the captive portal bypass, ad blocking,
  flows, traffic history and speed tests.
- `TestWifi` uses simulated radios (`mac80211_hwsim`): hostapd and
  wpa_supplicant for real, scanning, wrong and right passwords, a
  wireless client through the router, and one radio doing both jobs. It
  is skipped where the kernel module is unavailable.

Optional tests against real data are enabled by environment variables:
`PORTER_GEOIP_FILE` (a DB-IP file) and `PORTER_BLOCKLIST_DIR` (a
directory of downloaded block lists).

On the router, `porterctl status`, `porterctl config` and
`porterctl call <method> [json]` speak the same protocol as the app over
a local socket.

## The app

```bash
cd ios/PorterKit && swift test
```

`PorterKit` holds everything below the user interface: the Noise
handshake, framing, the RPC client, the data models, the CoreBluetooth
link, and a sample-data router for demo mode. Its tests check the Swift
protocol code against vectors generated by the Go implementation
(`go test ./internal/proto -run TestVectors -update` regenerates them)
and that the demo data decodes through the real models. With
`PORTER_CONTROL_SOCKET=/run/porter/control.sock` set they also drive a
running `porterd`. The package builds on Linux as well, where it is used
only for these tests.

Signing settings live in `ios/Local.xcconfig`, which git ignores; do not
pick a team in Xcode's Signing pane, which would write it into the
project file.

**Demo mode.** Launching with `-demo` runs the app against the built-in
sample router and never touches Bluetooth. `-demoScreen <name>` opens one
screen directly: a tab name (`overview`, `internet`, `devices`, `vpn`,
`settings`) or `traffic`, `flows`, `speedtest`, `device`, `adblock`. The
*iOS* workflow uses this to publish screenshots as an artifact.

## Continuous integration

| Workflow | Runs on | Does |
|---|---|---|
| Daemon | pushes touching `daemon/` | unit tests, kernel-level firewall tests, both end-to-end tests |
| iOS | pushes touching `ios/` | PorterKit tests, app build for the simulator, screenshots |
| Image | manually, and on `v*` tags | builds the SD card image as an artifact |

## Things that are sampled or approximate

- The connection list is read from the kernel every five seconds, so
  very short connections can be missed. Connections refused by a block
  rule never enter connection tracking and are not listed. Changing
  rules resets connection tracking, which also clears the active list.
- Downloaded data (block lists, the country database) is checked for
  plausibility before it replaces the copy in use.
- Clients hold only ULA IPv6 addresses, so for dual-stack destinations
  most systems prefer IPv4.
