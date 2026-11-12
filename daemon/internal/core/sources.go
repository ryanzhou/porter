package core

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/dnsfwd"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/netx"
	"github.com/ryanzhou/porter/daemon/internal/usage"
	"github.com/ryanzhou/porter/daemon/internal/wan"
)

// Uplinks as things of their own, beyond being the one in use or not: a
// route can name one ("source:<key>", "only:<key>"), a connection that
// came in by one is answered by it, and one can have only so much data to
// spend.
//
// A route that names an uplink works the way a route through a VPN does.
// Classification gives the connection the uplink's mark, the mark selects
// the uplink's routing table (fw.SourceRoutes), and the DNS forwarder
// sends the device's lookups the same way (dnsfwd.SourceDNS). What takes
// the place of the kill switch is in the action itself, because it
// belongs to the use and not to the uplink: the same phone can be the
// only way out for one device and merely the preferred one for another.
// "only:" traffic has a rule that always leads to the uplink's table,
// which refuses it while the uplink cannot be used; "source:" traffic has
// its rule only while the uplink reaches the internet, and without the
// rule is ordinary traffic of the uplink in use.
//
// An uplink that has used up its data is, to all of this and to the order
// of uplinks, one that is not there: it is left out when the uplink in
// use is chosen, its table refuses everything, and the forward chain
// refuses what would leave by it. So a route that prefers it falls back
// and a route that uses it alone is offline: the limit was set to stop
// spending, and a route does not overrule it. The status says which it is
// (data[].reached), and the person can lift the limit for the rest of the
// period.

// DataStatus is how one line with a data limit stands.
type DataStatus struct {
	// Key is the uplink's key, and SSID, for Wi-Fi, the saved network the
	// limit belongs to.
	Key  string `json:"key"`
	SSID string `json:"ssid,omitempty"`
	// Period is the kind of period, Since and Resets its beginning and
	// its end (unix time), Limit the amount for it and Used what has been
	// carried in it, both in bytes.
	Period string `json:"period"`
	Limit  uint64 `json:"limit"`
	Used   uint64 `json:"used"`
	Since  int64  `json:"since"`
	Resets int64  `json:"resets"`
	// Reached: the amount is spent, and the uplink is not used again
	// before Resets, unless Lifted: then it is used all the same.
	Reached bool `json:"reached"`
	Lifted  bool `json:"lifted"`
}

// countData reads the counters of the uplinks that have a data limit,
// and returns those among the uplinks present that are out of data, by
// key. joined is the network each wireless uplink is on, by interface.
func (c *Core) countData(cfg *config.Config, uplinks []wan.Uplink, joined map[string]string) map[string]bool {
	now := c.now()
	loc := cfg.Location()
	if loc == nil {
		loc = time.UTC
	}
	c.usage.Begin()
	// One interface is read for a line. Two phones plugged in at once are
	// one line and would need their counters added up; the second one's
	// traffic is not counted.
	here := map[string]*usage.Reading{}
	names := map[string]string{}
	for _, u := range uplinks {
		line := usage.Line(u.Key, joined[u.If])
		if here[line] != nil {
			continue
		}
		if index, bytes, ok := netx.Traffic(u.If); ok {
			here[line] = &usage.Reading{If: u.If, Index: index, Bytes: bytes}
		}
	}
	spent := map[string]bool{}
	lines := map[string]bool{}
	data := []DataStatus{}
	each := func(key, ssid string, l config.DataLimit) {
		if !l.Enabled {
			return
		}
		line := usage.Line(key, ssid)
		lines[line] = true
		st := c.usage.Count(line, l, here[line], now, loc, c.clockTrusted)
		data = append(data, DataStatus{
			Key: key, SSID: ssid, Period: l.Period, Limit: l.Bytes(), Used: st.Used,
			Since: st.Start.Unix(), Resets: st.End.Unix(), Reached: st.Reached, Lifted: st.Lifted,
		})
		if st.Spent() && here[line] != nil {
			spent[key] = true
		}
		if st.Reached && c.usage.Tell(line) {
			msg := fmt.Sprintf("%s has used its %s of data for today. The router does not use it again until tomorrow.", names[line], dataAmount(l.MB))
			if l.Period == config.PeriodMonth {
				msg = fmt.Sprintf("%s has used its %s of data. The router does not use it again until %s.", names[line], dataAmount(l.MB), st.End.In(loc).Format("2 January"))
			}
			c.addEvent("data", msg, "")
			// Not left for the next save, minutes away: a power cut must
			// not bring a spent line back.
			if !c.resetting {
				c.usage.Save()
			}
		}
	}
	for _, e := range cfg.Ethernet {
		if e.Role == config.EthWAN {
			names[config.UplinkEth(e.Name)] = "The wired internet port " + e.Name
			each(config.UplinkEth(e.Name), "", e.Data)
		}
	}
	for _, n := range cfg.WAN.Networks {
		names[usage.LineWifi(n.SSID)] = "The Wi-Fi network “" + n.SSID + "”"
		each(config.UplinkWifi, n.SSID, n.Data)
	}
	names[config.UplinkPhone] = "The phone"
	each(config.UplinkPhone, "", cfg.WAN.Phone.Data)
	c.usage.Keep(lines)
	c.data = data
	return spent
}

// dataAmount writes an amount of data as people do: "500 MB", "5 GB",
// "1.5 GB".
func dataAmount(mb int) string {
	if mb < 1000 {
		return fmt.Sprintf("%d MB", mb)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(mb)/1000), ".0") + " GB"
}

// inUse is how the uplinks stand for the goroutines that do not run on
// the control loop: which are out of data, and whether the one in use is
// one whose data is counted.
type inUse struct {
	spent   map[string]bool
	metered bool
}

// usable leaves out the uplinks that have used up their data. What is
// left is what the uplink in use is chosen from: a spent uplink is
// stepped past as one that is down would be.
func usable(states []wan.State, spent map[string]bool) []wan.State {
	if len(spent) == 0 {
		return states
	}
	out := make([]wan.State, 0, len(states))
	for _, s := range states {
		if !spent[s.Key] {
			out = append(out, s)
		}
	}
	return out
}

// active is the uplink in use. Safe from any goroutine.
func (c *Core) active(states []wan.State) *wan.State {
	if u := c.inUse.Load(); u != nil {
		states = usable(states, u.spent)
	}
	return wan.Active(states)
}

// sparing reports that the uplink in use has only so much data, so that
// the router keeps what it would fetch for itself for later. Safe from
// any goroutine.
func (c *Core) sparing() bool {
	u := c.inUse.Load()
	return u != nil && u.metered
}

// wayOut describes the way out by an uplink that has an address, for a
// routing table that is to lead there.
func wayOut(s *wan.State) *fw.UplinkOverride {
	o := &fw.UplinkOverride{If: s.If, V4: s.IPv4 != "", V6: s.HasV6}
	o.Gw4, _ = netip.ParseAddr(s.Gateway)
	if s.HasV6 {
		o.Gw6, _ = netx.DefaultRoute(s.If, unix.AF_INET6)
	}
	// A PPPoE session has no gateway: its default route is the link
	// itself.
	if _, routed := netx.DefaultRoute(s.If, unix.AF_INET); routed && o.V4 && !o.Gw4.IsValid() {
		o.Direct4 = true
	}
	o.Direct6 = o.V6 && !o.Gw6.IsValid()
	return o
}

// sourceView is the uplinks as the firewall, the routing tables and the
// DNS forwarder each need them.
type sourceView struct {
	fw     []fw.SourceState
	routes []fw.SourceRoutes
	dns    map[string]dnsfwd.SourceDNS
	// routed are the routes of the uplinks that a route of the
	// configuration names: when one of these changes, connections under
	// way have to find their way anew.
	routed []fw.SourceRoutes
}

// sources works out how each uplink stands: whether what is routed to it
// alone can leave by it (it is connected and has data left), and whether
// what prefers it should (it also reaches the internet).
func sources(cfg *config.Config, states []wan.State, spent map[string]bool) sourceView {
	v := sourceView{dns: map[string]dnsfwd.SourceDNS{}}
	named := map[string]bool{}
	for _, key := range cfg.RoutedSources() {
		named[key] = true
	}
	seen := map[string]bool{}
	for i := range states {
		s := &states[i]
		index, ok := fw.SourceIndex(cfg, s.Key)
		if !ok || seen[s.Key] {
			// A second phone: the key is the first one's.
			continue
		}
		seen[s.Key] = true
		up := s.Carrier && (s.IPv4 != "" || s.HasV6) && !spent[s.Key]
		online := up && s.Internet == wan.StOnline
		v.fw = append(v.fw, fw.SourceState{Key: s.Key, If: s.If, Usable: up, Spent: spent[s.Key]})
		r := fw.SourceRoutes{Index: index, If: s.If, Prefer: online}
		if up {
			r.Out = wayOut(s)
		}
		for _, a := range append([]string{s.IPv4}, s.IPv6...) {
			if p, err := netip.ParsePrefix(a); err == nil {
				r.Addrs = append(r.Addrs, p.Addr())
			}
		}
		v.routes = append(v.routes, r)
		d := dnsfwd.SourceDNS{Mark: fw.MarkOnly(index), If: s.If, Up: up, Online: online}
		for _, server := range s.DNS {
			if u, err := config.ParseUpstream(server); err == nil {
				d.Servers = append(d.Servers, u)
			}
		}
		v.dns[s.Key] = d
	}
	// An uplink that a route names and that is not there still has its
	// table, which refuses everything: what is for it alone is held there.
	for key := range named {
		if index, ok := fw.SourceIndex(cfg, key); ok && !seen[key] {
			v.routes = append(v.routes, fw.SourceRoutes{Index: index})
		}
	}
	sort.Slice(v.routes, func(i, j int) bool { return v.routes[i].Index < v.routes[j].Index })
	for _, r := range v.routes {
		if key, _ := fw.SourceKey(cfg, r.Index); named[key] {
			// Its own addresses coming and going move no connection.
			r.Addrs = nil
			v.routed = append(v.routed, r)
		}
	}
	return v
}
