package geoip

import (
	"net/netip"
	"strings"
	"testing"
)

const sample = `0.0.0.0,0.255.255.255,ZZ
1.0.0.0,1.0.0.255,AU
1.0.1.0,1.0.3.255,CN
1.0.4.0,1.0.7.255,AU
1.0.8.0,1.0.15.255,AU
198.51.100.0,198.51.100.255,JP
203.0.113.7,203.0.113.7,NZ
2001:db8::,2001:db8:ffff:ffff:ffff:ffff:ffff:ffff,JP
2a00::,2a00:ffff:ffff:ffff:ffff:ffff:ffff:ffff,DE
`

func TestGeoIP(t *testing.T) {
	db, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]string{
		"1.0.0.7": "AU", "1.0.2.1": "CN", "1.0.9.9": "AU", "198.51.100.20": "JP", "203.0.113.7": "NZ",
		"203.0.113.8": "", "0.1.2.3": "", "9.9.9.9": "", "2001:db8::1": "JP", "2a00:1::1": "DE", "2b00::1": "",
		"::ffff:1.0.0.7": "AU",
	} {
		if got := db.Lookup(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Lookup(%s) = %q, want %q", addr, got, want)
		}
	}
	v4, v6 := db.Ranges("AU")
	// The two adjacent AU ranges merge; the first is separate.
	if strings.Join(v4, " ") != "1.0.0.0-1.0.0.255 1.0.4.0-1.0.15.255" || len(v6) != 0 {
		t.Errorf("AU ranges: %v %v", v4, v6)
	}
	v4, v6 = db.Ranges("JP")
	if len(v4) != 1 || len(v6) != 1 || v6[0] != "2001:db8::-2001:db8:ffff:ffff:ffff:ffff:ffff:ffff" {
		t.Errorf("JP ranges: %v %v", v4, v6)
	}
	if v4, _ := db.Ranges("NZ"); len(v4) != 1 || v4[0] != "203.0.113.7" {
		t.Errorf("single address range: %v", v4)
	}
	if v4, v6 := db.Ranges("XX"); len(v4)+len(v6) != 0 {
		t.Error("unknown country has ranges")
	}
	var none *DB
	if none.Lookup(netip.MustParseAddr("1.0.0.1")) != "" {
		t.Error("nil database must answer nothing")
	}
	if _, err := Parse(strings.NewReader("1.0.0.9,1.0.0.1,AU\n")); err == nil {
		t.Error("inverted range accepted")
	}
}
