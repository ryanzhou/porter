# Developing Porter

Porter is its own router software on a minimal Debian base, not a
distribution of an existing router OS. The router and the app are the
only two components: there is no server anywhere, so no cloud control
plane, no account and no central management. Software updates come from
the project's GitHub releases, and only when asked for from the app.

| Part | Where | What |
|------|-------|------|
| Router daemon | [`daemon/`](../daemon) | `porterd` (Go): radios, uplinks, LAN, firewall, policy routing, DNS, VPN, what comes in from outside, control over BLE and over its own networks |
| OS image | [`image/`](../image) | Build script and root file system overlay for the SD card image |
| iOS app | [`ios/`](../ios) | SwiftUI app and `PorterKit`, the Swift protocol implementation |
| Protocol | [`PROTOCOL.md`](PROTOCOL.md) | The control protocol both sides implement |

## How the pieces fit

```
 iPhone app ─┬─ BLE GATT ── bluetoothd ─┬─ porterd ─┬─ hostapd          (access points)
             └─ TCP, on the router's ───┘           ├─ wpa_supplicant   (Wi-Fi uplink)
                own networks                        ├─ dhcpcd           (uplink DHCP / RA / DHCPv6, prefix delegation)
                                                    ├─ pppd             (uplink PPPoE)
                                                    ├─ dnsmasq          (LAN DHCP and router advertisements)
                                                    ├─ nftables         (firewall, NAT44/NAT66, classification)
                                                    ├─ tc               (CAKE queues on uplinks held to a speed)
                                                    ├─ policy routing   (one table per VPN profile)
                                                    ├─ WireGuard        (kernel: profiles, and the router's own server)
                                                    ├─ DNS forwarder    (built in)
                                                    └─ mDNS relay       (built in)
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
  that always names the uplink to use: its gateway per address family,
  and "unreachable" for a family it does not have, so that IPv6 never
  follows an uplink that has been stepped past. Reordering therefore
  restarts nothing, and a port with a fixed address whose cable is
  pulled is not used (the kernel would keep its route). The uplink to
  use is the first in the order that is online or behind a sign-in
  page; one that has an address but fails three reachability probes in
  a row is stepped past. A sign-in page is believed on the second probe
  that shows one when the uplink was online before, and a sign-in under
  way survives two failed probes.
- **Presence.** A Wi-Fi device is online while an access point lists
  it. The kernel's neighbour table is only asked about wired devices:
  with few entries it keeps an address until something is sent there
  and goes unanswered, long after a phone has left.
- **Phones on USB.** An interface driven by `ipheth` (iPhone) or
  `rndis_host` (Android) is an uplink of kind "phone" for as long as it
  is plugged in; nothing about it is stored. An iPhone only brings the
  link up for a computer it trusts: `usbmuxd`, started by udev when an
  iPhone is plugged in, asks it, and keeps the pairing record in
  `/data/lockdown` (linked from `/var/lib/lockdown`).
- **IPv6.** Each network gets a ULA /64; the uplink takes whatever RA or
  DHCPv6 offers; traffic is masqueraded (NAT66). That works on any
  network the router joins. Where the network delegates a prefix, devices
  get addresses out of it as well, and those are not translated: see
  *Prefix delegation*.
- **Prefix delegation.** dhcpcd asks for a prefix on every uplink whose
  IPv6 is automatic (`ia_pd 1 -`: obtain it, assign it to nothing) and
  reports it to the hook, where `internal/wan` reads prefix, length and
  lifetime from dhcpcd's variables (`new_dhcp6_ia_pd1_prefix1…`) into
  the uplink's state. dhcpcd also adds the reject route for the whole
  prefix, so that what no network uses of it is not sent back to the
  provider. Sharing it out is the control loop's: of the prefix of the
  uplink *in use*, each network has a /64 (`lan.Carve`), found from the
  network's id so that it keeps it while other networks come and go, the
  main network first where there are too few. The router's address in
  it (`::1`) goes on the bridge next to the ULA one. dnsmasq announces
  every prefix it finds there (`constructor:`), and when an address is
  taken away again (another uplink in use, the lease gone) goes on
  announcing its prefix with a preferred lifetime of zero for up to two
  hours: devices keep what they have open and start nothing new from
  it. The addresses have to be permanent ones for that, which is why the
  lease's lifetimes are tracked in `internal/wan` and not put on the
  bridge. The firewall leaves a source address in an uplink's delegated
  prefix alone when the packet leaves by that uplink, and masquerades
  it like any other when it leaves any other way: one provider does not
  route another's addresses, and neither does a VPN. Nothing comes in:
  the forward chain accepts only what belongs to a connection made from
  inside, which includes the ICMPv6 errors about it. Devices are still
  told the router's ULA address as their DNS server, because it stays
  the same whatever the provider does; the forwarder listens on the
  delegated addresses too, because a lookup that `dns.intercept`
  redirects arrives at the bridge address the kernel picks, the newest.
- **A tagged internet port.** A wired uplink with a VLAN ID runs on a
  sub-interface of the port (`v<id>-<port>`), made over netlink before
  the uplink starts and removed after it has stopped. Its key, its
  place in the order and its metric stay the port's. A PPPoE session's
  interface is `ppp-<port>`. Both names are shortened to a hash for a
  port whose name leaves no room in 15 bytes, and `config.MadeIf`
  recognises them, so that neither is ever listed or stored as a port.
- **Networks on one cable.** A network can have a VLAN ID, and a wired
  port for devices then carries, next to the one network it has
  untagged (or none), any of the others tagged. Each tagged network has
  a sub-interface of the port, made the same way and named by the same
  rule as a tagged internet port's (`v<id>-<port>`), and that
  sub-interface is a member of the network's bridge; the port itself is
  a member of its untagged network's bridge, or of none. The kernel
  hands a tagged frame to the sub-interface for its tag before the
  bridge the port is in gets to see it, so one port can be both.
  Nothing else knows about tags: addresses, router advertisements, the
  firewall, routes and rules by network, and which network a device is
  listed on all go by the bridge. The control loop names the
  sub-interfaces it wants on every pass (`tagged` in `reconcile`):
  one that has gone with its port (an adapter unplugged) is made again
  when the port is back, and one nobody names any more (another ID,
  the network taken off the port, the port given another job) is
  removed by the same sweep that removes a tagged internet port's.
- **Finding devices across networks.** Multicast DNS never leaves the
  network it is sent on, so the router repeats it (`internal/mdns`, a
  reflector: no cache, no answers of its own). A network with
  `discovery` set is paired with every network that is not isolated
  (`Config.DiscoveryPeers`); what is heard on one of a pair is sent out
  on the other from the router's address there, with the hop limit of
  255 that mDNS expects. One socket per address family is bound to port
  5353 and joins the group on the bridges that take part, and the input
  chain accepts the port there, for the two group addresses only.
  IPv6 goes out from a bridge's link-local address; what counts as the
  router's own is every address a bridge has, a delegated one included.
  A question that asks for a unicast answer is repeated without that
  bit: the answer would come to the router and end there, and as a
  multicast answer it is relayed back like any other. Questions from
  another port than 5353 (one-shot "legacy" lookups) are not relayed:
  their answers go back to the asking port alone, which a relay that
  remembers nothing cannot deliver. Three things keep it from looping
  or flooding: the sockets do not hear what they send, and anything
  from one of the router's own addresses is dropped; a message is
  repeated once in half a second wherever it is heard again (a hash of
  it is kept that long), which is what ends the exchange with a second
  reflector on the same networks or across a cable that joins them; and
  each network may have 50 messages a second repeated after a burst of
  100, the rest staying where it was said. The relay only makes devices
  visible. Whether a connection can follow is the forward chain's
  business as before: into an isolated network yes, out of it no. That
  covers what a phone opens itself (printing, HomeKit, most casting)
  and not what a device has to open back to the phone (AirPlay's timing
  and some speakers' control connections), for which the network must
  not be isolated. Discovery by other means than mDNS (SSDP, as DLNA
  and Sonos use) is not relayed.
- **PPPoE.** One pppd per wired uplink that signs in, with the kernel's
  PPPoE (`plugin pppoe.so`), over the port or its VLAN. Its options,
  with the user name and password, are in a file under the run
  directory that only root can read; the command line names the file
  and nothing else. pppd keeps the session up itself (`persist`, ten
  seconds between attempts, a session without an echo for half a
  minute given up), adds the default route with the uplink's metric as
  dhcpcd would, and runs the hook helper as `ip-up` and `ip-down`, which
  is how the provider's DNS servers arrive. The uplink's interface is
  the session's: addresses, routes, the firewall and the probe go by
  it, and it only exists while there is a session, so the status has
  the reason there is none from what pppd prints (`wan.pppPhase`): the
  provider refused the sign-in, or nothing answered. For IPv6, pppd
  gives the interface its link-local address and dhcpcd is started on
  it (IPv6 only) for as long as it is there. A session leaves 1492
  bytes for a packet: TCP segment sizes are cut to the route's on the
  way out, as for every uplink, and by number on the way in for any
  uplink whose MTU is below Ethernet's. The routing table that names
  the uplink in use names a session by its interface, since it has no
  gateway.
- **Holding an uplink to a speed.** When a line is full, something has
  to wait. Left alone it waits in the modem, in one long queue where a
  call's packets stand behind an upload's. So an uplink can be held to
  rates a little below what its line carries (`shaping`: per wired
  port, per saved Wi-Fi network, and for the phone on USB), which moves
  the waiting into the router, and there the queue is CAKE (`sch_cake`),
  which shares the rate between devices first and then between each
  device's connections, and tells a sender to slow down long before
  the queue is long. `internal/shape` does it with the `tc` program:
  CAKE's options and its keywords for what a line adds to each packet
  are tc's to know, and a netlink library would have to be taught each.
  What leaves is queued on the uplink's own interface, the one the
  traffic really leaves by (a PPPoE session's, a VLAN's):
  `cake bandwidth <up> … nat dual-srchost`. What arrives cannot be
  queued where it arrives, so everything is redirected (an ingress
  queue with one `u32` filter and `mirred`) to a stand-in interface,
  an IFB device per shaped uplink named `ifb-<interface>`, and queued
  there: `cake bandwidth <down> … nat dual-dsthost ingress besteffort`.
  `nat` makes CAKE look each packet up in connection tracking: after
  the router's address translation every packet on the uplink has the
  router's own address, and fairness is about the device behind it.
  `ingress` counts what CAKE drops against the rate, since it has used
  the line already. `besteffort` ignores the priority marks of packets
  from the internet, which a stranger set; what leaves keeps CAKE's
  default, so that a phone at home that marks its calls gets them
  ahead. What a line adds to a packet is told from what the router
  knows of it: `pppoe-ptm` for a PPPoE session (taken to run over
  VDSL2, where most do; over fibre that is 1.5 % too careful and
  nothing worse), `ethernet` for everything else (right for a cable to
  a modem, and the least any line adds for what is behind a Wi-Fi
  network or a phone), and `ether-vlan` on top where the uplink is
  tagged.
  The control loop names the uplinks to shape on every pass
  (`shapeSources`), for Wi-Fi with the setting of the network the
  station is on, and `shape.Manager.Apply` makes the kernel agree: it
  reads every queue with one `tc -s -j qdisc show` (which is also where
  the status's counters come from), leaves alone what is as wanted,
  changes rates in place (`tc qdisc replace` with the handle the queues
  are known by, `1ca:`, so that nothing passes unshaped meanwhile), and
  sets up anew what is missing: after a restart of the daemon, on an
  interface that was unplugged and plugged in again (it has the name
  and nothing else; the interface index tells), on a new PPPoE session.
  Whatever is not wanted goes: the queues of a source no longer shaped,
  and anything an earlier run left behind. The order matters in one
  place. A redirect to a stand-in interface that is gone or down loses
  every packet that arrives on the uplink, so the stand-in and its
  queue are made before the redirect and the redirect is removed
  before the stand-in. A router that shapes nothing runs tc once after
  it starts and not again. Like the firewall, the queues are left in
  place when the daemon stops.
  The rates come from the person or from **Measure**, which is
  `speedtest.run` with `via` `source:<key>`: the test's sockets are
  bound to the uplink's interface (as the reachability probe's are),
  so it measures that uplink whichever is in use, and for as long as
  it runs the uplink is left unshaped (`Core.measuring`), or it would
  measure the setting. The app fills in 95 % of the result.
  *Through a VPN* the fairness is lost: a tunnel's packets are
  encrypted before they reach the uplink's queue, and all CAKE sees of
  them is one flow per tunnel between the router and the VPN's server.
  The rate still holds, so the queue is still here and still short,
  and the tunnel as a whole gets its fair share next to devices that
  go out directly; but two devices inside the same tunnel are not kept
  from crowding each other. (That would take a queue on the tunnel's
  own interface, which nothing sets up.) The router's WireGuard server
  and the profiles' interfaces are not uplinks and are never shaped
  themselves.
- **MPQUIC.** A VPN profile of this type runs the `mpquic` program as it
  is released (`mpquic local`), attached to a network interface: its
  TUN mode, in which it proxies whatever the system routes there. Each
  MPQUIC profile has an interface where a WireGuard profile has one
  (`internal/vpn` makes it, a plain persistent TUN device, and
  `internal/mpquic` runs the program on it), so policy routing, the kill
  switch and per-path DNS need to know nothing about it, and nothing is
  encrypted a second time inside the router. The interface outlives the
  program: while no mpquic is attached, what is routed to it is dropped,
  not sent another way. The tunnel counts as up only while mpquic reports
  at least one working path. The release is pinned, with its hash,
  in `image/mpquic.pin` (v0.6.0 or newer: older ones have no TUN mode); a
  copy at `/data/porter/bin/mpquic` takes precedence on a router.
- **Uplink addresses.** dhcpcd configures an uplink (DHCP, router
  advertisements, DHCPv6) unless a family is set by hand, per wired port
  or per saved Wi-Fi network. A fixed address and its default route are
  put in place by porterd itself and re-checked every two seconds;
  dhcpcd then handles only the other family, or does not run. On a
  wired port that signs in with PPPoE, IPv4 is pppd's.
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
  While it starts or moves, the station is kept from scanning: hostapd
  scans as it starts, and when it cannot it takes the interface out of
  access point mode to do so, which drops the interface from its bridge.
  As a second line of defence, every pass of the control loop puts the
  interfaces of running access points back into their bridges.
- **Changes as one.** `config.set` validates the document, checks that
  it describes a router that can work (`Config.Consistent`), and the
  next pass of the control loop applies all of it: uplinks that stop
  being uplinks are released, then stations start, then access points.
  `config.check` runs the same checks, and the radio planning, on a
  document without applying it.
- **Control.** A Noise XX channel, independent of BLE pairing, above
  GATT and above TCP; see [PROTOCOL.md](PROTOCOL.md). Pairing is
  trust-on-first-use, gated by the power button, and only over
  Bluetooth: `core.Serve` is told which way a session came in
  (`Transport`), and that decides what the session is before it has
  asked for anything. The TCP listener (`internal/netctl`) hands over
  only sessions of phones that are paired already. It listens on every
  address and is fenced in twice: by the firewall's input rule, which
  names the networks that are not isolated the way the rule for SSH
  does, and by a check of each connection's two addresses, because the
  listener is there before the first ruleset is. `ping` and the
  listener's own checks do not go through the control loop, which can
  be busy for seconds while a radio restarts: whether a link works must
  not depend on whether the router has time.
- **Coming in from outside.** By default the input and forward chains
  let nothing in from an uplink but answers. Three things under
  `inbound` in the configuration change that, each as narrowly as it
  can.
  *The WireGuard server* (`vpn.Server`, interface `wgsrv`) is the other
  end of what a VPN profile is: devices of the owner's connect to it.
  Its range is treated as one more of the router's networks, one that
  is not isolated: the interface is in `lan_ifs` and its prefixes in
  `lan_nets4/6`, so what comes out of it is classified like a device's
  at home (rules for everyone and rules by address apply, then
  `policy.default`; rules by device or by network cannot, a peer being
  on no bridge and having no hardware address), asks the forwarder like
  one (which listens on the router's address in the range, the DNS
  server every peer is told), and reaches the other networks through
  `lan_lan`, an isolated one included. The app's port is opened to it,
  and the router's address in the range is among `control.addrs`; SSH
  is not. Its own UDP port is accepted from uplinks, and from inside
  for a phone that comes home with the connection up. The server's own
  packets carry no mark, so like everything the router sends for itself
  they follow the main table and the uplink override and can never be
  sent into a profile's tunnel, whose table only marked packets reach;
  that is why it sets no fwmark. A peer that is switched off is not
  configured into the interface at all. The server's key is made in
  `config` (`Normalize`, stdlib X25519) and stored like a profile's; a
  peer's key pair is made in `server.peer_add`, the public half stored,
  the private half put into the configuration text that is the call's
  answer and kept nowhere. The range is chosen from `ServerSubnets`,
  apart from the list networks are given theirs from, and never moves
  (the peers' configurations carry its addresses): a network that has
  to renumber avoids it.
  *A forwarded port* is, over IPv4, a `dnat` rule in `dstnat` for what
  arrives on an uplink for the router itself, and a second for what
  arrives from inside for any address of the router's except its own in
  the networks: the public address used at home ("hairpin"). Such a
  connection is masqueraded on its way back in, or the device would
  answer its neighbour directly, and passes `lan_lan` whatever the
  networks are to each other, the port being open to the whole internet
  anyway. (A device cannot reach itself this way: the kernel does not
  route a packet whose two addresses are the same.) What a device
  answers to a connection from outside comes in from a bridge like
  anything it sends, but must not be classified: prerouting returns on
  `ct direction reply`, and the forward chain lets it out by an uplink.
  It leaves by the uplink in use, like the router's own packets; a
  connection that came in by another uplink does not get its answers.
  A forward names its device by hardware address and is sent to the
  device's fixed address, which `config` insists on; `core.pinForwards`
  gives a device without one the address it has, when a forward for it
  is written. Over IPv6 nothing is translated: the forward chain accepts
  the device's port at the addresses the device table has seen it with
  inside a network's /64 of a delegated prefix (`Runtime.Addrs6`), which
  follow the device and the prefix by themselves. Those addresses are
  left out of `ClassKey`; the forwards are in it, so that taking one
  away flushes connection tracking and ends what came in by it.
  *Dynamic DNS* (`internal/ddns`) tells one of four kinds of provider
  the router's public address when it or the settings change, once a
  day otherwise, and after a failure with pauses that double from a
  minute to an hour; a provider that refuses outright (a wrong password)
  is not asked again for a day, or until something is changed. Its
  requests are the router's own and so leave by the uplink in use.
  Errors are put in words for the status and never carry the URL, which
  for most providers has the secret in it.
  *Whether any of it can work* is the question `inbound.reach` answers
  (`ddns.Classify`). While anything is switched on, the router asks
  `inbound.check_url` (by default a page of Cloudflare's, whom the speed
  test and the fallback resolver talk to already) over IPv4 which
  address its requests come from:
  when the uplink or its address changes, and every ten minutes. The
  same address as the uplink's own means reachable; another means
  another router in front (own address private) or the provider's
  translation. Without an answer the address is judged by its range:
  private, 100.64.0.0/10, or public. What a peer's configuration names
  as its endpoint follows from that: the dynamic DNS name, else the
  public IPv4 address, else a public IPv6 address, else nothing, and
  then no device is made.
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

- *A newer version changes the format of the settings.* A change to
  existing fields means raising `SchemaVersion` in
  `daemon/internal/config` and adding the step from the old version to
  `migrations` there. Without that step the new version would reject
  the stored settings and start in the factory state. A field that is
  only new needs no step (`Normalize` fills in its default), but the
  number is raised for it all the same, as it was for format 3 (not
  again within format 3, which no release has had yet): the app
  writes the whole document back, an app that does not know the field
  would drop it, and the number is how that app finds out that it must
  not write.
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
The image keeps the default. The other is chosen when the image is
built (it cannot be switched on a running router: the root file system
is read-only and replaced whole by every update):

```bash
sudo PORTER_WIFI_FIRMWARE=minimal image/build.sh
```

## The daemon

```bash
cd daemon && go test ./...
```

The end-to-end tests run the real daemon in network namespaces. They
need root and `ip`, `nft`, `dnsmasq`, `dhcpcd` and `wg` (with the
kernel's WireGuard, or a userspace one named by `PORTER_WG_USERSPACE`);
for PPPoE also
`pppd` and `pppoe-server`, and a kernel with VLANs, PPPoE and IPv6 for
the tests of those, and with CAKE and IFB interfaces (`sch_cake`,
`sch_ingress`, `cls_u32`, `act_mirred`, `ifb`) for the test of holding
an uplink to a speed (they skip themselves, saying why, where it has
none; the relay for multicast DNS is tested over IPv4 on any kernel):

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
  flows, traffic history and speed tests; the app's way in over the
  network (a paired phone from the main network, a phone that is not
  paired, the internet side, an isolated network, and the setting that
  switches it off); a second network (its own addresses, isolation, a
  route and a rule of its own, and its removal); a second uplink
  standing in for a phone (the order of uplinks, and falling back when
  the first stops reaching the internet); an uplink held to a speed
  (`shape_test.go`: the queues for both directions with their options,
  a device's download and upload landing near the rates set where they
  ran at hundreds of times that a moment before, the status counting
  what was slowed down, a measurement through the uplink seeing the
  line and not the setting, a change of rates, queues and a stand-in
  interface removed behind the router's back and put back, a phone
  that is unplugged and plugged in again, and everything gone when it
  is switched off; the band a measured rate has to land in is wide and
  a measurement is repeated once, because the machines these run on
  are shared); coming in from outside
  (`inbound_test.go`: a namespace beyond the upstream network brings up
  WireGuard from the configuration `server.peer_add` gave it and
  reaches a device at home, the router's DNS, the internet through the
  router and the app's port, under a rule by address, until it is
  switched off; and a forwarded port, from the upstream network and
  from inside, to and from a network that is kept apart, restricted to
  some addresses, switched off, and over IPv6 at a device's delegated
  address); and the ways a home connection
  arrives (`home_test.go`): the internet under a VLAN tag; PPPoE against
  a real `pppoe-server` (nobody answering, a wrong password, a session,
  the server restarting under it); and a prefix delegated by a DHCPv6
  server written out in the test (`dhcp6_test.go`), with a device going
  out from its own address, nothing coming in, a second network with a
  /64 of its own, and the prefix being withdrawn; devices finding each
  other across networks (`trunk_test.go`: a question asked by multicast
  DNS on one network heard on another from the router's address there,
  the answer coming back, nothing repeated twice or handed back, the
  connection it leads to, and none of it with the setting off); and
  several networks on one cable: a namespace that does what a managed
  switch does takes the cable, splits the tags, and a device behind
  each kind of its ports gets its addresses from the right network and
  is kept apart as configured, through a change of VLAN ID, the cable
  being unplugged and plugged in again, and a port for tagged networks
  only. These explain themselves at length when they fail, for whoever
  has only the log of a run elsewhere. There is no Bluetooth in the namespaces, so the phone
  these tests connect as is written into the pairing list before the
  daemon starts.
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
link and the TCP link, the supervisor that keeps a connection up over
the two, and a sample-data router for demo mode. Its tests check the
Swift protocol code against vectors generated by the Go implementation
(`go test ./internal/proto -run TestVectors -update` regenerates them),
that the demo data decodes through the real models, and how the
supervisor behaves when links come and go (below). With
`PORTER_CONTROL_SOCKET=/run/porter/control.sock` set they also drive a
running `porterd`. The package builds on Linux as well, where it is used
only for these tests.

Signing settings live in `ios/Local.xcconfig`, which git ignores; do not
pick a team in Xcode's Signing pane, which would write it into the
project file.

**Demo mode.** Launching with `-demo` runs the app against the built-in
sample router and never touches Bluetooth. `-demoScreen <name>` opens one
screen directly: a tab name (`overview`, `internet`, `devices`, `vpn`,
`settings`) or `traffic`, `flows`, `speedtest`, `device`, `adblock`,
`networks`, `outside`, `smoothing`, `software`, `setup`, `connect`, `share`. The *iOS* workflow
uses this to publish screenshots as an artifact.

**Two ways of changing things.** Most controls apply at once: the app
shows the new value immediately (`AppModel.update`) and takes it back if
the router refuses. The settings that depend on each other (Wi-Fi names,
networks, what radios and ports are for) are edited as a draft instead
(`NetworkSetupPage`): the router is asked what the draft would do
(`config.check`) as it changes, and Apply sends it as one `config.set`.

### Two links, one connection

The app reaches a router over Bluetooth or, much faster, over TCP on the
router's own network. Which of the two is there changes as the phone
moves about, and nothing on screen is to change with it. So the app
does not hold a link. It holds a `ConnectionSupervisor` (PorterKit,
`Supervisor.swift`), which is *the connection to the router* for as
long as the app wants one: calls are made on it, events come from it,
and it sees to the links underneath. `AppModel` follows the states it
reports and otherwise does not know which link is in use.

What it does, with the times the app runs with (`Timing`; the tests
shorten them):

| | |
|---|---|
| Start | Every remembered address (TCP, 2 s to connect) and Bluetooth are tried at the same time. The first to finish the handshake and `hello` is used; the others are cancelled. |
| The router's key | The key pinned at pairing is checked on every link. Another key over the network only ends that attempt (the address is somebody else's; it happens on any network numbered like yours). Another key over Bluetooth ends the connection, with a message. |
| On Bluetooth | The network is tried again every 30 s, when the app comes to the front, and when the router reports other addresses. When it answers, the new link takes over at once: new calls go to it, calls under way finish on the old link, which is closed after the last of them (or after 100 s, past the longest deadline a call has). |
| On the network | A `ping` every 2 s has to be answered within 3 s, so a link that has gone silent is noticed within 5 s; one that breaks with an error, at once. |
| A link is lost | Everything is tried again straight away, and then every second. Calls made meanwhile wait for the new link, up to 10 s. Calls that change nothing on the router (the list is in the supervisor) are made once more if they were lost with the link; others fail as they would have. After 2 s the app shows a small sign, which goes when the link is back. |
| Nothing answers for 20 s | The supervisor reports `searching` and the app returns to the search screen. The supervisor goes on trying every 5 s. |
| After any change of link | The state `connected` is reported again, and the app reads the status, the configuration, the devices and the radios afresh without anything showing: events may have been missed in between. Events are taken from the link in use only. |

Why it is built like this:

- *One object that outlives its links.* The trouble with switching
  between two transports is everything that hangs off a connection: a
  screen that is torn down with it, a call that fails with it, an edit
  that is forgotten. If the thing the app holds never goes away while a
  link is being replaced, there is nothing to put back afterwards.
- *Make before break.* On the way to the faster link there is never a
  moment without one, so no call has anything to wait for or fail on.
- *Writes are made once.* A configuration write whose answer was lost
  cannot be judged from the phone: the router may or may not have made
  it. `config.set` therefore carries an id, and the router remembers the
  ids of the writes it made. The app sends the very same request again
  over the next link (`RouterAPI.writeConfig`) and is told what became of
  the first. Should the router have restarted in between and forgotten,
  the write comes back as a conflict; the app then fetches what the
  router holds and writes only if the edit is not in it yet.
- *Quiet unless it matters.* The network not answering is the normal
  case whenever the phone is somewhere else, so nothing on that link is
  ever an error to show. An attempt the person asked for (picking a
  router) reports how it went. One the app makes by itself reports only
  a router that is there over Bluetooth and fails to connect five times
  running.

`SupervisorTests` are the check on all of this. They run the supervisor
against a router that speaks the real protocol (`FakeRouter`: the
responder's side of the handshake, the session, a handful of calls)
over links that can be cut, or made to swallow everything in silence,
at a chosen moment. On Linux as on a Mac.

`TCPTransport` and the Bluetooth link only build on Apple platforms.

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
- Without a delegated prefix clients hold only ULA IPv6 addresses, so
  for dual-stack destinations most systems prefer IPv4.
- A delegated prefix is announced to devices with the lifetimes of the
  router's own address leases (`lan.lease_minutes`), not with those of
  the delegation, and taken back when the delegation ends.
- An uplink held to a speed is held to the rates typed or measured
  once. A line whose speed moves (a phone's, a busy hotel's) is then
  shaped too loosely at its slow moments, when the queue is the
  modem's again, and too tightly at its fast ones. What a line adds to
  each packet is a guess from the kind of uplink (see *Holding an
  uplink to a speed*).
- A speed test counts a download by what it has read, and an upload by
  what the server has acknowledged (`TCP_INFO`), not by what it has
  handed to the kernel: on a slow line with a deep queue, a good part
  of that is still waiting to be sent when the test ends, and a figure
  that counted it would have **Measure** set an upload rate the line
  does not carry.
