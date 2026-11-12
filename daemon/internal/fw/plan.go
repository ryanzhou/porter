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

// ServerIf is the interface of the router's own WireGuard server. No
// profile's interface can have the name: those have a hyphen in theirs.
const ServerIf = "wgsrv"

// ServerMTU is what fits in a WireGuard packet on an Ethernet-sized path.
const ServerMTU = 1420

// Plan is everything the ruleset depends on.
type Plan struct {
	WANIfs []string
	// Delegated holds, by uplink interface, the IPv6 prefix that uplink's
	// network has delegated to the router. What leaves by that uplink
	// from an address in it keeps its address.
	Delegated map[string]netip.Prefix
	// WANMTU holds the MTU of the uplinks on which a packet has less room
	// than on Ethernet (a PPPoE session, mostly), by interface.
	WANMTU map[string]int
	// LANs are the router's own networks, the main one first.
	LANs []PlanLAN
	VPNs []PlanVPN
	SSH  bool
	// Control: the app may connect over the router's own networks.
	Control   bool
	Intercept bool
	// MDNS are the bridges of the networks between which multicast DNS is
	// relayed (see internal/mdns): the router listens to it there.
	MDNS []string
	// Paused are the devices that are cut off, by hardware address: those
	// paused themselves, and those whose group is offline.
	Paused     []string
	PortalDevs []string // MACs allowed to bypass policy while captive
	Rules      []PlanRule
	// DeviceMarks holds the route of each device that has one: its own,
	// or else its group's.
	DeviceMarks map[string]uint32
	DefaultMark uint32
	// Server is the router's own WireGuard server; nil while it is off.
	Server *PlanServer
	// Forwards are the ports let through from the internet to devices.
	Forwards []PlanForward
	// Learned holds addresses resolved for domain rules, per set name.
	Learned map[string][]Elem
	// Geo returns a country's address ranges in nftables notation, and
	// GeoVersion identifies the database they come from.
	Geo        func(country string) (v4, v6 []string)
	GeoVersion string
}

// PlanLAN is one of the router's networks as the ruleset sees it.
type PlanLAN struct {
	If string
	// Addr4 is the router's own address in Net4.
	Addr4      netip.Addr
	Net4, Net6 netip.Prefix // Net6 is the zero value without IPv6
	// Global is the network's /64 of the prefix delegated by the uplink
	// in use; the zero value when it has none.
	Global netip.Prefix
	// Isolated networks cannot open connections to the others.
	Isolated bool
	// Mark is the network's own route, 0 for none.
	Mark uint32
}

// PlanServer is the WireGuard server as the ruleset sees it: one more of
// the router's networks, not kept apart, whose devices are somewhere else.
type PlanServer struct {
	If         string
	Port       int
	Addr4      netip.Addr   // the router's own address in Net4
	Net4, Net6 netip.Prefix // Net6 is the zero value without IPv6
}

// PlanForward is one forwarded port.
type PlanForward struct {
	ID string
	// Proto is "tcp", "udp" or "both".
	Proto           string
	Outside, Inside config.PortRange
	// To4 is the device's address, where IPv4 connections are sent. To6
	// are the device's own IPv6 addresses out of a delegated prefix, to
	// which the port is opened as it is: there is nothing to translate.
	To4 netip.Addr
	To6 []netip.Addr
	// Restricted: only From4 and From6 may connect, and a family that
	// has none of its own in them not at all.
	Restricted   bool
	From4, From6 []netip.Prefix
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
	WANIfs []string
	// Delegated and WANMTU are as in Plan; Global holds each network's
	// /64 of a delegated prefix, by network id.
	Delegated map[string]netip.Prefix
	WANMTU    map[string]int
	Global    map[string]netip.Prefix
	// Addrs6 are the IPv6 addresses devices have been seen with, by
	// hardware address. A forwarded port is opened to those of them that
	// are in a network's /64 of a delegated prefix.
	Addrs6 map[string][]netip.Addr
	// Held are the devices that are cut off right now, by hardware
	// address (see sched.Evaluate): next to the devices the configuration
	// has paused, those whose group is offline at this hour.
	Held       []string
	PortalDevs []string
	Learned    map[string][]Elem
	Geo        func(country string) (v4, v6 []string)
	GeoVersion string
}

// BuildPlan compiles the configuration into a Plan.
func BuildPlan(c *config.Config, rt Runtime) *Plan {
	p := &Plan{
		WANIfs:      append([]string(nil), rt.WANIfs...),
		Delegated:   rt.Delegated,
		WANMTU:      rt.WANMTU,
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
	for id := range c.DiscoveryPeers() {
		p.MDNS = append(p.MDNS, Bridge(id))
	}
	sort.Strings(p.MDNS)
	for _, n := range c.Networks {
		l := PlanLAN{If: Bridge(n.ID), Isolated: n.Isolated}
		l.Addr4, l.Net4 = n.Prefix()
		if c.LAN.IPv6 {
			_, l.Net6 = n.ULA()
			l.Global = rt.Global[n.ID].Masked()
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
	if s := c.Inbound.Server; s.Enabled {
		ps := &PlanServer{If: ServerIf, Port: s.Port}
		ps.Addr4, ps.Net4 = s.Prefix()
		if c.LAN.IPv6 {
			_, ps.Net6 = s.ULA()
		}
		p.Server = ps
	}
	held := map[string]bool{}
	for _, mac := range rt.Held {
		held[mac] = true
	}
	for _, d := range c.Devices {
		if d.Paused {
			held[d.MAC] = true
		}
	}
	p.Paused = sortedNames(held)
	for _, f := range c.Inbound.Forwards {
		d := c.Device(f.Device)
		// A device that is cut off is cut off from the outside as well.
		if !f.Enabled || d == nil || held[d.MAC] {
			continue
		}
		to, err := netip.ParseAddr(d.IP)
		if err != nil {
			continue
		}
		pf := PlanForward{ID: f.ID, Proto: f.Proto, Outside: f.Range(), Inside: f.Inside(), To4: to, Restricted: len(f.From) > 0}
		for _, s := range f.From {
			if pfx, err := netip.ParsePrefix(s); err != nil {
				continue
			} else if pfx.Addr().Is4() {
				pf.From4 = append(pf.From4, pfx.Masked())
			} else {
				pf.From6 = append(pf.From6, pfx.Masked())
			}
		}
		for _, a := range rt.Addrs6[f.Device] {
			for _, l := range p.LANs {
				if l.Global.IsValid() && l.Global.Contains(a) {
					pf.To6 = append(pf.To6, a)
				}
			}
		}
		sort.Slice(pf.To6, func(i, j int) bool { return pf.To6[i].Less(pf.To6[j]) })
		p.Forwards = append(p.Forwards, pf)
	}
	for mac, route := range c.DeviceRoutes() {
		p.DeviceMarks[mac] = MarkFor(c, route)
	}
	for _, r := range c.Policy.Rules {
		if !r.Enabled {
			continue
		}
		pr := PlanRule{
			ID: r.ID,
			// The devices named one by one, and the members of the groups.
			SrcMACs:   c.SrcDevices(r.Src),
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
		// A rule for a group that has no members is a rule for nobody. Left
		// in, with no source to match, it would read as one for everybody.
		if !r.Src.Any() && len(pr.SrcMACs) == 0 && len(pr.SrcIfs) == 0 && len(pr.Src4) == 0 && len(pr.Src6) == 0 {
			continue
		}
		p.Rules = append(p.Rules, pr)
	}
	return p
}
