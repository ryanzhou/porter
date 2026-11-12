package fw

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// sourcesConfig routes through uplinks in each of the places a route can
// be: a rule, a device, a group, a network and the default.
func sourcesConfig() *config.Config {
	c := inboundConfig()
	c.Ethernet = append(c.Ethernet, config.EthPort{Name: "eth1", Role: config.EthWAN, Network: config.MainNetwork})
	c.Devices = append(c.Devices,
		config.Device{MAC: "02:00:00:00:00:21", Name: "Work laptop", Route: "only:phone"},
		config.Device{MAC: "02:00:00:00:00:22", Name: "Television", Route: "source:eth:eth0"},
	)
	c.Groups = []config.Group{{ID: "kids", Name: "Kids", Members: []string{"02:00:00:00:00:23"}, Route: "source:wifi"}}
	c.Networks[1].Route = "only:eth:eth1"
	c.Policy.Rules = append(c.Policy.Rules,
		config.Rule{ID: "s1", Enabled: true, Dst: config.Dst{Ports: []string{"8801"}}, Action: "source:phone"})
	c.Policy.Default = "source:eth:eth1"
	return c
}

func TestSourceMarks(t *testing.T) {
	c := sourcesConfig()
	c.Normalize()
	if err := c.Validate(); err != nil && !strings.Contains(err.Error(), "private key") {
		t.Fatal(err)
	}
	for action, want := range map[string]uint32{
		"source:wifi": 0x40, "only:wifi": 0x80,
		"source:phone": 0x41, "only:phone": 0x81,
		"source:eth:eth0": 0x42, "only:eth:eth0": 0x82,
		"source:eth:eth1": 0x43, "only:eth:eth1": 0x83,
		// A port there is none of: never a way out.
		"source:eth:eth9": MarkBlock, "only:": MarkBlock, "source:": MarkBlock,
	} {
		if got := MarkFor(c, action); got != want {
			t.Errorf("mark of %q: got %#x, want %#x", action, got, want)
		}
	}
	// No mark of an uplink is one of a tunnel's, and each has its key back.
	for i := 0; i < MaxSources; i++ {
		if MarkSource(i) < MarkVPN(8) || MarkSource(i) >= MarkOnly(0) || MarkOnly(i) > ClassMask {
			t.Errorf("uplink %d: marks %#x and %#x leave their ranges", i, MarkSource(i), MarkOnly(i))
		}
	}
	for _, key := range []string{"wifi", "phone", "eth:eth0", "eth:eth1"} {
		i, ok := SourceIndex(c, key)
		back, ok2 := SourceKey(c, i)
		if !ok || !ok2 || back != key {
			t.Errorf("%s: number %d, which is %q", key, i, back)
		}
	}
	if _, ok := SourceKey(c, 9); ok {
		t.Error("a number no uplink has was given a key")
	}
}

func TestRenderSources(t *testing.T) {
	c := sourcesConfig()
	c.Normalize()
	rt := Runtime{
		WANIfs: []string{"eth0", "eth1", "usb0"},
		// The wired ports are there, the second out of data; so is a
		// phone. Wi-Fi, which a group's route names, is not.
		Sources: []SourceState{
			{Key: "eth:eth0", If: "eth0", Usable: true},
			{Key: "eth:eth1", If: "eth1", Spent: true},
			{Key: "phone", If: "usb0", Usable: true},
		},
		Delegated: map[string]netip.Prefix{"eth0": netip.MustParsePrefix("2001:db8:1000::/60")},
		Global:    map[string]netip.Prefix{"lan": netip.MustParsePrefix("2001:db8:1000:3::1/64")},
	}
	p := BuildPlan(c, rt)
	out := Render(p)
	for _, want := range []string{
		// Each place a route can be gives the uplink's mark.
		`02:00:00:00:00:21 : 0x81`, `02:00:00:00:00:22 : 0x42`, `02:00:00:00:00:23 : 0x40`,
		`"br-guest" : 0x83`,
		"chain r_s1 {\n\t\tmeta l4proto { tcp, udp } th dport { 8801 } meta mark set 0x41 ct mark set 0x41 accept",
		"\t\tmeta mark set 0x43 ct mark set 0x43 accept\n\t}",
		// What is for one uplink alone leaves by it, and by nothing else:
		// not while the uplink is out of data, and not while it is away.
		"\t\tmeta mark & 0xff == 0x80 jump refuse\n",
		"\t\tmeta mark & 0xff == 0x81 oifname \"usb0\" accept\n\t\tmeta mark & 0xff == 0x81 jump refuse\n",
		"\t\tmeta mark & 0xff == 0x82 oifname \"eth0\" accept\n\t\tmeta mark & 0xff == 0x82 jump refuse\n",
		"oifname \"eth0\" accept\n\t\tmeta mark & 0xff == 0x82 jump refuse\n\t\tmeta mark & 0xff == 0x83 jump refuse\n",
		// Nothing is forwarded out by an uplink that is out of data.
		"\t\toifname { \"eth1\" } jump refuse\n",
		// Nor does the router send its own traffic for such a device
		// another way.
		"\t\tmeta mark & 0xff == 0x80 jump refuse\n\t\tmeta mark & 0xff == 0x81 oifname != \"usb0\" jump refuse\n\t\tmeta mark & 0xff == 0x82 oifname != \"eth0\" jump refuse\n\t\tmeta mark & 0xff == 0x83 jump refuse\n\t}",
		// A connection that came in by an uplink is noted as that
		// uplink's, and the answers to it are sent by its mark.
		`iifname @wan_ifs ct state new ct mark set iifname map { "eth0" : 0x282, "eth1" : 0x283, "usb0" : 0x281 }`,
		"\t\tct direction reply meta mark set ct mark return\n\t\tct mark != 0 meta mark set ct mark return",
		// An address out of the prefix one uplink delegated keeps itself
		// by that uplink only; by another it is masqueraded like the rest.
		"\t\toifname \"eth0\" ip6 saddr 2001:db8:1000::/60 accept\n",
		"\t\toifname @wan_ifs masquerade\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	if strings.Contains(out, `oifname "eth1" accept`) || strings.Contains(out, `oifname "usb0" ip6 saddr`) {
		t.Errorf("an uplink out of data is let out by, or an uplink that delegated nothing keeps addresses:\n%s", out)
	}
	if t.Failed() {
		t.Logf("ruleset:\n%s", out)
	}

	// An uplink coming, going or running out of data does not make every
	// connection start over; a route that names another uplink does.
	away := rt
	away.Sources = nil
	if ClassKey(BuildPlan(c, away)) != ClassKey(p) {
		t.Error("the uplinks that are there are part of what makes connections start over")
	}
	if Fingerprint(BuildPlan(c, away)) == Fingerprint(p) {
		t.Error("the ruleset does not follow the uplinks that are there")
	}
	moved := sourcesConfig()
	moved.Normalize()
	moved.Devices[len(moved.Devices)-1].Route = "only:eth:eth0"
	if ClassKey(BuildPlan(moved, rt)) == ClassKey(p) {
		t.Error("a device given another route does not make its connections start over")
	}

	// Without a forwarded port nothing that comes in is forwarded, and
	// nothing needs noting.
	plain := sourcesConfig()
	plain.Normalize()
	plain.Inbound.Forwards = nil
	if strings.Contains(Render(BuildPlan(plain, rt)), "ct state new ct mark set") {
		t.Error("connections from outside are marked although no port is forwarded")
	}

	if os.Getuid() != 0 {
		t.Skip("not root: skipping kernel validation")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	cmd := exec.Command("unshare", "-n", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the kernel refuses the ruleset: %v\n%s\n%s", err, b, out)
	}
}

// TestSyncSources makes the routing tables of two uplinks in a namespace
// and asks the kernel which way packets go.
func TestSyncSources(t *testing.T) {
	inNetns(t)
	// Two uplinks, and one of the router's networks for a device's
	// packets to arrive from.
	for _, pair := range [][2]string{{"eth0", "isp0"}, {"usb0", "isp1"}, {"br-lan", "dev0"}} {
		if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: pair[0]}, PeerName: pair[1]}); err != nil {
			t.Skip("cannot create test link:", err)
		}
		for _, n := range pair {
			l, _ := netlink.LinkByName(n)
			if err := netlink.LinkSetUp(l); err != nil {
				t.Fatal(err)
			}
		}
	}
	lo, _ := netlink.LinkByName("lo")
	netlink.LinkSetUp(lo)
	ipOut(t, "addr", "add", "10.83.0.1/24", "dev", "br-lan")
	if err := EnableForwarding(); err != nil {
		t.Fatal(err)
	}
	// Both uplinks are on networks numbered the same, as the cable and
	// the Wi-Fi of one hotel are.
	ipOut(t, "addr", "add", "192.0.2.2/24", "dev", "eth0", "metric", "100")
	ipOut(t, "addr", "add", "192.0.2.3/24", "dev", "usb0", "metric", "300")
	ipOut(t, "route", "add", "default", "via", "192.0.2.1", "dev", "eth0", "metric", "100")
	ipOut(t, "route", "add", "default", "via", "192.0.2.1", "dev", "usb0", "metric", "300")
	addr := netip.MustParseAddr
	wired := &UplinkOverride{If: "eth0", V4: true, Gw4: addr("192.0.2.1")}
	phone := &UplinkOverride{If: "usb0", V4: true, Gw4: addr("192.0.2.1")}
	// Numbers as SourceIndex gives them: the phone is 1, the port 2.
	both := func(phoneOut *UplinkOverride, prefer bool) []SourceRoutes {
		return []SourceRoutes{
			{Index: 1, Out: phoneOut, Prefer: prefer, If: "usb0", Addrs: []netip.Addr{addr("192.0.2.3")}},
			{Index: 2, Out: wired, Prefer: true, If: "eth0", Addrs: []netip.Addr{addr("192.0.2.2")}},
		}
	}
	sync := func(sources []SourceRoutes) {
		t.Helper()
		for i := 0; i < 2; i++ { // twice: the second time changes nothing
			if err := SyncRouting(nil, wired, sources); err != nil {
				t.Fatal(err)
			}
		}
	}
	// way asks the kernel how a packet would leave: "eth0", "usb0", or
	// what it says when it would not.
	way := func(args ...string) string {
		out, err := exec.Command("ip", append([]string{"route", "get", "198.51.100.1"}, args...)...).CombinedOutput()
		if err != nil {
			return strings.TrimSpace(string(out))
		}
		for _, dev := range []string{"eth0", "usb0"} {
			if strings.Contains(string(out), "dev "+dev) {
				return dev
			}
		}
		return strings.TrimSpace(string(out))
	}
	forwarded := func(mark string) string { return way("from", "10.83.0.50", "iif", "br-lan", "mark", mark) }
	// refused is what the kernel says of a route that is "unreachable".
	const refused = "No route to host"

	sync(both(phone, true))
	rules := ipOut(t, "rule", "show")
	for _, want := range []string{
		"130:\tfrom all fwmark 0x41/0xff lookup 151", "130:\tfrom all fwmark 0x81/0xff lookup 151",
		"130:\tfrom all fwmark 0x42/0xff lookup 152", "130:\tfrom all fwmark 0x82/0xff lookup 152",
		"120:\tfrom 192.0.2.3 lookup 151", "120:\tfrom all oif usb0 lookup 151",
		"120:\tfrom 192.0.2.2 lookup 152", "120:\tfrom all oif eth0 lookup 152",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("no rule %q in:\n%s", want, rules)
		}
	}
	if got := ipOut(t, "route", "show", "table", "151"); !strings.Contains(got, "default via 192.0.2.1 dev usb0") || !strings.Contains(got, "unreachable default") {
		t.Errorf("the phone's table: %q", got)
	}
	for mark, want := range map[string]string{"0x1": "eth0", "0x41": "usb0", "0x81": "usb0", "0x42": "eth0", "0x281": "usb0"} {
		if got := forwarded(mark); got != want {
			t.Errorf("a device's packet marked %s leaves by %q, want %s", mark, got, want)
		}
	}
	// What the router sends from the phone's address, or from a socket
	// bound to the phone, leaves by the phone although the wired uplink
	// is the one in use; unbound it goes by the one in use.
	if got := way("from", "192.0.2.3"); got != "usb0" {
		t.Errorf("an answer from the phone's own address leaves by %q", got)
	}
	if got := way("oif", "usb0"); got != "usb0" {
		t.Errorf("a socket bound to the phone sends by %q", got)
	}
	if got := way(); got != "eth0" {
		t.Errorf("the router's own traffic leaves by %q, want the uplink in use", got)
	}

	// The phone stops reaching the internet: what prefers it goes by the
	// uplink in use, what is for it alone still goes to it.
	sync(both(phone, false))
	if got := forwarded("0x41"); got != "eth0" {
		t.Errorf("traffic that prefers a phone without internet leaves by %q, want eth0", got)
	}
	if got := forwarded("0x81"); got != "usb0" {
		t.Errorf("traffic for the phone alone leaves by %q while the phone is connected", got)
	}

	// The phone is not to be used (unplugged, or out of data): what is
	// for it alone is refused, and so is what the router would send from
	// its address; what prefers it falls back.
	sync(both(nil, false))
	if got := forwarded("0x81"); !strings.Contains(got, refused) {
		t.Errorf("traffic for a phone that is away: %q, want it refused", got)
	}
	if got := forwarded("0x281"); !strings.Contains(got, refused) {
		t.Errorf("an answer to what came in by a phone that is away: %q, want it refused", got)
	}
	if got := forwarded("0x41"); got != "eth0" {
		t.Errorf("traffic that prefers a phone that is away leaves by %q, want eth0", got)
	}
	if got := ipOut(t, "route", "show", "table", "151"); strings.Contains(got, "usb0") || !strings.Contains(got, "unreachable default") {
		t.Errorf("the table of a phone that is away: %q", got)
	}

	// An uplink a route names and that is not there at all: a table that
	// refuses, and no rules for an interface or addresses.
	sync([]SourceRoutes{{Index: 0}, {Index: 2, Out: wired, Prefer: true, If: "eth0"}})
	rules = ipOut(t, "rule", "show")
	if !strings.Contains(rules, "fwmark 0x80/0xff lookup 150") || strings.Contains(rules, "fwmark 0x40/0xff") || strings.Contains(rules, "lookup 151") {
		t.Errorf("rules with Wi-Fi named and away, and the phone gone:\n%s", rules)
	}
	if got := forwarded("0x80"); !strings.Contains(got, refused) {
		t.Errorf("traffic for Wi-Fi alone, with no Wi-Fi: %q", got)
	}
	if got, _ := exec.Command("ip", "route", "show", "table", "151").CombinedOutput(); strings.TrimSpace(string(got)) != "" && !strings.Contains(string(got), "does not exist") {
		t.Errorf("the table of an uplink that is no longer listed was left: %q", got)
	}

	// Every uplink that could be used is out of data: nothing leaves by
	// the main table's default route.
	if err := SyncRouting(nil, &UplinkOverride{}, []SourceRoutes{{Index: 2, If: "eth0", Addrs: []netip.Addr{addr("192.0.2.2")}}}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"from", "192.0.2.2"}} {
		if got := way(args...); !strings.Contains(got, refused) {
			t.Errorf("with every uplink out of data, the router's traffic (%v): %q", args, got)
		}
	}
	if got := forwarded("0x1"); !strings.Contains(got, refused) {
		t.Errorf("with every uplink out of data, a device's traffic: %q", got)
	}

	if HaveIPv6() {
		// The phone has IPv6 and the wired uplink has none: what is
		// marked for the phone goes there, what is marked for the wired
		// uplink is refused and not left to the phone.
		ipOut(t, "-6", "addr", "add", "2001:db8:2::2/64", "dev", "usb0", "nodad")
		ipOut(t, "-6", "addr", "add", "fd00::1/64", "dev", "br-lan", "nodad")
		phone6 := &UplinkOverride{If: "usb0", V4: true, Gw4: addr("192.0.2.1"), V6: true, Gw6: addr("2001:db8:2::1")}
		src := both(phone6, true)
		src[0].Addrs = append(src[0].Addrs, addr("2001:db8:2::2"))
		sync(src)
		get6 := func(args ...string) string {
			out, _ := exec.Command("ip", append([]string{"-6", "route", "get", "2001:db8:f::1"}, args...)...).CombinedOutput()
			return strings.TrimSpace(string(out))
		}
		if got := get6("from", "fd00::50", "iif", "br-lan", "mark", "0x81"); !strings.Contains(got, "dev usb0") {
			t.Errorf("IPv6 for the phone alone: %q", got)
		}
		// Asked about an unreachable route, the kernel either shows it
		// or, depending on its version, answers with its error.
		if got := get6("from", "fd00::50", "iif", "br-lan", "mark", "0x82"); !strings.Contains(got, "unreachable") && !strings.Contains(got, refused) {
			t.Errorf("IPv6 for a wired uplink that has none: %q", got)
		}
		if got := ipOut(t, "-6", "rule", "show"); !strings.Contains(got, "120:\tfrom 2001:db8:2::2 lookup 151") {
			t.Errorf("no rule for the phone's own IPv6 address:\n%s", got)
		}
	}

	if err := SyncRouting(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if rules := ipOut(t, "rule", "show"); strings.Contains(rules, "fwmark") || strings.Contains(rules, "lookup 15") {
		t.Errorf("rules left behind:\n%s", rules)
	}
}
