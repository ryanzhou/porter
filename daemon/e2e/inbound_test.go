//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/vpn"
)

// What comes in from outside, as subtests of TestRouter: the owner's own
// devices through the router's WireGuard server, and anybody through a
// forwarded port. The WireGuard part needs a WireGuard to be the device
// with (the kernel's, or the userspace one the VPN checks use) and skips
// itself where there is none; where the router's image is tested a skip
// is a failure.

// inboundView is what the status says about coming in from outside.
type inboundView struct {
	Reach   string `json:"reach"`
	Address string `json:"address"`
	Public  string `json:"public"`
	Public6 string `json:"public6"`
	Host    string `json:"host"`
	Server  struct {
		State     string `json:"state"`
		Error     string `json:"error"`
		PublicKey string `json:"public_key"`
		Peers     []struct {
			ID        string `json:"id"`
			Connected bool   `json:"connected"`
			Endpoint  string `json:"endpoint"`
			Rx        int64  `json:"rx"`
			Tx        int64  `json:"tx"`
		} `json:"peers"`
	} `json:"server"`
	Forwards []struct {
		ID  string   `json:"id"`
		To  string   `json:"to"`
		To6 []string `json:"to6"`
	} `json:"forwards"`
	DDNS struct {
		State   string `json:"state"`
		Address string `json:"address"`
		Error   string `json:"error"`
	} `json:"ddns"`
}

func (r *rig) inbound(t *testing.T) inboundView {
	t.Helper()
	var st struct {
		Inbound inboundView `json:"inbound"`
	}
	if err := r.c.Call("status.get", nil, &st); err != nil {
		t.Fatalf("status.get: %v", err)
	}
	return st.Inbound
}

// wanAddr is the IPv4 address the router has on its internet source.
func (r *rig) wanAddr(t *testing.T) string {
	t.Helper()
	w, st := r.source(t, "eth:eth0")
	if st.Active != "eth0" || w.IPv4 == "" {
		t.Fatalf("the router is not online through its wired source: %+v", st)
	}
	return strings.Split(w.IPv4, "/")[0]
}

// addrOf is the IPv4 address the router knows a device by.
func (r *rig) addrOf(t *testing.T, mac string) string {
	t.Helper()
	var devs []device
	if err := r.c.Call("devices.list", nil, &devs); err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		if d.MAC == mac {
			return d.IPv4
		}
	}
	return ""
}

// whoAsked serves, at an address inside a namespace, the address each
// request came from.
func whoAsked(t *testing.T, ns, addr string) net.Listener {
	t.Helper()
	ln := listen(t, ns, addr)
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprint(w, host)
	}))
	return ln
}

var showInbound = []string{
	"ip netns exec " + nsRtr + " nft list table inet porter",
	"ip -n " + nsRtr + " addr show", "ip -n " + nsRtr + " rule show", "ip -n " + nsRtr + " route show table all",
	"ip netns exec " + nsRtr + " wg show all", "ip netns exec " + nsRtr + " ss -lun", "ip netns exec " + nsRtr + " ss -ltn",
}

func reachHome(t *testing.T, r *rig) {
	if !r.haveVPN {
		t.Skip("no WireGuard implementation available")
	}
	const (
		awayIP     = "100.66.0.2"
		awayGW     = "100.66.0.1"
		homeRouter = "10.89.0.1" // the router in the server's own range
		mainRouter = "10.83.0.1"
	)
	mark := r.logMark()
	wanIP := r.wanAddr(t)
	ip1 := r.addrOf(t, r.mac1)
	if !strings.HasPrefix(ip1, "10.83.0.") {
		t.Fatalf("the first device is at %q, not on the main network", ip1)
	}
	var before config.Config
	if err := r.c.Call("config.get", nil, &before); err != nil {
		t.Fatal(err)
	}

	// Somewhere else: a namespace that reaches the router only across the
	// upstream network, which routes between the two as the internet does.
	try("ip netns del %s", nsAway)
	sh(t, "ip netns add %s && ip -n %s link set lo up", nsAway, nsAway)
	sh(t, "ip -n %s link add away0 type veth peer name isp-away netns %s", nsAway, nsISP)
	sh(t, "ip -n %s addr add %s/24 dev away0 && ip -n %s link set away0 up && ip -n %s route add default via %s", nsAway, awayIP, nsAway, nsAway, awayGW)
	sh(t, "ip -n %s addr add %s/24 dev isp-away && ip -n %s link set isp-away up", nsISP, awayGW, nsISP)
	sh(t, "ip netns exec %s sysctl -qw net.ipv4.ip_forward=1", nsISP)
	var wgClient *exec.Cmd
	defer func() {
		try("ip netns exec %s sysctl -qw net.ipv4.ip_forward=0", nsISP)
		if wgClient != nil && wgClient.Process != nil {
			wgClient.Process.Kill()
		}
		try("ip -n %s link del isp-away", nsISP)
		try("ip netns del %s", nsAway)
		// The router ends as it began, whatever became of the test.
		r.edit(t, func(cfg *config.Config) { cfg.Inbound, cfg.Policy.Rules = before.Inbound, before.Policy.Rules })
	}()
	defer r.explain(t, mark, []string{"wg-client.log"}, append(append([]string{}, showInbound...),
		"ip netns exec "+nsAway+" wg show all", "ip -n "+nsAway+" addr show", "ip -n "+nsAway+" route show", "ip -n "+nsISP+" route show",
		"ip netns exec "+nsISP+" sysctl net.ipv4.ip_forward")...)

	// Switched on. The router's address here is one of those a provider
	// hands out behind its own address translation, and nothing tells
	// the router otherwise yet: it takes itself to be unreachable.
	r.edit(t, func(cfg *config.Config) { cfg.Inbound.Server.Enabled = true })
	eventually(t, "the WireGuard server running", 20*time.Second, func() bool { return r.inbound(t).Server.State == "up" })
	in := r.inbound(t)
	if in.Reach != "cgnat" || in.Address != wanIP || in.Public != "" || in.Host == wanIP {
		t.Errorf("with an address out of 100.64.0.0/10 and no word from outside, the router says: %+v", in)
	}
	var added struct {
		ID   string `json:"id"`
		Conf string `json:"conf"`
		Host string `json:"host"`
	}
	if in.Public6 == "" {
		// Nothing to tell a device to connect to, and it says so instead
		// of handing out a configuration that leads nowhere.
		if err := r.c.Call("server.peer_add", map[string]any{"name": "Phone", "full": true}, &added); err == nil || !strings.Contains(err.Error(), "cannot be reached from outside") {
			t.Errorf("a device added while the router cannot be reached: %+v %v", added, err)
		}
	}
	// Asked, the internet says it sees the router at that very address:
	// reachable after all.
	r.edit(t, func(cfg *config.Config) { cfg.Inbound.CheckURL = "http://" + siteA + "/ip" })
	eventually(t, "the router finding that the internet sees it at its own address", 40*time.Second, func() bool {
		in = r.inbound(t)
		return in.Reach == "direct" && in.Public == wanIP && in.Host == wanIP
	})

	// A device that sends everything through home.
	if err := r.c.Call("server.peer_add", map[string]any{"name": "Phone", "full": true}, &added); err != nil {
		t.Fatalf("server.peer_add: %v", err)
	}
	wg, err := vpn.ParseConf(added.Conf)
	if err != nil {
		t.Fatalf("the device's configuration does not parse: %v\n%s", err, added.Conf)
	}
	if added.Host != wanIP || len(wg.Peers) != 1 || wg.Peers[0].Endpoint != wanIP+":51820" || wg.Peers[0].PublicKey != in.Server.PublicKey ||
		!strings.HasPrefix(strings.Join(wg.Peers[0].AllowedIPs, ","), "0.0.0.0/0,::/0") || len(wg.Addresses) == 0 || wg.Addresses[0] != "10.89.0.2/32" || len(wg.DNS) == 0 || wg.DNS[0] != homeRouter {
		t.Fatalf("the device's configuration:\n%s", added.Conf)
	}
	// The key in it is the device's alone: the router has kept the
	// public half and nothing else.
	var cfg config.Config
	if err := r.c.Call("config.get", nil, &cfg); err != nil {
		t.Fatal(err)
	}
	stored, _ := json.Marshal(cfg)
	pub, _ := vpn.PublicKey(wg.PrivateKey)
	if peers := cfg.Inbound.Server.Peers; len(peers) != 1 || peers[0].ID != added.ID || peers[0].PublicKey != pub || !peers[0].Enabled || !peers[0].Full || peers[0].Address != "10.89.0.2" {
		t.Fatalf("the device as the router keeps it: %+v", peers)
	}
	if strings.Contains(string(stored), wg.PrivateKey) {
		t.Error("the router has kept the device's private key")
	}

	// The device brings its end up from that configuration, the way
	// wg-quick would: the key, the peer, the address, and routes for what
	// its AllowedIPs cover (here the server's range, the main network, and
	// the test's internet in place of "everything").
	if r.kernelWG {
		sh(t, "ip -n %s link add wgc0 type wireguard", nsAway)
	} else {
		wgClient = r.start("wg client", "wg-client.log", "ip", "netns", "exec", nsAway, "env", "WG_PROCESS_FOREGROUND=1", r.wgUser, "-f", "wgc0")
		eventually(t, "the device's userspace WireGuard interface", 5*time.Second, func() bool {
			return exec.Command("ip", "-n", nsAway, "link", "show", "wgc0").Run() == nil
		})
	}
	keyFile := filepath.Join(r.dir, "away.key")
	os.WriteFile(keyFile, []byte(wg.PrivateKey), 0o600)
	sh(t, "ip netns exec %s wg set wgc0 private-key %s peer %s endpoint %s allowed-ips 0.0.0.0/0 persistent-keepalive 1", nsAway, keyFile, wg.Peers[0].PublicKey, wg.Peers[0].Endpoint)
	sh(t, "ip -n %s addr add %s dev wgc0 && ip -n %s link set wgc0 mtu 1420 up", nsAway, wg.Addresses[0], nsAway)
	for _, dst := range []string{"10.89.0.0/24", "10.83.0.0/24", "198.51.100.0/24"} {
		sh(t, "ip -n %s route add %s dev wgc0", nsAway, dst)
	}
	connected := func() (bool, inboundView) {
		in := r.inbound(t)
		return len(in.Server.Peers) > 0 && in.Server.Peers[0].Connected, in
	}
	eventually(t, "the device connected to the router's WireGuard server", 30*time.Second, func() bool { ok, _ := connected(); return ok })

	// It reaches a device at home, which sees it at its own address in
	// the server's range: routed, not translated.
	ln := whoAsked(t, nsC1, ip1+":8089")
	defer ln.Close()
	eventually(t, "the device away reaching a device on the main network", 15*time.Second, func() bool {
		got, err := httpGet(nsAway, "http://"+ip1+":8089/")
		return err == nil && got == "10.89.0.2"
	})
	// Names are the router's to look up: its own and the devices', and
	// the world's the way a device at home would have them looked up.
	if a, _, err := lookup(nsAway, homeRouter, "porter.lan"); err != nil || a != mainRouter {
		t.Errorf("a local name from away: %q %v", a, err)
	}
	if a, _, err := lookup(nsAway, homeRouter, "direct.test"); err != nil || a != siteA {
		t.Errorf("a name out in the world from away: %q %v", a, err)
	}
	// The internet through home: the site sees the router, not the
	// address the device has where it is.
	eventually(t, "the device away reaching the internet through the router", 15*time.Second, func() bool {
		return reaches(nsAway, siteA) == wanIP
	})
	// The app, with the phone's own identity, at the router's address in
	// the server's range, which the router names for it; and at the
	// address it knows from home, which the tunnel carries as well.
	ctl := r.status(t).Control
	if !strings.Contains(" "+strings.Join(ctl.Addrs, " ")+" ", " "+homeRouter+" ") {
		t.Errorf("with the server running the app is told to connect to %v, and not to %s", ctl.Addrs, homeRouter)
	}
	for _, addr := range []string{homeRouter, mainRouter} {
		cl, err := dialControl(nsAway, addr, r.phone)
		if err != nil {
			t.Errorf("the app from away, at %s: %v", addr, err)
			continue
		}
		var hello struct {
			Paired bool `json:"paired"`
		}
		if err := cl.Call("hello", nil, &hello); err != nil || !hello.Paired || !answered(cl, "ping") {
			t.Errorf("the app's session from away, at %s: %+v %v", addr, hello, err)
		}
		cl.Close()
	}
	// The status has the connection: where from, and that data moved.
	if ok, in := connected(); !ok || !strings.HasPrefix(in.Server.Peers[0].Endpoint, awayIP+":") || in.Server.Peers[0].Rx == 0 || in.Server.Peers[0].Tx == 0 {
		t.Errorf("the device's connection in the status: %+v", in.Server.Peers)
	}

	// The same Route rules as for a device at home: one by address takes
	// a site away from the devices that come in, and from them only.
	r.edit(t, func(cfg *config.Config) {
		cfg.Policy.Rules = append(cfg.Policy.Rules, config.Rule{ID: "away", Name: "Away", Enabled: true,
			Src: config.Src{CIDRs: []string{"10.89.0.0/24"}}, Dst: config.Dst{CIDRs: []string{siteB}}, Action: config.ActBlock})
	})
	eventually(t, "a rule by address applying to the device away", 15*time.Second, func() bool {
		return reaches(nsAway, siteB) == "" && reaches(nsAway, siteA) == wanIP
	})
	if got := reaches(nsC1, siteB); got != wanIP {
		t.Errorf("a rule for the devices away reached a device at home: %q", got)
	}
	r.edit(t, func(cfg *config.Config) { cfg.Policy.Rules = before.Policy.Rules })
	eventually(t, "the rule gone again", 15*time.Second, func() bool { return reaches(nsAway, siteB) == wanIP })

	// Switched off, the device is cut off from all of it at once, with
	// its side none the wiser.
	setPeer := func(on bool) {
		r.edit(t, func(cfg *config.Config) { cfg.Inbound.Server.Peers[0].Enabled = on })
	}
	setPeer(false)
	eventually(t, "a device that is switched off cut off from home", 20*time.Second, func() bool {
		_, err := httpGet(nsAway, "http://"+ip1+":8089/")
		_, _, dnsErr := lookup(nsAway, homeRouter, "porter.lan")
		ok, _ := connected()
		return err != nil && dnsErr != nil && reaches(nsAway, siteA) == "" && !ok
	})
	if cl, err := dialControl(nsAway, homeRouter, r.phone); err == nil {
		cl.Close()
		t.Error("the app got through from a device that is switched off")
	}
	setPeer(true)
	eventually(t, "the device back once switched on again", 30*time.Second, func() bool {
		got, err := httpGet(nsAway, "http://"+ip1+":8089/")
		return err == nil && got == "10.89.0.2"
	})

	// A second device, for the networks at home only: its configuration
	// covers them and the server's range, and nothing else.
	var split struct {
		Conf string `json:"conf"`
	}
	if err := r.c.Call("server.peer_add", map[string]any{"name": "Laptop", "full": false}, &split); err != nil {
		t.Fatalf("a second device: %v", err)
	}
	wg2, err := vpn.ParseConf(split.Conf)
	if err != nil || len(wg2.Peers) != 1 || wg2.Addresses[0] != "10.89.0.3/32" || wg2.PrivateKey == wg.PrivateKey {
		t.Fatalf("the second device's configuration: %v\n%s", err, split.Conf)
	}
	allowed := strings.Join(wg2.Peers[0].AllowedIPs, " ")
	if !strings.HasPrefix(allowed, "10.89.0.0/24 10.83.0.0/24") || strings.Contains(allowed, "/0") {
		t.Errorf("a device for the networks at home only sends %s", allowed)
	}

	// With a name kept up to date, devices are told the name. The
	// provider here is the test's own site, told the router's address.
	r.edit(t, func(cfg *config.Config) {
		cfg.Inbound.DDNS = config.DDNS{Enabled: true, Provider: config.DDNSCustom, Hostname: "home.example.test", URL: "http://" + siteA + "/nic?token=secret&ip={ip}"}
	})
	eventually(t, "the name updated with the router's public address", 30*time.Second, func() bool {
		in = r.inbound(t)
		return in.DDNS.State == "ok" && in.DDNS.Address == wanIP && in.Host == "home.example.test"
	})
	if err := r.c.Call("server.peer_add", map[string]any{"name": "Tablet", "full": true}, &split); err != nil || !strings.Contains(split.Conf, "Endpoint = home.example.test:51820") {
		t.Errorf("a device added with dynamic DNS set up: %v\n%s", err, split.Conf)
	}
	// Its secret is in the settings and nowhere a person or a log reads.
	var raw json.RawMessage
	r.c.Call("status.get", nil, &raw)
	if log, _ := os.ReadFile(filepath.Join(r.dir, "porterd.log")); strings.Contains(string(raw), "token=secret") || strings.Contains(string(log), "token=secret") {
		t.Error("the dynamic DNS address with its secret is in the status or the log")
	}

	// The server switched off: its interface goes, and the router's
	// address in its range is no longer one for the app.
	r.edit(t, func(cfg *config.Config) { cfg.Inbound.Server.Enabled = false })
	eventually(t, "the server gone once switched off", 15*time.Second, func() bool {
		gone := exec.Command("ip", "-n", nsRtr, "link", "show", "wgsrv").Run() != nil
		return gone && r.inbound(t).Server.State == "off" && !strings.Contains(strings.Join(r.status(t).Control.Addrs, " "), homeRouter)
	})
	if got := reaches(nsC1, siteA); got != wanIP {
		t.Errorf("a device at home after all this: %q", got)
	}
}

// forward returns a port forward to a device.
func forward(id, ports string, to int, mac string) config.PortForward {
	return config.PortForward{ID: id, Name: "Forward " + id, Enabled: true, Proto: config.ProtoTCP, Ports: ports, To: to, Device: mac}
}

func portForwards(t *testing.T, r *rig) {
	const mainRouter = "10.83.0.1"
	mark := r.logMark()
	wanIP := r.wanAddr(t)
	ip1 := r.addrOf(t, r.mac1)
	if !strings.HasPrefix(ip1, "10.83.0.") {
		t.Fatalf("the first device is at %q, not on the main network", ip1)
	}
	var before config.Config
	if err := r.c.Call("config.get", nil, &before); err != nil {
		t.Fatal(err)
	}
	defer r.edit(t, func(cfg *config.Config) { cfg.Inbound.Forwards, cfg.Devices = before.Inbound.Forwards, before.Devices })
	defer r.explain(t, mark, nil, append(append([]string{}, showInbound...), "ip -n "+nsC1+" addr show", "ip -n "+nsC2+" addr show", "ip -n "+nsC2+" route show")...)

	// The device listens on one port; the world is given another.
	for _, port := range []string{"8090", "9001"} {
		ln := whoAsked(t, nsC1, ip1+":"+port)
		defer ln.Close()
	}
	outside := func(ns string, port int) string {
		got, err := httpGet(ns, fmt.Sprintf("http://%s:%d/", wanIP, port))
		if err != nil {
			return ""
		}
		return got
	}
	if got := outside(nsISP, 8443); got != "" {
		t.Fatalf("before any port is forwarded, the router's port 8443 answers the internet: %q", got)
	}
	r.edit(t, func(cfg *config.Config) {
		cfg.Inbound.Forwards = []config.PortForward{forward("web", "8443", 8090, r.mac1)}
	})
	// The forward names the device, and the device now keeps the address
	// it had: that is where connections are sent.
	var cfg config.Config
	if err := r.c.Call("config.get", nil, &cfg); err != nil {
		t.Fatal(err)
	}
	if d := cfg.Device(r.mac1); d == nil || d.IP != ip1 {
		t.Fatalf("the device a port is forwarded to has the fixed address %+v, not %s", d, ip1)
	}
	// From the internet, at the router's address: the device answers,
	// and sees who is really asking.
	eventually(t, "a connection from the internet reaching the device through the forwarded port", 15*time.Second, func() bool {
		return outside(nsISP, 8443) == ispIP
	})
	// Nothing else is open for it: not the port the device listens on.
	if got := outside(nsISP, 8090); got != "" {
		t.Errorf("the device's own port answers the internet at the router's address: %q", got)
	}
	if in := r.inbound(t); len(in.Forwards) != 1 || in.Forwards[0].ID != "web" || in.Forwards[0].To != ip1 {
		t.Errorf("the forward in the status: %+v", in.Forwards)
	}
	// From inside, by the router's public address: the same device
	// answers, to the router, through which the connection returns.
	eventually(t, "the public address working from inside", 15*time.Second, func() bool {
		return outside(nsC2, 8443) == mainRouter
	})
	// The router's address in its own network is not the public one: the
	// port there is the router's, which has nothing on it.
	if got, err := httpGet(nsC2, "http://"+mainRouter+":8443/"); err == nil {
		t.Errorf("the forwarded port answers at the router's address inside: %q", got)
	}
	// What a device at home does on its own is as it was.
	if got := reaches(nsC1, siteA); got != wanIP {
		t.Errorf("a device with a port forwarded to it going out: %q", got)
	}

	// The fixed address stays for as long as the forward does.
	if err := r.c.Call("device.set", config.Device{MAC: r.mac1}, nil); err == nil || !strings.Contains(err.Error(), "port forwarded") {
		t.Errorf("taking the fixed address from a device with a forwarded port: %v", err)
	}
	// And the router's own ports are not for forwarding.
	var draft config.Config
	r.c.Call("config.get", nil, &draft)
	draft.Inbound.Forwards = append(draft.Inbound.Forwards, config.PortForward{ID: "dhcp", Name: "DHCP", Enabled: true, Proto: config.ProtoUDP, Ports: "68", Device: r.mac1})
	if err := r.c.Call("config.set", map[string]any{"config": &draft}, nil); err == nil || !strings.Contains(err.Error(), "needs UDP port 68 itself") {
		t.Errorf("forwarding the port the router gets its address on: %v", err)
	}

	// Open to some addresses only: not to the upstream network's, then to
	// it. From inside it works either way.
	only := func(from ...string) {
		r.edit(t, func(cfg *config.Config) { cfg.Inbound.Forwards[0].From = from })
	}
	only("203.0.113.0/24")
	eventually(t, "the port closed to an address it is not open to", 15*time.Second, func() bool { return outside(nsISP, 8443) == "" })
	if got := outside(nsC2, 8443); got != mainRouter {
		t.Errorf("from inside, a port open to some addresses only: %q", got)
	}
	only("203.0.113.0/24", "100.64.0.0/24")
	eventually(t, "the port open to an address it is open to", 15*time.Second, func() bool { return outside(nsISP, 8443) == ispIP })
	only()

	// A range, for both protocols, keeps its numbers.
	r.edit(t, func(cfg *config.Config) {
		f := forward("range", "9000-9002", 0, r.mac1)
		f.Proto = config.ProtoBoth
		cfg.Inbound.Forwards = append(cfg.Inbound.Forwards, f)
	})
	eventually(t, "a port out of a forwarded range reaching the device", 15*time.Second, func() bool { return outside(nsISP, 9001) == ispIP })
	if got := outside(nsISP, 8443); got != ispIP {
		t.Errorf("the first forward beside a second: %q", got)
	}

	// Switched off: refused, from outside and from inside, the
	// connections that were under way included.
	r.edit(t, func(cfg *config.Config) {
		for i := range cfg.Inbound.Forwards {
			cfg.Inbound.Forwards[i].Enabled = false
		}
	})
	eventually(t, "the forwarded ports closed once switched off", 15*time.Second, func() bool {
		return outside(nsISP, 8443) == "" && outside(nsISP, 9001) == "" && outside(nsC2, 8443) == ""
	})
	// Removed, the device is free to have its address taken again.
	r.edit(t, func(cfg *config.Config) { cfg.Inbound.Forwards = nil })
	if err := r.c.Call("device.set", config.Device{MAC: r.mac1}, nil); err != nil {
		t.Errorf("taking the fixed address once nothing is forwarded: %v", err)
	}
	if got := reaches(nsC1, siteA); got != wanIP {
		t.Errorf("a device at home after all this: %q", got)
	}
}

// forwardsAcrossNetworks runs while the second device is on a network of
// its own that is kept apart, with a listener at each device's port 8088
// that says hello: a forwarded port leads to a device on such a network,
// and from such a network to a device on another, as it would for anyone
// on the internet.
func forwardsAcrossNetworks(t *testing.T, r *rig, ip1 string) {
	mark := r.logMark()
	wanIP := r.wanAddr(t)
	var before config.Config
	if err := r.c.Call("config.get", nil, &before); err != nil {
		t.Fatal(err)
	}
	defer r.edit(t, func(cfg *config.Config) { cfg.Inbound.Forwards, cfg.Devices = before.Inbound.Forwards, before.Devices })
	defer r.explain(t, mark, nil, showInbound...)
	r.edit(t, func(cfg *config.Config) {
		cfg.Inbound.Forwards = []config.PortForward{forward("apart", "8481", 8088, r.mac2), forward("main", "8482", 8088, r.mac1)}
	})
	hello := func(ns string, port int) bool {
		got, err := httpGet(ns, fmt.Sprintf("http://%s:%d/", wanIP, port))
		return err == nil && got == "hello"
	}
	eventually(t, "the internet reaching a device on a network that is kept apart, through a forwarded port", 15*time.Second, func() bool { return hello(nsISP, 8481) })
	eventually(t, "the main network reaching that device by the public address", 15*time.Second, func() bool { return hello(nsC1, 8481) })
	eventually(t, "the network that is kept apart reaching a forwarded port of the main network by the public address", 15*time.Second, func() bool { return hello(nsC2, 8482) })
	// Which opens that port and nothing else: the device itself is as
	// far away as before.
	if got, err := httpGet(nsC2, "http://"+ip1+":8088/"); err == nil {
		t.Errorf("with a port forwarded, a network that is kept apart reached into the main one directly: %q", got)
	}
}

// forwardOverIPv6 runs while the first device has an address out of a
// delegated prefix and a listener at the port there: a forward opens
// that port at the device's own address, to whom it says and nobody else,
// and closes it again when it goes.
func forwardOverIPv6(t *testing.T, r *rig, dev netip.Addr, port int) {
	var before config.Config
	if err := r.c.Call("config.get", nil, &before); err != nil {
		t.Fatal(err)
	}
	defer r.edit(t, func(cfg *config.Config) { cfg.Inbound.Forwards, cfg.Devices = before.Inbound.Forwards, before.Devices })
	other := listen(t, nsC1, fmt.Sprintf("[%s]:%d", dev, port+1))
	defer other.Close()
	r.edit(t, func(cfg *config.Config) {
		cfg.Inbound.Forwards = []config.PortForward{forward("six", fmt.Sprint(port), 0, r.mac1)}
	})
	eventually(t, "a forwarded port open from the internet at the device's own IPv6 address", 30*time.Second, func() bool {
		return !refused(nsISP, dev, port)
	})
	if in := r.inbound(t); len(in.Forwards) != 1 || !strings.Contains(" "+strings.Join(in.Forwards[0].To6, " ")+" ", " "+dev.String()+" ") {
		t.Errorf("the status does not say the port is open at %s: %+v", dev, in.Forwards)
	}
	if !refused(nsISP, dev, port+1) {
		t.Errorf("a forward of port %d opened port %d of the device as well", port, port+1)
	}
	// Open to some addresses only: with none of IPv6 among them, to
	// nobody over IPv6; then to the upstream network's.
	only := func(from ...string) {
		r.edit(t, func(cfg *config.Config) { cfg.Inbound.Forwards[0].From = from })
	}
	only("203.0.113.0/24")
	eventually(t, "a port open to IPv4 addresses only, closed over IPv6", 15*time.Second, func() bool { return refused(nsISP, dev, port) })
	only("203.0.113.0/24", "2001:db8::/32")
	eventually(t, "the port open over IPv6 to the range it names", 15*time.Second, func() bool { return !refused(nsISP, dev, port) })
	r.edit(t, func(cfg *config.Config) { cfg.Inbound.Forwards = nil })
	eventually(t, "the device's port closed to the internet again", 15*time.Second, func() bool { return refused(nsISP, dev, port) })
}
