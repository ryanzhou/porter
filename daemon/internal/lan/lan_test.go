package lan

import (
	"fmt"
	"net/netip"
	"testing"
)

func TestPool(t *testing.T) {
	for _, c := range []struct{ prefix, start, end string }{
		{"10.83.0.1/24", "10.83.0.50", "10.83.0.254"},
		{"192.168.1.1/28", "192.168.1.4", "192.168.1.14"},
		{"10.0.0.1/16", "10.0.0.50", "10.0.255.254"},
	} {
		s, e := Pool(netip.MustParsePrefix(c.prefix))
		if s.String() != c.start || e.String() != c.end {
			t.Errorf("%s: got %s-%s, want %s-%s", c.prefix, s, e, c.start, c.end)
		}
	}
}

func TestRenumber(t *testing.T) {
	cur := netip.MustParsePrefix("10.83.0.1/24")
	if _, ok := Renumber(cur, []netip.Prefix{netip.MustParsePrefix("192.168.1.20/24")}); ok {
		t.Error("no conflict should not renumber")
	}
	// A hotel handing out 10.0.0.0/8 swallows every 10.x candidate.
	next, ok := Renumber(cur, []netip.Prefix{netip.MustParsePrefix("10.20.30.40/8")})
	if !ok || next.String() != "172.23.83.1/24" {
		t.Errorf("got %v %v", next, ok)
	}
	a, ok := Remap(netip.MustParseAddr("10.83.0.77"), cur, next)
	if !ok || a.String() != "172.23.83.77" {
		t.Errorf("remap: %v %v", a, ok)
	}
}

func TestCarve(t *testing.T) {
	nets := []string{"lan", "guest", "iot", "a1b2c3d4"}
	for _, tc := range []struct {
		prefix string
		want   int // networks that get a /64
	}{
		{"2001:db8:aa00::/40", 4},
		{"2001:db8:1::/48", 4},
		{"2001:db8:1:ab00::/56", 4},
		{"2001:db8:1:abc0::/60", 4},
		{"2001:db8:1:abcc::/62", 4},
		{"2001:db8:1:abce::/63", 2},
		{"2001:db8:1:abcd::/64", 1},
		{"2001:db8:1:abcd:8000::/65", 0},
		{"192.0.2.0/24", 0},
	} {
		pd := netip.MustParsePrefix(tc.prefix)
		got := Carve(pd, nets)
		if len(got) != tc.want {
			t.Errorf("%s: %d networks have a /64, want %d: %v", tc.prefix, len(got), tc.want, got)
		}
		// The first networks are the ones that have one, the main one
		// before any other.
		for i, id := range nets {
			if _, has := got[id]; has != (i < tc.want) {
				t.Errorf("%s: network %d (%s) has a /64: %v", tc.prefix, i, id, has)
			}
		}
		seen := map[netip.Prefix]string{}
		for id, addr := range got {
			sub := addr.Masked()
			if addr.Bits() != 64 || !pd.Contains(addr.Addr()) || sub.Addr().Next() != addr.Addr() {
				t.Errorf("%s: %s is given %s", tc.prefix, id, addr)
			}
			if other, taken := seen[sub]; taken {
				t.Errorf("%s: %s and %s both have %s", tc.prefix, id, other, sub)
			}
			seen[sub] = id
		}
		// The same again, whenever it is asked.
		if again := Carve(pd, nets); fmt.Sprint(again) != fmt.Sprint(got) {
			t.Errorf("%s: carved differently the second time: %v, then %v", tc.prefix, got, again)
		}
	}

	// A network keeps its /64 when others come and go, as long as the
	// prefix is the same: with room to spare, each has the share its id
	// leads to.
	pd := netip.MustParsePrefix("2001:db8:1::/48")
	all := Carve(pd, nets)
	for _, subset := range [][]string{{"lan"}, {"lan", "iot"}, {"lan", "a1b2c3d4", "guest"}, {"lan", "guest", "new1", "new2"}} {
		for id, addr := range Carve(pd, subset) {
			if was, known := all[id]; known && was != addr {
				t.Errorf("%s moved from %s to %s when the networks became %v", id, was, addr, subset)
			}
		}
	}
	// Another prefix, the same place in it.
	other := Carve(netip.MustParsePrefix("2001:db8:2::/48"), nets)
	for id, addr := range all {
		a, b := addr.Addr().As16(), other[id].Addr().As16()
		if a[6] != b[6] || a[7] != b[7] {
			t.Errorf("%s has a different place in another prefix of the same size: %s, %s", id, addr, other[id])
		}
	}

	// Two networks whose ids lead to the same share: the earlier one has
	// it, the later one the next, and a third whose own share that is
	// keeps it all the same.
	small := netip.MustParsePrefix("2001:db8:1:abcc::/62")
	alone := func(id string) netip.Prefix { return Carve(small, []string{id})[id] }
	place := func(p netip.Prefix) byte { a := p.Addr().As16(); return a[7] & 3 }
	var same, next string
	for i := 0; same == "" || next == ""; i++ {
		id := fmt.Sprintf("n%d", i)
		switch place(alone(id)) {
		case place(alone("lan")):
			same = id
		case (place(alone("lan")) + 1) % 4:
			next = id
		}
	}
	got := Carve(small, []string{"lan", same, next})
	if got["lan"] != alone("lan") || got[next] != alone(next) || place(got[same]) != (place(got["lan"])+2)%4 {
		t.Errorf("lan, %s (whose share is lan's) and %s (whose share is the one after): %v", same, next, got)
	}

	// The router's address in a /64 is its first.
	if a := Carve(netip.MustParsePrefix("2001:db8:1:abcd::/64"), []string{"lan"})["lan"]; a.String() != "2001:db8:1:abcd::1/64" {
		t.Errorf("the only /64 of a /64: %s", a)
	}
	if got := Carve(netip.Prefix{}, nets); len(got) != 0 {
		t.Errorf("nothing delegated, and yet: %v", got)
	}
}

func TestRenumbered(t *testing.T) {
	p := netip.MustParsePrefix
	for _, tc := range []struct {
		changed []netip.Prefix
		want    bool
	}{
		{nil, false},
		{[]netip.Prefix{p("10.83.0.1/24")}, true},
		{[]netip.Prefix{p("fd00:1:2:3::1/64")}, true},
		// A delegated prefix arriving, leaving, or being exchanged for
		// another is no reason to make every device reconnect.
		{[]netip.Prefix{p("2001:db8:1:5::1/64")}, false},
		{[]netip.Prefix{p("2001:db8:1:5::1/64"), p("2001:db8:9:5::1/64")}, false},
	} {
		if got := renumbered(tc.changed); got != tc.want {
			t.Errorf("%v: renumbered %v, want %v", tc.changed, got, tc.want)
		}
	}
}
