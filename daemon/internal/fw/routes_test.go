package fw

import (
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// inNetns runs the test body inside a fresh network namespace.
func inNetns(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Skip("no netns support:", err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Skip("cannot create netns:", err)
	}
	t.Cleanup(func() {
		netns.Set(orig)
		ns.Close()
		orig.Close()
		runtime.UnlockOSThread()
	})
}

func ipOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ip %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestSyncRouting(t *testing.T) {
	inNetns(t)
	// A veth end stands in for the WireGuard interface.
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "wg-home"}, PeerName: "peer0"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Skip("cannot create test link:", err)
	}
	for _, n := range []string{"wg-home", "peer0"} {
		l, _ := netlink.LinkByName(n)
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
	}
	v4only := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}

	// Tunnel up, IPv4 only, no kill switch: IPv6 must still be backstopped.
	if err := SyncRouting([]VPNRoutes{{Index: 0, If: "wg-home", Up: true, Allowed: v4only}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := ipOut(t, "route", "show", "table", "110"); !strings.Contains(got, "default dev wg-home") || strings.Contains(got, "unreachable") {
		t.Errorf("v4 table: %q", got)
	}
	v6 := HaveIPv6()
	ip6 := func() string {
		if !v6 {
			return ""
		}
		return ipOut(t, "-6", "route", "show", "table", "110")
	}
	if got := ip6(); v6 && !strings.Contains(got, "unreachable default") {
		t.Errorf("v6 table lacks backstop: %q", got)
	}
	rules := ipOut(t, "rule", "show")
	if !strings.Contains(rules, "100:\tfrom all lookup main suppress_prefixlength 0") || !strings.Contains(rules, "110:\tfrom all fwmark 0x10/0xff lookup 110") {
		t.Errorf("rules: %q", rules)
	}
	// The router's own traffic for the tunnel goes by its table first.
	if !strings.Contains(rules, "60:\tfrom all fwmark 0x10/0xff iif lo lookup 110") {
		t.Errorf("no rule for the router's own traffic: %q", rules)
	}

	// Tunnel down, no kill switch: table empty so traffic falls back.
	if err := SyncRouting([]VPNRoutes{{Index: 0, If: "wg-home", Up: false, Allowed: v4only}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := ipOut(t, "route", "show", "table", "110") + ip6(); strings.TrimSpace(got) != "" {
		t.Errorf("tables should be empty: %q", got)
	}

	// Kill switch: backstop present up or down, in both families.
	for _, up := range []bool{false, true} {
		if err := SyncRouting([]VPNRoutes{{Index: 0, If: "wg-home", Up: up, KillSwitch: true, Allowed: v4only}}, nil, nil); err != nil {
			t.Fatal(err)
		}
		v4 := ipOut(t, "route", "show", "table", "110")
		if !strings.Contains(v4, "unreachable default") || (v6 && !strings.Contains(ip6(), "unreachable default")) {
			t.Errorf("up=%v: missing backstop: %q %q", up, v4, ip6())
		}
		if up != strings.Contains(v4, "default dev wg-home") {
			t.Errorf("up=%v: tunnel route presence wrong: %q", up, v4)
		}
	}

	// Removing the profile removes its rule; idempotent on repeat.
	for i := 0; i < 2; i++ {
		if err := SyncRouting(nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if rules := ipOut(t, "rule", "show"); strings.Contains(rules, "fwmark") {
		t.Errorf("stale rule: %q", rules)
	}

	// The uplink override: a table of its own, consulted after the
	// tunnels' and before the main table's default route.
	ipOut(t, "addr", "add", "192.0.2.2/24", "dev", "peer0")
	over := &UplinkOverride{If: "peer0", V4: true, Gw4: netip.MustParseAddr("192.0.2.1")}
	for i := 0; i < 2; i++ {
		if err := SyncRouting(nil, over, nil); err != nil {
			t.Fatal(err)
		}
	}
	if rules := ipOut(t, "rule", "show"); !strings.Contains(rules, "140:\tfrom all lookup 105") {
		t.Errorf("no rule for the override table: %q", rules)
	}
	if v4 := ipOut(t, "route", "show", "table", "105"); !strings.Contains(v4, "default via 192.0.2.1 dev peer0") {
		t.Errorf("override route: %q", v4)
	}
	if HaveIPv6() {
		// That uplink has no IPv6: IPv6 is refused rather than left to
		// the uplink being stepped past.
		if v6 := ipOut(t, "-6", "route", "show", "table", "105"); !strings.Contains(v6, "unreachable default") {
			t.Errorf("IPv6 with a v4-only override: %q", v6)
		}
	}
	// Another gateway replaces the first; no override empties the table.
	over.Gw4 = netip.MustParseAddr("192.0.2.9")
	if err := SyncRouting(nil, over, nil); err != nil {
		t.Fatal(err)
	}
	if v4 := ipOut(t, "route", "show", "table", "105"); !strings.Contains(v4, "via 192.0.2.9") || strings.Contains(v4, "192.0.2.1 ") {
		t.Errorf("override route after change: %q", v4)
	}
	// An uplink with an address but no gateway known is left to the main
	// table; one with no IPv4 at all has IPv4 refused.
	over.Gw4 = netip.Addr{}
	if err := SyncRouting(nil, over, nil); err != nil {
		t.Fatal(err)
	}
	if v4, _ := exec.Command("ip", "route", "show", "table", "105").CombinedOutput(); strings.Contains(string(v4), "default") {
		t.Errorf("override without a gateway: %q", v4)
	}
	// A link with only one other end (a PPPoE session) has no gateway,
	// and is named all the same: the main table would otherwise prefer
	// another uplink's route by its metric.
	over.Direct4 = true
	for i := 0; i < 2; i++ {
		if err := SyncRouting(nil, over, nil); err != nil {
			t.Fatal(err)
		}
	}
	if v4 := ipOut(t, "route", "show", "table", "105"); !strings.Contains(v4, "default dev peer0") || strings.Contains(v4, "via") {
		t.Errorf("override straight down the link: %q", v4)
	}
	// And is replaced by a gateway once there is one.
	over.Gw4 = netip.MustParseAddr("192.0.2.1")
	if err := SyncRouting(nil, over, nil); err != nil {
		t.Fatal(err)
	}
	if v4 := ipOut(t, "route", "show", "table", "105"); !strings.Contains(v4, "default via 192.0.2.1 dev peer0") || strings.Count(v4, "default") != 1 {
		t.Errorf("override after the link got a gateway: %q", v4)
	}
	over.Gw4, over.Direct4 = netip.Addr{}, false
	over.V4 = false
	if err := SyncRouting(nil, over, nil); err != nil {
		t.Fatal(err)
	}
	if v4 := ipOut(t, "route", "show", "table", "105"); !strings.Contains(v4, "unreachable default") {
		t.Errorf("override for an uplink without IPv4: %q", v4)
	}
	if err := SyncRouting(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("ip", "route", "show", "table", "105").CombinedOutput()
	if strings.Contains(string(out), "default") {
		t.Errorf("override left behind: %q", out)
	}
}
