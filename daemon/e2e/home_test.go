//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/client"
	"github.com/ryanzhou/porter/daemon/internal/config"
)

// The ways a home connection arrives, as subtests of TestRouter: the
// internet under a VLAN tag, through a PPPoE sign-in, and with an IPv6
// prefix delegated for the router's own networks. They need kernel
// features that not every development machine has (VLANs, PPP, IPv6) and
// skip themselves there, saying which; where the router's image is tested
// (see in-image-userland.sh) a skip is a failure.
//
// Each explains itself when it fails: whoever reads the log of a run
// somewhere else has nothing but that log.

// rig is what these subtests need of TestRouter's set-up.
type rig struct {
	dir, run   string
	c          *client.Client
	v6         bool
	mac1, mac2 string
	start      func(name, logTo string, argv ...string) *exec.Cmd
}

func (r *rig) status(t *testing.T) status {
	t.Helper()
	var st status
	if err := r.c.Call("status.get", nil, &st); err != nil {
		t.Fatalf("status.get: %v", err)
	}
	return st
}

// source returns the internet source with the given key, as the status
// has it.
func (r *rig) source(t *testing.T, key string) (uplinkState, status) {
	t.Helper()
	st := r.status(t)
	for _, w := range st.WAN {
		if w.Key == key {
			return w, st
		}
	}
	return uplinkState{}, st
}

func (r *rig) edit(t *testing.T, f func(cfg *config.Config)) {
	t.Helper()
	var cfg config.Config
	if err := r.c.Call("config.get", nil, &cfg); err != nil {
		t.Fatal(err)
	}
	f(&cfg)
	if err := r.c.Call("config.set", map[string]any{"config": &cfg}, nil); err != nil {
		t.Fatalf("config.set: %v", err)
	}
}

// port edits the router's internet port.
func (r *rig) port(t *testing.T, f func(e *config.EthPort)) {
	t.Helper()
	r.edit(t, func(cfg *config.Config) {
		for i := range cfg.Ethernet {
			if cfg.Ethernet[i].Name == "eth0" {
				f(&cfg.Ethernet[i])
			}
		}
	})
}

// logMark notes where the router's log stands, for explain.
func (r *rig) logMark() int64 {
	if fi, err := os.Stat(filepath.Join(r.dir, "porterd.log")); err == nil {
		return fi.Size()
	}
	return 0
}

// explain logs, if the test has failed, what is needed to see why from
// the log alone: the router's status, what it logged since mark, the
// named log files of the test's own helpers, and the output of the
// commands. It is deferred, after the test's clean-up is, so that it sees
// things as they were when the test gave up.
func (r *rig) explain(t *testing.T, mark int64, files []string, cmds ...string) {
	if !t.Failed() {
		return
	}
	last := func(b []byte, n int) string {
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > n {
			lines = append([]string{fmt.Sprintf("(%d lines before these left out)", len(lines)-n)}, lines[len(lines)-n:]...)
		}
		return strings.Join(lines, "\n")
	}
	var st json.RawMessage
	if err := r.c.Call("status.get", nil, &st); err == nil {
		t.Logf("status:\n%s", st)
	}
	var cfg struct {
		Ethernet json.RawMessage `json:"ethernet"`
		Networks json.RawMessage `json:"networks"`
	}
	if err := r.c.Call("config.get", nil, &cfg); err == nil {
		t.Logf("configured ports: %s\nconfigured networks: %s", cfg.Ethernet, cfg.Networks)
	}
	for _, cmd := range cmds {
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		t.Logf("$ %s (%v)\n%s", cmd, err, out)
	}
	for _, name := range files {
		b, err := os.ReadFile(filepath.Join(r.dir, name))
		t.Logf("%s (%v):\n%s", name, err, last(b, 150))
	}
	if b, err := os.ReadFile(filepath.Join(r.dir, "porterd.log")); err == nil && int64(len(b)) >= mark {
		t.Logf("the router's log during this test:\n%s", last(b[mark:], 400))
	}
}

// The commands that show how the router and the provider's side stand.
var (
	showRouter = []string{
		"ip -n " + nsRtr + " -d link show", "ip -n " + nsRtr + " addr show", "ip -n " + nsRtr + " route show table all",
		"ip -n " + nsRtr + " -6 route show table all", "ip -n " + nsRtr + " rule show", "ip -n " + nsRtr + " -6 rule show",
	}
	showISP = []string{
		"ip -n " + nsISP + " -d link show", "ip -n " + nsISP + " addr show", "ip -n " + nsISP + " route show", "ip -n " + nsISP + " -6 route show",
	}
)

// showProcs is a command that lists the processes inside a namespace with
// their command lines (without ps, which the router's image lacks).
func showProcs(ns string) string {
	return "for p in $(ip netns pids " + ns + "); do echo \"$p $(tr '\\0' ' ' < /proc/$p/cmdline)\"; done"
}

// output runs a command and returns what it printed, errors included.
func output(format string, a ...any) string {
	out, _ := exec.Command("sh", "-c", fmt.Sprintf(format, a...)).CombinedOutput()
	return string(out)
}

// reaches fetches a site's /ip from inside a namespace: the address the
// site saw the request come from, or "".
func reaches(ns, site string) string {
	if strings.Contains(site, ":") {
		site = "[" + site + "]"
	}
	got, err := httpGet(ns, "http://"+site+"/ip")
	if err != nil {
		return ""
	}
	return got
}

// --- a tagged internet port ----------------------------------------------------

func taggedPort(t *testing.T, r *rig) {
	const tag = 35
	const gw = "100.66.0.1"
	sub := config.VLANIfName("eth0", tag) // on the router
	const ispSub = "isp0.35"              // on the provider's side of the cable
	if out, err := exec.Command("ip", "-n", nsISP, "link", "add", "link", "isp0", "name", ispSub, "type", "vlan", "id", strconv.Itoa(tag)).CombinedOutput(); err != nil {
		t.Skipf("this kernel cannot make VLAN interfaces (no 8021q): %s", bytes.TrimSpace(out))
	}
	defer try("ip -n %s link del %s", nsISP, ispSub)
	// Whatever happens, the port ends untagged, for the tests that follow.
	defer r.port(t, func(e *config.EthPort) { e.VLAN = 0 })
	defer r.explain(t, r.logMark(), []string{"tagged-dnsmasq.log"}, append(append([]string{}, showRouter...), showISP...)...)

	// The provider's network, which exists under the tag only: addresses
	// of its own, so that it shows which way traffic went.
	sh(t, "ip -n %s addr add %s/24 dev %s && ip -n %s link set %s up", nsISP, gw, ispSub, nsISP, ispSub)
	conf := filepath.Join(r.dir, "tagged-dnsmasq.conf")
	os.WriteFile(conf, []byte(strings.Join([]string{
		"interface=" + ispSub, "except-interface=lo", "bind-interfaces", "port=0", "user=root", "keep-in-foreground", "log-dhcp",
		"pid-file=" + filepath.Join(r.dir, "tagged-dnsmasq.pid"),
		"dhcp-range=100.66.0.50,100.66.0.99,255.255.255.0,10m", "dhcp-option=option:router," + gw,
		"dhcp-option=option:dns-server," + ispIP, "dhcp-leasefile=" + filepath.Join(r.dir, "tagged.leases"),
	}, "\n")+"\n"), 0o644)
	dhcp := r.start("tagged dnsmasq", "tagged-dnsmasq.log", "ip", "netns", "exec", nsISP, "dnsmasq", "--conf-file="+conf)
	defer dhcp.Process.Kill()

	untagged, _ := r.source(t, "eth:eth0")
	if untagged.If != "eth0" || !strings.HasPrefix(untagged.IPv4, "100.64.0.") {
		t.Fatalf("before the port is tagged its source is %+v", untagged)
	}

	r.port(t, func(e *config.EthPort) { e.VLAN = tag })
	var tagged uplinkState
	eventually(t, "the internet source online on the port's interface for the tag, with an address from the tagged network", 90*time.Second, func() bool {
		var st status
		tagged, st = r.source(t, "eth:eth0")
		return tagged.If == sub && strings.HasPrefix(tagged.IPv4, "100.66.0.") && tagged.Internet == "online" && st.Active == sub && st.Internet == "online"
	})
	// It is still the wired source it was, in the same place in the
	// order, and nothing else has become one.
	if tagged.Kind != "eth" || len(r.status(t).WAN) != 1 {
		t.Errorf("the sources with the port tagged: %+v", r.status(t).WAN)
	}
	// The interface is the port's VLAN and has the port's address.
	link := output("ip -n %s -d link show %s", nsRtr, sub)
	if !strings.Contains(link, sub+"@eth0") || !strings.Contains(link, "vlan protocol 802.1Q id "+strconv.Itoa(tag)) {
		t.Errorf("%s is not VLAN %d of eth0:\n%s", sub, tag, link)
	}
	portMAC := strings.TrimSpace(output("ip netns exec %s cat /sys/class/net/eth0/address", nsRtr))
	if subMAC := strings.TrimSpace(output("ip netns exec %s cat /sys/class/net/%s/address", nsRtr, sub)); subMAC != portMAC || tagged.MAC != portMAC {
		t.Errorf("hardware addresses: port %s, its VLAN %s, in the status %s", portMAC, subMAC, tagged.MAC)
	}
	// The untagged port itself has let go of its address.
	if addrs := output("ip -n %s -4 addr show dev eth0", nsRtr); strings.Contains(addrs, "inet ") {
		t.Errorf("the port still has an address of its own:\n%s", addrs)
	}
	// A device goes out through the tagged network.
	taggedIP := strings.Split(tagged.IPv4, "/")[0]
	eventually(t, "a device reaching the internet through the tagged port", 30*time.Second, func() bool {
		return reaches(nsC1, siteA) == taggedIP
	})
	if a, _, err := lookup(nsC1, "10.83.0.1", "direct.test"); err != nil || a != siteA {
		t.Errorf("lookup with the port tagged: %q %v", a, err)
	}
	// The router does not take its own interface for a port to be set up.
	var cfg config.Config
	if err := r.c.Call("config.get", nil, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, e := range cfg.Ethernet {
		if e.Name == sub {
			t.Errorf("%s is listed among the ports", sub)
		}
	}
	var ports struct {
		Ports []struct {
			Name string `json:"name"`
		} `json:"ports"`
	}
	if err := r.c.Call("status.get", nil, &ports); err != nil {
		t.Fatal(err)
	}
	for _, p := range ports.Ports {
		if p.Name == sub {
			t.Errorf("%s is listed among the plugged-in ports", sub)
		}
	}

	// Another tag: the interface for the first goes, and (nobody being
	// there under the new one) the source has no address.
	other := config.VLANIfName("eth0", tag+1)
	r.port(t, func(e *config.EthPort) { e.VLAN = tag + 1 })
	eventually(t, "the interface for the old tag replaced by one for the new", 30*time.Second, func() bool {
		w, _ := r.source(t, "eth:eth0")
		return w.If == other && w.IPv4 == "" && exec.Command("ip", "-n", nsRtr, "link", "show", sub).Run() != nil &&
			exec.Command("ip", "-n", nsRtr, "link", "show", other).Run() == nil
	})

	// Untagged again: the port is the source as before, and the VLAN
	// interface is gone.
	r.port(t, func(e *config.EthPort) { e.VLAN = 0 })
	eventually(t, "the untagged port online again", 90*time.Second, func() bool {
		w, st := r.source(t, "eth:eth0")
		return w.If == "eth0" && strings.HasPrefix(w.IPv4, "100.64.0.") && st.Active == "eth0" && st.Internet == "online"
	})
	if exec.Command("ip", "-n", nsRtr, "link", "show", other).Run() == nil {
		t.Errorf("%s left behind", other)
	}
	eventually(t, "a device reaching the internet through the untagged port", 30*time.Second, func() bool {
		return strings.HasPrefix(reaches(nsC1, siteA), "100.64.0.")
	})
}

// --- PPPoE ---------------------------------------------------------------------

// procsOf lists the processes among pids whose name is one of names.
func procsOf(pids []int, names ...string) []int {
	var out []int
	for _, pid := range pids {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue
		}
		for _, n := range names {
			if strings.TrimSpace(string(b)) == n {
				out = append(out, pid)
			}
		}
	}
	return out
}

// nsPids lists the processes inside a network namespace.
func nsPids(ns string) []int {
	var out []int
	for _, f := range strings.Fields(output("ip netns pids %s", ns)) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func pppoe(t *testing.T, r *rig) {
	for _, tool := range []string{"pppd", "pppoe-server"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if _, err := os.Stat("/dev/ppp"); err != nil {
		t.Skipf("this kernel has no PPP (ppp_generic): %v", err)
	}
	fd, err := unix.Socket(unix.AF_PPPOX, unix.SOCK_STREAM, 0) // PX_PROTO_OE
	if err != nil {
		t.Skipf("this kernel has no PPPoE (pppoe): %v", err)
	}
	unix.Close(fd)

	const (
		user        = "e2e-user@provider.test"
		secret      = "e2e secret #1" // a space and a '#': both need quoting in pppd's files
		serverIP    = "100.67.0.1"
		firstClient = "100.67.0.10"
		session     = "ppp-eth0"
	)
	if session != config.PPPIfName("eth0") {
		t.Fatalf("the session's interface would be %s", config.PPPIfName("eth0"))
	}

	// The provider's access concentrator: pppoe-server, which starts a
	// pppd for every session. Those pppds read /etc/ppp like any other,
	// and that is not the test's to write to. "ip netns exec" puts what
	// is in /etc/netns/<namespace>/ in the place of the same names in
	// /etc for the programs it starts, so the provider's side gets a
	// /etc/ppp of its own and the router's pppd the real one.
	etc := filepath.Join("/etc/netns", nsISP)
	if err := os.MkdirAll(filepath.Join(etc, "ppp"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() {
		os.RemoveAll(etc)
		os.Remove("/etc/netns") // if the test made it, and nobody else uses it
	}()
	secrets := fmt.Sprintf("%q * %q *\n", user, secret)
	for name, content := range map[string]string{
		"options": "# the provider's side of the PPPoE test\n",
		// The provider asks for a password (CHAP), names a DNS server,
		// and notices a dead session quickly. It says everything it does
		// (debug), for the log of a run that failed.
		"pppoe-server-options": "require-chap\nmtu 1492\nmru 1492\nlcp-echo-interval 2\nlcp-echo-failure 3\nms-dns " + ispIP + "\nnodefaultroute\ndebug\n",
		"chap-secrets":         secrets,
		"pap-secrets":          secrets,
	} {
		if err := os.WriteFile(filepath.Join(etc, "ppp", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stopServer := func() {
		for _, pid := range procsOf(nsPids(nsISP), "pppoe-server", "pppd", "pppoe") {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	runs := 0
	startServer := func() {
		// -F: stay in the foreground. Sessions get addresses from -R on,
		// the server's end of each is -L.
		runs++
		r.start("pppoe-server", fmt.Sprintf("pppoe-server-%d.log", runs), "ip", "netns", "exec", nsISP,
			"pppoe-server", "-F", "-I", "isp0", "-C", "pt-ac", "-L", serverIP, "-R", firstClient, "-N", "8", "-O", "/etc/ppp/pppoe-server-options")
	}
	defer stopServer()
	// Whatever happens, the port ends as it began.
	defer r.port(t, func(e *config.EthPort) { e.PPPoE = config.PPPoE{} })
	conf := filepath.Join(r.run, "pppd-"+session+".conf")
	defer r.explain(t, r.logMark(), []string{"pppoe-server-1.log", "pppoe-server-2.log"}, append(append(append([]string{}, showRouter...), showISP...),
		"ls -l "+r.run, "grep -v '^password' "+conf, "ip netns exec "+nsRtr+" nft list chain inet porter forward",
		"ip netns exec "+nsRtr+" nft list chain inet porter srcnat", showProcs(nsISP), showProcs(nsRtr),
		"ls -l /dev/ppp /etc/ppp "+etc+"/ppp")...)

	login := func(password string) {
		r.port(t, func(e *config.EthPort) {
			e.PPPoE = config.PPPoE{Enabled: true, Username: user, Password: password}
		})
	}
	phase := func() string {
		w, _ := r.source(t, "eth:eth0")
		return w.PPPoE
	}

	// Nobody there yet: the search for a provider goes unanswered, and
	// the router says so (pppd gives up a search after 35 seconds).
	login(secret)
	eventually(t, "the source signing in, on the session's interface", 20*time.Second, func() bool {
		w, _ := r.source(t, "eth:eth0")
		return w.If == session && w.Kind == "eth" && w.PPPoE != "" && w.Internet == "connecting"
	})
	eventually(t, "the router saying that the provider does not answer", 90*time.Second, func() bool { return phase() == "no_answer" })
	if st := r.status(t); st.Internet == "online" {
		t.Errorf("online without a provider: %+v", st)
	}

	// The sign-in is in a file only root can read, and not on pppd's
	// command line.
	if fi, err := os.Stat(conf); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("pppd's options file: %v %v", fi, err)
	} else if b, _ := os.ReadFile(conf); !strings.Contains(string(b), "e2e secret") {
		t.Errorf("pppd's options file does not hold the sign-in")
	}
	var pppds int
	for _, pid := range procsOf(nsPids(nsRtr), "pppd") {
		pppds++
		if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); bytes.Contains(b, []byte("e2e secret")) || bytes.Contains(b, []byte(user)) {
			t.Errorf("pppd was started with the sign-in on its command line: %q", b)
		}
	}
	if pppds != 1 {
		t.Errorf("%d pppd processes run for the router, want 1", pppds)
	}

	// The provider is there, and the password is wrong: told apart from
	// everything else that can go wrong.
	login("not the password")
	startServer()
	eventually(t, "the router saying that the user name or password is wrong", 120*time.Second, func() bool { return phase() == "wrong_login" })
	if st := r.status(t); st.Internet == "online" {
		t.Errorf("online with the wrong password: %+v", st)
	}

	// The right one: the session comes up, and it is the session's
	// interface that reaches the internet.
	login(secret)
	var up uplinkState
	eventually(t, "the session up and online", 120*time.Second, func() bool {
		var st status
		up, st = r.source(t, "eth:eth0")
		return up.If == session && up.PPPoE == "up" && strings.HasPrefix(up.IPv4, "100.67.0.") && up.Internet == "online" && st.Active == session && st.Internet == "online"
	})
	sessionIP := strings.Split(up.IPv4, "/")[0]
	eventually(t, "a device reaching the internet through the session", 30*time.Second, func() bool {
		return reaches(nsC1, siteA) == sessionIP
	})
	// Names resolve through the server the provider named with the
	// session.
	if a, _, err := lookup(nsC1, "10.83.0.1", "direct.test"); err != nil || a != siteA {
		t.Errorf("lookup through the session: %q %v", a, err)
	}
	// The default route has the source's own metric (the first wired
	// port's), and the route the router goes by names the session.
	if routes := output("ip -n %s route show default", nsRtr); !strings.Contains(routes, "default dev "+session) || !strings.Contains(routes, "metric 100") {
		t.Errorf("default route of the session:\n%s", routes)
	}
	if routes := output("ip -n %s route show table 105", nsRtr); !strings.Contains(routes, "default dev "+session) {
		t.Errorf("the table that names the source in use:\n%s", routes)
	}
	// A packet has eight bytes less room in the session, and connections
	// are told so in both directions.
	if link := output("ip -n %s link show %s", nsRtr, session); !strings.Contains(link, "mtu 1492") {
		t.Errorf("the session's interface:\n%s", link)
	}
	if rules := output("ip netns exec %s nft list chain inet porter forward", nsRtr); !strings.Contains(rules, `iifname "`+session+`"`) || !strings.Contains(rules, "1452") || !strings.Contains(rules, "rt mtu") {
		t.Errorf("no segment size limit for the session in the forward chain:\n%s", rules)
	}
	if _, err := httpGet(nsC1, "http://"+siteA+"/big"); err != nil {
		t.Errorf("a download through the session: %v", err)
	}
	// The port under the session is not itself a source, nor a port to
	// set up.
	if st := r.status(t); len(st.WAN) != 1 {
		t.Errorf("the sources with the session up: %+v", st.WAN)
	}
	if r.v6 {
		// IPv6 on the session: its interface has a link-local address
		// from pppd, and dhcpcd asks on it for the rest. (This provider
		// offers nothing more.) dhcpcd shows the interface it works on
		// as its command line.
		eventually(t, "dhcpcd started for IPv6 on the session", 30*time.Second, func() bool {
			for _, pid := range procsOf(nsPids(nsRtr), "dhcpcd") {
				if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); bytes.Contains(b, []byte(session)) {
					return true
				}
			}
			return false
		})
		if b, _ := os.ReadFile(filepath.Join(r.run, "dhcpcd-"+session+".conf")); !strings.Contains(string(b), "ipv6only\n") {
			t.Errorf("dhcpcd on the session is not left IPv6 alone:\n%s", b)
		}
	}

	// The provider's equipment restarts without a word. The router
	// notices that the session is dead, and makes a new one by itself.
	stopServer()
	eventually(t, "the router noticing that the session is gone", 90*time.Second, func() bool {
		w, st := r.source(t, "eth:eth0")
		return w.PPPoE != "up" && w.IPv4 == "" && st.Internet != "online"
	})
	startServer()
	eventually(t, "the session back by itself, and online", 150*time.Second, func() bool {
		w, st := r.source(t, "eth:eth0")
		return w.If == session && w.PPPoE == "up" && strings.HasPrefix(w.IPv4, "100.67.0.") && st.Active == session && st.Internet == "online"
	})
	eventually(t, "a device reaching the internet through the new session", 30*time.Second, func() bool {
		return strings.HasPrefix(reaches(nsC1, siteA), "100.67.0.")
	})

	// Back to a plain port: pppd, its file and the session go.
	r.port(t, func(e *config.EthPort) { e.PPPoE = config.PPPoE{} })
	eventually(t, "the plain port online again", 90*time.Second, func() bool {
		w, st := r.source(t, "eth:eth0")
		return w.If == "eth0" && w.PPPoE == "" && strings.HasPrefix(w.IPv4, "100.64.0.") && st.Active == "eth0" && st.Internet == "online"
	})
	if _, err := os.Stat(conf); err == nil {
		t.Errorf("the sign-in is still in %s", conf)
	}
	if left := procsOf(nsPids(nsRtr), "pppd"); len(left) != 0 {
		t.Errorf("pppd still runs for the router: %v", left)
	}
	if exec.Command("ip", "-n", nsRtr, "link", "show", session).Run() == nil {
		t.Errorf("%s left behind", session)
	}
	eventually(t, "a device reaching the internet through the plain port", 30*time.Second, func() bool {
		return strings.HasPrefix(reaches(nsC1, siteA), "100.64.0.")
	})
}

// --- prefix delegation -----------------------------------------------------------

// globalIn returns the address a namespace's interface has inside prefix,
// and whether the kernel has it marked as not to be used for new
// connections.
func globalIn(ns, ifname string, prefix netip.Prefix) (addr netip.Addr, deprecated, ok bool) {
	for _, line := range strings.Split(output("ip -n %s -6 -o addr show dev %s scope global", ns, ifname), "\n") {
		f := strings.Fields(line)
		for i := range f {
			if f[i] != "inet6" || i+1 >= len(f) {
				continue
			}
			if p, err := netip.ParsePrefix(f[i+1]); err == nil && prefix.Contains(p.Addr()) && !strings.Contains(line, "tentative") {
				return p.Addr(), strings.Contains(line, "deprecated"), true
			}
		}
	}
	return netip.Addr{}, false, false
}

// refused reports whether a connection from inside a namespace to an
// address and port fails.
func refused(ns string, addr netip.Addr, port int) bool {
	var conn net.Conn
	var err error
	inNS(ns, func() {
		conn, err = net.DialTimeout("tcp6", net.JoinHostPort(addr.String(), strconv.Itoa(port)), 3*time.Second)
	})
	if err == nil {
		conn.Close()
	}
	return err != nil
}

func prefixDelegation(t *testing.T, r *rig) {
	if !r.v6 {
		t.Skip("this kernel has IPv6 disabled")
	}
	delegated := netip.MustParsePrefix("2001:db8:1000::/60")
	const second = "second" // the id of a second network
	const fixed6 = "2001:db8:100::200"
	routerLog := r.logMark()

	// The provider's DHCPv6 server: an address for the router and a /60
	// for its networks, both short-lived so that the router renews them
	// while the test watches.
	srv := startPDServer(t, nsISP, "isp0", pdLease{Addr: netip.MustParseAddr("2001:db8:100::d6"), Prefix: delegated,
		T1: 20, T2: 32, Preferred: 120, Valid: 180}, filepath.Join(r.dir, "dhcp6-server.log"))
	closed := false
	stopServer := func() {
		if !closed {
			closed = true
			srv.Close()
		}
	}
	defer stopServer()
	rejoin := func() {
		sh(t, "ip -n %s link set pt-c2if down && sleep 1 && ip -n %s link set pt-c2if up", nsC2, nsC2)
	}
	// Whatever happens, the router ends with one network, its port asking
	// the network for its addresses, and no server to delegate anything.
	defer func() {
		stopServer()
		if !t.Failed() {
			return // the test has put things back itself
		}
		r.edit(t, func(cfg *config.Config) {
			cfg.Networks = cfg.Networks[:1]
			for i := range cfg.Ethernet {
				cfg.Ethernet[i].Network = config.MainNetwork
				cfg.Ethernet[i].IP = config.IPSetup{}
			}
		})
		rejoin()
	}()
	defer r.explain(t, routerLog, []string{"dhcp6-server.log", "isp-dnsmasq.log", "c1-dhcpcd.log", "c2-dhcpcd.log"}, append(append(append([]string{}, showRouter...), showISP...),
		"cat "+filepath.Join(r.run, "dhcpcd-eth0.conf"), "cat "+filepath.Join(r.run, "dnsmasq.conf"),
		"ip netns exec "+nsRtr+" nft list chain inet porter srcnat", "ip netns exec "+nsRtr+" nft list chain inet porter forward",
		"ip netns exec "+nsRtr+" nft list set inet porter lan_nets6", "ip netns exec "+nsRtr+" ss -lun", "ip netns exec "+nsRtr+" ss -ltn",
		"ip -n "+nsC1+" -6 addr show", "ip -n "+nsC1+" -6 route show", "ip -n "+nsC2+" -6 addr show", "ip -n "+nsC2+" -6 route show",
		"ip netns exec "+nsISP+" ss -lun")...)

	// Before: no prefix, private addresses only, IPv6 masqueraded.
	if w, st := r.source(t, "eth:eth0"); w.Prefix != "" || len(st.LAN.Prefixes) != 0 {
		t.Fatalf("a delegated prefix before there is a server to delegate one: %+v %+v", w, st.LAN)
	}
	// The router has been asking since it started, more and more rarely.
	// Its cable is replugged, as when the provider's equipment has been
	// restarted: it asks again at once.
	sh(t, "ip -n %s link set eth0 down && sleep 1 && ip -n %s link set eth0 up", nsRtr, nsRtr)

	var mainNet netip.Prefix
	eventually(t, "the prefix delegated to the wired source, and a /64 of it given to the main network", 90*time.Second, func() bool {
		w, st := r.source(t, "eth:eth0")
		p, err := netip.ParsePrefix(st.LAN.Prefixes[config.MainNetwork])
		if w.Prefix != delegated.String() || err != nil {
			return false
		}
		mainNet = p
		return w.HasV6 && st.Active == "eth0"
	})
	if mainNet.Bits() != 64 || !delegated.Contains(mainNet.Addr()) {
		t.Fatalf("the main network was given %s of %s", mainNet, delegated)
	}
	// (A router that still holds a lease from an earlier run of this test
	// asks to have that one confirmed instead of for a new one.)
	extended := srv.Renewed.Load()
	if srv.Bound.Load()+extended == 0 {
		t.Errorf("the server did not see the router commit to its lease")
	}
	routerAddr := mainNet.Addr().Next() // ::1
	// The router's address in it is on the network's bridge, next to the
	// private one.
	if addrs := output("ip -n %s -6 addr show dev br-lan scope global", nsRtr); !strings.Contains(addrs, routerAddr.String()+"/64") || !strings.Contains(addrs, "inet6 fd") {
		t.Errorf("the main network's bridge:\n%s", addrs)
	}
	// What nobody uses of the prefix goes nowhere, rather than back to
	// the provider (who would send it here again).
	if routes := output("ip -n %s -6 route show type unreachable table all", nsRtr); !strings.Contains(routes, delegated.String()) {
		t.Errorf("no unreachable route for %s:\n%s", delegated, routes)
	}
	unused := netip.MustParseAddr("2001:db8:1000:e:1::1")
	if mainNet.Contains(unused) {
		unused = netip.MustParseAddr("2001:db8:1000:f:1::1")
	}
	if got := output("ip -n %s -6 route get %s", nsRtr, unused); !strings.Contains(got, "unreachable") {
		t.Errorf("%s, in a part of the prefix that no network has, is routed: %s", unused, got)
	}

	// A device gets an address out of the /64 and keeps its private one.
	var dev1 netip.Addr
	eventually(t, "a device with an address from the delegated prefix", 60*time.Second, func() bool {
		var ok bool
		dev1, _, ok = globalIn(nsC1, "pt-c1if", mainNet)
		return ok
	})
	if _, _, ok := globalIn(nsC1, "pt-c1if", netip.MustParsePrefix("fc00::/7")); !ok {
		t.Errorf("the device lost its private address:\n%s", output("ip -n %s -6 addr show dev pt-c1if", nsC1))
	}
	// It reaches an IPv6 site from that address: not masqueraded.
	eventually(t, "the device reaching an IPv6 site from its own address", 60*time.Second, func() bool {
		return reaches(nsC1, site6) == dev1.String()
	})
	if rules := output("ip netns exec %s nft list chain inet porter srcnat", nsRtr); !strings.Contains(rules, `oifname "eth0" ip6 saddr `+delegated.String()+" accept") {
		t.Errorf("no rule sparing the delegated prefix from masquerading:\n%s", rules)
	}
	// IPv4 is what it was.
	if got := reaches(nsC1, siteA); !strings.HasPrefix(got, "100.64.0.") {
		t.Errorf("IPv4 with a delegated prefix in use: %q", got)
	}

	// Nothing gets in from the internet: not to the device, whose address
	// the provider now routes here (the site's answers above came that
	// way), and not to the router at its new address.
	ln := listen(t, nsC1, "["+dev1.String()+"]:8088")
	defer ln.Close()
	if !refused(nsISP, dev1, 8088) {
		t.Errorf("a connection from the internet to a device's delegated address got through")
	}
	for _, port := range []int{53, 22, 7678} {
		if !refused(nsISP, routerAddr, port) {
			t.Errorf("port %d of the router is open to the internet at its delegated address", port)
		}
	}
	// From another device on the network the same listener answers: it
	// is the direction that is refused, not the address.
	eventually(t, "the second device with an address from the delegated prefix", 60*time.Second, func() bool {
		_, _, ok := globalIn(nsC2, "pt-c2if", mainNet)
		return ok
	})
	if refused(nsC2, dev1, 8088) {
		t.Errorf("a device cannot reach its neighbour at its delegated address")
	}

	// Lookups: the router answers at its new address too, and a lookup
	// sent past the router to a server of the device's choosing is still
	// answered by the router.
	if a, _, err := lookup(nsC1, routerAddr.String(), "direct.test"); err != nil || a != siteA {
		t.Errorf("lookup at the router's delegated address: %q %v", a, err)
	}
	if a, _, err := lookup(nsC1, site6, "direct.test"); err != nil || a != siteA {
		t.Errorf("lookup sent to another server over IPv6, with a delegated prefix in use: %q %v", a, err)
	}
	// Devices are still told the private address as their server.
	if conf, _ := os.ReadFile(filepath.Join(r.run, "dnsmasq.conf")); strings.Contains(string(conf), "2001:db8:1000") || !strings.Contains(string(conf), "option6:dns-server,[fd") {
		t.Errorf("what devices are told has changed with the prefix:\n%s", conf)
	}

	// A second network gets a /64 of its own.
	r.edit(t, func(cfg *config.Config) {
		cfg.Networks = append(cfg.Networks, config.Network{ID: second, Name: "Second"})
		for i := range cfg.Ethernet {
			if cfg.Ethernet[i].Name == "eth2" {
				cfg.Ethernet[i].Network = second
			}
		}
	})
	rejoin()
	var secondNet netip.Prefix
	eventually(t, "a /64 of the prefix for the second network", 30*time.Second, func() bool {
		st := r.status(t)
		p, err := netip.ParsePrefix(st.LAN.Prefixes[second])
		secondNet = p
		return err == nil && st.LAN.Prefixes[config.MainNetwork] == mainNet.String()
	})
	if secondNet.Bits() != 64 || !delegated.Contains(secondNet.Addr()) || secondNet == mainNet {
		t.Fatalf("the second network was given %s; the main one has %s of %s", secondNet, mainNet, delegated)
	}
	var dev2 netip.Addr
	eventually(t, "a device on the second network with an address from its /64", 90*time.Second, func() bool {
		var ok bool
		dev2, _, ok = globalIn(nsC2, "pt-c2if", secondNet)
		return ok
	})
	eventually(t, "that device reaching an IPv6 site from its own address", 60*time.Second, func() bool {
		return reaches(nsC2, site6) == dev2.String()
	})
	if got := reaches(nsC1, site6); got != dev1.String() {
		t.Errorf("the first device after a second network was added: %q, want %s", got, dev1)
	}

	// The lease is renewed and the prefix stays what it is.
	eventually(t, "the router renewing its lease", 90*time.Second, func() bool { return srv.Renewed.Load() > extended })
	if w, st := r.source(t, "eth:eth0"); w.Prefix != delegated.String() || st.LAN.Prefixes[config.MainNetwork] != mainNet.String() || st.LAN.Prefixes[second] != secondNet.String() {
		t.Errorf("after a renewal: %+v %+v", w, st.LAN)
	}

	// The prefix goes: the port is given a fixed IPv6 address, so the
	// router stops asking the network for anything IPv6. The networks
	// lose their /64s, devices are told to stop using their addresses
	// (which they keep for connections under way), and IPv6 leaves
	// masqueraded again.
	stopServer()
	r.port(t, func(e *config.EthPort) {
		e.IP.V6 = config.IPFamily{Mode: config.IPStatic, Address: fixed6 + "/64", Gateway: ispIP6}
	})
	eventually(t, "the prefix gone from the source and from the networks", 60*time.Second, func() bool {
		w, st := r.source(t, "eth:eth0")
		return w.Prefix == "" && len(st.LAN.Prefixes) == 0
	})
	if addrs := output("ip -n %s -6 addr show dev br-lan scope global", nsRtr); strings.Contains(addrs, routerAddr.String()) || !strings.Contains(addrs, "inet6 fd") {
		t.Errorf("the main network's bridge after the prefix went:\n%s", addrs)
	}
	eventually(t, "the device's address from the prefix marked as not to be used", 90*time.Second, func() bool {
		_, deprecated, ok := globalIn(nsC1, "pt-c1if", mainNet)
		return ok && deprecated
	})
	eventually(t, "the device reaching an IPv6 site masqueraded again", 60*time.Second, func() bool {
		return reaches(nsC1, site6) == fixed6
	})
	if rules := output("ip netns exec %s nft list chain inet porter srcnat", nsRtr); strings.Contains(rules, "ip6 saddr") {
		t.Errorf("a rule sparing a prefix from masquerading is left:\n%s", rules)
	}

	// Back to what the network hands out, with nobody delegating.
	r.edit(t, func(cfg *config.Config) {
		cfg.Networks = cfg.Networks[:1]
		for i := range cfg.Ethernet {
			cfg.Ethernet[i].Network = config.MainNetwork
			cfg.Ethernet[i].IP = config.IPSetup{}
		}
	})
	rejoin()
	eventually(t, "IPv6 by router advertisement and masquerading, as before", 90*time.Second, func() bool {
		got := reaches(nsC1, site6)
		return strings.HasPrefix(got, ispNet6) && got != fixed6
	})
	eventually(t, "the second device back in the main network", 60*time.Second, func() bool {
		var devs []device
		r.c.Call("devices.list", nil, &devs)
		for _, d := range devs {
			if d.MAC == r.mac2 {
				return strings.HasPrefix(d.IPv4, "10.83.0.") && d.Network == config.MainNetwork
			}
		}
		return false
	})
	if w, st := r.source(t, "eth:eth0"); w.Prefix != "" || len(st.LAN.Prefixes) != 0 {
		t.Errorf("a prefix at the end: %+v %+v", w, st.LAN)
	}
}
