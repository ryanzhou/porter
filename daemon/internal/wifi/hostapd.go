package wifi

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/proc"
)

// VifName names the interfaces this package creates on a radio: the
// access point's own when the radio's first interface is busy being a
// station (n = 0), and one for each further Wi-Fi network (n >= 1). The
// names are short on purpose: an interface name has 15 characters, and an
// adapter's own name can use all of them.
func VifName(phy string, n int) string {
	idx := strings.TrimPrefix(phy, "phy")
	if idx == phy || idx == "" {
		idx = "x" + phy
	}
	if len(idx) > 8 {
		idx = idx[len(idx)-8:]
	}
	if n == 0 {
		return "ap" + idx
	}
	return fmt.Sprintf("ap%s-%d", idx, n)
}

var reVif = regexp.MustCompile(`^ap(-|[0-9x])`)

// isVif reports whether an interface is one of this package's own.
func isVif(name string) bool { return reVif.MatchString(name) }

// BSS is one Wi-Fi network on an access point.
type BSS struct {
	Iface  string
	Bridge string
	// BSSID is the address to give the interface; empty for the first
	// network of a radio, which uses the interface's own.
	BSSID string
	SSID  config.SSID
}

// APSpec is one access point to run: a radio on a channel, carrying one
// or more Wi-Fi networks.
type APSpec struct {
	Radio   Radio
	Iface   string // interface hostapd runs on; that of the first network
	Virtual bool   // Iface must be created on the PHY first
	Country string
	BSS     []BSS
	Band    string
	Channel int // concrete channel
	Width   int // maximum width, 0 = unrestricted
}

// Networks builds the list of Wi-Fi networks for an access point whose
// first network is on iface: one interface each, the further ones with
// addresses derived from the radio's.
func Networks(r Radio, iface string, ssids []config.SSID, bridge func(network string) string) []BSS {
	var out []BSS
	for i, s := range ssids {
		b := BSS{Iface: iface, Bridge: bridge(s.Network), SSID: s}
		if i > 0 {
			b.Iface = VifName(r.Phy, i)
			b.BSSID = derivedMAC(r.MAC, i)
		}
		out = append(out, b)
	}
	return out
}

// derivedMAC is the address of a radio's n-th further network: the
// radio's own with the "locally administered" bit set and n folded into
// the bits above it. This is the scheme drivers that filter by address
// expect.
func derivedMAC(base string, n int) string {
	hw, err := net.ParseMAC(base)
	if err != nil || len(hw) != 6 {
		return ""
	}
	b := append(net.HardwareAddr(nil), hw...)
	b[0] = (b[0] ^ byte(n<<2)) | 0x02
	return b.String()
}

// Assign decides which of n Wi-Fi networks each access point radio
// carries, given how many each can carry at once. Every radio takes the
// first ones in order; then, where a network is left on no radio at all
// and some radio is carrying only networks that another radio has too, that
// radio gives up its last one for it. So two radios that can carry one
// network each carry one each, rather than the same one twice.
func Assign(capacity []int, n int) [][]int {
	out := make([][]int, len(capacity))
	count := make([]int, n)
	for r, c := range capacity {
		for i := 0; i < n && i < c; i++ {
			out[r] = append(out[r], i)
			count[i]++
		}
	}
	for i := 0; i < n; i++ {
		if count[i] > 0 {
			continue
		}
		// The radio to take it: one whose last network is the most
		// widely carried elsewhere.
		best, bestCount := -1, 1
		for r := range out {
			if len(out[r]) == 0 {
				continue
			}
			if last := out[r][len(out[r])-1]; count[last] > bestCount {
				best, bestCount = r, count[last]
			}
		}
		if best < 0 {
			continue
		}
		last := out[best][len(out[best])-1]
		count[last]--
		out[best][len(out[best])-1] = i
		count[i]++
	}
	return out
}

// variant is one rung of the capability ladder: hostapd refuses to start
// when asked for more than a driver can do, and what a given USB adapter
// can do in AP mode is not reliably discoverable up front. So the widest
// plausible configuration is tried first and narrower ones after it.
type variant struct {
	he, vht, ht bool
	width       int
}

func (v variant) String() string {
	mode := "legacy"
	switch {
	case v.he:
		mode = "ax"
	case v.vht:
		mode = "ac"
	case v.ht:
		mode = "n"
	}
	return fmt.Sprintf("%s/%dMHz", mode, v.width)
}

func ladder(s APSpec) []variant {
	b := s.Radio.Band(s.Band)
	if b == nil {
		return []variant{{width: 20}}
	}
	maxW := s.Width
	if maxW == 0 {
		maxW = 80
	}
	_, _, can40, can80 := chanLayout(s.Band, s.Channel, b)
	var out []variant
	add := func(v variant) {
		if v.width > maxW {
			return
		}
		out = append(out, v)
	}
	if s.Band == "5" {
		if b.VHT && can80 {
			if b.HE {
				add(variant{he: true, vht: true, ht: true, width: 80})
			}
			add(variant{vht: true, ht: true, width: 80})
		}
		if b.HT && can40 {
			add(variant{ht: true, width: 40})
		}
	}
	if b.HT {
		if b.HE && s.Band == "2.4" {
			add(variant{he: true, ht: true, width: 20})
		}
		add(variant{ht: true, width: 20})
	}
	add(variant{width: 20})
	return out
}

// chanLayout returns the 40 MHz secondary channel direction and the
// 80 MHz centre channel for a primary channel, and whether every channel
// those widths would occupy is usable.
func chanLayout(band string, ch int, b *Band) (ht40 string, center80 int, can40, can80 bool) {
	if band != "5" {
		return "", 0, false, false
	}
	base := 0
	for _, start := range []int{36, 52, 100, 116, 132, 149} {
		if ch >= start && ch <= start+12 && (ch-start)%4 == 0 {
			base = start
		}
	}
	if base == 0 {
		return "", 0, false, false
	}
	usable := func(n int) bool {
		for _, c := range b.Channels {
			if c.Num == n {
				return c.Usable()
			}
		}
		return false
	}
	idx := (ch - base) / 4
	pair := ch + 4
	ht40 = "[HT40+]"
	if idx%2 == 1 {
		pair = ch - 4
		ht40 = "[HT40-]"
	}
	can40 = usable(ch) && usable(pair)
	center80 = base + 6
	can80 = usable(base) && usable(base+4) && usable(base+8) && usable(base+12)
	return ht40, center80, can40, can80
}

// PickChannel resolves a configured channel (0 = automatic) to one the
// radio may start an AP on without radar detection.
func PickChannel(b *Band, want int) (int, bool) {
	usable := map[int]bool{}
	for _, c := range b.Channels {
		if c.Usable() {
			usable[c.Num] = true
		}
	}
	if want != 0 {
		return want, usable[want]
	}
	prefs := []int{6, 1, 11}
	if b.Name == "5" {
		prefs = []int{36, 44, 149, 157, 40, 48, 153, 161, 165}
	}
	for _, c := range prefs {
		if usable[c] {
			return c, true
		}
	}
	return 0, false
}

// ChannelAwayFrom chooses the 5 GHz channel for an access point whose
// radio sits beside another radio that is connected upstream on
// uplinkFreq. Two radios centimetres apart on neighbouring channels
// drown each other out, so the access point goes to the other end of the
// band; where the rules leave no channel far enough away, it shares the
// uplink's own channel, where the two at least take turns. keep is the
// channel chosen before (0 for none) and is kept while it is still far
// enough, so that the access point does not move without need. The
// result is false when there is nothing to avoid or nothing to choose.
func ChannelAwayFrom(b *Band, uplinkFreq, keep int) (int, bool) {
	const far = 160 // MHz: an 80 MHz channel and room to spare
	freq := map[int]int{}
	for _, c := range b.Channels {
		if c.Usable() {
			freq[c.Num] = c.Freq
		}
	}
	if f, ok := freq[keep]; ok && (uplinkFreq < 4900 || abs(f-uplinkFreq) >= far) {
		return keep, true
	}
	if uplinkFreq < 4900 {
		return 0, false
	}
	best, bestGap := 0, 0
	for _, ch := range []int{36, 44, 149, 157, 40, 48, 153, 161, 165} {
		if f, ok := freq[ch]; ok && abs(f-uplinkFreq) > bestGap {
			best, bestGap = ch, abs(f-uplinkFreq)
		}
	}
	if bestGap >= far {
		return best, true
	}
	for ch, f := range freq {
		if f == uplinkFreq {
			return ch, true
		}
	}
	return 0, false
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func hostapdConf(s APSpec, v variant, ctrlDir string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# generated by porterd (%s)", v)
	w("interface=%s", s.Iface)
	w("driver=nl80211")
	w("country_code=%s", s.Country)
	w("ieee80211d=1")
	if s.Band == "5" {
		w("hw_mode=a")
	} else {
		w("hw_mode=g")
	}
	w("channel=%d", s.Channel)
	ht40, center80, _, _ := chanLayout(s.Band, s.Channel, s.Radio.Band(s.Band))
	if v.ht {
		w("ieee80211n=1")
		if v.width >= 40 && ht40 != "" {
			w("ht_capab=%s", ht40)
		}
	}
	if v.vht {
		w("ieee80211ac=1")
		if v.width >= 80 {
			w("vht_oper_chwidth=1")
			w("vht_oper_centr_freq_seg0_idx=%d", center80)
		} else {
			w("vht_oper_chwidth=0")
		}
	}
	if v.he {
		w("ieee80211ax=1")
		if v.width >= 80 {
			w("he_oper_chwidth=1")
			w("he_oper_centr_freq_seg0_idx=%d", center80)
		}
	}
	for i, n := range s.BSS {
		if i > 0 {
			// Everything from here to the next "bss=" belongs to this
			// network alone.
			w("bss=%s", n.Iface)
			if n.BSSID != "" {
				w("bssid=%s", n.BSSID)
			}
		}
		w("bridge=%s", n.Bridge)
		w("ctrl_interface=%s", ctrlDir)
		// Hex form: no SSID can break out of the config line.
		w("ssid2=%s", hex.EncodeToString([]byte(n.SSID.SSID)))
		w("utf8_ssid=1")
		w("wmm_enabled=1")
		w("wpa=2")
		w("rsn_pairwise=CCMP")
		switch n.SSID.Security {
		case config.SecWPA3:
			w("wpa_key_mgmt=SAE")
			w("ieee80211w=2")
			w("sae_pwe=2")
		case config.SecWPA2WPA3:
			w("wpa_key_mgmt=WPA-PSK SAE")
			w("ieee80211w=1")
			w("sae_pwe=2")
		default:
			w("wpa_key_mgmt=WPA-PSK")
		}
		w("wpa_passphrase=%s", n.SSID.Passphrase)
		if n.SSID.Hidden {
			w("ignore_broadcast_ssid=1")
		}
		if n.SSID.Isolate {
			w("ap_isolate=1")
		}
		w("max_num_sta=64")
		w("disassoc_low_ack=0")
	}
	return b.String()
}

// AP supervises one hostapd instance.
type AP struct {
	Log    *slog.Logger
	RunDir string
	// OnChange is called when the access point comes up, goes down or
	// gives up.
	OnChange func()

	mu     sync.Mutex
	spec   APSpec
	state  string // "starting", "up", "failed", "stopped"
	mode   string
	stop   chan struct{}
	done   chan struct{}
	failed string
}

type APStatus struct {
	Radio   string `json:"radio"`
	Iface   string `json:"iface"`
	State   string `json:"state"`
	Band    string `json:"band"`
	Channel int    `json:"channel"`
	Mode    string `json:"mode,omitempty"`
	Error   string `json:"error,omitempty"`
	// SSIDs are the ids of the Wi-Fi networks this radio carries.
	SSIDs []string `json:"ssids"`
}

func (a *AP) Status() APStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := APStatus{Radio: a.spec.Radio.ID, Iface: a.spec.Iface, State: a.state, Band: a.spec.Band, Channel: a.spec.Channel, Mode: a.mode, Error: a.failed, SSIDs: []string{}}
	for _, n := range a.spec.BSS {
		st.SSIDs = append(st.SSIDs, n.SSID.ID)
	}
	return st
}

// Ifaces lists the interfaces the access point's networks are on.
func (a *AP) Ifaces() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, n := range a.spec.BSS {
		out = append(out, n.Iface)
	}
	return out
}

// Bridge returns the bridge an access point interface belongs to.
func (a *AP) Bridge(iface string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, n := range a.spec.BSS {
		if n.Iface == iface {
			return n.Bridge, true
		}
	}
	return "", false
}

func (a *AP) changed() {
	if a.OnChange != nil {
		a.OnChange()
	}
}

func (a *AP) Spec() APSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.spec
}

func (a *AP) ctrlDir() string { return filepath.Join(a.RunDir, "hostapd") }

// Start runs the access point, replacing a previous instance.
func (a *AP) Start(spec APSpec) {
	a.Stop()
	a.mu.Lock()
	a.spec = spec
	a.state = "starting"
	a.failed = ""
	a.stop = make(chan struct{})
	a.done = make(chan struct{})
	stop, done := a.stop, a.done
	a.mu.Unlock()
	go a.run(spec, stop, done)
}

func (a *AP) Stop() {
	a.mu.Lock()
	stop, done := a.stop, a.done
	a.stop, a.done = nil, nil
	a.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	a.mu.Lock()
	a.state = "stopped"
	a.mu.Unlock()
}

func (a *AP) run(spec APSpec, stop, done chan struct{}) {
	defer close(done)
	// Interfaces left behind by an earlier instance would make hostapd
	// refuse to create its own.
	for _, n := range spec.BSS[1:] {
		exec.Command("iw", "dev", n.Iface, "del").Run()
	}
	if spec.Virtual {
		// Recreate the virtual interface from scratch each time.
		exec.Command("iw", "dev", spec.Iface, "del").Run()
		if out, err := exec.Command("iw", "phy", spec.Radio.Phy, "interface", "add", spec.Iface, "type", "__ap").CombinedOutput(); err != nil {
			a.fail(fmt.Sprintf("cannot create AP interface: %s", strings.TrimSpace(string(out))))
			return
		}
		defer exec.Command("iw", "dev", spec.Iface, "del").Run()
	}
	variants := ladder(spec)
	conf := filepath.Join(a.RunDir, "hostapd-"+spec.Iface+".conf")
	first := 0
	for {
		ranOnce := false
		retried := map[int]bool{}
		for i := first; i < len(variants) && !ranOnce; i++ {
			v := variants[i]
			if err := os.WriteFile(conf, []byte(hostapdConf(spec, v, a.ctrlDir())), 0o600); err != nil {
				a.fail(err.Error())
				return
			}
			enabled := make(chan struct{}, 1)
			exited := make(chan struct{})
			p := &proc.Proc{
				Name: "hostapd-" + spec.Iface, Log: a.Log, NoRestart: true,
				Argv: []string{"hostapd", conf},
				OnLine: func(line string) {
					if strings.Contains(line, "AP-ENABLED") {
						select {
						case enabled <- struct{}{}:
						default:
						}
					}
				},
				OnExit: func(error) { close(exited) },
			}
			began := time.Now()
			p.Start()
			up := false
			select {
			case <-enabled:
				up = true
			case <-exited:
			case <-time.After(20 * time.Second):
			case <-stop:
				p.Stop()
				return
			}
			if !up {
				p.Stop()
				// hostapd giving up at once is as likely to mean that the
				// interface was not free yet (a radio that has just
				// changed jobs) as that the radio cannot do this mode.
				// The first deserves another go; stepping down a rung
				// for it would leave the network slower for good.
				if time.Since(began) < 5*time.Second && !retried[i] {
					retried[i] = true
					a.Log.Info("access point did not start; trying once more", "iface", spec.Iface, "mode", v.String())
					select {
					case <-time.After(1500 * time.Millisecond):
					case <-stop:
						return
					}
					i--
					continue
				}
				a.Log.Warn("access point did not start in this mode", "iface", spec.Iface, "mode", v.String())
				continue
			}
			// It works: after an unexpected exit, retry this rung rather
			// than walking further down.
			ranOnce, first = true, i
			a.mu.Lock()
			a.state, a.mode, a.failed = "up", v.String(), ""
			a.mu.Unlock()
			a.Log.Info("access point up", "iface", spec.Iface, "channel", spec.Channel, "mode", v.String(), "networks", len(spec.BSS))
			a.changed()
			select {
			case <-exited:
				a.Log.Warn("hostapd exited", "iface", spec.Iface)
			case <-stop:
				p.Stop()
				return
			}
			a.mu.Lock()
			a.state = "starting"
			a.mu.Unlock()
			a.changed()
		}
		pause := 2 * time.Second
		if !ranOnce {
			first = 0
			pause = 15 * time.Second
			a.mu.Lock()
			a.state = "failed"
			a.failed = "the radio would not start an access point on channel " + strconv.Itoa(spec.Channel)
			a.mu.Unlock()
			a.changed()
		}
		select {
		case <-time.After(pause):
		case <-stop:
			return
		}
	}
}

func (a *AP) fail(msg string) {
	a.mu.Lock()
	a.state, a.failed = "failed", msg
	a.mu.Unlock()
	a.Log.Error("access point failed", "err", msg)
	a.changed()
}

// Station is a client associated to an access point.
type Station struct {
	MAC       string
	Signal    int
	Connected int // seconds
	Iface     string
}

// Stations lists the associated clients of all the access point's
// networks.
func (a *AP) Stations() []Station {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()
	if state != "up" {
		return nil
	}
	var all []Station
	for _, iface := range a.Ifaces() {
		out, err := exec.Command("hostapd_cli", "-p", a.ctrlDir(), "-i", iface, "all_sta").Output()
		if err != nil {
			continue
		}
		all = append(all, parseAllSta(string(out), iface)...)
	}
	return all
}

func parseAllSta(text, iface string) []Station {
	var out []Station
	var cur *Station
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 17 && strings.Count(line, ":") == 5 {
			out = append(out, Station{MAC: strings.ToLower(line), Iface: iface})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "signal":
			cur.Signal, _ = strconv.Atoi(v)
		case "connected_time":
			cur.Connected, _ = strconv.Atoi(v)
		}
	}
	return out
}

// Kick disconnects a client so that it reassociates (and re-runs DHCP and
// its captive portal check).
func (a *AP) Kick(mac string) {
	for _, iface := range a.Ifaces() {
		exec.Command("hostapd_cli", "-p", a.ctrlDir(), "-i", iface, "deauthenticate", mac).Run()
	}
}
