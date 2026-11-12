package config

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

// inbound returns a valid configuration with something of everything
// that may come in from outside.
func inbound() *Config {
	c := Default()
	c.Devices = []Device{{MAC: "02:00:00:00:00:10", Name: "NAS", IP: "10.83.0.10"}}
	s := &c.Inbound.Server
	s.Enabled = true
	s.Peers = []WGServerPeer{{ID: "p1", Name: "Phone", PublicKey: mustPub(newWGKey()), Address: "10.89.0.2", Enabled: true, Full: true}}
	c.Inbound.Forwards = []PortForward{{ID: "web", Name: "Web", Enabled: true, Proto: "tcp", Ports: "8443", To: 443, Device: "02:00:00:00:00:10"}}
	c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSDuckDNS, Hostname: "example.duckdns.org", Password: "0123-token"}
	c.Normalize()
	return c
}

func mustPub(priv string) string {
	pub, err := WGPublicKey(priv)
	if err != nil {
		panic(err)
	}
	return pub
}

func TestInboundDefaults(t *testing.T) {
	// A configuration stored before any of this existed gets a server
	// that is switched off but complete: its key, its ranges, its port.
	var old Config
	if err := json.Unmarshal([]byte(`{"version":3,"system":{"name":"Porter"},"lan":{"lease_minutes":720,"domain":"lan","ipv6":true},
		"networks":[{"id":"lan","name":"Main","address":"10.89.0.1/24"}],"wifi":[{"id":"main"}],
		"wan":{"wifi_mac":{"mode":"hardware"},"eth_mac":{"mode":"hardware"},"probe":{"url":"http://x.test/","expect":"ok"}},"policy":{"default":"direct"}}`), &old); err != nil {
		t.Fatal(err)
	}
	old.Normalize()
	if err := old.Validate(); err != nil {
		t.Fatal(err)
	}
	s := old.Inbound.Server
	if s.Enabled || s.Port != DefaultServerPort || !validWGKey(s.PrivateKey) || s.PublicKey() == "" || old.Inbound.CheckURL != DefaultCheckURL {
		t.Errorf("server after normalizing: %+v", s)
	}
	// Its range is the first that collides with no network: the main one
	// here sits on the usual first choice.
	if s.Address != "172.23.89.1/24" {
		t.Errorf("the server's range is %s beside a network on 10.89.0.0/24", s.Address)
	}
	// What was chosen stays: a second pass changes nothing.
	before, _ := json.Marshal(old)
	old.Normalize()
	if after, _ := json.Marshal(old); string(after) != string(before) {
		t.Errorf("normalizing twice changed the configuration:\n%s\n%s", before, after)
	}
	if strings.Contains(string(before), "null") {
		t.Errorf("configuration JSON contains null: %s", before)
	}
	// A network added later is not put on the server's range, nor does
	// the server take one of the ranges networks are given.
	c := Default()
	c.Inbound.Server.Address = "10.84.0.1/24"
	c.Networks = append(c.Networks, Network{Name: "Guest"})
	c.Normalize()
	if err := c.Validate(); err != nil || c.Networks[1].Address != "172.23.83.1/24" {
		t.Errorf("a new network beside a server on 10.84.0.0/24 got %s (%v)", c.Networks[1].Address, err)
	}
	for _, s := range ServerSubnets {
		for _, n := range Subnets {
			if netip.MustParsePrefix(s).Masked().Overlaps(netip.MustParsePrefix(n).Masked()) {
				t.Errorf("%s is offered to the server and to networks", s)
			}
		}
	}
}

func TestServerAddresses(t *testing.T) {
	c := inbound()
	s := &c.Inbound.Server
	s.ULAPrefix = "fd00:0:0:89::/64"
	// The lowest address nobody has, the router's and the range's ends
	// left out.
	if a, ok := s.FreeAddress(); !ok || a != "10.89.0.3" {
		t.Errorf("next address: %q %v", a, ok)
	}
	s.Peers = append(s.Peers, WGServerPeer{Address: "10.89.0.4"})
	if a, _ := s.FreeAddress(); a != "10.89.0.3" {
		t.Errorf("next address with a gap: %q", a)
	}
	s.Peers = s.Peers[1:]
	if a, _ := s.FreeAddress(); a != "10.89.0.2" {
		t.Errorf("an address that was given back: %q", a)
	}
	// A small range fills up.
	s.Address = "10.89.0.1/30"
	s.Peers = []WGServerPeer{{Address: "10.89.0.2"}}
	if a, ok := s.FreeAddress(); ok {
		t.Errorf("a full range still had %q", a)
	}
	s.Address = "10.89.0.1/24"
	// A device's IPv6 address is its host number in the server's /64.
	got := s.PeerAddrs(WGServerPeer{Address: "10.89.0.200"}, true)
	if len(got) != 2 || got[0].String() != "10.89.0.200/32" || got[1].String() != "fd00:0:0:89::c8/128" {
		t.Errorf("a device's addresses: %v", got)
	}
	if got := s.PeerAddrs(WGServerPeer{Address: "10.89.0.200"}, false); len(got) != 1 {
		t.Errorf("a device's addresses without IPv6: %v", got)
	}
	if a, p := s.ULA(); a.String() != "fd00:0:0:89::1" || p.String() != "fd00:0:0:89::/64" {
		t.Errorf("the router's IPv6 address in the range: %v %v", a, p)
	}
}

func TestInboundValidation(t *testing.T) {
	if err := inbound().Validate(); err != nil {
		t.Fatal(err)
	}
	key := mustPub(newWGKey())
	cases := map[string]struct {
		edit func(c *Config)
		want string // part of the message
	}{
		"port the router uses": {func(c *Config) { c.Inbound.Server.Port = 53 }, "uses UDP port 53 itself"},
		"port out of range":    {func(c *Config) { c.Inbound.Server.Port = 70000 }, "1 to 65535"},
		"no key":               {func(c *Config) { c.Inbound.Server.PrivateKey = "nope" }, "key"},
		"public range":         {func(c *Config) { c.Inbound.Server.Address = "8.8.8.1/24" }, "private"},
		"range on a network":   {func(c *Config) { c.Inbound.Server.Address = "10.83.0.129/25" }, "overlaps"},
		"network onto the range": {func(c *Config) {
			c.Networks = append(c.Networks, Network{ID: "g", Name: "Guest", Address: "10.89.0.129/25", ULAPrefix: "fd00:0:0:99::/64"})
		}, "overlaps"},
		"IPv6 range of a network":      {func(c *Config) { c.Inbound.Server.ULAPrefix = c.Networks[0].ULAPrefix }, "IPv6 prefix"},
		"device outside the range":     {func(c *Config) { c.Inbound.Server.Peers[0].Address = "10.83.0.5" }, "inside 10.89.0.0/24"},
		"device at the router's":       {func(c *Config) { c.Inbound.Server.Peers[0].Address = "10.89.0.1" }, "other than the router's"},
		"device at the last address":   {func(c *Config) { c.Inbound.Server.Peers[0].Address = "10.89.0.255" }, "inside 10.89.0.0/24"},
		"device with the server's key": {func(c *Config) { c.Inbound.Server.Peers[0].PublicKey = c.Inbound.Server.PublicKey() }, "public key"},
		"device without a name":        {func(c *Config) { c.Inbound.Server.Peers[0].Name = "" }, "name"},
		"two devices, one key": {func(c *Config) {
			c.Inbound.Server.Peers = append(c.Inbound.Server.Peers, WGServerPeer{ID: "p2", Name: "Laptop", PublicKey: c.Inbound.Server.Peers[0].PublicKey, Address: "10.89.0.3"})
		}, "same key"},
		"two devices, one address": {func(c *Config) {
			c.Inbound.Server.Peers = append(c.Inbound.Server.Peers, WGServerPeer{ID: "p2", Name: "Laptop", PublicKey: key, Address: "10.89.0.2"})
		}, "same address"},
		"too many devices": {func(c *Config) {
			for i := 0; i < MaxServerPeers; i++ {
				c.Inbound.Server.Peers = append(c.Inbound.Server.Peers, WGServerPeer{Name: "x", PublicKey: mustPub(newWGKey()), Address: netip.MustParseAddr("10.89.0.10").Next().String()})
			}
		}, "room for 32"},

		"forward without a name":       {func(c *Config) { c.Inbound.Forwards[0].Name = "" }, "name"},
		"forward of nothing":           {func(c *Config) { c.Inbound.Forwards[0].Ports = "0" }, "1 to 65535"},
		"forward of a word":            {func(c *Config) { c.Inbound.Forwards[0].Ports = "http" }, "1 to 65535"},
		"backwards range":              {func(c *Config) { c.Inbound.Forwards[0].Ports = "90-80" }, "range like"},
		"protocol":                     {func(c *Config) { c.Inbound.Forwards[0].Proto = "icmp" }, "TCP, UDP or both"},
		"range to another port":        {func(c *Config) { c.Inbound.Forwards[0].Ports = "8000-8010" }, "keeps its numbers"},
		"inside port out of range":     {func(c *Config) { c.Inbound.Forwards[0].To = 65536 }, "port on the device"},
		"no device":                    {func(c *Config) { c.Inbound.Forwards[0].Device = "" }, "choose the device"},
		"device without fixed address": {func(c *Config) { c.Devices[0].IP = "" }, "without a fixed address (NAS)"},
		"device nobody knows":          {func(c *Config) { c.Inbound.Forwards[0].Device = "02:00:00:00:00:99" }, "without a fixed address (02:00:00:00:00:99)"},
		"only from a name":             {func(c *Config) { c.Inbound.Forwards[0].From = []string{"example.com"} }, "not an address"},
		"the router's own address port": {func(c *Config) {
			c.Inbound.Forwards[0].Proto, c.Inbound.Forwards[0].Ports, c.Inbound.Forwards[0].To = "udp", "60-70", 0
		}, "needs UDP port 68 itself"},
		"the server's port": {func(c *Config) {
			c.Inbound.Forwards[0].Proto, c.Inbound.Forwards[0].Ports, c.Inbound.Forwards[0].To = "both", "51820", 0
		}, "reach home through"},
		"one port twice": {func(c *Config) {
			c.Inbound.Forwards = append(c.Inbound.Forwards, PortForward{ID: "two", Name: "Again", Enabled: true, Proto: "both", Ports: "8440-8450", Device: "02:00:00:00:00:10"})
		}, "“Web” and “Again” forward the same port"},

		"dynamic DNS without provider": {func(c *Config) { c.Inbound.DDNS.Provider = "" }, "Choose a provider"},
		"dynamic DNS with another":     {func(c *Config) { c.Inbound.DDNS.Provider = "nsupdate" }, "unknown"},
		"a DuckDNS name elsewhere":     {func(c *Config) { c.Inbound.DDNS.Hostname = "home.example.com" }, "example.duckdns.org"},
		"DuckDNS without token":        {func(c *Config) { c.Inbound.DDNS.Password = "" }, "token"},
		"no name":                      {func(c *Config) { c.Inbound.DDNS.Hostname = "" }, "needs the name"},
		"dyndns2 without server": {func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSDyndns2, Hostname: "home.example.com", Username: "u", Password: "p"}
		}, "update server"},
		"dyndns2 with a path": {func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSDyndns2, Hostname: "home.example.com", Server: "dyn.example.com/nic", Username: "u", Password: "p"}
		}, "update server"},
		"dyndns2 without password": {func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSDyndns2, Hostname: "home.example.com", Server: "dyn.example.com", Username: "u"}
		}, "user name and password"},
		"Cloudflare without token": {func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSCloudflare, Hostname: "home.example.com"}
		}, "API token"},
		"custom without placeholder": {func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSCustom, Hostname: "home.example.com", URL: "https://dns.example.com/update?key=1"}
		}, "{ip}"},
		"custom over something else": {func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSCustom, Hostname: "home.example.com", URL: "ftp://dns.example.com/{ip}"}
		}, "http://"},
		"a control character":           {func(c *Config) { c.Inbound.DDNS.Password = "to\nken" }, "cannot be typed"},
		"address check over ftp":        {func(c *Config) { c.Inbound.CheckURL = "ftp://example.com/ip" }, "public address"},
		"address check with a new line": {func(c *Config) { c.Inbound.CheckURL = "http://example.com/\n" }, "public address"},
	}
	for name, tc := range cases {
		c := inbound()
		tc.edit(c)
		// Not normalized again: this is what a careless or hostile phone
		// could send, and Normalize would put some of it right.
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refused with %q, which does not say %q", name, err, tc.want)
		}
	}

	// What is allowed.
	ok := map[string]func(c *Config){
		"forwarding the server's port while the server is off": func(c *Config) {
			c.Inbound.Server.Enabled = false
			c.Inbound.Forwards[0].Proto, c.Inbound.Forwards[0].Ports, c.Inbound.Forwards[0].To = "udp", "51820", 0
		},
		"the same port for TCP and for UDP": func(c *Config) {
			c.Inbound.Forwards = append(c.Inbound.Forwards, PortForward{ID: "two", Name: "Again", Enabled: true, Proto: "udp", Ports: "8443", Device: "02:00:00:00:00:10"})
		},
		"the same port again, switched off": func(c *Config) {
			c.Inbound.Forwards = append(c.Inbound.Forwards, PortForward{ID: "two", Name: "Again", Proto: "tcp", Ports: "8443", Device: "02:00:00:00:00:10"})
		},
		"only from, both families": func(c *Config) {
			c.Inbound.Forwards[0].From = []string{"203.0.113.0/24", "2001:db8::/32", "198.51.100.7/32"}
		},
		"dynamic DNS filled in and off": func(c *Config) { c.Inbound.DDNS = DDNS{Provider: DDNSDyndns2, Hostname: "home.example.com"} },
		"dyndns2": func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSDyndns2, Hostname: "home.example.com", Server: "dyn.example.com:8443", Username: "u", Password: " p "}
		},
		"Cloudflare": func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSCloudflare, Hostname: "home.example.com", Password: "tok_en-1"}
		},
		"custom": func(c *Config) {
			c.Inbound.DDNS = DDNS{Enabled: true, Provider: DDNSCustom, Hostname: "home.example.com", URL: "https://dns.example.com/u?k=1&a={ip}&b={ip6}"}
		},
	}
	for name, edit := range ok {
		c := inbound()
		edit(c)
		if err := c.Validate(); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func TestInboundNormalize(t *testing.T) {
	c := inbound()
	c.Inbound.Forwards = []PortForward{{Name: " Web ", Enabled: true, Proto: " TCP ", Ports: " 8443 ", To: 8443, Device: strings.ToUpper("02:00:00:00:00:10"), From: []string{" 203.0.113.9 ", "", "2001:DB8::1/32"}}}
	c.Inbound.DDNS = DDNS{Enabled: true, Provider: " DuckDNS ", Hostname: "Example", Password: "token"}
	c.Normalize()
	f := c.Inbound.Forwards[0]
	if f.ID == "" || f.Name != "Web" || f.Proto != ProtoTCP || f.Ports != "8443" || f.To != 0 || f.Device != "02:00:00:00:00:10" ||
		len(f.From) != 2 || f.From[0] != "203.0.113.9/32" || f.From[1] != "2001:db8::/32" {
		t.Errorf("forward after normalizing: %+v", f)
	}
	if f.Range() != (PortRange{8443, 8443}) || f.Inside() != (PortRange{8443, 8443}) || !f.Carries("tcp") || f.Carries("udp") {
		t.Errorf("forward's ports: %v %v", f.Range(), f.Inside())
	}
	if d := c.Inbound.DDNS; d.Provider != DDNSDuckDNS || d.Hostname != "example.duckdns.org" || d.DuckName() != "example" {
		t.Errorf("dynamic DNS after normalizing: %+v", d)
	}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	c.Inbound.Forwards[0].To = 443
	if got := c.Inbound.Forwards[0].Inside(); got != (PortRange{443, 443}) {
		t.Errorf("port on the device: %v", got)
	}
	if !c.Inbound.InUse() {
		t.Error("a forward, a server and a name, and nothing is said to come in")
	}
	c.Inbound.Server.Enabled, c.Inbound.DDNS.Enabled, c.Inbound.Forwards[0].Enabled = false, false, false
	if c.Inbound.InUse() {
		t.Error("everything switched off, and something is said to come in")
	}
}
