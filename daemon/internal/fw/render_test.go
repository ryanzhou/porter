package fw

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

func testConfig() *config.Config {
	c := config.Default()
	c.VPNs = []config.VPN{
		{ID: "home", Name: "Home", Enabled: true, KillSwitch: true},
		{ID: "work", Name: "Work", Enabled: true},
		{ID: "off", Name: "Off", Enabled: false, KillSwitch: true},
	}
	c.Devices = []config.Device{
		{MAC: "02:00:00:00:00:01", Route: "vpn:home"},
		{MAC: "02:00:00:00:00:02", Paused: true},
		{MAC: "02:00:00:00:00:03", Route: "block"},
	}
	c.Policy.Rules = []config.Rule{
		{ID: "r1", Enabled: true, Dst: config.Dst{Domains: []string{"example.com"}, CIDRs: []string{"198.51.100.0/24", "2001:db8::/32"}}, Action: "vpn:work"},
		{ID: "r2", Enabled: true, Src: config.Src{Devices: []string{"02:00:00:00:00:01"}, CIDRs: []string{"10.83.0.50", "fd00::/64"}}, Dst: config.Dst{Proto: "tcp", Ports: []string{"443", "8000-8100"}}, Action: "direct"},
		{ID: "r3", Enabled: true, Dst: config.Dst{Ports: []string{"25"}}, Action: "block"},
		{ID: "r4", Enabled: false, Action: "block"},
		{ID: "r5", Enabled: true, Dst: config.Dst{CIDRs: []string{"203.0.113.9"}}, Action: "vpn:off"},
		{ID: "r6", Enabled: true, Dst: config.Dst{Countries: []string{"JP", "NZ"}, Proto: "tcp"}, Action: "vpn:home"},
	}
	c.Networks = append(c.Networks,
		config.Network{ID: "guest", Name: "Guest", Address: "10.84.0.1/24", ULAPrefix: "fd00:0:0:84::/64", Isolated: true, Route: "vpn:work"},
		config.Network{ID: "iot", Name: "Things", Address: "10.85.0.1/24", ULAPrefix: "fd00:0:0:85::/64", Route: "block"},
	)
	c.Policy.Rules = append(c.Policy.Rules,
		config.Rule{ID: "r7", Enabled: true, Src: config.Src{Networks: []string{"guest", "iot"}}, Dst: config.Dst{Ports: []string{"9000"}}, Action: "block"})
	c.System.SSH = config.SSH{Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAA test"}}
	return c
}

func TestRender(t *testing.T) {
	c := testConfig()
	if err := c.Validate(); err != nil {
		// VPN keys are missing on purpose; everything else must be valid.
		if !strings.Contains(err.Error(), "private key") {
			t.Fatal(err)
		}
	}
	p := BuildPlan(c, Runtime{
		WANIfs:     []string{"eth0", "wlan1"},
		PortalDevs: []string{"02:00:00:00:00:09"},
		GeoVersion: "2026-11",
		Geo: func(cc string) ([]string, []string) {
			switch cc {
			case "JP":
				return []string{"198.51.100.0-198.51.100.255", "192.0.2.0-192.0.2.127"}, []string{"2001:db8::-2001:db8:ffff:ffff:ffff:ffff:ffff:ffff"}
			case "NZ":
				return []string{"203.0.113.7", "192.0.2.128-192.0.2.255"}, nil
			}
			return nil, nil
		},
		Learned: map[string][]Elem{
			SetName("r1", false): {{Addr: netip.MustParseAddr("93.184.216.34"), TTL: 300}},
			SetName("r1", true):  {{Addr: netip.MustParseAddr("2606:2800:220:1::1"), TTL: 60}},
		},
	})
	out := Render(p)
	for _, want := range []string{
		`ether saddr @portal_devs meta mark set 0x101`,
		`ether saddr @paused meta mark set 0x2`,
		`02:00:00:00:00:01 : 0x10`,
		`02:00:00:00:00:03 : 0x2`,
		`93.184.216.34 timeout 300s`,
		`meta l4proto tcp th dport { 443, 8000-8100 } meta mark set 0x1`,
		`meta mark & 0xff == 0x10 oifname "wg-home" accept`,
		`meta mark & 0xff == 0x10 jump refuse`,
		`meta mark & 0xff == 0x12 jump refuse`,
		`set r_r6_g4 { type ipv4_addr; flags interval; auto-merge; elements = { 198.51.100.0-198.51.100.255, 192.0.2.0-192.0.2.127, 203.0.113.7, 192.0.2.128-192.0.2.255 } }`,
		`meta l4proto tcp ip daddr @r_r6_g4 meta mark set 0x10`,
		`meta l4proto tcp ip6 daddr @r_r6_g6 meta mark set 0x10`,
		// Several networks.
		`set lan_ifs { type ifname; elements = { "br-guest", "br-iot", "br-lan" } }`,
		`set lan_nets4 { type ipv4_addr; flags interval; elements = { 10.83.0.0/24, 10.84.0.0/24, 10.85.0.0/24 } }`,
		`ip daddr @lan_nets4 return`,
		`iifname { "br-guest", "br-iot" } jump r_r7`,
		`meta mark set iifname map { "br-guest" : 0x11, "br-iot" : 0x2 } ct mark set meta mark accept`,
		`iifname @lan_ifs oifname @lan_ifs jump lan_lan`,
		`iifname { "br-guest" } jump refuse`,
		`iifname { "br-iot", "br-lan" } tcp dport 22 accept`,
		// The app over the network: the same networks as SSH, the
		// isolated one left out.
		`iifname { "br-iot", "br-lan" } tcp dport 7678 accept`,
		// What the router sends itself for a kill-switched tunnel leaves
		// by that tunnel or not at all.
		`meta mark & 0xff == 0x10 oifname != "wg-home" jump refuse`,
		`meta mark & 0xff == 0x0 jump refuse`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	// A tunnel that is switched off, kill switch on: its mark goes nowhere
	// from the router itself either.
	if !strings.Contains(out, "\t\tmeta mark & 0xff == 0x12 jump refuse\n\t}\n") {
		t.Error("no output rule for the switched-off kill-switched VPN")
	}
	// Order matters in three places.
	for _, pair := range [][2]string{
		{`ether saddr @paused meta mark set 0x2`, `ether saddr @portal_devs`},                          // paused before the sign-in pass
		{"chain lan_lan {\n\t\tether saddr @paused jump refuse", `iifname { "br-guest" } jump refuse`}, // paused before anything in lan_lan
		{`meta mark & 0xff == 0x0 jump refuse`, `oifname @wan_ifs accept`},                             // unclassified refused before the uplink accept
	} {
		a, b := strings.Index(out, pair[0]), strings.LastIndex(out, pair[1])
		if a < 0 || b < 0 || a > b {
			t.Errorf("%q does not come before %q", pair[0], pair[1])
		}
	}
	// Who may reach the router, and the list of uplinks, are not part of
	// what decides a connection's way.
	alt := *p
	alt.SSH, alt.Control, alt.Intercept, alt.WANIfs = !p.SSH, !p.Control, !p.Intercept, []string{"eth9"}
	if ClassKey(&alt) != ClassKey(p) || Fingerprint(&alt) == Fingerprint(p) {
		t.Error("ClassKey follows changes that reroute nothing")
	}
	alt = *p
	alt.Paused = append([]string{"02:00:00:00:00:aa"}, p.Paused...)
	if ClassKey(&alt) == ClassKey(p) {
		t.Error("ClassKey misses a paused device")
	}
	if strings.Contains(out, `"wg-off"`) {
		t.Error("switched-off VPN must not get interface rules")
	}
	// The fingerprint names the countries but carries none of their ranges,
	// and changes when the countries do.
	fp := Fingerprint(p)
	if strings.Contains(fp, "198.51.100.0-") || !strings.Contains(fp, "# countries: JP NZ (database 2026-11)") {
		t.Errorf("fingerprint wrong:\n%s", fp)
	}
	if strings.Contains(out, "r_r4") {
		t.Error("disabled rule rendered")
	}
	// A tunnel without kill switch must not have a refuse rule of its own.
	if strings.Contains(out, "meta mark & 0xff == 0x11 jump refuse") {
		t.Error("work VPN has no kill switch")
	}

	// One network: nothing about the others' is there.
	single := testConfig()
	single.Networks = single.Networks[:1]
	single.Policy.Rules = single.Policy.Rules[:6]
	one := Render(BuildPlan(single, Runtime{WANIfs: []string{"eth0"}}))
	for _, absent := range []string{"lan_lan", "lan_nets4", "iifname map"} {
		if strings.Contains(one, absent) {
			t.Errorf("a router with one network has %q in its ruleset", absent)
		}
	}
	if !strings.Contains(one, `set lan_ifs { type ifname; elements = { "br-lan" } }`) {
		t.Errorf("one network:\n%s", one)
	}

	// Bluetooth only: the port is closed again. Every network kept apart:
	// there is nowhere to open it from.
	if !strings.Contains(one, `iifname { "br-lan" } tcp dport 7678 accept`) {
		t.Error("one network: the app's port is not open")
	}
	single.System.BluetoothOnly = true
	if closed := Render(BuildPlan(single, Runtime{WANIfs: []string{"eth0"}})); strings.Contains(closed, "7678") {
		t.Error("set to Bluetooth only, the ruleset still mentions the app's port")
	}
	single.System.BluetoothOnly, single.Networks[0].Isolated = false, true
	if apart := Render(BuildPlan(single, Runtime{WANIfs: []string{"eth0"}})); strings.Contains(apart, "7678") {
		t.Error("the app's port is open to a network that is kept apart")
	}

	// If nft is usable (root, netfilter available), let the kernel judge.
	if os.Getuid() != 0 {
		t.Skip("not root: skipping kernel check")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	for _, ruleset := range []string{out, one} {
		cmd := exec.Command("unshare", "-n", "nft", "-f", "-")
		cmd.Stdin = strings.NewReader(ruleset)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("kernel rejected ruleset: %v\n%s\n%s", err, b, ruleset)
		}
	}
}
