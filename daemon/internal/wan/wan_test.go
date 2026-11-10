package wan

import (
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

func TestDhcpcdConf(t *testing.T) {
	v4 := config.IPFamily{Mode: config.IPStatic, Address: "192.168.1.50/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}}
	v6 := config.IPFamily{Mode: config.IPStatic, Address: "2001:db8::50/64", Gateway: "fe80::1"}
	has := func(conf, line string) bool {
		for _, l := range strings.Split(conf, "\n") {
			if l == line {
				return true
			}
		}
		return false
	}
	auto := dhcpcdConf(Uplink{If: "eth0", Metric: 100}, "/hook")
	if has(auto, "ipv4only") || has(auto, "ipv6only") || !has(auto, "ipv6rs") || !has(auto, "metric 100") {
		t.Errorf("automatic uplink:\n%s", auto)
	}
	// A fixed IPv4 address leaves only IPv6 to dhcpcd, and the other way
	// round.
	c := dhcpcdConf(Uplink{If: "eth0", Metric: 100, IP: config.IPSetup{V4: v4}}, "/hook")
	if !has(c, "ipv6only") || has(c, "ipv4only") || !has(c, "ipv6rs") {
		t.Errorf("fixed IPv4:\n%s", c)
	}
	c = dhcpcdConf(Uplink{If: "eth0", Metric: 100, IP: config.IPSetup{V6: v6}}, "/hook")
	if !has(c, "ipv4only") || has(c, "ipv6only") || has(c, "ipv6rs") || has(c, "ia_na") {
		t.Errorf("fixed IPv6:\n%s", c)
	}

	got := statics(Uplink{If: "eth0", IP: config.IPSetup{V4: v4, V6: v6}})
	if len(got) != 2 || got[0].Addr.String() != "192.168.1.50/24" || got[0].Gw.String() != "192.168.1.1" || got[1].Gw.String() != "fe80::1" {
		t.Errorf("fixed addresses: %+v", got)
	}
	if got := statics(Uplink{If: "eth0"}); len(got) != 0 {
		t.Errorf("an automatic uplink has fixed addresses: %+v", got)
	}
}

func TestActiveAndPreferred(t *testing.T) {
	up := func(ifname string, metric, rank int, internet string) State {
		return State{If: ifname, Metric: metric, Rank: rank, Carrier: true, IPv4: "192.0.2.2/24", Internet: internet}
	}
	name := func(s *State) string {
		if s == nil {
			return ""
		}
		return s.If
	}
	for _, tc := range []struct {
		what              string
		states            []State // in the configured order
		active, preferred string
	}{
		{"all well: the first in the order", []State{up("eth0", 100, 0, StOnline), up("wlan0", 200, 1, StOnline)}, "eth0", "eth0"},
		{"Wi-Fi ranked first", []State{up("wlan0", 200, 0, StOnline), up("eth0", 100, 1, StOnline)}, "wlan0", "eth0"},
		{"first has no internet", []State{up("eth0", 100, 0, StLimited), up("usb0", 300, 1, StOnline)}, "usb0", "eth0"},
		{"a sign-in page is not stepped past", []State{up("wlan0", 200, 0, StCaptive), up("usb0", 300, 1, StOnline)}, "wlan0", "wlan0"},
		{"nothing works: the first with an address", []State{up("eth0", 100, 0, StLimited), up("usb0", 300, 1, StLimited)}, "eth0", "eth0"},
		{"no link on the first", []State{{If: "eth0", Metric: 100, Internet: StDown}, up("wlan0", 200, 1, StOnline)}, "wlan0", "wlan0"},
		{"none", nil, "", ""},
	} {
		if got := name(Active(tc.states)); got != tc.active {
			t.Errorf("%s: active %q, want %q", tc.what, got, tc.active)
		}
		if got := name(Preferred(tc.states)); got != tc.preferred {
			t.Errorf("%s: preferred by the routing table %q, want %q", tc.what, got, tc.preferred)
		}
	}
}
