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
  not isolated, and from nowhere else: not from an uplink, a tunnel or an
  isolated network. The listener checks the same again for each
  connection (to one of the router's addresses in such a network, from an
  address inside one), because it exists before the first ruleset does.
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
the status changes, and with it what the app remembers.

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
`system.bluetooth_only`; `vlan`, `pppoe` and `tagged` for wired ports;
and `vlan` and `discovery` for networks, to format 2 and changed nothing
else; a router that stored format 2 carries it over as it is.) Its
parts, briefly:

```
system      {name country ssh{enabled authorized_keys[]} bluetooth_only}
            bluetooth_only: the router does not listen on TCP
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
ethernet[]  {name role ip vlan pppoe network tagged[]}
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
wan         {networks[] wifi_mac eth_mac order[] probe}
            order: uplink keys, best first: "eth:<port>", "wifi", "phone"
policy      {default rules[{id name enabled src{devices cidrs networks} dst action}]}
```

Status (`daemon/internal/core/api.go`):

```
name id version time uptime temp ready rev pairing
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
```

### Radios and upstream Wi-Fi

| Method | Params | Result |
|--------|--------|--------|
| `radios.list` | – | `[{id,phy,iface,driver,bus,product,mac,bands[],ap,sta,ap_sta,max_ap,max_ap_sta,present,role}]` |
| `wifi.uplink` | `{radio}` | `{}` – makes that radio the one that joins upstream networks (`""`: none) |
| `wifi.scan` | `{quick?}` | `[{ssid,signal,freq,security,known}]`; takes several seconds, or with `quick` answers at once from the last scan and starts a new one |
| `wifi.connect` | `{ssid,psk,hidden}` | `{}` – saves the network with top priority |
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
| `devices.list` | – | `[{mac,hostname,ipv4,ipv6[],first_seen,last_seen,online,online_since,link,signal,up,down,name,route,paused,ip,portal}]` |
| `device.set` | `{mac,name,route,paused,ip,no_adblock}` | `{}` – replaces the device's settings |
| `device.forget` | `{mac}` | `{}` |
| `device.queries` | `{mac}` | `[{t,mac,name,type,path,failed}]` recent DNS queries |

### Traffic

| Method | Params | Result |
|--------|--------|--------|
| `stats.get` | `{mac}` (`""` = whole network) | `{minutes: [60 × {t,up,down}], hours: [72 × {t,up,down}]}`, oldest first, evenly spaced |
| `stats.top` | `{hours}` | `[{mac,up,down}]`, busiest first |
| `flows.list` | `{mac}` (`""` = all devices) | `[{mac,proto,dst,port,name?,country?,path,up,down,started,last,active}]` – active first (busiest first), then recently ended (latest first) |
| `speedtest.run` | `{via: "direct"\|"vpn:<id>"}` | `{t,via,latency_ms,down_mbps,up_mbps,error?}` after about 20 s |
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

### VPN

Profiles are part of the configuration. Helpers:

| Method | Params | Result |
|--------|--------|--------|
| `vpn.parse` | `{conf}` | WireGuard section of a profile, from a `.conf` file |
| `vpn.genkey` | – | `{private_key,public_key}` |
| `vpn.pubkey` | `{private_key}` | `{public_key}` |

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
no battery-backed clock.

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
| `event` | `{t,kind,msg,mac?}` – a new activity log entry |
| `speedtest` | `{phase: "latency"\|"download"\|"upload"\|"done", mbps}` – progress of a running test |
| `firmware` | the result of `firmware.status`, whenever it changes |
