// Package lan manages the client-facing side: one bridge per network,
// their addresses, and dnsmasq for DHCPv4 and IPv6 router advertisements. DNS is not
// dnsmasq's job here; the router's own forwarder answers on port 53.
package lan

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/netx"
	"github.com/ryanzhou/porter/daemon/internal/proc"
)

type Reservation struct {
	MAC string
	IP  string
}

// Net is one network: a bridge, the router's addresses on it, and the
// wired ports that belong to it.
type Net struct {
	Bridge string
	Addr   netip.Prefix // router address with prefix length
	ULA    netip.Prefix // router IPv6 address with /64; zero = no IPv6
	Ports  []string     // wired members
}

// Desired is the LAN state to establish.
type Desired struct {
	// Nets are the networks, the main one first.
	Nets []Net
	// BridgeMAC is the address of the main network's bridge; the others
	// get addresses next to it.
	BridgeMAC    string
	LeaseMinutes int
	Domain       string
	Reservations []Reservation
	// CaptiveURL is passed on to clients (DHCP option 114) while the
	// uplink reports a captive portal API.
	CaptiveURL string
}

type Manager struct {
	Log      *slog.Logger
	RunDir   string
	HookPath string // executable dnsmasq calls on lease changes
	// HookEnv is passed to dnsmasq, and through it to the hook, which
	// has no other way of knowing where to report: dnsmasq starts it by
	// its bare name.
	HookEnv []string
	// Verbose has dnsmasq log every DHCP exchange.
	Verbose bool

	dnsmasq  *proc.Proc
	lastConf string
	swept    bool
	bridges  map[string]bool
	strays   map[string]time.Time // access point interfaces last found outside their bridge
}

// APBridge is consulted so that access point interfaces attached to a
// bridge by hostapd are not thrown out when syncing wired ports. It
// returns the bridge an access point interface belongs to.
var APBridge = func(iface string) (bridge string, ok bool) { return "", false }

// APPorts lists the interfaces of running access points that belong in a
// bridge. They are put (back) into it like wired ports.
var APPorts = func(bridge string) []string { return nil }

// macFor gives the i-th network's bridge its address.
func macFor(base string, i int) string {
	hw, err := net.ParseMAC(base)
	if err != nil || len(hw) != 6 || i == 0 {
		return base
	}
	hw[5] += byte(i)
	return hw.String()
}

// Apply establishes the desired state. addrChanged reports that a network
// was renumbered, in which case clients need to be made to re-request
// their leases.
func (m *Manager) Apply(d Desired) (addrChanged bool, err error) {
	want := map[string]bool{}
	for i, n := range d.Nets {
		want[n.Bridge] = true
		if err := netx.EnsureBridge(n.Bridge, macFor(d.BridgeMAC, i)); err != nil {
			return addrChanged, err
		}
		// The bridge must answer router solicitations itself (dnsmasq
		// does); it must never autoconfigure from someone else's.
		netx.Sysctl("net/ipv6/conf/"+n.Bridge+"/accept_ra", "0")
		addrs := []netip.Prefix{n.Addr}
		if n.ULA.IsValid() && fw.HaveIPv6() {
			addrs = append(addrs, n.ULA)
		}
		changed, err := netx.SyncAddrs(n.Bridge, addrs)
		if err != nil {
			return addrChanged, err
		}
		addrChanged = addrChanged || changed
		bridge := n.Bridge
		keep := func(name string) bool {
			b, ok := APBridge(name)
			return ok && b == bridge
		}
		ports := append([]string(nil), n.Ports...)
		for _, iface := range APPorts(n.Bridge) {
			if !netx.InBridge(iface, n.Bridge) && time.Since(m.strays[iface]) > time.Minute {
				// Said once a minute at most, should putting it back not work.
				if m.strays == nil {
					m.strays = map[string]time.Time{}
				}
				m.strays[iface] = time.Now()
				m.Log.Warn("access point interface was not in its bridge; putting it back", "iface", iface, "bridge", n.Bridge)
			}
			ports = append(ports, iface)
		}
		if err := netx.SetBridgePorts(n.Bridge, ports, keep); err != nil {
			m.Log.Warn("bridge ports", "bridge", n.Bridge, "err", err)
		}
	}
	// Bridges of networks that no longer exist go: those this run made,
	// and once, those an earlier run left behind.
	if !m.swept {
		m.swept = true
		for _, name := range netx.Bridges("br-") {
			if !want[name] {
				netx.DeleteLink(name)
			}
		}
	}
	for name := range m.bridges {
		if !want[name] {
			netx.DeleteLink(name)
		}
	}
	m.bridges = want

	conf := m.dnsmasqConf(d)
	if conf != m.lastConf || m.dnsmasq == nil {
		path := filepath.Join(m.RunDir, "dnsmasq.conf")
		if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
			return addrChanged, err
		}
		if m.dnsmasq != nil {
			m.dnsmasq.Stop()
		}
		m.dnsmasq = &proc.Proc{Name: "dnsmasq", Log: m.Log, Argv: []string{"dnsmasq", "--keep-in-foreground", "--conf-file=" + path}, Env: m.HookEnv}
		m.dnsmasq.Start()
		m.lastConf = conf
	}
	return addrChanged, nil
}

func (m *Manager) Stop() {
	if m.dnsmasq != nil {
		m.dnsmasq.Stop()
		m.dnsmasq = nil
	}
}

// Pool returns the DHCP range for a LAN prefix: the upper part of the
// subnet, leaving the low addresses for static use.
func Pool(p netip.Prefix) (start, end netip.Addr) {
	p = p.Masked()
	base := p.Addr().As4()
	n := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	size := uint32(1) << (32 - p.Bits())
	hosts := size - 2
	reserve := hosts / 4
	if reserve > 49 {
		reserve = 49
	}
	mk := func(v uint32) netip.Addr {
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
	return mk(n + 1 + reserve), mk(n + size - 2)
}

func (m *Manager) dnsmasqConf(d Desired) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("# generated by porterd")
	w("port=0")
	for _, n := range d.Nets {
		w("interface=%s", n.Bridge)
	}
	w("bind-dynamic")
	w("except-interface=lo")
	w("no-resolv")
	w("no-hosts")
	w("keep-in-foreground")
	w("log-facility=-")
	if !m.Verbose {
		w("quiet-dhcp")
		w("quiet-dhcp6")
		w("quiet-ra")
	}
	if _, err := user.Lookup("dnsmasq"); err != nil {
		w("user=root")
	}
	w("dhcp-authoritative")
	w("dhcp-leasefile=%s", filepath.Join(m.RunDir, "dnsmasq.leases"))
	if m.HookPath != "" {
		w("dhcp-script=%s", m.HookPath)
	}
	w("domain=%s", d.Domain)
	ra := false
	for i, n := range d.Nets {
		// Each network's clients are told about that network's router
		// and resolver: the tag ties the options to the address range.
		tag := fmt.Sprintf("n%d", i)
		router := n.Addr.Addr()
		start, end := Pool(n.Addr)
		mask := netip.PrefixFrom(netip.AddrFrom4([4]byte{255, 255, 255, 255}), n.Addr.Bits()).Masked().Addr()
		w("dhcp-range=set:%s,%s,%s,%s,%dm", tag, start, end, mask, d.LeaseMinutes)
		w("dhcp-option=tag:%s,option:router,%s", tag, router)
		w("dhcp-option=tag:%s,option:dns-server,%s", tag, router)
		if n.ULA.IsValid() && fw.HaveIPv6() {
			if !ra {
				w("enable-ra")
				ra = true
			}
			w("dhcp-range=set:%s,::,constructor:%s,ra-stateless,64,%dm", tag, n.Bridge, d.LeaseMinutes)
			w("dhcp-option=tag:%s,option6:dns-server,[%s]", tag, n.ULA.Addr())
			w("ra-param=%s,30,1800", n.Bridge)
		}
	}
	if d.CaptiveURL != "" && !strings.ContainsAny(d.CaptiveURL, ",\n\r ") {
		w("dhcp-option=114,%s", d.CaptiveURL)
	}
	for _, r := range d.Reservations {
		w("dhcp-host=%s,%s", r.MAC, r.IP)
	}
	return b.String()
}

// Renumber returns an address for a network that does not overlap any of
// the given subnets (the upstream networks, and the router's other
// networks), or ok=false when the current one is already fine.
func Renumber(current netip.Prefix, avoid []netip.Prefix) (next netip.Prefix, ok bool) {
	overlaps := false
	for _, u := range avoid {
		if u.Masked().Overlaps(current.Masked()) {
			overlaps = true
		}
	}
	if !overlaps {
		return current, false
	}
	if p, found := config.FreeSubnet(avoid); found {
		return p, true
	}
	return current, false
}

// Remap moves an address from one subnet to another keeping its host part.
func Remap(a netip.Addr, from, to netip.Prefix) (netip.Addr, bool) {
	if !a.Is4() || !from.Contains(a) {
		return a, false
	}
	hostBits := 32 - from.Bits()
	if 32-to.Bits() < hostBits {
		return a, false
	}
	a4, t4 := a.As4(), to.Masked().Addr().As4()
	av := uint32(a4[0])<<24 | uint32(a4[1])<<16 | uint32(a4[2])<<8 | uint32(a4[3])
	tv := uint32(t4[0])<<24 | uint32(t4[1])<<16 | uint32(t4[2])<<8 | uint32(t4[3])
	host := av & (1<<hostBits - 1)
	v := tv | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}
