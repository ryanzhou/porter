// Package wan manages the router's uplinks: address configuration through
// dhcpcd (DHCPv4, IPv6 router advertisements, DHCPv6) and reachability
// probing, including captive portal detection.
package wan

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// Uplink is an interface to use as WAN.
type Uplink struct {
	If string
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
	DNS      []string `json:"dns,omitempty"`
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
	dns4   []string
	dns6   map[string][]string // per env source
	api    string
	st     State
	lastOK time.Time
	fails  int // probes that got no answer, in a row
	// captives counts sign-in pages seen in a row.
	captives int
	probeAt  time.Time
	probing  bool
}

type Manager struct {
	Log      *slog.Logger
	RunDir   string
	HookPath string
	// HookEnv is passed to dhcpcd, and through it to the hook.
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
		if u.dhcpcd != nil {
			u.dhcpcd.Stop()
		}
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
	var stopped []*proc.Proc
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
		if u.dhcpcd != nil {
			stopped = append(stopped, u.dhcpcd)
		}
		cleared = append(cleared, u.cfg)
		delete(m.uplinks, name)
	}
	var started []*uplink
	for name, w := range want {
		if _, ok := m.uplinks[name]; ok {
			continue
		}
		u := &uplink{cfg: w, dns6: map[string][]string{}, st: State{If: w.If, Kind: w.Kind, Key: w.Key, Label: w.Label, Metric: w.Metric, Rank: w.Rank, Internet: StDown}}
		m.uplinks[name] = u
		started = append(started, u)
	}
	m.mu.Unlock()

	for _, p := range stopped {
		p.Stop()
	}
	for _, old := range cleared {
		clearStatic(old)
	}
	for _, u := range started {
		m.startDHCP(u)
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

func (m *Manager) startDHCP(u *uplink) {
	if err := netx.Up(u.cfg.If); err != nil {
		m.Log.Warn("uplink not available yet", "if", u.cfg.If, "err", err)
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
	conf := dhcpcdConf(u.cfg, m.HookPath)
	path := filepath.Join(m.RunDir, "dhcpcd-"+u.cfg.If+".conf")
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		m.Log.Error("write dhcpcd config", "err", err)
		return
	}
	p := &proc.Proc{
		Name: "dhcpcd-" + u.cfg.If, Log: m.Log,
		Argv: []string{"dhcpcd", "--nobackground", "--config", path, u.cfg.If},
		Env:  m.HookEnv,
	}
	m.mu.Lock()
	u.dhcpcd = p
	m.mu.Unlock()
	p.Start()
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
	// A family with a fixed address is none of dhcpcd's business.
	switch {
	case u.IP.V4.Static():
		w("ipv6only")
	case u.IP.V6.Static():
		w("ipv4only")
	}
	if !u.IP.V6.Static() {
		w("slaac private")
		w("ipv6rs")
		w("ia_na")
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
		u.dns4 = nil
		u.api = ""
		if reason == "NOCARRIER" || reason == "DEPARTED" || reason == "STOP" {
			u.dns6 = map[string][]string{}
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
	case "EXPIRE6", "STOP6", "RELEASE6":
		delete(u.dns6, "dhcp6")
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
	for _, u := range m.uplinks {
		m.applyStatic(u)
		st := u.st
		st.Carrier = netx.Carrier(u.cfg.If)
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
		_, st.HasV6 = netx.DefaultRoute(u.cfg.If, unix.AF_INET6)
		st.HasV6 = st.HasV6 && len(st.IPv6) > 0
		st.MAC, _, _ = netx.HardwareAddr(u.cfg.If)
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
