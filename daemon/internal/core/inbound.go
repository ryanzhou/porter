package core

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/ddns"
	"github.com/ryanzhou/porter/daemon/internal/devices"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/proto"
	"github.com/ryanzhou/porter/daemon/internal/vpn"
	"github.com/ryanzhou/porter/daemon/internal/wan"
)

// What comes in from outside: the WireGuard server, forwarded ports, and
// the name and address both are found by.

// InboundStatus says whether and where the router can be reached from the
// internet, and how what is set up to come in stands.
type InboundStatus struct {
	// Reach is how the router can be reached over IPv4: "direct", "nat"
	// (behind another router), "cgnat" (the provider shares its public
	// address) or "offline". See ddns.Classify.
	Reach string `json:"reach"`
	// Address is the IPv4 address the internet source in use has, Public
	// the one the internet sees (empty while that is not known), and
	// Public6 the router's own public IPv6 address, if it has one.
	Address string `json:"address,omitempty"`
	Public  string `json:"public,omitempty"`
	Public6 string `json:"public6,omitempty"`
	// Host is what a device outside is told to connect to: the dynamic
	// DNS name, else a public address. Empty when there is neither.
	Host     string          `json:"host,omitempty"`
	Server   ServerStatus    `json:"server"`
	Forwards []ForwardStatus `json:"forwards"`
	DDNS     ddns.Status     `json:"ddns"`
}

// ServerStatus is the WireGuard server's.
type ServerStatus struct {
	// State is "off", "up" or "error".
	State     string       `json:"state"`
	Error     string       `json:"error,omitempty"`
	PublicKey string       `json:"public_key"`
	Peers     []PeerStatus `json:"peers"`
}

// PeerStatus is one device's connection to the server.
type PeerStatus struct {
	ID string `json:"id"`
	vpn.PeerStatus
}

// ForwardStatus is where a forwarded port leads at the moment.
type ForwardStatus struct {
	ID string `json:"id"`
	// To is the device's IPv4 address, and To6 its IPv6 addresses the
	// port is open to: its own, out of a prefix delegated to the router.
	To  string   `json:"to,omitempty"`
	To6 []string `json:"to6"`
}

// addressCheck is what the address check last answered, and for which
// address of the router's.
type addressCheck struct {
	own, seen netip.Addr
}

// ownAddr4 is the IPv4 address of the internet source in use.
func ownAddr4(active *wan.State) netip.Addr {
	if active == nil {
		return netip.Addr{}
	}
	if p, err := netip.ParsePrefix(active.IPv4); err == nil {
		return p.Addr()
	}
	return netip.Addr{}
}

// publicAddr6 is an address of the router's that the internet can send
// to: one the internet source in use has itself, or else the router's in
// the main network's share of a delegated prefix.
func (c *Core) publicAddr6(active *wan.State) netip.Addr {
	if active == nil || !active.HasV6 {
		return netip.Addr{}
	}
	for _, s := range active.IPv6 {
		a, err := netip.ParseAddr(strings.Split(s, "/")[0])
		if err == nil && a.Is6() && a.IsGlobalUnicast() && !a.IsPrivate() {
			return a
		}
	}
	if g, ok := c.global[config.MainNetwork]; ok {
		return g.Addr()
	}
	return netip.Addr{}
}

// forwardAddrs6 collects the IPv6 addresses of the devices that have a
// port forwarded to them, as the device table has last seen them.
func forwardAddrs6(cfg *config.Config, devs []devices.Entry) map[string][]netip.Addr {
	out := map[string][]netip.Addr{}
	for _, f := range cfg.Inbound.Forwards {
		for _, d := range devs {
			if d.MAC != f.Device || out[d.MAC] != nil {
				continue
			}
			for _, s := range d.IPv6 {
				if a, err := netip.ParseAddr(s); err == nil {
					out[d.MAC] = append(out[d.MAC], a)
				}
			}
		}
	}
	return out
}

// inboundStatus must run on the control loop.
func (c *Core) inboundStatus(cfg *config.Config, active *wan.State) InboundStatus {
	in := cfg.Inbound
	own := ownAddr4(active)
	var seen netip.Addr
	if c.reach.own == own {
		seen = c.reach.seen
	}
	st := InboundStatus{Reach: ddns.Classify(own, seen), Forwards: []ForwardStatus{}, DDNS: c.ddns.Status()}
	if own.IsValid() {
		st.Address = own.String()
	}
	switch {
	case seen.IsValid():
		st.Public = seen.String()
	case st.Reach == ddns.ReachDirect:
		st.Public = own.String()
	}
	if a := c.publicAddr6(active); a.IsValid() {
		st.Public6 = a.String()
	}
	switch {
	case in.DDNS.Enabled && in.DDNS.Hostname != "":
		st.Host = in.DDNS.Hostname
	case st.Public != "" && st.Reach != ddns.ReachCGNAT:
		// Behind another router this is that router's address: it leads
		// here once that router passes on what arrives there.
		st.Host = st.Public
	default:
		st.Host = st.Public6
	}
	state, problem, peers := c.srv.Status()
	st.Server = ServerStatus{State: state, Error: problem, PublicKey: in.Server.PublicKey(), Peers: []PeerStatus{}}
	for _, p := range in.Server.Peers {
		st.Server.Peers = append(st.Server.Peers, PeerStatus{ID: p.ID, PeerStatus: peers[p.PublicKey]})
	}
	for _, f := range c.forwards {
		fs := ForwardStatus{ID: f.ID, To: f.To4.String(), To6: []string{}}
		for _, a := range f.To6 {
			fs.To6 = append(fs.To6, a.String())
		}
		st.Forwards = append(st.Forwards, fs)
	}
	return st
}

// applyInbound runs the WireGuard server as configured and tells the
// dynamic DNS updater which addresses the name is to have.
func (c *Core) applyInbound(cfg *config.Config, active *wan.State) {
	s := cfg.Inbound.Server
	var spec *vpn.ServerSpec
	if s.Enabled {
		spec = &vpn.ServerSpec{PrivateKey: s.PrivateKey, Port: s.Port}
		a, p := s.Prefix()
		spec.Addrs = append(spec.Addrs, netip.PrefixFrom(a, p.Bits()))
		if cfg.LAN.IPv6 {
			a, p := s.ULA()
			spec.Addrs = append(spec.Addrs, netip.PrefixFrom(a, p.Bits()))
		}
		for _, peer := range s.Peers {
			// A device that is switched off is not known to the server at
			// all: its connection ends with the next packet.
			if peer.Enabled {
				spec.Peers = append(spec.Peers, vpn.ServerPeer{PublicKey: peer.PublicKey, Addrs: s.PeerAddrs(peer, cfg.LAN.IPv6)})
			}
		}
	}
	c.srv.Apply(spec)

	// The name gets the address the internet reaches the router at. Behind
	// a provider's translation there is none over IPv4.
	st := c.inboundStatus(cfg, active)
	var ip4, ip6 netip.Addr
	if st.Reach != ddns.ReachCGNAT {
		ip4, _ = netip.ParseAddr(st.Public)
	}
	ip6, _ = netip.ParseAddr(st.Public6)
	c.ddns.Want(cfg.Inbound.DDNS, ip4, ip6)
}

// serverAddrs returns the router's addresses in the WireGuard server's
// ranges and the ranges, while the server runs.
func (c *Core) serverAddrs(cfg *config.Config) (addrs []netip.Addr, nets []netip.Prefix) {
	s := cfg.Inbound.Server
	if state, _, _ := c.srv.Status(); !s.Enabled || state != "up" {
		return nil, nil
	}
	a, p := s.Prefix()
	addrs, nets = append(addrs, a), append(nets, p)
	if cfg.LAN.IPv6 && fw.HaveIPv6() {
		a, p := s.ULA()
		addrs, nets = append(addrs, a), append(nets, p)
	}
	return addrs, nets
}

// runAddressCheck asks, while anything is set up to come in from outside,
// which address the internet sees the router at: when the internet source
// or its address changes, and every ten minutes, since behind another
// router the public address can change without anything here changing.
func (c *Core) runAddressCheck(ctx context.Context) {
	const every = 10 * time.Minute
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	var next time.Time
	var askedURL string
	var asked netip.Addr
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-c.reachNow:
			next = time.Time{}
		}
		var url string
		var own netip.Addr
		c.do(func() {
			if c.cfg.Inbound.InUse() {
				url = c.cfg.Inbound.CheckURL
			}
			own = ownAddr4(c.active(c.wan.States()))
		})
		if url == "" || !own.IsValid() || (own == asked && url == askedURL && time.Now().Before(next)) {
			continue
		}
		seen, err := ddns.PublicAddr(ctx, url)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fails++
			next = time.Now().Add(min(time.Minute<<min(fails-1, 4), every))
			c.log.Debug("asking for the router's public address", "err", err)
		} else {
			fails, next = 0, time.Now().Add(every)
		}
		changed := false
		c.do(func() {
			// An answer that did not come says nothing against the last
			// one, as long as it was about the same address.
			if err == nil || c.reach.own != own {
				changed = c.reach != addressCheck{own, seen}
				c.reach = addressCheck{own, seen}
			}
		})
		asked, askedURL = own, url
		if changed {
			c.log.Info("the router's public address", "own", own, "seen", seen, "reach", ddns.Classify(own, seen))
			c.Kick()
		}
	}
}

// pinForwards gives the device of a forwarded port a fixed address where
// it has none: the address it has at the moment. A forward names its
// device, and IPv4 connections are sent to an address; this is what keeps
// the two together when the device's lease would have run out. A device
// that has no address at the moment is left alone, and validation then
// says what is missing.
func (c *Core) pinForwards(next *config.Config) {
	var devs []devices.Entry
	for _, f := range next.Inbound.Forwards {
		if d := next.Device(f.Device); d != nil && d.IP != "" {
			continue
		}
		if devs == nil {
			devs = c.devs.List()
		}
		for _, e := range devs {
			a, err := netip.ParseAddr(e.IPv4)
			if e.MAC != f.Device || err != nil || next.NetworkOf(a) == nil {
				continue
			}
			if d := next.Device(f.Device); d != nil {
				d.IP = a.String()
			} else {
				next.Devices = append(next.Devices, config.Device{MAC: f.Device, IP: a.String()})
			}
		}
	}
}

// forwardedTo names a forward that goes to a device, "" if none does.
func forwardedTo(cfg *config.Config, mac string) string {
	for _, f := range cfg.Inbound.Forwards {
		if f.Device == mac {
			return f.Name
		}
	}
	return ""
}

// PeerAdded is the answer to "server.peer_add": the device's entry and
// its configuration, private key included. The key is in this answer and
// nowhere else; the router keeps the public half.
type PeerAdded struct {
	ID   string `json:"id"`
	Conf string `json:"conf"`
	// Host is what the configuration tells the device to connect to.
	Host string `json:"host"`
}

// addPeer makes a device for the WireGuard server; it must run on the
// control loop.
func (c *Core) addPeer(name string, full bool) (*PeerAdded, *proto.Error) {
	next := c.cfg.Clone()
	s := &next.Inbound.Server
	if !s.Enabled {
		return nil, proto.Errf(proto.ErrInvalid, "Switch on reaching home first.")
	}
	host := c.inboundStatus(c.cfg, c.active(c.wan.States())).Host
	if host == "" {
		return nil, proto.Errf(proto.ErrInvalid, "The router cannot be reached from outside as things stand, so the device would have nothing to connect to.")
	}
	addr, ok := s.FreeAddress()
	if !ok || len(s.Peers) >= config.MaxServerPeers {
		return nil, proto.Errf(proto.ErrInvalid, fmt.Sprintf("The router has room for %d devices that reach home.", config.MaxServerPeers))
	}
	priv, pub, err := vpn.GenerateKey()
	if err != nil {
		return nil, proto.Errf(proto.ErrInternal, err.Error())
	}
	peer := config.WGServerPeer{ID: config.NewID(), Name: strings.TrimSpace(name), PublicKey: pub, Address: addr, Enabled: true, Full: full}
	s.Peers = append(s.Peers, peer)
	if err := c.setConfig(next); err != nil {
		return nil, invalid(err)
	}
	return &PeerAdded{ID: peer.ID, Conf: ClientConf(c.cfg, peer, priv, host).String(), Host: host}, nil
}

// ClientConf is the configuration of a device that connects to the
// router's WireGuard server at host.
func ClientConf(cfg *config.Config, peer config.WGServerPeer, privateKey, host string) vpn.ClientConf {
	s := cfg.Inbound.Server
	v6 := cfg.LAN.IPv6
	conf := vpn.ClientConf{
		PrivateKey: privateKey, Addrs: s.PeerAddrs(peer, v6),
		ServerKey: s.PublicKey(), Endpoint: net.JoinHostPort(host, fmt.Sprint(s.Port)),
	}
	// Names are looked up by the router, at its address in the server's
	// own range, which every device's configuration routes to: local
	// names and ad blocking work as at home.
	router, range4 := s.Prefix()
	conf.DNS = append(conf.DNS, router)
	if v6 {
		router6, _ := s.ULA()
		conf.DNS = append(conf.DNS, router6)
	}
	if peer.Full {
		// Both families whether or not the router carries IPv6: what a
		// phone sends past the router would be what it was meant to hide.
		conf.Allowed = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
		return conf
	}
	conf.Allowed = append(conf.Allowed, range4)
	for _, n := range cfg.Networks {
		_, p := n.Prefix()
		conf.Allowed = append(conf.Allowed, p)
	}
	if v6 {
		_, range6 := s.ULA()
		conf.Allowed = append(conf.Allowed, range6)
		for _, n := range cfg.Networks {
			_, p := n.ULA()
			conf.Allowed = append(conf.Allowed, p)
		}
	}
	return conf
}
