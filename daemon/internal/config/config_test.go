package config

import (
	"encoding/json"
	"errors"
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
		"newline in SSID":       func(c *Config) { c.AP.SSID = "a\nb" },
		"short passphrase":      func(c *Config) { c.AP.Passphrase = "short" },
		"public LAN":            func(c *Config) { c.LAN.Address = "8.8.8.1/24" },
		"LAN network address":   func(c *Config) { c.LAN.Address = "10.0.0.0/24" },
		"non-ULA prefix":        func(c *Config) { c.LAN.ULAPrefix = "2001:db8::/64" },
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
	c.AP.SSID, c.AP.Passphrase, c.System.Country = "Travel", "correct horse", "AU"
	c.Normalize()
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.AP.SSID != "Travel" || !got.Ready() {
		t.Fatalf("reload: %+v %v", got, err)
	}
	// A corrupt file must not take the router down, nor be trusted.
	if err := WriteFileAtomic(filepath.Join(dir, "config.json"), []byte(`{"version":1,"ap":{"ssid":"x\ny"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load()
	if err == nil || got == nil || got.AP.SSID != "" {
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
