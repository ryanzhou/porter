# Developing Porter

Porter is its own router software on a minimal Debian base, not a
distribution of an existing router OS. The router and the app are the
only two components: there is no server anywhere, so no cloud control
plane, no account and no central management. Software updates come from
the project's GitHub releases, and only when asked for from the app.

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
- **Networks.** Each of the router's own networks is a bridge
  (`br-<id>`; the main one is `br-lan`) with its own address range,
  served by one dnsmasq. Wi-Fi networks (hostapd BSSes) and wired LAN
  ports are attached to the bridge of the network they belong to.
  Traffic between networks is routed by the main table and decided in
  the forward hook: allowed, except new connections out of a network
  marked isolated. A network's route is one more step in classification,
  after rules and a device's own route.
- **Several Wi-Fi networks on a radio.** hostapd runs one BSS per Wi-Fi
  network on a radio, each on its own interface (`ap<phy>-<n>`) with an
  address derived from the radio's. How many a radio can carry is read
  from its interface combinations (`iw phy`); the Raspberry Pi's on-board
  radio carries one. Networks are spread over the access point radios so
  that each is on the air somewhere if that is possible at all, and the
  status says which are not.
- **Uplink order.** Uplinks keep fixed route metrics; the configured
  order is carried out by a routing table of its own (105, consulted
  after the tunnels' tables and before the main table's default route)
  that holds a default route only while the uplink to use is not the one
  the metrics would pick. Reordering therefore restarts nothing. The
  uplink to use is the first in the order that is online or behind a
  sign-in page; one that has an address but fails three reachability
  probes in a row is stepped past.
- **Phones on USB.** An interface driven by `ipheth` (iPhone) or
  `rndis_host` (Android) is an uplink of kind "phone" for as long as it
  is plugged in; nothing about it is stored. An iPhone only brings the
  link up for a computer it trusts: `usbmuxd`, started by udev when an
  iPhone is plugged in, asks it, and keeps the pairing record in
  `/data/lockdown` (linked from `/var/lib/lockdown`).
- **IPv6.** Each network gets a ULA /64; the uplink takes whatever RA or
  DHCPv6 offers; traffic is masqueraded (NAT66). No prefix delegation is
  needed.
- **MPQUIC.** A VPN profile of this type runs the `mpquic` program as it
  is released (`mpquic local`), with only its WireGuard listener on, bound
  to a loopback port. The rest of the router is given an ordinary
  WireGuard profile pointing at that port (`internal/mpquic`), with keys
  made at each start, so policy routing, the kill switch and per-path DNS
  need to know nothing about it. The tunnel counts as up only while
  mpquic reports at least one working path. The release is pinned, with
  its hash, in `image/mpquic.pin`; a copy at `/data/porter/bin/mpquic`
  takes precedence on a router.
- **Uplink addresses.** dhcpcd configures an uplink (DHCP, router
  advertisements, DHCPv6) unless a family is set by hand, per wired port
  or per saved Wi-Fi network. A fixed address and its default route are
  put in place by porterd itself and re-checked every two seconds;
  dhcpcd then handles only the other family, or does not run.
- **Upstream band.** wpa_supplicant and radio firmware choose an access
  point mostly by signal strength, which favours 2.4 GHz. porterd decides
  the band itself from the scan results (5 GHz when it is at least fair,
  with hysteresis) and tells wpa_supplicant by switching between a 5 GHz
  and a 2.4 GHz copy of each saved network. On a radio that also runs the
  access point, only channels an access point may use count. An access
  point on its own radio is kept clear of the uplink on 5 GHz, on the
  first channel far enough away that takes an 80 MHz network.
- **One radio for both.** The access point of a radio that is also the
  uplink runs on a second interface (`ap<phy>`) and has to be on the
  uplink's channel. It moves there before the station is allowed to
  join (a station that tries first is refused, and wpa_supplicant then
  leaves the network alone for a while), and otherwise stays where it
  is rather than fall back to a default channel between connections.
- **Changes as one.** `config.set` validates the document, checks that
  it describes a router that can work (`Config.Consistent`), and the
  next pass of the control loop applies all of it: uplinks that stop
  being uplinks are released, then stations start, then access points.
  `config.check` runs the same checks, and the radio planning, on a
  document without applying it.
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
  database live on their own partition (`/data`). If it is ever
  unrecoverable it is recreated and the router returns to factory state
  rather than failing to start.
- There is no password login. SSH is off unless enabled in the app
  (LAN-side only, key-only). An empty file named `porter-debug` on the
  boot partition gives a root shell on HDMI and on the serial console
  (on a Pi 5, the 3-pin debug connector, 115200 baud).
- Time: the Pi 5 has no clock battery by default. Besides NTP, the app
  sets the clock at the start of every session and the last known time
  is restored at boot, so that WireGuard never sees time run backwards.

### Updates

The card holds the system twice:

| Partition | | |
|---|---|---|
| 1 | selector, FAT | `autoboot.txt`: which boot partition to start |
| 2, 3 | boot A, boot B, FAT | kernel, device trees, kernel command line |
| 5, 6 | root A, root B, ext4 | the read-only root file system |
| 7 | data, ext4 | settings; shared |

A release carries, besides the image to flash, the two partition images
of a slot (`porter-<version>-boot.vfat.gz`, `-root.ext4.gz`) and
`update.json`, which gives their sizes and SHA-256 hashes.
`daemon/internal/update` does the rest:

1. `firmware.check` fetches `update.json` from the newest release.
2. `firmware.install` downloads both images into the slot that is not
   running, checking the hashes as they arrive and again by reading the
   card back, and rewrites the root partition in that slot's
   `cmdline.txt`.
3. It points `[tryboot]` in `autoboot.txt` at the new slot and restarts
   with `systemctl reboot --reboot-argument="0 tryboot"`. The Raspberry
   Pi boot loader starts the `[tryboot]` partition once.
4. In the new system, `porterd` makes the slot the default after 45
   seconds of running. If it never gets that far, the next restart is a
   normal one and starts the old slot: `porter-update-guard` forces one
   after five minutes, the hardware watchdog on a hang, the kernel on a
   panic. Back on the old slot, `porterd` records that the update did
   not hold.

Partition numbers and slot sizes are fixed (see the top of
`image/build.sh`): every later version has to fit a card flashed today.
A change to them needs a new `Layout` number in the update package, and
cards have to be flashed again. A root slot is 3 GB, four times what the
system takes now, and as much as two of them can be on an 8 GB card.

**Settings across versions.** Both slots use the one data partition, so
an update keeps settings, pairings and the router's identity without
doing anything. Two cases need care:

- *A newer version changes the format of the settings.* New fields need
  nothing; `Normalize` fills in their defaults. A change to existing
  fields means raising `SchemaVersion` in `daemon/internal/config` and
  adding the step from the old version to `migrations` there. Without
  that step the new version would reject the stored settings and start
  in the factory state.
- *An update does not hold and the old version starts again.* The new
  version ran for a while on the shared partition and may have rewritten
  the settings in a form the old one cannot read. So the installer copies
  the small files of the data directory aside before the trial
  (`before-update/`), and `porterd` restores them at startup, before it
  reads anything, if it finds itself back on the old slot.

**Signing.** By default a router trusts an update because it came from
GitHub over HTTPS. To make it require a signature as well, create a key
pair:

```bash
cd daemon && go run ./cmd/porter-release keygen
```

Store the signing key as the repository secret `PORTER_UPDATE_KEY` and
commit the public key as `image/keys/update.pub`. Images built from then
on install only updates signed with that key, and the Image workflow
signs each release. Routers flashed before the key existed keep
accepting unsigned updates, so do this before the first release if you
want it at all.

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

A development machine has more installed, and other versions of it, than
the router does. To run the same tests against what the router actually
runs (Debian with exactly the packages in `image/packages`, built for
this machine's architecture):

```bash
sudo daemon/e2e/in-image-userland.sh
```

CI runs the end-to-end tests this way.

- `TestRouter` simulates an upstream network (DHCP, router
  advertisements, DNS, a captive portal, a WireGuard server, block lists
  and a country database to download) and two client devices, and checks
  what actually crosses the router: NAT44, NAT66, DNS interception,
  pause and block, VPN splits by device, domain, country and port,
  per-path DNS, the kill switch, the captive portal bypass, ad blocking,
  flows, traffic history and speed tests; a second network (its own
  addresses, isolation, a route and a rule of its own, and its removal);
  and a second uplink standing in for a phone (the order of uplinks, and
  falling back when the first stops reaching the internet).
- `TestWifi` uses simulated radios (`mac80211_hwsim`): hostapd and
  wpa_supplicant for real, scanning, wrong and right passwords, a
  wireless client through the router, a second Wi-Fi network on a
  network of its own, the two radios swapping jobs in one step, and one
  radio doing both jobs. It is skipped where the kernel module is
  unavailable.

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
| Daemon | pushes touching `daemon/` | unit tests, kernel-level firewall tests, both end-to-end tests inside the image's userland |
| iOS | pushes touching `ios/` | PorterKit tests, app build for the simulator, screenshots |
| Image | manually, and on `v*` tags | builds the SD card image as an artifact; a tag publishes it as a release |

A release is a tag:

```bash
git tag v0.2.0
```

```bash
git push origin v0.2.0
```

The Image workflow builds the image for it, named after the tag, and
attaches it and the update files to a release of that name. Routers
offer a release as an update once it is the newest one that is not a
draft or pre-release. The router and the app both show the version.

## Things that are sampled or approximate

- The connection list is read from the kernel every five seconds, so
  very short connections can be missed. Connections refused by a block
  rule never enter connection tracking and are not listed. Changing
  rules resets connection tracking, which also clears the active list.
- Downloaded data (block lists, the country database) is checked for
  plausibility before it replaces the copy in use.
- Clients hold only ULA IPv6 addresses, so for dual-stack destinations
  most systems prefer IPv4.
