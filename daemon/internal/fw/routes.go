package fw

import (
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// VPNRoutes describes the routing table of one VPN profile.
type VPNRoutes struct {
	Index      int
	If         string
	Up         bool // tunnel has a live session
	KillSwitch bool
	Allowed    []netip.Prefix
}

const backstopMetric = 1 << 30

// UplinkOverride names the uplink in use, in a routing table of its own
// that is consulted before the main table's default routes. The main
// table is not left to choose, because what it chooses is not what the
// router means: it prefers by metric, not by the configured order; it
// goes on using a route with a fixed address after the cable is pulled;
// and it decides IPv4 and IPv6 separately, so that one family can follow
// an uplink that has been stepped past.
type UplinkOverride struct {
	If       string
	Gw4, Gw6 netip.Addr // invalid: no gateway known for that family
	V4, V6   bool       // the uplink has an address of that family
	// Direct4 and Direct6: the uplink's own default route for that
	// family has no gateway because the link has only one other end (a
	// PPPoE session), and everything is simply sent down it.
	Direct4, Direct6 bool
}

// SourceRoutes describes the routing table of one uplink: what is marked
// for that uplink follows it, as does what the router itself sends from
// the uplink's own addresses or from a socket bound to its interface.
type SourceRoutes struct {
	Index int
	// Out is the way out by the uplink, as for the uplink in use; nil
	// while the uplink is not connected or is not to be used. The table
	// then refuses everything.
	Out *UplinkOverride
	// Prefer: the uplink reaches the internet, so what merely prefers it
	// is sent there. Without it that traffic is left to the uplink in
	// use.
	Prefer bool
	// If is the uplink's interface and Addrs are its own addresses, for
	// what the router sends itself.
	If    string
	Addrs []netip.Addr
}

// String writes the routes out in full, the way out included, so that one
// state can be compared with the next.
func (s SourceRoutes) String() string {
	out := "none"
	if s.Out != nil {
		out = fmt.Sprintf("%+v", *s.Out)
	}
	return fmt.Sprintf("{%d %s prefer=%v %s %v}", s.Index, out, s.Prefer, s.If, s.Addrs)
}

// SyncRouting makes the policy-routing rules, the per-VPN tables, the
// uplink override (nil for none) and the tables of the uplinks match the
// given state. It only ever replaces and then prunes, so a table that must
// not leak never passes through an empty state.
func SyncRouting(vpns []VPNRoutes, uplink *UplinkOverride, sources []SourceRoutes) error {
	families := []int{unix.AF_INET}
	if HaveIPv6() {
		families = append(families, unix.AF_INET6)
	}
	for _, family := range families {
		if err := syncRules(family, vpns, sources); err != nil {
			return err
		}
		if err := syncDefault(family, TableUplink, uplink, false); err != nil {
			return fmt.Errorf("uplink override: %w", err)
		}
		for _, v := range vpns {
			if err := syncTable(family, v); err != nil {
				return fmt.Errorf("vpn table %d: %w", TableVPN(v.Index), err)
			}
		}
		listed := map[int]bool{}
		for _, s := range sources {
			listed[TableSource(s.Index)] = true
			// The backstop is what holds traffic that is for this uplink
			// alone while the uplink is away, and what refuses the address
			// family it does not have.
			if err := syncDefault(family, TableSource(s.Index), s.Out, true); err != nil {
				return fmt.Errorf("uplink table %d: %w", TableSource(s.Index), err)
			}
		}
		if err := pruneSourceTables(family, listed); err != nil {
			return err
		}
	}
	return nil
}

// pruneSourceTables empties the tables of uplinks that are no longer
// listed. No rule leads to them any more; this is tidiness.
func pruneSourceTables(family int, keep map[int]bool) error {
	all, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for i := range all {
		r := all[i]
		if r.Table < TableSourceBase || r.Table >= TableSourceBase+MaxSources || keep[r.Table] {
			continue
		}
		if err := netlink.RouteDel(&r); err != nil && !os.IsNotExist(err) && err != unix.ESRCH {
			return err
		}
	}
	return nil
}

func syncRules(family int, vpns []VPNRoutes, sources []SourceRoutes) error {
	type key struct {
		pref  int
		table int
		mark  uint32
		src   string
		oif   string
	}
	want := map[key]*netlink.Rule{}
	// Specific routes in the main table (the LAN, the uplink subnet) win
	// over a tunnel's default route.
	r := netlink.NewRule()
	r.Family = family
	r.Priority = PrefMain
	r.Table = unix.RT_TABLE_MAIN
	r.SuppressPrefixlen = 0
	want[key{pref: PrefMain, table: unix.RT_TABLE_MAIN}] = r
	// After the tunnels' tables and before the main table's default
	// route: the table that holds the uplink override, empty most of the
	// time.
	o := netlink.NewRule()
	o.Family = family
	o.Priority = PrefUplink
	o.Table = TableUplink
	want[key{pref: PrefUplink, table: TableUplink}] = o
	for _, v := range vpns {
		r := netlink.NewRule()
		r.Family = family
		r.Priority = PrefVPNBase + v.Index
		r.Table = TableVPN(v.Index)
		r.Mark = MarkVPN(v.Index)
		mask := ClassMask
		r.Mask = &mask
		want[key{pref: r.Priority, table: r.Table, mark: r.Mark}] = r
		// What the router sends itself for a tunnel (the lookups of the
		// devices routed there, mostly) goes by the tunnel's table before
		// the main table is looked at. Otherwise a tunnel's resolver
		// whose address happens to lie in the upstream network's range
		// (192.168.1.1 at home, and the hotel uses 192.168.1.0/24) would
		// be asked over the uplink, in the clear, or with a kill switch
		// not at all.
		o := netlink.NewRule()
		o.Family = family
		o.Priority = PrefOwnBase + v.Index
		o.Table = TableVPN(v.Index)
		o.Mark = MarkVPN(v.Index)
		o.Mask = &mask
		o.IifName = "lo"
		want[key{pref: o.Priority, table: o.Table, mark: o.Mark}] = o
	}
	for _, s := range sources {
		table := TableSource(s.Index)
		marked := func(mark uint32) {
			r := netlink.NewRule()
			r.Family = family
			r.Priority = PrefSource
			r.Table = table
			r.Mark = mark
			mask := ClassMask
			r.Mask = &mask
			want[key{pref: PrefSource, table: table, mark: mark}] = r
		}
		// What is for this uplink alone always goes by its table, which
		// refuses it while the uplink is away. What prefers the uplink
		// only while that is worth it: without this rule it is the
		// traffic of the uplink in use.
		marked(MarkOnly(s.Index))
		if s.Prefer {
			marked(MarkSource(s.Index))
		}
		// What the router sends from one of the uplink's own addresses is
		// an answer to something that arrived there (its WireGuard server
		// answering a device, a ping answered), and what it sends from a
		// socket bound to the interface is meant for that uplink (the
		// reachability check, a speed test, a lookup). Neither is left to
		// the uplink in use.
		for _, a := range s.Addrs {
			if a.Is6() != (family == unix.AF_INET6) || a.IsLinkLocalUnicast() {
				continue
			}
			r := netlink.NewRule()
			r.Family = family
			r.Priority = PrefSourceOwn
			r.Table = table
			r.Src = ipnet(netip.PrefixFrom(a, a.BitLen()))
			want[key{pref: PrefSourceOwn, table: table, src: r.Src.String()}] = r
		}
		if s.If != "" {
			r := netlink.NewRule()
			r.Family = family
			r.Priority = PrefSourceOwn
			r.Table = table
			r.OifName = s.If
			want[key{pref: PrefSourceOwn, table: table, oif: s.If}] = r
		}
	}
	have, err := netlink.RuleList(family)
	if err != nil {
		return err
	}
	for i := range have {
		h := have[i]
		if h.Priority < PrefOwnBase || h.Priority >= PrefVPNBase+32 {
			continue
		}
		k := key{pref: h.Priority, table: h.Table, mark: h.Mark, oif: h.OifName}
		if h.Src != nil {
			k.src = h.Src.String()
		}
		if _, ok := want[k]; ok {
			delete(want, k)
			continue
		}
		h.Family = family
		if err := netlink.RuleDel(&h); err != nil {
			return fmt.Errorf("rule del: %w", err)
		}
	}
	for _, r := range want {
		if err := netlink.RuleAdd(r); err != nil {
			return fmt.Errorf("rule add: %w", err)
		}
	}
	return nil
}

// syncDefault makes a table lead out by one uplink, for one address
// family. A family the uplink has a gateway for goes there, and one it
// sends straight down a point-to-point link goes to the link. A family it
// has no address for is refused, so that clients fall back to the other
// at once instead of waiting on another uplink's route. A family it has
// an address for but no known way out is left to the tables that follow.
// Without an uplink (o is nil) the table is empty. With backstop set the
// table always refuses what it does not carry: it is never empty, and
// nothing in it is left to the tables that follow.
func syncDefault(family, table int, o *UplinkOverride, backstop bool) error {
	v6 := family == unix.AF_INET6
	def := netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	if v6 {
		def = netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	}
	var want []netlink.Route
	if backstop {
		want = append(want, netlink.Route{Dst: ipnet(def), Table: table, Type: unix.RTN_UNREACHABLE, Priority: backstopMetric, Family: family})
	}
	if o != nil {
		gw, has, direct := o.Gw4, o.V4, o.Direct4
		if v6 {
			gw, has, direct = o.Gw6, o.V6, o.Direct6
		}
		switch {
		case !has:
			if !backstop {
				want = append(want, netlink.Route{Dst: ipnet(def), Table: table, Type: unix.RTN_UNREACHABLE, Family: family})
			}
		case gw.IsValid():
			link, err := netlink.LinkByName(o.If)
			if err != nil {
				return err
			}
			want = append(want, netlink.Route{LinkIndex: link.Attrs().Index, Dst: ipnet(def), Table: table, Family: family, Gw: gw.AsSlice()})
		case direct:
			link, err := netlink.LinkByName(o.If)
			if err != nil {
				return err
			}
			want = append(want, netlink.Route{LinkIndex: link.Attrs().Index, Dst: ipnet(def), Table: table, Family: family, Scope: netlink.SCOPE_LINK})
		}
	}
	for i := range want {
		if err := netlink.RouteReplace(&want[i]); err != nil {
			return err
		}
	}
	have, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for i := range have {
		h := have[i]
		keep := false
		for _, w := range want {
			// The kernel gives an IPv6 route a metric where none was
			// asked for, so only the backstop is told by its metric.
			if h.Type != routeType(w) || (h.Priority == backstopMetric) != (w.Priority == backstopMetric) {
				continue
			}
			keep = keep || h.Type == unix.RTN_UNREACHABLE || (h.LinkIndex == w.LinkIndex && h.Gw.Equal(w.Gw))
		}
		if keep {
			continue
		}
		if err := netlink.RouteDel(&h); err != nil && !os.IsNotExist(err) && err != unix.ESRCH {
			return err
		}
	}
	return nil
}

func syncTable(family int, v VPNRoutes) error {
	table := TableVPN(v.Index)
	v6 := family == unix.AF_INET6
	var want []netlink.Route
	covered := false
	if v.Up {
		link, err := netlink.LinkByName(v.If)
		if err != nil {
			return err
		}
		for _, p := range v.Allowed {
			if p.Addr().Is6() != v6 {
				continue
			}
			covered = true
			want = append(want, netlink.Route{
				LinkIndex: link.Attrs().Index, Dst: ipnet(p), Table: table, Scope: netlink.SCOPE_LINK, Family: family,
			})
		}
	}
	// The backstop catches whatever the tunnel does not carry: everything
	// while a kill-switched tunnel is down, and the address family a
	// running tunnel has no routes for (so IPv6 cannot slip past an
	// IPv4-only tunnel).
	if v.KillSwitch || (v.Up && !covered) {
		def := netip.PrefixFrom(netip.IPv4Unspecified(), 0)
		if v6 {
			def = netip.PrefixFrom(netip.IPv6Unspecified(), 0)
		}
		want = append(want, netlink.Route{
			Dst: ipnet(def), Table: table, Type: unix.RTN_UNREACHABLE, Priority: backstopMetric, Family: family,
		})
	}
	for i := range want {
		if err := netlink.RouteReplace(&want[i]); err != nil {
			return fmt.Errorf("route replace %v: %w", want[i].Dst, err)
		}
	}
	have, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for i := range have {
		h := have[i]
		keep := false
		for _, wnt := range want {
			if sameDst(h.Dst, wnt.Dst, v6) && h.Type == routeType(wnt) && (h.Type == unix.RTN_UNREACHABLE || h.LinkIndex == wnt.LinkIndex) {
				keep = true
				break
			}
		}
		if !keep {
			if err := netlink.RouteDel(&h); err != nil && !os.IsNotExist(err) && err != unix.ESRCH {
				return fmt.Errorf("route del %v: %w", h.Dst, err)
			}
		}
	}
	return nil
}

func routeType(r netlink.Route) int {
	if r.Type == 0 {
		return unix.RTN_UNICAST
	}
	return r.Type
}

func sameDst(a, b *net.IPNet, v6 bool) bool {
	norm := func(n *net.IPNet) string {
		if n == nil {
			if v6 {
				return "::/0"
			}
			return "0.0.0.0/0"
		}
		return n.String()
	}
	return norm(a) == norm(b)
}

func ipnet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

// HaveIPv6 reports whether the kernel has IPv6 enabled.
func HaveIPv6() bool {
	_, err := os.Stat("/proc/sys/net/ipv6/conf/all")
	return err == nil
}

// FlushConntrack drops all tracked connections so that every flow is
// classified afresh.
func FlushConntrack() error {
	if err := netlink.ConntrackTableFlush(netlink.ConntrackTable); err != nil {
		return err
	}
	return nil
}

// EnableForwarding sets the sysctls a router needs.
func EnableForwarding() error {
	ctl := map[string]string{
		"/proc/sys/net/ipv4/ip_forward":              "1",
		"/proc/sys/net/ipv4/conf/all/rp_filter":      "0",
		"/proc/sys/net/ipv4/conf/default/rp_filter":  "0",
		"/proc/sys/net/ipv4/conf/all/src_valid_mark": "1",
	}
	if HaveIPv6() {
		ctl["/proc/sys/net/ipv6/conf/all/forwarding"] = "1"
		ctl["/proc/sys/net/ipv6/conf/default/forwarding"] = "1"
	}
	for path, val := range ctl {
		if err := os.WriteFile(path, []byte(val), 0); err != nil {
			return fmt.Errorf("sysctl %s: %w", path, err)
		}
	}
	return nil
}
