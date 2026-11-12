// Package config defines the persistent router configuration: its schema,
// defaults, validation and on-disk storage.
package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

const SchemaVersion = 3

// Config is the whole user-controllable state of the router. It is sent to
// the app as JSON and written back section by section.
type Config struct {
	Version  int       `json:"version"`
	Rev      uint64    `json:"rev"`
	System   System    `json:"system"`
	Radios   []Radio   `json:"radios"`
	Ethernet []EthPort `json:"ethernet"`
	// Wifi lists the Wi-Fi networks the router broadcasts. The first is
	// the main one and always exists.
	Wifi []SSID `json:"wifi"`
	WAN  WAN    `json:"wan"`
	// LAN holds what all of the router's own networks have in common;
	// Networks lists them. The first is the main one and always exists.
	LAN      LAN       `json:"lan"`
	Networks []Network `json:"networks"`
	DNS      DNS       `json:"dns"`
	VPNs     []VPN     `json:"vpns"`
	Policy   Policy    `json:"policy"`
	Devices  []Device  `json:"devices"`
	// Groups are sets of devices that share settings and schedules.
	Groups    []Group   `json:"groups"`
	SpeedTest SpeedTest `json:"speedtest"`
	// Inbound is what may reach the router and its networks from outside.
	Inbound Inbound `json:"inbound"`
}

// SpeedTest names the server a speed test exchanges data with. It is
// contacted only when a test is run from the app.
type SpeedTest struct {
	// DownloadURL must serve a large body; it is fetched repeatedly.
	DownloadURL string `json:"download_url"`
	// UploadURL must accept POSTed data of any size.
	UploadURL string `json:"upload_url"`
}

// DefaultSpeedTest uses Cloudflare's public speed test endpoints.
func DefaultSpeedTest() SpeedTest {
	return SpeedTest{
		DownloadURL: "https://speed.cloudflare.com/__down?bytes=50000000",
		UploadURL:   "https://speed.cloudflare.com/__up",
	}
}

type System struct {
	// Name is the router's display name; also used as the BLE local name.
	Name string `json:"name"`
	// Country is the ISO 3166-1 alpha-2 regulatory domain for all radios.
	// Radios stay off until it is set.
	Country string `json:"country"`
	// SSH enables an OpenSSH server reachable from the LAN only.
	SSH SSH `json:"ssh"`
	// BluetoothOnly switches off control over the router's own networks
	// (see internal/netctl), leaving Bluetooth as the only way in.
	BluetoothOnly bool `json:"bluetooth_only"`
	// Timezone is the IANA name of the time zone the household lives in,
	// such as "Europe/Berlin": schedules are kept in its time, daylight
	// saving included. Empty until a phone has said where it is.
	Timezone string `json:"timezone"`
}

type SSH struct {
	Enabled        bool     `json:"enabled"`
	AuthorizedKeys []string `json:"authorized_keys"`
}

// Radio roles.
const (
	RoleOff   = "off"
	RoleAP    = "ap"     // access point for LAN clients
	RoleWAN   = "wan"    // station connected to an upstream network
	RoleAPWAN = "ap+wan" // both on one radio (single channel, driver permitting)
)

// Radio assigns a role to one Wi-Fi radio. ID is "builtin" for the on-board
// radio or "usb:<permanent mac>" for an external adapter.
type Radio struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	// Band is "2.4" or "5" for the AP. Ignored for RoleAPWAN, where the
	// AP has to follow the upstream network's channel.
	Band string `json:"band"`
	// Channel is the AP channel number; 0 picks automatically.
	Channel int `json:"channel"`
	// Width is the maximum AP channel width in MHz (20, 40, 80); 0 = widest
	// that the radio and channel support.
	Width int `json:"width"`
}

// Ethernet port roles.
const (
	EthWAN = "wan"
	EthLAN = "lan"
	EthOff = "off"
)

// EthPort assigns a role to a wired (or USB-tethered) interface.
type EthPort struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// IP is how the port gets its addresses when it is an uplink.
	IP IPSetup `json:"ip"`
	// VLAN is the tag the provider delivers the internet under, 1 to
	// 4094, for a port that is an uplink; 0 for none. The uplink then
	// runs on a sub-interface of the port (VLANIfName).
	VLAN int `json:"vlan"`
	// PPPoE makes the uplink sign in to the provider, in place of DHCP
	// or a fixed address for IPv4.
	PPPoE PPPoE `json:"pppoe"`
	// Network is the network a LAN port carries untagged: what arrives
	// there without a VLAN tag belongs to it. Empty for a port that
	// carries tagged networks only.
	Network string `json:"network"`
	// Tagged are the networks a LAN port carries under their VLAN tags
	// (Network.VLAN), next to the untagged one. Each has a sub-interface
	// of the port (VLANIfName) in its bridge.
	Tagged []string `json:"tagged"`
	// Shaping holds the port, as an uplink, a little below the speed of
	// the line behind it.
	Shaping Shaping `json:"shaping"`
	// Data is the amount of data the line behind the port, as an uplink,
	// may carry in a day or a month.
	Data DataLimit `json:"data"`
}

// DataLimit says that the line behind an uplink has only so much data to
// spend in a day or a month. The router counts what the uplink carries
// (see internal/usage) and stops using it for the rest of the period once
// that is the amount. Like Shaping it belongs to the line: to a wired
// port, to a saved Wi-Fi network, to the phone on USB.
type DataLimit struct {
	Enabled bool `json:"enabled"`
	// Period is PeriodDay or PeriodMonth.
	Period string `json:"period"`
	// MB is the amount for one period, in megabytes of a million bytes,
	// which is how providers count. It is needed while the limit is
	// enabled, and kept while it is not.
	MB int `json:"mb"`
	// StartDay is the day of the month a monthly period begins on, 1 to
	// 31; in a month that is shorter, its last day.
	StartDay int `json:"start_day"`
	// LiftUntil lets the uplink be used although the amount is spent,
	// until that moment (unix time): the app sets it to the end of the
	// period. 0, or a moment that has passed, for not at all.
	LiftUntil int64 `json:"lift_until"`
}

const (
	PeriodDay   = "day"
	PeriodMonth = "month"
	// MaxDataMB is the largest amount a DataLimit takes: ten terabytes.
	MaxDataMB = 10_000_000
)

// Bytes is the amount for one period.
func (l DataLimit) Bytes() uint64 { return uint64(l.MB) * 1_000_000 }

// Shaping holds an uplink to rates a little below what the line behind it
// really carries, so that what has to wait waits in the router, where it
// is sorted fairly between devices, and not in the modem (see
// internal/shape). It belongs to the line: to a wired port, to a saved
// Wi-Fi network, to the phone on USB.
type Shaping struct {
	Enabled bool `json:"enabled"`
	// DownKbps and UpKbps are the rates in kbit/s. Both are needed while
	// it is enabled, and kept while it is not.
	DownKbps int `json:"down_kbps"`
	UpKbps   int `json:"up_kbps"`
}

// The rates Shaping takes: below the lower one nothing is usable, and the
// upper one is more than any port of the router carries.
const (
	MinShapeKbps = 100
	MaxShapeKbps = 10_000_000
)

// PPPoE is a provider's sign-in for a wired uplink. Username and Password
// are used, and kept, only while it is Enabled.
type PPPoE struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// VLANIfName is the interface that carries one VLAN of a wired port, and
// PPPIfName the interface of a PPPoE session over a port (tagged or not:
// a port has one session at most). Both are named after the port so that
// they are the same after every start, with a prefix no port's own name
// begins with: see MadeIf.
func VLANIfName(port string, id int) string { return madeName(fmt.Sprintf("v%d-", id), port) }
func PPPIfName(port string) string          { return madeName("ppp-", port) }

// madeName is prefix and port where that fits in an interface name (15
// bytes). A port with a longer name, such as a USB adapter named after
// its hardware address, is named by a hash instead: not readable, but
// just as much its own.
func madeName(prefix, port string) string {
	if len(prefix)+len(port) <= 15 {
		return prefix + port
	}
	sum := sha256.Sum256([]byte(port))
	return (prefix + hex.EncodeToString(sum[:]))[:15]
}

// MadeIf reports whether a name is that of an interface the router makes
// on top of a wired port (VLANIfName, PPPIfName), which is never itself
// a wired port.
func MadeIf(name string) bool { return reMade.MatchString(name) }

// IPSetup is how an uplink gets its addresses, per address family. The
// zero value is automatic for both: DHCP for IPv4; router advertisements
// and DHCPv6 for IPv6.
type IPSetup struct {
	V4 IPFamily `json:"v4"`
	V6 IPFamily `json:"v6"`
}

const (
	IPAuto   = "auto"
	IPStatic = "static"
)

// IPFamily is one address family of an IPSetup. Address, Gateway and DNS
// are used only when Mode is IPStatic.
type IPFamily struct {
	Mode string `json:"mode"`
	// Address is the uplink's own address with its prefix length, such
	// as 192.0.2.10/24 or 2001:db8::10/64.
	Address string `json:"address"`
	// Gateway is the next hop for everything else. For IPv6 it may be a
	// link-local address.
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

// Static reports whether the family is configured by hand.
func (f IPFamily) Static() bool { return f.Mode == IPStatic }

const (
	SecWPA2     = "wpa2"
	SecWPA2WPA3 = "wpa2+wpa3"
	SecWPA3     = "wpa3"
)

// SSID is one Wi-Fi network the router broadcasts. Every radio that is an
// access point carries all of them, as far as it is able to.
type SSID struct {
	ID         string `json:"id"`
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	Security   string `json:"security"`
	Hidden     bool   `json:"hidden"`
	// Isolate stops wireless clients from talking to each other.
	Isolate bool `json:"isolate"`
	// Network is the network its clients join.
	Network string `json:"network"`
	Enabled bool   `json:"enabled"`
}

// Identifiers of the two things that always exist.
const (
	MainSSID    = "main"
	MainNetwork = "lan"
)

// Limits on how much can be configured.
const (
	MaxSSIDs    = 4
	MaxNetworks = 4
)

// Network is one of the router's own networks: a subnet with its own
// address range, reached through the Wi-Fi networks and wired ports
// assigned to it.
type Network struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Address is the router's address with prefix, e.g. 10.83.0.1/24.
	Address string `json:"address"`
	// ULAPrefix is the IPv6 /64 announced; traffic is NAT66'd upstream.
	ULAPrefix string `json:"ula_prefix"`
	// Isolated keeps the devices here from opening connections to the
	// router's other networks. They can still be reached from those.
	Isolated bool `json:"isolated"`
	// Route overrides Policy.Default for this network: "" or an action
	// ("direct", "vpn:<id>", "source:<key>", "only:<key>", "block"). A
	// device's own route wins over it.
	Route string `json:"route"`
	// VLAN is the tag the network has on the wired ports that carry it
	// tagged (EthPort.Tagged), 1 to 4094; 0 for none. No two networks
	// have the same.
	VLAN int `json:"vlan"`
	// Discovery lets the devices here be found from the router's other
	// networks: multicast DNS is relayed between this network and those
	// that are not isolated (see DiscoveryPeers).
	Discovery bool `json:"discovery"`
}

// Prefix returns the router's address on the network and its subnet.
func (n Network) Prefix() (netip.Addr, netip.Prefix) {
	p := netip.MustParsePrefix(n.Address)
	return p.Addr(), p.Masked()
}

// ULA returns the router's IPv6 address on the network (::1 in the /64)
// and the prefix.
func (n Network) ULA() (netip.Addr, netip.Prefix) {
	p := netip.MustParsePrefix(n.ULAPrefix).Masked()
	a := p.Addr().As16()
	a[15] = 1
	return netip.AddrFrom16(a), p
}

// Contains reports whether an address belongs to the network.
func (n Network) Contains(a netip.Addr) bool {
	_, v4 := n.Prefix()
	_, v6 := n.ULA()
	return v4.Contains(a) || v6.Contains(a)
}

const (
	MACHardware = "hardware"
	MACRandom   = "random" // stable random address, regenerated on request
	MACCustom   = "custom"
)

type MACPolicy struct {
	Mode string `json:"mode"`
	// Address is the custom address, or the current random one.
	Address string `json:"address,omitempty"`
}

type WifiNetwork struct {
	SSID string `json:"ssid"`
	// PSK is the passphrase; empty for an open network.
	PSK      string `json:"psk"`
	Hidden   bool   `json:"hidden"`
	Priority int    `json:"priority"`
	// IP is how the router gets its addresses on this network.
	IP IPSetup `json:"ip"`
	// Shaping holds the router's traffic through this network below the
	// speed of the line behind it: a hotel's and a home's differ, so the
	// rates belong to the network joined, not to the radio.
	Shaping Shaping `json:"shaping"`
	// Data is the amount of data this network may carry in a day or a
	// month: a phone's hotspot has one where the Wi-Fi at home has none.
	Data DataLimit `json:"data"`
}

type WAN struct {
	// Networks are the upstream Wi-Fi networks the router may join.
	Networks []WifiNetwork `json:"networks"`
	// WifiMAC and EthMAC control the address presented upstream.
	WifiMAC MACPolicy `json:"wifi_mac"`
	EthMAC  MACPolicy `json:"eth_mac"`
	// Order ranks the uplinks, best first. The entries are uplink keys:
	// UplinkEth of a wired port, UplinkWifi for an upstream Wi-Fi network,
	// UplinkPhone for a phone sharing its connection over USB. The router
	// uses the first one that works.
	Order []string `json:"order"`
	// Phone is what is set for a phone sharing its connection over USB.
	Phone Phone `json:"phone"`
	// Probe controls connectivity / captive portal detection.
	Probe Probe `json:"probe"`
}

// Phone holds the settings of the uplink that is a phone on USB. Nothing
// about any one phone is stored: they apply to whichever is plugged in.
type Phone struct {
	Shaping Shaping `json:"shaping"`
	// Data is the amount of data a phone may carry in a day or a month.
	Data DataLimit `json:"data"`
}

// Uplink keys, as used in WAN.Order.
const (
	UplinkWifi  = "wifi"
	UplinkPhone = "phone"
	uplinkEth   = "eth:"
)

// UplinkEth is the key of a wired port.
func UplinkEth(name string) string { return uplinkEth + name }

// EthOfUplink returns the port an uplink key names.
func EthOfUplink(key string) (string, bool) {
	if strings.HasPrefix(key, uplinkEth) {
		return key[len(uplinkEth):], true
	}
	return "", false
}

// ShapingOf returns what is set for the uplink with the given key
// (UplinkEth of a port, UplinkWifi, UplinkPhone). For Wi-Fi it is that of
// the saved network named ssid, the one the router has joined.
func (c *Config) ShapingOf(key, ssid string) Shaping {
	switch port, wired := EthOfUplink(key); {
	case wired:
		for _, e := range c.Ethernet {
			if e.Name == port {
				return e.Shaping
			}
		}
	case key == UplinkWifi:
		for _, n := range c.WAN.Networks {
			if n.SSID == ssid {
				return n.Shaping
			}
		}
	case key == UplinkPhone:
		return c.WAN.Phone.Shaping
	}
	return Shaping{}
}

// DataLimitOf returns the data limit of the uplink with the given key,
// for Wi-Fi that of the saved network named ssid, as ShapingOf does.
func (c *Config) DataLimitOf(key, ssid string) DataLimit {
	switch port, wired := EthOfUplink(key); {
	case wired:
		for _, e := range c.Ethernet {
			if e.Name == port {
				return e.Data
			}
		}
	case key == UplinkWifi:
		for _, n := range c.WAN.Networks {
			if n.SSID == ssid {
				return n.Data
			}
		}
	case key == UplinkPhone:
		return c.WAN.Phone.Data
	}
	return DataLimit{}
}

// UplinkKey reports whether key names an uplink this configuration can
// have: Wi-Fi, a phone, or one of its wired ports.
func (c *Config) UplinkKey(key string) bool {
	if key == UplinkWifi || key == UplinkPhone {
		return true
	}
	port, wired := EthOfUplink(key)
	for _, e := range c.Ethernet {
		if wired && e.Name == port {
			return true
		}
	}
	return false
}

type Probe struct {
	// URL must be plain HTTP and return a body containing Expect when the
	// internet is reachable.
	URL    string `json:"url"`
	Expect string `json:"expect"`
}

type LAN struct {
	// AutoRenumber moves a network to another subnet when it collides
	// with the upstream network.
	AutoRenumber bool   `json:"auto_renumber"`
	LeaseMinutes int    `json:"lease_minutes"`
	Domain       string `json:"domain"`
	IPv6         bool   `json:"ipv6"`
}

type DNS struct {
	// Upstreams used for traffic leaving directly via the WAN. Empty means
	// the servers handed out by the upstream network. Forms:
	//   1.1.1.1            plain DNS
	//   tls://1.1.1.1#cloudflare-dns.com
	//   https://cloudflare-dns.com/dns-query#1.1.1.1
	Upstreams []string `json:"upstreams"`
	// Intercept redirects all LAN port-53 traffic to the router so that
	// domain-based rules cannot be bypassed by a hard-coded resolver.
	Intercept bool `json:"intercept"`
	// Blocked domains (and their subdomains) resolve to nothing.
	Blocked []string `json:"blocked"`
	Adblock Adblock  `json:"adblock"`
}

// Adblock blocks advertising and tracking domains using block lists the
// router downloads and keeps up to date.
type Adblock struct {
	Enabled bool `json:"enabled"`
	// Lists are URLs of block lists in hosts, plain-domain or Adblock
	// format.
	Lists []string `json:"lists"`
	// Allow lists domains (with their subdomains) that are never blocked,
	// whatever the lists say.
	Allow []string `json:"allow"`
}

// DefaultBlockList is used until the user chooses otherwise.
const DefaultBlockList = "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"

type VPN struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// KillSwitch drops traffic assigned to this VPN while the tunnel is
	// down instead of letting it leave directly.
	KillSwitch bool `json:"kill_switch"`
	// Type says which of the two sections below describes the tunnel:
	// VPNWireGuard (also when empty) or VPNMPQUIC.
	Type      string    `json:"type"`
	WireGuard WireGuard `json:"wireguard"`
	MPQUIC    MPQUIC    `json:"mpquic"`
}

const (
	VPNWireGuard = "wireguard"
	VPNMPQUIC    = "mpquic"
)

// MPQUIC is a multipath QUIC tunnel to an "mpquic remote". The router
// runs "mpquic local" for it and reaches that over a WireGuard interface
// of its own, so that rules, the kill switch and DNS work as they do for
// any other VPN.
type MPQUIC struct {
	// Servers are the paths to the remote: its own address and any
	// relays, each host or host:port.
	Servers []string `json:"servers"`
	// Port is used for servers given without one.
	Port     int    `json:"port"`
	Password string `json:"password"`
	// DNS servers to use through the tunnel.
	DNS []string `json:"dns"`
	// Up and Down are the rates of the line the router is on, if known,
	// such as "19mbit". Empty lets mpquic measure.
	Up   string `json:"up"`
	Down string `json:"down"`
	// ReliableUDP carries UDP over streams instead of datagrams.
	ReliableUDP bool `json:"reliable_udp"`
}

// DefaultMPQUICPort is mpquic's (and Shadowsocks') usual port.
const DefaultMPQUICPort = 8388

type WireGuard struct {
	PrivateKey string   `json:"private_key"`
	Addresses  []string `json:"addresses"`
	DNS        []string `json:"dns"`
	MTU        int      `json:"mtu"`
	Peers      []WGPeer `json:"peers"`
}

type WGPeer struct {
	PublicKey    string   `json:"public_key"`
	PresharedKey string   `json:"preshared_key,omitempty"`
	Endpoint     string   `json:"endpoint"`
	AllowedIPs   []string `json:"allowed_ips"`
	Keepalive    int      `json:"keepalive"`
}

// Actions. A VPN action is "vpn:<id>". An action that names one uplink
// is "source:<key>" or "only:<key>", with the uplink's key as in
// WAN.Order: the first goes by whichever uplink is in use while the named
// one is not available, the second is offline then.
const (
	ActDirect    = "direct"
	ActBlock     = "block"
	actVPNPfx    = "vpn:"
	actSourcePfx = "source:"
	actOnlyPfx   = "only:"
)

func ActVPN(id string) string { return actVPNPfx + id }

// ActSource is the action that prefers the uplink with the given key, and
// ActOnly the one that uses nothing else.
func ActSource(key string) string { return actSourcePfx + key }
func ActOnly(key string) string   { return actOnlyPfx + key }

// SourceOf returns the uplink key of a "source:<key>" or "only:<key>"
// action, and whether it is the second kind.
func SourceOf(action string) (key string, only, ok bool) {
	if strings.HasPrefix(action, actSourcePfx) {
		return action[len(actSourcePfx):], false, true
	}
	if strings.HasPrefix(action, actOnlyPfx) {
		return action[len(actOnlyPfx):], true, true
	}
	return "", false, false
}

// VPNOf returns the VPN id of a "vpn:<id>" action.
func VPNOf(action string) (string, bool) {
	if strings.HasPrefix(action, actVPNPfx) {
		return action[len(actVPNPfx):], true
	}
	return "", false
}

type Policy struct {
	// Default applies to traffic no rule or device setting claims.
	Default string `json:"default"`
	// Rules are evaluated in order; the first match wins.
	Rules []Rule `json:"rules"`
}

type Rule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Src     Src    `json:"src"`
	Dst     Dst    `json:"dst"`
	Action  string `json:"action"`
}

// Src matches where traffic comes from. Empty matches every LAN client;
// otherwise a packet matches if it comes from any listed device, prefix,
// network (by id) or member of a listed group (by id).
type Src struct {
	Devices  []string `json:"devices"`
	CIDRs    []string `json:"cidrs"`
	Networks []string `json:"networks"`
	Groups   []string `json:"groups"`
}

// Dst matches where traffic is going. Domains, CIDRs and Countries are
// alternatives (any may match); Proto and Ports narrow the match further.
type Dst struct {
	Domains []string `json:"domains"`
	CIDRs   []string `json:"cidrs"`
	// Countries are ISO 3166-1 alpha-2 codes, matched against the
	// country database shipped in the image.
	Countries []string `json:"countries"`
	Proto     string   `json:"proto"`
	Ports     []string `json:"ports"`
}

func (s Src) Any() bool {
	return len(s.Devices) == 0 && len(s.CIDRs) == 0 && len(s.Networks) == 0 && len(s.Groups) == 0
}
func (d Dst) Any() bool {
	return len(d.Domains) == 0 && len(d.CIDRs) == 0 && len(d.Countries) == 0 && d.Proto == "" && len(d.Ports) == 0
}

// Device holds per-client settings, keyed by MAC address.
type Device struct {
	MAC  string `json:"mac"`
	Name string `json:"name"`
	// Route overrides Policy.Default (and its group's and its network's
	// route) for this device: "" or an action, as for a network.
	Route string `json:"route"`
	// Paused cuts the device off without forgetting its settings.
	Paused bool `json:"paused"`
	// AllowUntil lets the device online although a schedule or a timer
	// has its group offline, until that moment (unix time); 0, or a
	// moment that has passed, for not at all.
	AllowUntil int64 `json:"allow_until"`
	// IP reserves a fixed IPv4 address inside one of the networks.
	IP string `json:"ip,omitempty"`
	// NoAdblock exempts the device from ad blocking.
	NoAdblock bool `json:"no_adblock"`
}

// Default returns the configuration of a freshly flashed router: wired WAN,
// built-in radio reserved for the access point, radios off until a country
// and a network name are set.
func Default() *Config {
	return &Config{
		Version: SchemaVersion,
		System:  System{Name: "Porter"},
		Radios:  []Radio{{ID: "builtin", Role: RoleAP, Band: "5"}},
		Ethernet: []EthPort{
			{Name: "eth0", Role: EthWAN, Network: MainNetwork},
		},
		Wifi: []SSID{{ID: MainSSID, Security: SecWPA2, Network: MainNetwork, Enabled: true}},
		WAN: WAN{
			WifiMAC: MACPolicy{Mode: MACHardware},
			EthMAC:  MACPolicy{Mode: MACHardware},
			Order:   []string{UplinkEth("eth0"), UplinkWifi, UplinkPhone},
			Probe: Probe{
				URL:    "http://captive.apple.com/hotspot-detect.html",
				Expect: "Success",
			},
		},
		LAN:       LAN{AutoRenumber: true, LeaseMinutes: 720, Domain: "lan", IPv6: true},
		Networks:  []Network{{ID: MainNetwork, Name: "Main", Address: "10.83.0.1/24", ULAPrefix: randomULA()}},
		DNS:       DNS{Intercept: true, Adblock: Adblock{Lists: []string{DefaultBlockList}}},
		Policy:    Policy{Default: ActDirect},
		SpeedTest: DefaultSpeedTest(),
		Inbound: Inbound{
			Server:   WGServer{Port: DefaultServerPort, PrivateKey: newWGKey(), Address: ServerSubnets[0], ULAPrefix: randomULA()},
			CheckURL: DefaultCheckURL,
		},
	}
}

func randomULA() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	// fd00::/8 + 40 random bits + subnet id 1.
	return fmt.Sprintf("fd%02x:%02x%02x:%02x%02x:1::/64", b[0], b[1], b[2], b[3], b[4])
}

// RandomMAC returns a locally administered unicast address.
func RandomMAC() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[0] = (b[0] | 0x02) &^ 0x01
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// NewID returns a short random identifier for rules and VPN profiles.
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

// Ready reports whether the minimum needed to start radios has been set.
func (c *Config) Ready() bool {
	return c.System.Country != "" && len(c.Wifi) > 0 && c.Wifi[0].SSID != "" && c.Wifi[0].Passphrase != ""
}

// SetWifiUplink makes one radio the one that joins upstream Wi-Fi
// networks, or none of them (id ""), and leaves the router's own network
// on the air: the radio shares itself between both jobs when no other
// radio is broadcasting, and a radio that was the uplink goes back to
// broadcasting. present says which radios are plugged in, and canBoth
// which can do both jobs at once.
func (c *Config) SetWifiUplink(id string, present, canBoth func(id string) bool) error {
	if id != "" && (c.Radio(id) == nil || !present(id)) {
		return fmt.Errorf("that radio is not there")
	}
	others := 0
	for i := range c.Radios {
		r := &c.Radios[i]
		if r.ID == id {
			continue
		}
		if r.Role == RoleWAN || r.Role == RoleAPWAN {
			r.Role = RoleAP
		}
		if r.Role == RoleAP && present(r.ID) {
			others++
		}
	}
	if id == "" {
		return nil
	}
	switch r := c.Radio(id); {
	case others > 0:
		r.Role = RoleWAN
	case canBoth(id):
		r.Role = RoleAPWAN
	default:
		return fmt.Errorf("This adapter cannot broadcast your network and join another at the same time, and there is no other radio for your network.")
	}
	return nil
}

// Main returns the main network.
func (c *Config) Main() *Network { return &c.Networks[0] }

// Network returns the network with the given id.
func (c *Config) Network(id string) *Network {
	for i := range c.Networks {
		if c.Networks[i].ID == id {
			return &c.Networks[i]
		}
	}
	return nil
}

// DiscoveryPeers says between which networks multicast DNS is relayed:
// for each network that takes part, by id, the others it is relayed to
// and from. A network that lets itself be found (Discovery) is paired
// with every network that is not isolated; two networks of which neither
// asked, or where the only one that did would be found from an isolated
// one, are not.
func (c *Config) DiscoveryPeers() map[string][]string {
	out := map[string][]string{}
	for i, a := range c.Networks {
		for _, b := range c.Networks[i+1:] {
			if (a.Discovery && !b.Isolated) || (b.Discovery && !a.Isolated) {
				out[a.ID] = append(out[a.ID], b.ID)
				out[b.ID] = append(out[b.ID], a.ID)
			}
		}
	}
	return out
}

// NetworkOf returns the network an address belongs to.
func (c *Config) NetworkOf(a netip.Addr) *Network {
	for i := range c.Networks {
		if c.Networks[i].Contains(a) {
			return &c.Networks[i]
		}
	}
	return nil
}

// SSID returns the Wi-Fi network with the given id.
func (c *Config) SSID(id string) *SSID {
	for i := range c.Wifi {
		if c.Wifi[i].ID == id {
			return &c.Wifi[i]
		}
	}
	return nil
}

// Radio returns the configured radio with the given id.
func (c *Config) Radio(id string) *Radio {
	for i := range c.Radios {
		if c.Radios[i].ID == id {
			return &c.Radios[i]
		}
	}
	return nil
}

func (c *Config) VPN(id string) *VPN {
	for i := range c.VPNs {
		if c.VPNs[i].ID == id {
			return &c.VPNs[i]
		}
	}
	return nil
}

func (c *Config) Device(mac string) *Device {
	for i := range c.Devices {
		if c.Devices[i].MAC == mac {
			return &c.Devices[i]
		}
	}
	return nil
}

// Store reads and writes the configuration file atomically.
type Store struct{ path string }

func NewStore(dir string) *Store { return &Store{path: filepath.Join(dir, "config.json")} }

// Load returns the stored configuration, or the defaults when none exists.
// A file that cannot be parsed or validated is moved aside rather than
// trusted, so a corrupt write can never brick the router.
func (s *Store) Load() (*Config, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	b, err = migrate(b, SchemaVersion, migrations)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err == nil {
		c.Normalize()
		err = c.Validate()
	}
	if err != nil {
		_ = os.Rename(s.path, s.path+".bad")
		return Default(), fmt.Errorf("stored config rejected, using defaults: %w", err)
	}
	return &c, nil
}

// migrations carries settings across versions of the software. Entry n
// rewrites a stored document of schema version n as version n+1; a
// version that needs no rewriting has no entry. Fields that are merely
// new need nothing here: Normalize gives them their defaults.
//
// The document is handled as plain JSON rather than as a Config, because
// a field an old version wrote may no longer exist in Config at all.
//
// When SchemaVersion is raised, the step from the previous version goes
// here. Without it a router that updates itself would lose its settings.
//
// Format 3 only added fields (system.bluetooth_only and system.timezone,
// a wired port's vlan, pppoe, tagged, shaping and data, a network's vlan
// and discovery, shaping and data for a saved Wi-Fi network and for a
// phone, inbound, groups with a rule's src.groups and a device's
// allow_until) and two kinds of action (source: and only:), so format
// 2 has no entry. The number was raised all the same: an app that knows format 2
// would write the document back without them, and has to be told that it
// cannot.
var migrations = map[int]func(doc map[string]any) error{
	1: networksAndOrder,
}

// networksAndOrder is the step from format 1, which had one Wi-Fi network
// ("ap"), one LAN ("lan.address") and a single switch for preferring Wi-Fi
// over wired uplinks, to format 2 with its lists of Wi-Fi networks and
// networks and its ranking of uplinks.
func networksAndOrder(doc map[string]any) error {
	section := func(name string) map[string]any {
		m, _ := doc[name].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		return m
	}
	ap := section("ap")
	ssid := map[string]any{"id": MainSSID, "network": MainNetwork, "enabled": true}
	for _, k := range []string{"ssid", "passphrase", "security", "hidden", "isolate"} {
		if v, ok := ap[k]; ok {
			ssid[k] = v
		}
	}
	doc["wifi"] = []any{ssid}
	delete(doc, "ap")

	lan := section("lan")
	network := map[string]any{"id": MainNetwork, "name": "Main"}
	for _, k := range []string{"address", "ula_prefix"} {
		if v, ok := lan[k]; ok {
			network[k] = v
		}
		delete(lan, k)
	}
	doc["lan"] = lan
	doc["networks"] = []any{network}

	// One radio at most joins other networks. Format 1 did not insist,
	// and a configuration that says otherwise could never be changed
	// again: every change is checked as a whole.
	uplink := false
	radios, _ := doc["radios"].([]any)
	for _, r := range radios {
		m, _ := r.(map[string]any)
		role, _ := m["role"].(string)
		if role != RoleWAN && role != RoleAPWAN {
			continue
		}
		if uplink {
			m["role"] = map[string]string{RoleWAN: RoleOff, RoleAPWAN: RoleAP}[role]
		}
		uplink = true
	}

	wan := section("wan")
	var wired []any
	ports, _ := doc["ethernet"].([]any)
	for _, p := range ports {
		if m, _ := p.(map[string]any); m != nil {
			if name, _ := m["name"].(string); name != "" {
				wired = append(wired, UplinkEth(name))
			}
		}
	}
	order := append(append([]any{}, wired...), UplinkWifi)
	if prefer, _ := wan["prefer_wifi"].(bool); prefer {
		order = append([]any{UplinkWifi}, wired...)
	}
	wan["order"] = append(order, UplinkPhone)
	delete(wan, "prefer_wifi")
	doc["wan"] = wan
	return nil
}

// migrate brings a stored document up to schema version to.
func migrate(raw []byte, to int, steps map[int]func(doc map[string]any) error) ([]byte, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	switch {
	case head.Version == to:
		return raw, nil
	case head.Version > to:
		return nil, fmt.Errorf("the settings were written by a newer version of the software (format %d; this version reads %d)", head.Version, to)
	case head.Version < 1:
		return nil, fmt.Errorf("unsupported config version %d", head.Version)
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keeps large numbers exact through the round trip
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	for v := head.Version; v < to; v++ {
		if step := steps[v]; step != nil {
			if err := step(doc); err != nil {
				return nil, fmt.Errorf("carrying the settings from format %d to %d: %w", v, v+1, err)
			}
		}
		doc["version"] = v + 1
	}
	return json.Marshal(doc)
}

func (s *Store) Save(c *Config) error {
	b, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path, b, 0o600)
}

// WriteFileAtomic replaces path with data so that a power cut leaves either
// the old or the new content, never a partial file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
