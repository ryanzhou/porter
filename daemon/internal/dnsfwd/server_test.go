package dnsfwd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// fakeUpstream answers every A query with a fixed address and counts hits.
type fakeUpstream struct {
	addr string
	hits atomic.Int32
	ip   string
}

func (f *fakeUpstream) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	f.hits.Add(1)
	m := new(dns.Msg)
	m.SetReply(r)
	if r.Question[0].Qtype == dns.TypeA {
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120},
			A:   net.ParseIP(f.ip),
		})
	}
	w.WriteMsg(m)
}

func startUpstream(t *testing.T, ip string) *fakeUpstream {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUpstream{addr: pc.LocalAddr().String(), ip: ip}
	srv := &dns.Server{PacketConn: pc, Handler: f}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })
	return f
}

func up(t *testing.T, s string) []config.Upstream {
	t.Helper()
	u, err := config.ParseUpstream(s)
	if err != nil {
		t.Fatal(err)
	}
	return []config.Upstream{u}
}

type learnRec struct {
	mu   sync.Mutex
	got  map[string][]netip.Addr
	ttls map[string]time.Duration
}

func (l *learnRec) learn(id string, addrs []netip.Addr, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got[id] = append(l.got[id], addrs...)
	l.ttls[id] = ttl
	return nil
}

func query(t *testing.T, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	c := &dns.Client{Timeout: 3 * time.Second}
	r, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	return r
}

func firstA(m *dns.Msg) string {
	for _, rr := range m.Answer {
		if a, ok := rr.(*dns.A); ok {
			return a.A.String()
		}
	}
	return ""
}

func TestForwarderPaths(t *testing.T) {
	direct := startUpstream(t, "192.0.2.1")
	wan := startUpstream(t, "192.0.2.2")
	vpn := startUpstream(t, "192.0.2.3")

	rec := &learnRec{got: map[string][]netip.Addr{}, ttls: map[string]time.Duration{}}
	var clientMAC atomic.Value
	clientMAC.Store("02:00:00:00:00:01")
	s := New(Hooks{
		LookupMAC: func(netip.Addr) string { return clientMAC.Load().(string) },
		Learn:     rec.learn,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	c := config.Default()
	c.VPNs = []config.VPN{{ID: "home", Name: "Home", Enabled: true}}
	c.Policy.Rules = []config.Rule{
		{ID: "r1", Enabled: true, Dst: config.Dst{Domains: []string{"tunnel.example"}}, Action: "vpn:home"},
		{ID: "r2", Enabled: true, Dst: config.Dst{Domains: []string{"ads.example"}}, Action: "block"},
		{ID: "r3", Enabled: true, Dst: config.Dst{Ports: []string{"25"}}, Action: "block"},
	}
	pol := &Policy{
		Hosts:        map[string][]netip.Addr{"porter.lan": {netip.MustParseAddr("10.83.0.1")}},
		Reverse:      map[netip.Addr]string{netip.MustParseAddr("10.83.0.1"): "porter.lan"},
		Blocked:      NewSuffixSet([]string{"blocked.example"}),
		Rules:        RulesFrom(c, nil),
		DeviceRoute:  map[string]string{"02:00:00:00:00:02": "vpn:home", "02:00:00:00:00:03": "block"},
		Default:      config.ActDirect,
		PortalDevs:   map[string]bool{"02:00:00:00:00:09": true},
		Direct:       up(t, direct.addr),
		DirectCustom: true,
		WAN:          up(t, wan.addr),
		VPN:          map[string]VPNDNS{"home": {Servers: up(t, vpn.addr), Up: true}},
	}
	s.SetPolicy(pol)

	s.loopbackClients = true
	if err := s.Listen([]string{"127.0.0.1:15353"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	addr := "127.0.0.1:15353"

	ask := func(name string) *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeA)
		cl := &dns.Client{Timeout: 3 * time.Second}
		r, _, err := cl.Exchange(m, addr)
		if err != nil {
			t.Fatalf("query %s: %v", name, err)
		}
		return r
	}
	// Default device: direct upstream.
	if got := firstA(ask("www.example.org")); got != "192.0.2.1" {
		t.Errorf("default path: got %q", got)
	}
	// Cached on repeat, with the question echoed in the asker's case.
	before := direct.hits.Load()
	if r := ask("WWW.example.ORG"); firstA(r) != "192.0.2.1" || r.Question[0].Name != "WWW.example.ORG." {
		t.Errorf("cache answer wrong: %v", r)
	}
	if direct.hits.Load() != before {
		t.Error("second query was not served from cache")
	}
	// Domain rule: resolved through the VPN's resolver, and learned.
	if got := firstA(ask("a.tunnel.example")); got != "192.0.2.3" {
		t.Errorf("domain rule path: got %q", got)
	}
	rec.mu.Lock()
	if len(rec.got["r1"]) != 1 || rec.got["r1"][0].String() != "192.0.2.3" || rec.ttls["r1"] != learnMin {
		t.Errorf("learned: %v ttl %v", rec.got, rec.ttls)
	}
	rec.mu.Unlock()
	// Block rule and global block list: NXDOMAIN without asking upstream.
	for _, n := range []string{"x.ads.example", "blocked.example"} {
		if r := ask(n); r.Rcode != dns.RcodeNameError {
			t.Errorf("%s: rcode %d", n, r.Rcode)
		}
	}
	// Local names and private reverse lookups never go upstream.
	hits := direct.hits.Load()
	if got := firstA(ask("porter.lan")); got != "10.83.0.1" {
		t.Errorf("local name: got %q", got)
	}
	m := new(dns.Msg)
	m.SetQuestion("1.0.83.10.in-addr.arpa.", dns.TypePTR)
	r, _, err := (&dns.Client{}).Exchange(m, addr)
	if err != nil || len(r.Answer) != 1 || r.Answer[0].(*dns.PTR).Ptr != "porter.lan." {
		t.Errorf("PTR: %v %v", r, err)
	}
	m.SetQuestion("7.7.168.192.in-addr.arpa.", dns.TypePTR)
	if r, _, _ := (&dns.Client{}).Exchange(m, addr); r == nil || r.Rcode != dns.RcodeNameError {
		t.Errorf("unknown private PTR should be NXDOMAIN: %v", r)
	}
	if direct.hits.Load() != hits {
		t.Error("local queries leaked upstream")
	}

	// Device routed through the VPN: all its lookups use the VPN resolver.
	clientMAC.Store("02:00:00:00:00:02")
	if got := firstA(ask("www.example.org")); got != "192.0.2.3" {
		t.Errorf("vpn device path: got %q", got)
	}
	// Blocked device: refused.
	clientMAC.Store("02:00:00:00:00:03")
	if r := ask("www.example.org"); r.Rcode != dns.RcodeRefused {
		t.Errorf("blocked device: rcode %d", r.Rcode)
	}
	// Portal device: the upstream network's resolver, whatever the rules.
	clientMAC.Store("02:00:00:00:00:09")
	if got := firstA(ask("a.tunnel.example")); got != "192.0.2.2" {
		t.Errorf("portal device path: got %q", got)
	}

	// A VPN profile without resolvers: configured upstreams go through
	// the tunnel; network-provided ones cannot.
	clientMAC.Store("02:00:00:00:00:02")
	p2 := *pol
	p2.VPN = map[string]VPNDNS{"home": {Mark: 0x10, Up: true, Public: true}}
	if pt := p2.decide("02:00:00:00:00:02", netip.Addr{}, "x.example"); pt.kind != "vpn" || pt.mark != 0x10 {
		t.Errorf("custom upstreams via tunnel: %+v", pt)
	}
	// Only the upstream network's resolver is known: it is not reachable
	// through the tunnel, and is not asked outside it either.
	p2.DirectCustom = false
	if pt := p2.decide("02:00:00:00:00:02", netip.Addr{}, "x.example"); pt.kind != "vpn" || pt.mark != 0x10 || pt.servers[0].Addr.Addr().String() != "1.1.1.1" {
		t.Errorf("no resolver known for the tunnel: %+v", pt)
	}
	// A tunnel to a few networks only: a public resolver is not behind it.
	p2.VPN = map[string]VPNDNS{"home": {Mark: 0x10, Up: true}}
	if pt := p2.decide("02:00:00:00:00:02", netip.Addr{}, "x.example"); pt.kind != "direct" {
		t.Errorf("split tunnel without a resolver: %+v", pt)
	}
	// A tunnel that is down: lookups go where the traffic goes.
	p2.VPN = map[string]VPNDNS{"home": {Mark: 0x10}}
	if pt := p2.decide("02:00:00:00:00:02", netip.Addr{}, "x.example"); pt.kind != "direct" {
		t.Errorf("tunnel down, no kill switch: %+v", pt)
	}
	p2.VPN = map[string]VPNDNS{"home": {Mark: 0x10, KillSwitch: true}}
	if pt := p2.decide("02:00:00:00:00:02", netip.Addr{}, "x.example"); pt.kind != "refuse" {
		t.Errorf("tunnel down, kill switch: %+v", pt)
	}
	// Paused wins over being the device that signs in.
	p2.Paused = map[string]bool{"02:00:00:00:00:09": true}
	if pt := p2.decide("02:00:00:00:00:09", netip.Addr{}, "x.example"); pt.kind != "refuse" {
		t.Errorf("paused sign-in device: %+v", pt)
	}

	// A network's devices are known by the addresses they have out of a
	// delegated prefix as well as by their private ones: its route, and a
	// rule for it, apply to a lookup from either.
	g := config.Default()
	g.Networks = append(g.Networks, config.Network{ID: "guest", Name: "Guest", Address: "10.84.0.1/24", ULAPrefix: "fd00:0:0:84::/64", Route: config.ActBlock})
	g.Policy.Rules = []config.Rule{{ID: "n1", Enabled: true, Src: config.Src{Networks: []string{"lan"}}, Dst: config.Dst{Domains: []string{"only.example"}}, Action: config.ActBlock}}
	global := map[string]netip.Prefix{"lan": netip.MustParsePrefix("2001:db8:1000:3::1/64"), "guest": netip.MustParsePrefix("2001:db8:1000:a::1/64")}
	for _, tc := range []struct {
		global     map[string]netip.Prefix
		from, name string
		want       string
	}{
		// The guest network's route is "block", which refuses lookups.
		{global, "10.84.0.77", "x.example", "refuse"},
		{global, "fd00:0:0:84::77", "x.example", "refuse"},
		{global, "2001:db8:1000:a::77", "x.example", "refuse"},
		{global, "2001:db8:1000:a::77", "www.only.example", "refuse"},
		// The rule is for the main network.
		{global, "2001:db8:1000:3::77", "x.example", "direct"},
		{global, "2001:db8:1000:3::77", "www.only.example", "block"},
		// Not told about the prefixes, the forwarder cannot place them.
		{nil, "2001:db8:1000:a::77", "x.example", "direct"},
		{nil, "2001:db8:1000:3::77", "www.only.example", "direct"},
	} {
		p3 := &Policy{Rules: RulesFrom(g, tc.global), NetRoute: NetRoutesFrom(g, tc.global), Default: config.ActDirect}
		if pt := p3.decide("", netip.MustParseAddr(tc.from), tc.name); pt.kind != tc.want {
			t.Errorf("%s asked from %s (delegated prefixes known: %v): %s, want %s", tc.name, tc.from, tc.global != nil, pt.kind, tc.want)
		}
	}

	// Query log has recorded paths.
	var sawVPN bool
	for _, q := range s.Recent("") {
		if q.Path == "vpn:home" {
			sawVPN = true
		}
	}
	if !sawVPN {
		t.Error("query log lacks vpn path")
	}
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dns.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"dns.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestEncryptedUpstreams(t *testing.T) {
	cert, pool := selfSigned(t)
	f := &fakeUpstream{ip: "192.0.2.53"}

	// DNS over TLS.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	dot := &dns.Server{Listener: ln, Handler: f}
	go dot.ActivateAndServe()
	t.Cleanup(func() { dot.Shutdown() })

	// DNS over HTTPS.
	doh := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if err := q.Unpack(body); err != nil || r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "bad", 400)
			return
		}
		m := new(dns.Msg)
		m.SetReply(q)
		m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.54")})
		out, _ := m.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(out)
	}))
	doh.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	doh.StartTLS()
	t.Cleanup(doh.Close)

	s := New(Hooks{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.up.roots = pool
	if err := s.Listen([]string{"127.0.0.1:15354"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	s.SetPolicy(&Policy{Default: config.ActDirect, Direct: up(t, "tls://"+ln.Addr().String()+"#dns.test")})
	for i := 0; i < 3; i++ { // repeated queries reuse the connection
		if got := firstA(query(t, "127.0.0.1:15354", "dot"+string(rune('a'+i))+".example", dns.TypeA)); got != "192.0.2.53" {
			t.Fatalf("DoT: got %q", got)
		}
	}
	// Wrong server name must fail closed.
	s.SetPolicy(&Policy{Default: config.ActDirect, Direct: up(t, "tls://"+ln.Addr().String()+"#other.test")})
	if r := query(t, "127.0.0.1:15354", "bad.example", dns.TypeA); r.Rcode != dns.RcodeServerFailure {
		t.Errorf("DoT with wrong name: rcode %d", r.Rcode)
	}

	s.SetPolicy(&Policy{Default: config.ActDirect, Direct: up(t, "https://dns.test/dns-query#"+doh.Listener.Addr().String())})
	if got := firstA(query(t, "127.0.0.1:15354", "doh.example", dns.TypeA)); got != "192.0.2.54" {
		t.Fatalf("DoH: got %q", got)
	}
}
