//go:build e2e

// Package e2e runs the real porterd in a network of namespaces that stand
// in for an upstream network (with DHCP, DNS, a captive portal and a
// WireGuard server) and two client devices, and checks what actually
// crosses the router.
//
// It needs root and: ip, nft, dnsmasq, dhcpcd, wg. WireGuard uses the
// kernel module when present, otherwise the wireguard-go binary named by
// PORTER_WG_USERSPACE; without either the VPN checks are skipped. The
// subtests for a home connection (home_test.go) also need pppd and
// pppoe-server, and a kernel with VLANs, PPPoE and IPv6; the one for
// several networks on a cable (trunk_test.go) a kernel with VLANs.
//
//	go test -tags e2e -count=1 ./e2e/
package e2e

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/vishvananda/netns"

	"github.com/ryanzhou/porter/daemon/internal/client"
	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/geoip"
	"github.com/ryanzhou/porter/daemon/internal/proto"
	"github.com/ryanzhou/porter/daemon/internal/vpn"
)

const (
	nsISP = "pt-isp"
	nsRtr = "pt-rtr"
	nsC1  = "pt-c1"
	nsC2  = "pt-c2"

	ispIP    = "100.64.0.1"
	siteA    = "198.51.100.10" // ordinary site, also the probe target
	siteB    = "198.51.100.20" // site reached through a domain rule
	vpnSrvIP = "10.99.0.1"
	vpnCliIP = "10.99.0.2"

	// IPv6, where the kernel has it: the upstream network hands out a /64
	// by router advertisement and nothing more (no prefix delegation).
	ispIP6  = "2001:db8:100::1"
	ispNet6 = "2001:db8:100:"
	site6   = "2001:db8:200::10"
)

func sh(t *testing.T, format string, a ...any) string {
	t.Helper()
	cmd := fmt.Sprintf(format, a...)
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, out)
	}
	return string(out)
}

func try(format string, a ...any) { exec.Command("sh", "-c", fmt.Sprintf(format, a...)).Run() }

// inNS runs f with the calling thread switched into a namespace. Sockets
// created inside stay in that namespace.
func inNS(name string, f func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		panic(err)
	}
	defer orig.Close()
	ns, err := netns.GetFromName(name)
	if err != nil {
		panic(err)
	}
	defer ns.Close()
	if err := netns.Set(ns); err != nil {
		panic(err)
	}
	defer netns.Set(orig)
	f()
}

func listen(t *testing.T, ns, addr string) net.Listener {
	t.Helper()
	var ln net.Listener
	var err error
	network := "tcp4"
	if strings.HasPrefix(addr, "[") {
		network = "tcp6"
	}
	inNS(ns, func() { ln, err = net.Listen(network, addr) })
	if err != nil {
		t.Fatalf("listen %s in %s: %v", addr, ns, err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// httpGet fetches a URL from inside a namespace and returns the body.
func httpGet(ns, url string) (string, error) {
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (c net.Conn, err error) {
			inNS(ns, func() { c, err = (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr) })
			return
		},
	}
	cl := &http.Client{Transport: tr, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := cl.Get(url)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return strings.TrimSpace(string(b)), nil
}

// lookup asks a DNS server from inside a namespace.
func lookup(ns, server, name string) (addr string, rcode int, err error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	var r *dns.Msg
	inNS(ns, func() {
		c := &dns.Client{Timeout: 3 * time.Second}
		r, _, err = c.Exchange(m, net.JoinHostPort(server, "53"))
	})
	if err != nil {
		return "", 0, err
	}
	for _, rr := range r.Answer {
		if a, ok := rr.(*dns.A); ok {
			return a.A.String(), r.Rcode, nil
		}
	}
	return "", r.Rcode, nil
}

func eventually(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// uplinkState is one internet source as the status describes it.
type uplinkState struct {
	If       string `json:"if"`
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Internet string `json:"internet"`
	IPv4     string `json:"ipv4"`
	HasV6    bool   `json:"has_v6"`
	MAC      string `json:"mac"`
	// A wired source that signs in to its provider says how that stands,
	// and any source which prefix was delegated to it.
	PPPoE  string `json:"pppoe"`
	Prefix string `json:"prefix"`
}

type status struct {
	Internet string        `json:"internet"`
	Active   string        `json:"active"`
	WAN      []uplinkState `json:"wan"`
	Portal   struct {
		Captive bool   `json:"captive"`
		URL     string `json:"url"`
		Device  string `json:"device"`
	} `json:"portal"`
	VPNs []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Type  string `json:"type"`
		Paths int    `json:"paths"`
		Of    int    `json:"of"`
	} `json:"vpns"`
	LAN struct {
		Clients int `json:"clients"`
		// Each network's /64 of a delegated prefix, by network id.
		Prefixes map[string]string `json:"prefixes"`
	} `json:"lan"`
	Control struct {
		Port  int      `json:"port"`
		Addrs []string `json:"addrs"`
	} `json:"control"`
}

// dialControl opens a control session over TCP from inside a namespace,
// as the app does from a phone on one of the router's networks. A session
// that the router means to refuse can still come back from here: the
// handshake is what shows the router whose it is.
func dialControl(ns, addr string, id proto.Identity) (*client.Client, error) {
	var conn net.Conn
	var err error
	inNS(ns, func() {
		conn, err = net.DialTimeout("tcp", net.JoinHostPort(addr, fmt.Sprint(proto.ControlPort)), 2*time.Second)
	})
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	cl, err := client.New(proto.NewStreamConn(conn), id)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return cl, nil
}

// answered reports whether a call on a control session gets an answer.
func answered(cl *client.Client, method string) bool {
	done := make(chan error, 1)
	go func() { done <- cl.Call(method, map[string]string{"name": "e2e"}, nil) }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(5 * time.Second):
		return false
	}
}

type device struct {
	MAC      string `json:"mac"`
	Network  string `json:"network"`
	IPv4     string `json:"ipv4"`
	Online   bool   `json:"online"`
	Hostname string `json:"hostname"`
	Link     string `json:"link"`
}

func TestRouter(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	for _, tool := range []string{"ip", "nft", "dnsmasq", "dhcpcd"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	// Unix socket paths are short; keep the runtime directory shallow.
	run, err := os.MkdirTemp("/tmp", "pt-run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })

	bin := daemonBinary(t, dir)

	var procs []*exec.Cmd
	start := func(name string, logTo string, argv ...string) *exec.Cmd {
		cmd := exec.Command(argv[0], argv[1:]...)
		if logTo != "" {
			f, _ := os.Create(filepath.Join(dir, logTo))
			cmd.Stdout, cmd.Stderr = f, f
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		procs = append(procs, cmd)
		return cmd
	}
	cleanup := func() {
		for _, p := range procs {
			if p.Process != nil {
				p.Process.Signal(os.Interrupt)
			}
		}
		time.Sleep(300 * time.Millisecond)
		for _, p := range procs {
			if p.Process != nil {
				p.Process.Kill()
				p.Wait()
			}
		}
		try("pkill -f 'dhcpcd.*pt-c[12]if'; pkill -f %s", run)
		for _, ns := range []string{nsISP, nsRtr, nsC1, nsC2} {
			try("ip netns pids %s 2>/dev/null | xargs -r kill -9", ns)
			try("ip netns del %s", ns)
		}
	}
	cleanup()
	t.Cleanup(func() {
		if t.Failed() {
			if b, err := os.ReadFile(filepath.Join(dir, "porterd.log")); err == nil {
				lines := strings.Split(string(b), "\n")
				if len(lines) > 400 {
					lines = lines[len(lines)-400:]
				}
				t.Logf("porterd log (tail):\n%s", strings.Join(lines, "\n"))
			}
			for _, cmd := range []string{"ip -n " + nsC2 + " -4 addr", "ip -n " + nsRtr + " -br link", "ip -n " + nsRtr + " -4 addr", "ip -n " + nsRtr + " -4 neigh"} {
				out, _ := exec.Command("sh", "-c", cmd).CombinedOutput()
				t.Logf("%s:\n%s", cmd, out)
			}
			for _, name := range []string{"isp-dnsmasq.log", "vpn-dnsmasq.log", "wg-server.log", "c2-dhcpcd.log"} {
				if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
					t.Logf("%s:\n%s", name, b)
				}
			}
		}
		cleanup()
	})

	// --- topology ---------------------------------------------------------
	for _, ns := range []string{nsISP, nsRtr, nsC1, nsC2} {
		sh(t, "ip netns add %s && ip -n %s link set lo up", ns, ns)
	}
	sh(t, "ip -n %s link add eth0 type veth peer name isp0 netns %s", nsRtr, nsISP)
	sh(t, "ip -n %s link add eth1 type veth peer name pt-c1if netns %s", nsRtr, nsC1)
	sh(t, "ip -n %s link add eth2 type veth peer name pt-c2if netns %s", nsRtr, nsC2)
	sh(t, "ip -n %s addr add %s/24 dev isp0 && ip -n %s link set isp0 up", nsISP, ispIP, nsISP)
	for _, a := range []string{siteA, siteB, "198.51.100.53"} {
		sh(t, "ip -n %s addr add %s/32 dev lo", nsISP, a)
	}
	sh(t, "ip -n %s link set pt-c1if up && ip -n %s link set pt-c2if up", nsC1, nsC2)
	v6 := fw.HaveIPv6()
	if v6 {
		sh(t, "ip -n %s addr add %s/64 dev isp0 nodad", nsISP, ispIP6)
		sh(t, "ip -n %s addr add %s/128 dev lo nodad", nsISP, site6)
	}

	// Upstream DHCP and DNS.
	ispConf := filepath.Join(dir, "isp-dnsmasq.conf")
	ispLines := []string{
		"interface=isp0", "except-interface=lo", "bind-interfaces", "no-resolv", "no-hosts", "user=root", "keep-in-foreground",
		"dhcp-range=100.64.0.50,100.64.0.99,255.255.255.0,10m", "dhcp-option=option:router," + ispIP,
		"dhcp-option=option:dns-server," + ispIP, "dhcp-leasefile=" + filepath.Join(dir, "isp.leases"),
		"address=/probe.test/" + siteA, "address=/direct.test/" + siteA, "address=/tunnel.test/" + siteB,
		"address=/dnspath.test/203.0.113.1", "address=/portal.test/" + ispIP,
		"address=/ads.blocked.test/" + siteA, "address=/tracker.test/" + siteA,
	}
	if v6 {
		// Addresses by router advertisement only.
		ispLines = append(ispLines, "enable-ra", "dhcp-range=::,constructor:isp0,ra-only,10m")
	}
	os.WriteFile(ispConf, []byte(strings.Join(ispLines, "\n")+"\n"), 0o644)
	start("isp dnsmasq", "isp-dnsmasq.log", "ip", "netns", "exec", nsISP, "dnsmasq", "--conf-file="+ispConf)

	// "The internet": every site reports the address it was reached from.
	var captive atomic.Bool
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "<HTML><BODY>Success</BODY></HTML>")
		case "/big":
			w.Write(make([]byte, 300_000))
		case "/down": // speed test download: as much as the client will take
			chunk := make([]byte, 32*1024)
			for i := 0; i < 8192; i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		case "/up": // speed test upload
			io.Copy(io.Discard, r.Body)
		case "/lists/ads.txt": // a block list, in two of the usual formats
			fmt.Fprint(w, "# test list\n0.0.0.0 ads.blocked.test\n||tracker.test^\n")
		case "/geoip/dbip-country-lite-" + geoip.Month(time.Now()) + ".csv.gz":
			// This month's country database: the second site has "moved".
			gz := gzip.NewWriter(w)
			fmt.Fprint(gz, "198.51.100.0,198.51.100.15,AU\n198.51.100.16,198.51.100.31,NZ\n")
			gz.Close()
		default:
			fmt.Fprint(w, host)
		}
	})
	for _, a := range []string{siteA, siteB} {
		go http.Serve(listen(t, nsISP, a+":80"), site)
	}
	if v6 {
		go http.Serve(listen(t, nsISP, "["+site6+"]:80"), site)
	}
	// The captive portal: while enabled, the upstream network redirects
	// every web request to its login page.
	go http.Serve(listen(t, nsISP, ispIP+":8080"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://portal.test/login", http.StatusFound)
	}))
	setCaptive := func(on bool) {
		captive.Store(on)
		if on {
			sh(t, "ip netns exec %s nft -f - <<'EOF'\ntable ip portal {\n chain pre {\n  type nat hook prerouting priority dstnat;\n  iifname \"isp0\" tcp dport 80 redirect to :8080\n }\n}\nEOF", nsISP)
		} else {
			try("ip netns exec %s nft delete table ip portal", nsISP)
		}
	}

	// --- VPN server ---------------------------------------------------------
	wgUser := os.Getenv("PORTER_WG_USERSPACE")
	kernelWG := exec.Command("sh", "-c", fmt.Sprintf("ip -n %s link add wgs0 type wireguard", nsISP)).Run() == nil
	haveVPN := kernelWG || wgUser != ""
	var srvPub, cliPriv string
	if haveVPN {
		if _, err := exec.LookPath("wg"); err != nil {
			t.Fatal("wg tool not installed")
		}
		if !kernelWG {
			c := start("wg server", "wg-server.log", "ip", "netns", "exec", nsISP, "env", "WG_PROCESS_FOREGROUND=1", wgUser, "-f", "wgs0")
			_ = c
			eventually(t, "userspace wg server interface", 5*time.Second, func() bool {
				return exec.Command("ip", "-n", nsISP, "link", "show", "wgs0").Run() == nil
			})
		}
		srvPriv, sp, _ := vpn.GenerateKey()
		cp, cliPub, _ := vpn.GenerateKey()
		srvPub, cliPriv = sp, cp
		keyFile := filepath.Join(dir, "srv.key")
		os.WriteFile(keyFile, []byte(srvPriv), 0o600)
		sh(t, "ip netns exec %s wg set wgs0 listen-port 51820 private-key %s peer %s allowed-ips %s/32", nsISP, keyFile, cliPub, vpnCliIP)
		sh(t, "ip -n %s addr add %s/24 dev wgs0 && ip -n %s link set wgs0 up", nsISP, vpnSrvIP, nsISP)
		// The VPN provider's own resolver answers differently, which
		// shows which resolver a client's query reached.
		vpnDNS := filepath.Join(dir, "vpn-dnsmasq.conf")
		os.WriteFile(vpnDNS, []byte(strings.Join([]string{
			"interface=wgs0", "except-interface=lo", "bind-interfaces", "listen-address=" + vpnSrvIP, "no-resolv", "no-hosts", "user=root", "keep-in-foreground",
			"no-dhcp-interface=wgs0", "pid-file=" + filepath.Join(dir, "vpn-dnsmasq.pid"),
			"address=/dnspath.test/203.0.113.2", "address=/direct.test/" + siteA, "address=/tunnel.test/" + siteB,
		}, "\n")+"\n"), 0o644)
		start("vpn dnsmasq", "vpn-dnsmasq.log", "ip", "netns", "exec", nsISP, "dnsmasq", "--conf-file="+vpnDNS)
	}

	// --- router ---------------------------------------------------------------
	// A miniature country database: the two sites are in different
	// "countries".
	geoFile := filepath.Join(dir, "geoip.csv")
	os.WriteFile(geoFile, []byte("198.51.100.0,198.51.100.15,AU\n198.51.100.16,198.51.100.31,JP\n2001:db8:200::,2001:db8:200:ffff:ffff:ffff:ffff:ffff,NZ\n"), 0o644)
	// Dated long ago, so that whatever the updater is offered is newer.
	os.WriteFile(geoFile+".date", []byte("2000-01\n"), 0o644)
	os.Setenv("PORTER_SPEEDTEST_DURATION", "1500ms")
	os.Setenv("PORTER_GEOIP_URL", "http://"+siteA+"/geoip/dbip-country-lite-%s.csv.gz")
	os.Setenv("PORTER_GEOIP_MIN", "1")
	// One phone is paired already, as if it had been over Bluetooth:
	// there is no Bluetooth here to pair one over, and control over the
	// network is for paired phones only.
	phone, err := proto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pairings, err := proto.LoadPairings(filepath.Join(dir, "data", "pairings.json"))
	if err == nil {
		err = pairings.Add(phone.Public, "Test phone")
	}
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"netns", "exec", nsRtr, bin, "-data", filepath.Join(dir, "data"), "-run", run, "-no-ble", "-debug", "-geoip", geoFile}
	if !kernelWG && wgUser != "" {
		args = append(args, "-wg-userspace", wgUser)
	}
	start("porterd", "porterd.log", append([]string{"ip"}, args...)...)
	var c *client.Client
	eventually(t, "control socket", 10*time.Second, func() bool {
		c, err = client.DialUnix(filepath.Join(run, "control.sock"))
		return err == nil
	})
	defer c.Close()

	getStatus := func() status {
		var st status
		if err := c.Call("status.get", nil, &st); err != nil {
			t.Fatalf("status.get: %v", err)
		}
		return st
	}
	editConfig := func(f func(cfg *config.Config)) {
		t.Helper()
		var cfg config.Config
		if err := c.Call("config.get", nil, &cfg); err != nil {
			t.Fatal(err)
		}
		f(&cfg)
		if err := c.Call("config.set", map[string]any{"config": &cfg}, nil); err != nil {
			t.Fatalf("config.set: %v", err)
		}
	}

	// The two extra ports are LAN ports; probe a site of our own.
	editConfig(func(cfg *config.Config) {
		for i := range cfg.Ethernet {
			if cfg.Ethernet[i].Name != "eth0" {
				cfg.Ethernet[i].Role = config.EthLAN
			}
		}
		cfg.WAN.Probe = config.Probe{URL: "http://probe.test/ok", Expect: "Success"}
		cfg.SpeedTest = config.SpeedTest{DownloadURL: "http://" + siteA + "/down", UploadURL: "http://" + siteA + "/up"}
	})

	// --- uplink ---------------------------------------------------------------
	eventually(t, "uplink online", 30*time.Second, func() bool { return getStatus().Internet == "online" })
	st := getStatus()
	if st.Active != "eth0" || !strings.HasPrefix(st.WAN[0].IPv4, "100.64.0.") {
		t.Fatalf("uplink: %+v", st)
	}
	wanIP := strings.Split(st.WAN[0].IPv4, "/")[0]

	// --- clients ----------------------------------------------------------------
	cliConf := filepath.Join(dir, "client-dhcpcd.conf")
	os.WriteFile(cliConf, []byte("script /bin/true\nnoipv4ll\nhostname\nipv6rs\nslaac hwaddr\n"), 0o644)
	start("client1 dhcp", "c1-dhcpcd.log", "ip", "netns", "exec", nsC1, "dhcpcd", "--nobackground", "--config", cliConf, "pt-c1if")
	start("client2 dhcp", "c2-dhcpcd.log", "ip", "netns", "exec", nsC2, "dhcpcd", "--nobackground", "--config", cliConf, "pt-c2if")
	var devs []device
	eventually(t, "both clients to get leases", 30*time.Second, func() bool {
		devs = nil
		c.Call("devices.list", nil, &devs)
		n := 0
		for _, d := range devs {
			// The name comes with the lease, and only with it: it shows
			// that the router hears about leases at all.
			if strings.HasPrefix(d.IPv4, "10.83.0.") && d.Hostname != "" {
				n++
			}
		}
		return n == 2
	})
	macOf := func(ns, ifname string) string {
		return strings.TrimSpace(sh(t, "ip netns exec %s cat /sys/class/net/%s/address", ns, ifname))
	}
	mac1, mac2 := macOf(nsC1, "pt-c1if"), macOf(nsC2, "pt-c2if")
	const router = "10.83.0.1"

	t.Run("NAT44", func(t *testing.T) {
		for _, ns := range []string{nsC1, nsC2} {
			eventually(t, ns+" reaching the internet", 10*time.Second, func() bool {
				got, err := httpGet(ns, "http://"+siteA+"/ip")
				return err == nil && got == wanIP
			})
		}
	})

	t.Run("DNS", func(t *testing.T) {
		if a, _, err := lookup(nsC1, router, "direct.test"); err != nil || a != siteA {
			t.Errorf("forwarded name: %q %v", a, err)
		}
		if a, _, err := lookup(nsC1, router, "porter.lan"); err != nil || a != router {
			t.Errorf("router name: %q %v", a, err)
		}
		// A client that insists on its own resolver is answered by the
		// router all the same.
		if a, _, err := lookup(nsC1, "198.51.100.53", "direct.test"); err != nil || a != siteA {
			t.Errorf("intercepted query: %q %v", a, err)
		}
	})

	t.Run("IPv6NAT66", func(t *testing.T) {
		if !v6 {
			t.Skip("this kernel has IPv6 disabled")
		}
		eventually(t, "uplink to get IPv6 by router advertisement", 40*time.Second, func() bool {
			s := getStatus()
			return len(s.WAN) > 0 && s.WAN[0].HasV6
		})
		// The client has only a ULA address, yet reaches an IPv6 site,
		// which sees the router's upstream address: NAT66.
		eventually(t, "client reaching an IPv6 site", 40*time.Second, func() bool {
			got, err := httpGet(nsC1, "http://["+site6+"]/ip")
			return err == nil && strings.HasPrefix(got, ispNet6)
		})
		addrs := sh(t, "ip -n %s -6 addr show dev pt-c1if scope global", nsC1)
		if !strings.Contains(addrs, "inet6 fd") || strings.Contains(addrs, ispNet6) {
			t.Errorf("client should hold only a ULA address:\n%s", addrs)
		}
		// The resolver answers on the router's IPv6 address too.
		var cfg config.Config
		if err := c.Call("config.get", nil, &cfg); err != nil {
			t.Fatal(err)
		}
		ula, _ := cfg.Main().ULA()
		if a, _, err := lookup(nsC1, ula.String(), "direct.test"); err != nil || a != siteA {
			t.Errorf("DNS over IPv6: %q %v", a, err)
		}
	})

	type flow struct {
		MAC     string `json:"mac"`
		Proto   string `json:"proto"`
		Dst     string `json:"dst"`
		Port    int    `json:"port"`
		Name    string `json:"name"`
		Country string `json:"country"`
		Path    string `json:"path"`
		Up      uint64 `json:"up"`
		Down    uint64 `json:"down"`
		Active  bool   `json:"active"`
	}
	findFlow := func(mac, dst string, match func(flow) bool) (flow, bool) {
		var list []flow
		if err := c.Call("flows.list", map[string]string{"mac": mac}, &list); err != nil {
			t.Fatalf("flows.list: %v", err)
		}
		for _, f := range list {
			if f.MAC == mac && f.Dst == dst && (match == nil || match(f)) {
				return f, true
			}
		}
		return flow{}, false
	}

	t.Run("TrafficAndFlows", func(t *testing.T) {
		// Look the site up by name, then fetch something sizeable from it.
		if a, _, err := lookup(nsC1, router, "direct.test"); err != nil || a != siteA {
			t.Fatalf("lookup: %q %v", a, err)
		}
		if _, err := httpGet(nsC1, "http://"+siteA+"/big"); err != nil {
			t.Fatal(err)
		}
		// The connection shows up by name, with country, path and byte counts.
		var f flow
		eventually(t, "the download in the flow list", 20*time.Second, func() bool {
			var ok bool
			f, ok = findFlow(mac1, siteA, func(f flow) bool { return f.Down >= 300_000 })
			return ok
		})
		if f.Proto != "tcp" || f.Port != 80 || f.Name != "direct.test" || f.Country != "AU" || f.Path != "direct" || f.Up == 0 {
			t.Errorf("flow: %+v", f)
		}
		// Queries the router answered itself are not listed as flows.
		if dns, ok := findFlow(mac1, "198.51.100.53", nil); ok {
			t.Errorf("intercepted DNS query listed as a flow: %+v", dns)
		}
		// Traffic history, per device and for the network.
		type sample struct {
			T    int64  `json:"t"`
			Up   uint64 `json:"up"`
			Down uint64 `json:"down"`
		}
		type history struct {
			Minutes []sample `json:"minutes"`
			Hours   []sample `json:"hours"`
		}
		sum := func(list []sample) (up, down uint64) {
			for _, s := range list {
				up, down = up+s.Up, down+s.Down
			}
			return
		}
		eventually(t, "the download in the traffic history", 20*time.Second, func() bool {
			var dev, all history
			c.Call("stats.get", map[string]string{"mac": mac1}, &dev)
			c.Call("stats.get", map[string]string{"mac": ""}, &all)
			if len(dev.Minutes) != 60 || len(dev.Hours) != 72 {
				return false
			}
			_, devDown := sum(dev.Minutes)
			_, hourDown := sum(dev.Hours)
			_, allDown := sum(all.Minutes)
			return devDown >= 300_000 && hourDown >= 300_000 && allDown >= devDown
		})
		var top []struct {
			MAC  string `json:"mac"`
			Down uint64 `json:"down"`
		}
		if err := c.Call("stats.top", map[string]int{"hours": 24}, &top); err != nil || len(top) == 0 || top[0].MAC != mac1 || top[0].Down < 300_000 {
			t.Errorf("top talkers: %+v %v", top, err)
		}
	})

	t.Run("AdBlocking", func(t *testing.T) {
		type adStatus struct {
			Adblock struct {
				Enabled bool   `json:"enabled"`
				Domains int    `json:"domains"`
				Blocked uint64 `json:"blocked"`
				Lists   []struct {
					Domains int    `json:"domains"`
					Error   string `json:"error"`
				} `json:"lists"`
			} `json:"adblock"`
		}
		// Off by default: nothing is filtered.
		if a, _, err := lookup(nsC1, router, "ads.blocked.test"); err != nil || a != siteA {
			t.Fatalf("before enabling: %q %v", a, err)
		}
		editConfig(func(cfg *config.Config) {
			cfg.DNS.Adblock = config.Adblock{Enabled: true, Lists: []string{"http://" + siteA + "/lists/ads.txt"}}
		})
		// The router downloads the list by itself and starts blocking.
		eventually(t, "block list downloaded and in effect", 30*time.Second, func() bool {
			_, rcode, err := lookup(nsC1, router, "ads.blocked.test")
			return err == nil && rcode == dns.RcodeNameError
		})
		if _, rcode, _ := lookup(nsC1, router, "deep.sub.tracker.test"); rcode != dns.RcodeNameError {
			t.Errorf("subdomain of an Adblock-style entry not blocked: rcode %d", rcode)
		}
		if a, _, err := lookup(nsC1, router, "direct.test"); err != nil || a != siteA {
			t.Errorf("unlisted name affected: %q %v", a, err)
		}
		var st adStatus
		if err := c.Call("status.get", nil, &st); err != nil || !st.Adblock.Enabled || st.Adblock.Domains != 2 || st.Adblock.Blocked < 2 ||
			len(st.Adblock.Lists) != 1 || st.Adblock.Lists[0].Domains != 2 || st.Adblock.Lists[0].Error != "" {
			t.Errorf("ad blocking status: %+v %v", st, err)
		}
		// The allow list wins over the block lists.
		editConfig(func(cfg *config.Config) { cfg.DNS.Adblock.Allow = []string{"tracker.test"} })
		eventually(t, "allowed domain resolving again", 10*time.Second, func() bool {
			a, _, err := lookup(nsC1, router, "deep.sub.tracker.test")
			return err == nil && a == siteA
		})
		// A device can be exempted; others stay filtered.
		if err := c.Call("device.set", config.Device{MAC: mac2, NoAdblock: true}, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "exempt device unfiltered", 10*time.Second, func() bool {
			a, _, err := lookup(nsC2, router, "ads.blocked.test")
			return err == nil && a == siteA
		})
		if _, rcode, _ := lookup(nsC1, router, "ads.blocked.test"); rcode != dns.RcodeNameError {
			t.Errorf("other device no longer filtered: rcode %d", rcode)
		}
		// Back to the defaults for the tests that follow.
		if err := c.Call("device.set", config.Device{MAC: mac2}, nil); err != nil {
			t.Fatal(err)
		}
		editConfig(func(cfg *config.Config) { cfg.DNS.Adblock.Enabled, cfg.DNS.Adblock.Allow = false, nil })
		eventually(t, "filtering off again", 10*time.Second, func() bool {
			a, _, err := lookup(nsC1, router, "ads.blocked.test")
			return err == nil && a == siteA
		})
	})

	type speedResult struct {
		Via       string  `json:"via"`
		LatencyMs float64 `json:"latency_ms"`
		DownMbps  float64 `json:"down_mbps"`
		UpMbps    float64 `json:"up_mbps"`
		Error     string  `json:"error"`
	}
	t.Run("SpeedTest", func(t *testing.T) {
		var res speedResult
		if err := c.Call("speedtest.run", map[string]string{"via": "direct"}, &res); err != nil {
			t.Fatal(err)
		}
		if res.Error != "" || res.DownMbps <= 1 || res.UpMbps <= 1 || res.LatencyMs <= 0 {
			t.Errorf("speed test: %+v", res)
		}
		var hist []speedResult
		if err := c.Call("speedtest.history", nil, &hist); err != nil || len(hist) != 1 || hist[0].DownMbps != res.DownMbps {
			t.Errorf("history: %+v %v", hist, err)
		}
		if err := c.Call("speedtest.run", map[string]string{"via": "vpn:nope"}, &res); err == nil {
			t.Error("speed test through an unknown VPN accepted")
		}
	})

	t.Run("BlockAndPause", func(t *testing.T) {
		if err := c.Call("device.set", config.Device{MAC: mac1, Name: "One", Paused: true}, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "paused device cut off", 10*time.Second, func() bool {
			_, err := httpGet(nsC1, "http://"+siteA+"/ip")
			return err != nil
		})
		if got, err := httpGet(nsC2, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("other device affected: %q %v", got, err)
		}
		if _, rcode, _ := lookup(nsC1, router, "direct.test"); rcode != dns.RcodeRefused {
			t.Errorf("paused device DNS rcode %d", rcode)
		}
		if err := c.Call("device.set", config.Device{MAC: mac1, Name: "One"}, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "resumed device back", 10*time.Second, func() bool {
			got, err := httpGet(nsC1, "http://"+siteA+"/ip")
			return err == nil && got == wanIP
		})
		// A destination rule: block one site for everyone.
		editConfig(func(cfg *config.Config) {
			cfg.Policy.Rules = []config.Rule{{ID: "blk", Name: "No B", Enabled: true, Dst: config.Dst{CIDRs: []string{siteB}, Proto: "tcp", Ports: []string{"80"}}, Action: config.ActBlock}}
		})
		eventually(t, "blocked destination refused", 10*time.Second, func() bool {
			_, err := httpGet(nsC1, "http://"+siteB+"/ip")
			return err != nil
		})
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("unblocked destination: %q %v", got, err)
		}
		editConfig(func(cfg *config.Config) { cfg.Policy.Rules = nil })
	})

	t.Run("VPNPolicy", func(t *testing.T) {
		if !haveVPN {
			t.Skip("no WireGuard implementation available")
		}
		profile := config.VPN{ID: "v1", Name: "Test VPN", Enabled: true, KillSwitch: true, WireGuard: config.WireGuard{
			PrivateKey: cliPriv, Addresses: []string{vpnCliIP + "/32"}, DNS: []string{vpnSrvIP},
			Peers: []config.WGPeer{{PublicKey: srvPub, Endpoint: ispIP + ":51820", AllowedIPs: []string{"0.0.0.0/0"}, Keepalive: 1}},
		}}
		editConfig(func(cfg *config.Config) {
			cfg.VPNs = []config.VPN{profile}
			cfg.Devices = []config.Device{{MAC: mac2, Name: "Two", Route: "vpn:v1"}}
		})
		eventually(t, "tunnel up", 30*time.Second, func() bool {
			s := getStatus()
			return len(s.VPNs) == 1 && s.VPNs[0].State == "up"
		})
		// Source split: device two leaves through the tunnel, device one
		// directly.
		eventually(t, "device two through the tunnel", 10*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err == nil && got == vpnCliIP
		})
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("device one should be direct: %q %v", got, err)
		}
		if v6 {
			// The tunnel carries IPv4 only. Device two's IPv6 must be
			// refused, not quietly sent out the uplink beside the tunnel.
			eventually(t, "IPv6 of the tunnelled device refused", 15*time.Second, func() bool {
				_, err := httpGet(nsC2, "http://["+site6+"]/ip")
				return err != nil
			})
			if got, err := httpGet(nsC1, "http://["+site6+"]/ip"); err != nil || !strings.HasPrefix(got, ispNet6) {
				t.Errorf("direct device IPv6 should be unaffected: %q %v", got, err)
			}
		}
		// DNS follows the path: device two's queries reach the VPN's
		// resolver, device one's the upstream network's.
		if a, _, err := lookup(nsC2, router, "dnspath.test"); err != nil || a != "203.0.113.2" {
			t.Errorf("tunnelled device resolver: %q %v", a, err)
		}
		if a, _, err := lookup(nsC1, router, "dnspath.test"); err != nil || a != "203.0.113.1" {
			t.Errorf("direct device resolver: %q %v", a, err)
		}

		// Destination split by domain: device one stays direct except for
		// one site.
		editConfig(func(cfg *config.Config) {
			cfg.Policy.Rules = []config.Rule{{ID: "dom", Name: "Tunnel site", Enabled: true, Dst: config.Dst{Domains: []string{"tunnel.test"}}, Action: "vpn:v1"}}
		})
		eventually(t, "domain rule steering device one", 10*time.Second, func() bool {
			a, _, err := lookup(nsC1, router, "www.tunnel.test")
			if err != nil || a != siteB {
				return false
			}
			got, err := httpGet(nsC1, "http://"+siteB+"/ip")
			return err == nil && got == vpnCliIP
		})
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("device one to other sites should stay direct: %q %v", got, err)
		}
		// Destination split by country: the site "in Japan" goes through
		// the tunnel, the one "in Australia" does not.
		var st0 struct {
			GeoIP struct {
				Available bool   `json:"available"`
				Date      string `json:"date"`
			} `json:"geoip"`
		}
		if err := c.Call("status.get", nil, &st0); err != nil || !st0.GeoIP.Available || st0.GeoIP.Date != "2000-01" {
			t.Errorf("country database status: %+v %v", st0, err)
		}
		editConfig(func(cfg *config.Config) {
			cfg.Policy.Rules = []config.Rule{{ID: "geo", Name: "Japan via VPN", Enabled: true, Dst: config.Dst{Countries: []string{"jp"}}, Action: "vpn:v1"}}
		})
		eventually(t, "country rule steering device one", 10*time.Second, func() bool {
			got, err := httpGet(nsC1, "http://"+siteB+"/ip")
			return err == nil && got == vpnCliIP
		})
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("device one to the other country should stay direct: %q %v", got, err)
		}
		// The flow list shows which path each connection took.
		eventually(t, "tunnelled flow listed with its path and country", 30*time.Second, func() bool {
			// Keep the connection table populated: applying a rule change
			// clears it, and that may still be in progress.
			httpGet(nsC1, "http://"+siteB+"/ip")
			f, ok := findFlow(mac1, siteB, func(f flow) bool { return f.Path == "vpn:v1" })
			return ok && f.Country == "JP"
		})
		// A speed test through the tunnel measures the tunnel.
		var viaVPN speedResult
		if err := c.Call("speedtest.run", map[string]string{"via": "vpn:v1"}, &viaVPN); err != nil {
			t.Fatal(err)
		}
		if viaVPN.Error != "" || viaVPN.DownMbps <= 1 || viaVPN.UpMbps <= 1 {
			t.Errorf("speed test through the VPN: %+v", viaVPN)
		}
		if _, ok := findFlow("", siteA, nil); ok {
			// The router's own test traffic is not a LAN device's flow.
			var all []flow
			c.Call("flows.list", map[string]string{"mac": ""}, &all)
			for _, f := range all {
				if f.MAC == "" {
					t.Errorf("router's own connection listed as a flow: %+v", f)
				}
			}
		}
		// Destination split by port, overriding device two's VPN route.
		editConfig(func(cfg *config.Config) {
			cfg.Policy.Rules = []config.Rule{{ID: "prt", Name: "Web direct", Enabled: true, Src: config.Src{Devices: []string{mac2}}, Dst: config.Dst{CIDRs: []string{siteB}, Proto: "tcp", Ports: []string{"80"}}, Action: config.ActDirect}}
		})
		eventually(t, "port rule sending device two direct", 10*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+siteB+"/ip")
			return err == nil && got == wanIP
		})
		if got, err := httpGet(nsC2, "http://"+siteA+"/ip"); err != nil || got != vpnCliIP {
			t.Errorf("device two elsewhere should use the tunnel: %q %v", got, err)
		}
		editConfig(func(cfg *config.Config) { cfg.Policy.Rules = nil })

		// Kill switch: with the tunnel gone, device two must be cut off,
		// not quietly sent out the uplink.
		editConfig(func(cfg *config.Config) { cfg.VPNs[0].Enabled = false })
		eventually(t, "kill switch cutting device two off", 10*time.Second, func() bool {
			_, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err != nil
		})
		for i := 0; i < 5; i++ {
			if got, err := httpGet(nsC2, "http://"+siteA+"/ip"); err == nil {
				t.Fatalf("leak past kill switch: reached the site from %q", got)
			}
		}
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("device one should be unaffected: %q %v", got, err)
		}

		// Captive portal while a device is kill-switched: only the portal
		// device gets through, straight to the upstream network.
		setCaptive(true)
		// A working uplink is probed every 45 seconds, so that is how long
		// noticing can take.
		eventually(t, "captive portal detected", 60*time.Second, func() bool {
			s := getStatus()
			return s.Internet == "captive" && s.Portal.Captive
		})
		if s := getStatus(); s.Portal.URL != "http://portal.test/login" || s.Portal.Device == "" {
			t.Errorf("portal status: %+v", s.Portal)
		}
		if err := c.Call("portal.set_device", map[string]string{"mac": mac2}, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "portal device reaching the login redirect", 10*time.Second, func() bool {
			tr := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, addr string) (cn net.Conn, err error) {
				inNS(nsC2, func() { cn, err = (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp4", addr) })
				return
			}}
			cl := &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := cl.Get("http://" + siteA + "/ip")
			if err != nil {
				return false
			}
			res.Body.Close()
			return res.StatusCode == http.StatusFound && res.Header.Get("Location") == "http://portal.test/login"
		})
		// Its lookups go to the upstream network's resolver.
		if a, _, err := lookup(nsC2, router, "dnspath.test"); err != nil || a != "203.0.113.1" {
			t.Errorf("portal device resolver: %q %v", a, err)
		}
		// "Signing in" clears the portal; the kill switch applies again.
		setCaptive(false)
		eventually(t, "portal cleared", 30*time.Second, func() bool { return getStatus().Internet == "online" })
		eventually(t, "kill switch back after portal", 10*time.Second, func() bool {
			_, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err != nil
		})

		// Without the kill switch a switched-off tunnel falls back to the
		// uplink.
		editConfig(func(cfg *config.Config) { cfg.VPNs[0].KillSwitch = false })
		eventually(t, "fallback without kill switch", 10*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err == nil && got == wanIP
		})
	})

	t.Run("CountryDatabaseUpdate", func(t *testing.T) {
		month := geoip.Month(time.Now())
		type geoStatus struct {
			GeoIP struct {
				Date string `json:"date"`
			} `json:"geoip"`
		}
		if err := c.Call("updates.check", nil, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "this month's country database fetched and in use", 30*time.Second, func() bool {
			var st geoStatus
			c.Call("status.get", nil, &st)
			return st.GeoIP.Date == month
		})
		// The new data is what is used from here on: the second site is
		// now attributed to its new country.
		eventually(t, "new connections labelled from the new database", 30*time.Second, func() bool {
			httpGet(nsC1, "http://"+siteB+"/ip")
			_, ok := findFlow(mac1, siteB, func(f flow) bool { return f.Country == "NZ" })
			return ok
		})
		// And it was stored, so it survives a restart.
		if got := geoip.ReadDate(filepath.Join(dir, "data", "geoip.csv.gz")); got != month {
			t.Errorf("stored database date: %q, want %q", got, month)
		}
	})

	t.Run("MPQUIC", func(t *testing.T) {
		// The daemon runs the mpquic program named by PORTER_MPQUIC; the
		// same program is the remote end here.
		bin := os.Getenv("PORTER_MPQUIC")
		if bin == "" {
			t.Skip("PORTER_MPQUIC does not name an mpquic program")
		}
		if !haveVPN {
			t.Skip("no WireGuard implementation available")
		}
		const password = "a-long-password-for-the-test"
		remote := func() *exec.Cmd {
			return start("mpquic remote", "mpquic-remote.log", "ip", "netns", "exec", nsISP, bin, "remote", "-s", "0.0.0.0", "-p", "8388", "-k", password, "-u")
		}
		srv := remote()
		var before config.Config
		if err := c.Call("config.get", nil, &before); err != nil {
			t.Fatal(err)
		}
		editConfig(func(cfg *config.Config) {
			cfg.VPNs = append(cfg.VPNs, config.VPN{ID: "mq", Name: "Multipath", Enabled: true, KillSwitch: true, Type: config.VPNMPQUIC,
				MPQUIC: config.MPQUIC{Servers: []string{ispIP}, Password: password, DNS: []string{ispIP}}})
			cfg.Devices = []config.Device{{MAC: mac2, Name: "Two", Route: "vpn:mq"}}
		})
		tunnel := func() (state string, paths int) {
			for _, v := range getStatus().VPNs {
				if v.ID == "mq" {
					return v.State, v.Paths
				}
			}
			return "", 0
		}
		eventually(t, "mpquic tunnel up with its path", 60*time.Second, func() bool {
			state, paths := tunnel()
			return state == "up" && paths == 1
		})
		// Device two leaves through the remote, which sits beside the
		// site and so reaches it from the site's own address; device one
		// still goes out directly.
		eventually(t, "device two through mpquic", 20*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err == nil && got != "" && got != wanIP
		})
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("device one should be direct: %q %v", got, err)
		}
		// UDP goes through as well: device two's lookups are sent to the
		// tunnel's DNS server.
		eventually(t, "a lookup through mpquic", 20*time.Second, func() bool {
			a, _, err := lookup(nsC2, router, "portal.test")
			return err == nil && a == ispIP
		})

		// The profile's interface is a plain TUN device with mpquic
		// attached to it: nothing of WireGuard in between.
		if out, err := exec.Command("ip", "-n", nsRtr, "-d", "link", "show", "wg-mq").CombinedOutput(); err != nil ||
			!strings.Contains(string(out), "tun type tun") || strings.Contains(string(out), "wireguard") {
			t.Errorf("the MPQUIC profile's interface: %v\n%s", err, out)
		}

		// The remote goes away. The interface stays, but there is no
		// path: the tunnel must be reported down and, with the kill
		// switch, device two cut off.
		srv.Process.Kill()
		srv.Wait()
		eventually(t, "tunnel reported down without a path", 60*time.Second, func() bool {
			state, paths := tunnel()
			return state != "up" && paths == 0
		})
		eventually(t, "kill switch cutting device two off", 20*time.Second, func() bool {
			_, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err != nil
		})
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("device one should be unaffected: %q %v", got, err)
		}
		// And comes back.
		remote()
		eventually(t, "tunnel back up", 90*time.Second, func() bool {
			state, paths := tunnel()
			return state == "up" && paths == 1
		})
		eventually(t, "device two through mpquic again", 20*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err == nil && got != "" && got != wanIP
		})

		editConfig(func(cfg *config.Config) {
			cfg.VPNs, cfg.Devices = before.VPNs, before.Devices
		})
		eventually(t, "device two direct again", 20*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err == nil && got == wanIP
		})
	})

	t.Run("StaticUplink", func(t *testing.T) {
		const fixed4, fixed6 = "100.64.0.200", "2001:db8:100::200"
		set := func(ip config.IPSetup) {
			editConfig(func(cfg *config.Config) {
				for i := range cfg.Ethernet {
					if cfg.Ethernet[i].Name == "eth0" {
						cfg.Ethernet[i].IP = ip
					}
				}
			})
		}
		uplink4 := func() (string, string) {
			s := getStatus()
			for _, w := range s.WAN {
				if w.If == "eth0" {
					return w.IPv4, s.Internet
				}
			}
			return "", s.Internet
		}
		v4 := config.IPFamily{Mode: config.IPStatic, Address: fixed4 + "/24", Gateway: ispIP, DNS: []string{ispIP}}
		set(config.IPSetup{V4: v4})
		eventually(t, "uplink online on its fixed IPv4 address", 40*time.Second, func() bool {
			addr, internet := uplink4()
			return addr == fixed4+"/24" && internet == "online"
		})
		eventually(t, "client going out from the fixed address", 20*time.Second, func() bool {
			got, err := httpGet(nsC1, "http://"+siteA+"/ip")
			return err == nil && got == fixed4
		})
		// Names resolve through the DNS server that came with the address.
		if a, _, err := lookup(nsC1, router, "direct.test"); err != nil || a != siteA {
			t.Errorf("lookup with a fixed uplink address: %q %v", a, err)
		}
		if v6 {
			// IPv6 by hand as well; IPv4 stays as it is.
			set(config.IPSetup{V4: v4, V6: config.IPFamily{Mode: config.IPStatic, Address: fixed6 + "/64", Gateway: ispIP6}})
			eventually(t, "client going out from the fixed IPv6 address", 40*time.Second, func() bool {
				got, err := httpGet(nsC1, "http://["+site6+"]/ip")
				return err == nil && got == fixed6
			})
			if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != fixed4 {
				t.Errorf("IPv4 after fixing IPv6 too: %q %v", got, err)
			}
		}
		// And back to what the network hands out.
		set(config.IPSetup{})
		eventually(t, "uplink back on DHCP", 60*time.Second, func() bool {
			addr, internet := uplink4()
			return addr != "" && addr != fixed4+"/24" && internet == "online"
		})
		eventually(t, "client going out from the leased address", 20*time.Second, func() bool {
			got, err := httpGet(nsC1, "http://"+siteA+"/ip")
			return err == nil && got != "" && got != fixed4
		})
		if v6 {
			eventually(t, "IPv6 back to automatic", 60*time.Second, func() bool {
				got, err := httpGet(nsC1, "http://["+site6+"]/ip")
				return err == nil && strings.HasPrefix(got, ispNet6) && got != fixed6
			})
		}
	})

	// The app on a phone that is on the router's network: the same
	// sessions as over Bluetooth, for paired phones, from the router's own
	// networks only, and not at all once that is switched off.
	t.Run("ControlOverNetwork", func(t *testing.T) {
		ctl := getStatus().Control
		if ctl.Port != proto.ControlPort || len(ctl.Addrs) == 0 || ctl.Addrs[0] != router {
			t.Fatalf("status says the app connects to %+v", ctl)
		}
		if v6 && len(ctl.Addrs) != 2 {
			t.Errorf("with IPv6 on, the router names %v", ctl.Addrs)
		}
		// At every address the router names, a paired phone has a session
		// like any other.
		var over *client.Client
		for _, addr := range ctl.Addrs {
			cl, err := dialControl(nsC1, addr, phone)
			if err != nil {
				t.Fatalf("paired phone connecting to %s: %v", addr, err)
			}
			defer cl.Close()
			var hello struct {
				Paired  bool `json:"paired"`
				Control struct {
					Port  int      `json:"port"`
					Addrs []string `json:"addrs"`
				} `json:"control"`
			}
			if err := cl.Call("hello", nil, &hello); err != nil || !hello.Paired || hello.Control.Port != ctl.Port || len(hello.Control.Addrs) != len(ctl.Addrs) {
				t.Errorf("hello over %s: %+v %v", addr, hello, err)
			}
			var st status
			if err := cl.Call("status.get", nil, &st); err != nil || st.Internet != "online" {
				t.Errorf("status over %s: %+v %v", addr, st, err)
			}
			if err := cl.Call("ping", nil, nil); err != nil {
				t.Errorf("ping over %s: %v", addr, err)
			}
			over = cl
		}
		var phones []struct {
			Name     string `json:"name"`
			Self     bool   `json:"self"`
			LastSeen int64  `json:"last_seen"`
		}
		if err := over.Call("pairing.list", nil, &phones); err != nil || len(phones) != 1 || !phones[0].Self || phones[0].LastSeen == 0 {
			t.Errorf("paired phones as seen over the network: %+v %v", phones, err)
		}

		// A write sent twice under one name is made once: the app does
		// that when the answer to the first was lost with its link.
		var cfg config.Config
		if err := over.Call("config.get", nil, &cfg); err != nil {
			t.Fatal(err)
		}
		cfg.System.Name = "Twice"
		var first, again struct {
			Rev uint64 `json:"rev"`
		}
		write := map[string]any{"config": &cfg, "id": "e2e-write-1"}
		if err := over.Call("config.set", write, &first); err != nil || first.Rev != cfg.Rev+1 {
			t.Fatalf("config.set over the network: %+v %v", first, err)
		}
		// The second time over another link, as it would be.
		if err := c.Call("config.set", write, &again); err != nil || again.Rev != first.Rev {
			t.Errorf("the same write again: %+v %v", again, err)
		}
		if rev := getRev(t, c); rev != first.Rev {
			t.Errorf("after the same write twice the router is at revision %d, not %d", rev, first.Rev)
		}
		// Under no name, or another, it is the stale write it looks like.
		if err := c.Call("config.set", map[string]any{"config": &cfg}, nil); err == nil || !strings.Contains(err.Error(), proto.ErrConflict) {
			t.Errorf("a stale write without a name: %v", err)
		}
		editConfig(func(cfg *config.Config) { cfg.System.Name = "Porter" })

		// A phone that is not paired gets through the handshake, which is
		// what shows who it is, and no further: nothing it calls is
		// answered, asking to be paired included.
		stranger, _ := proto.NewIdentity()
		for _, method := range []string{"hello", "pair", "status.get"} {
			cl, err := dialControl(nsC1, router, stranger)
			if err != nil {
				continue // refused even earlier
			}
			if answered(cl, method) {
				t.Errorf("a phone that is not paired had %s answered over the network", method)
			}
			cl.Close()
		}
		if err := c.Call("pairing.list", nil, &phones); err != nil || len(phones) != 1 {
			t.Errorf("paired phones after a stranger asked: %+v %v", phones, err)
		}

		// Not from an internet source, whoever asks.
		if cl, err := dialControl(nsISP, wanIP, phone); err == nil {
			cl.Close()
			t.Error("the app's port is open to the internet source")
		}

		// Set to Bluetooth only: sessions that came over the network end,
		// nothing listens any more, and the status says so.
		editConfig(func(cfg *config.Config) { cfg.System.BluetoothOnly = true })
		listening := func() bool {
			var conn net.Conn
			var err error
			// From the router itself, where no firewall rule is in the way.
			inNS(nsRtr, func() {
				conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proto.ControlPort), time.Second)
			})
			if err == nil {
				conn.Close()
			}
			return err == nil
		}
		eventually(t, "nothing listening once set to Bluetooth only", 10*time.Second, func() bool {
			return !listening() && getStatus().Control.Port == 0
		})
		if len(getStatus().Control.Addrs) != 0 {
			t.Errorf("set to Bluetooth only, the router still names %v", getStatus().Control.Addrs)
		}
		if answered(over, "ping") {
			t.Error("a session over the network outlived the setting")
		}
		if cl, err := dialControl(nsC1, router, phone); err == nil {
			cl.Close()
			t.Error("a new session over the network after it was switched off")
		}
		editConfig(func(cfg *config.Config) { cfg.System.BluetoothOnly = false })
		eventually(t, "control over the network back", 10*time.Second, func() bool {
			cl, err := dialControl(nsC1, router, phone)
			if err != nil {
				return false
			}
			defer cl.Close()
			return answered(cl, "ping") && getStatus().Control.Port == proto.ControlPort
		})
	})

	// A second network: its own addresses, kept apart from the main one,
	// with a route and rules of its own.
	t.Run("Networks", func(t *testing.T) {
		const guestRouter = "10.84.0.1"
		rejoin := func() {
			// The client finds its port in another network the way a
			// device does that has been unplugged and plugged in again.
			sh(t, "ip -n %s link set pt-c2if down && sleep 1 && ip -n %s link set pt-c2if up", nsC2, nsC2)
		}
		addrOf := func(mac string) (ip, network string) {
			var devs []device
			c.Call("devices.list", nil, &devs)
			for _, d := range devs {
				if d.MAC == mac {
					return d.IPv4, d.Network
				}
			}
			return "", ""
		}
		var check struct {
			Error   string   `json:"error"`
			Effects []string `json:"effects"`
		}
		var draft config.Config
		if err := c.Call("config.get", nil, &draft); err != nil {
			t.Fatal(err)
		}
		// Looking at what a change would do changes nothing.
		draft.Networks = append(draft.Networks, config.Network{ID: "guest", Name: "Guest", Address: "10.83.0.129/25"})
		if err := c.Call("config.check", map[string]any{"config": &draft}, &check); err != nil || !strings.Contains(check.Error, "overlaps") {
			t.Errorf("config.check on overlapping networks: %+v %v", check, err)
		}
		editConfig(func(cfg *config.Config) {
			// Whatever the tests before this one set up for the two
			// clients is out of the way.
			cfg.Devices, cfg.Policy.Rules, cfg.Policy.Default = nil, nil, config.ActDirect
			cfg.Networks = append(cfg.Networks, config.Network{ID: "guest", Name: "Guest", Isolated: true})
			for i := range cfg.Ethernet {
				if cfg.Ethernet[i].Name == "eth2" {
					cfg.Ethernet[i].Network = "guest"
				}
			}
		})
		rejoin()
		var ip1, ip2 string
		eventually(t, "second client in the guest network", 60*time.Second, func() bool {
			var n1, n2 string
			ip1, n1 = addrOf(mac1)
			ip2, n2 = addrOf(mac2)
			return strings.HasPrefix(ip2, "10.84.0.") && n2 == "guest" && strings.HasPrefix(ip1, "10.83.0.") && n1 == "lan"
		})
		eventually(t, "guest reaching the internet", 20*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err == nil && got == wanIP
		})
		if a, _, err := lookup(nsC2, guestRouter, "direct.test"); err != nil || a != siteA {
			t.Errorf("DNS in the guest network: %q %v", a, err)
		}
		// An isolated network cannot open connections into the others,
		// but can be reached from them.
		hello := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello") })
		l1, l2 := listen(t, nsC1, ip1+":8088"), listen(t, nsC2, ip2+":8088")
		defer l1.Close()
		defer l2.Close()
		go http.Serve(l1, hello)
		go http.Serve(l2, hello)
		eventually(t, "main network reaching a device in the guest network", 15*time.Second, func() bool {
			got, err := httpGet(nsC1, "http://"+ip2+":8088/")
			return err == nil && got == "hello"
		})
		if got, err := httpGet(nsC2, "http://"+ip1+":8088/"); err == nil {
			t.Errorf("an isolated network reached into the main one: %q", got)
		}
		// Nor is the app let in from there, paired or not, at either of
		// the router's addresses; from the main network it still is.
		for _, addr := range []string{guestRouter, router} {
			if cl, err := dialControl(nsC2, addr, phone); err == nil {
				cl.Close()
				t.Errorf("the app's port at %s is open to an isolated network", addr)
			}
		}
		if addrs := getStatus().Control.Addrs; len(addrs) == 0 || addrs[0] != router || strings.Contains(strings.Join(addrs, " "), guestRouter) {
			t.Errorf("with the guest network isolated the app is told to connect to %v", addrs)
		}
		if cl, err := dialControl(nsC1, router, phone); err != nil || !answered(cl, "ping") {
			t.Errorf("the app from the main network while another is isolated: %v", err)
		} else {
			cl.Close()
		}
		editConfig(func(cfg *config.Config) { cfg.Network("guest").Isolated = false })
		eventually(t, "guest network, no longer isolated, reaching the main one", 15*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+ip1+":8088/")
			return err == nil && got == "hello"
		})
		eventually(t, "the app let in from the guest network once it is no longer isolated", 15*time.Second, func() bool {
			cl, err := dialControl(nsC2, guestRouter, phone)
			if err != nil {
				return false
			}
			defer cl.Close()
			return answered(cl, "ping") && strings.Contains(strings.Join(getStatus().Control.Addrs, " "), guestRouter)
		})

		// A route for the whole network, which a device's own overrides.
		editConfig(func(cfg *config.Config) { cfg.Network("guest").Route = config.ActBlock })
		eventually(t, "guest network blocked", 15*time.Second, func() bool {
			_, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err != nil
		})
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("main network while the guest network is blocked: %q %v", got, err)
		}
		editConfig(func(cfg *config.Config) {
			cfg.Devices = append(cfg.Devices, config.Device{MAC: mac2, Route: config.ActDirect})
		})
		eventually(t, "device's own route over its network's", 15*time.Second, func() bool {
			got, err := httpGet(nsC2, "http://"+siteA+"/ip")
			return err == nil && got == wanIP
		})
		// A rule for one network.
		editConfig(func(cfg *config.Config) {
			cfg.Network("guest").Route = ""
			for i := range cfg.Devices {
				if cfg.Devices[i].MAC == mac2 {
					cfg.Devices[i].Route = ""
				}
			}
			cfg.Policy.Rules = append(cfg.Policy.Rules, config.Rule{ID: "gst", Name: "Guests", Enabled: true,
				Src: config.Src{Networks: []string{"guest"}}, Dst: config.Dst{CIDRs: []string{siteA}}, Action: config.ActBlock})
		})
		eventually(t, "rule for the guest network", 15*time.Second, func() bool {
			_, errA := httpGet(nsC2, "http://"+siteA+"/ip")
			gotB, errB := httpGet(nsC2, "http://"+siteB+"/ip")
			return errA != nil && errB == nil && gotB == wanIP
		})
		if got, err := httpGet(nsC1, "http://"+siteA+"/ip"); err != nil || got != wanIP {
			t.Errorf("main network under a rule for the guest network: %q %v", got, err)
		}

		// A network that still has a rule cannot be removed; without it,
		// it can, and its bridge goes with it.
		var cfg config.Config
		if err := c.Call("config.get", nil, &cfg); err != nil {
			t.Fatal(err)
		}
		cfg.Networks = cfg.Networks[:1]
		if err := c.Call("config.set", map[string]any{"config": &cfg}, nil); err == nil {
			t.Error("removed a network that a port and a rule still refer to")
		}
		editConfig(func(cfg *config.Config) {
			cfg.Networks = cfg.Networks[:1]
			cfg.Policy.Rules = cfg.Policy.Rules[:len(cfg.Policy.Rules)-1]
			for i := range cfg.Ethernet {
				cfg.Ethernet[i].Network = config.MainNetwork
			}
		})
		rejoin()
		eventually(t, "second client back in the main network", 60*time.Second, func() bool {
			ip, n := addrOf(mac2)
			return strings.HasPrefix(ip, "10.83.0.") && n == "lan"
		})
		eventually(t, "guest bridge removed", 10*time.Second, func() bool {
			return exec.Command("ip", "-n", nsRtr, "link", "show", "br-guest").Run() != nil
		})
	})

	// A phone sharing its connection over USB: an uplink like any other,
	// ranked last unless told otherwise, and the one the router falls back
	// on when the uplink ahead of it stops reaching the internet.
	t.Run("UplinkOrder", func(t *testing.T) {
		const phoneGW = "100.65.0.1"
		sh(t, "ip -n %s link add usb0 type veth peer name isp1 netns %s", nsRtr, nsISP)
		defer try("ip -n %s link del usb0", nsRtr)
		sh(t, "ip -n %s addr add %s/24 dev isp1 && ip -n %s link set isp1 up", nsISP, phoneGW, nsISP)
		phoneConf := filepath.Join(dir, "phone-dnsmasq.conf")
		os.WriteFile(phoneConf, []byte(strings.Join([]string{
			"interface=isp1", "except-interface=lo", "bind-interfaces", "port=0", "user=root", "keep-in-foreground",
			"pid-file=" + filepath.Join(dir, "phone-dnsmasq.pid"),
			"dhcp-range=100.65.0.50,100.65.0.99,255.255.255.0,10m", "dhcp-option=option:router," + phoneGW,
			"dhcp-option=option:dns-server," + ispIP, "dhcp-leasefile=" + filepath.Join(dir, "phone.leases"),
		}, "\n")+"\n"), 0o644)
		start("phone dnsmasq", "phone-dnsmasq.log", "ip", "netns", "exec", nsISP, "dnsmasq", "--conf-file="+phoneConf)

		phoneIP := ""
		eventually(t, "phone listed as an uplink, online, behind the wired one", 60*time.Second, func() bool {
			s := getStatus()
			for _, w := range s.WAN {
				if w.If == "usb0" {
					phoneIP = strings.Split(w.IPv4, "/")[0]
					return w.Kind == "phone" && w.Key == "phone" && w.Internet == "online" && s.Active == "eth0"
				}
			}
			return false
		})
		wired := func() string {
			got, _ := httpGet(nsC1, "http://"+siteA+"/ip")
			return got
		}
		if got := wired(); got == phoneIP || got == "" {
			t.Fatalf("traffic with the wired uplink ranked first left from %q", got)
		}
		wiredIP := wired()

		// Ranked first, the phone is used.
		setOrder := func(first string) {
			editConfig(func(cfg *config.Config) {
				order := []string{first}
				for _, k := range cfg.WAN.Order {
					if k != first {
						order = append(order, k)
					}
				}
				cfg.WAN.Order = order
			})
		}
		setOrder("phone")
		eventually(t, "phone in use once ranked first", 30*time.Second, func() bool {
			return getStatus().Active == "usb0" && wired() == phoneIP
		})
		setOrder("eth:eth0")
		eventually(t, "wired uplink in use again", 30*time.Second, func() bool {
			return getStatus().Active == "eth0" && wired() == wiredIP
		})

		// The wired uplink keeps its address but stops leading anywhere:
		// the router steps past it, and comes back when it works again.
		sh(t, "ip netns exec %s nft -f - <<'EOF'\ntable inet dead {\n chain in {\n  type filter hook input priority -10;\n  iifname \"isp0\" udp dport 67 accept\n  iifname \"isp0\" drop\n }\n}\nEOF", nsISP)
		defer try("ip netns exec %s nft delete table inet dead", nsISP)
		eventually(t, "router falling back on the phone", 150*time.Second, func() bool {
			s := getStatus()
			return s.Active == "usb0" && s.Internet == "online" && wired() == phoneIP
		})
		sh(t, "ip netns exec %s nft delete table inet dead", nsISP)
		eventually(t, "router back on the wired uplink", 60*time.Second, func() bool {
			s := getStatus()
			return s.Active == "eth0" && s.Internet == "online" && wired() == wiredIP
		})
	})

	// How a home connection arrives: under a VLAN tag, through a PPPoE
	// sign-in, and with a prefix for the router's own networks.
	home := &rig{dir: dir, run: run, c: c, v6: v6, mac1: mac1, mac2: mac2, start: start}
	t.Run("TaggedPort", func(t *testing.T) { taggedPort(t, home) })
	t.Run("PPPoE", func(t *testing.T) { pppoe(t, home) })
	t.Run("PrefixDelegation", func(t *testing.T) { prefixDelegation(t, home) })

	// Devices finding each other across the router's networks, and
	// several networks on one cable (trunk_test.go).
	t.Run("Discovery", func(t *testing.T) { discovery(t, home) })
	t.Run("Trunk", func(t *testing.T) { trunk(t, home) })

	t.Run("Events", func(t *testing.T) {
		var evs []struct {
			Kind string `json:"kind"`
			MAC  string `json:"mac"`
		}
		if err := c.Call("events.list", nil, &evs); err != nil {
			t.Fatal(err)
		}
		kinds := map[string]int{}
		joined := map[string]int{}
		for _, e := range evs {
			kinds[e.Kind]++
			if e.Kind == "device" {
				joined[e.MAC]++
			}
		}
		// Each of the two clients was announced as a new device, once.
		// So, at most once, was each device a subtest put behind the
		// router, and nothing else: none of the router's own interfaces,
		// whatever it has made of them since.
		known := map[string]bool{mac1: true, mac2: true}
		for _, mac := range home.joined {
			known[mac] = true
		}
		if joined[mac1] != 1 || joined[mac2] != 1 {
			t.Errorf("expected each of the two clients as a new device once, got %v", joined)
		}
		for mac, n := range joined {
			if !known[mac] || n != 1 {
				t.Errorf("%s was announced as a new device %d times; the devices there were: %v", mac, n, known)
			}
		}
		b, _ := json.Marshal(kinds)
		t.Logf("events: %s", b)
	})
}

// getRev reads the revision of the router's configuration.
func getRev(t *testing.T, c *client.Client) uint64 {
	t.Helper()
	var cfg struct {
		Rev uint64 `json:"rev"`
	}
	if err := c.Call("config.get", nil, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Rev
}

// daemonBinary returns the porterd to test: the one PORTER_E2E_BIN names
// (for running the compiled tests somewhere without a Go toolchain, such
// as inside the image's userland), or else one built from this tree.
func daemonBinary(t *testing.T, dir string) string {
	t.Helper()
	if bin := os.Getenv("PORTER_E2E_BIN"); bin != "" {
		return bin
	}
	bin := filepath.Join(dir, "porterd")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/porterd").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}
