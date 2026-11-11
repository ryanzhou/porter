// Package core is the router's control loop. It owns the configuration
// and continuously reconciles every subsystem (radios, uplinks, LAN,
// firewall, routing, DNS, VPN) against it and against what is happening
// on the network.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/adblock"
	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/devices"
	"github.com/ryanzhou/porter/daemon/internal/dnsfwd"
	"github.com/ryanzhou/porter/daemon/internal/flows"
	"github.com/ryanzhou/porter/daemon/internal/fw"
	"github.com/ryanzhou/porter/daemon/internal/geoip"
	"github.com/ryanzhou/porter/daemon/internal/lan"
	"github.com/ryanzhou/porter/daemon/internal/mpquic"
	"github.com/ryanzhou/porter/daemon/internal/netx"
	"github.com/ryanzhou/porter/daemon/internal/proto"
	"github.com/ryanzhou/porter/daemon/internal/speedtest"
	"github.com/ryanzhou/porter/daemon/internal/stats"
	"github.com/ryanzhou/porter/daemon/internal/system"
	"github.com/ryanzhou/porter/daemon/internal/update"
	"github.com/ryanzhou/porter/daemon/internal/vpn"
	"github.com/ryanzhou/porter/daemon/internal/wan"
	"github.com/ryanzhou/porter/daemon/internal/wifi"
)

type Options struct {
	DataDir string // persistent state
	RunDir  string // runtime files
	// Hook executables (symlinks to this binary) called by dhcpcd and
	// dnsmasq.
	DHCPHook  string
	LeaseHook string
	// WGUserspace is a wireguard-go binary to use instead of the kernel
	// module (tests).
	WGUserspace string
	// GeoIPPath is the country database; a missing file only disables
	// country rules and country display.
	GeoIPPath string
	// MPQUIC lists where the mpquic program may be, in order of
	// preference.
	MPQUIC []string
	// PhoneRecords is where usbmuxd keeps its records of iPhones that
	// trust the router; a factory reset empties it. Empty: there is none.
	PhoneRecords string
	Version      string
	// Update installs new versions; nil where that is not possible.
	// UpdateURL is where the newest release's files are, ending in a
	// slash. UpdateSettle is how long a new version has to run before it
	// is kept.
	Update       *update.System
	UpdateURL    string
	UpdateSettle time.Duration
	BuildTime    time.Time
	Log          *slog.Logger
	Logs         *LogRing
}

// Event is an entry of the router's activity log.
type Event struct {
	Time int64  `json:"t"`
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
	MAC  string `json:"mac,omitempty"`
}

type Core struct {
	opt Options
	log *slog.Logger

	firmware firmware

	store    *config.Store
	cfg      *config.Config
	identity proto.Identity
	pairings *proto.Pairings

	fwm  *fw.Manager
	dns  *dnsfwd.Server
	lan  *lan.Manager
	wan  *wan.Manager
	vpn  *vpn.Manager
	mpq  *mpquic.Manager
	devs *devices.Table

	geo *geoip.DB
	ads *adblock.Manager
	// geoCheck asks the updater to look for a newer country database now.
	geoCheck chan struct{}
	flows    *flows.Tracker
	stats    *stats.Store

	speedMu      sync.Mutex // held while a speed test runs
	speedHistMu  sync.Mutex
	speedHistory []speedtest.Result
	acctEnabled  bool

	radios []wifi.Radio
	aps    map[string]*wifi.AP  // by radio id
	apKeys map[string]string    // spec fingerprint by radio id
	stas   map[string]*wifi.STA // by radio id
	staMAC map[string]string    // address policy last applied, by interface
	apAuto map[string]int       // 5 GHz channel chosen to keep clear of an uplink, by radio id
	// apShared is the frequency last used by the access point of a radio
	// that is also the uplink, by radio id. The access point stays there
	// until the uplink needs it elsewhere.
	apShared map[string]int
	// offAir lists the Wi-Fi networks that are switched on but that no
	// radio carries, and ports the wired ports as last seen.
	offAir []string
	ports  []wiredPort

	reqs chan func()
	kick chan struct{}

	started      time.Time
	pairingUntil time.Time
	portalDev    string
	portalManual bool
	wasCaptive   bool

	// Fingerprints of what was last applied, to skip no-op work.
	planKey    string
	routeKey   string
	dnsKey     string
	sysKey     string
	statusKey  string
	lanApplied bool
	overrideIf string
	regdom     string
	lastAcct   time.Time
	lastSave   time.Time

	evMu   sync.Mutex
	events []Event

	sessMu   sync.Mutex
	sessions map[*apiSession]struct{}
}

func New(opt Options) (*Core, error) {
	for _, d := range []string{opt.DataDir, opt.RunDir, filepath.Join(opt.RunDir, "hostapd"), filepath.Join(opt.RunDir, "wpa")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	c := &Core{
		opt: opt, log: opt.Log,
		store:    config.NewStore(opt.DataDir),
		fwm:      fw.NewManager(),
		aps:      map[string]*wifi.AP{},
		apKeys:   map[string]string{},
		stas:     map[string]*wifi.STA{},
		staMAC:   map[string]string{},
		apAuto:   map[string]int{},
		apShared: map[string]int{},
		reqs:     make(chan func(), 64),
		kick:     make(chan struct{}, 1),
		started:  time.Now(),
		sessions: map[*apiSession]struct{}{},
	}
	system.RestoreClock(filepath.Join(opt.DataDir, "clock"), opt.BuildTime, c.log)

	cfg, err := c.store.Load()
	if err != nil {
		c.log.Error("configuration", "err", err)
	}
	if cfg == nil {
		cfg = config.Default()
	}
	cfg.Normalize()
	c.cfg = cfg

	if c.identity, err = proto.LoadIdentity(filepath.Join(opt.DataDir, "identity.json")); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	if c.pairings, err = proto.LoadPairings(filepath.Join(opt.DataDir, "pairings.json")); err != nil {
		return nil, fmt.Errorf("pairings: %w", err)
	}
	// The country database comes with the image and is refreshed into
	// the data partition; use whichever is newer.
	if c.geo = geoip.Newest(opt.GeoIPPath, c.geoDataPath()); c.geo == nil {
		c.log.Warn("no country database yet; country rules match nothing until one is downloaded")
	} else {
		c.log.Info("country database loaded", "date", c.geo.Date)
	}
	c.geoCheck = make(chan struct{}, 1)
	c.ads = &adblock.Manager{Log: c.log, Dir: filepath.Join(opt.DataDir, "blocklists")}
	c.stats = stats.Load(filepath.Join(opt.DataDir, "stats.json"))
	c.devs = devices.Load(filepath.Join(opt.DataDir, "devices.json"))
	c.devs.OnTraffic = func(mac string, up, down uint64) { c.stats.Add(mac, up, down, time.Now()) }
	// The flow tracker is polled from the control loop, so its hooks may
	// read the configuration directly.
	c.flows = flows.New(flows.Hooks{
		LAN:       func(a netip.Addr) bool { return c.cfg.NetworkOf(a) != nil },
		LookupMAC: c.devs.LookupMAC,
		NameFor:   func(a netip.Addr) string { return c.dns.NameFor(a) },
		Country:   func(a netip.Addr) string { return c.geo.Lookup(a) },
		Path: func(mark uint32) string {
			switch class := mark & fw.ClassMask; {
			case class == fw.MarkBlock:
				return config.ActBlock
			case class >= fw.MarkVPN(0):
				if i := int(class - fw.MarkVPN(0)); i < len(c.cfg.VPNs) {
					return config.ActVPN(c.cfg.VPNs[i].ID)
				}
			}
			return config.ActDirect
		},
	})
	c.devs.OnNew = func(e devices.Entry) {
		c.addEvent("device", "New device joined", e.MAC)
		c.Kick()
	}
	c.dns = dnsfwd.New(dnsfwd.Hooks{LookupMAC: c.devs.LookupMAC, Learn: c.fwm.Learn, AdBlocked: c.ads.Blocked}, c.log)
	hookEnv := []string{HookSocketEnv + "=" + HookSocket(opt.RunDir)}
	c.lan = &lan.Manager{Log: c.log, RunDir: opt.RunDir, HookPath: opt.LeaseHook, HookEnv: hookEnv}
	c.wan = &wan.Manager{Log: c.log, RunDir: opt.RunDir, HookPath: opt.DHCPHook, HookEnv: hookEnv, OnChange: c.Kick}
	c.mpq = &mpquic.Manager{Log: c.log, RunDir: opt.RunDir, Binaries: opt.MPQUIC, OnChange: c.Kick}
	c.vpn = &vpn.Manager{Log: c.log, OnChange: c.Kick, UserspaceImpl: opt.WGUserspace, Resolve: c.resolveDirect}
	c.vpn.Carrying = func(id string) (bool, string) {
		if paths, managed := c.mpq.Health(id); managed && paths == 0 {
			return false, "No path to the server"
		}
		return true, ""
	}
	lan.APBridge = func(name string) (string, bool) {
		// Only this package's goroutine mutates aps, but the bridge sync
		// runs on it too, so no locking is needed.
		for _, ap := range c.aps {
			if b, ok := ap.Bridge(name); ok {
				return b, true
			}
		}
		return "", false
	}
	return c, nil
}

// Kick schedules a reconcile. Safe from any goroutine.
func (c *Core) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// do runs f on the control loop and waits for it.
func (c *Core) do(f func()) {
	done := make(chan struct{})
	c.reqs <- func() { f(); close(done) }
	<-done
}

func (c *Core) addEvent(kind, msg, mac string) {
	c.evMu.Lock()
	c.events = append(c.events, Event{Time: time.Now().Unix(), Kind: kind, Msg: msg, MAC: mac})
	if len(c.events) > 200 {
		c.events = c.events[len(c.events)-200:]
	}
	ev := c.events[len(c.events)-1]
	c.evMu.Unlock()
	c.log.Info("event", "kind", kind, "msg", msg, "mac", mac)
	c.broadcast("event", ev)
}

// Run is the control loop. It returns when ctx is cancelled.
func (c *Core) Run(ctx context.Context) error {
	if err := fw.EnableForwarding(); err != nil {
		c.log.Warn("enabling forwarding", "err", err)
	}
	wifi.Unblock()
	_ = os.WriteFile(filepath.Join(c.opt.RunDir, "resolv.conf"), []byte("nameserver 127.0.0.1\n"), 0o644)
	c.wan.Start()
	c.vpn.Start()
	if err := c.serveHooks(ctx); err != nil {
		return err
	}
	if err := c.serveControlSocket(ctx); err != nil {
		return err
	}
	system.WatchPowerButton(c.log, func(short bool) {
		if short {
			c.reqs <- func() { c.openPairing(2 * time.Minute) }
		} else {
			c.log.Info("power button held: shutting down")
			system.Poweroff()
		}
	})

	c.reconcile()
	go c.ads.Run(ctx, c.online)
	go c.runGeoUpdates(ctx)
	go c.settleUpdate(ctx)
	system.Notify("READY=1")
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			c.shutdown()
			return nil
		case f := <-c.reqs:
			f()
		case <-c.kick:
			c.reconcile()
		case <-tick.C:
			system.Notify("WATCHDOG=1")
			c.periodic()
			c.reconcile()
		}
	}
}

func (c *Core) shutdown() {
	c.log.Info("shutting down")
	system.SaveClock(filepath.Join(c.opt.DataDir, "clock"))
	c.devs.Save()
	c.stats.Save()
	for _, ap := range c.aps {
		ap.Stop()
	}
	for _, sta := range c.stas {
		sta.Stop()
	}
	c.wan.Stop()
	c.vpn.Stop()
	c.mpq.Stop()
	c.lan.Stop()
	c.dns.Close()
	// The firewall table is left in place on purpose: a router whose
	// daemon is restarting must keep filtering.
}

func (c *Core) periodic() {
	now := time.Now()
	// Who is here, and on which network: from each network's neighbour
	// table and from the access points.
	neigh := map[netip.Addr]string{}
	home := map[string]string{}
	for _, n := range c.cfg.Networks {
		for ip, mac := range netx.Neighbors(fw.Bridge(n.ID)) {
			neigh[ip], home[mac] = mac, n.ID
		}
	}
	var stations []devices.Station
	for _, ap := range c.aps {
		for _, s := range ap.Stations() {
			stations = append(stations, devices.Station{MAC: s.MAC, Iface: s.Iface, Signal: s.Signal})
			if b, ok := ap.Bridge(s.Iface); ok {
				home[s.MAC] = strings.TrimPrefix(b, "br-")
			}
		}
	}
	c.devs.Observe(neigh, stations, home)

	// Read traffic counters often while someone is looking, rarely
	// otherwise.
	every := 20 * time.Second
	c.sessMu.Lock()
	if len(c.sessions) > 0 {
		every = 5 * time.Second
	}
	c.sessMu.Unlock()
	if now.Sub(c.lastAcct) >= every && c.planKey != "" {
		c.lastAcct = now
		if ctr, err := fw.ReadCounters(); err == nil {
			c.devs.Account(ctr)
		}
	}
	if c.planKey != "" {
		if err := c.flows.Poll(); err != nil {
			c.log.Debug("reading connection table", "err", err)
		}
	}
	if now.Sub(c.lastSave) >= 10*time.Minute {
		c.lastSave = now
		c.devs.Save()
		c.stats.Save()
		system.SaveClock(filepath.Join(c.opt.DataDir, "clock"))
	}
	c.updateLED()
}

func (c *Core) pairingOpen() bool {
	return c.pairings.Count() == 0 || time.Now().Before(c.pairingUntil)
}

func (c *Core) openPairing(d time.Duration) {
	c.pairingUntil = time.Now().Add(d)
	c.addEvent("pairing", "Pairing window opened", "")
	c.updateLED()
}

func (c *Core) updateLED() {
	switch {
	case c.pairings.Count() > 0 && time.Now().Before(c.pairingUntil):
		system.SetLED(system.LEDPairing)
	case c.pairings.Count() == 0 || !c.cfg.Ready():
		system.SetLED(system.LEDSetup)
	default:
		system.SetLED(system.LEDNormal)
	}
}

// setConfig validates, persists and applies a new configuration. The
// whole of it takes effect in one pass of the control loop, so a change
// to several things at once never passes through a state in which only
// some of them have been made.
func (c *Core) setConfig(next *config.Config) error {
	next.Normalize()
	if err := next.Validate(); err != nil {
		return err
	}
	if err := next.Consistent(); err != nil {
		return err
	}
	next.Rev = c.cfg.Rev + 1
	if err := c.store.Save(next); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.cfg = next
	c.Kick()
	return nil
}

// --- reconcile ----------------------------------------------------------------

func (c *Core) reconcile() {
	cfg := c.cfg
	changedCfg := false

	// Hardware inventory. New radios and ports are added to the
	// configuration (radios switched off) so the app can assign them.
	c.radios = wifi.Discover()
	for _, r := range c.radios {
		if cfg.Radio(r.ID) == nil {
			cfg.Radios = append(cfg.Radios, config.Radio{ID: r.ID, Role: config.RoleOff})
			changedCfg = true
			c.addEvent("radio", "New Wi-Fi adapter detected: "+r.Product, "")
		}
	}
	c.ports = wiredPorts()
	present := map[string]wiredPort{}
	for _, p := range c.ports {
		present[p.Name] = p
		if p.Phone {
			// A phone is an uplink for as long as it is plugged in, and
			// nothing about it is worth remembering.
			continue
		}
		known := false
		for _, e := range cfg.Ethernet {
			known = known || e.Name == p.Name
		}
		if !known {
			cfg.Ethernet = append(cfg.Ethernet, config.EthPort{Name: p.Name, Role: config.EthWAN, Network: config.MainNetwork})
			changedCfg = true
		}
	}
	if changedCfg {
		cfg.Normalize() // ranks the new ports among the uplinks
	}

	// Radios.
	ready := cfg.Ready()
	if ready && c.regdom != cfg.System.Country {
		if err := wifi.SetRegDomain(cfg.System.Country); err != nil {
			c.log.Warn("setting regulatory domain", "err", err)
		}
		c.regdom = cfg.System.Country
		// Channel availability depends on the domain.
		c.radios = wifi.Discover()
	}
	wantAP := map[string]wifi.APSpec{}
	wantSTA := map[string]wifi.STASpec{}
	staStatus := map[string]wifi.STAStatus{}
	for id, sta := range c.stas {
		staStatus[id] = sta.Status()
	}
	// Whether a station has to wait for its radio's access point; see
	// wifi.STA.Steer.
	knownFreq := map[string]int{}
	c.offAir = nil
	if ready {
		plan := planRadios(cfg, c.radios)
		c.offAir = plan.offAir
		for _, rc := range cfg.Radios {
			hw := plan.hw[rc.ID]
			if hw == nil {
				continue
			}
			role := plan.role[rc.ID]
			if role == config.RoleWAN || role == config.RoleAPWAN {
				spec := wifi.STASpec{Radio: rc.ID, Iface: hw.Iface, Country: cfg.System.Country, Networks: cfg.WAN.Networks}
				if role == config.RoleAPWAN && len(plan.ssids[rc.ID]) > 0 {
					// The access point has to share the uplink's channel,
					// and it is not allowed on every channel the uplink
					// could use.
					spec.Shared = true
					spec.Only5 = []int{}
					if b := hw.Band("5"); b != nil {
						for _, ch := range b.Channels {
							if ch.Usable() {
								spec.Only5 = append(spec.Only5, ch.Freq)
							}
						}
					}
				}
				wantSTA[rc.ID] = spec
			}
			ssids := plan.ssids[rc.ID]
			if len(ssids) == 0 {
				continue
			}
			spec := wifi.APSpec{Radio: *hw, Iface: hw.Iface, Country: cfg.System.Country, Width: rc.Width}
			band, channel := rc.Band, rc.Channel
			if role == config.RoleAPWAN {
				spec.Iface, spec.Virtual = wifi.VifName(hw.Phy, 0), true
				// A shared radio has one channel, and the uplink decides
				// it: the access point sits on the channel the station
				// is using or, while it is not connected, on the channel
				// of the saved network it is most likely to join. An
				// access point parked elsewhere would stop the station
				// from ever associating. With nothing to follow it stays
				// where it last was: moving costs every device its
				// connection, and is only worth it for an uplink.
				freq := 0
				if st := staStatus[rc.ID]; st.State == "connected" && st.Freq != 0 {
					freq = st.Freq
				} else if sta := c.stas[rc.ID]; sta != nil {
					if f, ok := sta.KnownInRange(); ok {
						freq, knownFreq[rc.ID] = f, f
					}
				}
				if freq == 0 {
					freq = c.apShared[rc.ID]
				}
				if freq != 0 && hw.Band(wifi.BandOfFreq(freq)) != nil {
					band, channel = wifi.BandOfFreq(freq), wifi.FreqToChannel(freq)
					c.apShared[rc.ID] = freq
				}
			} else {
				delete(c.apShared, rc.ID)
			}
			if band == "" || hw.Band(band) == nil {
				band = "2.4"
				if b := hw.Band("5"); b != nil {
					if _, ok := wifi.PickChannel(b, 0); ok {
						band = "5"
					}
				}
			}
			if b := hw.Band(band); b != nil {
				ch, ok := wifi.PickChannel(b, channel)
				if !ok {
					ch, ok = wifi.PickChannel(b, 0)
				}
				if role == config.RoleAP && channel == 0 && band == "5" {
					// Left to choose, keep clear of an uplink on
					// another radio: two radios a few centimetres
					// apart on neighbouring channels deafen each
					// other.
					if away, found := wifi.ChannelAwayFrom(b, c.uplinkFreq(rc.ID), c.apAuto[rc.ID]); found {
						ch, ok = away, true
						c.apAuto[rc.ID] = away
					}
				}
				if ok {
					spec.Band, spec.Channel = band, ch
					spec.BSS = wifi.Networks(*hw, spec.Iface, ssids, fw.Bridge)
					wantAP[rc.ID] = spec
				}
			}
		}
	}

	// Uplinks, ranked as configured. They are worked out before anything
	// is started so that an interface that stops being an uplink is let
	// go of first: an access point must not start on an interface that
	// still has the upstream network's address on it.
	// The order is carried out by the routing override further down, not
	// by route metrics: those stay what the kind of uplink gives them, so
	// that reordering never costs an uplink its lease.
	rank := func(key string) int {
		for i, k := range cfg.WAN.Order {
			if k == key {
				return i
			}
		}
		return len(cfg.WAN.Order)
	}
	ethMAC := ""
	if cfg.WAN.EthMAC.Mode != config.MACHardware {
		ethMAC = cfg.WAN.EthMAC.Address
	}
	var uplinks []wan.Uplink
	lanPorts := map[string][]string{}
	for i, e := range cfg.Ethernet {
		hw, here := present[e.Name]
		if !here {
			continue
		}
		switch e.Role {
		case config.EthWAN:
			if err := netx.SetMAC(e.Name, ethMAC); err != nil {
				c.log.Warn("setting wired MAC", "if", e.Name, "err", err)
			}
			uplinks = append(uplinks, wan.Uplink{If: e.Name, Kind: "eth", Key: config.UplinkEth(e.Name), Label: hw.Label, Metric: 100 + i, Rank: rank(config.UplinkEth(e.Name)), IP: e.IP})
		case config.EthLAN:
			_ = netx.SetMAC(e.Name, "")
			lanPorts[e.Network] = append(lanPorts[e.Network], e.Name)
		}
	}
	nPhone := 0
	for _, p := range c.ports {
		if p.Phone {
			uplinks = append(uplinks, wan.Uplink{If: p.Name, Kind: "phone", Key: config.UplinkPhone, Label: p.Label, Metric: 300 + nPhone, Rank: rank(config.UplinkPhone)})
			nPhone++
		}
	}
	staIDs := make([]string, 0, len(wantSTA))
	for id := range wantSTA {
		staIDs = append(staIDs, id)
	}
	sort.Strings(staIDs)
	for i, id := range staIDs {
		up := wan.Uplink{If: wantSTA[id].Iface, Kind: "wifi", Key: config.UplinkWifi, Metric: 200 + i, Rank: rank(config.UplinkWifi)}
		// How a wireless uplink gets its addresses belongs to the network
		// it is on (or is joining).
		if ssid := staStatus[id].SSID; ssid != "" {
			for _, n := range cfg.WAN.Networks {
				if n.SSID == ssid {
					up.IP = n.IP
				}
			}
		}
		uplinks = append(uplinks, up)
	}

	for id, sta := range c.stas {
		if _, ok := wantSTA[id]; !ok {
			delete(c.staMAC, sta.Spec().Iface)
			sta.Stop()
			delete(c.stas, id)
		}
	}
	for id, ap := range c.aps {
		if _, ok := wantAP[id]; !ok {
			ap.Stop()
			delete(c.aps, id)
			delete(c.apKeys, id)
		}
	}
	wifiMAC := ""
	if cfg.WAN.WifiMAC.Mode != config.MACHardware {
		wifiMAC = cfg.WAN.WifiMAC.Address
	}
	apKey := func(spec wifi.APSpec) string {
		return fmt.Sprintf("%s|%v|%s|%d|%d|%s|%+v", spec.Iface, spec.Virtual, spec.Band, spec.Channel, spec.Width, spec.Country, spec.BSS)
	}
	// An access point that is about to change is stopped before stations
	// start: when a radio changes role, its interface has to be free.
	for id, spec := range wantAP {
		if ap := c.aps[id]; ap != nil && c.apKeys[id] != apKey(spec) {
			ap.Stop()
		}
	}
	c.wan.Apply(uplinks, cfg.WAN.Probe)
	for id, spec := range wantSTA {
		sta := c.stas[id]
		// The address is set when the policy for it changes, and once
		// when a radio is first used. Not whenever the interface's
		// address differs from the one wanted: an adapter that will not
		// take the address would have its connection torn down on every
		// pass.
		if applied, seen := c.staMAC[spec.Iface]; !seen || applied != wifiMAC {
			c.staMAC[spec.Iface] = wifiMAC
			cur, perm, _ := netx.HardwareAddr(spec.Iface)
			want := wifiMAC
			if want == "" {
				want = perm
			}
			if cur != want {
				if sta != nil {
					sta.Stop()
				}
				if err := netx.SetMAC(spec.Iface, wifiMAC); err != nil {
					c.log.Warn("setting Wi-Fi MAC", "if", spec.Iface, "err", err)
				} else if now, _, _ := netx.HardwareAddr(spec.Iface); now != want {
					c.log.Warn("this Wi-Fi adapter does not take the address asked for", "if", spec.Iface, "wanted", want, "has", now)
				}
			}
		}
		if sta == nil {
			sta = &wifi.STA{Log: c.log, RunDir: c.opt.RunDir, OnChange: c.Kick}
			c.stas[id] = sta
		}
		if err := sta.Start(spec); err != nil {
			c.log.Error("starting station", "err", err)
		}
		// On a shared radio the station joins a network only once the
		// access point is on that network's channel (or has shown that it
		// cannot be: the uplink matters more than being tidy).
		hold := false
		if ap, shared := wantAP[id]; shared && knownFreq[id] != 0 && staStatus[id].State != "connected" {
			settled := false
			if cur := c.aps[id]; cur != nil && c.apKeys[id] == apKey(ap) {
				st := cur.Status().State
				settled = st == "up" || st == "failed"
			}
			hold = !settled
		}
		sta.Steer(hold)
	}
	for id, spec := range wantAP {
		key := apKey(spec)
		if c.apKeys[id] == key {
			continue
		}
		ap := c.aps[id]
		if ap == nil {
			ap = &wifi.AP{Log: c.log, RunDir: c.opt.RunDir, OnChange: c.Kick}
			c.aps[id] = ap
		}
		if !spec.Virtual {
			// A radio that used to be an uplink goes back to its own
			// address.
			_ = netx.SetMAC(spec.Iface, "")
			delete(c.staMAC, spec.Iface)
		}
		ap.Start(spec)
		c.apKeys[id] = key
	}

	states := c.wan.States()
	active := wan.Active(states)

	// Move a network out of the way of the upstream network (and of the
	// router's other networks, should it have to move onto one's range).
	if cfg.LAN.AutoRenumber {
		var upstream []netip.Prefix
		for _, s := range states {
			if p, err := netip.ParsePrefix(s.IPv4); err == nil {
				upstream = append(upstream, p)
			}
		}
		for i := range cfg.Networks {
			n := &cfg.Networks[i]
			avoid := append([]netip.Prefix(nil), upstream...)
			cur := netip.MustParsePrefix(n.Address)
			collides := false
			for _, u := range upstream {
				collides = collides || u.Masked().Overlaps(cur.Masked())
			}
			if !collides {
				continue
			}
			for j, o := range cfg.Networks {
				if j != i {
					avoid = append(avoid, netip.MustParsePrefix(o.Address))
				}
			}
			next, ok := lan.Renumber(cur, avoid)
			if !ok {
				continue
			}
			c.addEvent("lan", fmt.Sprintf("%s moved to %s: %s is in use upstream", n.Name, next.Masked(), cur.Masked()), "")
			n.Address = next.String()
			for d := range cfg.Devices {
				if a, err := netip.ParseAddr(cfg.Devices[d].IP); err == nil && cur.Masked().Contains(a) {
					if m, ok := lan.Remap(a, cur, next); ok {
						cfg.Devices[d].IP = m.String()
					} else {
						cfg.Devices[d].IP = ""
					}
				}
			}
			changedCfg = true
		}
	}

	// Captive portal: one device is let straight through to sign in.
	captive := active != nil && active.Internet == wan.StCaptive
	devList := c.devs.List()
	if captive {
		online := func(mac string) bool {
			for _, d := range devList {
				if d.MAC == mac {
					return d.Online
				}
			}
			return false
		}
		if c.portalDev == "" || (!c.portalManual && !online(c.portalDev)) {
			c.portalDev = ""
			var first int64
			for _, d := range devList {
				if d.Online && (first == 0 || d.OnlineSince < first) {
					first, c.portalDev = d.OnlineSince, d.MAC
				}
			}
		}
		if !c.wasCaptive {
			c.addEvent("portal", "Captive portal detected on the upstream network", c.portalDev)
		}
	} else {
		if c.wasCaptive && active != nil && active.Internet == wan.StOnline {
			c.addEvent("portal", "Captive portal cleared; internet is reachable", "")
		}
		c.portalDev, c.portalManual = "", false
	}
	c.wasCaptive = captive
	var portalDevs []string
	if c.portalDev != "" {
		portalDevs = []string{c.portalDev}
	}

	// LAN.
	routerAddr := netip.MustParsePrefix(cfg.Main().Address)
	d := lan.Desired{BridgeMAC: bridgeMAC(c.identity.Public), LeaseMinutes: cfg.LAN.LeaseMinutes, Domain: cfg.LAN.Domain}
	for _, n := range cfg.Networks {
		ln := lan.Net{Bridge: fw.Bridge(n.ID), Addr: netip.MustParsePrefix(n.Address), Ports: lanPorts[n.ID]}
		if cfg.LAN.IPv6 {
			ula, pfx := n.ULA()
			ln.ULA = netip.PrefixFrom(ula, pfx.Bits())
		}
		d.Nets = append(d.Nets, ln)
	}
	for _, dev := range cfg.Devices {
		if dev.IP != "" {
			d.Reservations = append(d.Reservations, lan.Reservation{MAC: dev.MAC, IP: dev.IP})
		}
	}
	if captive && active.CaptiveAPI != "" {
		d.CaptiveURL = active.CaptiveAPI
	}
	renumbered, err := c.lan.Apply(d)
	if err != nil {
		c.log.Error("LAN setup", "err", err)
	}
	if renumbered && c.lanApplied {
		// Clients still hold leases for the old subnet; make wireless
		// ones reconnect so they ask again.
		for id := range c.apKeys {
			delete(c.apKeys, id)
		}
		c.Kick()
	}
	c.lanApplied = true

	// Tunnels.
	// From here on the VPN profiles are seen as they run: an MPQUIC
	// profile as the WireGuard hop to its mpquic. The stored
	// configuration is left as it is.
	c.mpq.Apply(cfg.VPNs)
	running := *cfg
	running.VPNs = c.mpq.Effective(cfg.VPNs)
	c.vpn.Apply(running.VPNs)

	// Firewall and routing.
	var wanIfs []string
	for _, u := range uplinks {
		wanIfs = append(wanIfs, u.If)
	}
	plan := fw.BuildPlan(&running, fw.Runtime{WANIfs: wanIfs, PortalDevs: portalDevs, Geo: c.geo.Ranges, GeoVersion: c.geo.Version()})
	flush := false
	if key := fw.Fingerprint(plan); key != c.planKey {
		if err := c.fwm.Apply(plan); err != nil {
			c.log.Error("firewall", "err", err)
		} else {
			flush = c.planKey != ""
			c.planKey = key
		}
	}
	if !c.acctEnabled && c.planKey != "" {
		// Per-connection byte counters for the flow list. The setting
		// only exists once connection tracking is loaded, which the
		// ruleset above has just seen to.
		netx.Sysctl("net/netfilter/nf_conntrack_acct", "1")
		c.acctEnabled = true
	}
	routes := c.vpn.Routes(running.VPNs)
	// When the best-ranked uplink has an address but no way out, and a
	// lower one works, traffic is sent past it.
	var override *fw.UplinkOverride
	if pref := wan.Preferred(states); active != nil && pref != nil && pref.If != active.If {
		override = &fw.UplinkOverride{If: active.If, V6: active.HasV6}
		override.Gw4, _ = netip.ParseAddr(active.Gateway)
		if active.HasV6 {
			override.Gw6, _ = netx.DefaultRoute(active.If, unix.AF_INET6)
		}
	}
	if (override == nil) != (c.overrideIf == "") || (override != nil && override.If != c.overrideIf) {
		c.overrideIf = ""
		if override != nil {
			c.overrideIf = override.If
			c.log.Info("uplink override", "use", active.If, "instead_of", wan.Preferred(states).If)
		}
	}
	if key := fmt.Sprintf("%+v|%+v", routes, override); key != c.routeKey {
		if err := fw.SyncRouting(routes, override); err != nil {
			c.log.Error("policy routing", "err", err)
		} else {
			flush = flush || c.routeKey != ""
			c.routeKey = key
		}
	}
	if flush {
		// Paths changed: existing flows must be classified again rather
		// than continue on a route that no longer applies to them.
		if err := fw.FlushConntrack(); err != nil {
			c.log.Debug("conntrack flush", "err", err)
		} else {
			c.log.Debug("connection tracking flushed: paths changed")
		}
	}

	c.applyDNS(&running, states, active, portalDevs, routerAddr)
	c.applySystem(cfg)

	if changedCfg {
		cfg.Rev++
		if err := c.store.Save(cfg); err != nil {
			c.log.Error("saving configuration", "err", err)
		}
	}
	c.publishStatus()
}

func (c *Core) applyDNS(cfg *config.Config, states []wan.State, active *wan.State, portalDevs []string, router netip.Prefix) {
	pol := &dnsfwd.Policy{
		Hosts:       map[string][]netip.Addr{},
		Reverse:     map[netip.Addr]string{},
		Blocked:     dnsfwd.NewSuffixSet(cfg.DNS.Blocked),
		Rules:       dnsfwd.RulesFrom(cfg),
		Adblock:     cfg.DNS.Adblock.Enabled,
		AdAllow:     dnsfwd.NewSuffixSet(cfg.DNS.Adblock.Allow),
		AdExempt:    map[string]bool{},
		Paused:      map[string]bool{},
		DeviceRoute: map[string]string{},
		Default:     cfg.Policy.Default,
		PortalDevs:  map[string]bool{},
		VPN:         map[string]dnsfwd.VPNDNS{},
		NetRoute:    dnsfwd.NetRoutesFrom(cfg),
	}
	// The forwarder answers on the router's address in every network;
	// the router's own name is its address in the main one.
	listen := []string{"127.0.0.1:53"}
	own := []netip.Addr{router.Addr()}
	for i, n := range cfg.Networks {
		a, _ := n.Prefix()
		listen = append(listen, net.JoinHostPort(a.String(), "53"))
		if cfg.LAN.IPv6 && fw.HaveIPv6() {
			ula, _ := n.ULA()
			listen = append(listen, net.JoinHostPort(ula.String(), "53"))
			if i == 0 {
				own = append(own, ula)
			}
		}
	}
	for _, n := range []string{"porter", "porter." + cfg.LAN.Domain} {
		pol.Hosts[n] = own
	}
	for _, a := range own {
		pol.Reverse[a] = "porter." + cfg.LAN.Domain
	}
	for name, addrs := range c.devs.Hosts() {
		name = strings.ToLower(name)
		if _, taken := pol.Hosts[name]; taken {
			continue
		}
		pol.Hosts[name] = addrs
		pol.Hosts[name+"."+cfg.LAN.Domain] = addrs
		for _, a := range addrs {
			pol.Reverse[a] = name + "." + cfg.LAN.Domain
		}
	}
	for _, d := range cfg.Devices {
		if d.Paused {
			pol.Paused[d.MAC] = true
		}
		if d.Route != "" {
			pol.DeviceRoute[d.MAC] = d.Route
		}
		if d.NoAdblock {
			pol.AdExempt[d.MAC] = true
		}
	}
	c.ads.Configure(cfg.DNS.Adblock.Enabled, cfg.DNS.Adblock.Lists)
	for _, m := range portalDevs {
		pol.PortalDevs[m] = true
	}
	var wanDNS []string
	if active != nil {
		wanDNS = active.DNS
	} else {
		for _, s := range states {
			wanDNS = append(wanDNS, s.DNS...)
		}
	}
	for _, s := range wanDNS {
		if u, err := config.ParseUpstream(s); err == nil {
			pol.WAN = append(pol.WAN, u)
		}
	}
	for _, s := range cfg.DNS.Upstreams {
		if u, err := config.ParseUpstream(s); err == nil {
			pol.Direct = append(pol.Direct, u)
			pol.DirectCustom = true
		}
	}
	if !pol.DirectCustom {
		pol.Direct = pol.WAN
	}
	for i, v := range cfg.VPNs {
		vd := dnsfwd.VPNDNS{Mark: fw.MarkVPN(i)}
		for _, s := range v.WireGuard.DNS {
			if u, err := config.ParseUpstream(s); err == nil {
				vd.Servers = append(vd.Servers, u)
			}
		}
		pol.VPN[v.ID] = vd
	}
	keyBytes, _ := json.Marshal(struct {
		P *dnsfwd.Policy
		L []string
	}{pol, listen})
	if key := string(keyBytes); key != c.dnsKey {
		c.dns.SetPolicy(pol)
		if err := c.dns.Listen(listen); err != nil {
			c.log.Warn("DNS listener", "err", err)
			return // retry on the next pass
		}
		c.dnsKey = key
	}
}

func (c *Core) applySystem(cfg *config.Config) {
	key := fmt.Sprintf("%+v", cfg.System.SSH)
	if key == c.sysKey {
		return
	}
	c.sysKey = key
	if err := system.SetSSH(c.opt.DataDir, cfg.System.SSH.Enabled, cfg.System.SSH.AuthorizedKeys); err != nil {
		c.log.Warn("SSH setup", "err", err)
	}
}

func (c *Core) geoDataPath() string { return filepath.Join(c.opt.DataDir, "geoip.csv.gz") }

// online reports whether the active uplink reaches the internet. Safe
// from any goroutine.
func (c *Core) online() bool {
	a := wan.Active(c.wan.States())
	return a != nil && a.Internet == wan.StOnline
}

// runGeoUpdates keeps the country database current. DB-IP publishes one
// per month; the router looks for the current month's once it is online,
// checks that the download is a complete database, and swaps it in.
func (c *Core) runGeoUpdates(ctx context.Context) {
	urlTemplate := geoip.URLTemplate
	minRanges := geoip.MinRanges
	// Overridable so tests can serve a small database locally.
	if v := os.Getenv("PORTER_GEOIP_URL"); v != "" {
		urlTemplate = v
	}
	if v, err := strconv.Atoi(os.Getenv("PORTER_GEOIP_MIN")); err == nil {
		minRanges = v
	}
	client := &http.Client{}
	var lastTry time.Time
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		forced := false
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-c.geoCheck:
			forced = true
		}
		month := geoip.Month(time.Now())
		var have string
		c.do(func() { have = c.geo.Version() })
		if have >= month || !c.online() || (!forced && time.Since(lastTry) < 12*time.Hour) {
			continue
		}
		lastTry = time.Now()
		db, notFound, err := geoip.Download(ctx, client, urlTemplate, month, c.geoDataPath(), minRanges)
		switch {
		case notFound:
			c.log.Debug("this month's country database is not published yet", "month", month)
		case err != nil:
			c.log.Warn("country database update failed", "err", err)
		default:
			c.do(func() { c.geo = db })
			c.addEvent("update", "Country database updated to "+month, "")
			c.Kick() // country rules are re-applied with the new ranges
		}
	}
}

// resolveDirect looks a name up through the uplink (used for VPN
// endpoints, which by definition cannot be resolved through the VPN).
func (c *Core) resolveDirect(ctx context.Context, host string) ([]netip.Addr, error) {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, "127.0.0.1:53")
	}}
	return r.LookupNetIP(ctx, "ip", host)
}

// bridgeMAC derives a stable locally administered address for the LAN
// bridge, so that clients' neighbour caches survive membership changes.
func bridgeMAC(seed []byte) string {
	if len(seed) < 5 {
		return ""
	}
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", seed[0], seed[1], seed[2], seed[3], seed[4])
}

// uplinkFreq returns the frequency a station on another radio than the
// given one is connected on, or 0.
func (c *Core) uplinkFreq(radio string) int {
	for id, sta := range c.stas {
		if id == radio {
			continue
		}
		if st := sta.Status(); st.State == "connected" && st.Freq != 0 {
			return st.Freq
		}
	}
	return 0
}

// wiredPort is a physical non-wireless network interface.
type wiredPort struct {
	Name string
	// Phone: the interface is a phone sharing its connection over USB.
	Phone bool
	// Label is what a USB device calls itself.
	Label string
}

func wiredPorts() []wiredPort {
	entries, _ := os.ReadDir("/sys/class/net")
	var out []wiredPort
	for _, e := range entries {
		name := e.Name()
		base := filepath.Join("/sys/class/net", name)
		if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
			// Virtual interface. Test rigs use veth pairs named like
			// real ports, which are accepted.
			if !(strings.HasPrefix(name, "eth") || strings.HasPrefix(name, "lan") || strings.HasPrefix(name, "usb")) || name == "lo" {
				continue
			}
		}
		if wifi.IsWireless(name) || strings.HasPrefix(name, "wg-") || strings.HasPrefix(name, "br-") {
			continue
		}
		p := wiredPort{Name: name}
		driver := ""
		if l, err := os.Readlink(filepath.Join(base, "device", "driver")); err == nil {
			driver = filepath.Base(l)
		}
		// For USB the product string sits on the device above the
		// interface the driver bound to.
		if b, err := os.ReadFile(filepath.Join(base, "device", "..", "product")); err == nil {
			p.Label = strings.TrimSpace(string(b))
		}
		p.Phone = isPhone(name, driver, readTrim(filepath.Join(base, "address")))
		if p.Phone && p.Label == "" {
			p.Label = "Phone"
			if driver == "ipheth" {
				p.Label = "iPhone"
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

// isPhone decides whether a wired interface is a phone sharing its
// connection over USB rather than an Ethernet port, from the driver that
// runs it and its address.
func isPhone(name, driver, address string) bool {
	switch driver {
	case "ipheth":
		// An iPhone with Personal Hotspot.
		return true
	case "rndis_host":
		// The way older Android phones tether.
		return true
	case "cdc_ncm", "cdc_ether", "cdc_eem":
		// Newer Android phones, and also many real Ethernet adapters.
		// A phone makes its address up (the "locally administered" bit
		// is set); an adapter has one from its manufacturer.
		hw, err := net.ParseMAC(address)
		return err == nil && len(hw) == 6 && hw[0]&0x02 != 0
	case "":
		// How test rigs name a stand-in for a phone.
		return strings.HasPrefix(name, "usb")
	}
	return false
}

// radioPlan is what the configuration asks of the radios that are
// present, as far as they can do it.
type radioPlan struct {
	hw    map[string]*wifi.Radio   // radios present and usable, by id
	role  map[string]string        // the role each will actually have
	ssids map[string][]config.SSID // the Wi-Fi networks each carries
	// offAir lists the ids of the Wi-Fi networks that are switched on but
	// fit on no radio, and notes says, for people, what could not be done
	// as asked.
	offAir []string
	notes  []string
}

func radioName(r *wifi.Radio) string {
	if r.ID == "builtin" {
		return "The built-in radio"
	}
	if r.Product != "" {
		return r.Product
	}
	return "The USB adapter"
}

// planRadios works out which radio does what. It depends on nothing but
// its arguments, so it can also say what a configuration that is only
// being considered would lead to.
func planRadios(cfg *config.Config, radios []wifi.Radio) radioPlan {
	p := radioPlan{hw: map[string]*wifi.Radio{}, role: map[string]string{}, ssids: map[string][]config.SSID{}}
	var on []config.SSID
	for _, w := range cfg.Wifi {
		if w.Enabled && w.SSID != "" && w.Passphrase != "" {
			on = append(on, w)
		}
	}
	var aps []string
	var capacity []int
	for _, rc := range cfg.Radios {
		var hw *wifi.Radio
		for i := range radios {
			if radios[i].ID == rc.ID {
				hw = &radios[i]
			}
		}
		if hw == nil || hw.Iface == "" {
			continue
		}
		p.hw[rc.ID] = hw
		role := rc.Role
		if role == config.RoleAPWAN && !hw.APSTA {
			// Not possible on this radio; the uplink matters more.
			role = config.RoleWAN
			p.notes = append(p.notes, radioName(hw)+" cannot broadcast your network and join another at the same time. It will only join.")
		}
		if (role == config.RoleWAN || role == config.RoleAPWAN) && !hw.STA {
			role = config.RoleOff
		}
		if (role == config.RoleAP || role == config.RoleAPWAN) && !hw.AP {
			p.notes = append(p.notes, radioName(hw)+" cannot broadcast a network.")
			role = map[string]string{config.RoleAP: config.RoleOff, config.RoleAPWAN: config.RoleWAN}[role]
		}
		p.role[rc.ID] = role
		switch role {
		case config.RoleAP:
			aps, capacity = append(aps, rc.ID), append(capacity, max(hw.MaxAP, 1))
		case config.RoleAPWAN:
			aps, capacity = append(aps, rc.ID), append(capacity, max(hw.MaxAPSTA, 1))
		}
	}
	carried := map[int]bool{}
	for r, picks := range wifi.Assign(capacity, len(on)) {
		for _, i := range picks {
			p.ssids[aps[r]] = append(p.ssids[aps[r]], on[i])
			carried[i] = true
		}
	}
	for i, w := range on {
		if carried[i] || len(aps) == 0 {
			continue
		}
		p.offAir = append(p.offAir, w.ID)
		most := 0
		for _, c := range capacity {
			most = max(most, c)
		}
		if most <= 1 {
			p.notes = append(p.notes, fmt.Sprintf("“%s” will not be on air: this radio broadcasts one network at a time. A USB Wi-Fi adapter that can carry several, or a second radio for it, would.", w.SSID))
		} else {
			p.notes = append(p.notes, fmt.Sprintf("“%s” will not be on air: the radios cannot carry that many networks.", w.SSID))
		}
	}
	return p
}
