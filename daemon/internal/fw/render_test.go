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
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	if strings.Contains(out, `"wg-off"`) {
		t.Error("switched-off VPN must not get interface rules")
	}
	// The fingerprint names the countries but carries none of their ranges,
	// and changes when the countries do.
	fp := Fingerprint(p)
	if strings.Contains(fp, "198.51.100.0-") || !strings.Contains(fp, "# countries: JP NZ") {
		t.Errorf("fingerprint wrong:\n%s", fp)
	}
	if strings.Contains(out, "r_r4") {
		t.Error("disabled rule rendered")
	}
	// A tunnel without kill switch must not have a refuse rule of its own.
	if strings.Contains(out, "meta mark & 0xff == 0x11 jump refuse") {
		t.Error("work VPN has no kill switch")
	}

	// If nft is usable (root, netfilter available), let the kernel judge.
	if os.Getuid() != 0 {
		t.Skip("not root: skipping kernel check")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	cmd := exec.Command("unshare", "-n", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected ruleset: %v\n%s\n%s", err, b, out)
	}
}
