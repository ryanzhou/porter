package wan

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// A probe that finds the internet reachable brings back the time the
// site gave, which is one of the ways the router learns it; an answer
// from anything else (a sign-in page) is not asked the time.
func TestProbeDate(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("binding a socket to an interface needs root")
	}
	said := time.Date(2031, 3, 4, 21, 0, 7, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", said.Format(http.TimeFormat))
		if r.URL.Path == "/ok" {
			fmt.Fprint(w, "<HTML><BODY>Success</BODY></HTML>")
			return
		}
		http.Redirect(w, r, "http://portal.example/login", http.StatusFound)
	}))
	defer srv.Close()
	res := Probe("lo", config.Probe{URL: srv.URL + "/ok", Expect: "Success"}, nil)
	if res.State != StOnline || !res.Date.Equal(said) {
		t.Errorf("reachable: %+v, want online with the date %s", res, said)
	}
	res = Probe("lo", config.Probe{URL: srv.URL + "/elsewhere", Expect: "Success"}, nil)
	if res.State != StCaptive || !res.Date.IsZero() {
		t.Errorf("a sign-in page: %+v, want captive and no date", res)
	}
	// A site that gives no time, or one that cannot be read.
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "yesterday, around noon")
		fmt.Fprint(w, "Success")
	}))
	defer quiet.Close()
	if res := Probe("lo", config.Probe{URL: quiet.URL, Expect: "Success"}, nil); res.State != StOnline || !res.Date.IsZero() {
		t.Errorf("a date that is none: %+v", res)
	}
}
