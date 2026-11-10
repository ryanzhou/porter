package geoip

import (
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestRealDatabase loads a full DB-IP file when PORTER_GEOIP_FILE points
// at one, to check parsing, size and speed against real data.
func TestRealDatabase(t *testing.T) {
	path := os.Getenv("PORTER_GEOIP_FILE")
	if path == "" {
		t.Skip("PORTER_GEOIP_FILE not set")
	}
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	db, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded := time.Since(start)
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("%d IPv4 + %d IPv6 ranges, loaded in %v, %d MB retained", len(db.v4), len(db.v6), loaded.Round(time.Millisecond), (after.HeapAlloc-before.HeapAlloc)>>20)
	// Unicast addresses with an unambiguous home. (Anycast services are
	// attributed to a single country and are not a fair test.)
	for addr, want := range map[string]string{"8.8.8.8": "US", "133.11.0.1": "JP", "10.0.0.1": ""} {
		if got := db.Lookup(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Lookup(%s) = %q, want %q", addr, got, want)
		}
	}
	start = time.Now()
	v4, v6 := db.Ranges("US")
	t.Logf("US: %d IPv4 and %d IPv6 ranges in %v", len(v4), len(v6), time.Since(start).Round(time.Millisecond))
	if len(v4) < 1000 || len(v6) < 1000 {
		t.Error("implausibly few ranges for US")
	}
}
