// Package wan manages the router's uplinks: address configuration through
// dhcpcd (DHCPv4, IPv6 router advertisements, DHCPv6 with prefix
// delegation), signing in to a provider with pppd (PPPoE), and
// reachability probing, including captive portal detection.
package wan

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/netx"
	"github.com/ryanzhou/porter/daemon/internal/proc"
)

// Internet states of an uplink.
const (
	StDown       = "down"       // no link
	StConnecting = "connecting" // link, no address yet
	StLimited    = "limited"    // address, but the probe gets no answer
	StCaptive    = "captive"    // a captive portal intercepts traffic
	StOnline     = "online"
)

// How signing in to a provider stands, for an uplink that does (PPPoE).
const (
	PPPSigningIn  = "signing_in"  // no session yet, and no reason known
	PPPWrongLogin = "wrong_login" // the provider turned the user name or password down
	PPPNoAnswer   = "no_answer"   // nobody at the other end of the cable speaks PPPoE
	PPPUp         = "up"
)

// Uplink is an interface to use as WAN.
type Uplink struct {
	// If is the interface that reaches the internet: where the addresses
	// and routes are, and what the firewall and the probe go by.
	If string
	// Link is the Ethernet interface a PPPoE session runs over (the port,
	// or its VLAN), If then being the session's own interface, which
	// only exists while there is a session. Empty for any other uplink:
	// If is the link.
	Link string
	// PPPoE is the sign-in, for an uplink that has one.
	PPPoE config.PPPoE
	// PD asks the network to delegate an IPv6 prefix for the router's own
	// networks.
	PD bool
	// Kind is "eth", "wifi" or "phone" (a phone sharing its connection
	// over USB). Key is the uplink's entry in the configured order, and
	// Label what the hardware calls itself, where it says.
	Kind  string
	Key   string
	Label string
	// Metric is the uplink's route metric. It goes into the addresses and
	// routes the uplink is given, so it is chosen once and left alone.
	// Rank is the uplink's place in the configured order, 0 first, and
	// can change at any time without the uplink noticing.
	Metric int
	Rank   int
	// IP says, per address family, whether the uplink configures itself
	// (DHCP; router advertisements and DHCPv6) or has a fixed address.
	IP config.IPSetup
}

// link is the interface whose cable it is.
func (u Uplink) link() string {
	if u.Link != "" {
		return u.Link
	}
	return u.If
}

// State is the observable condition of an uplink.
type State struct {
	If       string   `json:"if"`
	Kind     string   `json:"kind"`
	Key      string   `json:"key"`
	Label    string   `json:"label,omitempty"`
	Metric   int      `json:"metric"`
	Rank     int      `json:"rank"`
	Carrier  bool     `json:"carrier"`
	Internet string   `json:"internet"`
	IPv4     string   `json:"ipv4,omitempty"` // address/prefix
	Gateway  string   `json:"gateway,omitempty"`
	IPv6     []string `json:"ipv6,omitempty"`
	HasV6    bool     `json:"has_v6"` // default IPv6 route present
	// Prefix is the IPv6 prefix the network has delegated to the router
	// for its own networks, if it has.
	Prefix string   `json:"prefix,omitempty"`
	DNS    []string `json:"dns,omitempty"`
	// PPPoE is how signing in to the provider stands (PPPSigningIn and
	// so on); empty for an uplink that does not sign in.
	PPPoE string `json:"pppoe,omitempty"`
	// PortalURL is where to log in when Internet is "captive".
	PortalURL string `json:"portal_url,omitempty"`
	// CaptiveAPI is the RFC 8910 URL announced by the network, if any.
	CaptiveAPI string `json:"captive_api,omitempty"`
	MAC        string `json:"mac,omitempty"`
}

type uplink struct {
	cfg    Uplink
	static string // the last problem applying fixed addresses, for logging once
	dhcpcd *proc.Proc
	// pppd runs the PPPoE session of an uplink that signs in, and ppp is
	// how that stands. dhcpcd then only runs while there is a session,
	// for IPv6.
	pppd *proc.Proc
	ppp  string
	dns4 []string
	dns6 map[string][]string // per env source
	// pd is the prefix delegated to the router and pdUntil when it ends
	// unless renewed (zero: never).
	pd      netip.Prefix
	pdUntil time.Time
	api     string
	st      State
	lastOK  time.Time
	fails   int // probes that got no answer, in a row
	// captives counts sign-in pages seen in a row.
	captives int
	probeAt  time.Time
	probing  bool
}

type Manager struct {
	Log      *slog.Logger
	RunDir   string
	HookPath string
	// PPPHooks are the programs pppd runs as a session's addresses come
	// and go, by event: "ip-up", "ip-down", "ipv6-up", "ipv6-down".
	PPPHooks map[string]string
	// HookEnv is passed to dhcpcd and pppd, and through them to the hooks.
	HookEnv []string
	// OnChange is called (from any goroutine) when an uplink's state
	// changes in a way the rest of the router cares about.
	OnChange func()

	mu      sync.Mutex
	uplinks map[string]*uplink
	probe   config.Probe
	stop    chan struct{}
}

func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop != nil {
		return
	}
	m.uplinks = map[string]*uplink{}
	m.stop = make(chan struct{})
	go m.loop(m.stop)
}

func (m *Manager) Stop() {
	m.mu.Lock()
	stop := m.stop
	m.stop = nil
	ups := m.uplinks
	m.uplinks = map[string]*uplink{}
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	for _, u := range ups {
		m.halt(u)
	}
}

// halt stops the programs running for an uplink that is no longer one,
// and takes its sign-in out of the run directory. It waits for them, so
// it is not called with the lock held.
func (m *Manager) halt(u *uplink) {
	for _, p := range []*proc.Proc{u.dhcpcd, u.pppd} {
		if p != nil {
			p.Stop()
		}
	}
	if u.cfg.PPPoE.Enabled {
		os.Remove(m.pppdPath(u.cfg))
	}
}

// Apply sets the uplinks to manage. Interfaces that are no longer uplinks
// are released.
func (m *Manager) Apply(list []Uplink, probe config.Probe) {
	m.mu.Lock()
	m.probe = probe
	want := map[string]Uplink{}
	for _, u := range list {
		want[u.If] = u
	}
	var stopped []*uplink
	var cleared []Uplink
	reranked := false
	for name, u := range m.uplinks {
		w, ok := want[name]
		if ok {
			// A new place in the order is only noted: the uplink keeps
			// its addresses and its lease.
			if w.Rank != u.cfg.Rank {
				u.cfg.Rank, u.st.Rank = w.Rank, w.Rank
				reranked = true
			}
			if sameUplink(w, u.cfg) {
				continue
			}
		}
		stopped = append(stopped, u)
		cleared = append(cleared, u.cfg)
		delete(m.uplinks, name)
	}
	var started []*uplink
	for name, w := range want {
		if _, ok := m.uplinks[name]; ok {
			continue
		}
		u := &uplink{cfg: w, dns6: map[string][]string{}, st: State{If: w.If, Kind: w.Kind, Key: w.Key, Label: w.Label, Metric: w.Metric, Rank: w.Rank, Internet: StDown}}
		if w.PPPoE.Enabled {
			u.ppp, u.st.PPPoE = PPPSigningIn, PPPSigningIn
		}
		m.uplinks[name] = u
		started = append(started, u)
	}
	m.mu.Unlock()

	for _, u := range stopped {
		m.halt(u)
	}
	for _, old := range cleared {
		clearStatic(old)
	}
	for _, u := range started {
		m.start(u)
	}
	if len(cleared)+len(started) > 0 || reranked {
		m.changed()
	}
}

// sameUplink reports whether two descriptions of an uplink call for the
// same handling. An address setup that is automatic is the same however
// it is written: restarting DHCP because "automatic" arrived spelled out
// (as it does the moment a wireless uplink learns which network it is on)
// would cost every Wi-Fi connection several seconds.
func sameUplink(a, b Uplink) bool {
	canon := func(f config.IPFamily) config.IPFamily {
		if !f.Static() {
			return config.IPFamily{}
		}
		return f
	}
	a.IP, b.IP = config.IPSetup{V4: canon(a.IP.V4), V6: canon(a.IP.V6)}, config.IPSetup{V4: canon(b.IP.V4), V6: canon(b.IP.V6)}
	return reflect.DeepEqual(a, b)
}

// statics lists the fixed addresses of an uplink: address and gateway
// per family configured by hand. The configuration has been validated,
// so these parse.
func statics(u Uplink) (out []struct {
	Addr netip.Prefix
	Gw   netip.Addr
}) {
	for _, f := range []config.IPFamily{u.IP.V4, u.IP.V6} {
		if !f.Static() {
			continue
		}
		addr, err1 := netip.ParsePrefix(f.Address)
		gw, err2 := netip.ParseAddr(f.Gateway)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, struct {
			Addr netip.Prefix
			Gw   netip.Addr
		}{addr, gw})
	}
	return out
}

// applyStatic puts an uplink's fixed addresses and routes in place. It is
// called when the uplink starts and on every refresh: an interface that
// was not there yet, or lost its addresses when its link went down and
// came back, is put right within seconds.
func (m *Manager) applyStatic(u *uplink) {
	problem := ""
	for _, s := range statics(u.cfg) {
		if err := netx.SetStatic(u.cfg.If, s.Addr, s.Gw, u.cfg.Metric); err != nil {
			problem = err.Error()
		}
	}
	if problem != u.static {
		u.static = problem
		if problem != "" {
			m.Log.Warn("fixed uplink address", "if", u.cfg.If, "err", problem)
		}
	}
}

func clearStatic(u Uplink) {
	for _, s := range statics(u) {
		netx.ClearStatic(u.If, s.Addr, s.Gw, u.Metric)
	}
}

func (m *Manager) start(u *uplink) {
	if err := netx.Up(u.cfg.link()); err != nil {
		m.Log.Warn("uplink not available yet", "if", u.cfg.link(), "err", err)
	}
	if u.cfg.PPPoE.Enabled {
		m.startPPP(u)
		return
	}
	// A route out of a port whose cable has been pulled is not a route.
	// The kernel keeps using it otherwise, which matters for fixed
	// addresses: nothing takes those away when the link goes.
	netx.Sysctl("net/ipv4/conf/"+u.cfg.If+"/ignore_routes_with_linkdown", "1")
	netx.Sysctl("net/ipv6/conf/"+u.cfg.If+"/ignore_routes_with_linkdown", "1")
	if u.cfg.IP.V4.Static() || u.cfg.IP.V6.Static() {
		m.Log.Info("uplink with fixed addresses", "if", u.cfg.If, "ipv4", u.cfg.IP.V4.Address, "ipv6", u.cfg.IP.V6.Address)
		m.mu.Lock()
		m.applyStatic(u)
		m.mu.Unlock()
	}
	if u.cfg.IP.V4.Static() && u.cfg.IP.V6.Static() {
		// Nothing left to ask the network for.
		return
	}
	m.mu.Lock()
	m.runDHCP(u)
	m.mu.Unlock()
}

// runDHCP starts dhcpcd on an uplink's interface. Called with the lock
// held.
func (m *Manager) runDHCP(u *uplink) {
	conf := dhcpcdConf(u.cfg, m.HookPath)
	path := filepath.Join(m.RunDir, "dhcpcd-"+u.cfg.If+".conf")
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		m.Log.Error("write dhcpcd config", "err", err)
		return
	}
	u.dhcpcd = &proc.Proc{
		Name: "dhcpcd-" + u.cfg.If, Log: m.Log,
		Argv: []string{"dhcpcd", "--nobackground", "--config", path, u.cfg.If},
		Env:  m.HookEnv,
	}
	u.dhcpcd.Start()
}

func (m *Manager) pppdPath(u Uplink) string {
	return filepath.Join(m.RunDir, "pppd-"+u.If+".conf")
}

// startPPP runs pppd for an uplink that signs in to its provider. pppd
// keeps the session up by itself; what it prints is how the router knows
// why there is none.
func (m *Manager) startPPP(u *uplink) {
	// The file holds the password: for root only, and written in one
	// step so that it is never there with other rights.
	path := m.pppdPath(u.cfg)
	if err := config.WriteFileAtomic(path, []byte(pppdConf(u.cfg, m.PPPHooks, m.HookEnv)), 0o600); err != nil {
		m.Log.Error("write pppd options", "err", err)
		return
	}
	p := &proc.Proc{
		Name: "pppd-" + u.cfg.If, Log: m.Log,
		Argv:   []string{"pppd", "file", path},
		OnLine: func(line string) { m.pppLine(u, line) },
	}
	m.mu.Lock()
	u.pppd = p
	m.mu.Unlock()
	m.Log.Info("uplink signing in with PPPoE", "if", u.cfg.If, "over", u.cfg.Link)
	p.Start()
}

// pppLine takes note of what pppd says about a sign-in that failed.
func (m *Manager) pppLine(u *uplink, line string) {
	m.mu.Lock()
	next := pppPhase(u.ppp, line)
	changed := next != u.ppp && m.uplinks[u.cfg.If] == u
	if changed {
		u.ppp = next
	}
	m.mu.Unlock()
	if changed {
		m.Log.Info("PPPoE", "if", u.cfg.If, "state", next, "pppd", line)
		m.refresh()
	}
}

// pppPhase is how a sign-in stands after pppd has printed line, given how
// it stood before. pppd says many things; the ones that matter are the
// two ways of not getting in that a person can do something about. A
// failure stays the state until something else happens, so that it does
// not flicker with every new attempt.
func pppPhase(prev, line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "authentication fail"):
		// "PAP authentication failed", "CHAP authentication failed",
		// "MS-CHAP authentication failed: ...", "EAP: peer reports
		// authentication failure", and what a provider that closes the
		// line instead says: "LCP terminated by peer (Authentication
		// failed)".
		return PPPWrongLogin
	case strings.Contains(l, "authentication succeeded"):
		return PPPSigningIn
	case strings.Contains(l, "timeout waiting for pad"),
		strings.Contains(l, "unable to complete pppoe discovery"),
		strings.Contains(l, "timeout sending config-requests"):
		// Nobody answered the search for a provider ("Timeout waiting
		// for PADO packets"), or the one that did went quiet.
		return PPPNoAnswer
	}
	return prev
}

// pppSettle adjusts how a sign-in stands to what is there to see: a
// session with an address is up whatever was said before it, one that
// has just ended is being made again, and without a cable there is
// nothing to say.
func pppSettle(phase string, carrier, hasAddr bool) string {
	switch {
	case hasAddr:
		return PPPUp
	case !carrier, phase == PPPUp, phase == "":
		return PPPSigningIn
	}
	return phase
}

// pppQuote writes a word for pppd's options file so that it is read back
// as it is, whatever is in it.
func pppQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// pppdConf is the options file of one PPPoE session. pppd also reads
// /etc/ppp/options first, which on Debian asks the other side to
// authenticate and sets echo timings; what matters of that is overridden
// here.
func pppdConf(u Uplink, hooks map[string]string, env []string) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	w("# generated by porterd")
	w("plugin pppoe.so")
	w("nic-" + u.Link)
	w("ifname " + u.If)
	w("user " + pppQuote(u.PPPoE.Username))
	w("password " + pppQuote(u.PPPoE.Password))
	w("hide-password")
	// The provider checks who the router is, not the other way round.
	w("noauth")
	w("noipdefault")
	// The default route with this uplink's own metric, next to those of
	// the other uplinks, as dhcpcd adds its.
	w("defaultroute")
	w(fmt.Sprintf("defaultroute-metric %d", u.Metric))
	w("usepeerdns")
	// IPv6 on the session gets its link-local address here; addresses
	// and a delegated prefix are then dhcpcd's business.
	w("+ipv6")
	// A session the provider ends is made again, for ever, after a pause
	// that keeps a wrong password from hammering the provider. One that
	// goes silent is given up after half a minute without an echo.
	w("persist")
	w("maxfail 0")
	w("holdoff 10")
	w("lcp-echo-interval 10")
	w("lcp-echo-failure 3")
	w("nodetach")
	events := make([]string, 0, len(hooks))
	for event := range hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	for _, event := range events {
		w(event + "-script " + pppQuote(hooks[event]))
	}
	// pppd gives its scripts an environment of its own making.
	for _, kv := range env {
		w("set " + pppQuote(kv))
	}
	return b.String()
}

func dhcpcdConf(u Uplink, hook string) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	w("# generated by porterd")
	// Identify by hardware address only, and send no host name: the
	// upstream network learns nothing beyond the MAC it sees anyway.
	w("clientid")
	w("option rapid_commit")
	w("option domain_name_servers, domain_name, interface_mtu, captive_portal_uri")
	w("require dhcp_server_identifier")
	w("noipv4ll")
	// Take the address the network hands out without first spending five
	// seconds asking whether anyone else has it. Handing out addresses
	// that are free is the network's job, and a travel router is joined
	// to a new one too often to wait each time.
	w("noarp")
	// A family with a fixed address is none of dhcpcd's business, nor is
	// IPv4 where pppd has negotiated it.
	switch {
	case u.IP.V4.Static() || u.PPPoE.Enabled:
		w("ipv6only")
	case u.IP.V6.Static():
		w("ipv4only")
	}
	if !u.IP.V6.Static() {
		w("slaac private")
		w("ipv6rs")
		w("ia_na")
		if u.PD {
			// A prefix for the router's own networks. It is given an
			// identifier because dhcpcd takes a second request without
			// one for the first again, and "-" in place of an interface
			// because sharing the prefix out is not left to dhcpcd: it
			// only has to obtain it, report it to the hook, and keep
			// what nobody uses of it from being sent back upstream (the
			// reject route it adds for the whole prefix).
			w("ia_pd 1 -")
		}
	}
	w(fmt.Sprintf("metric %d", u.Metric))
	w("timeout 0")
	w("script " + hook)
	return b.String()
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		m.OnChange()
	}
}

// rePD matches the variable in which dhcpcd's hook is told a delegated
// prefix: one per prefix of each IA_PD of the lease, numbered from 1.
// Its length and lifetimes are in variables named after it.
var rePD = regexp.MustCompile(`^new_dhcp6_ia_pd([0-9]+)_prefix([0-9]+)$`)

// delegated reads the prefix delegated to the router out of the
// environment of a dhcpcd hook, and how long it is valid for (0: for
// ever). Of several it is the first that may be used for new
// connections, or else the first that is still valid. A prefix longer
// than /64 is of no use: no network could be given a /64 of it.
func delegated(env map[string]string) (prefix netip.Prefix, valid time.Duration) {
	type candidate struct {
		ia, n     int
		prefix    netip.Prefix
		preferred uint64
		valid     uint64
	}
	var all []candidate
	for k, v := range env {
		m := rePD.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		addr, err := netip.ParseAddr(v)
		bits, err2 := strconv.Atoi(env[k+"_length"])
		if err != nil || err2 != nil || !addr.Is6() || addr.Is4In6() || bits < 1 || bits > 64 {
			continue
		}
		c := candidate{prefix: netip.PrefixFrom(addr, bits).Masked()}
		c.ia, _ = strconv.Atoi(m[1])
		c.n, _ = strconv.Atoi(m[2])
		c.preferred, _ = strconv.ParseUint(env[k+"_pltime"], 10, 32)
		c.valid, _ = strconv.ParseUint(env[k+"_vltime"], 10, 32)
		if c.valid > 0 {
			all = append(all, c)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if (a.preferred > 0) != (b.preferred > 0) {
			return a.preferred > 0
		}
		if a.ia != b.ia {
			return a.ia < b.ia
		}
		return a.n < b.n
	})
	if len(all) == 0 {
		return netip.Prefix{}, 0
	}
	if all[0].valid != 0xffffffff {
		valid = time.Duration(all[0].valid) * time.Second
	}
	return all[0].prefix, valid
}

// setPD records what a DHCPv6 lease says about a delegated prefix.
func (u *uplink) setPD(env map[string]string, now time.Time) {
	prefix, valid := delegated(env)
	u.pd, u.pdUntil = prefix, time.Time{}
	if prefix.IsValid() && valid > 0 {
		u.pdUntil = now.Add(valid)
	}
}

// HandlePPP digests one of pppd's reports that a session's addresses
// have come or gone: event is "ip-up", "ip-down", "ipv6-up" or
// "ipv6-down", env what pppd tells its scripts.
func (m *Manager) HandlePPP(event, ifname string, env map[string]string) {
	m.mu.Lock()
	u := m.uplinks[ifname]
	if u == nil {
		m.mu.Unlock()
		return
	}
	switch event {
	case "ip-up":
		// The DNS servers come with the session, as they come with a
		// lease elsewhere.
		u.dns4 = nil
		for _, k := range []string{"DNS1", "DNS2"} {
			if a, err := netip.ParseAddr(env[k]); err == nil && a.Is4() && !a.IsUnspecified() {
				u.dns4 = append(u.dns4, a.String())
			}
		}
		u.dns4 = dedup(u.dns4)
	case "ip-down":
		u.dns4 = nil
		u.lastOK = time.Time{}
	}
	u.probeAt = time.Time{} // re-probe promptly
	m.mu.Unlock()
	m.Log.Info("uplink event", "if", ifname, "reason", event)
	m.refresh()
}

// HandleHook digests one dhcpcd hook invocation.
func (m *Manager) HandleHook(env map[string]string) {
	ifname := env["interface"]
	reason := env["reason"]
	m.mu.Lock()
	u := m.uplinks[ifname]
	if u == nil {
		m.mu.Unlock()
		return
	}
	switch reason {
	case "BOUND", "RENEW", "REBIND", "REBOOT", "INFORM", "STATIC":
		u.dns4 = strings.Fields(env["new_domain_name_servers"])
		u.api = env["new_captive_portal_uri"]
	case "EXPIRE", "FAIL", "NAK", "STOP", "RELEASE", "NOCARRIER", "DEPARTED":
		// The IPv4 servers of a PPPoE session are pppd's to give and
		// take (HandlePPP); dhcpcd only sees to IPv6 there.
		if !u.cfg.PPPoE.Enabled {
			u.dns4 = nil
		}
		u.api = ""
		if reason == "NOCARRIER" || reason == "DEPARTED" || reason == "STOP" {
			u.dns6 = map[string][]string{}
			u.pd, u.pdUntil = netip.Prefix{}, time.Time{}
		}
		u.lastOK = time.Time{}
	case "ROUTERADVERT":
		var servers []string
		for k, v := range env {
			if strings.HasPrefix(k, "nd") && strings.Contains(k, "_rdnss") && strings.HasSuffix(k, "_servers") {
				servers = append(servers, strings.Fields(v)...)
			}
		}
		u.dns6["ra"] = servers
	case "BOUND6", "RENEW6", "REBIND6", "REBOOT6", "INFORM6":
		u.dns6["dhcp6"] = strings.Fields(env["new_dhcp6_name_servers"])
		if v := env["new_dhcp6_captive_portal_uri"]; v != "" && u.api == "" {
			u.api = v
		}
		// The lease as it now is says whether a prefix is delegated,
		// and which.
		was := u.pd
		u.setPD(env, time.Now())
		if u.pd != was {
			m.Log.Info("delegated prefix", "if", ifname, "prefix", u.pd, "was", was)
		}
	case "EXPIRE6", "STOP6", "RELEASE6":
		delete(u.dns6, "dhcp6")
		u.pd, u.pdUntil = netip.Prefix{}, time.Time{}
	}
	u.probeAt = time.Time{} // re-probe promptly
	m.mu.Unlock()
	m.Log.Info("uplink event", "if", ifname, "reason", reason)
	m.refresh()
}

func (m *Manager) loop(stop chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.refresh()
		}
	}
}

// refresh re-reads kernel state for every uplink and schedules probes.
func (m *Manager) refresh() {
	m.mu.Lock()
	changed := false
	now := time.Now()
	var gone []*proc.Proc
	for _, u := range m.uplinks {
		m.applyStatic(u)
		st := u.st
		st.Carrier = netx.Carrier(u.cfg.link())
		if u.cfg.PPPoE.Enabled {
			// IPv6 on a PPPoE session is dhcpcd's, for as long as the
			// session's interface is there: it starts once pppd has
			// given the interface its link-local address, and goes with
			// the interface.
			switch {
			case u.dhcpcd == nil && netx.LinkLocal(u.cfg.If):
				m.runDHCP(u)
			case u.dhcpcd != nil && !netx.Exists(u.cfg.If):
				gone = append(gone, u.dhcpcd)
				u.dhcpcd, u.dns6 = nil, map[string][]string{}
				u.pd, u.pdUntil = netip.Prefix{}, time.Time{}
			}
		}
		if !u.pdUntil.IsZero() && now.After(u.pdUntil) {
			// Not renewed, and nobody said so.
			u.pd, u.pdUntil = netip.Prefix{}, time.Time{}
		}
		v4, v6 := netx.Addrs(u.cfg.If)
		st.IPv4, st.Gateway, st.IPv6 = "", "", nil
		if len(v4) > 0 {
			st.IPv4 = v4[0].String()
		}
		for _, a := range v6 {
			st.IPv6 = append(st.IPv6, a.String())
		}
		if gw, ok := netx.DefaultRoute(u.cfg.If, unix.AF_INET); ok && gw.IsValid() {
			st.Gateway = gw.String()
		}
		// IPv6 needs a way out and an address to be answered at: one of
		// the uplink's own, or (a provider may give the link itself none)
		// those of the prefix delegated for the router's networks.
		_, st.HasV6 = netx.DefaultRoute(u.cfg.If, unix.AF_INET6)
		st.HasV6 = st.HasV6 && (len(st.IPv6) > 0 || u.pd.IsValid())
		st.Prefix = ""
		if u.pd.IsValid() {
			st.Prefix = u.pd.String()
		}
		st.MAC, _, _ = netx.HardwareAddr(u.cfg.link())
		st.CaptiveAPI = u.api
		// Resolvers come from wherever the addresses come from: typed in
		// with a fixed address, learned from the network otherwise.
		var dns []string
		if u.cfg.IP.V4.Static() {
			dns = append(dns, u.cfg.IP.V4.DNS...)
		} else {
			dns = append(dns, u.dns4...)
		}
		if u.cfg.IP.V6.Static() {
			dns = append(dns, u.cfg.IP.V6.DNS...)
		} else {
			for _, k := range []string{"ra", "dhcp6"} {
				for _, s := range u.dns6[k] {
					// Link-local resolvers need a zone; skip them, a
					// global one is practically always offered as well.
					if a, err := netip.ParseAddr(s); err == nil && !a.IsLinkLocalUnicast() {
						dns = append(dns, a.String())
					}
				}
			}
		}
		st.DNS = dedup(dns)
		hasAddr := st.IPv4 != "" || st.HasV6
		if u.cfg.PPPoE.Enabled {
			u.ppp = pppSettle(u.ppp, st.Carrier, hasAddr)
			st.PPPoE = u.ppp
		}
		switch {
		case !st.Carrier:
			st.Internet, st.PortalURL = StDown, ""
		case !hasAddr:
			st.Internet, st.PortalURL = StConnecting, ""
		case st.Internet == StDown || st.Internet == StConnecting:
			st.Internet = StLimited
			u.probeAt = time.Time{}
		}
		if !stateEqual(st, u.st) {
			changed = true
		}
		u.st = st
		if hasAddr && st.Carrier && !u.probing && !now.Before(u.probeAt) {
			u.probing = true
			go m.runProbe(u, m.probe, st.DNS)
		}
	}
	m.mu.Unlock()
	for _, p := range gone {
		p.Stop()
	}
	if changed {
		m.changed()
	}
}

func (m *Manager) runProbe(u *uplink, p config.Probe, dns []string) {
	res := Probe(u.cfg.If, p, dns)
	m.mu.Lock()
	u.probing = false
	// The uplink may have been replaced while probing.
	if m.uplinks[u.cfg.If] != u {
		m.mu.Unlock()
		return
	}
	prev := u.st.Internet
	prevURL := u.st.PortalURL
	now := time.Now()
	next := 5 * time.Second
	switch res.State {
	case StOnline:
		u.lastOK, u.fails, u.captives = now, 0, 0
		u.st.Internet, u.st.PortalURL = StOnline, ""
		next = 45 * time.Second
		// With somewhere else to go, a dead uplink is worth noticing
		// sooner.
		others := 0
		for _, o := range m.uplinks {
			if o != u && o.st.Carrier && (o.st.IPv4 != "" || o.st.HasV6) {
				others++
			}
		}
		if others > 0 {
			next = 15 * time.Second
		}
	case StCaptive:
		u.fails = 0
		u.captives++
		if prev == StOnline && u.captives < 2 {
			// A working uplink does not turn into a sign-in page on the
			// strength of one odd answer: the device chosen to sign in
			// would be let past the VPN for it. Ask again first.
			next = 2 * time.Second
			break
		}
		u.st.Internet, u.st.PortalURL = StCaptive, res.PortalURL
		next = 4 * time.Second
	default:
		// One lost probe does not make a working uplink "limited", nor
		// end a sign-in that is under way; three in a row do. Another
		// uplink can then take over.
		u.fails++
		u.captives = 0
		if (prev == StOnline || prev == StCaptive) && u.fails < 3 {
			next = 3 * time.Second
			if u.fails == 1 {
				next = time.Second
			}
		} else if u.st.Internet != StDown && u.st.Internet != StConnecting {
			u.st.Internet, u.st.PortalURL = StLimited, ""
		}
	}
	u.probeAt = now.Add(next)
	changed := prev != u.st.Internet || prevURL != u.st.PortalURL
	m.mu.Unlock()
	if changed {
		m.Log.Info("uplink reachability", "if", u.cfg.If, "state", res.State, "detail", res.Detail)
		m.changed()
	}
}

// ProbeNow makes every uplink re-check reachability immediately.
func (m *Manager) ProbeNow() {
	m.mu.Lock()
	for _, u := range m.uplinks {
		u.probeAt = time.Time{}
	}
	m.mu.Unlock()
	m.refresh()
}

// States returns all uplinks in the configured order, best first.
func (m *Manager) States() []State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.uplinks))
	for _, u := range m.uplinks {
		out = append(out, u.st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		if out[i].Metric != out[j].Metric {
			return out[i].Metric < out[j].Metric
		}
		return out[i].If < out[j].If
	})
	return out
}

// Preferred picks the uplink the main routing table uses by itself: of
// those that have an address, the one with the lowest metric.
func Preferred(states []State) *State {
	var best *State
	for i := range states {
		if s := &states[i]; s.Carrier && (s.IPv4 != "" || s.HasV6) && (best == nil || s.Metric < best.Metric) {
			best = s
		}
	}
	return best
}

// Active picks the uplink to use from states in their configured order:
// the first that reaches the internet or stands behind a sign-in page
// (which only signing in will get past, so it is not skipped). When none
// does either, it is the first with an address, as there is nothing
// better to offer.
func Active(states []State) *State {
	var fallback *State
	for i := range states {
		s := &states[i]
		if !s.Carrier || (s.IPv4 == "" && !s.HasV6) {
			continue
		}
		if s.Internet == StOnline || s.Internet == StCaptive {
			return s
		}
		if fallback == nil {
			fallback = s
		}
	}
	return fallback
}

func stateEqual(a, b State) bool {
	return a.Carrier == b.Carrier && a.Internet == b.Internet && a.IPv4 == b.IPv4 && a.Gateway == b.Gateway &&
		a.HasV6 == b.HasV6 && a.PortalURL == b.PortalURL && a.CaptiveAPI == b.CaptiveAPI && a.MAC == b.MAC &&
		a.Prefix == b.Prefix && a.PPPoE == b.PPPoE &&
		strings.Join(a.IPv6, ",") == strings.Join(b.IPv6, ",") && strings.Join(a.DNS, ",") == strings.Join(b.DNS, ",")
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
