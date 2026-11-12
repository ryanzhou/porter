package core

import (
	"net/netip"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/wan"
)

// How each uplink stands for the routes that name it, as the firewall,
// the routing tables and the DNS forwarder are told: what is routed to an
// uplink alone goes there while it is connected and has data left, what
// prefers it only while it also reaches the internet.
func TestSources(t *testing.T) {
	cfg := config.Default()
	cfg.Ethernet = append(cfg.Ethernet, config.EthPort{Name: "eth1", Role: config.EthWAN})
	cfg.Devices = []config.Device{{MAC: "02:00:00:00:00:01", Route: "only:phone"}, {MAC: "02:00:00:00:00:02", Route: "source:wifi"}}
	cfg.Policy.Rules = []config.Rule{{ID: "r1", Enabled: true, Action: "source:eth:eth1"}}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	states := []wan.State{
		{If: "eth0", Kind: "eth", Key: "eth:eth0", Carrier: true, Internet: wan.StOnline, IPv4: "192.0.2.2/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.53"}},
		// Connected, and its sign-in page not passed.
		{If: "eth1", Kind: "eth", Key: "eth:eth1", Carrier: true, Internet: wan.StCaptive, IPv4: "198.51.100.2/24", Gateway: "198.51.100.1"},
		{If: "usb0", Kind: "phone", Key: "phone", Carrier: true, Internet: wan.StOnline, IPv4: "172.20.10.2/28", Gateway: "172.20.10.1", IPv6: []string{"2001:db8:5::2/64"}, DNS: []string{"172.20.10.1"}},
		// A second phone has the key of the first.
		{If: "usb1", Kind: "phone", Key: "phone", Carrier: true, Internet: wan.StOnline, IPv4: "172.20.11.2/28", Gateway: "172.20.11.1"},
	}
	byIndex := func(v sourceView, key string) fw.SourceRoutes {
		t.Helper()
		index, _ := fw.SourceIndex(cfg, key)
		for _, r := range v.routes {
			if r.Index == index {
				return r
			}
		}
		t.Fatalf("no routes for %s in %v", key, v.routes)
		return fw.SourceRoutes{}
	}

	v := sources(cfg, states, nil)
	if len(v.fw) != 3 || len(v.routes) != 4 {
		t.Fatalf("with three uplinks there and Wi-Fi named by a route: %+v, %v", v.fw, v.routes)
	}
	phone := byIndex(v, "phone")
	if phone.Out == nil || phone.Out.If != "usb0" || phone.Out.Gw4 != netip.MustParseAddr("172.20.10.1") || !phone.Prefer || phone.If != "usb0" {
		t.Errorf("the phone's routes: %v", phone)
	}
	if len(phone.Addrs) != 2 || phone.Addrs[0] != netip.MustParseAddr("172.20.10.2") || phone.Addrs[1] != netip.MustParseAddr("2001:db8:5::2") {
		t.Errorf("the phone's own addresses: %v", phone.Addrs)
	}
	// Behind a sign-in page: what is for it alone goes to it (and meets
	// the page), what prefers it does not.
	if port := byIndex(v, "eth:eth1"); port.Out == nil || port.Prefer {
		t.Errorf("an uplink behind a sign-in page: %v", port)
	}
	// Named by a route and not there: a table, and nothing in it.
	if wifi := byIndex(v, "wifi"); wifi.Out != nil || wifi.Prefer || wifi.If != "" || len(wifi.Addrs) != 0 {
		t.Errorf("Wi-Fi, which is not there: %v", wifi)
	}
	if d := v.dns["phone"]; d.If != "usb0" || d.Mark != fw.MarkOnly(1) || !d.Up || !d.Online || len(d.Servers) != 1 {
		t.Errorf("the phone for lookups: %+v", d)
	}
	if d := v.dns["eth:eth1"]; !d.Up || d.Online {
		t.Errorf("the uplink behind a sign-in page for lookups: %+v", d)
	}
	if _, there := v.dns["wifi"]; there {
		t.Error("Wi-Fi is not there, and is offered for lookups")
	}
	// What moves connections: the uplinks that routes name, and not their
	// own addresses. The wired port in use is named by no route.
	if len(v.routed) != 3 {
		t.Fatalf("uplinks whose routes move connections: %v", v.routed)
	}
	for _, r := range v.routed {
		if key, _ := fw.SourceKey(cfg, r.Index); key == "eth:eth0" || len(r.Addrs) != 0 {
			t.Errorf("among what moves connections: %v", r)
		}
	}

	// The phone is out of data: as good as unplugged for every route, and
	// the firewall is told why.
	spent := map[string]bool{"phone": true}
	v = sources(cfg, states, spent)
	if phone := byIndex(v, "phone"); phone.Out != nil || phone.Prefer {
		t.Errorf("a phone that is out of data: %v", phone)
	}
	if d := v.dns["phone"]; d.Up || d.Online {
		t.Errorf("a phone that is out of data, for lookups: %+v", d)
	}
	told := false
	for _, s := range v.fw {
		if s.Key == "phone" {
			told = s.Spent && !s.Usable && s.If == "usb0"
		}
	}
	if !told {
		t.Errorf("the firewall is not told that the phone is out of data: %+v", v.fw)
	}
	// And it is stepped past when the uplink in use is chosen.
	first := []wan.State{states[2], states[0]}
	if a := wan.Active(usable(first, nil)); a == nil || a.If != "usb0" {
		t.Errorf("the phone ranked first: %+v in use", a)
	}
	if a := wan.Active(usable(first, spent)); a == nil || a.If != "eth0" {
		t.Errorf("the phone ranked first and out of data: %+v in use", a)
	}
	if a := wan.Active(usable(first[:1], spent)); a != nil {
		t.Errorf("nothing but a phone that is out of data, and %+v is in use", a)
	}

	// Unplugged, no cable, no address: not usable either.
	for what, s := range map[string]wan.State{
		"no carrier": {If: "usb0", Key: "phone", Internet: wan.StDown, IPv4: "172.20.10.2/28"},
		"no address": {If: "usb0", Key: "phone", Carrier: true, Internet: wan.StConnecting},
	} {
		v = sources(cfg, []wan.State{s}, nil)
		if phone := byIndex(v, "phone"); phone.Out != nil || phone.Prefer || v.dns["phone"].Up {
			t.Errorf("a phone with %s: %v", what, phone)
		}
	}
}

func TestDataAmount(t *testing.T) {
	for mb, want := range map[int]string{1: "1 MB", 500: "500 MB", 999: "999 MB", 1000: "1 GB", 1500: "1.5 GB", 5000: "5 GB", 200_000: "200 GB", 2048: "2 GB"} {
		if got := dataAmount(mb); got != want {
			t.Errorf("%d MB is written %q, want %q", mb, got, want)
		}
	}
}
