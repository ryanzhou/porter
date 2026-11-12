package dnsfwd

import (
	"net/netip"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// A device routed through one uplink has its lookups sent out by that
// uplink, to the resolvers that uplink's network named, as its traffic
// goes; and where its traffic would go another way or nowhere, so do its
// lookups.
func TestSourceRoutes(t *testing.T) {
	const (
		laptop = "02:00:00:00:00:21" // only the phone
		tv     = "02:00:00:00:00:22" // prefers the wired port
		tablet = "02:00:00:00:00:23" // in a group that prefers Wi-Fi
		other  = "02:00:00:00:00:24"
	)
	c := config.Default()
	c.Devices = []config.Device{{MAC: laptop, Route: "only:phone"}, {MAC: tv, Route: "source:eth:eth0"}}
	c.Groups = []config.Group{{ID: "kids", Name: "Kids", Members: []string{tablet}, Route: "source:wifi"}}
	c.Policy.Rules = []config.Rule{
		{ID: "v1", Name: "Video", Enabled: true, Dst: config.Dst{Domains: []string{"video.example"}}, Action: "only:eth:eth0"},
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	up := func(s string) []config.Upstream {
		u, err := config.ParseUpstream(s)
		if err != nil {
			t.Fatal(err)
		}
		return []config.Upstream{u}
	}
	inUse := up("192.0.2.53")
	policy := func(sources map[string]SourceDNS) *Policy {
		p := &Policy{Rules: RulesFrom(c, nil), Default: c.Policy.Default, Direct: inUse, WAN: inUse, Source: sources}
		p.SetDevices(c, nil)
		return p
	}
	phone := SourceDNS{Mark: 0x81, If: "usb0", Servers: up("172.20.10.1"), Up: true, Online: true}
	wired := SourceDNS{Mark: 0x82, If: "eth0", Servers: up("192.0.2.53"), Up: true, Online: true}
	ask := func(p *Policy, mac, name string) path { return p.decide(mac, netip.MustParseAddr("10.83.0.50"), name) }

	// Everything there and online.
	p := policy(map[string]SourceDNS{"phone": phone, "eth:eth0": wired})
	if got := ask(p, laptop, "example.org"); got.kind != "source" || got.source != "phone" || got.mark != 0x81 || got.dev != "usb0" || got.servers[0] != phone.Servers[0] {
		t.Errorf("a device for the phone alone: %+v", got)
	}
	if got := ask(p, tv, "example.org"); got.kind != "source" || got.source != "eth:eth0" || got.dev != "eth0" {
		t.Errorf("a device that prefers the wired port: %+v", got)
	}
	// A rule comes first, for the names it is about.
	if got := ask(p, laptop, "cdn.video.example"); got.kind != "source" || got.source != "eth:eth0" {
		t.Errorf("a name a rule routes to the wired port, asked by the laptop: %+v", got)
	}
	if got := ask(p, other, "example.org"); got.kind != "direct" {
		t.Errorf("a device without a route: %+v", got)
	}
	// Wi-Fi is not there: what prefers it goes by the uplink in use.
	if got := ask(p, tablet, "example.org"); got.kind != "direct" || got.servers[0] != inUse[0] || got.mark != 0 || got.dev != "" {
		t.Errorf("a group that prefers Wi-Fi, with no Wi-Fi: %+v", got)
	}
	// A lookup that took another way is not answered from the cache of
	// this one.
	if groupKey(ask(p, laptop, "example.org")) == groupKey(ask(p, tv, "example.org")) || groupKey(ask(p, tv, "example.org")) == groupKey(ask(p, other, "example.org")) {
		t.Error("lookups by different uplinks share a cache")
	}

	// The phone is connected and does not reach the internet (or the
	// check of it fails): what is for it alone still goes to it, what
	// prefers the wired port is not affected.
	limited := phone
	limited.Online = false
	p = policy(map[string]SourceDNS{"phone": limited, "eth:eth0": wired})
	if got := ask(p, laptop, "example.org"); got.kind != "source" || got.source != "phone" {
		t.Errorf("only the phone, the phone connected without internet: %+v", got)
	}
	// The wired port in that state: what prefers it falls back, and what
	// a rule sends to it alone still goes there.
	offline := wired
	offline.Online = false
	p = policy(map[string]SourceDNS{"phone": phone, "eth:eth0": offline})
	if got := ask(p, tv, "example.org"); got.kind != "direct" {
		t.Errorf("prefers the wired port, which has no internet: %+v", got)
	}
	if got := ask(p, tv, "video.example"); got.kind != "source" || got.source != "eth:eth0" {
		t.Errorf("a rule for the wired port alone, the port connected: %+v", got)
	}

	// The phone is away, or out of data (which is the same from here):
	// the device that uses nothing else is refused at once.
	away := phone
	away.Up, away.Online = false, false
	for what, sources := range map[string]map[string]SourceDNS{
		"unplugged":   {"eth:eth0": wired},
		"out of data": {"phone": away, "eth:eth0": wired},
	} {
		p = policy(sources)
		if got := ask(p, laptop, "example.org"); got.kind != "refuse" {
			t.Errorf("only the phone, the phone %s: %+v", what, got)
		}
		if got := ask(p, tv, "example.org"); got.kind != "source" {
			t.Errorf("the phone %s, and the device that prefers the wired port: %+v", what, got)
		}
	}

	// Resolvers that are configured are asked through the uplink in place
	// of the uplink's own; an uplink whose network named none is asked
	// for a public one, through it.
	p = policy(map[string]SourceDNS{"phone": phone})
	p.Direct, p.DirectCustom = up("tls://9.9.9.9#dns.quad9.net"), true
	if got := ask(p, laptop, "example.org"); got.kind != "source" || got.servers[0].Proto != "tls" || got.dev != "usb0" {
		t.Errorf("configured resolvers through the phone: %+v", got)
	}
	bare := phone
	bare.Servers = nil
	p = policy(map[string]SourceDNS{"phone": bare})
	if got := ask(p, laptop, "example.org"); got.kind != "source" || len(got.servers) == 0 || got.servers[0] != tunnelFallback[0] {
		t.Errorf("a phone that named no resolver: %+v", got)
	}

	// A paused device is refused before any of this.
	p = policy(map[string]SourceDNS{"phone": phone})
	p.Paused = map[string]bool{laptop: true}
	if got := ask(p, laptop, "example.org"); got.kind != "refuse" {
		t.Errorf("a paused device with a route through the phone: %+v", got)
	}
}
