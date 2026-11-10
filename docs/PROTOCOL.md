# Porter control protocol

The app controls the router over Bluetooth Low Energy only. This document
is the contract between `daemon/internal/proto` + `daemon/internal/ble`
(Go) and `ios/PorterKit` (Swift). The shared test vectors in
`ios/PorterKit/Tests/PorterKitTests/Resources/vectors.json` are generated
by the Go implementation and checked by both.

Layers, bottom up:

| Layer    | Unit     | Purpose                                             |
|----------|----------|-----------------------------------------------------|
| GATT     | fragment | one ATT write or notification                       |
| Link     | packet   | fragments reassembled; ≤ 65535 bytes                |
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
- The **router** looks the app's key up in its pairing list. Unknown apps
  get a session in which only `hello` and `pair` work.

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
phone; it closes again as soon as one phone has paired.

| Method | Params | Result |
|--------|--------|--------|
| `hello` | – | `{name,id,version,paired,claimed,pairing,ready}` |
| `pair` | `{name}` | `{}` or `pairing_closed` |
| `pairing.open` | – | `{}` |
| `pairing.list` | – | `[{key,name,added,last_seen,self}]` |
| `pairing.revoke` | `{key}` | `{}` |

### Status and configuration

| Method | Params | Result |
|--------|--------|--------|
| `status.get` | – | Status |
| `config.get` | – | Config |
| `config.set` | `{config}` | `{rev}`; `conflict` if `config.rev` is stale |
| `events.list` | – | `[{t,kind,msg,mac?}]` |

The configuration is one document (`daemon/internal/config/config.go`
defines every field). The app edits a copy and writes the whole document
back with the `rev` it read; the router validates all of it, rejects
stale writes with `conflict`, and applies the change atomically.

Status (`daemon/internal/core/api.go`):

```
name id version time uptime temp ready rev pairing
internet   "down" | "connecting" | "limited" | "captive" | "online"
active     interface carrying the default route
wan[]      {if kind metric carrier internet ipv4 gateway ipv6[] has_v6 dns[] portal_url captive_api mac}
portal     {captive url device}
aps[]      {radio iface state band channel mode error}
stas[]     {radio iface state ssid bssid freq signal failed fail_reason}
lan        {address ula clients}
vpns[]     {id name type if state error endpoint handshake rx tx paths? of?}
           type "wireguard" | "mpquic"; paths of an MPQUIC tunnel working, of how many
geoip      {available date}   country database for country rules and flows
adblock    {enabled domains blocked lists[{url domains updated error}]}
```

### Radios and upstream Wi-Fi

| Method | Params | Result |
|--------|--------|--------|
| `radios.list` | – | `[{id,phy,iface,driver,bus,product,mac,bands[],ap,sta,ap_sta,present,role}]` |
| `wifi.scan` | – | `[{ssid,signal,freq,security,known}]` (takes several seconds) |
| `wifi.connect` | `{ssid,psk,hidden}` | `{}` – saves the network with top priority |
| `wifi.forget` | `{ssid}` | `{}` |
| `wan.new_mac` | `{kind: "wifi"\|"eth"}` | `{}` – new random upstream MAC |
| `wan.recheck` | – | `{}` – probe reachability now |

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
