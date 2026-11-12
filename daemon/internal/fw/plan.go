// Package fw turns the router configuration into an nftables ruleset and
// the matching policy-routing state.
//
// Every connection from the LAN is classified once, in the mangle
// prerouting hook, and tagged with a mark that selects how it leaves:
//
//	MarkDirect   main routing table (the active uplink)
//	MarkBlock    rejected in the forward hook
//	MarkVPN(i)   routing table TableVPN(i), which holds the tunnel's routes
//
// The mark is stored in the conntrack entry so later packets of the flow
// skip classification. Whenever the classification inputs change the
// conntrack table is flushed, so flows are never left on a stale path.
package fw

import (
	"net/netip"
	"sort"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

const (
	ClassMask  uint32 = 0xff
	MarkDirect uint32 = 0x01
	MarkBlock  uint32 = 0x02
	// MarkPortal is OR'd onto MarkDirect for a device that is allowed
	// straight out while the uplink sits behind a captive portal.
	MarkPortal uint32 = 0x100

	LANBridge = "br-lan"
	Table     = "porter"

	// Rule priorities and routing tables.
	// PrefOwnBase: what the router itself sends for a tunnel (see
	// syncRules), ahead of everything else.
	PrefOwnBase  = 60
	PrefMain     = 100
	PrefVPNBase  = 110
	PrefUplink   = 140
	TableVPNBase = 110
	TableUplink  = 105
)

// Bridge is the bridge interface of one of the router's networks. The
// main network's is LANBridge.
func Bridge(networkID string) string { return "br-" + networkID }

func MarkVPN(index int) uint32 { return 0x10 + uint32(index) }
func TableVPN(index int) int   { return TableVPNBase + index }

// VPNIfName is the WireGuard interface for a VPN profile.
func VPNIfName(id string) string { return "wg-" + id }

// Plan is everything the ruleset depends on.
type Plan struct {
	WANIfs []string
	// LANs are the router's own networks, the main one first.
	LANs []PlanLAN
	VPNs []PlanVPN
	SSH  bool
	// Control: the app may connect over the router's own networks.
	Control    bool
	Intercept  bool
	Paused     []string // MACs
	PortalDevs []string // MACs allowed to bypass policy while captive
	Rules      []PlanRule
	// DeviceMarks holds per-device route overrides.
	DeviceMarks map[string]uint32
	DefaultMark uint32
	// Learned holds addresses resolved for domain rules, per set name.
	Learned map[string][]Elem
	// Geo returns a country's address ranges in nftables notation, and
	// GeoVersion identifies the database they come from.
	Geo        func(country string) (v4, v6 []string)
	GeoVersion string
}

// PlanLAN is one of the router's networks as the ruleset sees it.
type PlanLAN struct {
	If         string
	Net4, Net6 netip.Prefix // Net6 is the zero value without IPv6
	// Isolated networks cannot open connections to the others.
	Isolated bool
	// Mark is the network's own route, 0 for none.
	Mark uint32
}

type PlanVPN struct {
	ID         string
	If         string
	Mark       uint32
	KillSwitch bool
	MTU        int
	// Active is false for a profile that is switched off.
	Active bool
}

type PlanRule struct {
	ID      string
	SrcMACs []string
	// SrcIfs are the bridges of the networks the rule applies to.
	SrcIfs     []string
	Src4, Src6 []netip.Prefix
	Dst4, Dst6 []netip.Prefix
	Countries  []string
	Domains    bool
	Proto      string
	Ports      []config.PortRange
	Mark       uint32
}

// Elem is a learned address with its remaining lifetime in seconds.
type Elem struct {
	Addr netip.Addr
	TTL  int
}

// Set names for the addresses a rule matches.
func SetCIDR(ruleID string, v6 bool) string { return "r_" + ruleID + "_c" + fam(v6) }
func SetName(ruleID string, v6 bool) string { return "r_" + ruleID + "_n" + fam(v6) }
func SetGeo(ruleID string, v6 bool) string  { return "r_" + ruleID + "_g" + fam(v6) }

func fam(v6 bool) string {
	if v6 {
		return "6"
	}
	return "4"
}

// MarkFor maps a config action to a mark. An action naming a VPN that does
// not exist blocks, so a dangling reference can never open a path.
func MarkFor(c *config.Config, action string) uint32 {
	switch action {
	case config.ActDirect:
		return MarkDirect
	case config.ActBlock:
		return MarkBlock
	}
	if id, ok := config.VPNOf(action); ok {
		for i, v := range c.VPNs {
			if v.ID == id {
				return MarkVPN(i)
			}
		}
	}
	return MarkBlock
}

// Runtime is the non-config input to a plan.
type Runtime struct {
	WANIfs     []string
	PortalDevs []string
	Learned    map[string][]Elem
	Geo        func(country string) (v4, v6 []string)
	GeoVersion string
}

// BuildPlan compiles the configuration into a Plan.
func BuildPlan(c *config.Config, rt Runtime) *Plan {
	p := &Plan{
		WANIfs:      append([]string(nil), rt.WANIfs...),
		SSH:         c.System.SSH.Enabled,
		Control:     !c.System.BluetoothOnly,
		Intercept:   c.DNS.Intercept,
		PortalDevs:  append([]string(nil), rt.PortalDevs...),
		DeviceMarks: map[string]uint32{},
		DefaultMark: MarkFor(c, c.Policy.Default),
		Learned:     rt.Learned,
		Geo:         rt.Geo,
		GeoVersion:  rt.GeoVersion,
	}
	sort.Strings(p.WANIfs)
	sort.Strings(p.PortalDevs)
	for _, n := range c.Networks {
		l := PlanLAN{If: Bridge(n.ID), Isolated: n.Isolated}
		_, l.Net4 = n.Prefix()
		if c.LAN.IPv6 {
			_, l.Net6 = n.ULA()
		}
		if n.Route != "" {
			l.Mark = MarkFor(c, n.Route)
		}
		p.LANs = append(p.LANs, l)
	}
	for i, v := range c.VPNs {
		mtu := v.WireGuard.MTU
		if mtu == 0 {
			mtu = 1420
		}
		// A switched-off profile is treated like a tunnel that is down: it
		// has no interface, and its kill switch still applies.
		p.VPNs = append(p.VPNs, PlanVPN{
			ID: v.ID, If: VPNIfName(v.ID), Mark: MarkVPN(i), KillSwitch: v.KillSwitch, MTU: mtu, Active: v.Enabled,
		})
	}
	for _, d := range c.Devices {
		if d.Paused {
			p.Paused = append(p.Paused, d.MAC)
		}
		if d.Route != "" {
			p.DeviceMarks[d.MAC] = MarkFor(c, d.Route)
		}
	}
	sort.Strings(p.Paused)
	for _, r := range c.Policy.Rules {
		if !r.Enabled {
			continue
		}
		pr := PlanRule{
			ID:        r.ID,
			SrcMACs:   append([]string(nil), r.Src.Devices...),
			Domains:   len(r.Dst.Domains) > 0,
			Countries: append([]string(nil), r.Dst.Countries...),
			Proto:     r.Dst.Proto,
			Mark:      MarkFor(c, r.Action),
		}
		for _, id := range r.Src.Networks {
			pr.SrcIfs = append(pr.SrcIfs, Bridge(id))
		}
		for _, s := range r.Src.CIDRs {
			pf, _ := config.ParsePrefixLoose(s)
			pf = pf.Masked()
			if pf.Addr().Is4() {
				pr.Src4 = append(pr.Src4, pf)
			} else {
				pr.Src6 = append(pr.Src6, pf)
			}
		}
		for _, s := range r.Dst.CIDRs {
			pf, _ := config.ParsePrefixLoose(s)
			pf = pf.Masked()
			if pf.Addr().Is4() {
				pr.Dst4 = append(pr.Dst4, pf)
			} else {
				pr.Dst6 = append(pr.Dst6, pf)
			}
		}
		for _, s := range r.Dst.Ports {
			pt, _ := config.ParsePorts(s)
			pr.Ports = append(pr.Ports, pt)
		}
		p.Rules = append(p.Rules, pr)
	}
	return p
}
