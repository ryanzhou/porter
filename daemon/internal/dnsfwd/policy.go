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
	Rules   []Rule
	// Paused devices are refused outright.
	Paused map[string]bool
	// DeviceRoute holds per-device actions; Default applies otherwise.
	DeviceRoute map[string]string
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

type VPNDNS struct {
	Mark    uint32
	Servers []config.Upstream
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

// RulesFrom compiles the enabled policy rules.
func RulesFrom(c *config.Config) []Rule {
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
		for _, m := range r.Src.Devices {
			cr.SrcMACs[m] = true
		}
		for _, s := range r.Src.CIDRs {
			if p, err := config.ParsePrefixLoose(s); err == nil {
				cr.SrcCIDRs = append(cr.SrcCIDRs, p.Masked())
			}
		}
		out = append(out, cr)
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
// then the device's own route, then the default.
func (p *Policy) decide(mac string, ip netip.Addr, name string) path {
	if mac != "" && p.PortalDevs[mac] {
		return path{kind: "wan", servers: p.WAN}
	}
	if mac != "" && p.Paused[mac] {
		return path{kind: "refuse"}
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
		case len(v.Servers) > 0:
			return path{kind: "vpn", vpn: id, servers: v.Servers, mark: v.Mark}
		case p.DirectCustom:
			// No resolver in the profile: send the configured upstreams
			// through the tunnel.
			return path{kind: "vpn", vpn: id, servers: p.Direct, mark: v.Mark}
		default:
			// Only the local network's resolver is known, and it is not
			// reachable through the tunnel.
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
