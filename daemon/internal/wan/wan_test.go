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
