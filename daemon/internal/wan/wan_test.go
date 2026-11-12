package wan

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/netx"
)

func TestDhcpcdConf(t *testing.T) {
	v4 := config.IPFamily{Mode: config.IPStatic, Address: "192.168.1.50/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}}
	v6 := config.IPFamily{Mode: config.IPStatic, Address: "2001:db8::50/64", Gateway: "fe80::1"}
	has := func(conf, line string) bool {
		for _, l := range strings.Split(conf, "\n") {
			if l == line {
				return true
			}
		}
		return false
	}
	auto := dhcpcdConf(Uplink{If: "eth0", Metric: 100}, "/hook")
	if has(auto, "ipv4only") || has(auto, "ipv6only") || !has(auto, "ipv6rs") || !has(auto, "metric 100") {
		t.Errorf("automatic uplink:\n%s", auto)
	}
	// A fixed IPv4 address leaves only IPv6 to dhcpcd, and the other way
	// round.
	c := dhcpcdConf(Uplink{If: "eth0", Metric: 100, IP: config.IPSetup{V4: v4}}, "/hook")
	if !has(c, "ipv6only") || has(c, "ipv4only") || !has(c, "ipv6rs") {
		t.Errorf("fixed IPv4:\n%s", c)
	}
	c = dhcpcdConf(Uplink{If: "eth0", Metric: 100, IP: config.IPSetup{V6: v6}}, "/hook")
	if !has(c, "ipv4only") || has(c, "ipv6only") || has(c, "ipv6rs") || has(c, "ia_na") {
		t.Errorf("fixed IPv6:\n%s", c)
	}

	// A prefix for the router's networks is asked for where IPv6 is
	// automatic and the uplink is told to, by a request of its own that
	// leaves the sharing out to the router.
	if has(auto, "ia_pd 1 -") || !has(auto, "ia_na") {
		t.Errorf("no prefix was to be asked for:\n%s", auto)
	}
	c = dhcpcdConf(Uplink{If: "eth0", Metric: 100, PD: true}, "/hook")
	if !has(c, "ia_pd 1 -") || !has(c, "ia_na") || strings.Index(c, "ia_na") > strings.Index(c, "ia_pd") {
		t.Errorf("asking for a prefix:\n%s", c)
	}
	if c = dhcpcdConf(Uplink{If: "eth0", Metric: 100, PD: true, IP: config.IPSetup{V6: v6}}, "/hook"); strings.Contains(c, "ia_pd") {
		t.Errorf("fixed IPv6, and a prefix is asked for:\n%s", c)
	}
	// On a PPPoE session dhcpcd has IPv6 only: pppd has seen to IPv4.
	c = dhcpcdConf(Uplink{If: "ppp-eth0", Link: "eth0", Metric: 100, PD: true, PPPoE: config.PPPoE{Enabled: true, Username: "u", Password: "p"}}, "/hook")
	if !has(c, "ipv6only") || !has(c, "ipv6rs") || !has(c, "ia_pd 1 -") || !has(c, "metric 100") {
		t.Errorf("on a PPPoE session:\n%s", c)
	}

	got := statics(Uplink{If: "eth0", IP: config.IPSetup{V4: v4, V6: v6}})
	if len(got) != 2 || got[0].Addr.String() != "192.168.1.50/24" || got[0].Gw.String() != "192.168.1.1" || got[1].Gw.String() != "fe80::1" {
		t.Errorf("fixed addresses: %+v", got)
	}
	if got := statics(Uplink{If: "eth0"}); len(got) != 0 {
		t.Errorf("an automatic uplink has fixed addresses: %+v", got)
	}
}

func TestActiveAndPreferred(t *testing.T) {
	up := func(ifname string, metric, rank int, internet string) State {
		return State{If: ifname, Metric: metric, Rank: rank, Carrier: true, IPv4: "192.0.2.2/24", Internet: internet}
	}
	name := func(s *State) string {
		if s == nil {
			return ""
		}
		return s.If
	}
	for _, tc := range []struct {
		what              string
		states            []State // in the configured order
		active, preferred string
	}{
		{"all well: the first in the order", []State{up("eth0", 100, 0, StOnline), up("wlan0", 200, 1, StOnline)}, "eth0", "eth0"},
		{"Wi-Fi ranked first", []State{up("wlan0", 200, 0, StOnline), up("eth0", 100, 1, StOnline)}, "wlan0", "eth0"},
		{"first has no internet", []State{up("eth0", 100, 0, StLimited), up("usb0", 300, 1, StOnline)}, "usb0", "eth0"},
		{"a sign-in page is not stepped past", []State{up("wlan0", 200, 0, StCaptive), up("usb0", 300, 1, StOnline)}, "wlan0", "wlan0"},
		{"nothing works: the first with an address", []State{up("eth0", 100, 0, StLimited), up("usb0", 300, 1, StLimited)}, "eth0", "eth0"},
		{"no link on the first", []State{{If: "eth0", Metric: 100, Internet: StDown}, up("wlan0", 200, 1, StOnline)}, "wlan0", "wlan0"},
		{"none", nil, "", ""},
	} {
		if got := name(Active(tc.states)); got != tc.active {
			t.Errorf("%s: active %q, want %q", tc.what, got, tc.active)
		}
		if got := name(Preferred(tc.states)); got != tc.preferred {
			t.Errorf("%s: preferred by the routing table %q, want %q", tc.what, got, tc.preferred)
		}
	}
}

func TestSameUplink(t *testing.T) {
	bare := Uplink{If: "wlan0", Kind: "wifi", Key: "wifi", Metric: 200}
	spelled := bare
	spelled.IP = config.IPSetup{V4: config.IPFamily{Mode: config.IPAuto, DNS: []string{}}, V6: config.IPFamily{Mode: config.IPAuto, DNS: []string{}}}
	if !sameUplink(bare, spelled) {
		t.Error("automatic addresses, written two ways, taken for a change")
	}
	fixed := bare
	fixed.IP.V4 = config.IPFamily{Mode: config.IPStatic, Address: "192.0.2.5/24", Gateway: "192.0.2.1"}
	other := fixed
	other.IP.V4.Gateway = "192.0.2.254"
	if sameUplink(bare, fixed) || sameUplink(fixed, other) || !sameUplink(fixed, fixed) {
		t.Error("fixed addresses compared wrongly")
	}
	metric := bare
	metric.Metric = 300
	if sameUplink(bare, metric) {
		t.Error("another metric taken for no change")
	}
}

func TestPPPDConf(t *testing.T) {
	u := Uplink{
		If: "ppp-eth0", Link: "v35-eth0", Kind: "eth", Key: "eth:eth0", Metric: 101,
		PPPoE: config.PPPoE{Enabled: true, Username: `user#1@provider.example`, Password: `pa"ss\word with spaces #x'`},
	}
	hooks := map[string]string{"ip-up": "/run/porter/porter-ppp-ip-up", "ip-down": "/run/porter/porter-ppp-ip-down",
		"ipv6-up": "/run/porter/porter-ppp-ipv6-up", "ipv6-down": "/run/porter/porter-ppp-ipv6-down"}
	conf := pppdConf(u, hooks, []string{"PORTER_HOOK_SOCKET=/run/porter/hook.sock"})
	lines := strings.Split(strings.TrimSuffix(conf, "\n"), "\n")
	at := func(line string) int {
		for i, l := range lines {
			if l == line {
				return i
			}
		}
		return -1
	}
	for _, want := range []string{
		"plugin pppoe.so", "nic-v35-eth0", "ifname ppp-eth0",
		`user "user#1@provider.example"`, `password "pa\"ss\\word with spaces #x'"`,
		"noauth", "noipdefault", "defaultroute", "defaultroute-metric 101", "usepeerdns", "+ipv6",
		"persist", "maxfail 0", "holdoff 10", "lcp-echo-interval 10", "lcp-echo-failure 3", "nodetach",
		`ip-up-script "/run/porter/porter-ppp-ip-up"`, `ip-down-script "/run/porter/porter-ppp-ip-down"`,
		`ipv6-up-script "/run/porter/porter-ppp-ipv6-up"`, `ipv6-down-script "/run/porter/porter-ppp-ipv6-down"`,
		`set "PORTER_HOOK_SOCKET=/run/porter/hook.sock"`,
	} {
		if at(want) < 0 {
			t.Errorf("pppd options lack %q:\n%s", want, conf)
		}
	}
	// The Ethernet interface is only understood once the plugin that
	// takes it is loaded.
	if at("plugin pppoe.so") > at("nic-v35-eth0") {
		t.Errorf("the interface is named before the plugin is loaded:\n%s", conf)
	}
	// Every line is one option: nothing typed into a field starts a line
	// of its own.
	for _, l := range lines {
		if l == "" || strings.ContainsAny(l, "\r\x00") {
			t.Errorf("odd line %q", l)
		}
	}

	// What pppd reads back out of a quoted word is what went in. This is
	// pppd's own rule (getword in its options.c): inside double quotes a
	// backslash makes the next character stand for itself, unless it is
	// one of a few letters or a digit, which a backslash is never put
	// before here.
	unquote := func(q string) string {
		if len(q) < 2 || q[0] != '"' || q[len(q)-1] != '"' {
			t.Fatalf("%q is not quoted", q)
		}
		var out []byte
		body := q[1 : len(q)-1]
		for i := 0; i < len(body); i++ {
			switch c := body[i]; {
			case c == '\\':
				i++
				if i == len(body) || strings.IndexByte(`abfnrstx01234567`, body[i]) >= 0 {
					t.Fatalf("%q has an escape pppd would read as something else", q)
				}
				out = append(out, body[i])
			case c == '"':
				t.Fatalf("%q ends early", q)
			default:
				out = append(out, c)
			}
		}
		return string(out)
	}
	for _, word := range []string{"plain", `back\slash`, `\n`, `\\`, `"quoted"`, `ends with \`, "tab\there", "#comment", "sp ace", "ünï©ödé", `a\x41b`, `'single'`, `\"`} {
		if got := unquote(pppQuote(word)); got != word {
			t.Errorf("%q written as %s, which reads back as %q", word, pppQuote(word), got)
		}
	}
}

func TestPPPPhase(t *testing.T) {
	// Lines as pppd 2.5 prints them, with what they make of the state.
	for _, tc := range []struct{ prev, line, want string }{
		{PPPSigningIn, "Plugin pppoe.so loaded.", PPPSigningIn},
		{PPPSigningIn, "PPP session is 12", PPPSigningIn},
		{PPPSigningIn, "Connected to 02:00:00:00:00:01 via interface eth0", PPPSigningIn},
		{PPPSigningIn, "CHAP authentication failed", PPPWrongLogin},
		{PPPSigningIn, "CHAP authentication failed: Access denied", PPPWrongLogin},
		{PPPSigningIn, "PAP authentication failed", PPPWrongLogin},
		{PPPSigningIn, "MS-CHAP authentication failed: E=691 Authentication failure", PPPWrongLogin},
		{PPPSigningIn, "EAP: peer reports authentication failure", PPPWrongLogin},
		{PPPSigningIn, "LCP terminated by peer (Authentication failed)", PPPWrongLogin},
		{PPPSigningIn, "Timeout waiting for PADO packets", PPPNoAnswer},
		{PPPSigningIn, "Timeout waiting for PADS packets", PPPNoAnswer},
		{PPPSigningIn, "Unable to complete PPPoE Discovery phase 1", PPPNoAnswer},
		{PPPSigningIn, "LCP: timeout sending Config-Requests", PPPNoAnswer},
		// A failure stays what it is through the next attempt's chatter,
		// until that attempt has an outcome of its own.
		{PPPWrongLogin, "Connection terminated.", PPPWrongLogin},
		{PPPWrongLogin, "PPP session is 13", PPPWrongLogin},
		{PPPWrongLogin, "Modem hangup", PPPWrongLogin},
		{PPPWrongLogin, "Timeout waiting for PADO packets", PPPNoAnswer},
		{PPPNoAnswer, "CHAP authentication failed", PPPWrongLogin},
		{PPPWrongLogin, "CHAP authentication succeeded", PPPSigningIn},
		{PPPWrongLogin, "PAP authentication succeeded", PPPSigningIn},
		{PPPUp, "No response to 3 echo-requests", PPPUp},
		{PPPUp, "LCP terminated by peer (Session timeout)", PPPUp},
	} {
		if got := pppPhase(tc.prev, tc.line); got != tc.want {
			t.Errorf("%s, then %q: %s, want %s", tc.prev, tc.line, got, tc.want)
		}
	}
	// What is there to see settles it.
	for _, tc := range []struct {
		phase            string
		carrier, hasAddr bool
		want             string
	}{
		{"", true, false, PPPSigningIn},
		{PPPSigningIn, true, true, PPPUp},
		{PPPWrongLogin, true, true, PPPUp},          // the password has been put right
		{PPPUp, true, false, PPPSigningIn},          // the session ended
		{PPPWrongLogin, true, false, PPPWrongLogin}, // still wrong
		{PPPNoAnswer, true, false, PPPNoAnswer},     // still silent
		{PPPNoAnswer, false, false, PPPSigningIn},   // no cable: that is said elsewhere
		{PPPWrongLogin, false, false, PPPSigningIn},
	} {
		if got := pppSettle(tc.phase, tc.carrier, tc.hasAddr); got != tc.want {
			t.Errorf("%q with carrier %v and address %v: %s, want %s", tc.phase, tc.carrier, tc.hasAddr, got, tc.want)
		}
	}
}

func TestDelegated(t *testing.T) {
	// The hook's environment for a lease with an address and a prefix,
	// as dhcpcd 10 writes it (see TestDelegatedFromDhcpcd).
	lease := map[string]string{
		"interface": "eth0", "reason": "BOUND6",
		"new_dhcp6_ia_na1_iaid": "00000001", "new_dhcp6_ia_na1_ia_addr1": "2001:db8:100::d6",
		"new_dhcp6_ia_pd1_iaid": "00000001", "new_dhcp6_ia_pd1_t1": "1800", "new_dhcp6_ia_pd1_t2": "2880",
		"new_dhcp6_ia_pd1_prefix1": "2001:db8:1000::", "new_dhcp6_ia_pd1_prefix1_length": "60",
		"new_dhcp6_ia_pd1_prefix1_pltime": "3600", "new_dhcp6_ia_pd1_prefix1_vltime": "7200",
		"new_dhcp6_name_servers": "2001:db8:100::1",
	}
	with := func(changes map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range lease {
			out[k] = v
		}
		for k, v := range changes {
			if v == "" {
				delete(out, k)
			} else {
				out[k] = v
			}
		}
		return out
	}
	for _, tc := range []struct {
		what   string
		env    map[string]string
		prefix string
		valid  time.Duration
	}{
		{"a /60", lease, "2001:db8:1000::/60", 2 * time.Hour},
		{"a /56 with host bits set", with(map[string]string{"new_dhcp6_ia_pd1_prefix1": "2001:db8:1000:ff00::1", "new_dhcp6_ia_pd1_prefix1_length": "56"}), "2001:db8:1000:ff00::/56", 2 * time.Hour},
		{"a /64", with(map[string]string{"new_dhcp6_ia_pd1_prefix1_length": "64"}), "2001:db8:1000::/64", 2 * time.Hour},
		{"for ever", with(map[string]string{"new_dhcp6_ia_pd1_prefix1_vltime": "4294967295"}), "2001:db8:1000::/60", 0},
		{"no prefix in the lease", map[string]string{"interface": "eth0", "reason": "BOUND6", "new_dhcp6_ia_na1_ia_addr1": "2001:db8:100::d6"}, "", 0},
		{"withdrawn", with(map[string]string{"new_dhcp6_ia_pd1_prefix1_pltime": "0", "new_dhcp6_ia_pd1_prefix1_vltime": "0"}), "", 0},
		{"too long to share out", with(map[string]string{"new_dhcp6_ia_pd1_prefix1_length": "72"}), "", 0},
		{"no length", with(map[string]string{"new_dhcp6_ia_pd1_prefix1_length": ""}), "", 0},
		{"not an address", with(map[string]string{"new_dhcp6_ia_pd1_prefix1": "bogus"}), "", 0},
		{"an IPv4 address", with(map[string]string{"new_dhcp6_ia_pd1_prefix1": "192.0.2.0"}), "", 0},
		// The old prefix on its way out and its successor: the one to
		// use for new connections, whichever comes first.
		{"old and new", with(map[string]string{
			"new_dhcp6_ia_pd1_prefix1_pltime": "0", "new_dhcp6_ia_pd1_prefix1_vltime": "600",
			"new_dhcp6_ia_pd1_prefix2": "2001:db8:2000::", "new_dhcp6_ia_pd1_prefix2_length": "56",
			"new_dhcp6_ia_pd1_prefix2_pltime": "3600", "new_dhcp6_ia_pd1_prefix2_vltime": "7200",
		}), "2001:db8:2000::/56", 2 * time.Hour},
		{"deprecated only", with(map[string]string{"new_dhcp6_ia_pd1_prefix1_pltime": "0", "new_dhcp6_ia_pd1_prefix1_vltime": "600"}), "2001:db8:1000::/60", 10 * time.Minute},
		{"two requests answered", with(map[string]string{
			"new_dhcp6_ia_pd2_prefix1": "2001:db8:3000::", "new_dhcp6_ia_pd2_prefix1_length": "56",
			"new_dhcp6_ia_pd2_prefix1_pltime": "3600", "new_dhcp6_ia_pd2_prefix1_vltime": "7200",
		}), "2001:db8:1000::/60", 2 * time.Hour},
		// What the lease used to say is not what it says.
		{"only the old lease has one", map[string]string{"old_dhcp6_ia_pd1_prefix1": "2001:db8:1000::", "old_dhcp6_ia_pd1_prefix1_length": "60", "old_dhcp6_ia_pd1_prefix1_vltime": "7200"}, "", 0},
	} {
		got, valid := delegated(tc.env)
		want := netip.Prefix{}
		if tc.prefix != "" {
			want = netip.MustParsePrefix(tc.prefix)
		}
		if got != want || valid != tc.valid {
			t.Errorf("%s: %v for %v, want %v for %v", tc.what, got, valid, want, tc.valid)
		}
	}
}

// TestDelegatedFromDhcpcd has the dhcpcd that is installed describe a
// DHCPv6 reply the way it describes one to the hook, and reads the
// delegated prefix out of that: a check on the variable names, which are
// dhcpcd's to choose.
func TestDelegatedFromDhcpcd(t *testing.T) {
	for _, tool := range []string{"dhcpcd", "stdbuf"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	opt := func(code uint16, data ...[]byte) []byte {
		body := bytes.Join(data, nil)
		return append(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, code), uint16(len(body))), body...)
	}
	u32 := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	addr := func(s string) []byte { a := netip.MustParseAddr(s).As16(); return a[:] }
	reply := bytes.Join([][]byte{
		{7, 0x12, 0x34, 0x56}, // Reply, transaction id
		opt(1, []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 1}),
		opt(2, []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 2}),
		opt(3, u32(1), u32(1800), u32(2880), opt(5, addr("2001:db8:100::d6"), u32(3600), u32(7200))),
		opt(25, u32(1), u32(1800), u32(2880),
			opt(26, u32(0), u32(600), []byte{56}, addr("2001:db8:900::")),
			opt(26, u32(3600), u32(7200), []byte{60}, addr("2001:db8:1000::"))),
	}, nil)
	// dhcpcd leaves without flushing what it printed unless every line
	// is written as it is made.
	cmd := exec.Command("stdbuf", "-oL", "dhcpcd", "-6", "-U", "-")
	cmd.Stdin = bytes.NewReader(reply)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		t.Skipf("this dhcpcd does not describe a lease given to it: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			// As the hook has them: it is given what is in the lease
			// under "new_".
			env["new_"+k] = v
		}
	}
	got, valid := delegated(env)
	if got != netip.MustParsePrefix("2001:db8:1000::/60") || valid != 2*time.Hour {
		t.Errorf("read %v for %v out of what dhcpcd says:\n%s", got, valid, out)
	}
}

func testManager() *Manager {
	m := &Manager{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), uplinks: map[string]*uplink{}}
	return m
}

func (m *Manager) testUplink(cfg Uplink) *uplink {
	u := &uplink{cfg: cfg, dns6: map[string][]string{}, st: State{If: cfg.If, Kind: cfg.Kind, Key: cfg.Key}}
	m.uplinks[cfg.If] = u
	return u
}

func TestDelegatedPrefixComesAndGoes(t *testing.T) {
	m := testManager()
	// No interface of this name exists: only what the hook says is seen.
	u := m.testUplink(Uplink{If: "pt-none0", Kind: "eth", Key: "eth:pt-none0", PD: true})
	prefix := func() string { return m.States()[0].Prefix }
	lease := func(reason, pd, valid string) map[string]string {
		env := map[string]string{"interface": "pt-none0", "reason": reason}
		if pd != "" {
			env["new_dhcp6_ia_pd1_prefix1"], env["new_dhcp6_ia_pd1_prefix1_length"] = pd, "56"
			env["new_dhcp6_ia_pd1_prefix1_pltime"], env["new_dhcp6_ia_pd1_prefix1_vltime"] = valid, valid
		}
		return env
	}
	if prefix() != "" {
		t.Fatalf("a prefix before any lease: %s", prefix())
	}
	const first, second = "2001:db8:aa00::/56", "2001:db8:bb00::/56"
	for _, step := range []struct {
		what string
		env  map[string]string
		want string
	}{
		{"bound", lease("BOUND6", "2001:db8:aa00::", "3600"), first},
		{"renewed", lease("RENEW6", "2001:db8:aa00::", "3600"), first},
		{"a router advertisement in between", map[string]string{"interface": "pt-none0", "reason": "ROUTERADVERT"}, first},
		{"IPv4 lease lost", map[string]string{"interface": "pt-none0", "reason": "EXPIRE"}, first},
		{"the provider hands out another", lease("REBIND6", "2001:db8:bb00::", "3600"), second},
		{"renewed without a prefix", lease("RENEW6", "", ""), ""},
		{"bound again", lease("BOUND6", "2001:db8:aa00::", "3600"), first},
		{"expired", lease("EXPIRE6", "", ""), ""},
		{"bound again", lease("REBOOT6", "2001:db8:aa00::", "3600"), first},
		{"cable pulled", map[string]string{"interface": "pt-none0", "reason": "NOCARRIER"}, ""},
		{"bound again", lease("BOUND6", "2001:db8:aa00::", "3600"), first},
		{"dhcpcd stopped", map[string]string{"interface": "pt-none0", "reason": "STOP"}, ""},
		{"an event for another interface", map[string]string{"interface": "eth9", "reason": "BOUND6", "new_dhcp6_ia_pd1_prefix1": "2001:db8:cc00::", "new_dhcp6_ia_pd1_prefix1_length": "56", "new_dhcp6_ia_pd1_prefix1_vltime": "60"}, ""},
	} {
		m.HandleHook(step.env)
		if got := prefix(); got != step.want {
			t.Fatalf("%s: prefix %q, want %q", step.what, got, step.want)
		}
	}
	// A lease that runs out without dhcpcd saying so ends all the same.
	m.HandleHook(lease("BOUND6", "2001:db8:aa00::", "3600"))
	if u.pdUntil.IsZero() || time.Until(u.pdUntil) > time.Hour || time.Until(u.pdUntil) < 59*time.Minute {
		t.Errorf("a prefix valid for an hour ends %v from now", time.Until(u.pdUntil))
	}
	m.mu.Lock()
	u.pdUntil = time.Now().Add(-time.Second)
	m.mu.Unlock()
	m.refresh()
	if prefix() != "" {
		t.Errorf("prefix still there after its time: %s", prefix())
	}
}

func TestPPPEvents(t *testing.T) {
	m := testManager()
	u := m.testUplink(Uplink{If: "ppp-none0", Link: "pt-none0", Kind: "eth", Key: "eth:pt-none0",
		PPPoE: config.PPPoE{Enabled: true, Username: "u", Password: "p"}})
	state := func() State { return m.States()[0] }
	m.refresh()
	if s := state(); s.PPPoE != PPPSigningIn || s.Internet != StDown {
		t.Fatalf("at the start: %+v", s)
	}
	// The servers the provider names come with the session.
	m.HandlePPP("ip-up", "ppp-none0", map[string]string{"IFNAME": "ppp-none0", "DNS1": "192.0.2.53", "DNS2": "192.0.2.54", "IPLOCAL": "100.64.1.2"})
	if got := strings.Join(state().DNS, " "); got != "192.0.2.53 192.0.2.54" {
		t.Errorf("DNS servers from the session: %q", got)
	}
	// dhcpcd, which sees to IPv6 on the session, adds its own and takes
	// only its own away.
	m.HandleHook(map[string]string{"interface": "ppp-none0", "reason": "BOUND6", "new_dhcp6_name_servers": "2001:db8::53"})
	if got := strings.Join(state().DNS, " "); got != "192.0.2.53 192.0.2.54 2001:db8::53" {
		t.Errorf("with DHCPv6: %q", got)
	}
	m.HandleHook(map[string]string{"interface": "ppp-none0", "reason": "STOP"})
	if got := strings.Join(state().DNS, " "); got != "192.0.2.53 192.0.2.54" {
		t.Errorf("after dhcpcd stopped: %q", got)
	}
	// One server, a nonsense value and a repeat.
	m.HandlePPP("ip-up", "ppp-none0", map[string]string{"DNS1": "192.0.2.53", "DNS2": "192.0.2.53"})
	if got := strings.Join(state().DNS, " "); got != "192.0.2.53" {
		t.Errorf("the same server twice: %q", got)
	}
	m.HandlePPP("ip-up", "ppp-none0", map[string]string{"DNS1": "0.0.0.0", "DNS2": "not an address"})
	if got := state().DNS; len(got) != 0 {
		t.Errorf("no usable server: %q", got)
	}
	m.HandlePPP("ip-up", "ppp-none0", map[string]string{"DNS1": "192.0.2.53"})
	m.HandlePPP("ip-down", "ppp-none0", nil)
	if got := state().DNS; len(got) != 0 {
		t.Errorf("after the session ended: %q", got)
	}
	m.HandlePPP("ip-up", "ppp-other", map[string]string{"DNS1": "192.0.2.99"})
	if got := state().DNS; len(got) != 0 {
		t.Errorf("another session's servers: %q", got)
	}

	// What pppd prints about a failed sign-in shows in the state, and
	// only for the uplink it is about. The loopback interface stands in
	// for a port with a cable in it: without one there is nothing to say
	// about signing in.
	if !netx.Carrier("lo") {
		t.Skip("the loopback interface is down")
	}
	m = testManager()
	u = m.testUplink(Uplink{If: "ppp-none0", Link: "lo", Kind: "eth", Key: "eth:lo", PPPoE: config.PPPoE{Enabled: true, Username: "u", Password: "p"}})
	m.refresh()
	if s := state(); s.PPPoE != PPPSigningIn || s.Internet != StConnecting {
		t.Errorf("with a cable and no session: %+v", s)
	}
	m.pppLine(u, "CHAP authentication failed")
	if s := state(); s.PPPoE != PPPWrongLogin || s.Internet != StConnecting {
		t.Errorf("after a refused sign-in: %+v", s)
	}
	m.pppLine(u, "PPP session is 4")
	m.refresh()
	if s := state(); s.PPPoE != PPPWrongLogin {
		t.Errorf("at the next attempt: %+v", s)
	}
	stale := &uplink{cfg: u.cfg, ppp: PPPSigningIn}
	m.pppLine(stale, "Timeout waiting for PADO packets")
	if s := state(); s.PPPoE != PPPWrongLogin || stale.ppp != PPPSigningIn {
		t.Errorf("a line from a pppd that is no longer this uplink's: %+v", s)
	}
	m.pppLine(u, "Timeout waiting for PADO packets")
	if s := state(); s.PPPoE != PPPNoAnswer {
		t.Errorf("after a search nobody answered: %+v", s)
	}
}

func TestSamePPPoEUplink(t *testing.T) {
	a := Uplink{If: "ppp-eth0", Link: "eth0", Kind: "eth", Key: "eth:eth0", Metric: 100, PPPoE: config.PPPoE{Enabled: true, Username: "u", Password: "p"}}
	b := a
	if !sameUplink(a, b) {
		t.Error("the same sign-in taken for a change")
	}
	b.PPPoE.Password = "q"
	if sameUplink(a, b) {
		t.Error("another password taken for no change")
	}
	b = a
	b.Link = "v35-eth0"
	if sameUplink(a, b) {
		t.Error("a tag on the port taken for no change")
	}
	b = a
	b.PD = true
	if sameUplink(a, b) {
		t.Error("asking for a prefix taken for no change")
	}
}
