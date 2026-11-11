// Package mpquic runs "mpquic local" for VPN profiles of that type.
//
// MPQUIC (github.com/ryanzhou/mpquic) is a multipath QUIC proxy: its local
// side reaches a remote over several paths at once. It is used here as it
// is shipped, as a separate program, so that a newer mpquic can replace
// the one in the image without anything else changing.
//
// The local side proxies whatever the system routes to a network (TUN)
// interface it is attached to. That is how the router feeds it: each
// profile has an interface, like a WireGuard profile has one, and one
// mpquic attached to it. Policy routing, the kill switch, per-path DNS
// and IPv6 then work for an MPQUIC tunnel exactly as for any other;
// nothing downstream knows the difference. The interface belongs to the
// router (package vpn makes it, and keeps it while mpquic restarts), so
// that addresses, routes and firewall rules outlive the process, and
// what is routed to it while no mpquic is attached goes nowhere.
package mpquic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/proc"
)

// MTU of a profile's interface. TCP ends at the interface and is carried
// as a stream from there, so nothing is taken from the packet size.
const MTU = 1500

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
	proc    *proc.Proc
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
	TUN         fileTUN  `json:"tun"`
}

// fileTUN names the interface to attach to. It has no addresses or
// routes here: those are the router's to set.
type fileTUN struct {
	Name string `json:"name"`
	MTU  int    `json:"mtu"`
}

func file(v config.VPN) fileConfig {
	return fileConfig{
		Server: v.MPQUIC.Servers, ServerPort: v.MPQUIC.Port, Password: v.MPQUIC.Password,
		// UDP is always relayed: without it clients behind the tunnel
		// would have no DNS and no QUIC.
		Mode: "tcp_and_udp", NoSOCKS: true,
		Up: v.MPQUIC.Up, Down: v.MPQUIC.Down, ReliableUDP: v.MPQUIC.ReliableUDP,
		TUN: fileTUN{Name: fw.VPNIfName(v.ID), MTU: MTU},
	}
}

func spec(v config.VPN) string {
	// What mpquic is started with. The DNS servers are not part of it:
	// they are the router's business.
	q := v.MPQUIC
	q.DNS = nil
	return fmt.Sprintf("%+v", q)
}

func wanted(vpns []config.VPN) map[string]config.VPN {
	want := map[string]config.VPN{}
	for _, v := range vpns {
		if v.Enabled && v.Type == config.VPNMPQUIC {
			want[v.ID] = v
		}
	}
	return want
}

// Retire stops the mpquics that are no longer wanted or whose settings
// changed. It is for calling before the interfaces of removed profiles
// are taken away; Apply does the same by itself.
func (m *Manager) Retire(vpns []config.VPN) {
	want := wanted(vpns)
	m.mu.Lock()
	var stop []*proc.Proc
	for id, in := range m.inst {
		if w, ok := want[id]; ok && in.spec == spec(w) {
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
}

// Apply starts an mpquic for every enabled MPQUIC profile and stops the
// ones no longer wanted. A profile whose settings changed is restarted.
// The profiles' interfaces have to exist by now.
func (m *Manager) Apply(vpns []config.VPN) {
	m.Retire(vpns)
	want := wanted(vpns)
	m.mu.Lock()
	if m.inst == nil {
		m.inst = map[string]*instance{}
	}
	for id := range m.inst {
		delete(want, id)
	}
	m.mu.Unlock()
	if len(want) == 0 {
		return
	}
	bin := m.Binary()
	for id, v := range want {
		in := &instance{spec: spec(v)}
		m.mu.Lock()
		m.inst[id] = in
		m.mu.Unlock()
		if bin == "" {
			m.Log.Error("mpquic: the mpquic program is not installed", "looked_in", strings.Join(m.Binaries, ", "))
			continue
		}
		// The file holds the tunnel's password: for root only, and in RAM.
		path := filepath.Join(m.RunDir, "mpquic-"+id+".json")
		raw, _ := json.Marshal(file(v))
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			m.Log.Error("mpquic: writing its configuration", "err", err)
			continue
		}
		id := id
		in.proc = &proc.Proc{
			Name: "mpquic-" + id, Log: m.Log,
			Argv:   []string{bin, "local", "-c", path},
			OnLine: func(line string) { m.onLine(id, in, line) },
			// A process that has gone has no paths, whatever it last
			// said.
			OnExit: func(error) { m.setPaths(id, in, 0) },
		}
		in.started = true
		m.Log.Info("mpquic starting", "vpn", v.Name, "paths", len(v.MPQUIC.Servers), "program", bin)
		in.proc.Start()
	}
}

// onLine follows mpquic's log for the number of working paths.
func (m *Manager) onLine(id string, in *instance, line string) {
	if count, ok := pathCount(line); ok {
		m.setPaths(id, in, count)
	}
}

func (m *Manager) setPaths(id string, in *instance, count int) {
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
// them: an MPQUIC profile is given the settings of its interface in the
// place where a WireGuard profile has them. Other profiles are passed
// through, and the stored ones are left as they are.
func Effective(vpns []config.VPN) []config.VPN {
	out := make([]config.VPN, len(vpns))
	copy(out, vpns)
	for i := range out {
		v := &out[i]
		if v.Type != config.VPNMPQUIC {
			continue
		}
		v.WireGuard = config.WireGuard{
			// Any private addresses do: they are what traffic for the
			// tunnel is sent from, and mpquic proxies whatever comes in.
			Addresses: []string{fmt.Sprintf("10.66.%d.2/32", i), fmt.Sprintf("fd66:%x::2/128", i)},
			DNS:       v.MPQUIC.DNS,
			MTU:       MTU,
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
