package wifi

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Abridged `iw phy` output of the Raspberry Pi 5's on-board radio.
const brcmInfo = `Wiphy phy0
	max # scan SSIDs: 10
	Supported interface modes:
		 * IBSS
		 * managed
		 * AP
		 * P2P-client
		 * P2P-GO
		 * P2P-device
	Band 1:
		Capabilities: 0x1062
			HT20/HT40
		Frequencies:
			* 2412.0 MHz [1] (20.0 dBm)
			* 2437.0 MHz [6] (20.0 dBm)
			* 2462.0 MHz [11] (20.0 dBm)
			* 2484.0 MHz [14] (disabled)
	Band 2:
		Capabilities: 0x1062
			HT20/HT40
		VHT Capabilities (0x00001020):
			Max MPDU length: 3895
		Frequencies:
			* 5180.0 MHz [36] (20.0 dBm)
			* 5200.0 MHz [40] (20.0 dBm)
			* 5220.0 MHz [44] (20.0 dBm)
			* 5240.0 MHz [48] (20.0 dBm)
			* 5260.0 MHz [52] (20.0 dBm) (no IR, radar detection)
			* 5745.0 MHz [149] (20.0 dBm)
			* 5765.0 MHz [153] (20.0 dBm)
			* 5785.0 MHz [157] (20.0 dBm)
			* 5805.0 MHz [161] (disabled)
	Supported commands:
		 * new_interface
	valid interface combinations:
		 * #{ managed } <= 1, #{ P2P-device } <= 1, #{ P2P-client, P2P-GO } <= 1,
		   total <= 3, #channels <= 2
		 * #{ managed } <= 1, #{ AP } <= 1, #{ P2P-client } <= 1, #{ P2P-device } <= 1,
		   total <= 4, #channels <= 1
	Device supports scan flush.
`

func TestParsePhyInfo(t *testing.T) {
	var r Radio
	parsePhyInfo(brcmInfo, &r)
	if !r.AP || !r.STA || !r.APSTA {
		t.Errorf("modes: %+v", r)
	}
	if len(r.Bands) != 2 || r.Bands[0].Name != "2.4" || r.Bands[1].Name != "5" {
		t.Fatalf("bands: %+v", r.Bands)
	}
	b5 := r.Band("5")
	if !b5.HT || !b5.VHT || b5.HE || len(b5.Channels) != 9 {
		t.Errorf("5 GHz band: %+v", b5)
	}
	if ch, ok := PickChannel(b5, 0); !ok || ch != 36 {
		t.Errorf("auto channel: %d %v", ch, ok)
	}
	if _, ok := PickChannel(b5, 52); ok {
		t.Error("radar channel must not be usable")
	}
	if ch, ok := PickChannel(r.Band("2.4"), 0); !ok || ch != 6 {
		t.Errorf("auto 2.4 channel: %d %v", ch, ok)
	}

	// The on-board radio: one network, also next to a station.
	if r.MaxAP != 1 || r.MaxAPSTA != 1 {
		t.Errorf("capacity: %d alone, %d with a station", r.MaxAP, r.MaxAPSTA)
	}
	for combo, want := range map[string][2]int{
		// An adapter that only does one thing at a time.
		"#{ managed, AP, mesh point } <= 1, total <= 1, #channels <= 1": {1, 0},
		// One group that allows several interfaces of either kind.
		"#{ managed, AP, mesh point } <= 2048, #{ P2P-device } <= 1, total <= 2048, #channels <= 1": {2048, 2047},
		// Two interfaces in all, of whichever kind.
		"#{ IBSS } <= 1, #{ managed, AP, mesh point, P2P-client, P2P-GO } <= 2, total <= 2, #channels <= 1": {2, 1},
		// Several networks, and a station beside them.
		"#{ managed } <= 1, #{ AP } <= 4, total <= 4, #channels <= 1":         {4, 3},
		"#{ managed } <= 2, #{ P2P-device } <= 1, total <= 3, #channels <= 2": {0, 0},
	} {
		if alone, with := comboAPs(combo); alone != want[0] || with != want[1] {
			t.Errorf("%s: %d alone, %d with a station; want %v", combo, alone, with, want)
		}
	}

	main := config.SSID{ID: "main", SSID: "Tést \"net\"", Passphrase: "correct horse", Security: config.SecWPA2, Network: "lan"}
	bridge := func(network string) string { return "br-" + network }
	r.Phy, r.MAC = "phy0", "d8:3a:dd:00:11:22"
	spec := APSpec{Radio: r, Iface: "wlan0", Country: "US", Band: "5", Channel: 36, BSS: Networks(r, "wlan0", []config.SSID{main}, bridge)}
	l := ladder(spec)
	if len(l) != 4 || l[0].String() != "ac/80MHz" || l[1].String() != "n/40MHz" || l[3].String() != "legacy/20MHz" {
		t.Errorf("ladder: %v", l)
	}
	conf := hostapdConf(spec, l[0], "/run/x")
	for _, want := range []string{"hw_mode=a", "channel=36", "ht_capab=[HT40+]", "vht_oper_centr_freq_seg0_idx=42",
		"ssid2=54c3a9737420226e657422", "wpa_key_mgmt=WPA-PSK", "wpa_passphrase=correct horse"} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("hostapd config lacks %q:\n%s", want, conf)
		}
	}
	// 149-161 cannot do 80 MHz here because 161 is disabled.
	spec.Channel = 153
	l = ladder(spec)
	if l[0].String() != "n/40MHz" || !strings.Contains(hostapdConf(spec, l[0], "/run/x"), "ht_capab=[HT40-]") {
		t.Errorf("ladder for 153: %v", l)
	}
	spec.Width = 20
	if l = ladder(spec); l[0].String() != "n/20MHz" {
		t.Errorf("width cap ignored: %v", l)
	}

	// A second network: its own interface, address, bridge and password,
	// after everything that belongs to the first.
	guest := config.SSID{ID: "g", SSID: "guest", Passphrase: "another one", Security: config.SecWPA3, Network: "guest", Isolate: true}
	spec.BSS = Networks(r, "wlan0", []config.SSID{main, guest}, bridge)
	conf = hostapdConf(spec, l[0], "/run/x")
	first, second, found := strings.Cut(conf, "bss=ap0-1\n")
	if !found {
		t.Fatalf("no second network:\n%s", conf)
	}
	for _, want := range []string{"interface=wlan0", "bridge=br-lan", "wpa_passphrase=correct horse", "ctrl_interface=/run/x"} {
		if !strings.Contains(first, want+"\n") {
			t.Errorf("first network lacks %q:\n%s", want, first)
		}
	}
	for _, want := range []string{"bssid=de:3a:dd:00:11:22", "bridge=br-guest", "ssid2=6775657374", "wpa_key_mgmt=SAE", "wpa_passphrase=another one", "ap_isolate=1", "ctrl_interface=/run/x"} {
		if !strings.Contains(second, want+"\n") {
			t.Errorf("second network lacks %q:\n%s", want, second)
		}
	}
	if strings.Contains(first, "ap_isolate") || strings.Contains(first, "bssid=") {
		t.Errorf("the first network has the second's settings:\n%s", first)
	}
}

func TestVifNames(t *testing.T) {
	for _, tc := range []struct {
		phy  string
		n    int
		want string
	}{{"phy0", 0, "ap0"}, {"phy0", 2, "ap0-2"}, {"phy12", 3, "ap12-3"}, {"radio", 0, "apxradio"}} {
		got := VifName(tc.phy, tc.n)
		if got != tc.want || len(got) > 15 || !isVif(got) {
			t.Errorf("VifName(%s, %d) = %q", tc.phy, tc.n, got)
		}
	}
	for _, real := range []string{"wlan0", "wlx00c0ca123456", "end0", "apple0"} {
		if isVif(real) && real != "apple0" {
			t.Errorf("%s taken for one of our own", real)
		}
	}
	// Further networks get addresses of their own, all different.
	seen := map[string]bool{"d8:3a:dd:00:11:22": true}
	for n := 1; n < 4; n++ {
		m := derivedMAC("d8:3a:dd:00:11:22", n)
		if seen[m] || len(m) != 17 {
			t.Errorf("address %d: %q", n, m)
		}
		seen[m] = true
	}
}

func TestAssign(t *testing.T) {
	for _, tc := range []struct {
		caps []int
		n    int
		want string
	}{
		{[]int{4}, 2, "[[0 1]]"},
		{[]int{1}, 2, "[[0]]"},          // one radio, one network: the main one
		{[]int{1, 1}, 2, "[[1] [0]]"},   // one each rather than the same twice
		{[]int{4, 1}, 2, "[[0 1] [0]]"}, // the able radio carries both
		{[]int{1, 1}, 3, "[[1] [0]]"},   // not enough radios: the third stays off
		{[]int{2, 1}, 3, "[[0 1] [2]]"}, // spread over both
		{[]int{0, 2}, 2, "[[] [0 1]]"},
		{[]int{1, 1}, 1, "[[0] [0]]"}, // nothing to spread
	} {
		if got := fmt.Sprint(Assign(tc.caps, tc.n)); got != tc.want {
			t.Errorf("Assign(%v, %d) = %s, want %s", tc.caps, tc.n, got, tc.want)
		}
	}
}

func TestBusTag(t *testing.T) {
	for in, want := range map[string]string{"usb": "usb", "pci": "pci", "mac80211_hwsim": "mac80211", "": "radio", "x": "radio", "SDIO": "sdio"} {
		if got := busTag(in); got != want {
			t.Errorf("busTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScanResults(t *testing.T) {
	out := "bssid / frequency / signal level / flags / ssid\n" +
		"aa:bb:cc:dd:ee:01\t2437\t-70\t[WPA2-PSK-CCMP][ESS]\tHotel Guest\n" +
		"aa:bb:cc:dd:ee:02\t5180\t-50\t[WPA2-PSK-CCMP][ESS]\tHotel Guest\n" +
		"aa:bb:cc:dd:ee:03\t5200\t-60\t[ESS]\tCaf\\xc3\\xa9\n" +
		"aa:bb:cc:dd:ee:04\t5200\t-65\t[WPA2-EAP-CCMP][ESS]\tCorp\n" +
		"aa:bb:cc:dd:ee:05\t5200\t-40\t[WPA2-PSK-CCMP][ESS]\t\n"
	res := parseScanResults(out)
	if len(res) != 3 {
		t.Fatalf("results: %+v", res)
	}
	if res[0].SSID != "Hotel Guest" || res[0].Signal != -50 || res[0].Security != "psk" {
		t.Errorf("first: %+v", res[0])
	}
	if res[1].SSID != "Café" || res[1].Security != "open" || res[2].Security != "enterprise" {
		t.Errorf("rest: %+v", res[1:])
	}
}

func TestWPAConf(t *testing.T) {
	conf, ids := wpaConf(STASpec{Iface: "wlan1", Country: "DE", Networks: []config.WifiNetwork{
		{SSID: "Open Net", Priority: 1},
		{SSID: "Home", PSK: `pass"word`, Hidden: true, Priority: 5},
	}}, "/run/x")
	for _, want := range []string{"country=DE", "ssid=4f70656e204e6574", "key_mgmt=NONE", "scan_ssid=1", `psk="pass"word"`, "priority=10",
		"freq_list=5180 5200", "freq_list=2412 2417", "disabled=1"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
	// A visible network is a 5 GHz block and a 2.4 GHz block, both off
	// until steering picks one; a hidden one is a single block, on.
	if got := ids["Open Net"]; got != (netIDs{five: 0, two: 1, any: -1}) {
		t.Errorf("blocks of the visible network: %+v", got)
	}
	if got := ids["Home"]; got != (netIDs{five: -1, two: -1, any: 2}) {
		t.Errorf("block of the hidden network: %+v", got)
	}
	if n := strings.Count(conf, "disabled=1"); n != 2 {
		t.Errorf("%d blocks start switched off, want 2:\n%s", n, conf)
	}

	// On a radio shared with the access point, 5 GHz is limited to the
	// channels an access point may use.
	conf, _ = wpaConf(STASpec{Iface: "wlan0", Country: "DE", Only5: []int{5180, 5200}, Networks: []config.WifiNetwork{{SSID: "x"}}}, "/run/x")
	if !strings.Contains(conf, "freq_list=5180 5200\n") {
		t.Errorf("5 GHz list not limited:\n%s", conf)
	}
}

func TestBandChoice(t *testing.T) {
	scan := func(lines ...string) []bss {
		return parseBSS("bssid / frequency / signal level / flags / ssid\n" + strings.Join(lines, "\n"))
	}
	hotel := func(freq, level int) string {
		return fmt.Sprintf("aa:bb:cc:00:00:%02x\t%d\t%d\t[WPA2-PSK-CCMP][ESS]\tHotel", freq%100, freq, level)
	}
	cases := []struct {
		name    string
		list    []bss
		only5   []int
		current string
		want    string
		decided bool
	}{
		{"strong 2.4, fair 5: take 5", scan(hotel(2437, -45), hotel(5500, -68)), nil, "", "5", true},
		{"5 too weak: take 2.4", scan(hotel(2437, -45), hotel(5500, -76)), nil, "", "2.4", true},
		{"already on 5: stay while it holds", scan(hotel(2437, -45), hotel(5500, -76)), nil, "5", "5", true},
		{"already on 5: leave when it fades", scan(hotel(2437, -45), hotel(5500, -84)), nil, "5", "2.4", true},
		{"on 2.4: not back to 5 until it is good", scan(hotel(2437, -45), hotel(5500, -76)), nil, "2.4", "2.4", true},
		{"only 5 exists: use it however weak", scan(hotel(5500, -85)), nil, "", "5", true},
		{"only 2.4 exists", scan(hotel(2437, -60)), nil, "", "2.4", true},
		{"several access points: the best 5 counts", scan(hotel(2437, -45), hotel(5500, -80), hotel(5180, -60)), nil, "", "5", true},
		{"shared radio: a radar channel is no use", scan(hotel(2437, -45), hotel(5500, -50)), []int{5180, 5200}, "", "2.4", true},
		{"shared radio: an allowed channel is", scan(hotel(2437, -45), hotel(5500, -50), hotel(5200, -65)), []int{5180, 5200}, "", "5", true},
		{"not in range: nothing to decide", scan(), nil, "5", "5", false},
	}
	for _, c := range cases {
		got, decided := pickBand(see(c.list, "Hotel", c.only5), c.current)
		if got != c.want || decided != c.decided {
			t.Errorf("%s: got %q (decided %v), want %q (decided %v)", c.name, got, decided, c.want, c.decided)
		}
	}
	if v := see(scan(hotel(2437, -45), hotel(5500, -80), hotel(5180, -60)), "Hotel", nil); v.freq5 != 5180 || v.freq2 != 2437 || v.freq != 2437 {
		t.Errorf("strongest per band: %+v", v)
	}
}

func TestChannelAwayFrom(t *testing.T) {
	band := func(nums ...int) *Band {
		b := &Band{Name: "5"}
		for _, n := range nums {
			b.Channels = append(b.Channels, Channel{Num: n, Freq: 5000 + 5*n})
		}
		// Radar channels exist but are not for access points.
		for _, n := range []int{52, 100, 116} {
			b.Channels = append(b.Channels, Channel{Num: n, Freq: 5000 + 5*n, Radar: true})
		}
		return b
	}
	wide := band(36, 40, 44, 48, 149, 153, 157, 161, 165)
	low := band(36, 40, 44, 48)
	cases := []struct {
		name   string
		b      *Band
		uplink int
		keep   int
		want   int
		ok     bool
	}{
		{"no uplink: no opinion", wide, 0, 0, 0, false},
		{"uplink on 2.4 GHz: no opinion", wide, 2437, 0, 0, false},
		{"uplink low: go high", wide, 5200, 0, 165, true},
		{"uplink high: go low", wide, 5745, 0, 36, true},
		{"uplink on a radar channel in the middle: go low", wide, 5600, 0, 36, true},
		{"already clear: stay", wide, 5200, 149, 149, true},
		{"no longer clear: move", wide, 5200, 36, 165, true},
		{"uplink gone: stay where it was", wide, 0, 149, 149, true},
		{"nowhere far enough: share the uplink's channel", low, 5220, 0, 44, true},
		{"nowhere far enough, uplink on a radar channel: no opinion", low, 5260, 0, 0, false},
	}
	for _, c := range cases {
		got, ok := ChannelAwayFrom(c.b, c.uplink, c.keep)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %d (%v), want %d (%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}
