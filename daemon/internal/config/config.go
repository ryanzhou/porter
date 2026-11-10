// Package config defines the persistent router configuration: its schema,
// defaults, validation and on-disk storage.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

const SchemaVersion = 1

// Config is the whole user-controllable state of the router. It is sent to
// the app as JSON and written back section by section.
type Config struct {
	Version  int       `json:"version"`
	Rev      uint64    `json:"rev"`
	System   System    `json:"system"`
	Radios   []Radio   `json:"radios"`
	Ethernet []EthPort `json:"ethernet"`
	AP       AP        `json:"ap"`
	WAN      WAN       `json:"wan"`
	LAN      LAN       `json:"lan"`
	DNS      DNS       `json:"dns"`
	VPNs     []VPN     `json:"vpns"`
	Policy   Policy    `json:"policy"`
	Devices  []Device  `json:"devices"`
}

type System struct {
	// Name is the router's display name; also used as the BLE local name.
	Name string `json:"name"`
	// Country is the ISO 3166-1 alpha-2 regulatory domain for all radios.
	// Radios stay off until it is set.
	Country string `json:"country"`
	// NTP enables network time sync. Time is also set from the phone on
	// every control session, so this can be switched off.
	NTP bool `json:"ntp"`
	// SSH enables an OpenSSH server reachable from the LAN only.
	SSH SSH `json:"ssh"`
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
	// Band is "2.4", "5" or "6" for the AP. Ignored for RoleAPWAN, where the
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
}

const (
	SecWPA2     = "wpa2"
	SecWPA2WPA3 = "wpa2+wpa3"
	SecWPA3     = "wpa3"
)

// AP is the network the router broadcasts. All AP radios share it.
type AP struct {
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	Security   string `json:"security"`
	Hidden     bool   `json:"hidden"`
	// Isolate stops wireless clients from talking to each other.
	Isolate bool `json:"isolate"`
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
}

type WAN struct {
	// Networks are the upstream Wi-Fi networks the router may join.
	Networks []WifiNetwork `json:"networks"`
	// WifiMAC and EthMAC control the address presented upstream.
	WifiMAC MACPolicy `json:"wifi_mac"`
	EthMAC  MACPolicy `json:"eth_mac"`
	// PreferWifi puts wireless uplinks ahead of wired ones when both are up.
	PreferWifi bool `json:"prefer_wifi"`
	// Probe controls connectivity / captive portal detection.
	Probe Probe `json:"probe"`
}

type Probe struct {
	Enabled bool `json:"enabled"`
	// URL must be plain HTTP and return a body containing Expect when the
	// internet is reachable.
	URL    string `json:"url"`
	Expect string `json:"expect"`
}

type LAN struct {
	// Address is the router's LAN address with prefix, e.g. 10.83.0.1/24.
	Address string `json:"address"`
	// AutoRenumber moves the LAN to another subnet when it collides with
	// the upstream network.
	AutoRenumber bool   `json:"auto_renumber"`
	LeaseMinutes int    `json:"lease_minutes"`
	Domain       string `json:"domain"`
	IPv6         bool   `json:"ipv6"`
	// ULAPrefix is the /64 announced on the LAN; traffic is NAT66'd upstream.
	ULAPrefix string `json:"ula_prefix"`
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
}

type VPN struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// KillSwitch drops traffic assigned to this VPN while the tunnel is
	// down instead of letting it leave directly.
	KillSwitch bool      `json:"kill_switch"`
	WireGuard  WireGuard `json:"wireguard"`
}

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

// Actions. A VPN action is "vpn:<id>".
const (
	ActDirect = "direct"
	ActBlock  = "block"
	actVPNPfx = "vpn:"
)

func ActVPN(id string) string { return actVPNPfx + id }

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
// otherwise a packet matches if it comes from any listed device or prefix.
type Src struct {
	Devices []string `json:"devices"`
	CIDRs   []string `json:"cidrs"`
}

// Dst matches where traffic is going. Domains and CIDRs are alternatives
// (either may match); Proto and Ports narrow the match further.
type Dst struct {
	Domains []string `json:"domains"`
	CIDRs   []string `json:"cidrs"`
	Proto   string   `json:"proto"`
	Ports   []string `json:"ports"`
}

func (s Src) Any() bool { return len(s.Devices) == 0 && len(s.CIDRs) == 0 }
func (d Dst) Any() bool {
	return len(d.Domains) == 0 && len(d.CIDRs) == 0 && d.Proto == "" && len(d.Ports) == 0
}

// Device holds per-client settings, keyed by MAC address.
type Device struct {
	MAC  string `json:"mac"`
	Name string `json:"name"`
	// Route overrides Policy.Default for this device: "", "direct",
	// "vpn:<id>" or "block".
	Route string `json:"route"`
	// Paused cuts the device off without forgetting its settings.
	Paused bool `json:"paused"`
	// IP reserves a fixed IPv4 address inside the LAN subnet.
	IP string `json:"ip,omitempty"`
}

// Default returns the configuration of a freshly flashed router: wired WAN,
// built-in radio reserved for the access point, radios off until a country
// and a network name are set.
func Default() *Config {
	return &Config{
		Version: SchemaVersion,
		System:  System{Name: "Porter", NTP: true},
		Radios:  []Radio{{ID: "builtin", Role: RoleAP, Band: "5"}},
		Ethernet: []EthPort{
			{Name: "eth0", Role: EthWAN},
		},
		AP: AP{Security: SecWPA2},
		WAN: WAN{
			WifiMAC: MACPolicy{Mode: MACHardware},
			EthMAC:  MACPolicy{Mode: MACHardware},
			Probe: Probe{
				Enabled: true,
				URL:     "http://captive.apple.com/hotspot-detect.html",
				Expect:  "Success",
			},
		},
		LAN: LAN{
			Address:      "10.83.0.1/24",
			AutoRenumber: true,
			LeaseMinutes: 720,
			Domain:       "lan",
			IPv6:         true,
			ULAPrefix:    randomULA(),
		},
		DNS:    DNS{Intercept: true},
		Policy: Policy{Default: ActDirect},
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
	return c.System.Country != "" && c.AP.SSID != "" && c.AP.Passphrase != ""
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

// LANPrefix returns the router address and the subnet it sits in.
func (c *Config) LANPrefix() (netip.Addr, netip.Prefix) {
	p := netip.MustParsePrefix(c.LAN.Address)
	return p.Addr(), p.Masked()
}

// ULA returns the router's IPv6 LAN address (::1 in the ULA /64) and prefix.
func (c *Config) ULA() (netip.Addr, netip.Prefix) {
	p := netip.MustParsePrefix(c.LAN.ULAPrefix).Masked()
	a := p.Addr().As16()
	a[15] = 1
	return netip.AddrFrom16(a), p
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
	err = json.Unmarshal(b, &c)
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
