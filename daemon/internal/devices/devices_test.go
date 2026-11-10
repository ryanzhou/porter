package devices

import (
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/fw"
)

func TestTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	tb := Load(path)
	var mu sync.Mutex
	var fresh []string
	tb.OnNew = func(e Entry) {
		mu.Lock()
		fresh = append(fresh, e.MAC)
		mu.Unlock()
	}
	const a, b = "02:00:00:00:00:0a", "02:00:00:00:00:0b"
	ip4 := netip.MustParseAddr("10.83.0.50")
	ip6 := netip.MustParseAddr("fd00:1::50")

	tb.Lease("add", a, ip4.String(), "laptop")
	tb.Observe(map[netip.Addr]string{ip4: a, ip6: a, netip.MustParseAddr("fe80::1"): a},
		[]Station{{MAC: b, Iface: "wlan0", Signal: -48}}, map[string]string{a: "lan", b: "guest"})
	for _, e := range tb.List() {
		if want := map[string]string{a: "lan", b: "guest"}[e.MAC]; e.Network != want {
			t.Errorf("%s is on %q, want %q", e.MAC, e.Network, want)
		}
	}

	if tb.LookupMAC(ip4) != a || tb.LookupMAC(ip6) != a {
		t.Error("address lookup failed")
	}
	list := tb.List()
	if len(list) != 2 || !list[0].Online || !list[1].Online {
		t.Fatalf("list: %+v", list)
	}
	byMAC := map[string]Entry{}
	for _, e := range list {
		byMAC[e.MAC] = e
	}
	if e := byMAC[a]; e.Hostname != "laptop" || e.Link != "wired" || len(e.IPv6) != 1 || e.OnlineSince == 0 {
		t.Errorf("wired device: %+v", e)
	}
	if e := byMAC[b]; e.Link != "wlan0" || e.Signal != -48 {
		t.Errorf("wireless device: %+v", e)
	}
	if h := tb.Hosts(); len(h["laptop"]) != 1 || h["laptop"][0] != ip4 {
		t.Errorf("hosts: %v", h)
	}

	// Counters accumulate and survive a reset of the kernel's counters.
	tb.Account(&fw.Counters{Up: map[string]uint64{a: 1000}, Down4: map[netip.Addr]uint64{ip4: 5000}, Down6: map[netip.Addr]uint64{ip6: 70}})
	tb.Account(&fw.Counters{Up: map[string]uint64{a: 1500}, Down4: map[netip.Addr]uint64{ip4: 5000}, Down6: map[netip.Addr]uint64{}})
	tb.Account(&fw.Counters{Up: map[string]uint64{a: 200}, Down4: map[netip.Addr]uint64{ip4: 100}, Down6: map[netip.Addr]uint64{}})
	for _, e := range tb.List() {
		if e.MAC == a && (e.Up != 1700 || e.Down != 5170) {
			t.Errorf("accounting: up %d down %d", e.Up, e.Down)
		}
	}

	// Gone from the network: offline, but remembered across restarts.
	tb.Observe(map[netip.Addr]string{}, nil, nil)
	for _, e := range tb.List() {
		if e.Online {
			t.Errorf("%s still online", e.MAC)
		}
	}
	if tb.LookupMAC(ip6) != "" {
		t.Error("stale IPv6 mapping kept")
	}
	tb.Save()
	again := Load(path)
	if got := again.List(); len(got) != 2 {
		t.Fatalf("reloaded: %+v", got)
	}
	for _, e := range again.List() {
		if e.MAC == a && (e.Hostname != "laptop" || e.Up != 1700) {
			t.Errorf("reloaded entry: %+v", e)
		}
	}
	again.Forget(b)
	if len(again.List()) != 1 {
		t.Error("forget failed")
	}

	// Exactly one new-device notification per device.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(fresh) != 2 {
		t.Errorf("new-device notifications: %v", fresh)
	}
}
