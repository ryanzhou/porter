# Porter control protocol

The app controls the router over Bluetooth Low Energy and, once it is
paired, over TCP inside the router's own networks. Both carry the same
sessions and the same calls. This document is the contract between
`daemon/internal/proto` + `daemon/internal/ble` + `daemon/internal/netctl`
(Go) and `ios/PorterKit` (Swift). The shared test vectors in
`ios/PorterKit/Tests/PorterKitTests/Resources/vectors.json` are generated
by the Go implementation and checked by both.

Layers, bottom up:

| Layer    | Unit     | Purpose                                             |
|----------|----------|-----------------------------------------------------|
| GATT     | fragment | one ATT write or notification (Bluetooth)           |
| Link     | packet   | fragments reassembled, or a length-prefixed packet on TCP; ≤ 65535 bytes |
| Session  | message  | Noise XX encrypted; any size up to 1 MiB            |
| RPC      | JSON     | requests, responses, events                         |

## GATT service

| What     | UUID                                   | Properties                     |
|----------|----------------------------------------|--------------------------------|
| Service  | `7E0D0001-5C3A-4B8E-9F1D-706F72746572` | advertised                     |
| RX       | `7E0D0002-5C3A-4B8E-9F1D-706F72746572` | write, write without response  |
| TX       | `7E0D0003-5C3A-4B8E-9F1D-706F72746572` | notify                         |
| Info     | `7E0D0004-5C3A-4B8E-9F1D-706F72746572` | read                           |

No BLE-level pairing, bonding or link encryption is used or required.

**Info** is unauthenticated JSON for the discovery screen:

```json
{"v":1,"name":"Porter","id":"3fa9c210","claimed":true,"pairing":false}
```

`id` is the first four bytes of the router's static public key in hex.
Nothing in Info is trusted; the handshake authenticates the router.

## Link: fragments

Each write to RX and each notification on TX is one fragment:

```
byte 0-1   session tag, big endian
byte 2     flags: 0x80 = first fragment of a packet, 0x40 = last
byte 3..   payload
```

The app picks a random tag when it connects and uses it for every
fragment; the router echoes it. The app ignores notifications carrying
another tag (TX notifications reach every subscribed central). A fragment
with the *first* flag and an unknown tag starts a new session.

A fragment is never longer than 512 bytes, the limit for an attribute
value, whatever the MTU: BlueZ silently discards longer writes and cuts
longer notifications short. Within that, the app writes at most ATT MTU −
3 bytes and the router notifies at most ATT MTU − 5, taking the MTU from
what BlueZ reports with the app's writes. (The two bytes less are because
BlueZ sends notifications in ATT's batched form to phones that support
it.)

The app should use write-without-response and honour
`canSendWriteWithoutResponse`.

## Link: TCP

The router listens on TCP port **7678** for as long as
`system.bluetooth_only` is false. A packet on the stream is its length as
two bytes, big endian, followed by that many bytes; there is nothing
else. The local control socket (`porterctl`) is framed the same way.

Who may connect:

- The firewall accepts the port from the router's own networks that are
  not isolated and from the devices connected to its WireGuard server
  (`inbound.server`), and from nowhere else: not from an uplink, a
  profile's tunnel or an isolated network. The listener checks the same
  again for each connection (to one of the router's addresses in such a
  network, from an address inside one), because it exists before the
  first ruleset does.
- Only paired phones get a session. The handshake is what shows the
  router whose connection it is, so it runs to the end; a key that is not
  in the pairing list is then disconnected without a single call
  answered, `hello` and `pair` included. Pairing is done over Bluetooth.
- A connection has 10 seconds to finish the handshake. At most 4
  connections may be in the handshake at once and at most 8 sessions
  open; a phone that opens a third session loses its oldest, which it
  has usually lost already without the router having noticed. TCP
  keepalive is on (15 s idle, then 3 probes 5 s apart), and unacknowledged
  data ends a connection after 30 s.

The router tells the app where to connect in `hello` and in the status
(`control`). The addresses follow the networks: when one is renumbered
the status changes, and with it what the app remembers. While the
WireGuard server runs, the router's address in the server's range is
among them: it is the one a phone reaches from anywhere once it has
connected to the server, and the app, which tries every address it
remembers, needs to know nothing more to work from outside.

How the app chooses between the two links, and moves from one to the
other, is described in [DEVELOPMENT.md](DEVELOPMENT.md#two-links-one-connection).

## Session: Noise

`Noise_XX_25519_ChaChaPoly_SHA256`, prologue = ASCII `porter/1`, empty
handshake payloads. The app is the initiator. One handshake message per
packet:

```
-> e                    32 bytes
<- e, ee, s, es         96 bytes
-> s, se                64 bytes
```

After the handshake each side holds the other's static public key.

- The **app** must compare the router's key with the one stored when it
  paired, and refuse the session if it differs. On first pairing the key
  is stored (trust on first use; see *Pairing*).
- The **router** looks the app's key up in its pairing list. Over
  Bluetooth, unknown apps get a session in which only `hello` and `pair`
  work; over TCP they get none (see *Link: TCP*).

Transport packets are Noise transport messages (ChaCha20-Poly1305, 64-bit
counter nonce per direction, no rekeying). The plaintext of each is:

```
byte 0     1 if more chunks of this message follow, 0 on the last
byte 1..   chunk (at most 8000 bytes)
```

A message is the concatenation of its chunks, at most 1 MiB.

## RPC

Messages are UTF-8 JSON.

```json
{"id": 7, "m": "status.get", "p": {}}          // request (p optional)
{"id": 7, "r": {...}}                           // success
{"id": 7, "e": {"code": "invalid", "msg": "…"}} // failure
{"ev": "status", "d": {...}}                    // event, router to app
```

Error codes: `unpaired`, `pairing_closed`, `invalid`, `not_found`,
`conflict`, `no_station`, `internal`, `unknown_method`. `msg` is written
for people and may be shown as is.

### Pairing

A router with no paired phone accepts the first `pair` it receives.
After that, pairing is open only for two minutes after a short press of
the Pi's power button, or after `pairing.open` from an already paired
phone; it closes again as soon as one phone has paired. `pair` is only
ever accepted over Bluetooth: being within its range, and the button,
are what pairing rests on.

| Method | Params | Result |
|--------|--------|--------|
| `hello` | – | `{name,id,version,paired,claimed,pairing,ready,control?}` – `control` as in the status, for a paired phone |
| `ping` | – | `{}` – answered without involving the control loop; the app's sign that a link works |
| `pair` | `{name}` | `{}` or `pairing_closed` |
| `pairing.open` | – | `{}` |
| `pairing.list` | – | `[{key,name,added,last_seen,self}]` |
| `pairing.revoke` | `{key}` | `{}` |

### Status and configuration

| Method | Params | Result |
|--------|--------|--------|
| `status.get` | – | Status |
| `config.get` | – | Config |
| `config.set` | `{config, id?}` | `{rev}`; `conflict` if `config.rev` is stale |
| `config.check` | `{config}` | `{error?, notes[], effects[]}`; changes nothing |
| `events.list` | – | `[{t,kind,msg,mac?}]` |

The configuration is one document (`daemon/internal/config/config.go`
defines every field). The app edits a copy and writes the whole document
back with the `rev` it read; the router validates all of it, rejects
stale writes with `conflict`, and applies the change atomically: a
change to several things at once never passes through a state in which
only some of them have been made.

A write may name itself with an `id` (any string of up to 64 bytes; the
app uses a random one per write). The router remembers the last 32 it
carried out, until it restarts, and answers a `config.set` with one of
those ids with the revision that write produced, without looking at the
document again. An app whose link went before the answer arrived sends
the same request again over the next link: the change is made if it had
not been, and not made twice if it had.

Beyond every field being well formed, a change has to describe a router
that can work: at most one radio joins upstream networks, and something
is left to carry the router's own network. `config.check` answers what
`config.set` would do with a document without doing it: `error` is why
it would be refused, `notes` what the hardware cannot do of it (a Wi-Fi
network that fits on no radio), `effects` what will be noticed (Wi-Fi
restarting, devices having to rejoin or getting new addresses, the
devices behind a wired port losing their connection when the port
changes hands or a network on it changes its VLAN ID).

The document's format is `version` 3. (Format 3 added
`system.bluetooth_only` and `system.timezone`; `vlan`, `pppoe`, `tagged`
and `shaping` for wired ports; `vlan` and `discovery` for networks;
`shaping` for saved Wi-Fi networks and `wan.phone`; `inbound`; and
`groups`, with `groups` in a rule's `src` and `allow_until` for a
device, to format 2 and changed nothing else; a router that stored
format 2 carries it over as it is, with nothing let in, no source held
to a speed and no groups.) Its parts, briefly:

```
system      {name country ssh{enabled authorized_keys[]} bluetooth_only timezone}
            bluetooth_only: the router does not listen on TCP
            timezone: the IANA name of the zone schedules are kept in
            ("Europe/Berlin"), "" until one is set. The app sets it to
            the phone's when it finds it empty. A name the router's
            copy of the zone database lacks is refused.
wifi[]      {id ssid passphrase security hidden isolate network enabled}
            the Wi-Fi networks broadcast; the first ("main") always exists
networks[]  {id name address ula_prefix isolated route vlan discovery}
            the router's own networks; the first ("lan") always exists.
            address and ula_prefix may be left empty for a new one: the
            router chooses. isolated: cannot open connections to the
            others. route: "", "direct", "block" or "vpn:<id>".
            vlan: the VLAN ID the network has on wired ports that carry
            it tagged, 1 to 4094, 0 for none; no two networks have the
            same. discovery: multicast DNS is relayed between this
            network and every network that is not isolated, both ways
lan         {auto_renumber lease_minutes domain ipv6}   shared by all networks
radios[]    {id role band channel width}   role "off" | "ap" | "wan" | "ap+wan"
ethernet[]  {name role ip vlan pppoe shaping network tagged[]}
            role "wan" | "lan" | "off".
            For a "lan" port: network: the id of the network it carries
            untagged, "" for none; tagged: the ids of the networks it
            carries under their VLAN IDs as well. Each of those needs a
            vlan, and none can be the port's untagged network. A "lan"
            port with neither is given the main network; for any other
            role the router sets network to "lan" and empties tagged.
            For a "wan" port: ip {v4 v6}, each {mode address gateway dns[]},
            mode "auto" | "static"; vlan: the VLAN ID the internet arrives
            under, 1 to 4094, 0 for none; pppoe {enabled username password}:
            sign in to the provider, in place of ip.v4 (both families of
            ip must then be "auto"). The router clears username and
            password when pppoe is not enabled.
            shaping {enabled down_kbps up_kbps}: the uplink is held to
            these rates, in kbit/s, each from 100 to 10000000. Both are
            needed while it is enabled; they are kept while it is not.
wan         {networks[] wifi_mac eth_mac order[] phone probe}
            networks[] {ssid psk hidden priority ip shaping}: the saved
            Wi-Fi networks; shaping as for a wired port, applied while
            the router is on that network.
            order: uplink keys, best first: "eth:<port>", "wifi", "phone"
            phone {shaping}: what is set for a phone sharing its
            connection over USB, whichever phone it is
policy      {default rules[{id name enabled src{devices cidrs networks groups} dst action}]}
            src.groups: ids of groups; the rule is for whoever is in
            them when it is applied. A source that names only groups
            without members matches nobody.
devices[]   {mac name route paused ip no_adblock allow_until}
            allow_until: unix time until which the device is online
            although a schedule or override "pause" has its group
            offline (not against the group's paused); 0 or past for
            not at all.
groups[]    {id name icon members[] route paused no_adblock schedules[] override}
            a set of devices, by hardware address; a device is in one
            group at most. icon: a key of the app's ([a-z0-9-], up to
            20), which the router keeps and does not interpret.
            route, no_adblock: for its members; a member's own route
            comes first, and it skips ad blocking if either says so.
            paused: every member is cut off until it is cleared.
  schedules[] {id name enabled days[] from to}
            a time the group is offline. days: the days of the week it
            starts on, 0 (Sunday) to 6. from, to: "HH:MM" on a clock in
            system.timezone; a to that is not after from is on the day
            after, and the two the same is 24 hours. An enabled
            schedule needs system.timezone to be set.
  override  {mode until}
            what was asked for now. mode "allow": online although a
            schedule says otherwise; "pause": offline; each until
            `until` (unix time). "" for none. One whose time has passed
            means nothing and is left in place.
inbound     {server forwards[] ddns check_url}   what may come in from the internet
  server    {enabled port private_key address ula_prefix peers[]}
            the router's own WireGuard server. port: UDP, 51820 unless
            changed. private_key, address (the router's, with prefix, like
            10.89.0.1/24) and ula_prefix are made by the router when left
            empty and then stay: the devices' configurations depend on
            them. The range may overlap none of the networks.
  peers[]   {id name public_key address enabled full}
            a device that may connect: address is its IPv4 address in the
            server's range (its IPv6 address is the same host number in
            ula_prefix); full records that its configuration sends
            everything through the router. Made by `server.peer_add`;
            renamed, switched off and removed by writing the document.
  forwards[] {id name enabled proto ports device to from[]}
            proto "tcp" | "udp" | "both"; ports: the port on the outside,
            or a range ("5000-5010"); device: the hardware address of the
            device it goes to, which has to have a fixed address
            (devices[].ip): the router gives it the one it has at the
            moment when a forward is written for a device without;
            to: the port on the device, for a single port, 0 for the
            same; from: when not empty, the only address ranges (IPv4
            or IPv6) it is open to. No two enabled forwards share a port
            of a protocol, and none takes UDP 68, 546 or the server's.
  ddns      {enabled provider hostname server username password url ipv6}
            provider "dyndns2" (server: the update server, host or
            host:port; username, password) | "duckdns" (password: the
            token; hostname ends in .duckdns.org, which the router adds
            to a bare name) | "cloudflare" (password: an API token;
            hostname: the record) | "custom" (url, fetched with {ip} and
            {ip6} replaced). ipv6: publish the IPv6 address as well.
  check_url answers with the address a request comes from (the address
            alone, or a line "ip=<address>"); asked while anything above
            is enabled, to learn the router's public IPv4 address
```

Status (`daemon/internal/core/api.go`):

```
name id version time uptime temp ready rev pairing
           time: the time schedules go by (unix time): the router's
           clock, or what the site its connection is checked against
           gave while the clock itself has not been set since the start
internet   "down" | "connecting" | "limited" | "captive" | "online"
active     interface of the uplink in use: the first in the configured
           order that reaches the internet or stands behind a sign-in page
wan[]      {if kind key label metric rank carrier internet ipv4 gateway ipv6[] has_v6 prefix? dns[] pppoe? portal_url captive_api mac}
           kind "eth" | "wifi" | "phone"; key: its entry in wan.order;
           rank: its place in that order, 0 first.
           if: the interface that reaches the internet, which for a wired
           port is the port, its VLAN ("v35-eth0") or its PPPoE session
           ("ppp-eth0"); key stays "eth:<port>".
           prefix: the IPv6 prefix delegated to the router there.
           pppoe: for a port that signs in, "signing_in" | "wrong_login"
           (the provider refused the user name or password) | "no_answer"
           (nothing answers PPPoE on the cable) | "up"
portal     {captive url device}
aps[]      {radio iface state band channel mode error ssids[]}
           ssids: ids of the Wi-Fi networks the radio carries
stas[]     {radio iface state ssid bssid freq signal failed fail_reason}
           state "connected" | "connecting" | "scanning" | "disconnected"
off_air[]  ids of Wi-Fi networks that are switched on but fit on no radio
ports[]    {name label phone carrier}   wired interfaces that are plugged in
lan        {address ula clients networks{id: devices online} prefixes{id: prefix}}
           prefixes: the /64 each network has of the prefix delegated by
           the uplink in use; a network that is not in it has its
           ula_prefix only
control    {port addrs[]}   where the app connects over TCP: the router's
           addresses in its networks that are not isolated (IPv4, and the
           ULA where IPv6 is on). port 0 and no addresses while it does
           not listen.
vpns[]     {id name type if state error endpoint handshake rx tx paths? of?}
           type "wireguard" | "mpquic"; paths of an MPQUIC tunnel working, of how many
geoip      {available date}   country database for country rules and flows
adblock    {enabled domains blocked lists[{url domains updated error}]}
inbound    {reach address public public6 host server forwards[] ddns}
           reach: over IPv4, "direct" (the uplink in use has a public
           address of its own) | "nat" (behind another router) | "cgnat"
           (behind the provider's address translation) | "offline".
           address: the uplink's own IPv4 address; public: the address
           the internet sees, once check_url has answered, or address
           where that is taken to be public; public6: a public IPv6
           address of the router's.
           host: what a device outside is told to connect to: the
           dynamic DNS name if one is kept, else public (not with
           "cgnat"), else public6; absent when there is none.
           server {state error public_key peers[{id connected endpoint handshake rx tx}]}
           state "off" | "up" | "error"; a peer is connected while its
           last handshake is younger than three minutes; endpoint: where
           it last connected from; rx, tx: bytes from and to it.
           forwards[] {id to to6[]}: where an enabled forward leads: the
           device's IPv4 address, and its IPv6 addresses out of a
           delegated prefix, to which the port is open untranslated.
           ddns {state updated address address6 error}
           state "off" | "waiting" (no address to publish, or not
           updated yet) | "ok" | "error"; error is for people.
shaping[]  {key if state down up error?}
           one entry for each uplink that is there and has shaping
           enabled. key: its entry in wan.order; if: the interface the
           queues are on (as in wan[]).
           state "active" | "waiting" (the interface is not there yet:
           a PPPoE session that is not up) | "measuring" (left unshaped
           while a speed test runs through it) | "error" (error: what
           the tc program said).
           down, up {kbps dropped marked}: the rate, and how many
           packets the queue has dropped or marked (ECN) to make a
           sender slow down since it was put there. The counters are
           read every few seconds and do not cause status events.
clock      {zone trusted}
           zone: system.timezone. trusted: since the router started,
           `time` has been confirmed by a time server, by
           `system.set_time`, or by the Date of the site its
           connection is checked against. Until then schedules go by
           the clock as restored from the time last saved.
groups[]   {id connected offline why? schedule? until? allowed_until? next? next_schedule?}
           one entry for each group, in the configuration's order.
           connected: how many members are on a network now.
           offline: its members are cut off, by `why`: "paused" (the
           group's switch), "timer" (override "pause") or "schedule",
           then with the schedule's id. until: when it comes back by
           itself (unix time), through whatever follows without a gap;
           absent for the switch, and when nothing ends it within a
           week. allowed_until: a schedule is in force and override
           "allow" holds it off until then. next, next_schedule: when
           a schedule next takes it offline, within a week.
held[]     {mac why group? until?}
           every device that is cut off right now. why "device" (it is
           paused itself), else its group's why, with the group's id
           and until. A member whose allow_until has not passed is not
           held by its group's schedule or timer.
```

A device that is held is treated in every respect as one with `paused`
set: its traffic is refused, its DNS lookups are refused, a port
forwarded to it is closed, and connection tracking is flushed when the
set of held devices changes. `groups[]` and `held[]` change at the
start and end of a schedule, which causes a `status` event; the control
loop notices within five seconds of the minute.

### Radios and upstream Wi-Fi

| Method | Params | Result |
|--------|--------|--------|
| `radios.list` | – | `[{id,phy,iface,driver,bus,product,mac,bands[],ap,sta,ap_sta,max_ap,max_ap_sta,present,role}]` |
| `wifi.uplink` | `{radio}` | `{}` – makes that radio the one that joins upstream networks (`""`: none) |
| `wifi.scan` | `{quick?}` | `[{ssid,signal,freq,security,known}]`; takes several seconds, or with `quick` answers at once from the last scan and starts a new one |
| `wifi.connect` | `{ssid,psk,hidden}` | `{}` – saves the network with top priority; one that is saved already keeps its `ip` and `shaping` |
| `wifi.forget` | `{ssid}` | `{}` |
| `wan.new_mac` | `{kind: "wifi"\|"eth"}` | `{}` – new random upstream MAC |
| `wan.recheck` | – | `{}` – probe reachability now |

`wifi.uplink` arranges the other radios in the same step: a radio that
was the uplink goes back to broadcasting, and the chosen one shares
itself between both jobs only when no other radio carries the router's
network. `max_ap` and `max_ap_sta` say how many Wi-Fi networks a radio
can broadcast, alone and while it is also the uplink.

Whether joining succeeded shows up in `stas[]`: `state` becomes
`connected`, or `failed`/`fail_reason` (`wrong_password`, `rejected`) name
the network that was refused.

### Devices

| Method | Params | Result |
|--------|--------|--------|
| `devices.list` | – | `[{mac,hostname,ipv4,ipv6[],first_seen,last_seen,online,online_since,link,signal,up,down,name,route,paused,ip,portal,no_adblock,allow_until?,group?}]` |
| `device.set` | `{mac,name,route,paused,ip,no_adblock,allow_until}` | `{}` – replaces the device's settings |
| `device.forget` | `{mac}` | `{}` – also takes it out of its group |
| `device.queries` | `{mac}` | `[{t,mac,name,type,path,failed}]` recent DNS queries |

`paused`, `route` and `no_adblock` in `devices.list` are the device's
own settings. `group` is the id of the group it is in; which group a
device is in is changed by writing `groups[].members`. Whether it is
cut off right now, and by what, is in the status (`held[]`). The list
also has the members of groups that the router has never seen.

### Traffic

| Method | Params | Result |
|--------|--------|--------|
| `stats.get` | `{mac}` (`""` = whole network) | `{minutes: [60 × {t,up,down}], hours: [72 × {t,up,down}]}`, oldest first, evenly spaced |
| `stats.top` | `{hours}` | `[{mac,up,down}]`, busiest first |
| `flows.list` | `{mac}` (`""` = all devices) | `[{mac,proto,dst,port,name?,country?,path,up,down,started,last,active}]` – active first (busiest first), then recently ended (latest first) |
| `speedtest.run` | `{via: "direct"\|"vpn:<id>"\|"source:<key>"}` | `{t,via,latency_ms,down_mbps,up_mbps,error?}` after about 20 s |
| `speedtest.history` | – | the last 20 results, newest first |

A flow is a connection from a LAN device to somewhere beyond the router,
read from connection tracking every few seconds. `name` is the host name
the address was resolved as through the router's DNS, `country` comes
from the country database, `path` is the route the connection was
classified onto. Connections that were refused by a block rule never
enter connection tracking and are not listed.

`speedtest.run` measures from the router itself, against the server in
`config.speedtest`, optionally through a connected VPN. While it runs the
router sends `speedtest` events.

With `via` `"source:<key>"` (a key as in `wan.order`: `source:eth:eth0`,
`source:wifi`, `source:phone`) the test goes out by that uplink, whether
or not it is the one in use, and measures the line itself: an uplink
with `shaping` enabled is left unshaped for as long as the test runs
(`shaping[].state` is `measuring`). The app fills 95 % of the result in
as the rates to hold the uplink to. Such a test is refused (`invalid`)
for an uplink that is not connected, and is not kept in
`speedtest.history`.

### VPN

Profiles are part of the configuration. Helpers:

| Method | Params | Result |
|--------|--------|--------|
| `vpn.parse` | `{conf}` | WireGuard section of a profile, from a `.conf` file |
| `vpn.genkey` | – | `{private_key,public_key}` |
| `vpn.pubkey` | `{private_key}` | `{public_key}` |

### From outside

The WireGuard server, forwarded ports and dynamic DNS are part of the
configuration (`inbound`). Helpers:

| Method | Params | Result |
|--------|--------|--------|
| `server.peer_add` | `{name,full}` | `{id,conf,host}` – adds a device to `inbound.server.peers` and returns its WireGuard configuration |
| `inbound.refresh` | – | `{}` – ask for the public address again and update the dynamic DNS name now |

`server.peer_add` makes a key pair for the device, gives it the lowest
free address in the server's range, stores the public key, and answers
with the configuration as the WireGuard apps import it: the private
key, the device's addresses, the router as DNS server, the server's
public key, `host` and the port as endpoint, and as AllowedIPs either
`0.0.0.0/0, ::/0` (`full`) or the server's range and those of the
router's networks. The private key is in this answer and nowhere else.
The call is refused (`invalid`) while the server is switched off, and
while the status has no `inbound.host`: the configuration would lead
nowhere.

`device.set` without `ip`, and `device.forget`, are refused for a device
that has a port forwarded to it.

### Captive portals

| Method | Params | Result |
|--------|--------|--------|
| `portal.set_device` | `{mac}` | `{}` – choose the device that signs in (`""` = automatic) |
| `portal.kick` | – | `{}` – make that device reconnect so its sign-in sheet appears |

### System

| Method | Params | Result |
|--------|--------|--------|
| `system.set_time` | `{unix_ms}` | `{adjusted}` |
| `system.logs` | – | `[string]` |
| `system.reboot` / `system.poweroff` | – | `{}` |
| `system.factory_reset` | – | `{}` – erases settings, pairings and the router's key |
| `updates.check` | – | `{}` – look for newer block lists and a newer country database now |

The app sends `system.set_time` at the start of every session: the Pi has
no battery-backed clock. The clock is set if it differs by more than two
seconds (five minutes once a time server has set it), and counts as
confirmed from then on (`clock.trusted`).

### Software updates

| Method | Params | Result |
|--------|--------|--------|
| `firmware.check` | – | `{current,supported,latest?,newer,download?,built?}` – asks the release server now |
| `firmware.install` | – | `{}` – starts installing the newest release |
| `firmware.status` | – | `{state,phase?,percent,version?,error?}` |

`supported` is false on a card without the two-slot layout. An
installation runs in the background and reports as `firmware` events;
it ends with the router restarting, which drops the session. `state` is
`idle`, `installing` or `failed`; `phase` is `download`, `verify` or
`restart`.

### Events

| Event | Data |
|-------|------|
| `status` | Status, whenever something other than a counter changed |
| `event` | `{t,kind,msg,mac?}` – a new activity log entry; kind `group` when a schedule takes a group offline or a group comes back by itself |
| `speedtest` | `{phase: "latency"\|"download"\|"upload"\|"done", mbps}` – progress of a running test |
| `firmware` | the result of `firmware.status`, whenever it changes |
