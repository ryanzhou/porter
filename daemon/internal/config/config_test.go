package config

import (
	"encoding/json"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	c := Default()
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Ready() {
		t.Error("a fresh router must wait for setup before starting radios")
	}
}

func TestNoNullLists(t *testing.T) {
	c := Default()
	c.VPNs = []VPN{{ID: "a", Name: "A", WireGuard: WireGuard{Peers: []WGPeer{{}}}}}
	c.Policy.Rules = []Rule{{ID: "r", Action: ActDirect}}
	c.Normalize()
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "null") {
		t.Errorf("configuration JSON contains null: %s", b)
	}
}

func TestValidationRejects(t *testing.T) {
	cases := map[string]func(c *Config){
		"newline in SSID":       func(c *Config) { c.Wifi[0].SSID = "a\nb" },
		"short passphrase":      func(c *Config) { c.Wifi[0].Passphrase = "short" },
		"public LAN":            func(c *Config) { c.Networks[0].Address = "8.8.8.1/24" },
		"LAN network address":   func(c *Config) { c.Networks[0].Address = "10.0.0.0/24" },
		"non-ULA prefix":        func(c *Config) { c.Networks[0].ULAPrefix = "2001:db8::/64" },
		"unknown VPN in policy": func(c *Config) { c.Policy.Default = "vpn:nope" },
		"unknown VPN on device": func(c *Config) { c.Devices = []Device{{MAC: "02:00:00:00:00:01", Route: "vpn:nope"}} },
		"bad rule port":         func(c *Config) { c.Policy.Rules = []Rule{{ID: "r", Action: ActBlock, Dst: Dst{Ports: []string{"0"}}}} },
		"bad rule domain": func(c *Config) {
			c.Policy.Rules = []Rule{{ID: "r", Action: ActBlock, Dst: Dst{Domains: []string{"exa mple.com"}}}}
		},
		"https probe":            func(c *Config) { c.WAN.Probe.URL = "https://example.com/" },
		"multicast custom MAC":   func(c *Config) { c.WAN.EthMAC = MACPolicy{Mode: MACCustom, Address: "01:00:5e:00:00:01"} },
		"reservation outside":    func(c *Config) { c.Devices = []Device{{MAC: "02:00:00:00:00:01", IP: "192.168.1.5"}} },
		"SSH without keys":       func(c *Config) { c.System.SSH.Enabled = true },
		"DoH without bootstrap":  func(c *Config) { c.DNS.Upstreams = []string{"https://dns.example/dns-query"} },
		"lower-case country":     func(c *Config) { c.System.Country = "u1" },
		"bad radio role":         func(c *Config) { c.Radios[0].Role = "mesh" },
		"comma in blocked entry": func(c *Config) { c.DNS.Blocked = []string{"a,b.com"} },
	}
	for name, mutate := range cases {
		c := Default()
		mutate(c)
		c.Normalize()
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	c, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Wifi[0].SSID, c.Wifi[0].Passphrase, c.System.Country = "Travel", "correct horse", "AU"
	c.Normalize()
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.Wifi[0].SSID != "Travel" || !got.Ready() {
		t.Fatalf("reload: %+v %v", got, err)
	}
	// A corrupt file must not take the router down, nor be trusted.
	if err := WriteFileAtomic(filepath.Join(dir, "config.json"), []byte(`{"version":1,"ap":{"ssid":"x\ny"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load()
	if err == nil || got == nil || got.Wifi[0].SSID != "" {
		t.Fatalf("corrupt config: %+v %v", got, err)
	}
}

func TestParseUpstream(t *testing.T) {
	for in, want := range map[string]string{
		"1.1.1.1":                              "udp 1.1.1.1:53",
		"[2606:4700:4700::1111]:5353":          "udp [2606:4700:4700::1111]:5353",
		"tls://9.9.9.9#dns.quad9.net":          "tls 9.9.9.9:853 dns.quad9.net",
		"https://dns.example/q#192.0.2.1":      "https 192.0.2.1:443 dns.example",
		"https://dns.example:8443/q#192.0.2.1": "https 192.0.2.1:8443 dns.example",
	} {
		u, err := ParseUpstream(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		got := strings.TrimSpace(u.Proto + " " + u.Addr.String() + " " + u.ServerName)
		if got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
	for _, bad := range []string{"dns.example", "tls://dns.example", "https://dns.example/q", "1.1.1.1:99999"} {
		if _, err := ParseUpstream(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestMigrate(t *testing.T) {
	old := []byte(`{"version":1,"rev":18446744073709551615,"system":{"name":"Porter","hostname":"gone-in-2"},"wifi_name":"Mine"}`)
	steps := map[int]func(map[string]any) error{
		// Format 2 moved the Wi-Fi name and dropped a field.
		1: func(doc map[string]any) error {
			doc["ap"] = map[string]any{"ssid": doc["wifi_name"]}
			delete(doc, "wifi_name")
			delete(doc["system"].(map[string]any), "hostname")
			return nil
		},
		// Format 3 changed nothing that needs rewriting; format 4 did.
		3: func(doc map[string]any) error {
			doc["ap"].(map[string]any)["band"] = "5"
			return nil
		},
	}
	got, err := migrate(old, 4, steps)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ap":{"band":"5","ssid":"Mine"},"rev":18446744073709551615,"system":{"name":"Porter"},"version":4}`
	if string(got) != want {
		t.Errorf("migrated:\n%s\nwant:\n%s", got, want)
	}
	// Already current: untouched, byte for byte.
	if same, err := migrate(old, 1, steps); err != nil || string(same) != string(old) {
		t.Errorf("a current document was rewritten: %s %v", same, err)
	}
	// Written by a newer version than this one: refused, not guessed at.
	if _, err := migrate([]byte(`{"version":5}`), 4, steps); err == nil {
		t.Error("settings from a newer version were accepted")
	}
	if _, err := migrate([]byte(`{"version":0}`), 4, steps); err == nil {
		t.Error("a document without a version was accepted")
	}
	// A step that fails stops the migration.
	steps[2] = func(map[string]any) error { return errors.New("no") }
	if _, err := migrate(old, 4, steps); err == nil {
		t.Error("a failed step was ignored")
	}

	// What is stored today loads as it always did.
	dir := t.TempDir()
	st := NewStore(dir)
	c := Default()
	c.System.Name = "Kept"
	if err := st.Save(c); err != nil {
		t.Fatal(err)
	}
	back, err := st.Load()
	if err != nil || back.System.Name != "Kept" {
		t.Errorf("round trip through the store: %+v %v", back, err)
	}
}

func TestIPSetup(t *testing.T) {
	base := func() *Config {
		c := Default()
		c.System.Country, c.Wifi[0].SSID, c.Wifi[0].Passphrase = "AU", "net", "password1"
		c.Ethernet = []EthPort{{Name: "eth0", Role: EthWAN}}
		return c
	}
	// Nothing set means automatic, spelled out after normalising.
	c := base()
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.Ethernet[0].IP; got.V4.Mode != IPAuto || got.V6.Mode != IPAuto || got.V4.DNS == nil {
		t.Errorf("default setup: %+v", got)
	}

	good := IPSetup{
		V4: IPFamily{Mode: "Static", Address: " 192.168.1.50/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1", " "}},
		V6: IPFamily{Mode: IPStatic, Address: "2001:DB8::50/64", Gateway: "FE80::1"},
	}
	c = base()
	c.Ethernet[0].IP = good
	c.WAN.Networks = []WifiNetwork{{SSID: "Hotel", IP: good}}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatalf("a correct static setup was refused: %v", err)
	}
	got := c.WAN.Networks[0].IP
	if got.V4.Mode != IPStatic || got.V4.Address != "192.168.1.50/24" || len(got.V4.DNS) != 1 || got.V6.Address != "2001:db8::50/64" || got.V6.Gateway != "fe80::1" {
		t.Errorf("normalised: %+v", got)
	}
	// Going back to automatic drops what was typed.
	c.Ethernet[0].IP.V4.Mode = IPAuto
	c.Normalize()
	if c.Ethernet[0].IP.V4.Address != "" || c.Ethernet[0].IP.V4.Gateway != "" {
		t.Errorf("automatic kept static values: %+v", c.Ethernet[0].IP.V4)
	}

	v4 := func(addr, gw string, dns ...string) IPSetup {
		return IPSetup{V4: IPFamily{Mode: IPStatic, Address: addr, Gateway: gw, DNS: dns}}
	}
	v6 := func(addr, gw string, dns ...string) IPSetup {
		return IPSetup{V6: IPFamily{Mode: IPStatic, Address: addr, Gateway: gw, DNS: dns}}
	}
	bad := map[string]IPSetup{
		"no prefix length":          v4("192.168.1.50", "192.168.1.1", "1.1.1.1"),
		"gateway outside":           v4("192.168.1.50/24", "192.168.2.1", "1.1.1.1"),
		"gateway is self":           v4("192.168.1.50/24", "192.168.1.50", "1.1.1.1"),
		"no gateway":                v4("192.168.1.50/24", "", "1.1.1.1"),
		"network address":           v4("192.168.1.0/24", "192.168.1.1", "1.1.1.1"),
		"broadcast address":         v4("192.168.1.255/24", "192.168.1.1", "1.1.1.1"),
		"no DNS":                    v4("192.168.1.50/24", "192.168.1.1"),
		"IPv6 DNS in the IPv4 list": v4("192.168.1.50/24", "192.168.1.1", "2001:db8::1"),
		"IPv6 address as IPv4":      v4("2001:db8::50/64", "2001:db8::1", "1.1.1.1"),
		"loopback":                  v4("127.0.0.5/8", "127.0.0.1", "1.1.1.1"),
		"too many DNS":              v4("192.168.1.50/24", "192.168.1.1", "1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4"),
		"v6 without prefix":         v6("2001:db8::50", "fe80::1"),
		"v6 link-local as address":  v6("fe80::50/64", "fe80::1"),
		"v6 gateway outside":        v6("2001:db8::50/64", "2001:db9::1"),
		"v6 IPv4 gateway":           v6("2001:db8::50/64", "192.168.1.1"),
		"v6 gateway with a zone":    v6("2001:db8::50/64", "fe80::1%eth0"),
		"v6 IPv4 DNS":               v6("2001:db8::50/64", "fe80::1", "1.1.1.1"),
		"unknown mode":              {V4: IPFamily{Mode: "manual"}},
	}
	for name, setup := range bad {
		c := base()
		c.Ethernet[0].IP = setup
		c.Normalize()
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A static IPv4 setup may leave DNS out when servers are set globally.
	c = base()
	c.DNS.Upstreams = []string{"9.9.9.9"}
	c.Ethernet[0].IP = v4("192.168.1.50/24", "192.168.1.1")
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Errorf("static IPv4 without its own DNS, with global servers: %v", err)
	}
	// A /31 has no network or broadcast address.
	c = base()
	c.Ethernet[0].IP = v4("192.0.2.0/31", "192.0.2.1", "1.1.1.1")
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Errorf("/31: %v", err)
	}
}

func TestMPQUICProfile(t *testing.T) {
	base := func(m MPQUIC) *Config {
		c := Default()
		c.System.Country, c.Wifi[0].SSID, c.Wifi[0].Passphrase = "AU", "net", "password1"
		c.VPNs = []VPN{{ID: "mq", Name: "Multi", Enabled: true, Type: "MPQUIC", MPQUIC: m}}
		c.Normalize()
		return c
	}
	c := base(MPQUIC{Servers: []string{" 203.0.113.10 ", "relay.example.net:9000", "[2001:db8::1]:8388", ""}, Password: "long-random-password", Up: "19Mbit"})
	if err := c.Validate(); err != nil {
		t.Fatalf("a correct profile was refused: %v", err)
	}
	m := c.VPNs[0].MPQUIC
	if c.VPNs[0].Type != VPNMPQUIC || len(m.Servers) != 3 || m.Port != DefaultMPQUICPort || len(m.DNS) != 2 || m.Up != "19mbit" {
		t.Errorf("normalised: %+v", c.VPNs[0])
	}
	// No WireGuard section is needed for it.
	if c.VPNs[0].WireGuard.PrivateKey != "" {
		t.Error("unexpected WireGuard settings")
	}
	for name, bad := range map[string]MPQUIC{
		"no servers":     {Password: "long-random-password"},
		"short password": {Servers: []string{"a.example"}, Password: "short"},
		"bad host":       {Servers: []string{"not a host"}, Password: "long-random-password"},
		"bad port":       {Servers: []string{"a.example:99999"}, Password: "long-random-password"},
		"empty port":     {Servers: []string{"a.example:"}, Password: "long-random-password"},
		"bad rate":       {Servers: []string{"a.example"}, Password: "long-random-password", Down: "fast"},
		"bad DNS":        {Servers: []string{"a.example"}, Password: "long-random-password", DNS: []string{"dns.example"}},
		"too many paths": {Servers: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, Password: "long-random-password"},
	} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A profile saved before there were types is WireGuard.
	c = Default()
	c.VPNs = []VPN{{ID: "wg", Name: "Old"}}
	c.Normalize()
	if c.VPNs[0].Type != VPNWireGuard {
		t.Errorf("type of an old profile: %q", c.VPNs[0].Type)
	}
	c.VPNs[0].Type = "openvpn"
	if err := c.Validate(); err == nil {
		t.Error("an unknown type was accepted")
	}
}

// A router that updates itself from the version before networks, several
// Wi-Fi networks and the ranking of uplinks keeps everything it was set up
// with.
func TestFormat1Carries(t *testing.T) {
	ip := `{"v4":{"mode":"auto","address":"","gateway":"","dns":[]},"v6":{"mode":"auto","address":"","gateway":"","dns":[]}}`
	old := `{"version":1,"rev":7,
 "system":{"name":"Porter","country":"AU","ssh":{"enabled":false,"authorized_keys":[]}},
 "radios":[{"id":"builtin","role":"ap+wan","band":"5","channel":0,"width":0},{"id":"usb:02:00:00:00:00:77","role":"wan","band":"","channel":0,"width":0}],
 "ethernet":[{"name":"eth0","role":"wan","ip":` + ip + `},{"name":"end0","role":"lan","ip":` + ip + `}],
 "ap":{"ssid":"Travel","passphrase":"correct horse","security":"wpa2+wpa3","hidden":false,"isolate":true},
 "wan":{"networks":[{"ssid":"Hotel","psk":"","hidden":false,"priority":1,"ip":` + ip + `}],
  "wifi_mac":{"mode":"hardware"},"eth_mac":{"mode":"hardware"},"prefer_wifi":true,
  "probe":{"url":"http://captive.apple.com/hotspot-detect.html","expect":"Success"}},
 "lan":{"address":"10.84.0.1/24","auto_renumber":true,"lease_minutes":720,"domain":"lan","ipv6":true,"ula_prefix":"fd12:3456:789a:1::/64"},
 "dns":{"upstreams":[],"intercept":true,"blocked":[],"adblock":{"enabled":false,"lists":[],"allow":[]}},
 "vpns":[],"policy":{"default":"direct","rules":[]},
 "devices":[{"mac":"02:00:00:00:00:01","name":"TV","route":"","paused":false,"ip":"10.84.0.20","no_adblock":false}],
 "speedtest":{"download_url":"https://speed.cloudflare.com/__down?bytes=50000000","upload_url":"https://speed.cloudflare.com/__up"}}`
	dir := t.TempDir()
	if err := WriteFileAtomic(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewStore(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	// Format 1 let two radios be uplinks; only the first still is, so
	// that the configuration can be changed at all.
	if c.Radios[0].Role != RoleAPWAN || c.Radios[1].Role != RoleOff || c.Consistent() != nil {
		t.Errorf("radios after migration: %+v (%v)", c.Radios, c.Consistent())
	}
	w := c.Wifi[0]
	if len(c.Wifi) != 1 || w.ID != MainSSID || w.SSID != "Travel" || w.Passphrase != "correct horse" || w.Security != SecWPA2WPA3 || !w.Isolate || !w.Enabled || w.Network != MainNetwork {
		t.Errorf("Wi-Fi network: %+v", c.Wifi)
	}
	n := c.Networks[0]
	if len(c.Networks) != 1 || n.ID != MainNetwork || n.Address != "10.84.0.1/24" || n.ULAPrefix != "fd12:3456:789a:1::/64" || n.Name == "" {
		t.Errorf("network: %+v", c.Networks)
	}
	if !c.LAN.AutoRenumber || c.LAN.LeaseMinutes != 720 || c.LAN.Domain != "lan" || !c.LAN.IPv6 {
		t.Errorf("LAN settings: %+v", c.LAN)
	}
	// Wi-Fi was preferred over the wired ports.
	if got := strings.Join(c.WAN.Order, " "); got != "wifi eth:eth0 eth:end0 phone" {
		t.Errorf("order of uplinks: %s", got)
	}
	if c.Ethernet[1].Role != EthLAN || c.Ethernet[1].Network != MainNetwork {
		t.Errorf("LAN port: %+v", c.Ethernet[1])
	}
	if !c.Ready() || c.Version != SchemaVersion || len(c.Devices) != 1 || c.Devices[0].IP != "10.84.0.20" || len(c.WAN.Networks) != 1 {
		t.Errorf("the rest: %+v", c)
	}
}

// Format 3 added a field and rewrote nothing: what a router stored as
// format 2 loads as it was, with control over its networks on.
func TestFormat2Carries(t *testing.T) {
	c := ready()
	c.System.Name, c.Rev = "Kept", 41
	c.Devices = []Device{{MAC: "02:00:00:00:00:01", Name: "TV", IP: "10.83.0.20"}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	// As format 2 wrote it: its own number, and no such field.
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	doc["version"] = 2
	delete(doc["system"].(map[string]any), "bluetooth_only")
	old, _ := json.Marshal(doc)
	dir := t.TempDir()
	if err := WriteFileAtomic(filepath.Join(dir, "config.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewStore(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 3 || got.System.BluetoothOnly {
		t.Errorf("format %d, bluetooth only %v", got.Version, got.System.BluetoothOnly)
	}
	want := c.Clone()
	want.Version = 3
	if a, b := mustJSON(got), mustJSON(want); a != b {
		t.Errorf("a format 2 document changed on the way in:\n%s\nwant:\n%s", a, b)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func ready() *Config {
	c := Default()
	c.System.Country, c.Wifi[0].SSID, c.Wifi[0].Passphrase = "AU", "net", "password1"
	c.Normalize()
	return c
}

func TestNetworks(t *testing.T) {
	c := ready()
	// A network and a Wi-Fi network added with nothing but a name get
	// what they need.
	c.Networks = append(c.Networks, Network{ID: "guest", Name: "Guest", Isolated: true})
	c.Wifi = append(c.Wifi, SSID{SSID: "net-guest", Passphrase: "password2", Network: "guest", Enabled: true})
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	g := c.Network("guest")
	if g.Address != "10.84.0.1/24" || g.ULAPrefix == "" || g.ULAPrefix == c.Main().ULAPrefix || c.Wifi[1].ID == "" || c.Wifi[1].Security != SecWPA2 {
		t.Errorf("defaults: %+v %+v", g, c.Wifi[1])
	}
	if n := c.NetworkOf(netip.MustParseAddr("10.84.0.77")); n == nil || n.ID != "guest" {
		t.Errorf("NetworkOf: %+v", n)
	}
	// A device may be given an address in any of them, but not the
	// router's own.
	c.Devices = []Device{{MAC: "02:00:00:00:00:01", IP: "10.84.0.20"}}
	if err := c.Validate(); err != nil {
		t.Errorf("reservation in the second network: %v", err)
	}
	c.Devices[0].IP = "10.84.0.1"
	if err := c.Validate(); err == nil {
		t.Error("the router's own address was reserved for a device")
	}
	c.Devices = nil

	bad := map[string]func(c *Config){
		"overlapping subnets":     func(c *Config) { c.Networks[1].Address = "10.83.0.129/25" },
		"shared IPv6 prefix":      func(c *Config) { c.Networks[1].ULAPrefix = c.Networks[0].ULAPrefix },
		"two networks, one id":    func(c *Config) { c.Networks = append(c.Networks, Network{ID: "guest", Name: "Again"}) },
		"Wi-Fi on no network":     func(c *Config) { c.Wifi[1].Network = "gone" },
		"two Wi-Fi with one name": func(c *Config) { c.Wifi[1].SSID = "net" },
		"second Wi-Fi unnamed":    func(c *Config) { c.Wifi[1].SSID = "" },
		"second Wi-Fi open":       func(c *Config) { c.Wifi[1].Passphrase = "" },
		"port on no network":      func(c *Config) { c.Ethernet[0].Role, c.Ethernet[0].Network = EthLAN, "gone" },
		"rule for no network": func(c *Config) {
			c.Policy.Rules = []Rule{{ID: "r", Action: ActBlock, Src: Src{Networks: []string{"gone"}}}}
		},
		"route through no VPN": func(c *Config) { c.Networks[1].Route = "vpn:nope" },
		"unnamed network":      func(c *Config) { c.Networks[1].Name = " " },
		"too many networks": func(c *Config) {
			for i := 0; i < MaxNetworks; i++ {
				c.Networks = append(c.Networks, Network{Name: "More"})
			}
		},
		"main network removed": func(c *Config) { c.Networks = c.Networks[1:]; c.Networks[0].ID = "guest" },
	}
	for name, mutate := range bad {
		next := c.Clone()
		mutate(next)
		if name != "main network removed" {
			next.Normalize()
		}
		if err := next.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A rule may name a network, and a network may have a route.
	c.Policy.Rules = []Rule{{ID: "r", Enabled: true, Action: ActBlock, Src: Src{Networks: []string{"guest"}}}}
	c.Networks[1].Route = ActBlock
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	if c.Policy.Rules[0].Src.Any() {
		t.Error("a rule for one network matches everything")
	}
}

// A list of networks sent in another order is refused; the main one is
// not quietly made out of whichever came first.
func TestFirstIDsAreNotRewritten(t *testing.T) {
	c := ready()
	c.Networks = append(c.Networks, Network{ID: "guest", Name: "Guest", Address: "10.99.0.1/24", Isolated: true})
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Networks[0], c.Networks[1] = c.Networks[1], c.Networks[0]
	c.Normalize()
	if c.Networks[0].ID != "guest" {
		t.Errorf("first network renamed to %q", c.Networks[0].ID)
	}
	if err := c.Validate(); err == nil {
		t.Error("networks in another order accepted")
	}
	// Addresses nothing can have.
	c = ready()
	c.Networks[0].Address = "10.83.0.255/24"
	if err := c.Validate(); err == nil {
		t.Error("router on the broadcast address accepted")
	}
	c = ready()
	c.Devices = []Device{{MAC: "02:00:00:00:00:01", IP: "10.83.0.255"}}
	if err := c.Validate(); err == nil {
		t.Error("reservation of the broadcast address accepted")
	}
}

func TestConsistent(t *testing.T) {
	c := ready()
	if err := c.Consistent(); err != nil {
		t.Fatal(err)
	}
	usb := Radio{ID: "usb:00:11:22:33:44:55", Role: RoleWAN}
	cases := map[string]struct {
		mutate func(c *Config)
		ok     bool
	}{
		"one radio for both":         {func(c *Config) { c.Radios[0].Role = RoleAPWAN }, true},
		"a second radio as uplink":   {func(c *Config) { c.Radios = append(c.Radios, usb) }, true},
		"both radios broadcasting":   {func(c *Config) { u := usb; u.Role = RoleAP; c.Radios = append(c.Radios, u) }, true},
		"two radios as uplink":       {func(c *Config) { c.Radios[0].Role = RoleAPWAN; c.Radios = append(c.Radios, usb) }, false},
		"no radio for the network":   {func(c *Config) { c.Radios[0].Role = RoleWAN }, false},
		"radios off":                 {func(c *Config) { c.Radios[0].Role = RoleOff }, false},
		"radios off, a wired port":   {func(c *Config) { c.Radios[0].Role = RoleOff; c.Ethernet[0].Role = EthLAN }, true},
		"every Wi-Fi network off":    {func(c *Config) { c.Wifi[0].Enabled = false }, false},
		"Wi-Fi off, but a LAN port":  {func(c *Config) { c.Wifi[0].Enabled = false; c.Ethernet[0].Role = EthLAN }, true},
		"not set up yet: no opinion": {func(c *Config) { c.Radios[0].Role = RoleWAN; c.System.Country = "" }, true},
	}
	for name, tc := range cases {
		next := c.Clone()
		tc.mutate(next)
		next.Normalize()
		if err := next.Validate(); err != nil {
			t.Errorf("%s: not even valid: %v", name, err)
		}
		if err := next.Consistent(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestUplinkOrder(t *testing.T) {
	c := ready()
	if got := strings.Join(c.WAN.Order, " "); got != "eth:eth0 wifi phone" {
		t.Fatalf("default order: %s", got)
	}
	// A port that turns up is ranked with the other wired ports; one that
	// is ranked twice, or is not a port, goes.
	c.Ethernet = append(c.Ethernet, EthPort{Name: "end0", Role: EthWAN})
	c.WAN.Order = []string{"phone", "wifi", "eth:eth0", "wifi", "eth:gone"}
	c.Normalize()
	if got := strings.Join(c.WAN.Order, " "); got != "phone wifi eth:eth0 eth:end0" {
		t.Errorf("order: %s", got)
	}
	// With no wired port ranked yet, a new one goes ahead of Wi-Fi.
	c.WAN.Order = []string{"phone", "wifi"}
	c.Normalize()
	if got := strings.Join(c.WAN.Order, " "); got != "phone eth:eth0 eth:end0 wifi" {
		t.Errorf("order: %s", got)
	}
	c.WAN.Order = []string{"wifi", "satellite"}
	if err := c.Validate(); err == nil {
		t.Error("an unknown kind of uplink was accepted")
	}
	// A Wi-Fi interface once mistaken for a wired port is forgotten.
	c.Ethernet = append(c.Ethernet, EthPort{Name: "ap-wlan0", Role: EthWAN}, EthPort{Name: "ap1", Role: EthWAN}, EthPort{Name: "wlx001122334455", Role: EthLAN})
	c.WAN.Order = []string{"eth:ap-wlan0", "eth:eth0", "wifi"}
	c.Normalize()
	if len(c.Ethernet) != 2 || strings.Contains(strings.Join(c.WAN.Order, " "), "ap") {
		t.Errorf("after forgetting: %+v %v", c.Ethernet, c.WAN.Order)
	}
}

func TestSetWifiUplink(t *testing.T) {
	const usb = "usb:00:11:22:33:44:55"
	roles := func(c *Config) string {
		var out []string
		for _, r := range c.Radios {
			out = append(out, r.Role)
		}
		return strings.Join(out, " ")
	}
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	onlyBuiltin := func(id string) bool { return id == "builtin" }

	// One radio: it does both jobs, and stops when the uplink is turned
	// off.
	c := ready()
	if err := c.SetWifiUplink("builtin", all, all); err != nil || roles(c) != RoleAPWAN {
		t.Errorf("one radio: %s %v", roles(c), err)
	}
	if err := c.SetWifiUplink("", all, all); err != nil || roles(c) != RoleAP {
		t.Errorf("uplink off: %s %v", roles(c), err)
	}
	// One radio that cannot do both: refused, and nothing is changed.
	if err := c.SetWifiUplink("builtin", all, none); err == nil || roles(c) != RoleAP {
		t.Errorf("a radio that cannot do both: %s %v", roles(c), err)
	}

	// Two radios: the one chosen only joins, the other carries the
	// network; choosing the other swaps them in one step.
	c.Radios = append(c.Radios, Radio{ID: usb, Role: RoleOff})
	if err := c.SetWifiUplink(usb, all, all); err != nil || roles(c) != RoleAP+" "+RoleWAN {
		t.Errorf("adapter as uplink: %s %v", roles(c), err)
	}
	if err := c.SetWifiUplink("builtin", all, all); err != nil || roles(c) != RoleWAN+" "+RoleAP {
		t.Errorf("swapped: %s %v", roles(c), err)
	}
	if err := c.Consistent(); err != nil {
		t.Errorf("after the swap: %v", err)
	}
	// Uplink off: both carry the network.
	if err := c.SetWifiUplink("", all, all); err != nil || roles(c) != RoleAP+" "+RoleAP {
		t.Errorf("uplink off with two radios: %s %v", roles(c), err)
	}
	// The other radio is remembered but unplugged: it does not count as
	// carrying the network, so the one present does both.
	if err := c.SetWifiUplink("builtin", onlyBuiltin, all); err != nil || c.Radios[0].Role != RoleAPWAN {
		t.Errorf("with the adapter unplugged: %s %v", roles(c), err)
	}
	if err := c.SetWifiUplink(usb, onlyBuiltin, all); err == nil {
		t.Error("a radio that is not there was made the uplink")
	}
}

func TestTaggedPortAndPPPoE(t *testing.T) {
	port := func(c *Config) *EthPort { return &c.Ethernet[0] }
	check := func(mutate func(c *Config)) error {
		c := Default()
		mutate(c)
		c.Normalize()
		return c.Validate()
	}
	// A router that has never heard of either reads as neither.
	var old Config
	if err := json.Unmarshal([]byte(`{"version":3,"ethernet":[{"name":"eth0","role":"wan"}]}`), &old); err != nil {
		t.Fatal(err)
	}
	old.Normalize()
	if e := old.Ethernet[0]; e.VLAN != 0 || e.PPPoE != (PPPoE{}) {
		t.Errorf("a port stored without the fields: %+v", e)
	}

	for _, id := range []int{0, 1, 35, 4094} {
		if err := check(func(c *Config) { port(c).VLAN = id }); err != nil {
			t.Errorf("VLAN %d: %v", id, err)
		}
	}
	for _, id := range []int{-1, 4095, 70000} {
		if err := check(func(c *Config) { port(c).VLAN = id }); err == nil {
			t.Errorf("VLAN %d accepted", id)
		}
	}

	login := PPPoE{Enabled: true, Username: "user@provider.example", Password: `p"a\ss word#1`}
	if err := check(func(c *Config) { port(c).PPPoE, port(c).VLAN = login, 7 }); err != nil {
		t.Errorf("PPPoE on a tagged port: %v", err)
	}
	static4 := IPFamily{Mode: IPStatic, Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}
	static6 := IPFamily{Mode: IPStatic, Address: "2001:db8::10/64", Gateway: "fe80::1"}
	for what, mutate := range map[string]func(c *Config){
		"no user name":            func(c *Config) { port(c).PPPoE = PPPoE{Enabled: true, Password: "secret"} },
		"no password":             func(c *Config) { port(c).PPPoE = PPPoE{Enabled: true, Username: "user"} },
		"a user name of spaces":   func(c *Config) { port(c).PPPoE = PPPoE{Enabled: true, Username: "  ", Password: "secret"} },
		"a newline in the name":   func(c *Config) { port(c).PPPoE = PPPoE{Enabled: true, Username: "us\ner", Password: "secret"} },
		"a newline in the secret": func(c *Config) { port(c).PPPoE = PPPoE{Enabled: true, Username: "user", Password: "sec\nret"} },
		"a very long user name": func(c *Config) {
			port(c).PPPoE = PPPoE{Enabled: true, Username: strings.Repeat("u", 129), Password: "x"}
		},
		"a very long password": func(c *Config) {
			port(c).PPPoE = PPPoE{Enabled: true, Username: "u", Password: strings.Repeat("p", 129)}
		},
		"with a fixed IPv4 as well": func(c *Config) { port(c).PPPoE, port(c).IP.V4 = login, static4 },
		"with a fixed IPv6 as well": func(c *Config) { port(c).PPPoE, port(c).IP.V6 = login, static6 },
	} {
		if err := check(mutate); err == nil {
			t.Errorf("PPPoE %s: accepted", what)
		}
	}

	// Switched off, the sign-in is not kept; the tag is, whatever the
	// port is used for.
	c := Default()
	port(c).PPPoE = PPPoE{Username: " user ", Password: " secret "}
	port(c).VLAN, port(c).Role = 35, EthLAN
	c.Normalize()
	if port(c).PPPoE != (PPPoE{}) || port(c).VLAN != 35 {
		t.Errorf("after normalizing: %+v", port(c))
	}
	// A user name loses the spaces around it, a password does not.
	port(c).PPPoE = PPPoE{Enabled: true, Username: " user ", Password: " secret "}
	c.Normalize()
	if port(c).PPPoE.Username != "user" || port(c).PPPoE.Password != " secret " {
		t.Errorf("trimming: %+v", port(c).PPPoE)
	}
}

func TestMadeInterfaceNames(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{VLANIfName("eth0", 35), "v35-eth0"},
		{VLANIfName("end0", 4094), "v4094-end0"},
		{VLANIfName("enp1s0f0", 4094), "v4094-enp1s0f0"},
		{PPPIfName("eth0"), "ppp-eth0"},
		{PPPIfName("enp1s0f0np0"), "ppp-enp1s0f0np0"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %s, want %s", tc.got, tc.want)
		}
	}
	// A port whose name leaves no room: still 15 bytes at most, still
	// one name per port and tag, the same every time.
	const a, b = "enx3a1f0c2299d4", "enx3a1f0c2299d5"
	seen := map[string]string{}
	for _, port := range []string{a, b} {
		for _, name := range []string{VLANIfName(port, 7), VLANIfName(port, 4094), PPPIfName(port)} {
			if len(name) > 15 || !reIface.MatchString(name) {
				t.Errorf("%q is not an interface name", name)
			}
			if other, taken := seen[name]; taken {
				t.Errorf("%q names both %s and %s", name, other, port)
			}
			seen[name] = port
		}
	}
	if VLANIfName(a, 7) != VLANIfName(a, 7) || !strings.HasPrefix(VLANIfName(a, 7), "v7-") || !strings.HasPrefix(PPPIfName(a), "ppp-") {
		t.Errorf("names for a long port name: %s %s", VLANIfName(a, 7), PPPIfName(a))
	}

	// None of them is ever taken for a wired port, and no port's own name
	// is taken for one of them.
	for name := range seen {
		if !MadeIf(name) {
			t.Errorf("%s not recognised as made by the router", name)
		}
	}
	for _, name := range []string{"v35-eth0", "v1-lan1", "ppp-eth0", "ppp0"} {
		if !MadeIf(name) {
			t.Errorf("%s not recognised as made by the router", name)
		}
	}
	for _, name := range []string{"eth0", "end0", "lan1", "usb0", "enx3a1f0c2299d4", "enp1s0", "veth0", "vlan1", "v6"} {
		if MadeIf(name) {
			t.Errorf("the port %s taken for an interface the router made", name)
		}
	}
	// One remembered as a port by mistake is forgotten, with its place
	// in the order.
	c := Default()
	c.Ethernet = append(c.Ethernet, EthPort{Name: "v35-eth0", Role: EthWAN}, EthPort{Name: "ppp-eth0", Role: EthWAN})
	c.WAN.Order = []string{"eth:v35-eth0", "eth:eth0", "eth:ppp-eth0", "wifi", "phone"}
	c.Normalize()
	if len(c.Ethernet) != 1 || strings.Join(c.WAN.Order, " ") != "eth:eth0 wifi phone" {
		t.Errorf("after forgetting: %+v %v", c.Ethernet, c.WAN.Order)
	}
}
