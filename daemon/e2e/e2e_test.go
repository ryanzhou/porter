//go:build e2e

// Package e2e runs the real porterd in a network of namespaces that stand
// in for an upstream network (with DHCP, DNS, a captive portal and a
// WireGuard server) and two client devices, and checks what actually
// crosses the router.
//
// It needs root and: ip, nft, dnsmasq, dhcpcd, wg. WireGuard uses the
// kernel module when present, otherwise the wireguard-go binary named by
// PORTER_WG_USERSPACE; without either the VPN checks are skipped.
//
//	go test -tags e2e -count=1 ./e2e/
package e2e

import (
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

type status struct {
	Internet string `json:"internet"`
	Active   string `json:"active"`
	WAN      []struct {
		If    string `json:"if"`
		IPv4  string `json:"ipv4"`
		HasV6 bool   `json:"has_v6"`
	} `json:"wan"`
	Portal struct {
		Captive bool   `json:"captive"`
		URL     string `json:"url"`
		Device  string `json:"device"`
	} `json:"portal"`
	VPNs []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"vpns"`
	LAN struct {
		Clients int `json:"clients"`
	} `json:"lan"`
}

type device struct {
	MAC      string `json:"mac"`
	IPv4     string `json:"ipv4"`
	Online   bool   `json:"online"`
	Hostname string `json:"hostname"`
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

	bin := filepath.Join(dir, "porterd")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/porterd").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

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
				if len(lines) > 120 {
					lines = lines[len(lines)-120:]
				}
				t.Logf("porterd log (tail):\n%s", strings.Join(lines, "\n"))
			}
			for _, name := range []string{"isp-dnsmasq.log", "vpn-dnsmasq.log", "wg-server.log"} {
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
	os.WriteFile(geoFile+".date", []byte("2026-11\n"), 0o644)
	os.Setenv("PORTER_SPEEDTEST_DURATION", "1500ms")
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
			if strings.HasPrefix(d.IPv4, "10.83.0.") {
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
		ula, _ := cfg.ULA()
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
		if err := c.Call("status.get", nil, &st0); err != nil || !st0.GeoIP.Available || st0.GeoIP.Date != "2026-11" {
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
		eventually(t, "captive portal detected", 30*time.Second, func() bool {
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

	t.Run("Events", func(t *testing.T) {
		var evs []struct {
			Kind string `json:"kind"`
		}
		if err := c.Call("events.list", nil, &evs); err != nil {
			t.Fatal(err)
		}
		kinds := map[string]int{}
		for _, e := range evs {
			kinds[e.Kind]++
		}
		if kinds["device"] != 2 {
			t.Errorf("expected exactly the two clients as new devices, got %v", kinds)
		}
		b, _ := json.Marshal(kinds)
		t.Logf("events: %s", b)
	})
}
