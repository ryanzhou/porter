package adblock

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRealLists parses downloaded block lists when PORTER_BLOCKLIST_DIR
// names a directory of them: every published format must yield a
// plausible number of entries, block a well-known ad domain, and leave
// ordinary sites alone.
func TestRealLists(t *testing.T) {
	dir := os.Getenv("PORTER_BLOCKLIST_DIR")
	if dir == "" {
		t.Skip("PORTER_BLOCKLIST_DIR not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.txt"))
	if len(files) == 0 {
		t.Fatal("no lists found")
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		s := newSet()
		start := time.Now()
		n := s.parse(f)
		f.Close()
		took := time.Since(start)
		runtime.GC()
		runtime.ReadMemStats(&after)
		t.Logf("%s: %d entries (%d exact, %d with subdomains), parsed in %v, %d MB",
			filepath.Base(path), n, len(s.exact), len(s.suffix), took.Round(time.Millisecond), (after.HeapAlloc-before.HeapAlloc)>>20)
		if n < 10000 {
			t.Errorf("%s: only %d entries; format not understood?", path, n)
		}
		if !s.Match("doubleclick.net") && !s.Match("ad.doubleclick.net") && !s.Match("googleads.g.doubleclick.net") {
			t.Errorf("%s: a well-known ad domain is not blocked", path)
		}
		for _, ok := range []string{"example.com", "wikipedia.org", "www.apple.com", "github.com", "captive.apple.com", "localhost"} {
			if s.Match(ok) {
				t.Errorf("%s: blocks %s", path, ok)
			}
		}
		runtime.KeepAlive(s)
	}
}
