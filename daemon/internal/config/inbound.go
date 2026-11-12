package config

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
)

// Inbound is everything that may come in from an internet source, which
// otherwise is nothing: a WireGuard server for the owner's own devices
// while they are away, ports forwarded to devices at home, and a name
// kept pointing at the router's public address for both to be found by.
type Inbound struct {
	Server   WGServer      `json:"server"`
	Forwards []PortForward `json:"forwards"`
	DDNS     DDNS          `json:"ddns"`
	// CheckURL answers with the address a request reaches it from: either
	// nothing but the address, or lines of which one is "ip=<address>".
	// It is how the router learns its public IPv4 address, and so whether
	// it is behind another router's or a provider's address translation.
	// It is asked only while something above is switched on.
	CheckURL string `json:"check_url"`
}

// DefaultCheckURL is Cloudflare's, whom the router's speed test and its
// fallback resolver already talk to.
const DefaultCheckURL = "https://cloudflare.com/cdn-cgi/trace"

// InUse reports whether anything is set up to come in from outside.
func (in Inbound) InUse() bool {
	if in.Server.Enabled || in.DDNS.Enabled {
		return true
	}
	for _, f := range in.Forwards {
		if f.Enabled {
			return true
		}
	}
	return false
}

// DefaultServerPort is WireGuard's usual port.
const DefaultServerPort = 51820

// MaxServerPeers and MaxForwards limit how much can be configured.
const (
	MaxServerPeers = 32
	MaxForwards    = 32
)

// WGServer is the router's own WireGuard server: the way home for the
// owner's devices while they are somewhere else. It has an address range
// of its own, which the firewall treats as one more of the router's
// networks, one that is not kept apart.
type WGServer struct {
	Enabled bool `json:"enabled"`
	// Port is the UDP port it listens on, on every internet source.
	Port int `json:"port"`
	// PrivateKey is the server's own key, made by the router and kept:
	// every device's configuration names the public key that goes with it.
	PrivateKey string `json:"private_key"`
	// Address is the router's address in the server's range, with prefix,
	// like 10.89.0.1/24, and ULAPrefix the range's IPv6 /64. Both are
	// chosen by the router and stay: the devices' configurations carry
	// their addresses in them.
	Address   string         `json:"address"`
	ULAPrefix string         `json:"ula_prefix"`
	Peers     []WGServerPeer `json:"peers"`
}

// WGServerPeer is one device that may connect to the server. Its private
// key is never stored: the router makes it, hands it out once inside the
// device's configuration (see core's "server.peer_add") and keeps the
// public half.
type WGServerPeer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	// Address is the device's IPv4 address in the server's range. Its
	// IPv6 address is the same host number in the server's /64.
	Address string `json:"address"`
	Enabled bool   `json:"enabled"`
	// Full records what the device's configuration was made to send
	// through the router: everything, or only what is for the router's
	// own networks. The router itself does not act on it.
	Full bool `json:"full"`
}

// ServerSubnets are the address ranges the router gives its WireGuard
// server when left to choose, the first that overlaps none of its
// networks. They are not among Subnets, so that a network added later
// still gets the range it would have had.
var ServerSubnets = []string{"10.89.0.1/24", "172.23.89.1/24", "192.168.89.1/24", "10.90.0.1/24", "172.23.90.1/24"}

// Prefix returns the router's address in the server's range and the range.
func (s WGServer) Prefix() (netip.Addr, netip.Prefix) {
	p := netip.MustParsePrefix(s.Address)
	return p.Addr(), p.Masked()
}

// ULA returns the router's IPv6 address in the server's range (::1 in
// the /64) and the range.
func (s WGServer) ULA() (netip.Addr, netip.Prefix) {
	p := netip.MustParsePrefix(s.ULAPrefix).Masked()
	return hostIn(p, 1), p
}

// hostIn returns the address with the given host number in an IPv6 /64.
func hostIn(p netip.Prefix, host uint32) netip.Addr {
	a := p.Masked().Addr().As16()
	binary.BigEndian.PutUint32(a[12:], host)
	return netip.AddrFrom16(a)
}

// PeerAddrs returns a device's addresses: the IPv4 one it was given and,
// with v6, the one with the same host number in the server's /64.
func (s WGServer) PeerAddrs(p WGServerPeer, v6 bool) []netip.Prefix {
	a, err := netip.ParseAddr(p.Address)
	if err != nil || !a.Is4() {
		return nil
	}
	out := []netip.Prefix{netip.PrefixFrom(a, 32)}
	if v6 {
		_, v4 := s.Prefix()
		_, ula := s.ULA()
		host := binary.BigEndian.Uint32(a.AsSlice()) - binary.BigEndian.Uint32(v4.Addr().AsSlice())
		out = append(out, netip.PrefixFrom(hostIn(ula, host), 128))
	}
	return out
}

// FreeAddress returns the lowest address in the server's range that
// neither the router nor a device has.
func (s WGServer) FreeAddress() (string, bool) {
	router, p := s.Prefix()
	taken := map[netip.Addr]bool{router: true, broadcast(p): true}
	for _, peer := range s.Peers {
		if a, err := netip.ParseAddr(peer.Address); err == nil {
			taken[a] = true
		}
	}
	for a := p.Addr().Next(); p.Contains(a); a = a.Next() {
		if !taken[a] {
			return a.String(), true
		}
	}
	return "", false
}

// PublicKey returns the public half of the server's key.
func (s WGServer) PublicKey() string {
	pub, _ := WGPublicKey(s.PrivateKey)
	return pub
}

// newWGKey returns a new WireGuard private key, in base64.
func newWGKey() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	// The form every implementation brings a key into before using it.
	b[0] &= 248
	b[31] = (b[31] & 127) | 64
	return base64.StdEncoding.EncodeToString(b[:])
}

// WGPublicKey derives the public key of a WireGuard private key (base64).
func WGPublicKey(priv string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(priv)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("bad key")
	}
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// Protocols of a port forward.
const (
	ProtoTCP  = "tcp"
	ProtoUDP  = "udp"
	ProtoBoth = "both"
)

// PortForward lets connections from the internet through to one device.
type PortForward struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Proto is ProtoTCP, ProtoUDP or ProtoBoth.
	Proto string `json:"proto"`
	// Ports is the port on the outside, or a range of them: "8080",
	// "5000-5010".
	Ports string `json:"ports"`
	// Device is the hardware address of the device it goes to. The
	// device has a fixed address (Device.IP), which is where IPv4
	// connections are sent; over IPv6 nothing is translated, and the
	// port is opened to the addresses the device has at the time.
	Device string `json:"device"`
	// To is the port on the device, for a single port; 0 for the same.
	// A range keeps its numbers.
	To int `json:"to"`
	// From, when not empty, are the only address ranges connections are
	// let in from.
	From []string `json:"from"`
}

// Range returns the ports on the outside.
func (f PortForward) Range() PortRange {
	r, _ := ParsePorts(f.Ports)
	return r
}

// Inside returns the ports on the device.
func (f PortForward) Inside() PortRange {
	r := f.Range()
	if f.To != 0 && r.Lo == r.Hi {
		return PortRange{uint16(f.To), uint16(f.To)}
	}
	return r
}

// Carries reports whether the forward applies to a protocol.
func (f PortForward) Carries(proto string) bool { return f.Proto == proto || f.Proto == ProtoBoth }

// Dynamic DNS providers.
const (
	DDNSDyndns2    = "dyndns2"
	DDNSDuckDNS    = "duckdns"
	DDNSCloudflare = "cloudflare"
	DDNSCustom     = "custom"
)

// DDNS keeps one name pointing at the router's public address.
type DDNS struct {
	Enabled bool `json:"enabled"`
	// Provider is one of the DDNS constants.
	Provider string `json:"provider"`
	// Hostname is the name that is kept up to date, and what devices
	// outside are told to connect to: for DuckDNS "<name>.duckdns.org",
	// for Cloudflare the record's full name.
	Hostname string `json:"hostname"`
	// Server is the dyndns2 provider's update server, host or host:port.
	Server string `json:"server"`
	// Username is the dyndns2 user name; Password its password, or the
	// DuckDNS or Cloudflare token. Like every secret here it is stored as
	// it is and appears in no log and no status.
	Username string `json:"username"`
	Password string `json:"password"`
	// URL is fetched for DDNSCustom, with {ip} and {ip6} replaced by the
	// addresses. It may carry a secret and is treated as one.
	URL string `json:"url"`
	// IPv6 publishes the router's IPv6 address next to the IPv4 one.
	IPv6 bool `json:"ipv6"`
}

const duckDomain = ".duckdns.org"

// DuckName is the part of a DuckDNS name that is the owner's.
func (d DDNS) DuckName() string { return strings.TrimSuffix(d.Hostname, duckDomain) }

// freeOf returns the first candidate that overlaps none of the prefixes
// in use.
func freeOf(candidates []string, taken []netip.Prefix) (netip.Prefix, bool) {
	for _, s := range candidates {
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

// normalizeInbound fills in what the server is created without (its key,
// its ranges, its port) and brings what was typed into one form.
func (c *Config) normalizeInbound() {
	in := &c.Inbound
	s := &in.Server
	s.PrivateKey, s.Address, s.ULAPrefix = strings.TrimSpace(s.PrivateKey), strings.TrimSpace(s.Address), strings.TrimSpace(s.ULAPrefix)
	if s.Port == 0 {
		s.Port = DefaultServerPort
	}
	if s.PrivateKey == "" {
		s.PrivateKey = newWGKey()
	}
	if s.Address == "" {
		var taken []netip.Prefix
		for _, n := range c.Networks {
			if p, err := netip.ParsePrefix(n.Address); err == nil {
				taken = append(taken, p)
			}
		}
		if p, ok := freeOf(ServerSubnets, taken); ok {
			s.Address = p.String()
		}
	}
	if s.ULAPrefix == "" {
		s.ULAPrefix = randomULA()
	}
	for i := range s.Peers {
		p := &s.Peers[i]
		p.ID, p.Name, p.PublicKey, p.Address = strings.TrimSpace(p.ID), strings.TrimSpace(p.Name), strings.TrimSpace(p.PublicKey), strings.TrimSpace(p.Address)
		if p.ID == "" {
			p.ID = NewID()
		}
	}
	for i := range in.Forwards {
		f := &in.Forwards[i]
		f.ID, f.Name = strings.TrimSpace(f.ID), strings.TrimSpace(f.Name)
		if f.ID == "" {
			f.ID = NewID()
		}
		f.Proto = strings.ToLower(strings.TrimSpace(f.Proto))
		if f.Proto == "" {
			f.Proto = ProtoTCP
		}
		f.Ports = strings.ReplaceAll(f.Ports, " ", "")
		f.Device = strings.ToLower(strings.TrimSpace(f.Device))
		// "The same port" is said one way.
		if r, err := ParsePorts(f.Ports); err == nil && r.Lo == r.Hi && f.To == int(r.Lo) {
			f.To = 0
		}
		from := []string{}
		for _, s := range f.From {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if p, err := ParsePrefixLoose(s); err == nil {
				s = p.Masked().String()
			}
			from = append(from, s)
		}
		f.From = from
	}
	d := &in.DDNS
	d.Provider = strings.ToLower(strings.TrimSpace(d.Provider))
	d.Hostname = NormalizeDomain(d.Hostname)
	d.Server, d.Username, d.URL = strings.TrimSpace(d.Server), strings.TrimSpace(d.Username), strings.TrimSpace(d.URL)
	if d.Provider == DDNSDuckDNS && d.Hostname != "" && !strings.Contains(d.Hostname, ".") {
		d.Hostname += duckDomain
	}
	if in.CheckURL = strings.TrimSpace(in.CheckURL); in.CheckURL == "" {
		in.CheckURL = DefaultCheckURL
	}
}
