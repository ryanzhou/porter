package fw

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/proto"
)

// Render produces a complete nftables script that atomically replaces the
// router's table.
func Render(p *Plan) string { return render(p, true) }

// Fingerprint is Render without the bulk of country address ranges (which
// are a pure function of the country codes): cheap to compute and compare
// to decide whether the ruleset needs replacing.
func Fingerprint(p *Plan) string { return render(p, false) }

// ClassKey identifies the part of a plan that decides which way a
// connection goes. Connections under way have to be classified again when
// it changes, and only then: a change to who may reach the router, or to
// the list of uplinks, moves no connection, and forgetting them all for
// it would stall every one of them.
func ClassKey(p *Plan) string {
	q := *p
	q.WANIfs, q.SSH, q.Control, q.Intercept, q.Learned, q.MDNS = nil, false, false, false, nil, nil
	// Nor does a delegated prefix arriving or changing: the connections
	// there are go on as they were set up, and nothing was under way to
	// addresses that did not exist.
	q.Delegated, q.WANMTU = nil, nil
	// Nor the IPv6 addresses a forwarded port is open to, which come and
	// go with the device. A forward itself does decide: one that is taken
	// away has to end the connections that came in by it.
	q.Forwards = append([]PlanForward(nil), p.Forwards...)
	for i := range q.Forwards {
		q.Forwards[i].To6 = nil
	}
	q.LANs = append([]PlanLAN(nil), p.LANs...)
	for i := range q.LANs {
		q.LANs[i].Global = netip.Prefix{}
	}
	return render(&q, false)
}

func render(p *Plan, full bool) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("table inet %s {}", Table)
	w("delete table inet %s", Table)
	w("table inet %s {", Table)

	w("\tset wan_ifs { type ifname; %s}", elems(quoteAll(p.WANIfs)))
	var lanIfs, open, isolated []string
	var nets4, nets6 []netip.Prefix
	for _, l := range p.LANs {
		lanIfs = append(lanIfs, l.If)
		if l.Isolated {
			isolated = append(isolated, l.If)
		} else {
			open = append(open, l.If)
		}
		nets4 = append(nets4, l.Net4)
		if l.Net6.IsValid() {
			nets6 = append(nets6, l.Net6)
		}
		if l.Global.IsValid() {
			nets6 = append(nets6, l.Global)
		}
	}
	// The app may come in wherever SSH may, and through the WireGuard
	// server: only paired phones get anywhere with it.
	control := open
	if s := p.Server; s != nil {
		// The server's range is one more of the router's networks to
		// everything below: its devices are classified, asked about DNS
		// and let through to the other networks like those at home.
		lanIfs = append(lanIfs, s.If)
		control = append(append([]string(nil), open...), s.If)
		nets4 = append(nets4, s.Net4)
		if s.Net6.IsValid() {
			nets6 = append(nets6, s.Net6)
		}
	}
	forwards := len(p.Forwards) > 0
	// several: there is traffic between the router's own networks to
	// decide about. A forwarded port makes for some even with one network:
	// a device at home that uses the public address is sent back inside.
	several := len(p.LANs) > 1 || p.Server != nil || forwards
	w("\tset lan_ifs { type ifname; %s}", elems(quoteAll(sorted(lanIfs))))
	if several {
		w("\tset lan_nets4 { type ipv4_addr; flags interval; %s}", elems(prefixes(nets4)))
		w("\tset lan_nets6 { type ipv6_addr; flags interval; %s}", elems(prefixes(nets6)))
	}
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
			w("\t# countries: %s (database %s)", strings.Join(r.Countries, " "), p.GeoVersion)
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
	w("\t\tiifname != @lan_ifs return")
	// Traffic to the router itself (DNS, DHCP) and to the LAN is not routed
	// by policy.
	w("\t\tfib daddr type { local, broadcast, multicast } return")
	if several {
		// Nor is traffic between the router's own networks; the forward
		// hook decides about that.
		w("\t\tip daddr @lan_nets4 return")
		w("\t\tip6 daddr @lan_nets6 return")
	}
	if forwards {
		// What a device answers to a connection that came in from outside
		// is not a connection of its own to classify: it goes back out
		// the way the router itself would send it, by the uplink in use.
		w("\t\tct direction reply return")
	}
	w("\t\tct mark != 0 meta mark set ct mark return")
	w("\t\tjump classify")
	w("\t}")

	set := func(mark uint32) string {
		return fmt.Sprintf("meta mark set 0x%x ct mark set 0x%x accept", mark, mark)
	}
	w("\tchain classify {")
	// Paused comes first: a paused device is not let out to sign in for
	// everyone either.
	w("\t\tether saddr @paused %s", set(MarkBlock))
	if len(p.PortalDevs) > 0 {
		w("\t\tether saddr @portal_devs %s", set(MarkDirect|MarkPortal))
	}
	for _, r := range p.Rules {
		chain := "r_" + r.ID
		if len(r.SrcMACs) == 0 && len(r.Src4) == 0 && len(r.Src6) == 0 && len(r.SrcIfs) == 0 {
			w("\t\tjump %s", chain)
			continue
		}
		if len(r.SrcIfs) > 0 {
			w("\t\tiifname { %s } jump %s", strings.Join(quoteAll(sorted(r.SrcIfs)), ", "), chain)
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
		for _, mac := range sortedNames(p.DeviceMarks) {
			kv = append(kv, fmt.Sprintf("%s : 0x%x", mac, p.DeviceMarks[mac]))
		}
		w("\t\tmeta mark set ether saddr map { %s } ct mark set meta mark accept", strings.Join(kv, ", "))
	}
	// A network's own route, for devices that have none of their own.
	var byNet []string
	for _, l := range p.LANs {
		if l.Mark != 0 {
			byNet = append(byNet, fmt.Sprintf("%q : 0x%x", l.If, l.Mark))
		}
	}
	if len(byNet) > 0 {
		sort.Strings(byNet)
		w("\t\tmeta mark set iifname map { %s } ct mark set meta mark accept", strings.Join(byNet, ", "))
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
		w("\t\tiifname @lan_ifs meta l4proto { tcp, udp } th dport 53 redirect to :53")
	}
	// Forwarded ports: what arrives for the router itself on one of them
	// is sent on to the device. From an uplink, and from inside as well,
	// where a device has used the router's public address or name; not
	// where it meant the router's address in its own network.
	var own4 []string
	for _, l := range p.LANs {
		own4 = append(own4, l.Addr4.String())
	}
	if p.Server != nil {
		own4 = append(own4, p.Server.Addr4.String())
	}
	for _, f := range p.Forwards {
		to := "dnat ip to " + f.To4.String()
		if f.Inside != f.Outside {
			to += ":" + ports(f.Inside)
		}
		if !f.Restricted {
			w("\t\tiifname @wan_ifs meta nfproto ipv4 fib daddr type local %s%s", l4ports(f.Proto, f.Outside), to)
		} else if len(f.From4) > 0 {
			w("\t\tiifname @wan_ifs fib daddr type local ip saddr { %s } %s%s", strings.Join(prefixes(f.From4), ", "), l4ports(f.Proto, f.Outside), to)
		}
		w("\t\tiifname @lan_ifs fib daddr type local ip daddr != { %s } %s%s", strings.Join(sorted(own4), ", "), l4ports(f.Proto, f.Outside), to)
	}
	w("\t}")
	w("\tchain srcnat {")
	w("\t\ttype nat hook postrouting priority srcnat; policy accept;")
	// An address out of a prefix an uplink's network has delegated is
	// known to that network and goes out by that uplink as it is. By any
	// other way out it is as foreign as a private one: one provider does
	// not carry another's addresses, nor does a tunnel.
	for _, ifname := range sortedNames(p.Delegated) {
		w("\t\toifname %q ip6 saddr %s accept", ifname, p.Delegated[ifname].Masked())
	}
	if forwards {
		// A connection sent back inside by a forwarded port has to return
		// through the router, or the device that made it would be
		// answered from an address it never spoke to.
		w("\t\tct status dnat ip saddr @lan_nets4 ip daddr @lan_nets4 masquerade")
	}
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
	// "icmpv6" is a word nft knows by itself. Its other name, "ipv6-icmp",
	// is looked up in /etc/protocols, which the router image does not have.
	w("\t\tmeta l4proto icmpv6 accept")
	w("\t\ticmp type echo-request limit rate 20/second accept")
	w("\t\tiifname @lan_ifs meta l4proto { tcp, udp } th dport 53 accept")
	w("\t\tiifname @lan_ifs udp dport { 67, 547 } accept")
	if p.SSH && len(open) > 0 {
		// Not from an isolated network: that is where guests are.
		w("\t\tiifname { %s } tcp dport 22 accept", strings.Join(quoteAll(sorted(open)), ", "))
	}
	if p.Control && len(control) > 0 {
		// The app, from a phone on one of the router's networks. Like
		// SSH, not from an isolated one, and never from an uplink: no
		// rule lets anything in from there but replies, DHCP and the
		// WireGuard server's own port.
		w("\t\tiifname { %s } tcp dport %d accept", strings.Join(quoteAll(sorted(control)), ", "), proto.ControlPort)
	}
	if len(p.MDNS) > 0 {
		// Multicast DNS, to be repeated on the other networks it is
		// relayed between. Only what is sent to its group: a question put
		// to the router's own address is not one for the relay.
		mdns := strings.Join(quoteAll(p.MDNS), ", ")
		w("\t\tiifname { %s } ip daddr 224.0.0.251 udp dport 5353 accept", mdns)
		w("\t\tiifname { %s } ip6 daddr ff02::fb udp dport 5353 accept", mdns)
	}
	w("\t\tiifname @wan_ifs udp dport { 68, 546 } accept")
	if s := p.Server; s != nil {
		// The WireGuard server, which answers nobody whose key it does
		// not know. From inside as well: a phone that comes home keeps
		// the connection it has to the router's public address.
		w("\t\tiifname @wan_ifs udp dport %d accept", s.Port)
		w("\t\tiifname @lan_ifs udp dport %d accept", s.Port)
	}
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
	if s := p.Server; s != nil {
		w("\t\toifname %q tcp flags syn / syn,rst tcp option maxseg size set rt mtu", s.If)
		w("\t\tiifname %q meta nfproto ipv4 tcp flags syn / syn,rst tcp option maxseg size > %d tcp option maxseg size set %d", s.If, ServerMTU-40, ServerMTU-40)
		w("\t\tiifname %q meta nfproto ipv6 tcp flags syn / syn,rst tcp option maxseg size > %d tcp option maxseg size set %d", s.If, ServerMTU-60, ServerMTU-60)
	}
	// A connection's two ends each say how large a segment they take,
	// and neither knows about the narrowest stretch in between. Outwards
	// that is corrected from the route; a segment size coming in by an
	// uplink that carries less than Ethernet does (PPPoE takes 8 bytes of
	// every packet) has to be cut down by number.
	w("\t\toifname @wan_ifs tcp flags syn / syn,rst tcp option maxseg size set rt mtu")
	for _, ifname := range sortedNames(p.WANMTU) {
		mtu := p.WANMTU[ifname]
		w("\t\tiifname %q meta nfproto ipv4 tcp flags syn / syn,rst tcp option maxseg size > %d tcp option maxseg size set %d", ifname, mtu-40, mtu-40)
		w("\t\tiifname %q meta nfproto ipv6 tcp flags syn / syn,rst tcp option maxseg size > %d tcp option maxseg size set %d", ifname, mtu-60, mtu-60)
	}
	w("\t\tiifname @lan_ifs update @acct_up { ether saddr }")
	w("\t\toifname @lan_ifs meta nfproto ipv4 update @acct_down4 { ip daddr }")
	w("\t\toifname @lan_ifs meta nfproto ipv6 update @acct_down6 { ip6 daddr }")
	if several {
		w("\t\tiifname @lan_ifs oifname @lan_ifs jump lan_lan")
	}
	if forwards {
		// A device's answers to what a forwarded port let in. They were
		// not classified (see prerouting) and leave by an uplink only.
		w("\t\tiifname @lan_ifs ct direction reply oifname @wan_ifs accept")
	}
	w("\t\tiifname @lan_ifs jump lan_out")
	if forwards {
		w("\t\tiifname @wan_ifs jump forwarded")
	}
	// Nothing else is forwarded but what belongs to a connection made
	// from inside, or to one a forwarded port let in: with a delegated
	// prefix devices have addresses the whole internet can send to, and
	// what arrives for them unasked ends here, at the chain's policy. The
	// ICMPv6 that IPv6 cannot do without on the way (packet too big,
	// unreachable, time exceeded) is about a connection that exists and
	// so counts as related to it.
	w("\t\tct state established,related accept")
	w("\t}")

	// The router's own traffic that is meant for a tunnel (its DNS queries
	// on behalf of devices routed there, a speed test through it). Policy
	// routing looks at the main table's specific routes before the
	// tunnel's, so a tunnel resolver whose address falls inside the
	// upstream network's range would be reached over the uplink, in the
	// clear. Forwarded traffic is stopped by lan_out; this is the same
	// check for what the router sends itself.
	w("\tchain output {")
	w("\t\ttype filter hook output priority filter; policy accept;")
	for _, v := range p.VPNs {
		if !v.KillSwitch {
			continue
		}
		if v.Active {
			w("\t\tmeta mark & 0x%x == 0x%x oifname != %q jump refuse", ClassMask, v.Mark, v.If)
		} else {
			w("\t\tmeta mark & 0x%x == 0x%x jump refuse", ClassMask, v.Mark)
		}
	}
	w("\t}")

	if several {
		// Between the router's own networks: nothing from a paused device,
		// then anything already under way, and anything new unless it
		// comes from an isolated network. Traffic between networks is not
		// classified (see prerouting), so pausing has to be looked at
		// here, and before replies are let through.
		w("\tchain lan_lan {")
		w("\t\tether saddr @paused jump refuse")
		w("\t\tct state established,related accept")
		if forwards {
			// What a forwarded port sent back inside: open to the whole
			// internet, and so to a network that is kept apart as well.
			w("\t\tct status dnat accept")
		}
		if len(isolated) > 0 {
			w("\t\tiifname { %s } jump refuse", strings.Join(quoteAll(sorted(isolated)), ", "))
		}
		w("\t\taccept")
		w("\t}")
	}

	if forwards {
		// What an uplink brings for a forwarded port. Over IPv4 that is
		// what the rules in dstnat translated, and nothing else there is
		// translated on its way in. Over IPv6 the device is addressed
		// itself, at the addresses it has out of a delegated prefix, on
		// the port it listens on.
		w("\tchain forwarded {")
		w("\t\tct status dnat accept")
		for _, f := range p.Forwards {
			if len(f.To6) == 0 || (f.Restricted && len(f.From6) == 0) {
				continue
			}
			from := ""
			if f.Restricted {
				from = "ip6 saddr { " + strings.Join(prefixes(f.From6), ", ") + " } "
			}
			to := make([]string, len(f.To6))
			for i, a := range f.To6 {
				to[i] = a.String()
			}
			w("\t\tip6 daddr { %s } %s%saccept", strings.Join(to, ", "), from, l4ports(f.Proto, f.Inside))
		}
		w("\t}")
	}

	// lan_out decides for every packet leaving the LAN, new or established,
	// so that a flow can never continue on a path its mark does not allow.
	w("\tchain lan_out {")
	// Everything that leaves the router's networks for elsewhere has been
	// classified. A packet that has not was let past classification as
	// bound for another of the router's networks and then found no route
	// there (a network just removed or renumbered): it is not to leave by
	// an uplink instead.
	w("\t\tmeta mark & 0x%x == 0x0 jump refuse", ClassMask)
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

// ports writes a port or a range of ports.
func ports(r config.PortRange) string {
	if r.Lo == r.Hi {
		return fmt.Sprint(r.Lo)
	}
	return fmt.Sprintf("%d-%d", r.Lo, r.Hi)
}

// l4ports matches the destination ports of a forwarded port.
func l4ports(proto string, r config.PortRange) string {
	if proto == config.ProtoBoth {
		return "meta l4proto { tcp, udp } th dport " + ports(r) + " "
	}
	return proto + " dport " + ports(r) + " "
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

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
