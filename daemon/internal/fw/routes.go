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

// UplinkOverride sends everything that would leave by the main table's
// default route through another uplink instead. It is how the router
// steps past its best-ranked uplink when that one has an address but no
// way to the internet.
type UplinkOverride struct {
	If       string
	Gw4, Gw6 netip.Addr // invalid: none for that family
	V6       bool       // the uplink has IPv6 at all
}

// SyncRouting makes the policy-routing rules, the per-VPN tables and the
// uplink override (nil for none) match the given state. It only ever
// replaces and then prunes, so a table that must not leak never passes
// through an empty state.
func SyncRouting(vpns []VPNRoutes, uplink *UplinkOverride) error {
	families := []int{unix.AF_INET}
	if HaveIPv6() {
		families = append(families, unix.AF_INET6)
	}
	for _, family := range families {
		if err := syncRules(family, vpns); err != nil {
			return err
		}
		if err := syncUplink(family, uplink); err != nil {
			return fmt.Errorf("uplink override: %w", err)
		}
		for _, v := range vpns {
			if err := syncTable(family, v); err != nil {
				return fmt.Errorf("vpn table %d: %w", TableVPN(v.Index), err)
			}
		}
	}
	return nil
}

func syncRules(family int, vpns []VPNRoutes) error {
	type key struct {
		pref  int
		table int
		mark  uint32
	}
	want := map[key]*netlink.Rule{}
	// Specific routes in the main table (the LAN, the uplink subnet) win
	// over a tunnel's default route.
	r := netlink.NewRule()
	r.Family = family
	r.Priority = PrefMain
	r.Table = unix.RT_TABLE_MAIN
	r.SuppressPrefixlen = 0
	want[key{PrefMain, unix.RT_TABLE_MAIN, 0}] = r
	// After the tunnels' tables and before the main table's default
	// route: the table that holds the uplink override, empty most of the
	// time.
	o := netlink.NewRule()
	o.Family = family
	o.Priority = PrefUplink
	o.Table = TableUplink
	want[key{PrefUplink, TableUplink, 0}] = o
	for _, v := range vpns {
		r := netlink.NewRule()
		r.Family = family
		r.Priority = PrefVPNBase + v.Index
		r.Table = TableVPN(v.Index)
		r.Mark = MarkVPN(v.Index)
		mask := ClassMask
		r.Mask = &mask
		want[key{r.Priority, r.Table, r.Mark}] = r
	}
	have, err := netlink.RuleList(family)
	if err != nil {
		return err
	}
	for i := range have {
		h := have[i]
		if h.Priority < PrefMain || h.Priority >= PrefVPNBase+32 {
			continue
		}
		k := key{h.Priority, h.Table, h.Mark}
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

// syncUplink fills or empties the override table for one address family.
func syncUplink(family int, o *UplinkOverride) error {
	v6 := family == unix.AF_INET6
	var want *netlink.Route
	if o != nil {
		gw := o.Gw4
		if v6 {
			gw = o.Gw6
		}
		switch {
		case v6 && !o.V6:
			// The uplink in use has no IPv6. Without this, IPv6 would
			// still follow the skipped uplink's route and hang; refused
			// at once, clients fall back to IPv4.
			want = &netlink.Route{Dst: ipnet(netip.PrefixFrom(netip.IPv6Unspecified(), 0)), Table: TableUplink, Type: unix.RTN_UNREACHABLE, Family: family}
		default:
			link, err := netlink.LinkByName(o.If)
			if err != nil {
				return err
			}
			def := netip.PrefixFrom(netip.IPv4Unspecified(), 0)
			if v6 {
				def = netip.PrefixFrom(netip.IPv6Unspecified(), 0)
			}
			want = &netlink.Route{LinkIndex: link.Attrs().Index, Dst: ipnet(def), Table: TableUplink, Family: family}
			if gw.IsValid() {
				want.Gw = gw.AsSlice()
			} else {
				want.Scope = netlink.SCOPE_LINK
			}
		}
		if err := netlink.RouteReplace(want); err != nil {
			return err
		}
	}
	have, err := netlink.RouteListFiltered(family, &netlink.Route{Table: TableUplink}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for i := range have {
		h := have[i]
		if want != nil && h.Type == routeType(*want) && (h.Type == unix.RTN_UNREACHABLE || (h.LinkIndex == want.LinkIndex && h.Gw.Equal(want.Gw))) {
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
