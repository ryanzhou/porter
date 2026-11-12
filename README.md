# Porter

A travel router you build from a Raspberry Pi 5 and control from your
iPhone.

Plug it in at a hotel, café or Airbnb and it gives all your devices one
private Wi-Fi network of their own: it joins the venue's network (or a
wired port) for you, gets through the sign-in page once for everything,
and can send some or all of your traffic through your VPN.

<p>
<img src="docs/screenshots/overview.png" width="200" alt="Overview">
<img src="docs/screenshots/traffic.png" width="200" alt="Traffic">
<img src="docs/screenshots/flows.png" width="200" alt="Connections">
<img src="docs/screenshots/vpn.png" width="200" alt="VPN and rules">
</p>

- **Your own network anywhere.** The internet comes from a wired port,
  the venue's Wi-Fi or a phone on a USB cable, in the order you rank
  them; if the first stops working the router moves on to the next.
- **At home too.** On a fibre or DSL line it signs in to your provider
  with PPPoE, takes the internet under the VLAN ID the provider uses,
  and passes on the IPv6 addresses the provider gives out.
- **More than one network.** A second Wi-Fi name for guests or for work,
  on addresses of its own, kept apart from your devices if you like.
- **Controlled from your phone, directly.** The app talks to the router
  over Bluetooth, or over your own network when the phone is on it, so
  it works whether or not anything has internet yet. No account, no
  cloud service: the router and the app are the only two parts.
- **Sign-in pages handled once.** One device signs in to the venue's
  network; every device behind the router is then online.
- **VPN with rules.** WireGuard or MPQUIC, with a kill switch. Send
  everything through it, or only certain devices, sites, countries or
  ports.
- **See and manage what is connected.** Per-device traffic, live
  connections, pause a device, block ads for the whole network, run a
  speed test.
- **IPv6** works too, wherever the network the router joins offers it.

## What you need

- A **Raspberry Pi 5** with its power supply (any RAM size)
- A **microSD card**, 8 GB or larger
- An **iPhone** on iOS 17 or later
- A **Mac with Xcode 16** or later, to put the app on your phone
- Optional: an Ethernet cable to get online by wire, a USB Wi-Fi adapter
  for a second radio, your phone's USB cable to share its data, a
  micro-HDMI cable and keyboard for troubleshooting

## 1. Flash the router

1. Download `porter-<version>.img.xz` from the
   [latest release](https://github.com/ryanzhou/porter/releases/latest).
   (Or [build it yourself](docs/DEVELOPMENT.md).)
2. Open [Raspberry Pi Imager](https://www.raspberrypi.com/software/).
   Choose **Raspberry Pi 5**, then **Use custom** and select the
   `.img.xz` file, then your SD card. When asked about OS customisation,
   choose **No**.
3. Put the card in the Pi and plug in power.

After about a minute the green light blinks slowly: the router is waiting
to be set up.

## 2. Install the app on your iPhone

The app is installed from source with Xcode. A free Apple ID is enough;
you do not need a paid developer account.

1. Download this repository to your Mac and open Xcode.
2. In **Xcode → Settings → Accounts**, add your Apple ID.
3. In the `ios` folder, copy `Local.xcconfig.example` to
   `Local.xcconfig` and fill in the two values it describes: your Team ID
   and a bundle identifier of your own, such as `com.yourname.porter`.
   (Xcode shows your Team ID next to your account; the file itself
   explains where else to find it.)
4. Open `ios/Porter.xcodeproj`. Connect your iPhone with a cable, choose
   it at the top of the Xcode window, and press **Run** (▶).
5. The first time, your iPhone asks you to turn on **Developer Mode**
   (Settings → Privacy & Security) and to trust your certificate
   (Settings → General → VPN & Device Management). Do both, then press
   Run again.

With a free Apple ID the app stops opening after 7 days. Connect the
phone and press Run again to renew it; nothing is lost.

Want to look around first? In Xcode choose **Product → Scheme → Edit
Scheme → Run → Arguments**, add `-demo`, and run in the Simulator. The
app then shows sample data and needs neither a router nor an Apple ID.

## 3. Set up the router

1. Stand near the Pi and open the app. Allow Bluetooth when asked.
2. Tap **Porter** in the list. A new router pairs with the first phone
   that connects.
3. Choose a **Wi-Fi name**, a **password** and your **country** (it
   decides which Wi-Fi channels are legal), then tap **Start**.
4. Join that Wi-Fi network from your devices. The QR button on
   **Overview** shows a code they can scan instead of typing.

Out of the box the built-in radio broadcasts your Wi-Fi and the Ethernet
port is how the router gets online. If you have a cable, plug it in and you are online.
If not, read on.

## Getting online

Everything here is on the **Internet** tab. At the top are the ways out,
in the order the router tries them.

| You have | Do this |
|---|---|
| A wired port | Plug in Ethernet. Nothing else to do. |
| Wi-Fi only | Tap **Join a Wi-Fi network** and pick the network. With the Pi alone, the built-in radio does both jobs, and your devices drop off for a moment whenever it joins a different network. With a USB Wi-Fi adapter plugged in, the adapter joins and the built-in radio keeps broadcasting your Wi-Fi, which is faster and steadier. |
| Your phone's data | Plug the phone into one of the Pi's USB ports and turn on Personal Hotspot (iPhone) or USB tethering (Android). An iPhone asks once whether to trust the router: tap **Trust**. Or join the phone's hotspot as a Wi-Fi network. |

Networks you have joined are remembered and rejoined automatically. If
the venue's network uses the same address range as one of yours, the
router moves yours out of the way by itself.

**Order.** The router gets online through the first internet source in
the list that works, and moves to the next when one stops reaching the
internet (a sign-in
page does not count as not working: it waits for you). Tap **Edit** and
drag to change the order, for instance to put your phone last so that
its data is only used when nothing else works.

**Wi-Fi source** has the rest: which radio joins when there are two, the
hardware address the router shows to the network it joins, and **Stop
using Wi-Fi**.

Addresses come from the network by default. Where a network wants a
fixed address instead, set it by hand: for a wired port by tapping
**Ethernet** in the list, for a Wi-Fi network by tapping it under
**Saved Wi-Fi networks**. IPv4 and IPv6 are set separately, each with address, gateway
and DNS servers.

### On your provider's line at home

Plugged into a fibre or DSL modem, the wired port may need what your
provider's welcome letter lists. Tap **Ethernet** in the list of
internet sources:

- **PPPoE.** Under IPv4 choose **PPPoE** and enter the PPPoE user name
  and password. The source shows **Signing in to your provider** until
  it is online, and signs in again by itself whenever the provider ends
  the connection.
- **VLAN ID.** If the letter gives one (7, 10 and 35 are common), enter
  it. Leave it empty otherwise.
- **IPv6** needs nothing. Where the provider gives the router IPv6
  addresses for your devices (a "prefix"), each of your networks gets a
  share of them and the page shows the prefix. Your devices then have
  IPv6 addresses of their own on the internet; the router still lets
  nothing in to them that they did not ask for.

Where a network is on both bands the router joins it on 5 GHz, unless
that signal is poor; the band shows next to the network. With one radio
doing both jobs, your Wi-Fi uses the same channel as the network it
joins, and 5 GHz is only used on channels where an access point is allowed
(venues often use the radar channels, which are not). A second adapter
has no such limit.

### Sign-in pages (captive portals)

When the venue wants you to accept terms or enter a room number,
**Overview** shows **Sign-in required**. One of your devices does the
signing in, for everyone:

- On the device named there, connected to *your* Wi-Fi, tap **Open
  sign-in page** (or just open any website). Complete the page.
- If nothing appears, tap **Reconnect device**: that device rejoins your
  Wi-Fi and its own sign-in sheet pops up.
- To use a different device, pick it from the menu on the same card.

Once the venue lets the router through, every device is online and the
section disappears. Until then only the sign-in device can reach the
venue's network; devices you have put behind a VPN kill switch stay
protected.

## Your networks

**Settings → Wi-Fi & networks** is where your own side is set up: the
Wi-Fi names, the networks behind them, which radio broadcasts, and what
each wired port is for. These depend on each other, so nothing there
changes the router until you tap **Apply**. Before you do, the page says
what the changes will do (devices having to reconnect, for instance), or
why they cannot be applied; **Discard** puts everything back.

- **A guest network.** Tap **Add Wi-Fi network**, give it a name and a
  password, and leave it on **A new network**, **Keep apart from your other
  networks**. Guests get
  online, on addresses of their own, and cannot reach your devices.
- **A network** is a set of addresses with its own rules. A Wi-Fi name or
  a wired port belongs to one; a network can have a **Route** (everything
  on it through a VPN, say), and rules on the VPN tab can apply to a
  whole network.
- **Radios.** Each radio either broadcasts your Wi-Fi or does not. The
  Pi's built-in radio broadcasts one Wi-Fi name at a time; a second name
  needs a USB Wi-Fi adapter that can broadcast several, or a second
  radio. The page tells you when a name is not broadcast.
- **Wired ports** can each be an **Internet source**, be for devices on
  one of your networks, or not be used.

## Using a VPN

On the **VPN** tab tap **Add VPN** and paste the WireGuard configuration
from your VPN provider or your own server (or import the `.conf` file).

- **Everything through the VPN:** set **Everything else**, the last row
  of the rules, to that VPN.
- **One device:** open it on the **Devices** tab and set its **Route**.
- **One network:** set its **Route** under Settings → Wi-Fi & networks.
- **Some destinations:** tap **Add rule** and choose sources (devices or
  whole networks) and destinations (domains, address ranges, countries,
  ports), then where that traffic goes: a VPN, **Direct**, or
  **Blocked**.

Rules are tried from the top and traffic follows the first one it fits;
after the rules comes the device's own route, then its network's, then
**Everything else**. Tap **Edit**
to reorder, swipe a rule right to turn it off.

**Kill switch** (per VPN, on by default): while the VPN is disconnected,
devices that use it are offline rather than online without it.

**MPQUIC.** Instead of WireGuard, a profile can be a tunnel to your own
[mpquic](https://github.com/ryanzhou/mpquic) server, which spreads
traffic over several paths at once. Choose **MPQUIC** when adding a VPN
and enter the server, any relays (one path each) and the password of
your `mpquic remote`, which has to run with `-u`. Everything above works
the same for it; the profile shows how many of its paths are up.

DNS lookups follow the traffic: a device routed through a VPN uses that
VPN's DNS server. If the profile names none, the DNS servers you set
under Settings are asked through the tunnel, or public ones (1.1.1.1,
9.9.9.9) if you set none; the network you are connected to is not asked
(unless the tunnel only carries a few networks rather than everything).
While a VPN is disconnected, lookups go where the traffic goes: nowhere with
the kill switch on, directly without it.

## Everyday things

- **Devices.** See what is connected, rename it, **pause** its internet,
  give it a fixed address, see what it has been looking up.
- **Ad blocking.** Settings → Ad blocking. Blocks ad and tracker domains
  for every device. If a site breaks, add it under **Always allow**, or
  turn on **Skip ad blocking** for one device.
- **Traffic.** Overview → Traffic shows how much each device has used;
  **Connections** shows what each is talking to right now, in which
  country, and whether it went through the VPN.
- **Speed test.** Overview → Speed test measures the router's own
  connection, directly or through a VPN. (It measures the venue's
  internet, not the Wi-Fi between you and the router.)
- **Another phone.** Briefly press the Pi's power button; the light
  blinks fast for two minutes while a new phone can pair. Or use
  Settings → Paired phones. Remove a phone there too.
- **Turning it off.** Hold the power button for a second and release, or
  use Settings → Power off. Simply pulling the plug is safe as well: the
  system is read-only and cannot be corrupted that way.

### From anywhere on your network

The first time, the app finds the router over Bluetooth, and that is how
a phone pairs with it. From then on the app also reaches the router
through your own network whenever the phone is on your Wi-Fi (or on a
wired port of the router), which is a good deal faster and works from
the other end of the house, or with the router in a cupboard.

There is nothing to set up. The app uses whichever of the two works and
moves between them by itself as you come and go; the first time it tries
your network, iOS asks whether Porter may find devices on your local
network. Settings shows under **Connection** which one is in use.

- Only phones that are already paired are let in this way, and only from
  your own networks: never from the internet side, and not from a
  network you keep apart from the others (a guest network).
- Bluetooth stays the way back in. If the phone is not on your Wi-Fi, or
  you said no to the local network question, the app carries on over
  Bluetooth as before. Pairing a new phone always happens over
  Bluetooth, next to the router.
- To have the router answer over Bluetooth only, switch off
  Settings → **Connect over your network too**.

### The light and the button

| Green light | Meaning |
|---|---|
| Slow blink | Waiting for first-time setup |
| Fast blink | A new phone can pair (2 minutes) |
| Steady | Running normally |

| Power button | Does |
|---|---|
| Short press | Lets a new phone pair |
| Hold 1 second, release | Shuts down |
| Press while off | Starts up |

## Troubleshooting

**The router does not appear in the app.** Give it a full minute after
power-on and stay within a few metres. Check that Bluetooth is on and
that the app is allowed to use it (Settings → Porter). If the light is
not blinking at all, the Pi did not boot: re-flash the card. A router
you have already paired with is also found without Bluetooth, as long
as the phone is on its Wi-Fi.

**"Press the power button".** The router already belongs to another
phone. Press the button briefly, then try again.

**The app stopped opening.** With a free Apple ID that happens after 7
days. Connect the phone to your Mac and press Run in Xcode.

**No Wi-Fi network appears after setup.** Open **Overview** and look at
the Wi-Fi card for an error, and check the country is right.

**A second Wi-Fi name says "Not broadcast".** The radio broadcasting
your Wi-Fi can broadcast one name at a time (the Pi's built-in radio
is like that). The second name appears once there is a radio that can
broadcast it: see **Your networks**.

**The phone on USB stays "Turn on the phone's hotspot".** Check that
Personal Hotspot (or USB tethering) is on, and on an iPhone that you
tapped **Trust** when it asked; unplugging and plugging it in again makes
it ask again. Some cables only charge.

**Ethernet says "Wrong user name or password".** Your provider turned
the PPPoE sign-in down. Tap **Ethernet** and type both again exactly as
the provider gave them; a user name often has an `@` part that belongs
to it.

**Ethernet says "No answer from the provider".** Nothing at the other
end of the cable answered the sign-in. Check that the cable goes to the
modem's port for a router, that the modem itself is online, and whether
your provider wants a VLAN ID.

**Online, but a device has no internet.** Check it is not paused, and
whether it is routed through a VPN that is down (kill switch).

**Seeing what the router is doing.** Put the SD card in your computer
and create an empty file named `porter-debug` on the `BOOTFS` volume.
Boot the Pi with a display (micro-HDMI) and keyboard attached: you get a
command line as root. Useful commands:

```bash
porterctl status
```

```bash
journalctl -u porterd -b --no-pager | tail -60
```

```bash
bluetoothctl show
```

The first shows what the router thinks is happening, the second its log,
the third whether Bluetooth is up. If Bluetooth is the problem you can
still do first-time setup from there:

```bash
porterctl config | sed -e 's/"country": ""/"country": "AU"/' -e 's/"ssid": ""/"ssid": "MyNetwork"/' -e 's/"passphrase": ""/"passphrase": "choose-a-password"/' > /tmp/c.json
```

```bash
porterctl call config.set "{\"config\": $(cat /tmp/c.json)}"
```

Delete the `porter-debug` file when you are done.

**Starting over.** Settings → Reset erases all
settings and pairings. Re-flashing the card does the same.

## Good to know

- **Updating.** Settings → Software shows the router's version and
  whether a newer [release](https://github.com/ryanzhou/porter/releases)
  exists; **Install** downloads it, restarts into it and keeps your
  settings. The card holds the system twice: the update goes into the
  spare copy, and if the new version does not come up properly the
  router goes back to the one before. The router only looks for updates
  when you open that page. The app and the router's software go
  together: after updating one, update the other from the same version.
  An app that is ahead of the router still connects, and offers the
  router's update. (A card flashed before v0.1.0 has to be flashed once
  more.)
- **What the router contacts by itself:** a time server; one small page
  on `captive.apple.com` to detect sign-in pages (the same check an
  iPhone makes); your chosen ad block lists, daily, when ad blocking is
  on; and a country database from db-ip.com, monthly. A speed test
  contacts its test server only when you run one, and the router asks
  github.com for a newer version only when you open Settings → Software. Nothing reports to
  the authors of this project; there is nowhere for it to report to.
- **Security.** The app's connection to the router is encrypted and
  authenticated independently of Bluetooth pairing and of your Wi-Fi
  password, the same way over Bluetooth and over your network, and a
  phone can only pair while the router is new or its button has just
  been pressed, and only over Bluetooth.
  There are no passwords to log in to the router itself; SSH is off
  unless you enable it in Settings, and then only from your own network
  with your key.
- **Ad blocking** works by refusing to look up ad domains. It cannot
  remove ads served from the same place as the content, and a device
  using its own encrypted DNS bypasses it.
- **Country rules** use a public database that is approximate. Global
  services such as public DNS resolvers appear under a single country.
- **IPv6.** Where the internet source gives the router IPv6 addresses
  for your devices (home lines usually do), devices get addresses from
  them and use IPv6 as they would behind any home router. Nothing can
  connect in to them from outside. If there are fewer of those addresses
  than you have networks, your main network has them first. Everywhere
  else (a hotel's Wi-Fi, a phone) devices get private IPv6 addresses and
  the router translates, so for sites that have both, most devices will
  prefer IPv4; IPv6-only sites work. Translation is also used for what
  leaves through a VPN, or through another internet source than the one
  the addresses came from.
- **USB Wi-Fi adapters** that draw a lot of power may need a 5 A power
  supply, or `usb_max_current_enable=1` in `config.txt` on the card.
- **A phone on USB charges from the Pi** while it shares its data. With
  a USB Wi-Fi adapter plugged in as well, a 5 A power supply is the safe
  choice.
- **Many devices on the built-in radio.** The Pi's Wi-Fi chip supports
  only a handful of simultaneous clients with its default firmware.
  [The developer notes](docs/DEVELOPMENT.md#built-in-radio-client-limit)
  describe the alternative firmware.
- **MPQUIC versions.** The image carries one mpquic release, and the
  two ends of a tunnel have to speak the same protocol version. To use a
  newer mpquic without waiting for a new image, enable SSH and copy the
  `mpquic-linux-arm64` binary to `/data/porter/bin/mpquic` on the router.
  It has to be v0.6.0 or newer: the router uses mpquic's network
  interface mode, which older releases do not have.
- **Not included:** OpenVPN, a VPN server, enterprise (username and
  password) Wi-Fi as an internet source, intrusion detection, load
  balancing across several internet sources.

## For developers

How it is built, how to build the image yourself, the tests, and the
control protocol are in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) and
[docs/PROTOCOL.md](docs/PROTOCOL.md).

## Licence

[Apache 2.0](LICENSE). [NOTICE](NOTICE) lists the third-party software
and data Porter uses; the software inside a built SD card image keeps its
own licences.

IP geolocation by [DB-IP](https://db-ip.com).
