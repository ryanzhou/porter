package adblock

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const mixed = `# Title: test list
! Adblock-style comment
[Adblock Plus]
0.0.0.0 ads.example.com
127.0.0.1 tracker.example.net  # trailing comment
0.0.0.0 0.0.0.0
127.0.0.1 localhost
::1 localhost
plain.example.org
||wild.example.io^
||thirdparty.example^$third-party
||path.example.com/ads/banner
@@||allowed.example.com^
*.star.example.co
192.168.1.1 router.example
not a domain line at all
BAD_chars!.example
`

func TestParseFormats(t *testing.T) {
	s := newSet()
	if n := s.parse(strings.NewReader(mixed)); n != 6 {
		t.Errorf("entries: %d, want 6 (%v %v)", n, s.exact, s.suffix)
	}
	for name, want := range map[string]bool{
		"ads.example.com":         true,
		"sub.ads.example.com":     false, // hosts entries are exact
		"tracker.example.net":     true,
		"plain.example.org":       true,
		"wild.example.io":         true,
		"a.b.wild.example.io":     true, // Adblock entries cover subdomains
		"thirdparty.example":      true,
		"x.star.example.co":       true,
		"path.example.com":        false, // rule had a path
		"allowed.example.com":     false, // exception rule
		"router.example":          false, // not a blocking address
		"localhost":               false,
		"example.com":             false,
		"notwild.example.io.evil": false,
	} {
		if got := s.Match(name); got != want {
			t.Errorf("Match(%q) = %v, want %v", name, got, want)
		}
	}
	var none *Set
	if none.Match("ads.example.com") || none.Len() != 0 {
		t.Error("nil set must match nothing")
	}
}

func TestManager(t *testing.T) {
	var hits atomic.Int32
	content := atomic.Pointer[string]{}
	first := "0.0.0.0 ads.example.com\n"
	content.Store(&first)
	modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/list":
			if ims := r.Header.Get("If-Modified-Since"); ims != "" && *content.Load() == first {
				if tm, err := http.ParseTime(ims); err == nil && !modified.After(tm) {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
			io.WriteString(w, *content.Load())
		case "/html":
			io.WriteString(w, "<html><body>Sign in to continue</body></html>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := &Manager{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Dir: dir}
	m.Configure(true, []string{srv.URL + "/list", srv.URL + "/html", srv.URL + "/missing"})
	if m.Blocked("ads.example.com") {
		t.Fatal("nothing can be blocked before the first download")
	}
	m.refreshDue(context.Background())
	if !m.Blocked("ads.example.com") || m.Blocked("example.com") {
		t.Fatal("downloaded list not in effect")
	}
	st := m.Status()
	if !st.Enabled || st.Domains != 1 || st.Blocked != 1 || len(st.Lists) != 3 {
		t.Fatalf("status: %+v", st)
	}
	if st.Lists[0].Domains != 1 || st.Lists[0].Error != "" || st.Lists[0].Updated == 0 {
		t.Errorf("good list: %+v", st.Lists[0])
	}
	// A page that is not a list, and a missing one, are reported and
	// change nothing.
	if st.Lists[1].Error == "" || st.Lists[2].Error == "" {
		t.Errorf("bad lists not reported: %+v", st.Lists[1:])
	}

	// A new manager (a reboot) blocks from the cache without the network.
	before := hits.Load()
	m2 := &Manager{Log: m.Log, Dir: dir}
	m2.Configure(true, []string{srv.URL + "/list"})
	if !m2.Blocked("ads.example.com") || hits.Load() != before {
		t.Fatal("cached list not used at start")
	}
	// The cached copy is fresh, so nothing is fetched.
	m2.refreshDue(context.Background())
	if hits.Load() != before {
		t.Error("fresh list fetched again")
	}
	// Forced refresh: unchanged upstream answers 304 and keeps the list.
	m2.RefreshNow()
	m2.refreshDue(context.Background())
	if hits.Load() != before+1 || !m2.Blocked("ads.example.com") {
		t.Error("forced refresh of an unchanged list went wrong")
	}
	// Upstream changes: the new content replaces the old.
	second := "||tracker.example.net^\n"
	content.Store(&second)
	m2.RefreshNow()
	m2.refreshDue(context.Background())
	if m2.Blocked("ads.example.com") || !m2.Blocked("x.tracker.example.net") {
		t.Error("updated list not applied")
	}
	// Switching off stops blocking at once.
	m2.Configure(false, []string{srv.URL + "/list"})
	if m2.Blocked("x.tracker.example.net") || m2.Status().Enabled {
		t.Error("still blocking after being switched off")
	}
}
