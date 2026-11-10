package lan

import (
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
