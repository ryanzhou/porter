package vpn

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/netx"
)

// ServerSpec is the router's own WireGuard server as it should run: the
// other end of what the profiles above are. Devices connect to it from
// outside; what they may do once connected is the firewall's business.
type ServerSpec struct {
	PrivateKey string
	Port       int
	// Addrs are the router's addresses in the server's ranges, with the
	// ranges' prefix lengths: the route to the devices comes with them.
	Addrs []netip.Prefix
	Peers []ServerPeer
}

// ServerPeer is a device that may connect, and the addresses that are
// its own: anything it sends from another is dropped.
type ServerPeer struct {
	PublicKey string
	Addrs     []netip.Prefix
}

// PeerStatus is how one device's connection stands.
type PeerStatus struct {
	// Connected: the device and the router have exchanged keys within
	// the last three minutes, which they do every two while either has
	// something to send.
	Connected bool `json:"connected"`
	// Endpoint is the address the device last connected from.
	Endpoint  string `json:"endpoint,omitempty"`
	Handshake int64  `json:"handshake,omitempty"` // unix time of the last key exchange
	Rx        int64  `json:"rx"`                  // bytes received from the device
	Tx        int64  `json:"tx"`                  // bytes sent to it
}

// Server keeps the server's interface (fw.ServerIf) as the last ServerSpec
// says.
type Server struct {
	Log *slog.Logger
	// OnChange is called when a device connects or is gone.
	OnChange func()
	// UserspaceImpl is as for Manager.
	UserspaceImpl string

	mu      sync.Mutex
	applied string // fingerprint of the spec the interface was last given
	err     string
	cmd     *exec.Cmd
	peers   map[string]PeerStatus // by public key
	stop    chan struct{}
}

// Apply brings the server in line with spec; nil switches it off. It is
// called on every pass of the control loop, and tries again by itself
// what did not work the last time.
func (s *Server) Apply(spec *ServerSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if spec == nil {
		s.down()
		return
	}
	key := fmt.Sprintf("%+v", *spec)
	if key == s.applied && s.err == "" && netx.Exists(fw.ServerIf) {
		return
	}
	if err := s.configure(spec); err != nil {
		if s.err != err.Error() {
			s.Log.Error("WireGuard server", "err", err)
		}
		s.err = err.Error()
		return
	}
	if s.applied == "" || s.err != "" {
		s.Log.Info("WireGuard server listening", "port", spec.Port, "devices", len(spec.Peers))
	}
	s.applied, s.err = key, ""
	if s.stop == nil {
		s.stop = make(chan struct{})
		go s.loop(s.stop)
	}
}

// Stop takes the server down.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down()
}

// down removes the interface; s.mu is held.
func (s *Server) down() {
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	if s.applied != "" {
		s.Log.Info("WireGuard server switched off")
	}
	s.applied, s.err, s.peers = "", "", nil
	if l, err := netlink.LinkByName(fw.ServerIf); err == nil {
		netlink.LinkDel(l)
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}
	s.cmd = nil
}

// userspaceLink starts a userspace WireGuard for an interface and waits
// for the interface to be there.
func userspaceLink(impl, name string) (*exec.Cmd, error) {
	cmd := exec.Command(impl, "-f", name)
	cmd.Env = append(os.Environ(), "WG_PROCESS_FOREGROUND=1")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	for i := 0; i < 50; i++ {
		if netx.Exists(name) {
			return cmd, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Kill()
	cmd.Wait()
	return nil, fmt.Errorf("userspace WireGuard did not create %s", name)
}

func ipNet(p netip.Prefix) net.IPNet {
	p = p.Masked()
	return net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

// configure makes the interface if it is not there and gives it its key,
// port, devices and addresses; s.mu is held.
func (s *Server) configure(spec *ServerSpec) error {
	name := fw.ServerIf
	if !netx.Exists(name) {
		if s.UserspaceImpl == "" {
			la := netlink.NewLinkAttrs()
			la.Name = name
			if err := netlink.LinkAdd(&netlink.GenericLink{LinkAttrs: la, LinkType: "wireguard"}); err != nil {
				return fmt.Errorf("create interface: %w", err)
			}
		} else {
			cmd, err := userspaceLink(s.UserspaceImpl, name)
			if err != nil {
				return fmt.Errorf("create interface: %w", err)
			}
			s.cmd = cmd
		}
	}
	priv, err := key(spec.PrivateKey)
	if err != nil {
		return err
	}
	port := spec.Port
	// The server's own packets carry no mark: like everything the router
	// sends for itself they follow the main table and the uplink in use,
	// never a profile's table, which only marked packets are sent to.
	wcfg := wgtypes.Config{PrivateKey: &priv, ListenPort: &port, ReplacePeers: true}
	for _, p := range spec.Peers {
		pub, err := key(p.PublicKey)
		if err != nil {
			return err
		}
		pc := wgtypes.PeerConfig{PublicKey: pub, ReplaceAllowedIPs: true}
		for _, a := range p.Addrs {
			if a.Addr().Is6() && !fw.HaveIPv6() {
				continue
			}
			pc.AllowedIPs = append(pc.AllowedIPs, ipNet(a))
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
	if link.Attrs().MTU != fw.ServerMTU {
		if err := netlink.LinkSetMTU(link, fw.ServerMTU); err != nil {
			return err
		}
	}
	var addrs []netip.Prefix
	for _, a := range spec.Addrs {
		if a.Addr().Is6() && !fw.HaveIPv6() {
			continue
		}
		addrs = append(addrs, a)
	}
	if _, err := netx.SyncAddrs(name, addrs); err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}

func (s *Server) loop(stop chan struct{}) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.poll(stop)
		}
	}
}

// poll reads how the devices' connections stand.
func (s *Server) poll(stop chan struct{}) {
	c, err := wgctrl.New()
	if err != nil {
		return
	}
	defer c.Close()
	dev, err := c.Device(fw.ServerIf)
	if err != nil {
		return
	}
	peers := map[string]PeerStatus{}
	for _, p := range dev.Peers {
		st := PeerStatus{Rx: p.ReceiveBytes, Tx: p.TransmitBytes}
		if !p.LastHandshakeTime.IsZero() {
			st.Handshake = p.LastHandshakeTime.Unix()
			st.Connected = time.Since(p.LastHandshakeTime) < handshakeFresh
		}
		if p.Endpoint != nil {
			st.Endpoint = p.Endpoint.String()
		}
		peers[p.PublicKey.String()] = st
	}
	s.mu.Lock()
	if s.stop != stop {
		// Switched off, or on again, while this was being read.
		s.mu.Unlock()
		return
	}
	changed := len(peers) != len(s.peers)
	for k, p := range peers {
		changed = changed || p.Connected != s.peers[k].Connected || p.Endpoint != s.peers[k].Endpoint
	}
	s.peers = peers
	s.mu.Unlock()
	if changed && s.OnChange != nil {
		s.OnChange()
	}
}

// Status says whether the server is running ("off", "up" or "error", with
// the reason) and how each device's connection stands, by public key.
func (s *Server) Status() (state, problem string, peers map[string]PeerStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	peers = map[string]PeerStatus{}
	for k, p := range s.peers {
		peers[k] = p
	}
	switch {
	case s.err != "":
		return "error", s.err, peers
	case s.applied != "":
		return "up", "", peers
	}
	return "off", "", peers
}

// ClientConf is the configuration of one device that connects to the
// server, in the form the WireGuard apps import and show as a QR code.
type ClientConf struct {
	// PrivateKey is the device's own, made for it and kept nowhere else.
	PrivateKey string
	Addrs      []netip.Prefix
	DNS        []netip.Addr
	// ServerKey is the server's public key, Endpoint where it is reached
	// (host:port), Allowed what the device sends through it.
	ServerKey string
	Endpoint  string
	Allowed   []netip.Prefix
}

// String writes the configuration. ParseConf reads it back.
func (c ClientConf) String() string {
	join := func(n int, at func(int) string) string {
		s := ""
		for i := 0; i < n; i++ {
			if i > 0 {
				s += ", "
			}
			s += at(i)
		}
		return s
	}
	return "[Interface]\n" +
		"PrivateKey = " + c.PrivateKey + "\n" +
		"Address = " + join(len(c.Addrs), func(i int) string { return c.Addrs[i].String() }) + "\n" +
		"DNS = " + join(len(c.DNS), func(i int) string { return c.DNS[i].String() }) + "\n" +
		"\n[Peer]\n" +
		"PublicKey = " + c.ServerKey + "\n" +
		"Endpoint = " + c.Endpoint + "\n" +
		"AllowedIPs = " + join(len(c.Allowed), func(i int) string { return c.Allowed[i].String() }) + "\n" +
		// Keeps the way back open through whatever the device is behind,
		// so that it can be reached from home as well.
		"PersistentKeepalive = 25\n"
}
