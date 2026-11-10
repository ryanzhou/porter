package wifi

import (
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

	// An adapter that only does one thing at a time.
	if comboAllowsAPSTA("#{ managed, AP, mesh point } <= 1, total <= 1, #channels <= 1") {
		t.Error("single-interface combination misread")
	}

	spec := APSpec{Radio: r, Iface: "wlan0", Bridge: "br-lan", Country: "US", Band: "5", Channel: 36,
		AP: config.AP{SSID: "Tést \"net\"", Passphrase: "correct horse", Security: config.SecWPA2}}
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
	conf := wpaConf(STASpec{Iface: "wlan1", Country: "DE", Networks: []config.WifiNetwork{
		{SSID: "Open Net", Priority: 1},
		{SSID: "Home", PSK: `pass"word`, Hidden: true, Priority: 5},
	}}, "/run/x")
	for _, want := range []string{"country=DE", "ssid=4f70656e204e6574", "key_mgmt=NONE", "scan_ssid=1", `psk="pass"word"`, "priority=5"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
}
