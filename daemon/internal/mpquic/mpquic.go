// Package mpquic runs "mpquic local" for VPN profiles of that type.
//
// MPQUIC (github.com/ryanzhou/mpquic) is a multipath QUIC proxy: its local
// side reaches a remote over several paths at once. It is used here as it
// is shipped, as a separate program, so that a newer mpquic can replace
// the one in the image without anything else changing.
//
// The local side can act as a WireGuard server whose clients' traffic it
// proxies. That is how the router feeds it: for each profile it starts
// one mpquic listening for WireGuard on a loopback port, and hands the
// rest of the router an ordinary WireGuard profile pointing at that port.
// Policy routing, the kill switch, per-path DNS and IPv6 then work for an
// MPQUIC tunnel exactly as for any other; nothing downstream knows the
// difference. The WireGuard keys exist only for this hop inside the
// machine and are made afresh at every start.
package mpquic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/proc"
)

const (
	// basePort is the loopback port of the first profile's WireGuard
	// listener; a profile's place in the configuration is added to it.
	basePort = 47800
	// mtu of the WireGuard hop. It never leaves the machine, so there is
	// no smaller path to fit; a full-size packet from the LAN goes in
	// whole.
	mtu = 1500
)

// Manager supervises one mpquic per enabled MPQUIC profile.
type Manager struct {
	Log    *slog.Logger
	RunDir string
	// Binaries are the places to look for the mpquic program, in order
	// of preference; the first that exists is used.
	Binaries []string
	// OnChange is called when a tunnel gains its first path or loses its
	// last.
	OnChange func()

	mu   sync.Mutex
	inst map[string]*instance
}

type instance struct {
	spec    string // what the process was started for
	index   int
	proc    *proc.Proc
	server  wgtypes.Key // mpquic's WireGuard key
	client  wgtypes.Key // the router's
	paths   int
	started bool
}

// Binary returns the mpquic program to run, or "" if there is none.
func (m *Manager) Binary() string {
	for _, b := range m.Binaries {
		if info, err := os.Stat(b); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return b
		}
	}
	return ""
}

func port(index int) int { return basePort + index }

// fileConfig is mpquic's own configuration file, as far as it is used.
type fileConfig struct {
	Server      []string `json:"server"`
	ServerPort  int      `json:"server_port"`
	Password    string   `json:"password"`
	Mode        string   `json:"mode"`
	NoSOCKS     bool     `json:"no_socks"`
	Up          string   `json:"up,omitempty"`
	Down        string   `json:"down,omitempty"`
	ReliableUDP bool     `json:"reliable_udp,omitempty"`
	WireGuard   fileWG   `json:"wireguard"`
}

type fileWG struct {
	Listen     string     `json:"listen"`
	PrivateKey string     `json:"private_key"`
	MTU        int        `json:"mtu"`
	Peers      []filePeer `json:"peers"`
}

type filePeer struct {
	PublicKey string `json:"public_key"`
}

func (i *instance) file(v config.VPN) fileConfig {
	return fileConfig{
		Server: v.MPQUIC.Servers, ServerPort: v.MPQUIC.Port, Password: v.MPQUIC.Password,
		// UDP is always relayed: without it clients behind the tunnel
		// would have no DNS and no QUIC.
		Mode: "tcp_and_udp", NoSOCKS: true,
		Up: v.MPQUIC.Up, Down: v.MPQUIC.Down, ReliableUDP: v.MPQUIC.ReliableUDP,
		WireGuard: fileWG{
			Listen: net.JoinHostPort("127.0.0.1", strconv.Itoa(port(i.index))), PrivateKey: i.server.String(), MTU: mtu,
			Peers: []filePeer{{PublicKey: i.client.PublicKey().String()}},
		},
	}
}

// Apply starts an mpquic for every enabled MPQUIC profile and stops the
// ones no longer wanted. A profile whose settings changed is restarted.
func (m *Manager) Apply(vpns []config.VPN) {
	type wanted struct {
		v     config.VPN
		index int
	}
	want := map[string]wanted{}
	for i, v := range vpns {
		if v.Enabled && v.Type == config.VPNMPQUIC {
			want[v.ID] = wanted{v, i}
		}
	}
	m.mu.Lock()
	if m.inst == nil {
		m.inst = map[string]*instance{}
	}
	var stop []*proc.Proc
	for id, in := range m.inst {
		w, ok := want[id]
		if ok && in.spec == fmt.Sprintf("%d %+v", w.index, w.v.MPQUIC) {
			delete(want, id)
			continue
		}
		if in.proc != nil {
			stop = append(stop, in.proc)
		}
		delete(m.inst, id)
	}
	m.mu.Unlock()
	for _, p := range stop {
		p.Stop()
	}
	if len(want) == 0 {
		return
	}
	bin := m.Binary()
	for id, w := range want {
		in := &instance{spec: fmt.Sprintf("%d %+v", w.index, w.v.MPQUIC), index: w.index}
		var err error
		if in.server, err = wgtypes.GeneratePrivateKey(); err == nil {
			in.client, err = wgtypes.GeneratePrivateKey()
		}
		if err != nil {
			m.Log.Error("mpquic: making keys", "err", err)
			continue
		}
		m.mu.Lock()
		m.inst[id] = in
		m.mu.Unlock()
		if bin == "" {
			m.Log.Error("mpquic: the mpquic program is not installed", "looked_in", strings.Join(m.Binaries, ", "))
			continue
		}
		// The file holds the tunnel's password: for root only, and in RAM.
		path := filepath.Join(m.RunDir, "mpquic-"+id+".json")
		raw, _ := json.Marshal(in.file(w.v))
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			m.Log.Error("mpquic: writing its configuration", "err", err)
			continue
		}
		id := id
		in.proc = &proc.Proc{
			Name: "mpquic-" + id, Log: m.Log,
			Argv:   []string{bin, "local", "-c", path},
			OnLine: func(line string) { m.onLine(id, in, line) },
		}
		in.started = true
		m.Log.Info("mpquic starting", "vpn", w.v.Name, "paths", len(w.v.MPQUIC.Servers), "program", bin)
		in.proc.Start()
	}
}

// onLine follows mpquic's log for the number of working paths.
func (m *Manager) onLine(id string, in *instance, line string) {
	count, ok := pathCount(line)
	if !ok {
		return
	}
	m.mu.Lock()
	// A process that was replaced may still be writing.
	if m.inst[id] != in {
		m.mu.Unlock()
		return
	}
	before := in.paths
	in.paths = count
	m.mu.Unlock()
	if (before == 0) != (count == 0) {
		m.Log.Info("mpquic paths", "vpn", id, "working", count)
		if m.OnChange != nil {
			m.OnChange()
		}
	}
}

// pathCount reads the number of working paths from a line of mpquic's
// log: it says so whenever a path comes up or goes down, and a fresh
// start has none.
func pathCount(line string) (int, bool) {
	switch {
	case strings.Contains(line, `msg="path up"`), strings.Contains(line, `msg="path down"`):
		i := strings.LastIndex(line, " count=")
		if i < 0 {
			return 0, false
		}
		digits := line[i+len(" count="):]
		if j := strings.IndexAny(digits, " \t\r\n"); j >= 0 {
			digits = digits[:j]
		}
		n, err := strconv.Atoi(digits)
		return n, err == nil && n >= 0
	case strings.Contains(line, `msg="mpquic local started"`):
		return 0, true
	}
	return 0, false
}

// Effective returns the profiles as the rest of the router should see
// them: an MPQUIC profile that is running is given the WireGuard settings
// of the hop to its mpquic. Other profiles are passed through.
func (m *Manager) Effective(vpns []config.VPN) []config.VPN {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]config.VPN, len(vpns))
	copy(out, vpns)
	for i := range out {
		v := &out[i]
		if v.Type != config.VPNMPQUIC {
			continue
		}
		in := m.inst[v.ID]
		if in == nil {
			// Not running: nothing for a WireGuard interface to talk
			// to. The profile keeps its place, switched off.
			v.Enabled = false
			continue
		}
		v.WireGuard = config.WireGuard{
			PrivateKey: in.client.String(),
			// Any private addresses do: mpquic proxies whatever comes
			// in and does not route between clients.
			Addresses: []string{fmt.Sprintf("10.66.%d.2/32", in.index), fmt.Sprintf("fd66:%x::2/128", in.index)},
			DNS:       v.MPQUIC.DNS,
			MTU:       mtu,
			Peers: []config.WGPeer{{
				PublicKey:  in.server.PublicKey().String(),
				Endpoint:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port(in.index))),
				AllowedIPs: []string{"0.0.0.0/0", "::/0"},
				Keepalive:  25,
			}},
		}
	}
	return out
}

// Health reports on an MPQUIC profile's tunnel: how many of its paths
// work. managed is false for a profile this package does not run.
func (m *Manager) Health(id string) (paths int, managed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.inst[id]
	if in == nil {
		return 0, false
	}
	return in.paths, true
}

// Stop ends every mpquic.
func (m *Manager) Stop() {
	m.mu.Lock()
	var stop []*proc.Proc
	for _, in := range m.inst {
		if in.proc != nil {
			stop = append(stop, in.proc)
		}
	}
	m.inst = map[string]*instance{}
	m.mu.Unlock()
	for _, p := range stop {
		p.Stop()
	}
}
