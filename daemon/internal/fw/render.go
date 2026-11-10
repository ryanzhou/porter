package fw

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Render produces a complete nftables script that atomically replaces the
// router's table.
func Render(p *Plan) string { return render(p, true) }

// Fingerprint is Render without the bulk of country address ranges (which
// are a pure function of the country codes): cheap to compute and compare
// to decide whether the ruleset needs replacing.
func Fingerprint(p *Plan) string { return render(p, false) }

func render(p *Plan, full bool) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("table inet %s {}", Table)
	w("delete table inet %s", Table)
	w("table inet %s {", Table)

	w("\tset wan_ifs { type ifname; %s}", elems(quoteAll(p.WANIfs)))
	var vpnIfs []string
	for _, v := range p.VPNs {
		if v.Active {
			vpnIfs = append(vpnIfs, v.If)
		}
	}
	w("\tset vpn_ifs { type ifname; %s}", elems(quoteAll(vpnIfs)))
	w("\tset paused { type ether_addr; %s}", elems(p.Paused))
	w("\tset portal_devs { type ether_addr; %s}", elems(p.PortalDevs))

	// Per-device traffic accounting.
	w("\tset acct_up { type ether_addr; size 2048; flags dynamic,timeout; counter; timeout 1d; }")
	w("\tset acct_down4 { type ipv4_addr; size 4096; flags dynamic,timeout; counter; timeout 1d; }")
	w("\tset acct_down6 { type ipv6_addr; size 8192; flags dynamic,timeout; counter; timeout 1d; }")

	for _, r := range p.Rules {
		if len(r.Dst4) > 0 {
			w("\tset %s { type ipv4_addr; flags interval; auto-merge; %s}", SetCIDR(r.ID, false), elems(prefixes(r.Dst4)))
		}
		if len(r.Dst6) > 0 {
			w("\tset %s { type ipv6_addr; flags interval; auto-merge; %s}", SetCIDR(r.ID, true), elems(prefixes(r.Dst6)))
		}
		if len(r.Countries) > 0 {
			var g4, g6 []string
			if full && p.Geo != nil {
				for _, cc := range r.Countries {
					a, b := p.Geo(cc)
					g4, g6 = append(g4, a...), append(g6, b...)
				}
			}
			// The sets always exist so the rules can refer to them; without
			// a country database they are simply empty and match nothing.
			w("\t# countries: %s", strings.Join(r.Countries, " "))
			w("\tset %s { type ipv4_addr; flags interval; auto-merge; %s}", SetGeo(r.ID, false), elems(g4))
			w("\tset %s { type ipv6_addr; flags interval; auto-merge; %s}", SetGeo(r.ID, true), elems(g6))
		}
		if r.Domains {
			for _, v6 := range []bool{false, true} {
				name := SetName(r.ID, v6)
				typ := "ipv4_addr"
				if v6 {
					typ = "ipv6_addr"
				}
				w("\tset %s { type %s; size 65536; flags timeout; %s}", name, typ, elems(learned(p.Learned[name])))
			}
		}
	}

	// --- classification -------------------------------------------------
	w("\tchain prerouting {")
	w("\t\ttype filter hook prerouting priority mangle; policy accept;")
	w("\t\tiifname != %q return", LANBridge)
	// Traffic to the router itself (DNS, DHCP) and to the LAN is not routed
	// by policy.
	w("\t\tfib daddr type { local, broadcast, multicast } return")
	w("\t\tct mark != 0 meta mark set ct mark return")
	w("\t\tjump classify")
	w("\t}")

	set := func(mark uint32) string {
		return fmt.Sprintf("meta mark set 0x%x ct mark set 0x%x accept", mark, mark)
	}
	w("\tchain classify {")
	if len(p.PortalDevs) > 0 {
		w("\t\tether saddr @portal_devs %s", set(MarkDirect|MarkPortal))
	}
	w("\t\tether saddr @paused %s", set(MarkBlock))
	for _, r := range p.Rules {
		chain := "r_" + r.ID
		if len(r.SrcMACs) == 0 && len(r.Src4) == 0 && len(r.Src6) == 0 {
			w("\t\tjump %s", chain)
			continue
		}
		if len(r.SrcMACs) > 0 {
			w("\t\tether saddr { %s } jump %s", strings.Join(sorted(r.SrcMACs), ", "), chain)
		}
		if len(r.Src4) > 0 {
			w("\t\tip saddr { %s } jump %s", strings.Join(prefixes(r.Src4), ", "), chain)
		}
		if len(r.Src6) > 0 {
			w("\t\tip6 saddr { %s } jump %s", strings.Join(prefixes(r.Src6), ", "), chain)
		}
	}
	if len(p.DeviceMarks) > 0 {
		var kv []string
		for _, mac := range sortedKeys(p.DeviceMarks) {
			kv = append(kv, fmt.Sprintf("%s : 0x%x", mac, p.DeviceMarks[mac]))
		}
		w("\t\tmeta mark set ether saddr map { %s } ct mark set meta mark accept", strings.Join(kv, ", "))
	}
	w("\t\t%s", set(p.DefaultMark))
	w("\t}")

	for _, r := range p.Rules {
		w("\tchain r_%s {", r.ID)
		l4 := l4match(r)
		verdict := set(r.Mark)
		anyAddr := len(r.Dst4) == 0 && len(r.Dst6) == 0 && !r.Domains && len(r.Countries) == 0
		if anyAddr {
			w("\t\t%s%s", l4, verdict)
		}
		if len(r.Dst4) > 0 {
			w("\t\t%sip daddr @%s %s", l4, SetCIDR(r.ID, false), verdict)
		}
		if len(r.Dst6) > 0 {
			w("\t\t%sip6 daddr @%s %s", l4, SetCIDR(r.ID, true), verdict)
		}
		if len(r.Countries) > 0 {
			w("\t\t%sip daddr @%s %s", l4, SetGeo(r.ID, false), verdict)
			w("\t\t%sip6 daddr @%s %s", l4, SetGeo(r.ID, true), verdict)
		}
		if r.Domains {
			w("\t\t%sip daddr @%s %s", l4, SetName(r.ID, false), verdict)
			w("\t\t%sip6 daddr @%s %s", l4, SetName(r.ID, true), verdict)
		}
		w("\t}")
	}

	// --- NAT ------------------------------------------------------------
	w("\tchain dstnat {")
	w("\t\ttype nat hook prerouting priority dstnat; policy accept;")
	if p.Intercept {
		// Every DNS query from the LAN goes through the router's resolver,
		// whichever server the client asked.
		w("\t\tiifname %q meta l4proto { tcp, udp } th dport 53 redirect to :53", LANBridge)
	}
	w("\t}")
	w("\tchain srcnat {")
	w("\t\ttype nat hook postrouting priority srcnat; policy accept;")
	// NAT44 and NAT66 alike: the LAN's private and ULA addresses never
	// leave the router.
	w("\t\toifname @wan_ifs masquerade")
	w("\t\toifname @vpn_ifs masquerade")
	w("\t}")

	// --- filter ---------------------------------------------------------
	w("\tchain input {")
	w("\t\ttype filter hook input priority filter; policy drop;")
	w("\t\tiifname \"lo\" accept")
	w("\t\tct state established,related accept")
	w("\t\tct state invalid drop")
	w("\t\tmeta l4proto ipv6-icmp accept")
	w("\t\ticmp type echo-request limit rate 20/second accept")
	w("\t\tiifname %q meta l4proto { tcp, udp } th dport 53 accept", LANBridge)
	w("\t\tiifname %q udp dport { 67, 547 } accept", LANBridge)
	if p.SSH {
		w("\t\tiifname %q tcp dport 22 accept", LANBridge)
	}
	w("\t\tiifname @wan_ifs udp dport { 68, 546 } accept")
	w("\t}")

	w("\tchain forward {")
	w("\t\ttype filter hook forward priority filter; policy drop;")
	w("\t\tct state invalid drop")
	for _, v := range p.VPNs {
		if !v.Active {
			continue
		}
		w("\t\toifname %q tcp flags syn / syn,rst tcp option maxseg size set rt mtu", v.If)
		w("\t\tiifname %q meta nfproto ipv4 tcp flags syn / syn,rst tcp option maxseg size > %d tcp option maxseg size set %d", v.If, v.MTU-40, v.MTU-40)
		w("\t\tiifname %q meta nfproto ipv6 tcp flags syn / syn,rst tcp option maxseg size > %d tcp option maxseg size set %d", v.If, v.MTU-60, v.MTU-60)
	}
	w("\t\toifname @wan_ifs tcp flags syn / syn,rst tcp option maxseg size set rt mtu")
	w("\t\tiifname %q update @acct_up { ether saddr }", LANBridge)
	w("\t\toifname %q meta nfproto ipv4 update @acct_down4 { ip daddr }", LANBridge)
	w("\t\toifname %q meta nfproto ipv6 update @acct_down6 { ip6 daddr }", LANBridge)
	w("\t\tiifname %q jump lan_out", LANBridge)
	w("\t\tct state established,related accept")
	w("\t}")

	// lan_out decides for every packet leaving the LAN, new or established,
	// so that a flow can never continue on a path its mark does not allow.
	w("\tchain lan_out {")
	w("\t\tmeta mark & 0x%x == 0x%x jump refuse", ClassMask, MarkBlock)
	for _, v := range p.VPNs {
		if v.Active {
			w("\t\tmeta mark & 0x%x == 0x%x oifname %q accept", ClassMask, v.Mark, v.If)
		}
		if v.KillSwitch {
			w("\t\tmeta mark & 0x%x == 0x%x jump refuse", ClassMask, v.Mark)
		}
	}
	// Whatever is left may leave directly: direct traffic, and traffic for
	// a tunnel without a kill switch that is currently down.
	w("\t\tmeta mark & 0x%x >= 0x10 oifname @vpn_ifs jump refuse", ClassMask)
	w("\t\tmeta mark & 0x%x != 0x%x oifname @wan_ifs accept", ClassMask, MarkBlock)
	w("\t\tjump refuse")
	w("\t}")
	w("\tchain refuse {")
	w("\t\tmeta l4proto tcp reject with tcp reset")
	w("\t\treject with icmpx admin-prohibited")
	w("\t}")

	w("}")
	return b.String()
}

func l4match(r PlanRule) string {
	if r.Proto == "" && len(r.Ports) == 0 {
		return ""
	}
	proto := r.Proto
	if proto == "" {
		proto = "{ tcp, udp }"
	}
	s := "meta l4proto " + proto + " "
	if len(r.Ports) > 0 {
		var ps []string
		for _, p := range r.Ports {
			if p.Lo == p.Hi {
				ps = append(ps, fmt.Sprint(p.Lo))
			} else {
				ps = append(ps, fmt.Sprintf("%d-%d", p.Lo, p.Hi))
			}
		}
		s += "th dport { " + strings.Join(ps, ", ") + " } "
	}
	return s
}

func elems(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return "elements = { " + strings.Join(items, ", ") + " } "
}

func quoteAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

func prefixes(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		if p.IsSingleIP() {
			out[i] = p.Addr().String()
		} else {
			out[i] = p.String()
		}
	}
	sort.Strings(out)
	return out
}

func learned(es []Elem) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		if e.TTL > 0 {
			out = append(out, fmt.Sprintf("%s timeout %ds", e.Addr, e.TTL))
		}
	}
	sort.Strings(out)
	return out
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]uint32) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
