// Package vpn runs WireGuard tunnels. It only brings interfaces up and
// reports their health; which traffic uses a tunnel is decided by the
// policy-routing tables in package fw.
package vpn

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/netx"
)

// A tunnel counts as up while its last handshake is younger than this.
// WireGuard re-handshakes every two minutes while traffic (or the
// keepalive) flows.
const handshakeFresh = 190 * time.Second

// Status of one tunnel.
type Status struct {
	ID        string `json:"id"`
	If        string `json:"if"`
	State     string `json:"state"` // "off", "connecting", "up", "error"
	Error     string `json:"error,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Handshake int64  `json:"handshake,omitempty"` // unix time of last handshake
	Rx        int64  `json:"rx"`
	Tx        int64  `json:"tx"`
}

type tunnel struct {
	cfg      config.VPN
	st       Status
	resolved time.Time
	started  time.Time
}

// Resolver looks up an endpoint host name via the uplink.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

type Manager struct {
	Log *slog.Logger
	// Resolve is used for endpoint host names.
	Resolve Resolver
	// OnChange is called when a tunnel goes up or down.
	OnChange func()
	// UserspaceImpl, when set, is a wireguard-go binary used instead of
	// the kernel module (for tests on kernels without WireGuard).
	UserspaceImpl string

	mu      sync.Mutex
	tunnels map[string]*tunnel
	procs   map[string]*exec.Cmd
	stop    chan struct{}
}

func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop != nil {
		return
	}
	m.tunnels = map[string]*tunnel{}
	m.procs = map[string]*exec.Cmd{}
	m.stop = make(chan struct{})
	go m.loop(m.stop)
}

func (m *Manager) Stop() {
	m.mu.Lock()
	stop := m.stop
	m.stop = nil
	ids := make([]string, 0, len(m.tunnels))
	for id := range m.tunnels {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	for _, id := range ids {
		m.remove(id)
	}
}

// Apply brings the enabled profiles up and removes everything else.
func (m *Manager) Apply(vpns []config.VPN) {
	want := map[string]config.VPN{}
	for _, v := range vpns {
		if v.Enabled {
			want[v.ID] = v
		}
	}
	m.mu.Lock()
	var gone []string
	for id := range m.tunnels {
		if _, ok := want[id]; !ok {
			gone = append(gone, id)
		}
	}
	m.mu.Unlock()
	for _, id := range gone {
		m.remove(id)
	}
	for _, v := range want {
		m.mu.Lock()
		t := m.tunnels[v.ID]
		same := t != nil && sameWG(t.cfg, v) // a failed tunnel is retried by poll()
		if t != nil {
			t.cfg.KillSwitch, t.cfg.Name = v.KillSwitch, v.Name
		}
		m.mu.Unlock()
		if same {
			continue
		}
		if err := m.configure(v); err != nil {
			m.Log.Error("vpn setup failed", "vpn", v.Name, "err", err)
			m.mu.Lock()
			m.tunnels[v.ID] = &tunnel{cfg: v, st: Status{ID: v.ID, If: fw.VPNIfName(v.ID), State: "error", Error: err.Error()}}
			m.mu.Unlock()
		}
	}
	// Leftover interfaces from a previous run.
	if links, err := netlink.LinkList(); err == nil {
		for _, l := range links {
			name := l.Attrs().Name
			if len(name) > 3 && name[:3] == "wg-" {
				if _, ok := want[name[3:]]; !ok {
					netlink.LinkDel(l)
				}
			}
		}
	}
}

func sameWG(a, b config.VPN) bool {
	return fmt.Sprintf("%+v", a.WireGuard) == fmt.Sprintf("%+v", b.WireGuard)
}

func (m *Manager) remove(id string) {
	name := fw.VPNIfName(id)
	m.mu.Lock()
	delete(m.tunnels, id)
	cmd := m.procs[id]
	delete(m.procs, id)
	m.mu.Unlock()
	if l, err := netlink.LinkByName(name); err == nil {
		netlink.LinkDel(l)
	}
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

func (m *Manager) ensureLink(id, name string) error {
	if netx.Exists(name) {
		return nil
	}
	if m.UserspaceImpl == "" {
		la := netlink.NewLinkAttrs()
		la.Name = name
		return netlink.LinkAdd(&netlink.GenericLink{LinkAttrs: la, LinkType: "wireguard"})
	}
	cmd := exec.Command(m.UserspaceImpl, "-f", name)
	cmd.Env = append(os.Environ(), "WG_PROCESS_FOREGROUND=1")
	if err := cmd.Start(); err != nil {
		return err
	}
	m.mu.Lock()
	m.procs[id] = cmd
	m.mu.Unlock()
	for i := 0; i < 50; i++ {
		if netx.Exists(name) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("userspace WireGuard did not create %s", name)
}

func key(s string) (wgtypes.Key, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return wgtypes.Key{}, fmt.Errorf("bad key")
	}
	return wgtypes.NewKey(b)
}

func (m *Manager) configure(v config.VPN) error {
	name := fw.VPNIfName(v.ID)
	if err := m.ensureLink(v.ID, name); err != nil {
		return fmt.Errorf("create interface: %w", err)
	}
	priv, err := key(v.WireGuard.PrivateKey)
	if err != nil {
		return err
	}
	wcfg := wgtypes.Config{PrivateKey: &priv, ReplacePeers: true}
	endpoint := ""
	for _, p := range v.WireGuard.Peers {
		pub, err := key(p.PublicKey)
		if err != nil {
			return err
		}
		pc := wgtypes.PeerConfig{PublicKey: pub, ReplaceAllowedIPs: true}
		if p.PresharedKey != "" {
			psk, err := key(p.PresharedKey)
			if err != nil {
				return err
			}
			pc.PresharedKey = &psk
		}
		for _, a := range p.AllowedIPs {
			pf, err := netip.ParsePrefix(a)
			if err != nil {
				return err
			}
			pf = pf.Masked()
			pc.AllowedIPs = append(pc.AllowedIPs, net.IPNet{IP: pf.Addr().AsSlice(), Mask: net.CIDRMask(pf.Bits(), pf.Addr().BitLen())})
		}
		// A keepalive keeps the session (and so our view of the tunnel's
		// health) alive without client traffic.
		ka := time.Duration(p.Keepalive) * time.Second
		if p.Keepalive == 0 {
			ka = 25 * time.Second
		}
		pc.PersistentKeepaliveInterval = &ka
		if p.Endpoint != "" {
			ep, err := m.resolveEndpoint(p.Endpoint)
			if err != nil {
				// Not fatal: the uplink may simply not be there yet. The
				// loop retries.
				m.Log.Warn("vpn endpoint not resolvable yet", "vpn", v.Name, "err", err)
			} else {
				pc.Endpoint = ep
				endpoint = ep.String()
			}
		}
		wcfg.Peers = append(wcfg.Peers, pc)
	}
	c, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.ConfigureDevice(name, wcfg); err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	mtu := v.WireGuard.MTU
	if mtu == 0 {
		mtu = 1420
	}
	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return err
	}
	var addrs []netip.Prefix
	for _, a := range v.WireGuard.Addresses {
		pf, err := config.ParsePrefixLoose(a)
		if err != nil {
			return err
		}
		if pf.Addr().Is6() && !fw.HaveIPv6() {
			continue
		}
		addrs = append(addrs, pf)
	}
	if _, err := netx.SyncAddrs(name, addrs); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	now := time.Now()
	m.mu.Lock()
	m.tunnels[v.ID] = &tunnel{cfg: v, resolved: now, started: now,
		st: Status{ID: v.ID, If: name, State: "connecting", Endpoint: endpoint}}
	m.mu.Unlock()
	return nil
}

func (m *Manager) resolveEndpoint(ep string) (*net.UDPAddr, error) {
	host, portStr, err := net.SplitHostPort(ep)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(portStr)
	if a, err := netip.ParseAddr(host); err == nil {
		return &net.UDPAddr{IP: a.AsSlice(), Port: port}, nil
	}
	if m.Resolve == nil {
		return nil, fmt.Errorf("no resolver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := m.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	// Prefer IPv4: it is what the uplink is most likely to carry.
	for _, a := range addrs {
		if a.Is4() {
			return &net.UDPAddr{IP: a.AsSlice(), Port: port}, nil
		}
	}
	if len(addrs) > 0 && fw.HaveIPv6() {
		return &net.UDPAddr{IP: addrs[0].AsSlice(), Port: port}, nil
	}
	return nil, fmt.Errorf("no usable address for %s", host)
}

func (m *Manager) loop(stop chan struct{}) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.poll()
		}
	}
}

// poll refreshes tunnel health and re-resolves endpoints of tunnels that
// are not getting through.
func (m *Manager) poll() {
	c, err := wgctrl.New()
	if err != nil {
		return
	}
	defer c.Close()
	m.mu.Lock()
	changed := false
	var retry []config.VPN
	now := time.Now()
	for _, t := range m.tunnels {
		if t.st.State == "error" {
			if now.Sub(t.resolved) > 15*time.Second {
				t.resolved = now
				retry = append(retry, t.cfg)
			}
			continue
		}
		dev, err := c.Device(t.st.If)
		if err != nil {
			continue
		}
		var last time.Time
		var rx, tx int64
		hasEndpoint := false
		for _, p := range dev.Peers {
			if p.LastHandshakeTime.After(last) {
				last = p.LastHandshakeTime
			}
			rx += p.ReceiveBytes
			tx += p.TransmitBytes
			if p.Endpoint != nil {
				hasEndpoint = true
			}
		}
		state := "connecting"
		if !last.IsZero() && now.Sub(last) < handshakeFresh {
			state = "up"
		}
		if state != t.st.State {
			changed = true
			m.Log.Info("vpn state", "vpn", t.cfg.Name, "state", state)
		}
		t.st.State, t.st.Rx, t.st.Tx = state, rx, tx
		t.st.Handshake = 0
		if !last.IsZero() {
			t.st.Handshake = last.Unix()
		}
		// Endpoint addresses change (and may not have resolved at all
		// while the uplink was down): look them up again periodically
		// for as long as the tunnel is not up.
		if state != "up" && (!hasEndpoint || now.Sub(t.resolved) > 30*time.Second) {
			t.resolved = now
			retry = append(retry, t.cfg)
		}
	}
	m.mu.Unlock()
	for _, v := range retry {
		m.reresolve(c, v)
	}
	if changed && m.OnChange != nil {
		m.OnChange()
	}
}

func (m *Manager) reresolve(c *wgctrl.Client, v config.VPN) {
	m.mu.Lock()
	t := m.tunnels[v.ID]
	isErr := t != nil && t.st.State == "error"
	m.mu.Unlock()
	if isErr {
		if err := m.configure(v); err != nil {
			m.mu.Lock()
			if t := m.tunnels[v.ID]; t != nil {
				t.st.Error = err.Error()
			}
			m.mu.Unlock()
		} else if m.OnChange != nil {
			m.OnChange()
		}
		return
	}
	var peers []wgtypes.PeerConfig
	endpoint := ""
	for _, p := range v.WireGuard.Peers {
		if p.Endpoint == "" {
			continue
		}
		pub, err := key(p.PublicKey)
		if err != nil {
			continue
		}
		ep, err := m.resolveEndpoint(p.Endpoint)
		if err != nil {
			continue
		}
		endpoint = ep.String()
		peers = append(peers, wgtypes.PeerConfig{PublicKey: pub, UpdateOnly: true, Endpoint: ep})
	}
	if len(peers) == 0 {
		return
	}
	if err := c.ConfigureDevice(fw.VPNIfName(v.ID), wgtypes.Config{Peers: peers}); err != nil {
		m.Log.Debug("vpn endpoint update failed", "err", err)
		return
	}
	m.mu.Lock()
	if t := m.tunnels[v.ID]; t != nil {
		t.st.Endpoint = endpoint
	}
	m.mu.Unlock()
}

// Statuses returns every tunnel's state, keyed by profile id.
func (m *Manager) Statuses() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Status{}
	for id, t := range m.tunnels {
		out[id] = t.st
	}
	return out
}

// Routes returns the routing-table description for every profile in the
// configuration, in order.
func (m *Manager) Routes(vpns []config.VPN) []fw.VPNRoutes {
	st := m.Statuses()
	var out []fw.VPNRoutes
	for i, v := range vpns {
		r := fw.VPNRoutes{Index: i, If: fw.VPNIfName(v.ID), KillSwitch: v.KillSwitch}
		if s, ok := st[v.ID]; ok && v.Enabled && s.State == "up" {
			r.Up = true
			for _, p := range v.WireGuard.Peers {
				for _, a := range p.AllowedIPs {
					if pf, err := netip.ParsePrefix(a); err == nil {
						r.Allowed = append(r.Allowed, pf.Masked())
					}
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// ParseConf converts a wg-quick style configuration file into a profile.
func ParseConf(text string) (config.WireGuard, error) {
	return parseConf(text)
}

// GenerateKey returns a new private key and its public key (base64).
func GenerateKey() (priv, pub string, err error) {
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return "", "", err
	}
	return k.String(), k.PublicKey().String(), nil
}

// PublicKey derives the public key of a base64 private key.
func PublicKey(priv string) (string, error) {
	k, err := key(priv)
	if err != nil {
		return "", err
	}
	return k.PublicKey().String(), nil
}
