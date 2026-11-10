package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var (
	reCountry = regexp.MustCompile(`^[A-Z]{2}$`)
	reID      = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
	reIface   = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`)
	reRadio   = regexp.MustCompile(`^(builtin|[a-z0-9]{2,8}:[0-9a-f]{2}(:[0-9a-f]{2}){5})$`)
	reLabel   = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)
)

// Normalize canonicalises user input (case, whitespace) so that validation
// and comparisons can be strict.
func (c *Config) Normalize() {
	c.System.Name = strings.TrimSpace(c.System.Name)
	c.System.Country = strings.ToUpper(strings.TrimSpace(c.System.Country))
	for i := range c.Devices {
		c.Devices[i].MAC = strings.ToLower(strings.TrimSpace(c.Devices[i].MAC))
		c.Devices[i].Name = strings.TrimSpace(c.Devices[i].Name)
	}
	c.WAN.WifiMAC.Address = strings.ToLower(c.WAN.WifiMAC.Address)
	c.WAN.EthMAC.Address = strings.ToLower(c.WAN.EthMAC.Address)
	for i := range c.Policy.Rules {
		r := &c.Policy.Rules[i]
		for j := range r.Src.Devices {
			r.Src.Devices[j] = strings.ToLower(strings.TrimSpace(r.Src.Devices[j]))
		}
		for j := range r.Dst.Domains {
			r.Dst.Domains[j] = NormalizeDomain(r.Dst.Domains[j])
		}
		r.Dst.Proto = strings.ToLower(r.Dst.Proto)
	}
	for i := range c.DNS.Blocked {
		c.DNS.Blocked[i] = NormalizeDomain(c.DNS.Blocked[i])
	}
	if c.WAN.WifiMAC.Mode == MACRandom && c.WAN.WifiMAC.Address == "" {
		c.WAN.WifiMAC.Address = RandomMAC()
	}
	if c.WAN.EthMAC.Mode == MACRandom && c.WAN.EthMAC.Address == "" {
		c.WAN.EthMAC.Address = RandomMAC()
	}
	// The app decodes this document strictly: a list is always a list,
	// never null.
	fillNilSlices(reflect.ValueOf(c).Elem())
}

// fillNilSlices replaces every nil slice reachable from v with an empty
// one.
func fillNilSlices(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillNilSlices(v.Field(i))
		}
	case reflect.Slice:
		if v.IsNil() && v.CanSet() {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		}
		for i := 0; i < v.Len(); i++ {
			fillNilSlices(v.Index(i))
		}
	}
}

// NormalizeDomain lower-cases a domain and strips wildcard and dot
// decoration: "*.Example.com." becomes "example.com".
func NormalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "*.")
	return strings.Trim(d, ".")
}

func validDomain(d string) bool {
	if d == "" || len(d) > 253 {
		return false
	}
	for _, l := range strings.Split(d, ".") {
		if !reLabel.MatchString(l) {
			return false
		}
	}
	return true
}

func validMAC(s string) bool {
	hw, err := net.ParseMAC(s)
	return err == nil && len(hw) == 6 && s == hw.String()
}

// Validate checks every field. The configuration comes from the phone, so
// nothing in it is trusted to be well formed: values end up in hostapd,
// wpa_supplicant, dnsmasq and nftables input.
func (c *Config) Validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if err := c.validateSystem(); err != nil {
		return err
	}
	if err := c.validateRadios(); err != nil {
		return err
	}
	if err := c.validateAP(); err != nil {
		return err
	}
	if err := c.validateWAN(); err != nil {
		return err
	}
	if err := c.validateLAN(); err != nil {
		return err
	}
	if err := c.validateDNS(); err != nil {
		return err
	}
	if err := c.validateVPNs(); err != nil {
		return err
	}
	if err := c.validateDevices(); err != nil {
		return err
	}
	return c.validatePolicy()
}

func printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (c *Config) validateSystem() error {
	s := c.System
	if s.Name == "" || len(s.Name) > 20 || !printable(s.Name) {
		return fmt.Errorf("system.name must be 1-20 printable characters")
	}
	if s.Country != "" && !reCountry.MatchString(s.Country) {
		return fmt.Errorf("system.country must be a two-letter country code")
	}
	if len(s.SSH.AuthorizedKeys) > 8 {
		return fmt.Errorf("too many SSH keys")
	}
	for _, k := range s.SSH.AuthorizedKeys {
		if len(k) > 2048 || !printable(k) || !(strings.HasPrefix(k, "ssh-") || strings.HasPrefix(k, "ecdsa-") || strings.HasPrefix(k, "sk-")) {
			return fmt.Errorf("invalid SSH public key")
		}
	}
	if s.SSH.Enabled && len(s.SSH.AuthorizedKeys) == 0 {
		return fmt.Errorf("SSH needs at least one authorized key")
	}
	return nil
}

func (c *Config) validateRadios() error {
	seen := map[string]bool{}
	for _, r := range c.Radios {
		if !reRadio.MatchString(r.ID) {
			return fmt.Errorf("invalid radio id %q", r.ID)
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate radio %q", r.ID)
		}
		seen[r.ID] = true
		switch r.Role {
		case RoleOff, RoleAP, RoleWAN, RoleAPWAN:
		default:
			return fmt.Errorf("radio %s: invalid role %q", r.ID, r.Role)
		}
		switch r.Band {
		case "", "2.4", "5":
		default:
			return fmt.Errorf("radio %s: invalid band %q", r.ID, r.Band)
		}
		if r.Channel < 0 || r.Channel > 233 {
			return fmt.Errorf("radio %s: invalid channel", r.ID)
		}
		switch r.Width {
		case 0, 20, 40, 80:
		default:
			return fmt.Errorf("radio %s: invalid width", r.ID)
		}
	}
	seenEth := map[string]bool{}
	for _, e := range c.Ethernet {
		if !reIface.MatchString(e.Name) {
			return fmt.Errorf("invalid interface name %q", e.Name)
		}
		if seenEth[e.Name] {
			return fmt.Errorf("duplicate interface %q", e.Name)
		}
		seenEth[e.Name] = true
		switch e.Role {
		case EthWAN, EthLAN, EthOff:
		default:
			return fmt.Errorf("%s: invalid role %q", e.Name, e.Role)
		}
	}
	return nil
}

func (c *Config) validateAP() error {
	a := c.AP
	if a.SSID != "" {
		if len(a.SSID) > 32 || !printable(a.SSID) {
			return fmt.Errorf("network name must be 1-32 printable bytes")
		}
	}
	if a.Passphrase != "" {
		if len(a.Passphrase) < 8 || len(a.Passphrase) > 63 || !asciiPrintable(a.Passphrase) {
			return fmt.Errorf("Wi-Fi password must be 8-63 ASCII characters")
		}
	}
	switch a.Security {
	case SecWPA2, SecWPA2WPA3, SecWPA3:
	default:
		return fmt.Errorf("invalid Wi-Fi security %q", a.Security)
	}
	return nil
}

func asciiPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func validMACPolicy(p MACPolicy, what string) error {
	switch p.Mode {
	case MACHardware:
	case MACRandom, MACCustom:
		hw, err := net.ParseMAC(p.Address)
		if err != nil || len(hw) != 6 || hw[0]&1 != 0 {
			return fmt.Errorf("%s: invalid MAC address", what)
		}
	default:
		return fmt.Errorf("%s: invalid mode %q", what, p.Mode)
	}
	return nil
}

func (c *Config) validateWAN() error {
	w := c.WAN
	if len(w.Networks) > 64 {
		return fmt.Errorf("too many saved networks")
	}
	seen := map[string]bool{}
	for _, n := range w.Networks {
		if n.SSID == "" || len(n.SSID) > 32 {
			return fmt.Errorf("saved network: name must be 1-32 bytes")
		}
		if seen[n.SSID] {
			return fmt.Errorf("saved network %q listed twice", n.SSID)
		}
		seen[n.SSID] = true
		if n.PSK != "" && (len(n.PSK) < 8 || len(n.PSK) > 63 || !asciiPrintable(n.PSK)) {
			return fmt.Errorf("saved network %q: password must be 8-63 ASCII characters", n.SSID)
		}
		if n.Priority < 0 || n.Priority > 1000 {
			return fmt.Errorf("saved network %q: invalid priority", n.SSID)
		}
	}
	if err := validMACPolicy(w.WifiMAC, "wan.wifi_mac"); err != nil {
		return err
	}
	if err := validMACPolicy(w.EthMAC, "wan.eth_mac"); err != nil {
		return err
	}
	if w.Probe.Enabled {
		u, err := url.Parse(w.Probe.URL)
		if err != nil || u.Scheme != "http" || u.Hostname() == "" {
			return fmt.Errorf("probe URL must be a plain http:// URL")
		}
		if w.Probe.Expect == "" || len(w.Probe.Expect) > 128 {
			return fmt.Errorf("probe needs an expected response text")
		}
	}
	return nil
}

func (c *Config) validateLAN() error {
	l := c.LAN
	p, err := netip.ParsePrefix(l.Address)
	if err != nil || !p.Addr().Is4() {
		return fmt.Errorf("lan.address must be an IPv4 address with prefix")
	}
	if p.Bits() < 16 || p.Bits() > 28 {
		return fmt.Errorf("lan.address prefix must be between /16 and /28")
	}
	if !p.Addr().IsPrivate() {
		return fmt.Errorf("lan.address must be a private address")
	}
	if p.Addr() == p.Masked().Addr() {
		return fmt.Errorf("lan.address must be a host address, not the network address")
	}
	if l.LeaseMinutes < 2 || l.LeaseMinutes > 7*24*60 {
		return fmt.Errorf("lan.lease_minutes out of range")
	}
	if !validDomain(l.Domain) {
		return fmt.Errorf("lan.domain is not a valid domain")
	}
	u, err := netip.ParsePrefix(l.ULAPrefix)
	if err != nil || !u.Addr().Is6() || u.Bits() != 64 {
		return fmt.Errorf("lan.ula_prefix must be an IPv6 /64")
	}
	if !netip.MustParsePrefix("fc00::/7").Contains(u.Addr()) {
		return fmt.Errorf("lan.ula_prefix must be inside fc00::/7")
	}
	return nil
}

// Upstream is a parsed DNS upstream.
type Upstream struct {
	Proto      string // "udp", "tls", "https"
	Addr       netip.AddrPort
	ServerName string // TLS name for tls/https
	URL        string // for https
}

// ParseUpstream parses the forms documented on DNS.Upstreams.
func ParseUpstream(s string) (Upstream, error) {
	switch {
	case strings.HasPrefix(s, "tls://"):
		rest := strings.TrimPrefix(s, "tls://")
		host, name, ok := strings.Cut(rest, "#")
		ap, err := parseAddrPort(host, 853)
		if err != nil {
			return Upstream{}, fmt.Errorf("dns upstream %q: %w", s, err)
		}
		if ok && !validDomain(name) {
			return Upstream{}, fmt.Errorf("dns upstream %q: bad server name", s)
		}
		return Upstream{Proto: "tls", Addr: ap, ServerName: name}, nil
	case strings.HasPrefix(s, "https://"):
		raw, boot, ok := strings.Cut(s, "#")
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || !validDomain(strings.ToLower(u.Hostname())) {
			return Upstream{}, fmt.Errorf("dns upstream %q: bad URL", s)
		}
		if !ok {
			return Upstream{}, fmt.Errorf("dns upstream %q: needs #<server ip>", s)
		}
		port := 443
		if u.Port() != "" {
			port, _ = strconv.Atoi(u.Port())
		}
		ap, err := parseAddrPort(boot, port)
		if err != nil {
			return Upstream{}, fmt.Errorf("dns upstream %q: %w", s, err)
		}
		return Upstream{Proto: "https", Addr: ap, ServerName: u.Hostname(), URL: raw}, nil
	default:
		ap, err := parseAddrPort(s, 53)
		if err != nil {
			return Upstream{}, fmt.Errorf("dns upstream %q: %w", s, err)
		}
		return Upstream{Proto: "udp", Addr: ap}, nil
	}
}

func parseAddrPort(s string, defPort int) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("not an IP address")
	}
	return netip.AddrPortFrom(a, uint16(defPort)), nil
}

func (c *Config) validateDNS() error {
	if len(c.DNS.Upstreams) > 8 {
		return fmt.Errorf("too many DNS upstreams")
	}
	for _, u := range c.DNS.Upstreams {
		if _, err := ParseUpstream(u); err != nil {
			return err
		}
	}
	if len(c.DNS.Blocked) > 5000 {
		return fmt.Errorf("too many blocked domains")
	}
	for _, d := range c.DNS.Blocked {
		if !validDomain(d) {
			return fmt.Errorf("blocked domain %q is not valid", d)
		}
	}
	return nil
}

func validWGKey(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

func (c *Config) validateVPNs() error {
	if len(c.VPNs) > 8 {
		return fmt.Errorf("at most 8 VPN profiles")
	}
	seen := map[string]bool{}
	for _, v := range c.VPNs {
		if !reID.MatchString(v.ID) {
			return fmt.Errorf("invalid VPN id %q", v.ID)
		}
		if seen[v.ID] {
			return fmt.Errorf("duplicate VPN id %q", v.ID)
		}
		seen[v.ID] = true
		if v.Name == "" || len(v.Name) > 40 || !printable(v.Name) {
			return fmt.Errorf("VPN name must be 1-40 characters")
		}
		w := v.WireGuard
		if !validWGKey(w.PrivateKey) {
			return fmt.Errorf("VPN %q: invalid private key", v.Name)
		}
		if len(w.Addresses) == 0 || len(w.Addresses) > 4 {
			return fmt.Errorf("VPN %q: needs 1-4 interface addresses", v.Name)
		}
		for _, a := range w.Addresses {
			if _, err := ParsePrefixLoose(a); err != nil {
				return fmt.Errorf("VPN %q: bad address %q", v.Name, a)
			}
		}
		if len(w.DNS) > 4 {
			return fmt.Errorf("VPN %q: too many DNS servers", v.Name)
		}
		for _, d := range w.DNS {
			if _, err := netip.ParseAddr(d); err != nil {
				return fmt.Errorf("VPN %q: DNS server %q is not an IP address", v.Name, d)
			}
		}
		if w.MTU != 0 && (w.MTU < 1280 || w.MTU > 9000) {
			return fmt.Errorf("VPN %q: MTU out of range", v.Name)
		}
		if len(w.Peers) == 0 || len(w.Peers) > 8 {
			return fmt.Errorf("VPN %q: needs 1-8 peers", v.Name)
		}
		for _, p := range w.Peers {
			if !validWGKey(p.PublicKey) {
				return fmt.Errorf("VPN %q: invalid peer public key", v.Name)
			}
			if p.PresharedKey != "" && !validWGKey(p.PresharedKey) {
				return fmt.Errorf("VPN %q: invalid preshared key", v.Name)
			}
			if p.Endpoint != "" {
				host, port, err := net.SplitHostPort(p.Endpoint)
				if err != nil {
					return fmt.Errorf("VPN %q: endpoint must be host:port", v.Name)
				}
				if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
					return fmt.Errorf("VPN %q: bad endpoint port", v.Name)
				}
				if _, err := netip.ParseAddr(host); err != nil && !validDomain(strings.ToLower(host)) {
					return fmt.Errorf("VPN %q: bad endpoint host", v.Name)
				}
			}
			if len(p.AllowedIPs) > 256 {
				return fmt.Errorf("VPN %q: too many allowed IPs", v.Name)
			}
			for _, a := range p.AllowedIPs {
				if _, err := netip.ParsePrefix(a); err != nil {
					return fmt.Errorf("VPN %q: bad allowed IP %q", v.Name, a)
				}
			}
			if p.Keepalive < 0 || p.Keepalive > 65535 {
				return fmt.Errorf("VPN %q: bad keepalive", v.Name)
			}
		}
	}
	return nil
}

// ParsePrefixLoose accepts "10.0.0.2/32" as well as a bare address.
func ParsePrefixLoose(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func (c *Config) validAction(a string, allowEmpty bool) error {
	if a == "" && allowEmpty {
		return nil
	}
	if a == ActDirect || a == ActBlock {
		return nil
	}
	if id, ok := VPNOf(a); ok {
		if c.VPN(id) == nil {
			return fmt.Errorf("action refers to unknown VPN %q", id)
		}
		return nil
	}
	return fmt.Errorf("invalid action %q", a)
}

func (c *Config) validateDevices() error {
	if len(c.Devices) > 512 {
		return fmt.Errorf("too many devices")
	}
	_, subnet := c.LANPrefix()
	router, _ := c.LANPrefix()
	seen := map[string]bool{}
	ips := map[string]bool{}
	for _, d := range c.Devices {
		if !validMAC(d.MAC) {
			return fmt.Errorf("invalid device MAC %q", d.MAC)
		}
		if seen[d.MAC] {
			return fmt.Errorf("device %s listed twice", d.MAC)
		}
		seen[d.MAC] = true
		if len(d.Name) > 40 || !printable(d.Name) {
			return fmt.Errorf("device name too long")
		}
		if err := c.validAction(d.Route, true); err != nil {
			return fmt.Errorf("device %s: %w", d.MAC, err)
		}
		if d.IP != "" {
			a, err := netip.ParseAddr(d.IP)
			if err != nil || !subnet.Contains(a) || a == router {
				return fmt.Errorf("device %s: reserved address must be inside %s", d.MAC, subnet)
			}
			if ips[d.IP] {
				return fmt.Errorf("address %s reserved twice", d.IP)
			}
			ips[d.IP] = true
		}
	}
	return nil
}

// PortRange is an inclusive range of TCP/UDP ports.
type PortRange struct{ Lo, Hi uint16 }

func ParsePorts(s string) (PortRange, error) {
	lo, hi, isRange := strings.Cut(s, "-")
	a, err := strconv.ParseUint(lo, 10, 16)
	if err != nil || a == 0 {
		return PortRange{}, fmt.Errorf("bad port %q", s)
	}
	b := a
	if isRange {
		b, err = strconv.ParseUint(hi, 10, 16)
		if err != nil || b < a {
			return PortRange{}, fmt.Errorf("bad port range %q", s)
		}
	}
	return PortRange{uint16(a), uint16(b)}, nil
}

func (c *Config) validatePolicy() error {
	if err := c.validAction(c.Policy.Default, false); err != nil {
		return fmt.Errorf("policy.default: %w", err)
	}
	if len(c.Policy.Rules) > 100 {
		return fmt.Errorf("at most 100 rules")
	}
	seen := map[string]bool{}
	for _, r := range c.Policy.Rules {
		if !reID.MatchString(r.ID) {
			return fmt.Errorf("invalid rule id %q", r.ID)
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		if len(r.Name) > 40 || !printable(r.Name) {
			return fmt.Errorf("rule name too long")
		}
		if err := c.validAction(r.Action, false); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
		if len(r.Src.Devices) > 64 || len(r.Src.CIDRs) > 64 || len(r.Dst.Domains) > 256 || len(r.Dst.CIDRs) > 256 || len(r.Dst.Ports) > 32 {
			return fmt.Errorf("rule %q: too many entries", r.Name)
		}
		for _, m := range r.Src.Devices {
			if !validMAC(m) {
				return fmt.Errorf("rule %q: invalid device %q", r.Name, m)
			}
		}
		for _, p := range r.Src.CIDRs {
			if _, err := ParsePrefixLoose(p); err != nil {
				return fmt.Errorf("rule %q: invalid source %q", r.Name, p)
			}
		}
		for _, d := range r.Dst.Domains {
			if !validDomain(d) {
				return fmt.Errorf("rule %q: invalid domain %q", r.Name, d)
			}
		}
		for _, p := range r.Dst.CIDRs {
			if _, err := ParsePrefixLoose(p); err != nil {
				return fmt.Errorf("rule %q: invalid destination %q", r.Name, p)
			}
		}
		switch r.Dst.Proto {
		case "", "tcp", "udp":
		default:
			return fmt.Errorf("rule %q: protocol must be tcp, udp or empty", r.Name)
		}
		for _, p := range r.Dst.Ports {
			if _, err := ParsePorts(p); err != nil {
				return fmt.Errorf("rule %q: %w", r.Name, err)
			}
		}
	}
	return nil
}
