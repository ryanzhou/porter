package config

import (
	"encoding/base64"
	"encoding/binary"
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
	// Names that belong to Wi-Fi interfaces, never to wired ports.
	reNotWired = regexp.MustCompile(`^(wl|ap(-|[0-9x]))`)
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
		for j := range r.Dst.Countries {
			r.Dst.Countries[j] = strings.ToUpper(strings.TrimSpace(r.Dst.Countries[j]))
		}
		r.Dst.Proto = strings.ToLower(r.Dst.Proto)
	}
	for i := range c.DNS.Blocked {
		c.DNS.Blocked[i] = NormalizeDomain(c.DNS.Blocked[i])
	}
	for i := range c.DNS.Adblock.Allow {
		c.DNS.Adblock.Allow[i] = NormalizeDomain(c.DNS.Adblock.Allow[i])
	}
	for i := range c.DNS.Adblock.Lists {
		c.DNS.Adblock.Lists[i] = strings.TrimSpace(c.DNS.Adblock.Lists[i])
	}
	// Switching ad blocking on should just work: there is always a list
	// to start from.
	if len(c.DNS.Adblock.Lists) == 0 && !c.DNS.Adblock.Enabled {
		c.DNS.Adblock.Lists = []string{DefaultBlockList}
	}
	if c.WAN.WifiMAC.Mode == MACRandom && c.WAN.WifiMAC.Address == "" {
		c.WAN.WifiMAC.Address = RandomMAC()
	}
	if c.WAN.EthMAC.Mode == MACRandom && c.WAN.EthMAC.Address == "" {
		c.WAN.EthMAC.Address = RandomMAC()
	}
	for i := range c.VPNs {
		v := &c.VPNs[i]
		v.Type = strings.ToLower(strings.TrimSpace(v.Type))
		if v.Type == "" {
			v.Type = VPNWireGuard
		}
		if v.Type != VPNMPQUIC {
			continue
		}
		m := &v.MPQUIC
		servers := []string{}
		for _, s := range m.Servers {
			if s = strings.TrimSpace(s); s != "" {
				servers = append(servers, s)
			}
		}
		m.Servers = servers
		if m.Port == 0 {
			m.Port = DefaultMPQUICPort
		}
		if len(m.DNS) == 0 {
			m.DNS = []string{"1.1.1.1", "1.0.0.1"}
		}
		m.Up, m.Down = strings.ToLower(strings.TrimSpace(m.Up)), strings.ToLower(strings.TrimSpace(m.Down))
	}
	// Earlier versions could mistake a Wi-Fi interface for a wired port
	// in the instant it was created, and remember it as one.
	ports := c.Ethernet[:0]
	for _, e := range c.Ethernet {
		if !reNotWired.MatchString(e.Name) {
			ports = append(ports, e)
		}
	}
	c.Ethernet = ports
	c.normalizeNetworks()
	for i := range c.Ethernet {
		c.Ethernet[i].IP.normalize()
		if c.Ethernet[i].Network == "" || c.Ethernet[i].Role != EthLAN {
			c.Ethernet[i].Network = MainNetwork
		}
	}
	c.normalizeOrder()
	for i := range c.WAN.Networks {
		c.WAN.Networks[i].IP.normalize()
	}
	// Configurations saved before speed tests existed have no server set.
	if c.SpeedTest.DownloadURL == "" && c.SpeedTest.UploadURL == "" {
		c.SpeedTest = DefaultSpeedTest()
	}
	// The app decodes this document strictly: a list is always a list,
	// never null.
	fillNilSlices(reflect.ValueOf(c).Elem())
}

// Subnets are the address ranges the router gives its networks when left
// to choose: for a new network, and for one that has to move out of the
// way of the upstream network.
var Subnets = []string{
	"10.83.0.1/24", "10.84.0.1/24", "172.23.83.1/24", "192.168.83.1/24",
	"10.85.0.1/24", "10.86.0.1/24", "172.23.84.1/24", "192.168.84.1/24",
}

// FreeSubnet returns the first of Subnets that overlaps none of the
// prefixes in use.
func FreeSubnet(taken []netip.Prefix) (netip.Prefix, bool) {
	for _, s := range Subnets {
		p := netip.MustParsePrefix(s)
		free := true
		for _, t := range taken {
			if t.Masked().Overlaps(p.Masked()) {
				free = false
			}
		}
		if free {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// normalizeNetworks makes sure the two things that always exist do, and
// fills in what a new Wi-Fi network or network was created without: an id,
// an address range, an IPv6 prefix.
func (c *Config) normalizeNetworks() {
	if len(c.Networks) == 0 {
		c.Networks = []Network{{Name: "Main"}}
	}
	// The first of each is the main one and goes by a fixed id. That is
	// filled in where it is missing and never written over: a list sent
	// in another order has to be refused, not quietly have every port
	// and rule that names the main network point at a different one.
	if c.Networks[0].ID == "" {
		c.Networks[0].ID = MainNetwork
	}
	var taken []netip.Prefix
	for _, n := range c.Networks {
		if p, err := netip.ParsePrefix(strings.TrimSpace(n.Address)); err == nil {
			taken = append(taken, p)
		}
	}
	for i := range c.Networks {
		n := &c.Networks[i]
		n.ID, n.Name, n.Address, n.ULAPrefix = strings.TrimSpace(n.ID), strings.TrimSpace(n.Name), strings.TrimSpace(n.Address), strings.TrimSpace(n.ULAPrefix)
		if n.ID == "" {
			n.ID = NewID()
		}
		if n.Address == "" {
			if p, ok := FreeSubnet(taken); ok {
				n.Address = p.String()
				taken = append(taken, p)
			}
		}
		if n.ULAPrefix == "" {
			n.ULAPrefix = randomULA()
		}
	}
	if len(c.Wifi) == 0 {
		c.Wifi = []SSID{{Security: SecWPA2, Enabled: true}}
	}
	if c.Wifi[0].ID == "" {
		c.Wifi[0].ID = MainSSID
	}
	for i := range c.Wifi {
		w := &c.Wifi[i]
		if w.ID = strings.TrimSpace(w.ID); w.ID == "" {
			w.ID = NewID()
		}
		if w.Security == "" {
			w.Security = SecWPA2
		}
		if w.Network == "" {
			w.Network = MainNetwork
		}
	}
	for i := range c.Policy.Rules {
		for j := range c.Policy.Rules[i].Src.Networks {
			c.Policy.Rules[i].Src.Networks[j] = strings.TrimSpace(c.Policy.Rules[i].Src.Networks[j])
		}
	}
}

// normalizeOrder makes the ranking of uplinks complete: every wired port
// has a place in it, as do Wi-Fi and a tethered phone, and nothing is in
// it twice. What is missing goes where it would be by default: wired
// ports ahead of Wi-Fi, a phone last.
func (c *Config) normalizeOrder() {
	port := map[string]bool{}
	for _, e := range c.Ethernet {
		port[e.Name] = true
	}
	seen := map[string]bool{}
	order := []string{}
	for _, k := range c.WAN.Order {
		name, wired := EthOfUplink(k)
		if seen[k] || (wired && !port[name]) || (!wired && k != UplinkWifi && k != UplinkPhone) {
			continue
		}
		seen[k] = true
		order = append(order, k)
	}
	insert := func(at int, k string) {
		order = append(order, "")
		copy(order[at+1:], order[at:])
		order[at] = k
		seen[k] = true
	}
	if !seen[UplinkWifi] {
		insert(len(order), UplinkWifi)
	}
	if !seen[UplinkPhone] {
		insert(len(order), UplinkPhone)
	}
	for _, e := range c.Ethernet {
		k := UplinkEth(e.Name)
		if seen[k] {
			continue
		}
		// After the last wired port already ranked, or else just ahead of
		// Wi-Fi.
		at := -1
		for i, o := range order {
			if _, wired := EthOfUplink(o); wired {
				at = i + 1
			}
		}
		if at < 0 {
			for i, o := range order {
				if o == UplinkWifi {
					at = i
				}
			}
		}
		insert(at, k)
	}
	c.WAN.Order = order
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
	if err := c.validateLAN(); err != nil {
		return err
	}
	if err := c.validateWifi(); err != nil {
		return err
	}
	if err := c.validateWAN(); err != nil {
		return err
	}
	if err := c.validateRadios(); err != nil {
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
	if err := c.validateSpeedTest(); err != nil {
		return err
	}
	return c.validatePolicy()
}

func (c *Config) validateSpeedTest() error {
	for what, raw := range map[string]string{"download": c.SpeedTest.DownloadURL, "upload": c.SpeedTest.UploadURL} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || len(raw) > 512 || !printable(raw) {
			return fmt.Errorf("The speed test's %s address has to start with http:// or https://.", what)
		}
	}
	return nil
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
		return fmt.Errorf("The router's name needs 1 to 20 characters.")
	}
	if s.Country != "" && !reCountry.MatchString(s.Country) {
		return fmt.Errorf("Choose the country the router is in.")
	}
	if len(s.SSH.AuthorizedKeys) > 8 {
		return fmt.Errorf("That is more SSH keys than the router keeps.")
	}
	for _, k := range s.SSH.AuthorizedKeys {
		if len(k) > 2048 || !printable(k) || !(strings.HasPrefix(k, "ssh-") || strings.HasPrefix(k, "ecdsa-") || strings.HasPrefix(k, "sk-")) {
			return fmt.Errorf("That is not an SSH public key.")
		}
	}
	if s.SSH.Enabled && len(s.SSH.AuthorizedKeys) == 0 {
		return fmt.Errorf("Add a key before allowing SSH.")
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
		if err := e.IP.validate(e.Name, len(c.DNS.Upstreams) > 0); err != nil {
			return err
		}
		if c.Network(e.Network) == nil {
			return fmt.Errorf("%s: belongs to a network that does not exist", e.Name)
		}
	}
	return nil
}

func (c *Config) validateWifi() error {
	if len(c.Wifi) == 0 || c.Wifi[0].ID != MainSSID {
		return fmt.Errorf("the main Wi-Fi network is missing")
	}
	if len(c.Wifi) > MaxSSIDs {
		return fmt.Errorf("The router has room for %d Wi-Fi networks.", MaxSSIDs)
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for i, a := range c.Wifi {
		if a.ID != MainSSID && !reID.MatchString(a.ID) {
			return fmt.Errorf("invalid Wi-Fi network id %q", a.ID)
		}
		if ids[a.ID] {
			return fmt.Errorf("duplicate Wi-Fi network id %q", a.ID)
		}
		ids[a.ID] = true
		// Only the main one may be unnamed: that is a router that has not
		// been set up yet.
		if a.SSID == "" && i > 0 {
			return fmt.Errorf("A Wi-Fi network needs a name.")
		}
		if len(a.SSID) > 32 || !printable(a.SSID) {
			return fmt.Errorf("A Wi-Fi name can be 32 characters long at most, fewer with accents or emoji.")
		}
		if a.Passphrase == "" && i > 0 {
			return fmt.Errorf("“%s” needs a password.", a.SSID)
		}
		if a.Passphrase != "" {
			if len(a.Passphrase) < 8 || len(a.Passphrase) > 63 || !asciiPrintable(a.Passphrase) {
				return fmt.Errorf("A Wi-Fi password needs 8 to 63 characters, without accents or emoji.")
			}
		}
		switch a.Security {
		case SecWPA2, SecWPA2WPA3, SecWPA3:
		default:
			return fmt.Errorf("invalid Wi-Fi security %q", a.Security)
		}
		if c.Network(a.Network) == nil {
			return fmt.Errorf("%q belongs to a network that does not exist", a.SSID)
		}
		if a.SSID != "" && names[a.SSID] {
			return fmt.Errorf("Two of your Wi-Fi networks are named “%s”.", a.SSID)
		}
		names[a.SSID] = true
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
		if err != nil || len(hw) != 6 || hw[0]&1 != 0 || hw.String() == "00:00:00:00:00:00" {
			return fmt.Errorf("%s: that is not a hardware address.", what)
		}
	default:
		return fmt.Errorf("%s: invalid mode %q", what, p.Mode)
	}
	return nil
}

// normalize puts an address setup into its canonical form: a mode is
// always named, addresses are written the standard way, and what a family
// set to automatic does not use is cleared.
func (s *IPSetup) normalize() {
	for _, f := range []*IPFamily{&s.V4, &s.V6} {
		f.Mode = strings.ToLower(strings.TrimSpace(f.Mode))
		if f.Mode == "" {
			f.Mode = IPAuto
		}
		if f.Mode == IPAuto {
			f.Address, f.Gateway, f.DNS = "", "", []string{}
			continue
		}
		f.Address = strings.TrimSpace(f.Address)
		if p, err := netip.ParsePrefix(f.Address); err == nil {
			f.Address = p.String()
		}
		f.Gateway = strings.TrimSpace(f.Gateway)
		if a, err := netip.ParseAddr(f.Gateway); err == nil && a.Zone() == "" {
			f.Gateway = a.String()
		}
		dns := []string{}
		for _, d := range f.DNS {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			if a, err := netip.ParseAddr(d); err == nil && a.Zone() == "" {
				d = a.String()
			}
			dns = append(dns, d)
		}
		f.DNS = dns
	}
}

// validate checks an address setup. what names the port or network for
// messages; haveDNS says that DNS servers are configured elsewhere, so a
// static IPv4 setup need not bring its own.
func (s IPSetup) validate(what string, haveDNS bool) error {
	if err := s.V4.validate(what, false, haveDNS); err != nil {
		return err
	}
	return s.V6.validate(what, true, true)
}

func (f IPFamily) validate(what string, v6, dnsOptional bool) error {
	name := "IPv4"
	if v6 {
		name = "IPv6"
	}
	switch f.Mode {
	case IPAuto, "":
		return nil
	case IPStatic:
	default:
		return fmt.Errorf("%s: %s is either automatic or static", what, name)
	}
	p, err := netip.ParsePrefix(f.Address)
	if err != nil || p.Addr().Is6() != v6 || p.Addr().Is4In6() {
		if v6 {
			return fmt.Errorf("%s: the IPv6 address needs its prefix length, like 2001:db8::10/64", what)
		}
		return fmt.Errorf("%s: the IPv4 address needs its prefix length, like 192.168.1.50/24", what)
	}
	a := p.Addr()
	if a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.IsLinkLocalUnicast() {
		return fmt.Errorf("%s: %s cannot be the router's address on that network", what, a)
	}
	if v6 {
		if p.Bits() < 16 || p.Bits() > 128 {
			return fmt.Errorf("%s: IPv6 prefix length must be 16 to 128", what)
		}
	} else {
		if p.Bits() < 8 || p.Bits() > 31 {
			return fmt.Errorf("%s: IPv4 prefix length must be 8 to 31", what)
		}
		if p.Bits() < 31 {
			// Not the network's own address, nor its broadcast address.
			host := binary.BigEndian.Uint32(a.AsSlice()) & (1<<(32-p.Bits()) - 1)
			if host == 0 || host == 1<<(32-p.Bits())-1 {
				return fmt.Errorf("%s: %s is the first or last address of %s, which no device can have", what, a, p.Masked())
			}
		}
	}
	gw, err := netip.ParseAddr(f.Gateway)
	if err != nil || gw.Is6() != v6 || gw.Is4In6() || gw.Zone() != "" || gw.IsUnspecified() || gw.IsMulticast() || gw.IsLoopback() {
		return fmt.Errorf("%s: %s needs a gateway address", what, name)
	}
	if gw == a {
		return fmt.Errorf("%s: the %s gateway cannot be the router's own address", what, name)
	}
	// The gateway has to be a neighbour: inside the uplink's own network
	// or, for IPv6, a link-local address.
	if !p.Masked().Contains(gw) && !(v6 && gw.IsLinkLocalUnicast()) {
		return fmt.Errorf("%s: the gateway %s is outside %s", what, gw, p.Masked())
	}
	if len(f.DNS) > 3 {
		return fmt.Errorf("%s: at most 3 %s DNS servers", what, name)
	}
	for _, d := range f.DNS {
		s, err := netip.ParseAddr(d)
		if err != nil || s.Is6() != v6 || s.Is4In6() || s.Zone() != "" || s.IsUnspecified() || s.IsMulticast() {
			return fmt.Errorf("%s: %q is not an %s DNS server address", what, d, name)
		}
	}
	if len(f.DNS) == 0 && !dnsOptional {
		return fmt.Errorf("%s: a fixed IPv4 address needs a DNS server, here or under Settings, DNS", what)
	}
	return nil
}

func (c *Config) validateWAN() error {
	w := c.WAN
	if len(w.Networks) > 64 {
		return fmt.Errorf("That is more saved networks than the router keeps.")
	}
	seen := map[string]bool{}
	for _, n := range w.Networks {
		if n.SSID == "" || len(n.SSID) > 32 {
			return fmt.Errorf("A Wi-Fi name can be 32 characters long at most, fewer with accents or emoji.")
		}
		if seen[n.SSID] {
			return fmt.Errorf("“%s” is already saved.", n.SSID)
		}
		seen[n.SSID] = true
		if n.PSK != "" && (len(n.PSK) < 8 || len(n.PSK) > 63 || !asciiPrintable(n.PSK)) {
			return fmt.Errorf("The password for “%s” needs 8 to 63 characters.", n.SSID)
		}
		if n.Priority < 0 || n.Priority > 1000 {
			return fmt.Errorf("saved network %q: invalid priority", n.SSID)
		}
		if err := n.IP.validate(n.SSID, len(c.DNS.Upstreams) > 0); err != nil {
			return err
		}
	}
	if err := validMACPolicy(w.WifiMAC, "wan.wifi_mac"); err != nil {
		return err
	}
	if err := validMACPolicy(w.EthMAC, "wan.eth_mac"); err != nil {
		return err
	}
	seenKey := map[string]bool{}
	for _, k := range w.Order {
		name, wired := EthOfUplink(k)
		if seenKey[k] || (wired && !reIface.MatchString(name)) || (!wired && k != UplinkWifi && k != UplinkPhone) {
			return fmt.Errorf("the order of uplinks is not valid")
		}
		seenKey[k] = true
	}
	u, err := url.Parse(w.Probe.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		return fmt.Errorf("probe URL must be a plain http:// URL")
	}
	if w.Probe.Expect == "" || len(w.Probe.Expect) > 128 {
		return fmt.Errorf("probe needs an expected response text")
	}
	return nil
}

func (c *Config) validateLAN() error {
	l := c.LAN
	if l.LeaseMinutes < 2 || l.LeaseMinutes > 7*24*60 {
		return fmt.Errorf("Addresses can be kept for between 2 minutes and 7 days.")
	}
	if !validDomain(l.Domain) {
		return fmt.Errorf("The local domain is not a valid domain name.")
	}
	if len(c.Networks) == 0 || c.Networks[0].ID != MainNetwork {
		return fmt.Errorf("the main network is missing")
	}
	if len(c.Networks) > MaxNetworks {
		return fmt.Errorf("The router has room for %d networks.", MaxNetworks)
	}
	ids := map[string]bool{}
	var v4, v6 []netip.Prefix
	for _, n := range c.Networks {
		if !reID.MatchString(n.ID) {
			return fmt.Errorf("invalid network id %q", n.ID)
		}
		if ids[n.ID] {
			return fmt.Errorf("duplicate network id %q", n.ID)
		}
		ids[n.ID] = true
		if n.Name == "" || len(n.Name) > 20 || !printable(n.Name) {
			return fmt.Errorf("A network's name needs 1 to 20 characters.")
		}
		p, err := netip.ParsePrefix(n.Address)
		if err != nil || !p.Addr().Is4() {
			return fmt.Errorf("%s: the address must be an IPv4 address with prefix, like 10.83.0.1/24", n.Name)
		}
		if p.Bits() < 16 || p.Bits() > 28 {
			return fmt.Errorf("%s: the prefix must be between /16 and /28", n.Name)
		}
		if !p.Addr().IsPrivate() {
			return fmt.Errorf("%s: the address must be a private one (10.x.x.x, 172.16 to 172.31.x.x or 192.168.x.x)", n.Name)
		}
		if p.Addr() == p.Masked().Addr() || p.Addr() == broadcast(p) {
			return fmt.Errorf("%s: the address must be a host address, not the first or last of its range", n.Name)
		}
		u, err := netip.ParsePrefix(n.ULAPrefix)
		if err != nil || !u.Addr().Is6() || u.Bits() != 64 {
			return fmt.Errorf("%s: the IPv6 prefix must be a /64", n.Name)
		}
		if !netip.MustParsePrefix("fc00::/7").Contains(u.Addr()) {
			return fmt.Errorf("%s: the IPv6 prefix must be inside fc00::/7", n.Name)
		}
		for _, o := range v4 {
			if o.Overlaps(p.Masked()) {
				return fmt.Errorf("%s: %s overlaps another network's addresses (%s)", n.Name, p.Masked(), o)
			}
		}
		for _, o := range v6 {
			if o.Overlaps(u.Masked()) {
				return fmt.Errorf("%s: its IPv6 prefix is also another network's", n.Name)
			}
		}
		v4, v6 = append(v4, p.Masked()), append(v6, u.Masked())
		if err := c.validAction(n.Route, true); err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
	}
	return nil
}

// Consistent checks that the configuration describes a router that can
// work as a whole, beyond every field being well formed. It is applied to
// changes, not to what is already stored: a stored configuration is never
// thrown away for being impractical.
func (c *Config) Consistent() error {
	uplinks, broadcasting := 0, 0
	for _, r := range c.Radios {
		switch r.Role {
		case RoleWAN:
			uplinks++
		case RoleAPWAN:
			uplinks++
			broadcasting++
		case RoleAP:
			broadcasting++
		}
	}
	if uplinks > 1 {
		return fmt.Errorf("Only one radio can join another Wi-Fi network. Use the other for your Wi-Fi, or turn it off.")
	}
	if !c.Ready() {
		return nil
	}
	enabled := 0
	for _, w := range c.Wifi {
		if w.Enabled {
			enabled++
		}
	}
	wired := 0
	for _, e := range c.Ethernet {
		if e.Role == EthLAN {
			wired++
		}
	}
	if wired == 0 && len(c.Radios) > 0 {
		if broadcasting == 0 {
			return fmt.Errorf("No radio would be left to broadcast your Wi-Fi. Keep one for it, or let the radio that joins do both.")
		}
		if enabled == 0 {
			return fmt.Errorf("Every Wi-Fi network is switched off and no wired port is set up for your devices. Nothing could connect to the router.")
		}
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
		return fmt.Errorf("The router takes 8 DNS servers at most.")
	}
	for _, u := range c.DNS.Upstreams {
		if _, err := ParseUpstream(u); err != nil {
			return err
		}
	}
	if len(c.DNS.Blocked) > 5000 {
		return fmt.Errorf("That is more blocked domains than the router keeps.")
	}
	for _, d := range c.DNS.Blocked {
		if !validDomain(d) {
			return fmt.Errorf("“%s” is not a domain name.", d)
		}
	}
	ab := c.DNS.Adblock
	if ab.Enabled && len(ab.Lists) == 0 {
		return fmt.Errorf("Ad blocking needs at least one list.")
	}
	if len(ab.Lists) > 16 {
		return fmt.Errorf("The router takes 16 lists at most.")
	}
	seen := map[string]bool{}
	for _, raw := range ab.Lists {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || len(raw) > 512 || !printable(raw) {
			return fmt.Errorf("“%s” is not a web address starting with http:// or https://.", raw)
		}
		if seen[raw] {
			return fmt.Errorf("“%s” is already on the list.", raw)
		}
		seen[raw] = true
	}
	if len(ab.Allow) > 1000 {
		return fmt.Errorf("That is more allowed domains than the router keeps.")
	}
	for _, d := range ab.Allow {
		if !validDomain(d) {
			return fmt.Errorf("“%s” is not a domain name.", d)
		}
	}
	return nil
}

func validWGKey(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

var reRate = regexp.MustCompile(`^[0-9]+(\.[0-9]+)? ?[kmg]?(bit|bits|bps)?(/s)?$`)

func (m MPQUIC) validate(name string) error {
	if len(m.Servers) == 0 || len(m.Servers) > 8 {
		return fmt.Errorf("VPN %q: needs 1-8 server addresses", name)
	}
	for _, s := range m.Servers {
		host := s
		if h, port, err := net.SplitHostPort(s); err == nil {
			host = h
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("VPN %q: bad port in %q", name, s)
			}
		} else if strings.Count(s, ":") == 1 {
			return fmt.Errorf("VPN %q: bad server address %q", name, s)
		}
		host = strings.Trim(host, "[]")
		if _, err := netip.ParseAddr(host); err != nil && !validDomain(strings.ToLower(host)) {
			return fmt.Errorf("VPN %q: bad server address %q", name, s)
		}
	}
	if m.Port < 1 || m.Port > 65535 {
		return fmt.Errorf("VPN %q: bad port", name)
	}
	if len(m.Password) < 8 || len(m.Password) > 256 || !printable(m.Password) {
		return fmt.Errorf("VPN %q: the password needs 8 to 256 characters", name)
	}
	if len(m.DNS) > 4 {
		return fmt.Errorf("VPN %q: too many DNS servers", name)
	}
	for _, d := range m.DNS {
		if _, err := netip.ParseAddr(d); err != nil {
			return fmt.Errorf("VPN %q: DNS server %q is not an IP address", name, d)
		}
	}
	for _, r := range []string{m.Up, m.Down} {
		if r != "" && !reRate.MatchString(r) {
			return fmt.Errorf("VPN %q: a rate looks like 20mbit, not %q", name, r)
		}
	}
	return nil
}

func (c *Config) validateVPNs() error {
	if len(c.VPNs) > 8 {
		return fmt.Errorf("The router has room for 8 VPNs.")
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
			return fmt.Errorf("A VPN's name needs 1 to 40 characters.")
		}
		if v.Type == VPNMPQUIC {
			if err := v.MPQUIC.validate(v.Name); err != nil {
				return err
			}
			continue
		}
		if v.Type != VPNWireGuard && v.Type != "" {
			return fmt.Errorf("VPN %q: unknown type %q", v.Name, v.Type)
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

// broadcast returns the last address of an IPv4 range.
func broadcast(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	for bit := p.Bits(); bit < 32; bit++ {
		a[bit/8] |= 0x80 >> (bit % 8)
	}
	return netip.AddrFrom4(a)
}

func (c *Config) validateDevices() error {
	if len(c.Devices) > 512 {
		return fmt.Errorf("too many devices")
	}
	if len(c.Radios) > 16 || len(c.Ethernet) > 32 {
		return fmt.Errorf("too many radios or ports")
	}
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
			return fmt.Errorf("A device's name can be 40 characters long at most.")
		}
		if err := c.validAction(d.Route, true); err != nil {
			return fmt.Errorf("device %s: %w", d.MAC, err)
		}
		if d.IP != "" {
			a, err := netip.ParseAddr(d.IP)
			var home *Network
			if err == nil && a.Is4() {
				home = c.NetworkOf(a)
			}
			if home == nil {
				return fmt.Errorf("The fixed address %s is not inside any of your networks.", d.IP)
			}
			if router, _ := home.Prefix(); a == router {
				return fmt.Errorf("%s is the router's own address.", d.IP)
			}
			if _, p := home.Prefix(); a == p.Addr() || a == broadcast(p) {
				return fmt.Errorf("%s is the first or last address of its range, which no device can have.", d.IP)
			}
			if ips[d.IP] {
				return fmt.Errorf("Two devices have the fixed address %s.", d.IP)
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
		return fmt.Errorf("The router has room for 100 rules.")
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
			return fmt.Errorf("A rule's name can be 40 characters long at most.")
		}
		if err := c.validAction(r.Action, false); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
		for _, id := range r.Src.Networks {
			if c.Network(id) == nil {
				return fmt.Errorf("The rule “%s” applies to a network that no longer exists.", r.Name)
			}
		}
		if len(r.Src.Networks) > MaxNetworks || len(r.Src.Devices) > 64 || len(r.Src.CIDRs) > 64 || len(r.Dst.Domains) > 256 || len(r.Dst.CIDRs) > 256 || len(r.Dst.Ports) > 32 || len(r.Dst.Countries) > 64 {
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
		for _, cc := range r.Dst.Countries {
			if !reCountry.MatchString(cc) {
				return fmt.Errorf("rule %q: %q is not a two-letter country code", r.Name, cc)
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
