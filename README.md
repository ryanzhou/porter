# Porter

A travel router for the Raspberry Pi 5, controlled from an iPhone over
Bluetooth Low Energy.

Porter is its own router software on a minimal Debian base, not a
distribution of an existing router OS. The router and the app are the
only two components: there is no server anywhere, so no cloud control
plane, no account, no central management and no automatic updates.

| Part | Where | What |
|------|-------|------|
| Router daemon | [`daemon/`](daemon) | `porterd` (Go): radios, uplinks, LAN, firewall, policy routing, DNS, VPN, BLE control |
| OS image | [`image/`](image) | Build script and root file system overlay for the SD card image |
| iOS app | [`ios/`](ios) | SwiftUI app and `PorterKit`, the Swift protocol implementation |
| Protocol | [`docs/PROTOCOL.md`](docs/PROTOCOL.md) | The BLE control protocol both sides implement |

## What it does

- **Radios in any arrangement.** Each Wi-Fi radio (the built-in one and
  any USB adapters) is assigned a role: access point, internet uplink,
  both at once (where the driver supports it; one shared channel), or
  off. Wired ports are likewise internet or local network. Roles can be
  changed at any time from the app.
- **Wired or wireless WAN**, with automatic preference between uplinks,
  MAC address choice per uplink type (hardware, random, custom), and
  automatic renumbering of the LAN when it collides with the upstream
  network.
- **Bluetooth-only control.** The app never needs IP connectivity to the
  router, so it works the same whether the phone is on the router's
  Wi-Fi, on a network without internet, or on no network at all. The
  channel is end-to-end authenticated and encrypted above BLE
  (Noise XX); pairing is gated by the Pi's power button.
- **Captive portals** are signed into natively by a client device. While
  the upstream network is captive, one device (the first one connected,
  or one chosen in the app) is routed straight to it and uses its DNS,
  so the device's own sign-in sheet works; everything else keeps its
  normal policy, including kill switches. A network that announces a
  captive portal API (RFC 8910) has it passed on to clients.
- **IPv6 by NAT66.** The LAN gets a ULA /64; the router takes whatever
  address the upstream network offers by RA or DHCPv6 and masquerades.
  No prefix delegation is needed.
- **WireGuard VPN with policy routing.** Traffic is split by source
  (device, address range) and destination (domain, address range,
  protocol, port); the first matching rule wins, then the device's own
  setting, then the default. Per-profile kill switch. DNS follows the
  traffic: a query leaves by the same path the connection will, so
  tunnelled devices use the tunnel's resolver. An IPv4-only tunnel never
  lets IPv6 slip around it.
- **Country rules.** A rule's destination can also be a set of
  countries, matched against a country database built into the image.
- **Device management**: names, pause, per-device route, address
  reservations, recent DNS lookups, new-device log.
- **Traffic and flows.** Per-device and whole-network history (per minute
  for the last hour, per hour for three days), top devices, and a live
  list of each device's connections with host name, country, the path it
  took (direct or which VPN) and byte counts.
- **Speed test** from the router, directly or through a chosen VPN.
- **DNS**: forwarding to the upstream network's resolvers or your own
  over plain DNS, DNS-over-TLS or DNS-over-HTTPS; interception of
  hard-coded resolvers; a block list.

Not included (compared with Firewalla's feature set): intrusion
detection, curated ad/content block lists (they need an online feed),
OpenVPN/AnyConnect clients, a VPN server, multi-WAN load balancing, and
WPA-Enterprise upstream networks.

## Install

### Router

1. Get the image: build it (below) or take `porter-image` from a run of
   the *Image* workflow.
2. Write `porter-<version>.img.xz` to an SD card of 2 GB or more with
   Raspberry Pi Imager ("Use custom"; apply no OS customisation) or:
   ```bash
   xz -dc porter-<version>.img.xz | sudo dd of=/dev/sdX bs=4M conv=fsync status=progress
   ```
3. Boot the Pi 5. The green LED blinks slowly until the router has been
   set up.

### App

Open `ios/Porter.xcodeproj` in Xcode 16 or later, set your development
team and a bundle identifier of your own on the *Porter* target, and run
it on an iPhone (iOS 17+). The app cannot be distributed pre-signed.

### First run

1. In the app, pick the router from the list. A router that has never
   been paired accepts the first phone that asks.
2. Choose a country (radios stay off until the regulatory domain is
   known), a Wi-Fi name and a password.
3. By default the built-in radio is the access point and the Ethernet
   port is the uplink. Change roles under *Internet*.

To pair another phone, press the Pi's power button briefly (the LED
blinks fast for two minutes) or use *Settings → Paired phones*. Holding
the button for a second or more shuts the router down cleanly.

## Build

### Image

On Debian or Ubuntu (x86-64 or arm64), as root:

```bash
sudo apt-get install mmdebstrap qemu-user-static binfmt-support arch-test dosfstools mtools fdisk e2fsprogs xz-utils gpg curl
```

```bash
sudo image/build.sh
```

The result is `build/porter-<version>.img.xz` with its SHA-256. The
script needs Go (the version in `daemon/go.mod`) and downloads Debian
*trixie* and Raspberry Pi packages; that is the only time anything is
fetched. Nothing is loop-mounted, so it also runs in containers.

The image is deliberately appliance-like:

- The root file system is mounted read-only; `/var` and `/tmp` are in
  RAM. Pulling the power cannot corrupt the system.
- Settings live on a small third partition (`/data`). If it is ever
  unrecoverable it is recreated and the router returns to factory state
  rather than failing to start.
- There is no password login. SSH is off unless enabled in the app
  (LAN-side only, key-only). For a console, create an empty file named
  `porter-debug` on the boot partition: that gives a root shell on the
  serial port and on HDMI.

### Daemon

```bash
cd daemon && go test ./...
```

The end-to-end test runs the real daemon in network namespaces against a
simulated upstream network (DHCP, DNS, captive portal, WireGuard server)
and two client devices:

```bash
cd daemon && sudo go test -tags e2e -count=1 -v ./e2e/
```

On the router, `porterctl status`, `porterctl config` and
`porterctl call <method> [json]` speak the same protocol as the app over
a local socket.

### PorterKit

```bash
cd ios/PorterKit && swift test
```

The tests check the Swift protocol code against vectors generated by the
Go implementation. With `PORTER_CONTROL_SOCKET=/run/porter/control.sock`
set they also drive a running `porterd`.

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
every subsystem against it and against what the network is doing. Each
connection from the LAN is classified once in nftables and marked; the
mark selects a routing table. Whenever the inputs to classification
change, connection tracking is flushed so no flow stays on a path that no
longer applies to it. The firewall is replaced atomically and is left in
place if the daemon restarts.

## Things to know

- **Time.** The Pi 5 has no clock battery by default. The router keeps
  time by NTP, the app also sets its clock at the start of every session
  (which covers a router that is not online yet), and the last known time
  is restored at boot.
- **Connectivity check.** To tell a captive portal from a working uplink
  the router fetches one small page every so often (Apple's
  `captive.apple.com`, the same one an iPhone asks).
- **Country database.** The image build downloads DB-IP's free "IP to
  Country Lite" database (CC BY 4.0, attribution in the app's About
  page) and bakes it in; the router never fetches it. It is as old as
  the image: rebuild to refresh it, or build with `PORTER_NO_GEOIP=1` to
  leave it out (country rules then match nothing and the app says so).
  Country data is approximate, and anycast services such as public DNS
  resolvers are attributed to a single country.
- **Speed test.** A speed test has to exchange data with a server on the
  internet. It runs only when started from the app, against the URLs
  under *Settings → Speed test server* (Cloudflare's by default). It
  measures the router's uplink, not the Wi-Fi between a device and the
  router.
- **Flows are sampled.** The connection list is read from the kernel
  every five seconds, so very short connections can be missed, and
  connections refused by a block rule are not listed. Changing rules
  resets connection tracking, which also clears the active list.
- **NAT66 and address selection.** Clients hold only ULA addresses, so
  for destinations that have both address families most systems prefer
  IPv4. IPv6-only destinations work.
- **USB power.** The Pi 5 limits its USB ports to 600 mA on supplies
  that do not identify as 5 A. High-power Wi-Fi adapters may need
  `usb_max_current_enable=1` in `config.txt` (see the comment there).
- **Built-in radio as access point.** Raspberry Pi ships two firmware
  builds for the on-board radio; the default one allows only a handful
  of simultaneous access point clients. The image keeps the default. To
  switch, on the router:
  `update-alternatives --set cyfmac43455-sdio.bin /lib/firmware/cypress/cyfmac43455-sdio-minimal.bin`
  (needs the root file system remounted read-write).
