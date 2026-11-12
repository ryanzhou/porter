package ddns

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

var (
	ip4 = netip.MustParseAddr("203.0.113.7")
	ip6 = netip.MustParseAddr("2001:db8::7")
)

// provider is a fake update server that notes what it was asked.
type provider struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *provider {
	p := &provider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.reqs, p.body = append(p.reqs, r), append(p.body, string(b))
		p.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *provider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *provider) host() string { return strings.TrimPrefix(p.URL, "http://") }

func TestDyndns2(t *testing.T) {
	answer := "good 203.0.113.7"
	status := http.StatusOK
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintln(w, answer)
	})
	ep := Endpoints{Dyndns2Scheme: "http"}
	cfg := config.DDNS{Enabled: true, Provider: config.DDNSDyndns2, Hostname: "home.example.com", Server: srv.host(), Username: "user", Password: "pass word"}
	if err := Update(context.Background(), srv.Client(), ep, cfg, ip4, netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	r := srv.reqs[0]
	user, pass, _ := r.BasicAuth()
	if r.Method != "GET" || r.URL.Path != "/nic/update" || r.URL.Query().Get("hostname") != "home.example.com" || r.URL.Query().Get("myip") != "203.0.113.7" ||
		user != "user" || pass != "pass word" || r.UserAgent() == "" || strings.HasPrefix(r.UserAgent(), "Go-http-client") {
		t.Errorf("request: %s %s, user %q, password %q, agent %q", r.Method, r.URL, user, pass, r.UserAgent())
	}
	// Both families go in the one parameter.
	if err := Update(context.Background(), srv.Client(), ep, cfg, ip4, ip6); err != nil || srv.reqs[1].URL.Query().Get("myip") != "203.0.113.7,2001:db8::7" {
		t.Errorf("both families: %v %s", err, srv.reqs[1].URL)
	}
	for word, want := range map[string]struct {
		permanent bool
		says      string
	}{
		"nochg 203.0.113.7": {false, ""},
		"badauth":           {true, "user name or the password"},
		"nohost":            {true, "does not know the name home.example.com"},
		"notfqdn":           {true, "complete name"},
		"abuse":             {true, "blocked"},
		"!donator":          {true, "your account"},
		"dnserr":            {false, "problem of its own"},
		"911":               {false, "problem of its own"},
		"<html>hello":       {false, "does not understand"},
		"":                  {false, "does not understand"},
	} {
		answer = word
		err := Update(context.Background(), srv.Client(), ep, cfg, ip4, netip.Addr{})
		if want.says == "" {
			if err != nil {
				t.Errorf("%q: %v", word, err)
			}
			continue
		}
		e, ok := err.(*Error)
		if !ok || e.Permanent != want.permanent || !strings.Contains(e.Msg, want.says) {
			t.Errorf("%q: %#v, want permanent=%v saying %q", word, err, want.permanent, want.says)
		}
	}
	// A server that says it with the status alone.
	answer, status = "", http.StatusUnauthorized
	if e, ok := Update(context.Background(), srv.Client(), ep, cfg, ip4, netip.Addr{}).(*Error); !ok || !e.Permanent || !strings.Contains(e.Msg, "password") {
		t.Errorf("401: %#v", e)
	}
	// Nobody there: said without the address that was asked, which has
	// the name in it and, for other providers, a secret.
	srv.Close()
	err := Update(context.Background(), http.DefaultClient, ep, cfg, ip4, netip.Addr{})
	if e, ok := err.(*Error); !ok || e.Permanent || !strings.Contains(e.Msg, "could not be reached") || strings.Contains(e.Msg, "nic/update") || strings.Contains(e.Msg, "home.example.com") {
		t.Errorf("unreachable: %#v", err)
	}
}

func TestDuckDNS(t *testing.T) {
	answer := "OK"
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, answer) })
	ep := Endpoints{DuckDNS: srv.URL}
	cfg := config.DDNS{Enabled: true, Provider: config.DDNSDuckDNS, Hostname: "example.duckdns.org", Password: "tok-123"}
	if err := Update(context.Background(), srv.Client(), ep, cfg, ip4, ip6); err != nil {
		t.Fatal(err)
	}
	q := srv.reqs[0].URL.Query()
	if srv.reqs[0].URL.Path != "/update" || q.Get("domains") != "example" || q.Get("token") != "tok-123" || q.Get("ip") != "203.0.113.7" || q.Get("ipv6") != "2001:db8::7" {
		t.Errorf("request: %s", srv.reqs[0].URL)
	}
	// An address the router does not have is not sent empty, which
	// DuckDNS would take as "use the one this request comes from".
	if err := Update(context.Background(), srv.Client(), ep, cfg, netip.Addr{}, ip6); err != nil || srv.reqs[1].URL.Query().Has("ip") {
		t.Errorf("IPv6 only: %v %s", err, srv.reqs[1].URL)
	}
	answer = "KO"
	err := Update(context.Background(), srv.Client(), ep, cfg, ip4, netip.Addr{})
	if e, ok := err.(*Error); !ok || !e.Permanent || !strings.Contains(e.Msg, "token") || strings.Contains(e.Msg, "tok-123") {
		t.Errorf("KO: %#v", err)
	}
	answer = "busy"
	if e, ok := Update(context.Background(), srv.Client(), ep, cfg, ip4, netip.Addr{}).(*Error); !ok || e.Permanent {
		t.Errorf("neither OK nor KO: %#v", e)
	}
}

func TestCloudflare(t *testing.T) {
	// A small Cloudflare: one zone, example.com, and the records in it.
	records := map[string]map[string]string{} // type -> {id, content}
	denied := false
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		ok := func(result any) {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
		}
		if denied || r.Header.Get("Authorization") != "Bearer cf-token" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`)
			return
		}
		switch {
		case r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				ok([]map[string]string{{"id": "zone1"}})
			} else {
				ok([]any{})
			}
		case r.URL.Path == "/zones/zone1/dns_records" && r.Method == "GET":
			if rec, there := records[r.URL.Query().Get("type")]; there && r.URL.Query().Get("name") == "home.lab.example.com" {
				ok([]map[string]string{rec})
			} else {
				ok([]any{})
			}
		case r.URL.Path == "/zones/zone1/dns_records" && r.Method == "POST":
			ok(nil)
		case strings.HasPrefix(r.URL.Path, "/zones/zone1/dns_records/") && r.Method == "PATCH":
			ok(nil)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":7003,"message":"No route for that URI"}]}`)
		}
	})
	ep := Endpoints{Cloudflare: srv.URL}
	cfg := config.DDNS{Enabled: true, Provider: config.DDNSCloudflare, Hostname: "home.lab.example.com", Password: "cf-token"}
	shape := func(from int) []string {
		var out []string
		for i, r := range srv.reqs[from:] {
			out = append(out, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+srv.body[from+i]))
		}
		return out
	}
	// No record yet: the zone is looked for from the name upwards, and
	// both records are made, not behind Cloudflare's proxy.
	if err := Update(context.Background(), srv.Client(), ep, cfg, ip4, ip6); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /zones?name=home.lab.example.com",
		"GET /zones?name=lab.example.com",
		"GET /zones?name=example.com",
		"GET /zones/zone1/dns_records?type=A&name=home.lab.example.com",
		`POST /zones/zone1/dns_records {"content":"203.0.113.7","name":"home.lab.example.com","proxied":false,"ttl":120,"type":"A"}`,
		"GET /zones/zone1/dns_records?type=AAAA&name=home.lab.example.com",
		`POST /zones/zone1/dns_records {"content":"2001:db8::7","name":"home.lab.example.com","proxied":false,"ttl":120,"type":"AAAA"}`,
	}
	if got := shape(0); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("creating the records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A record that says something else is changed, and only its
	// content; one that is right already is left alone.
	records["A"] = map[string]string{"id": "rec4", "content": "198.51.100.1"}
	records["AAAA"] = map[string]string{"id": "rec6", "content": "2001:db8::7"}
	n := srv.count()
	if err := Update(context.Background(), srv.Client(), ep, cfg, ip4, ip6); err != nil {
		t.Fatal(err)
	}
	got := shape(n + 3)
	want = []string{
		"GET /zones/zone1/dns_records?type=A&name=home.lab.example.com",
		`PATCH /zones/zone1/dns_records/rec4 {"content":"203.0.113.7"}`,
		"GET /zones/zone1/dns_records?type=AAAA&name=home.lab.example.com",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("changing a record:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Without an IPv6 address the AAAA record is not touched.
	n = srv.count()
	if err := Update(context.Background(), srv.Client(), ep, cfg, ip4, netip.Addr{}); err != nil || strings.Contains(strings.Join(shape(n), " "), "AAAA") {
		t.Errorf("IPv4 only: %v %v", err, shape(n))
	}
	// A name in no zone of the account, and a token that is refused.
	other := cfg
	other.Hostname = "home.example.org"
	if e, ok := Update(context.Background(), srv.Client(), ep, other, ip4, netip.Addr{}).(*Error); !ok || !e.Permanent || !strings.Contains(e.Msg, "no domain that home.example.org belongs to") {
		t.Errorf("unknown zone: %#v", e)
	}
	denied = true
	if e, ok := Update(context.Background(), srv.Client(), ep, cfg, ip4, netip.Addr{}).(*Error); !ok || !e.Permanent || !strings.Contains(e.Msg, "API token") || strings.Contains(e.Msg, "cf-token") {
		t.Errorf("refused token: %#v", e)
	}
}

func TestCustomURL(t *testing.T) {
	status := http.StatusOK
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
	cfg := config.DDNS{Enabled: true, Provider: config.DDNSCustom, Hostname: "home.example.com", URL: srv.URL + "/update?key=s3cret&a={ip}&aaaa={ip6}"}
	if err := Update(context.Background(), srv.Client(), Endpoints{}, cfg, ip4, ip6); err != nil {
		t.Fatal(err)
	}
	if got := srv.reqs[0].URL.RequestURI(); got != "/update?key=s3cret&a=203.0.113.7&aaaa=2001:db8::7" {
		t.Errorf("request: %s", got)
	}
	// An address the router does not have leaves its place empty.
	if err := Update(context.Background(), srv.Client(), Endpoints{}, cfg, ip4, netip.Addr{}); err != nil || srv.reqs[1].URL.RequestURI() != "/update?key=s3cret&a=203.0.113.7&aaaa=" {
		t.Errorf("IPv4 only: %v %s", err, srv.reqs[1].URL.RequestURI())
	}
	status = http.StatusInternalServerError
	if e, ok := Update(context.Background(), srv.Client(), Endpoints{}, cfg, ip4, ip6).(*Error); !ok || e.Permanent || !strings.Contains(e.Msg, "HTTP 500") || strings.Contains(e.Msg, "s3cret") {
		t.Errorf("500: %#v", e)
	}
	status = http.StatusForbidden
	if e, ok := Update(context.Background(), srv.Client(), Endpoints{}, cfg, ip4, ip6).(*Error); !ok || !e.Permanent {
		t.Errorf("403: %#v", e)
	}
	srv.Close()
	if e, ok := Update(context.Background(), http.DefaultClient, Endpoints{}, cfg, ip4, ip6).(*Error); !ok || strings.Contains(e.Msg, "s3cret") || strings.Contains(e.Msg, "update?") {
		t.Errorf("unreachable, and the message carries the address with its secret: %#v", e)
	}
}

// The manager updates when there is something new to say, once a day
// otherwise, and after a failure with a growing pause.
func TestManager(t *testing.T) {
	answer := "OK"
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, answer) })
	now := time.Unix(1_800_000_000, 0)
	changes := 0
	var logged strings.Builder
	m := &Manager{
		Log: slog.New(slog.NewTextHandler(&logged, nil)), Client: srv.Client(), Endpoints: Endpoints{DuckDNS: srv.URL},
		Now: func() time.Time { return now }, OnChange: func() { changes++ },
	}
	step := func(after time.Duration) int {
		now = now.Add(after)
		before := srv.count()
		m.step(context.Background())
		return srv.count() - before
	}
	cfg := config.DDNS{Enabled: true, Provider: config.DDNSDuckDNS, Hostname: "example.duckdns.org", Password: "tok-123", IPv6: true}

	// Nothing wanted, nothing done.
	if n := step(0); n != 0 || m.Status().State != "off" {
		t.Fatalf("before anything is set up: %d requests, %+v", n, m.Status())
	}
	// Switched on without an address to publish: it waits, and says so.
	m.Want(cfg, netip.Addr{}, netip.Addr{})
	if n := step(0); n != 0 || m.Status().State != "waiting" || m.Status().Error == "" {
		t.Fatalf("without an address: %d requests, %+v", n, m.Status())
	}
	// The address is there: one update.
	m.Want(cfg, ip4, ip6)
	if n := step(time.Second); n != 1 {
		t.Fatalf("first update: %d requests", n)
	}
	if st := m.Status(); st.State != "ok" || st.Address != "203.0.113.7" || st.Address6 != "2001:db8::7" || st.Updated != now.Unix() || st.Error != "" {
		t.Errorf("after the first update: %+v", st)
	}
	// Nothing changed: nothing sent, however often it is looked at, and
	// however often the same is asked for.
	for i := 0; i < 20; i++ {
		m.Want(cfg, ip4, ip6)
		if n := step(time.Hour); n != 0 {
			t.Fatalf("%d hours on with nothing changed: %d requests", i+1, n)
		}
	}
	// A day after the last update it is said once more.
	if n := step(4 * time.Hour); n != 1 {
		t.Errorf("after a day: %d requests", n)
	}
	// A new address: at once.
	other := netip.MustParseAddr("203.0.113.99")
	m.Want(cfg, other, ip6)
	if n := step(time.Minute); n != 1 || m.Status().Address != "203.0.113.99" || srv.reqs[srv.count()-1].URL.Query().Get("ip") != "203.0.113.99" {
		t.Errorf("new address: %d requests, %+v", n, m.Status())
	}
	// With IPv6 not to be published, the IPv6 address is left out, and
	// the change of setting is an update.
	v4only := cfg
	v4only.IPv6 = false
	m.Want(v4only, other, ip6)
	if n := step(time.Minute); n != 1 || m.Status().Address6 != "" || srv.reqs[srv.count()-1].URL.Query().Has("ipv6") {
		t.Errorf("IPv4 only: %d requests, %+v", n, m.Status())
	}
	// Asked for by hand: an update whether or not one is due.
	m.Refresh()
	if n := step(time.Second); n != 1 {
		t.Errorf("update on request: %d requests", n)
	}
	if n := step(time.Second); n != 0 {
		t.Errorf("after the update on request: %d requests", n)
	}

	// The provider has trouble. Tried again after a minute, then two,
	// four and so on up to an hour, and not in between.
	answer = "try later"
	m.Want(cfg, ip4, ip6)
	if n := step(time.Second); n != 1 || m.Status().State != "error" || !strings.Contains(m.Status().Error, "DuckDNS answered") {
		t.Fatalf("failing update: %d requests, %+v", n, m.Status())
	}
	// What was last published is not forgotten over an error.
	if m.Status().Address != "203.0.113.99" || m.Status().Updated == 0 {
		t.Errorf("status of a failing update lost what the name still says: %+v", m.Status())
	}
	for i, pause := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour} {
		if n := step(pause - time.Second); n != 0 {
			t.Fatalf("retry %d came before its time (%v)", i+1, pause)
		}
		if n := step(time.Second); n != 1 {
			t.Fatalf("retry %d did not come after %v", i+1, pause)
		}
	}
	// It works again: back to normal, and the pauses start over.
	answer = "OK"
	if n := step(time.Hour); n != 1 || m.Status().State != "ok" || m.Status().Error != "" {
		t.Fatalf("recovery: %d requests, %+v", n, m.Status())
	}
	// Refused outright: not asked again until a day has passed, or
	// something is changed.
	answer = "KO"
	m.Want(cfg, other, ip6)
	if n := step(time.Second); n != 1 || m.Status().State != "error" {
		t.Fatalf("refused: %d requests, %+v", n, m.Status())
	}
	if n := step(23 * time.Hour); n != 0 {
		t.Errorf("a refusal was put to the provider again after %d requests", n)
	}
	fixed := cfg
	fixed.Password = "tok-456"
	answer = "OK"
	m.Want(fixed, other, ip6)
	if n := step(time.Second); n != 1 || m.Status().State != "ok" {
		t.Errorf("after the token was changed: %d requests, %+v", n, m.Status())
	}
	// Switched off: forgotten.
	fixed.Enabled = false
	m.Want(fixed, other, ip6)
	if n := step(time.Second); n != 0 || m.Status() != (Status{State: "off"}) {
		t.Errorf("switched off: %d requests, %+v", n, m.Status())
	}
	if changes == 0 {
		t.Error("nobody was told that the status changed")
	}
	// Neither token is in anything that was logged.
	if log := logged.String(); strings.Contains(log, "tok-") || !strings.Contains(log, "example.duckdns.org") {
		t.Errorf("the log:\n%s", log)
	}
}

func TestClassify(t *testing.T) {
	a := netip.MustParseAddr
	none := netip.Addr{}
	for _, tc := range []struct {
		own, seen netip.Addr
		want      string
	}{
		{none, none, ReachOffline},
		{none, a("203.0.113.7"), ReachOffline},
		{a("203.0.113.7"), a("203.0.113.7"), ReachDirect},
		{a("203.0.113.7"), none, ReachDirect},             // looks public, and nothing says otherwise
		{a("203.0.113.7"), a("198.51.100.1"), ReachCGNAT}, // looks public and is not the router's
		{a("192.168.1.20"), a("198.51.100.1"), ReachNAT},
		{a("192.168.1.20"), none, ReachNAT},
		{a("10.0.0.5"), none, ReachNAT},
		{a("172.20.0.5"), none, ReachNAT},
		{a("169.254.3.4"), none, ReachNAT},
		{a("100.64.0.9"), none, ReachCGNAT},
		{a("100.127.255.9"), a("198.51.100.1"), ReachCGNAT},
		// What the internet saw decides: a provider that hands out such
		// an address and routes it all the same.
		{a("100.64.0.9"), a("100.64.0.9"), ReachDirect},
		{a("100.128.0.9"), none, ReachDirect},
	} {
		if got := Classify(tc.own, tc.seen); got != tc.want {
			t.Errorf("own %v, seen %v: %s, want %s", tc.own, tc.seen, got, tc.want)
		}
	}
}

func TestPublicAddr(t *testing.T) {
	body := ""
	status := http.StatusOK
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
	for answer, want := range map[string]string{
		"203.0.113.7":   "203.0.113.7",
		"203.0.113.7\n": "203.0.113.7",
		"fl=1\nh=cloudflare.com\nip=203.0.113.7\nts=1.5\nvisit_scheme=https\n": "203.0.113.7",
		"<html>no</html>": "",
		"ip=2001:db8::1":  "", // it is the IPv4 address that is asked for
		"0.0.0.0":         "",
		"":                "",
	} {
		body = answer
		got, err := PublicAddr(context.Background(), srv.URL+"/trace")
		if (want == "") != (err != nil) || (err == nil && got.String() != want) {
			t.Errorf("%q: %v %v, want %q", answer, got, err, want)
		}
	}
	body, status = "203.0.113.7", http.StatusServiceUnavailable
	if got, err := PublicAddr(context.Background(), srv.URL); err == nil {
		t.Errorf("an error page was taken for an answer: %v", got)
	}
}
