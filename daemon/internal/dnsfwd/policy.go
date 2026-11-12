// Package dnsfwd is the router's DNS forwarder. It is deliberately part of
// the policy engine rather than a generic resolver: a query for a name
// leaves the router on the same path that traffic to that name will take,
// and the answers feed the nftables sets that domain rules match on.
package dnsfwd

import (
	"net/netip"
	"strings"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Policy is an immutable snapshot of everything the forwarder decides on.
type Policy struct {
	// Hosts maps lower-case names (bare and with the LAN domain) to
	// addresses; it carries the router's own name and DHCP host names.
	Hosts map[string][]netip.Addr
	// Reverse maps addresses back to names for PTR queries.
	Reverse map[netip.Addr]string

	Blocked SuffixSet
	// Adblock turns on block-list filtering (Hooks.AdBlocked), except for
	// names in AdAllow and devices in AdExempt.
	Adblock  bool
	AdAllow  SuffixSet
	AdExempt map[string]bool
	Rules    []Rule
	// Paused devices are refused outright: those paused themselves, and
	// those whose group is offline.
	Paused map[string]bool
	// DeviceRoute holds per-device actions, NetRoute those of the
	// router's networks (matched by the client's address); Default applies
	// otherwise.
	DeviceRoute map[string]string
	NetRoute    []NetRoute
	Default     string
	// PortalDevs resolve via the upstream network's own DNS so that a
	// captive portal's host names work.
	PortalDevs map[string]bool

	// Direct is used for traffic leaving via the WAN: the configured
	// upstreams, or the network-provided ones when none are configured.
	Direct []config.Upstream
	// DirectCustom reports that Direct came from configuration and is
	// therefore reachable through a tunnel as well.
	DirectCustom bool
	// WAN is always the network-provided resolver list.
	WAN []config.Upstream
	VPN map[string]VPNDNS
}

// NetRoute is the route of one of the router's networks.
type NetRoute struct {
	Prefixes []netip.Prefix
	Action   string
}

type VPNDNS struct {
	Mark    uint32
	Servers []config.Upstream
	// Up: the tunnel has a live session. KillSwitch: its traffic never
	// leaves any other way. Together they say where a query goes while
	// the tunnel is down, which has to be where the traffic would go.
	Up         bool
	KillSwitch bool
	// Public: the tunnel carries traffic to the internet at large, not
	// just to a few networks behind it, so a public resolver can be
	// reached through it.
	Public bool
}

// PublicProbe is an address a tunnel has to carry for Public to hold.
var PublicProbe = netip.MustParseAddr("1.1.1.1")

// tunnelFallback is asked, through the tunnel, when a VPN profile names
// no resolver and none is configured: the resolver the upstream network
// handed out is not reachable there, and asking it outside the tunnel
// would tell that network every name the device looks up.
var tunnelFallback = []config.Upstream{
	{Proto: "udp", Addr: netip.MustParseAddrPort("1.1.1.1:53")},
	{Proto: "udp", Addr: netip.MustParseAddrPort("9.9.9.9:53")},
}

type Rule struct {
	ID       string
	SrcAny   bool
	SrcMACs  map[string]bool
	SrcCIDRs []netip.Prefix
	// DstAny marks a rule with no destination constraint at all.
	DstAny  bool
	Domains SuffixSet
	Action  string
}

func (r *Rule) srcMatch(mac string, ip netip.Addr) bool {
	if r.SrcAny {
		return true
	}
	if mac != "" && r.SrcMACs[mac] {
		return true
	}
	for _, p := range r.SrcCIDRs {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// SuffixSet matches a name against a set of domains and all their
// subdomains.
type SuffixSet map[string]struct{}

func NewSuffixSet(domains []string) SuffixSet {
	if len(domains) == 0 {
		return nil
	}
	s := SuffixSet{}
	for _, d := range domains {
		s[d] = struct{}{}
	}
	return s
}

// Match reports whether name (lower case, no trailing dot) is in the set.
func (s SuffixSet) Match(name string) bool {
	if len(s) == 0 {
		return false
	}
	for {
		if _, ok := s[name]; ok {
			return true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return false
		}
		name = name[i+1:]
	}
}

// RulesFrom compiles the enabled policy rules. global holds, by network
// id, the /64 a network has of a prefix delegated to the router, where
// it has one: its devices ask from addresses in it as well.
func RulesFrom(c *config.Config, global map[string]netip.Prefix) []Rule {
	var out []Rule
	for _, r := range c.Policy.Rules {
		if !r.Enabled {
			continue
		}
		cr := Rule{
			ID:      r.ID,
			SrcAny:  r.Src.Any(),
			SrcMACs: map[string]bool{},
			DstAny:  r.Dst.Any(),
			Domains: NewSuffixSet(r.Dst.Domains),
			Action:  r.Action,
		}
		// The devices named one by one, and the members of the groups.
		for _, m := range c.SrcDevices(r.Src) {
			cr.SrcMACs[m] = true
		}
		for _, s := range r.Src.CIDRs {
			if p, err := config.ParsePrefixLoose(s); err == nil {
				cr.SrcCIDRs = append(cr.SrcCIDRs, p.Masked())
			}
		}
		// A network is, to the forwarder, the addresses its clients have.
		for _, id := range r.Src.Networks {
			if n := c.Network(id); n != nil {
				cr.SrcCIDRs = append(cr.SrcCIDRs, prefixesOf(*n, global)...)
			}
		}
		out = append(out, cr)
	}
	return out
}

func prefixesOf(n config.Network, global map[string]netip.Prefix) []netip.Prefix {
	_, v4 := n.Prefix()
	_, v6 := n.ULA()
	out := []netip.Prefix{v4, v6}
	if g, ok := global[n.ID]; ok && g.IsValid() {
		out = append(out, g.Masked())
	}
	return out
}

// SetDevices fills in what the policy says of single devices: which are
// cut off (held: those paused themselves and those whose group is offline
// right now, as sched.Evaluate has them), which route each has, of its own
// or from its group, and which skip ad blocking.
func (p *Policy) SetDevices(c *config.Config, held []string) {
	p.Paused = map[string]bool{}
	for _, mac := range held {
		p.Paused[mac] = true
	}
	p.DeviceRoute = c.DeviceRoutes()
	p.AdExempt = c.AdExempt()
}

// NetRoutesFrom lists the routes set on networks; global as for RulesFrom.
func NetRoutesFrom(c *config.Config, global map[string]netip.Prefix) []NetRoute {
	var out []NetRoute
	for _, n := range c.Networks {
		if n.Route != "" {
			out = append(out, NetRoute{Prefixes: prefixesOf(n, global), Action: n.Route})
		}
	}
	return out
}

// path is where a query is sent.
type path struct {
	kind    string // "direct", "wan", "vpn", "block", "refuse"
	vpn     string
	servers []config.Upstream
	mark    uint32
}

// decide picks the path for a query, mirroring the packet classifier: the
// first rule that would claim traffic from this client to this name wins,
// then the device's own route, then its network's, then the default.
func (p *Policy) decide(mac string, ip netip.Addr, name string) path {
	// Paused comes first, as in the packet classifier: a paused device
	// is not let through to sign in for everybody else.
	if mac != "" && p.Paused[mac] {
		return path{kind: "refuse"}
	}
	if mac != "" && p.PortalDevs[mac] {
		return path{kind: "wan", servers: p.WAN}
	}
	action := ""
	for i := range p.Rules {
		r := &p.Rules[i]
		if !r.srcMatch(mac, ip) {
			continue
		}
		if r.DstAny || r.Domains.Match(name) {
			action = r.Action
			break
		}
	}
	fromRule := action != ""
	if action == "" && mac != "" {
		action = p.DeviceRoute[mac]
	}
	for i := 0; action == "" && i < len(p.NetRoute); i++ {
		for _, pf := range p.NetRoute[i].Prefixes {
			if pf.Contains(ip) {
				action = p.NetRoute[i].Action
			}
		}
	}
	if action == "" {
		action = p.Default
	}
	switch action {
	case config.ActDirect:
		return path{kind: "direct", servers: p.Direct}
	case config.ActBlock:
		if fromRule {
			return path{kind: "block"}
		}
		// A device that is blocked outright still gets an answer for
		// nothing; refusing lets its resolver fail fast.
		return path{kind: "refuse"}
	}
	if id, ok := config.VPNOf(action); ok {
		v, ok := p.VPN[id]
		if !ok {
			return path{kind: "refuse"}
		}
		switch {
		case !v.Up && v.KillSwitch:
			// Nothing of this device's gets out; say so at once instead
			// of letting each lookup time out.
			return path{kind: "refuse"}
		case !v.Up:
			// Its traffic leaves directly for now, and so do its lookups.
			return path{kind: "direct", servers: p.Direct}
		case len(v.Servers) > 0:
			return path{kind: "vpn", vpn: id, servers: v.Servers, mark: v.Mark}
		case p.DirectCustom:
			// No resolver in the profile: send the configured upstreams
			// through the tunnel.
			return path{kind: "vpn", vpn: id, servers: p.Direct, mark: v.Mark}
		case v.Public:
			return path{kind: "vpn", vpn: id, servers: tunnelFallback, mark: v.Mark}
		default:
			// A tunnel to a few networks only, and no resolver named for
			// it: names are looked up the way everything else of the
			// device's that is not for those networks goes.
			return path{kind: "direct", servers: p.Direct}
		}
	}
	return path{kind: "refuse"}
}

// matchingRules lists the domain rules whose sets should learn the
// addresses of name.
func (p *Policy) matchingRules(name string) []string {
	var ids []string
	for i := range p.Rules {
		if p.Rules[i].Domains.Match(name) {
			ids = append(ids, p.Rules[i].ID)
		}
	}
	return ids
}
