// Package wifi discovers radios and drives hostapd (access point) and
// wpa_supplicant (station) on them.
package wifi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Channel struct {
	Num      int  `json:"num"`
	Freq     int  `json:"freq"`
	Disabled bool `json:"disabled,omitempty"`
	NoIR     bool `json:"no_ir,omitempty"`
	Radar    bool `json:"radar,omitempty"`
}

// Usable reports whether an access point may start on the channel
// without radar detection.
func (c Channel) Usable() bool { return !c.Disabled && !c.NoIR && !c.Radar }

type Band struct {
	Name     string    `json:"name"` // "2.4", "5", "6"
	Channels []Channel `json:"channels"`
	HT       bool      `json:"ht"`
	VHT      bool      `json:"vht"`
	HE       bool      `json:"he"`
}

// Radio describes one Wi-Fi PHY.
type Radio struct {
	ID      string `json:"id"`
	Phy     string `json:"phy"`
	Iface   string `json:"iface"`
	Driver  string `json:"driver"`
	Bus     string `json:"bus"`
	Product string `json:"product"`
	MAC     string `json:"mac"`
	Bands   []Band `json:"bands"`
	AP      bool   `json:"ap"`
	STA     bool   `json:"sta"`
	// APSTA: the radio can run an access point and a station at once
	// (on one shared channel).
	APSTA bool `json:"ap_sta"`
	// MaxAP is how many Wi-Fi networks the radio can broadcast at once,
	// and MaxAPSTA how many while it is also a station.
	MaxAP    int `json:"max_ap"`
	MaxAPSTA int `json:"max_ap_sta"`
}

func (r *Radio) Band(name string) *Band {
	for i := range r.Bands {
		if r.Bands[i].Name == name {
			return &r.Bands[i]
		}
	}
	return nil
}

const sysPhy = "/sys/class/ieee80211"

// Discover lists the radios present.
func Discover() []Radio {
	entries, _ := os.ReadDir(sysPhy)
	phyMu.Lock()
	defer phyMu.Unlock()
	for phy := range phyInfo {
		gone := true
		for _, e := range entries {
			gone = gone && e.Name() != phy
		}
		if gone {
			delete(phyInfo, phy)
		}
	}
	var out []Radio
	for _, e := range entries {
		phy := e.Name()
		dir := filepath.Join(sysPhy, phy)
		r := Radio{Phy: phy, Bands: []Band{}}
		if b, err := os.ReadFile(filepath.Join(dir, "macaddress")); err == nil {
			r.MAC = strings.TrimSpace(string(b))
		}
		if l, err := os.Readlink(filepath.Join(dir, "device", "driver")); err == nil {
			r.Driver = filepath.Base(l)
		}
		if l, err := os.Readlink(filepath.Join(dir, "device", "subsystem")); err == nil {
			r.Bus = filepath.Base(l)
		}
		// For USB the product string sits on the device above the
		// interface the driver bound to.
		for _, p := range []string{"device/product", "device/../product"} {
			if b, err := os.ReadFile(filepath.Join(dir, p)); err == nil {
				r.Product = strings.TrimSpace(string(b))
				break
			}
		}
		r.Iface = baseIface(dir)
		switch {
		case r.Driver == "brcmfmac" && r.Bus != "usb":
			r.ID = "builtin"
			if r.Product == "" {
				r.Product = "Built-in Wi-Fi"
			}
		case r.MAC != "":
			r.ID = busTag(r.Bus) + ":" + r.MAC
		default:
			continue
		}
		// What a radio can do changes with the regulatory domain and
		// otherwise not at all, so it is not asked for on every pass. And
		// when asking fails for once, what was last known stands: a radio
		// that seemed to have lost every ability would be switched off.
		known := phyInfo[phy]
		if known.text == "" || time.Since(known.at) > time.Minute {
			if info, err := tool(5*time.Second, "iw", "phy", phy, "info").Output(); err == nil && len(info) > 0 {
				known = phyText{text: string(info), at: time.Now()}
				phyInfo[phy] = known
			}
		}
		parsePhyInfo(known.text, &r)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// busTag reduces a sysfs bus name to the short tag used in radio ids
// ("usb", "pci", ...), which must fit the id format the configuration
// accepts.
func busTag(bus string) string {
	var b []byte
	for i := 0; i < len(bus) && len(b) < 8; i++ {
		c := bus[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b = append(b, c)
		}
	}
	if len(b) < 2 {
		return "radio"
	}
	return string(b)
}

// baseIface returns the PHY's primary network interface: the one that is
// not a virtual AP interface created by this package.
func baseIface(phyDir string) string {
	entries, _ := os.ReadDir(filepath.Join(phyDir, "device", "net"))
	var names []string
	for _, e := range entries {
		if !isVif(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

var reFreq = regexp.MustCompile(`^\* (\d+)(?:\.\d+)? MHz \[(\d+)\](.*)$`)

func parsePhyInfo(text string, r *Radio) {
	var band *Band
	section := ""
	var combos []string
	flush := func() {
		if band != nil && len(band.Channels) > 0 {
			f := band.Channels[0].Freq
			switch {
			case f < 3000:
				band.Name = "2.4"
			case f < 5925:
				band.Name = "5"
			default:
				band.Name = "6"
			}
			r.Bands = append(r.Bands, *band)
		}
		band = nil
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "Band ") && strings.HasSuffix(line, ":"):
			flush()
			band = &Band{}
			section = "band"
			continue
		case line == "Supported interface modes:":
			flush()
			section = "modes"
			continue
		case line == "valid interface combinations:":
			flush()
			section = "combos"
			continue
		}
		switch section {
		case "modes":
			if !strings.HasPrefix(line, "* ") {
				section = ""
				break
			}
			switch strings.TrimPrefix(line, "* ") {
			case "AP":
				r.AP = true
			case "managed":
				r.STA = true
			}
		case "combos":
			if strings.HasPrefix(line, "* ") {
				combos = append(combos, strings.TrimPrefix(line, "* "))
			} else if len(combos) > 0 && (strings.Contains(line, "#{") || strings.HasPrefix(line, "total")) {
				combos[len(combos)-1] += " " + line
			} else {
				section = ""
			}
		case "band":
			switch {
			case strings.HasPrefix(line, "Capabilities: 0x"):
				band.HT = true
			case strings.HasPrefix(line, "VHT Capabilities"):
				band.VHT = true
			case strings.HasPrefix(line, "HE Iftypes:"):
				for _, t := range strings.Split(strings.TrimPrefix(line, "HE Iftypes:"), ",") {
					if strings.TrimSpace(t) == "AP" {
						band.HE = true
					}
				}
			default:
				if m := reFreq.FindStringSubmatch(line); m != nil {
					freq, _ := strconv.Atoi(m[1])
					num, _ := strconv.Atoi(m[2])
					band.Channels = append(band.Channels, Channel{
						Num: num, Freq: freq,
						Disabled: strings.Contains(m[3], "disabled"),
						NoIR:     strings.Contains(m[3], "no IR"),
						Radar:    strings.Contains(m[3], "radar detection"),
					})
				}
			}
		}
	}
	flush()
	for _, c := range combos {
		alone, withSTA := comboAPs(c)
		r.MaxAP, r.MaxAPSTA = max(r.MaxAP, alone), max(r.MaxAPSTA, withSTA)
	}
	if r.AP && r.MaxAP == 0 {
		// A driver that lists no combinations still does the one thing.
		r.MaxAP = 1
	}
	// More networks than this is more than anyone's radio does well.
	r.MaxAP, r.MaxAPSTA = min(r.MaxAP, 4), min(r.MaxAPSTA, 4)
	r.APSTA = r.MaxAPSTA > 0
}

var reGroup = regexp.MustCompile(`#\{([^}]*)\}\s*<=\s*(\d+)`)
var reTotal = regexp.MustCompile(`total\s*<=\s*(\d+)`)

// comboAPs reads one "valid interface combinations" entry: how many
// access point interfaces it allows by themselves, and how many next to
// one managed (station) interface. Each "#{ types } <= n" group limits how
// many interfaces of those types may exist together, and "total" limits
// them all.
func comboAPs(combo string) (alone, withSTA int) {
	apGroup, staGroup := -1, -1
	limits := map[int]int{}
	for i, g := range reGroup.FindAllStringSubmatch(combo, -1) {
		limits[i], _ = strconv.Atoi(g[2])
		for _, t := range strings.Split(g[1], ",") {
			switch strings.TrimSpace(t) {
			case "AP":
				if apGroup < 0 {
					apGroup = i
				}
			case "managed":
				if staGroup < 0 {
					staGroup = i
				}
			}
		}
	}
	if apGroup < 0 {
		return 0, 0
	}
	total := 1 << 30
	if m := reTotal.FindStringSubmatch(combo); m != nil {
		total, _ = strconv.Atoi(m[1])
	}
	alone = min(limits[apGroup], total)
	if staGroup < 0 {
		return alone, 0
	}
	withSTA = limits[apGroup]
	if staGroup == apGroup {
		withSTA--
	}
	withSTA = max(min(withSTA, total-1), 0)
	return alone, withSTA
}

// FreqToChannel converts a centre frequency in MHz to a channel number.
func FreqToChannel(freq int) int {
	switch {
	case freq == 2484:
		return 14
	case freq >= 2412 && freq <= 2472:
		return (freq - 2407) / 5
	case freq >= 5000 && freq < 5925:
		return (freq - 5000) / 5
	case freq >= 5955:
		return (freq - 5950) / 5
	}
	return 0
}

func BandOfFreq(freq int) string {
	switch {
	case freq < 3000:
		return "2.4"
	case freq < 5925:
		return "5"
	}
	return "6"
}

// SetRegDomain sets the regulatory domain for all radios.
func SetRegDomain(country string) error {
	err := tool(5*time.Second, "iw", "reg", "set", country).Run()
	Forget()
	return err
}

// Forget makes the next Discover ask the radios afresh what they can do.
func Forget() {
	phyMu.Lock()
	phyInfo = map[string]phyText{}
	phyMu.Unlock()
}

// Unblock clears soft rfkill on all radios (Wi-Fi and Bluetooth).
func Unblock() {
	_ = tool(5*time.Second, "rfkill", "unblock", "all").Run()
}

type phyText struct {
	text string
	at   time.Time
}

var (
	phyMu   sync.Mutex
	phyInfo = map[string]phyText{}
)

// tool prepares a run of one of the short-lived programs the router
// drives its radios with. None of them should take longer than a moment;
// one that hangs (a driver that has stopped answering) is killed instead
// of taking the control loop down with it.
func tool(limit time.Duration, name string, args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	// The context is released when the time is up at the latest.
	time.AfterFunc(limit+2*time.Second, cancel)
	return cmd
}
