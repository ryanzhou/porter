package core

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/adblock"
	"github.com/ryanzhou/porter/daemon/internal/ble"
	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/devices"
	"github.com/ryanzhou/porter/daemon/internal/dnsfwd"
	"github.com/ryanzhou/porter/daemon/internal/flows"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/netctl"
	"github.com/ryanzhou/porter/daemon/internal/netx"
	"github.com/ryanzhou/porter/daemon/internal/proto"
	"github.com/ryanzhou/porter/daemon/internal/speedtest"
	"github.com/ryanzhou/porter/daemon/internal/stats"
	"github.com/ryanzhou/porter/daemon/internal/system"
	"github.com/ryanzhou/porter/daemon/internal/vpn"
	"github.com/ryanzhou/porter/daemon/internal/wan"
	"github.com/ryanzhou/porter/daemon/internal/wifi"
)

// Transport is the way a control session came in. It decides what the
// session is before it has asked for anything.
type Transport int

const (
	// OverBluetooth: from within radio range of the router. A phone that
	// is not paired gets a session in which it can ask to be: being that
	// close, and the power button, are what pairing rests on.
	OverBluetooth Transport = iota
	// OverNetwork: from one of the router's own networks (see netctl).
	// For paired phones only; nobody is paired this way.
	OverNetwork
	// OverSocket: the local control socket, which only root can open, and
	// which is trusted as it is.
	OverSocket
)

// apiSession is one connected app.
type apiSession struct {
	s      *proto.Session
	paired bool
	via    Transport
	out    chan []byte
	// trust tells the link that the session has become a paired one.
	trust func()
}

// Serve runs one control session over an established packet link.
func (c *Core) Serve(pc proto.PacketConn, via Transport) {
	defer pc.Close()
	s, err := proto.Accept(pc, c.identity)
	if err != nil {
		c.log.Debug("control handshake failed", "err", err)
		return
	}
	// The link may want to know whose session this is (see ble).
	c.serve(s, via, func() {
		if t, ok := pc.(interface{ Trust() }); ok {
			t.Trust()
		}
	})
}

// serve runs a control session after its handshake.
func (c *Core) serve(s *proto.Session, via Transport, trust func()) {
	defer s.Close()
	sess := &apiSession{s: s, via: via, out: make(chan []byte, 64), trust: trust}
	sess.paired = via == OverSocket || c.pairings.Known(s.Remote)
	if via == OverNetwork && !sess.paired {
		// The listener has looked already; this is a phone removed in
		// the moment between.
		return
	}
	if sess.paired && via != OverSocket {
		c.pairings.Touch(s.Remote)
	}
	if sess.paired {
		trust()
	}
	c.sessMu.Lock()
	c.sessions[sess] = struct{}{}
	c.sessMu.Unlock()
	done := make(chan struct{})
	defer func() {
		c.sessMu.Lock()
		delete(c.sessions, sess)
		c.sessMu.Unlock()
		close(done)
	}()
	// Single writer: responses and events share the link.
	go func() {
		for {
			select {
			case m := <-sess.out:
				if err := s.Send(m); err != nil {
					s.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()
	for {
		msg, err := s.Recv()
		if err != nil {
			return
		}
		var req proto.Request
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		go func() {
			resp := proto.Response{ID: req.ID}
			result, perr := c.dispatch(sess, &req)
			if perr != nil {
				resp.Error = perr
			} else if result == nil {
				resp.Result = struct{}{}
			} else {
				resp.Result = result
			}
			b, err := json.Marshal(resp)
			if err != nil {
				b, _ = json.Marshal(proto.Response{ID: req.ID, Error: proto.Errf(proto.ErrInternal, err.Error())})
			}
			select {
			case sess.out <- b:
			case <-done:
			}
		}()
	}
}

// broadcast sends an event to every paired session. Slow sessions lose
// events rather than hold up the router.
func (c *Core) broadcast(event string, data any) {
	b, err := json.Marshal(proto.Event{Event: event, Data: data})
	if err != nil {
		return
	}
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	for s := range c.sessions {
		if !s.paired {
			continue
		}
		select {
		case s.out <- b:
		default:
		}
	}
}

// BLEInfo is the content of the unauthenticated Info characteristic.
func (c *Core) BLEInfo() []byte {
	var name string
	var open bool
	c.do(func() { name, open = c.cfg.System.Name, c.pairingOpen() })
	b, _ := json.Marshal(map[string]any{
		"v":       1,
		"name":    name,
		"id":      routerID(c.identity.Public),
		"claimed": c.pairings.Count() > 0,
		"pairing": open,
	})
	return b
}

// BLEName is the advertised name.
func (c *Core) BLEName() string {
	var name string
	c.do(func() { name = c.cfg.System.Name })
	return name
}

func routerID(pub []byte) string {
	return fmt.Sprintf("%x", pub[:4])
}

// RunBLE runs the Bluetooth control service until ctx ends.
func (c *Core) RunBLE(ctx context.Context, dial ble.Dialer) {
	srv := &ble.Server{Log: c.log, Name: c.BLEName, Info: c.BLEInfo, Handle: func(pc proto.PacketConn) { c.Serve(pc, OverBluetooth) }}
	for ctx.Err() == nil {
		if err := srv.Run(dial, ctx.Done()); err != nil {
			c.log.Warn("bluetooth control service", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// serveControlSocket accepts local sessions (porterctl, tests). The
// socket is root-only; whoever can open it already owns the machine.
func (c *Core) serveControlSocket(ctx context.Context) error {
	path := filepath.Join(c.opt.RunDir, "control.sock")
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o600)
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go c.Serve(proto.NewStreamConn(conn), OverSocket)
		}
	}()
	return nil
}

// controlNets is where the app may connect from over the network: the
// router's networks that are not isolated, and the router's own address
// in each.
type controlNets struct {
	addrs []netip.Addr
	nets  []netip.Prefix
}

// applyControl starts or stops the listener for the app on the router's
// own networks, and keeps what it accepts in step with the networks.
func (c *Core) applyControl(cfg *config.Config) {
	nets := &controlNets{}
	for _, n := range cfg.Networks {
		if n.Isolated {
			continue
		}
		a, p := n.Prefix()
		nets.addrs, nets.nets = append(nets.addrs, a), append(nets.nets, p)
		if cfg.LAN.IPv6 && fw.HaveIPv6() {
			a, p := n.ULA()
			nets.addrs, nets.nets = append(nets.addrs, a), append(nets.nets, p)
		}
	}
	c.ctlNets.Store(nets)
	switch {
	case cfg.System.BluetoothOnly && c.ctl != nil:
		// Sessions that came in this way end with it.
		c.ctl.Close()
		c.ctl = nil
		c.log.Info("control over the router's networks switched off")
	case !cfg.System.BluetoothOnly && c.ctl == nil:
		srv := &netctl.Server{
			Log: c.log, Identity: c.identity, Known: c.pairings.Known, Allowed: c.controlAllowed,
			Serve: func(s *proto.Session) { c.serve(s, OverNetwork, func() {}) },
		}
		if err := srv.Listen(fmt.Sprintf(":%d", proto.ControlPort)); err != nil {
			// Tried again on every pass; said once.
			if !c.ctlFailed {
				c.log.Warn("listening for the app on the router's networks", "err", err)
			}
			c.ctlFailed = true
			return
		}
		c.ctl, c.ctlFailed = srv, false
		c.log.Info("listening for the app on the router's networks", "port", proto.ControlPort)
	}
}

// controlAllowed reports whether a connection to the listener is one the
// firewall would let in: to one of the router's addresses, from inside a
// network that is not isolated. The listener is asked from its own
// goroutines and must not wait for the control loop, which can be busy
// for seconds.
func (c *Core) controlAllowed(local, remote netip.Addr) bool {
	nets := c.ctlNets.Load()
	if nets == nil {
		return false
	}
	to, from := false, false
	for _, a := range nets.addrs {
		to = to || a == local
	}
	for _, p := range nets.nets {
		from = from || p.Contains(remote)
	}
	return to && from
}

// ControlStatus says where the app finds the router on its own networks.
type ControlStatus struct {
	// Port is 0 while nothing listens there.
	Port int `json:"port"`
	// Addrs are the router's addresses in its networks that are not
	// isolated: IPv4, and the ULA where IPv6 is on.
	Addrs []string `json:"addrs"`
}

// controlStatus must run on the control loop.
func (c *Core) controlStatus() ControlStatus {
	st := ControlStatus{Addrs: []string{}}
	nets := c.ctlNets.Load()
	if c.ctl == nil || nets == nil {
		return st
	}
	st.Port = proto.ControlPort
	for _, a := range nets.addrs {
		st.Addrs = append(st.Addrs, a.String())
	}
	return st
}

// HookMessage is what the dhcpcd, dnsmasq and pppd hook helpers send.
// For pppd, Args are the event ("ip-up", ...) and then what pppd gives
// its scripts: the session's interface first.
type HookMessage struct {
	Kind string            `json:"kind"` // "dhcpcd", "lease" or "ppp"
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
}

func HookSocket(runDir string) string { return filepath.Join(runDir, "hook.sock") }

// HookSocketEnv names the environment variable that tells a hook helper
// where the daemon listens. dhcpcd and dnsmasq are started with it set
// and pass it on.
const HookSocketEnv = "PORTER_HOOK_SOCKET"

func (c *Core) serveHooks(ctx context.Context) error {
	path := HookSocket(c.opt.RunDir)
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o666) // dnsmasq's helper may not run as root
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				var m HookMessage
				if json.NewDecoder(bufio.NewReader(conn)).Decode(&m) != nil {
					return
				}
				switch m.Kind {
				case "dhcpcd":
					c.wan.HandleHook(m.Env)
				case "ppp":
					if len(m.Args) >= 2 {
						c.wan.HandlePPP(m.Args[0], m.Args[1], m.Env)
					}
				case "lease":
					// dnsmasq: <add|old|del> <mac> <ip> [hostname]
					if len(m.Args) >= 3 && strings.Count(m.Args[1], ":") == 5 {
						host := ""
						if len(m.Args) >= 4 {
							host = m.Args[3]
						}
						c.log.Debug("lease", "event", m.Args[0], "mac", m.Args[1], "ip", m.Args[2], "host", host)
						c.devs.Lease(m.Args[0], strings.ToLower(m.Args[1]), m.Args[2], host)
						c.Kick()
					}
				}
			}()
		}
	}()
	return nil
}

// --- status -----------------------------------------------------------------

type Status struct {
	Name     string           `json:"name"`
	ID       string           `json:"id"`
	Version  string           `json:"version"`
	Time     int64            `json:"time"`
	Uptime   int64            `json:"uptime"`
	Temp     float64          `json:"temp"`
	Ready    bool             `json:"ready"`
	Rev      uint64           `json:"rev"`
	Pairing  bool             `json:"pairing"`
	Internet string           `json:"internet"` // state of the active uplink, "down" if none
	Active   string           `json:"active,omitempty"`
	WAN      []wan.State      `json:"wan"`
	Portal   PortalStatus     `json:"portal"`
	APs      []wifi.APStatus  `json:"aps"`
	STAs     []wifi.STAStatus `json:"stas"`
	// OffAir lists the Wi-Fi networks that are switched on but that no
	// radio is able to carry.
	OffAir []string `json:"off_air"`
	// Ports are the wired interfaces that are plugged in, uplinks or not.
	Ports   []PortStatus   `json:"ports"`
	LAN     LANStatus      `json:"lan"`
	Control ControlStatus  `json:"control"`
	VPNs    []VPNStatus    `json:"vpns"`
	GeoIP   GeoIPStatus    `json:"geoip"`
	Adblock adblock.Status `json:"adblock"`
}

// GeoIPStatus says whether country rules and country display can work.
type GeoIPStatus struct {
	Available bool   `json:"available"`
	Date      string `json:"date,omitempty"`
}

type PortalStatus struct {
	Captive bool   `json:"captive"`
	URL     string `json:"url,omitempty"`
	Device  string `json:"device,omitempty"`
}

type LANStatus struct {
	Address string `json:"address"`
	ULA     string `json:"ula,omitempty"`
	Clients int    `json:"clients"`
	// Networks counts the devices online in each network, by id.
	Networks map[string]int `json:"networks"`
	// Prefixes holds, by id, the /64 each network has of the prefix
	// delegated by the uplink in use. A network that is not in it has
	// its private prefix only.
	Prefixes map[string]string `json:"prefixes"`
}

// PortStatus is one wired interface.
type PortStatus struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Phone: a phone sharing its connection over USB.
	Phone   bool `json:"phone"`
	Carrier bool `json:"carrier"`
}

type VPNStatus struct {
	vpn.Status
	Name string `json:"name"`
	// Type is the profile's kind. For an MPQUIC tunnel, Paths is how many
	// of its paths work and Of how many it has.
	Type  string `json:"type"`
	Paths int    `json:"paths,omitempty"`
	Of    int    `json:"of,omitempty"`
}

// status must run on the control loop.
func (c *Core) status() Status {
	cfg := c.cfg
	states := c.wan.States()
	st := Status{
		Name: cfg.System.Name, ID: routerID(c.identity.Public), Version: c.opt.Version,
		Time: time.Now().Unix(), Uptime: int64(time.Since(c.started) / time.Second), Temp: system.CPUTemp(),
		Ready: cfg.Ready(), Rev: cfg.Rev, Pairing: c.pairingOpen(),
		Internet: wan.StDown, WAN: states, APs: []wifi.APStatus{}, STAs: []wifi.STAStatus{}, VPNs: []VPNStatus{},
		OffAir: append([]string{}, c.offAir...), Ports: []PortStatus{},
		LAN:     LANStatus{Address: cfg.Main().Address, Networks: map[string]int{}, Prefixes: map[string]string{}},
		Control: c.controlStatus(),
	}
	for _, p := range c.ports {
		st.Ports = append(st.Ports, PortStatus{Name: p.Name, Label: p.Label, Phone: p.Phone, Carrier: netx.Carrier(p.Name)})
	}
	if st.WAN == nil {
		st.WAN = []wan.State{}
	}
	if c.geo != nil {
		st.GeoIP = GeoIPStatus{Available: true, Date: c.geo.Date}
	}
	st.Adblock = c.ads.Status()
	if a := wan.Active(states); a != nil {
		st.Internet, st.Active = a.Internet, a.If
		if a.Internet == wan.StCaptive {
			st.Portal = PortalStatus{Captive: true, URL: a.PortalURL, Device: c.portalDev}
		}
	}
	if cfg.LAN.IPv6 {
		st.LAN.ULA = cfg.Main().ULAPrefix
	}
	for id, g := range c.global {
		st.LAN.Prefixes[id] = g.Masked().String()
	}
	for _, d := range c.devs.List() {
		if d.Online {
			st.LAN.Clients++
			if d.Network != "" {
				st.LAN.Networks[d.Network]++
			}
		}
	}
	for _, rc := range cfg.Radios {
		if ap := c.aps[rc.ID]; ap != nil {
			st.APs = append(st.APs, ap.Status())
		}
		if sta := c.stas[rc.ID]; sta != nil {
			st.STAs = append(st.STAs, sta.Status())
		}
	}
	vs := c.vpn.Statuses()
	for _, v := range cfg.VPNs {
		s, ok := vs[v.ID]
		if !ok {
			s = vpn.Status{ID: v.ID, State: "off"}
		}
		entry := VPNStatus{Status: s, Name: v.Name, Type: v.Type}
		if v.Type == config.VPNMPQUIC {
			// The endpoint of the hop inside the router means nothing
			// to anyone; the server does.
			entry.Endpoint, entry.Of = "", len(v.MPQUIC.Servers)
			if len(v.MPQUIC.Servers) > 0 {
				entry.Endpoint = v.MPQUIC.Servers[0]
			}
			entry.Paths, _ = c.mpq.Health(v.ID)
			if v.Enabled && c.mpq.Binary() == "" {
				entry.State, entry.Error = "error", "The mpquic program is not installed"
			}
		}
		st.VPNs = append(st.VPNs, entry)
	}
	return st
}

// publishStatus pushes the status to connected apps when something other
// than a counter or the clock changed.
func (c *Core) publishStatus() {
	st := c.status()
	stable := st
	stable.Time, stable.Uptime, stable.Temp = 0, 0, 0
	stable.Adblock.Blocked = 0
	stable.VPNs = append([]VPNStatus(nil), st.VPNs...)
	for i := range stable.VPNs {
		stable.VPNs[i].Rx, stable.VPNs[i].Tx, stable.VPNs[i].Handshake = 0, 0, 0
	}
	stable.STAs = append([]wifi.STAStatus(nil), st.STAs...)
	for i := range stable.STAs {
		stable.STAs[i].Signal = 0
	}
	b, _ := json.Marshal(stable)
	if string(b) == c.statusKey {
		return
	}
	c.statusKey = string(b)
	c.broadcast("status", st)
}

// --- RPC ----------------------------------------------------------------------

func invalid(err error) *proto.Error { return proto.Errf(proto.ErrInvalid, err.Error()) }

func decode[T any](req *proto.Request) (T, *proto.Error) {
	var v T
	if len(req.Params) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(req.Params, &v); err != nil {
		return v, proto.Errf(proto.ErrInvalid, "malformed parameters")
	}
	return v, nil
}

// DeviceView is a device with its settings merged in.
type DeviceView struct {
	devices.Entry
	Name   string `json:"name"`
	Route  string `json:"route"`
	Paused bool   `json:"paused"`
	IP     string `json:"ip,omitempty"` // reserved address
	Portal bool   `json:"portal,omitempty"`
	// NoAdblock: the device is exempt from ad blocking.
	NoAdblock bool `json:"no_adblock"`
}

// RadioView is a radio's hardware description and configured role.
type RadioView struct {
	wifi.Radio
	Present bool   `json:"present"`
	Role    string `json:"role"`
}

func (c *Core) dispatch(sess *apiSession, req *proto.Request) (any, *proto.Error) {
	// The only things an unpaired session may do: learn who it is
	// talking to, and ask to be paired.
	switch req.Method {
	case "hello":
		var out map[string]any
		c.do(func() {
			out = map[string]any{
				"name": c.cfg.System.Name, "id": routerID(c.identity.Public), "version": c.opt.Version,
				"paired": sess.paired, "claimed": c.pairings.Count() > 0, "pairing": c.pairingOpen(),
				"ready": c.cfg.Ready(),
			}
			// Where to find the router on its networks is for phones
			// that may use it.
			if sess.paired {
				out["control"] = c.controlStatus()
			}
		})
		return out, nil
	case "pair":
		p, perr := decode[struct {
			Name string `json:"name"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		name := strings.TrimSpace(p.Name)
		if name == "" || len(name) > 40 {
			name = "Phone"
		}
		var out *proto.Error
		c.do(func() {
			if sess.paired {
				return
			}
			if sess.via == OverNetwork {
				out = proto.Errf(proto.ErrUnpaired, "a phone is paired over Bluetooth, next to the router")
				return
			}
			if !c.pairingOpen() {
				out = proto.Errf(proto.ErrPairingClosd, "press the power button on the router to allow pairing")
				return
			}
			if err := c.pairings.Add(sess.s.Remote, name); err != nil {
				out = proto.Errf(proto.ErrInternal, err.Error())
				return
			}
			sess.paired = true
			sess.trust()
			// One press, one phone.
			c.pairingUntil = time.Time{}
			c.addEvent("pairing", "Paired with "+name, "")
			c.updateLED()
		})
		return nil, out
	}
	if !sess.paired {
		return nil, proto.Errf(proto.ErrUnpaired, "this phone is not paired with the router")
	}

	switch req.Method {
	case "ping":
		// Answered without the control loop, so that it says whether the
		// link works and not whether the router is busy.
		return nil, nil

	case "status.get":
		var st Status
		c.do(func() { st = c.status() })
		return st, nil

	case "config.get":
		var cfg *config.Config
		c.do(func() { cfg = c.cfg.Clone() })
		return cfg, nil

	case "config.set":
		p, perr := decode[struct {
			Config *config.Config `json:"config"`
			ID     string         `json:"id"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		if p.Config == nil {
			return nil, proto.Errf(proto.ErrInvalid, "missing config")
		}
		if len(p.ID) > 64 {
			return nil, proto.Errf(proto.ErrInvalid, "malformed parameters")
		}
		var out *proto.Error
		var rev uint64
		c.do(func() {
			// The same write again: the app did not get the answer to
			// the first (its link went), and is told again what became
			// of it instead of having it judged against a document that
			// it has itself changed.
			for _, w := range c.writes {
				if p.ID != "" && w.id == p.ID {
					rev = w.rev
					return
				}
			}
			if p.Config.Rev != c.cfg.Rev {
				out = proto.Errf(proto.ErrConflict, "the configuration changed; reload and try again")
				return
			}
			if err := c.setConfig(p.Config); err != nil {
				out = invalid(err)
				return
			}
			rev = c.cfg.Rev
			if p.ID != "" {
				c.writes = append(c.writes, appliedWrite{p.ID, rev})
				if len(c.writes) > 32 {
					c.writes = c.writes[1:]
				}
			}
		})
		if out != nil {
			return nil, out
		}
		return map[string]uint64{"rev": rev}, nil

	case "config.check":
		// Says what applying a configuration would do, without applying
		// it: whether it would be accepted, what the hardware cannot do
		// of it, and what the people using the router will notice.
		p, perr := decode[struct {
			Config *config.Config `json:"config"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		if p.Config == nil {
			return nil, proto.Errf(proto.ErrInvalid, "missing config")
		}
		var out CheckResult
		c.do(func() { out = c.check(p.Config) })
		return out, nil

	case "wifi.uplink":
		// Chooses the radio that joins upstream Wi-Fi networks ("" for
		// none) and rearranges the others around it, in one step.
		p, perr := decode[struct {
			Radio string `json:"radio"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		var radios []wifi.Radio
		c.do(func() { radios = c.radios })
		find := func(id string) *wifi.Radio {
			for i := range radios {
				if radios[i].ID == id && radios[i].Iface != "" {
					return &radios[i]
				}
			}
			return nil
		}
		return nil, c.edit(func(cfg *config.Config) error {
			return cfg.SetWifiUplink(p.Radio,
				func(id string) bool { return find(id) != nil },
				func(id string) bool { r := find(id); return r != nil && r.APSTA })
		})

	case "radios.list":
		var out []RadioView
		c.do(func() {
			seen := map[string]bool{}
			for _, r := range c.radios {
				seen[r.ID] = true
				v := RadioView{Radio: r, Present: true, Role: config.RoleOff}
				if rc := c.cfg.Radio(r.ID); rc != nil {
					v.Role = rc.Role
				}
				out = append(out, v)
			}
			for _, rc := range c.cfg.Radios {
				if !seen[rc.ID] {
					out = append(out, RadioView{Radio: wifi.Radio{ID: rc.ID, Bands: []wifi.Band{}}, Role: rc.Role})
				}
			}
		})
		if out == nil {
			out = []RadioView{}
		}
		return out, nil

	case "wifi.scan":
		var sta *wifi.STA
		var known map[string]bool
		c.do(func() {
			for _, rc := range c.cfg.Radios {
				if s := c.stas[rc.ID]; s != nil && sta == nil {
					sta = s
				}
			}
			known = map[string]bool{}
			for _, n := range c.cfg.WAN.Networks {
				known[n.SSID] = true
			}
		})
		if sta == nil {
			return nil, proto.Errf("no_station", "no radio is set up to connect to Wi-Fi networks")
		}
		// With "quick", what the last scan found comes back at once and a
		// new scan starts behind it; without, the call waits for that
		// scan. An app asks quickly first, to have something to show.
		q, perr := decode[struct {
			Quick bool `json:"quick"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		res, err := sta.Scan(!q.Quick)
		if err != nil {
			return nil, proto.Errf(proto.ErrInternal, err.Error())
		}
		type item struct {
			wifi.ScanResult
			Known bool `json:"known"`
		}
		out := make([]item, 0, len(res))
		for _, r := range res {
			out = append(out, item{r, known[r.SSID]})
		}
		return out, nil

	case "wifi.connect":
		p, perr := decode[config.WifiNetwork](req)
		if perr != nil {
			return nil, perr
		}
		return nil, c.edit(func(cfg *config.Config) error {
			top := 0
			kept := cfg.WAN.Networks[:0]
			for _, n := range cfg.WAN.Networks {
				if n.SSID != p.SSID {
					kept = append(kept, n)
					top = max(top, n.Priority)
				} else if p.IP.V4.Mode == "" && p.IP.V6.Mode == "" {
					// Joining again with a new password keeps the
					// addresses set for the network.
					p.IP = n.IP
				}
			}
			p.Priority = min(top+1, 1000)
			cfg.WAN.Networks = append(kept, p)
			return nil
		})

	case "wifi.forget":
		p, perr := decode[struct {
			SSID string `json:"ssid"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		return nil, c.edit(func(cfg *config.Config) error {
			kept := cfg.WAN.Networks[:0]
			for _, n := range cfg.WAN.Networks {
				if n.SSID != p.SSID {
					kept = append(kept, n)
				}
			}
			cfg.WAN.Networks = kept
			return nil
		})

	case "wan.new_mac":
		p, perr := decode[struct {
			Kind string `json:"kind"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		return nil, c.edit(func(cfg *config.Config) error {
			pol := &cfg.WAN.WifiMAC
			if p.Kind == "eth" {
				pol = &cfg.WAN.EthMAC
			}
			pol.Mode, pol.Address = config.MACRandom, config.RandomMAC()
			return nil
		})

	case "wan.recheck":
		c.wan.ProbeNow()
		return nil, nil

	case "devices.list":
		var out []DeviceView
		c.do(func() {
			seen := map[string]bool{}
			for _, e := range c.devs.List() {
				seen[e.MAC] = true
				v := DeviceView{Entry: e, Portal: e.MAC == c.portalDev}
				if d := c.cfg.Device(e.MAC); d != nil {
					v.Name, v.Route, v.Paused, v.IP, v.NoAdblock = d.Name, d.Route, d.Paused, d.IP, d.NoAdblock
				}
				out = append(out, v)
			}
			for _, d := range c.cfg.Devices {
				if !seen[d.MAC] {
					out = append(out, DeviceView{Entry: devices.Entry{MAC: d.MAC}, Name: d.Name, Route: d.Route, Paused: d.Paused, IP: d.IP, NoAdblock: d.NoAdblock})
				}
			}
		})
		if out == nil {
			out = []DeviceView{}
		}
		return out, nil

	case "device.set":
		p, perr := decode[config.Device](req)
		if perr != nil {
			return nil, perr
		}
		p.MAC = strings.ToLower(p.MAC)
		return nil, c.edit(func(cfg *config.Config) error {
			empty := p.Name == "" && p.Route == "" && !p.Paused && p.IP == "" && !p.NoAdblock
			for i := range cfg.Devices {
				if cfg.Devices[i].MAC == p.MAC {
					if empty {
						cfg.Devices = append(cfg.Devices[:i], cfg.Devices[i+1:]...)
					} else {
						cfg.Devices[i] = p
					}
					return nil
				}
			}
			if !empty {
				cfg.Devices = append(cfg.Devices, p)
			}
			return nil
		})

	case "device.forget":
		p, perr := decode[struct {
			MAC string `json:"mac"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		mac := strings.ToLower(p.MAC)
		c.devs.Forget(mac)
		return nil, c.edit(func(cfg *config.Config) error {
			for i := range cfg.Devices {
				if cfg.Devices[i].MAC == mac {
					cfg.Devices = append(cfg.Devices[:i], cfg.Devices[i+1:]...)
					break
				}
			}
			return nil
		})

	case "device.queries":
		p, perr := decode[struct {
			MAC string `json:"mac"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		q := c.dns.Recent(strings.ToLower(p.MAC))
		if q == nil {
			q = []dnsfwd.Query{}
		}
		return q, nil

	case "stats.get":
		p, perr := decode[struct {
			MAC string `json:"mac"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		minutes, hours := c.stats.Get(strings.ToLower(p.MAC), time.Now())
		return map[string][]stats.Sample{"minutes": minutes, "hours": hours}, nil

	case "stats.top":
		p, perr := decode[struct {
			Hours int `json:"hours"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		if p.Hours == 0 {
			p.Hours = 24
		}
		return c.stats.TopDevices(p.Hours, time.Now()), nil

	case "flows.list":
		p, perr := decode[struct {
			MAC string `json:"mac"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		list := c.flows.List(strings.ToLower(p.MAC))
		if list == nil {
			list = []flows.Flow{}
		}
		return list, nil

	case "speedtest.run":
		p, perr := decode[struct {
			Via string `json:"via"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		if p.Via == "" {
			p.Via = config.ActDirect
		}
		var cfg config.SpeedTest
		var mark uint32
		var out *proto.Error
		c.do(func() {
			cfg = c.cfg.SpeedTest
			id, isVPN := config.VPNOf(p.Via)
			switch {
			case p.Via == config.ActDirect:
			case isVPN:
				if st, ok := c.vpn.Statuses()[id]; !ok || st.State != "up" {
					out = proto.Errf(proto.ErrInvalid, "that VPN is not connected")
					return
				}
				mark = fw.MarkFor(c.cfg, p.Via)
			default:
				out = proto.Errf(proto.ErrInvalid, "a speed test runs directly or through a VPN")
			}
		})
		if out != nil {
			return nil, out
		}
		if !c.speedMu.TryLock() {
			return nil, proto.Errf("busy", "a speed test is already running")
		}
		defer c.speedMu.Unlock()
		type progress struct {
			Phase string  `json:"phase"`
			Mbps  float64 `json:"mbps"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// The phase length can be shortened for tests.
		phase, _ := time.ParseDuration(os.Getenv("PORTER_SPEEDTEST_DURATION"))
		res := speedtest.Run(ctx, cfg, p.Via, speedtest.Options{Mark: mark, Duration: phase, Progress: func(phase string, mbps float64) {
			c.broadcast("speedtest", progress{phase, mbps})
		}})
		c.broadcast("speedtest", progress{"done", 0})
		c.speedHistMu.Lock()
		c.speedHistory = append([]speedtest.Result{res}, c.speedHistory...)
		if len(c.speedHistory) > 20 {
			c.speedHistory = c.speedHistory[:20]
		}
		c.speedHistMu.Unlock()
		return res, nil

	case "speedtest.history":
		c.speedHistMu.Lock()
		out := append([]speedtest.Result{}, c.speedHistory...)
		c.speedHistMu.Unlock()
		return out, nil

	case "vpn.parse":
		// Turns a WireGuard configuration file into a profile for the
		// app to review and save through config.set.
		p, perr := decode[struct {
			Conf string `json:"conf"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		wg, err := vpn.ParseConf(p.Conf)
		if err != nil {
			return nil, invalid(err)
		}
		return wg, nil

	case "vpn.genkey":
		priv, pub, err := vpn.GenerateKey()
		if err != nil {
			return nil, proto.Errf(proto.ErrInternal, err.Error())
		}
		return map[string]string{"private_key": priv, "public_key": pub}, nil

	case "vpn.pubkey":
		p, perr := decode[struct {
			PrivateKey string `json:"private_key"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		pub, err := vpn.PublicKey(p.PrivateKey)
		if err != nil {
			return nil, invalid(err)
		}
		return map[string]string{"public_key": pub}, nil

	case "portal.set_device":
		p, perr := decode[struct {
			MAC string `json:"mac"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		mac := ""
		if p.MAC != "" {
			hw, err := net.ParseMAC(p.MAC)
			if err != nil || len(hw) != 6 {
				return nil, proto.Errf(proto.ErrInvalid, "not a device address")
			}
			mac = hw.String()
		}
		c.do(func() {
			c.portalDev, c.portalManual = mac, mac != ""
			c.Kick()
		})
		return nil, nil

	case "portal.kick":
		// Makes the portal device reconnect so that its own captive
		// portal detection runs again and shows the sign-in page.
		c.do(func() {
			if c.portalDev != "" {
				for _, ap := range c.aps {
					ap.Kick(c.portalDev)
				}
			}
		})
		return nil, nil

	case "pairing.open":
		c.do(func() { c.openPairing(2 * time.Minute) })
		return nil, nil

	case "pairing.list":
		type item struct {
			proto.Peer
			Self bool `json:"self"`
		}
		self := base64.StdEncoding.EncodeToString(sess.s.Remote)
		out := []item{}
		for _, p := range c.pairings.List() {
			out = append(out, item{p, p.Key == self})
		}
		return out, nil

	case "pairing.revoke":
		p, perr := decode[struct {
			Key string `json:"key"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		ok, err := c.pairings.Remove(p.Key)
		if err != nil {
			return nil, proto.Errf(proto.ErrInternal, err.Error())
		}
		if !ok {
			return nil, proto.Errf(proto.ErrNotFound, "no such phone")
		}
		// Cut off any session the revoked phone still has.
		c.sessMu.Lock()
		for s := range c.sessions {
			if s.via != OverSocket && base64.StdEncoding.EncodeToString(s.s.Remote) == p.Key {
				s.paired = false
				s.s.Close()
			}
		}
		c.sessMu.Unlock()
		return nil, nil

	case "updates.check":
		// Looks for newer block lists and a newer country database now,
		// instead of at the next scheduled check.
		c.ads.RefreshNow()
		select {
		case c.geoCheck <- struct{}{}:
		default:
		}
		return nil, nil

	case "firmware.check":
		info, _, perr := c.firmwareCheck()
		if perr != nil {
			return nil, perr
		}
		return info, nil

	case "firmware.install":
		return nil, c.firmwareInstall()

	case "firmware.status":
		return c.firmwareProgress(), nil

	case "events.list":
		c.evMu.Lock()
		out := append([]Event{}, c.events...)
		c.evMu.Unlock()
		return out, nil

	case "system.set_time":
		p, perr := decode[struct {
			UnixMS int64 `json:"unix_ms"`
		}](req)
		if perr != nil {
			return nil, perr
		}
		if p.UnixMS < c.opt.BuildTime.UnixMilli() {
			return nil, proto.Errf(proto.ErrInvalid, "time is earlier than this software was built")
		}
		adjusted, err := system.TimeFromPhone(time.UnixMilli(p.UnixMS))
		if err != nil {
			return nil, proto.Errf(proto.ErrInternal, err.Error())
		}
		if adjusted {
			system.SaveClock(filepath.Join(c.opt.DataDir, "clock"))
		}
		return map[string]bool{"adjusted": adjusted}, nil

	case "system.logs":
		return c.opt.Logs.Lines(), nil

	case "system.reboot":
		c.do(func() { c.devs.Save() })
		go func() { time.Sleep(500 * time.Millisecond); system.Reboot() }()
		return nil, nil

	case "system.poweroff":
		c.do(func() { c.devs.Save() })
		go func() { time.Sleep(500 * time.Millisecond); system.Poweroff() }()
		return nil, nil

	case "system.factory_reset":
		// Wipes all settings, pairings and the router's identity. Done on
		// the control loop, which is what would otherwise write some of
		// it back before the restart.
		c.do(func() {
			c.resetting = true
			entries, _ := os.ReadDir(c.opt.DataDir)
			for _, e := range entries {
				os.RemoveAll(filepath.Join(c.opt.DataDir, e.Name()))
			}
			// And the iPhones that were told to trust the router, which
			// usbmuxd keeps in a directory of its own.
			if c.opt.PhoneRecords != "" {
				records, _ := os.ReadDir(c.opt.PhoneRecords)
				for _, e := range records {
					os.RemoveAll(filepath.Join(c.opt.PhoneRecords, e.Name()))
				}
			}
		})
		go func() { time.Sleep(500 * time.Millisecond); system.Reboot() }()
		return nil, nil
	}
	return nil, proto.Errf(proto.ErrUnknown, "unknown method "+req.Method)
}

// CheckResult is the answer to config.check.
type CheckResult struct {
	// Error says why the configuration would be refused; empty when it
	// would be accepted.
	Error string `json:"error,omitempty"`
	// Notes say what the hardware cannot do of what is asked.
	Notes []string `json:"notes"`
	// Effects say what applying it will be noticed as.
	Effects []string `json:"effects"`
}

// check must run on the control loop.
func (c *Core) check(next *config.Config) CheckResult {
	out := CheckResult{Notes: []string{}, Effects: []string{}}
	next = next.Clone()
	next.Normalize()
	if next.Rev != c.cfg.Rev {
		out.Error = "The settings changed on the router in the meantime. Reload and make the change again."
		return out
	}
	if err := next.Validate(); err != nil {
		out.Error = err.Error()
		return out
	}
	if err := next.Consistent(); err != nil && c.cfg.Consistent() == nil {
		out.Error = err.Error()
		return out
	}
	if !next.Ready() {
		return out
	}
	plan := planRadios(next, c.radios)
	out.Notes = append(out.Notes, plan.notes...)
	cur := c.cfg
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if j(cur.Wifi) != j(next.Wifi) || j(cur.Radios) != j(next.Radios) || cur.System.Country != next.System.Country {
		out.Effects = append(out.Effects, "Your Wi-Fi restarts. Devices reconnect by themselves within a few seconds.")
	}
	for _, w := range next.Wifi {
		if old := cur.SSID(w.ID); old != nil && old.SSID != "" && (old.SSID != w.SSID || old.Passphrase != w.Passphrase) && w.Enabled {
			out.Effects = append(out.Effects, fmt.Sprintf("Devices on “%s” have to join again with the new name or password.", old.SSID))
		}
	}
	for _, n := range next.Networks {
		if old := cur.Network(n.ID); old != nil && old.Address != n.Address {
			out.Effects = append(out.Effects, fmt.Sprintf("Devices on %s get new addresses.", n.Name))
		}
	}
	for _, n := range cur.Networks {
		if next.Network(n.ID) == nil {
			out.Effects = append(out.Effects, fmt.Sprintf("%s is removed. Its devices lose their connection.", n.Name))
		}
	}
	return out
}

// edit applies a change to a copy of the configuration on the control
// loop.
func (c *Core) edit(f func(cfg *config.Config) error) *proto.Error {
	var out *proto.Error
	c.do(func() {
		next := c.cfg.Clone()
		if err := f(next); err != nil {
			out = invalid(err)
			return
		}
		if err := c.setConfig(next); err != nil {
			out = invalid(err)
		}
	})
	return out
}
