// Package wifi discovers radios and drives hostapd (access point) and
// wpa_supplicant (station) on them.
package wifi

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	var out []Radio
	for _, e := range entries {
		phy := e.Name()
		dir := filepath.Join(sysPhy, phy)
		r := Radio{Phy: phy}
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
		case r.Bus != "" && r.MAC != "":
			r.ID = r.Bus + ":" + r.MAC
		default:
			continue
		}
		if info, err := exec.Command("iw", "phy", phy, "info").Output(); err == nil {
			parsePhyInfo(string(info), &r)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// baseIface returns the PHY's primary network interface: the one that is
// not a virtual AP interface created by this package.
func baseIface(phyDir string) string {
	entries, _ := os.ReadDir(filepath.Join(phyDir, "device", "net"))
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), VifPrefix) {
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
		if comboAllowsAPSTA(c) {
			r.APSTA = true
		}
	}
}

var reGroup = regexp.MustCompile(`#\{([^}]*)\}\s*<=\s*(\d+)`)
var reTotal = regexp.MustCompile(`total\s*<=\s*(\d+)`)

// comboAllowsAPSTA checks one "valid interface combinations" entry for
// simultaneous AP and managed interfaces.
func comboAllowsAPSTA(combo string) bool {
	apGroup, staGroup := -1, -1
	for i, g := range reGroup.FindAllStringSubmatch(combo, -1) {
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
	if apGroup < 0 || staGroup < 0 || apGroup == staGroup {
		return false
	}
	if m := reTotal.FindStringSubmatch(combo); m != nil {
		if n, _ := strconv.Atoi(m[1]); n < 2 {
			return false
		}
	}
	return true
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
	return exec.Command("iw", "reg", "set", country).Run()
}

// Unblock clears soft rfkill on all radios (Wi-Fi and Bluetooth).
func Unblock() {
	_ = exec.Command("rfkill", "unblock", "all").Run()
}
