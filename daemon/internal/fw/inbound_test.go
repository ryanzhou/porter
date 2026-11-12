package fw

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// inboundConfig is testConfig with things set up to come in from outside:
// the WireGuard server, and ports forwarded to three devices.
func inboundConfig() *config.Config {
	c := testConfig()
	c.Inbound.Server.Enabled = true
	c.Inbound.Server.Address, c.Inbound.Server.ULAPrefix = "10.89.0.1/24", "fd00:0:0:89::/64"
	c.Devices = append(c.Devices,
		config.Device{MAC: "02:00:00:00:00:10", Name: "NAS", IP: "10.83.0.10"},
		config.Device{MAC: "02:00:00:00:00:11", Name: "Camera", IP: "10.84.0.11"},
		config.Device{MAC: "02:00:00:00:00:12", Name: "Console", IP: "10.83.0.12", Paused: true},
	)
	c.Inbound.Forwards = []config.PortForward{
		{ID: "web", Name: "Web", Enabled: true, Proto: "tcp", Ports: "8443", To: 443, Device: "02:00:00:00:00:10"},
		{ID: "game", Name: "Game", Enabled: true, Proto: "both", Ports: "27000-27015", Device: "02:00:00:00:00:10", From: []string{"203.0.113.0/24", "2001:db8:f00::/48"}},
		{ID: "cam", Name: "Camera", Enabled: true, Proto: "udp", Ports: "5000", Device: "02:00:00:00:00:11", From: []string{"198.51.100.7/32"}},
		{ID: "off", Name: "Off", Enabled: false, Proto: "tcp", Ports: "9999", Device: "02:00:00:00:00:10"},
		{ID: "paused", Name: "Paused", Enabled: true, Proto: "tcp", Ports: "7777", Device: "02:00:00:00:00:12"},
	}
	return c
}

func TestRenderInbound(t *testing.T) {
	c := inboundConfig()
	if err := c.Validate(); err != nil && !strings.Contains(err.Error(), "private key") {
		t.Fatal(err)
	}
	pfx, addr := netip.MustParsePrefix, netip.MustParseAddr
	rt := Runtime{
		WANIfs:    []string{"eth0", "ppp-eth1"},
		Delegated: map[string]netip.Prefix{"eth0": pfx("2001:db8:1000::/60")},
		Global:    map[string]netip.Prefix{"lan": pfx("2001:db8:1000:3::1/64"), "guest": pfx("2001:db8:1000:a::1/64")},
		// The NAS has been seen with two addresses of its own out of the
		// delegated prefix and one that is not; the camera with none.
		Addrs6: map[string][]netip.Addr{"02:00:00:00:00:10": {addr("2001:db8:1000:3::beef"), addr("fd00::beef"), addr("2001:db8:1000:3::aa")}},
	}
	p := BuildPlan(c, rt)
	out := Render(p)
	for _, want := range []string{
		// The server's range is one more of the router's networks.
		`set lan_ifs { type ifname; elements = { "br-guest", "br-iot", "br-lan", "wgsrv" } }`,
		`set lan_nets4 { type ipv4_addr; flags interval; elements = { 10.83.0.0/24, 10.84.0.0/24, 10.85.0.0/24, 10.89.0.0/24 } }`,
		`fd00:0:0:89::/64`,
		// Its port is open on the uplinks, and inside for a phone that
		// has come home; the app may come in through it.
		`iifname @wan_ifs udp dport 51820 accept`,
		`iifname @lan_ifs udp dport 51820 accept`,
		`iifname { "br-iot", "br-lan", "wgsrv" } tcp dport 7678 accept`,
		// SSH stays with the networks at home.
		`iifname { "br-iot", "br-lan" } tcp dport 22 accept`,
		`oifname "wgsrv" tcp flags syn / syn,rst tcp option maxseg size set rt mtu`,
		// A port sent on to another port of the device, from anywhere.
		`iifname @wan_ifs meta nfproto ipv4 fib daddr type local tcp dport 8443 dnat ip to 10.83.0.10:443`,
		// From inside as well, at any address of the router's but its
		// own in the networks.
		`iifname @lan_ifs fib daddr type local ip daddr != { 10.83.0.1, 10.84.0.1, 10.85.0.1, 10.89.0.1 } tcp dport 8443 dnat ip to 10.83.0.10:443`,
		// A range for both protocols, from one range of addresses only,
		// keeping its numbers.
		`iifname @wan_ifs fib daddr type local ip saddr { 203.0.113.0/24 } meta l4proto { tcp, udp } th dport 27000-27015 dnat ip to 10.83.0.10`,
		// To a device on a network that is kept apart.
		`iifname @wan_ifs fib daddr type local ip saddr { 198.51.100.7 } udp dport 5000 dnat ip to 10.84.0.11`,
		// Answers to what came in are not classified, and leave by an
		// uplink.
		"\t\tct direction reply return\n\t\tct mark != 0 meta mark set ct mark return",
		`iifname @lan_ifs ct direction reply oifname @wan_ifs accept`,
		`iifname @wan_ifs jump forwarded`,
		"chain forwarded {\n\t\tct status dnat accept",
		// Over IPv6 the device is addressed itself: the port it listens
		// on, at its addresses out of the delegated prefix only.
		`ip6 daddr { 2001:db8:1000:3::aa, 2001:db8:1000:3::beef } tcp dport 443 accept`,
		`ip6 daddr { 2001:db8:1000:3::aa, 2001:db8:1000:3::beef } ip6 saddr { 2001:db8:f00::/48 } meta l4proto { tcp, udp } th dport 27000-27015 accept`,
		// Sent back inside: let through whatever the networks are to
		// each other, and returned through the router.
		"ct state established,related accept\n\t\tct status dnat accept\n\t\tiifname { \"br-guest\" } jump refuse",
		`ct status dnat ip saddr @lan_nets4 ip daddr @lan_nets4 masquerade`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	for _, absent := range []string{"9999", "7777", "10.83.0.12", "fd00::beef"} {
		if strings.Contains(out, absent) {
			t.Errorf("ruleset has %q: a forward that is off, or to a paused device, or an address that is not the internet's to reach", absent)
		}
	}
	// The camera's forward is for one IPv4 address only: nothing of it is
	// open over IPv6, whatever addresses the camera has.
	rt6 := rt
	rt6.Addrs6 = map[string][]netip.Addr{"02:00:00:00:00:11": {addr("2001:db8:1000:a::cc")}}
	if with := Render(BuildPlan(c, rt6)); strings.Contains(with, "2001:db8:1000:a::cc") {
		t.Error("a forward open to IPv4 addresses only is open to everyone over IPv6")
	}
	// From an uplink the router takes its address answers, the WireGuard
	// server's port, and nothing else; the forward chain passes on from
	// there only what a forward let in.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "iifname @wan_ifs") && strings.Contains(line, "accept") &&
			line != "iifname @wan_ifs udp dport { 68, 546 } accept" && line != "iifname @wan_ifs udp dport 51820 accept" {
			t.Errorf("accepted from an uplink: %q", line)
		}
	}
	// A forward decides where connections go: taking one away ends what
	// came in by it. The addresses a device has over IPv6 do not.
	without := inboundConfig()
	without.Inbound.Forwards = without.Inbound.Forwards[1:]
	if ClassKey(BuildPlan(without, rt)) == ClassKey(p) {
		t.Error("ClassKey misses a forwarded port")
	}
	bare := rt
	bare.Addrs6 = nil
	if q := BuildPlan(c, bare); ClassKey(q) != ClassKey(p) || Fingerprint(q) == Fingerprint(p) {
		t.Error("ClassKey follows the IPv6 addresses a forwarded port is open to, or the ruleset does not")
	}
	if len(p.Forwards[0].To6) != 2 {
		t.Errorf("ClassKey changed the plan it was given: %v", p.Forwards[0].To6)
	}

	// One network, one forward, no server: what is sent back inside still
	// needs deciding, between the one network and itself.
	single := testConfig()
	single.Networks = single.Networks[:1]
	single.Policy.Rules = single.Policy.Rules[:6]
	single.Devices = append(single.Devices, config.Device{MAC: "02:00:00:00:00:10", IP: "10.83.0.10"})
	single.Inbound.Forwards = []config.PortForward{{ID: "web", Name: "Web", Enabled: true, Proto: "tcp", Ports: "80", Device: "02:00:00:00:00:10"}}
	one := Render(BuildPlan(single, Runtime{WANIfs: []string{"eth0"}}))
	for _, want := range []string{
		`iifname @lan_ifs oifname @lan_ifs jump lan_lan`,
		`iifname @lan_ifs fib daddr type local ip daddr != { 10.83.0.1 } tcp dport 80 dnat ip to 10.83.0.10`,
		`set lan_nets4 { type ipv4_addr; flags interval; elements = { 10.83.0.0/24 } }`,
	} {
		if !strings.Contains(one, want) {
			t.Errorf("one network with a forward lacks %q", want)
		}
	}
	if strings.Contains(one, "wgsrv") || strings.Contains(one, "51820") {
		t.Error("the WireGuard server is off, and the ruleset mentions it")
	}
	// With IPv6 off the server has no IPv6 range.
	off := inboundConfig()
	off.LAN.IPv6 = false
	if got := BuildPlan(off, Runtime{}); got.Server == nil || got.Server.Net6.IsValid() {
		t.Errorf("IPv6 off, and the server has %+v", got.Server)
	}

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
