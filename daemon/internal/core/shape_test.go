package core

import (
	"reflect"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/shape"
	"github.com/ryanzhou/porter/daemon/internal/wan"
)

// Which uplinks are held to a speed, on which interface, and what is
// known of each one's line: the configuration says it per port, per saved
// Wi-Fi network and for the phone, and the queue belongs on the interface
// the traffic really leaves by.
func TestShapeSources(t *testing.T) {
	on := func(down, up int) config.Shaping { return config.Shaping{Enabled: true, DownKbps: down, UpKbps: up} }
	cfg := config.Default()
	cfg.Ethernet = []config.EthPort{
		{Name: "eth0", Role: config.EthWAN, Shaping: on(95000, 19000)},
		{Name: "eth1", Role: config.EthWAN, VLAN: 35, Shaping: on(50000, 5000)},
		{Name: "eth2", Role: config.EthWAN, PPPoE: config.PPPoE{Enabled: true, Username: "u", Password: "p"}, Shaping: on(40000, 10000)},
		{Name: "eth3", Role: config.EthWAN, VLAN: 7, PPPoE: config.PPPoE{Enabled: true, Username: "u", Password: "p"}, Shaping: on(250000, 40000)},
		{Name: "eth4", Role: config.EthWAN, Shaping: config.Shaping{DownKbps: 95000, UpKbps: 19000}},
		{Name: "eth5", Role: config.EthWAN},
	}
	cfg.WAN.Networks = []config.WifiNetwork{
		{SSID: "Hotel", Shaping: on(8000, 2500)},
		{SSID: "Home", PSK: "password1", Shaping: on(300000, 30000)},
		{SSID: "Cafe"},
	}
	cfg.WAN.Phone.Shaping = on(20000, 4000)
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	session := func(port string) config.PPPoE {
		for _, e := range cfg.Ethernet {
			if e.Name == port {
				return e.PPPoE
			}
		}
		return config.PPPoE{}
	}
	// The uplinks as the control loop makes them of this configuration.
	uplinks := []wan.Uplink{
		{If: "eth0", Kind: "eth", Key: "eth:eth0"},
		{If: "v35-eth1", Kind: "eth", Key: "eth:eth1"},
		{If: "ppp-eth2", Link: "eth2", Kind: "eth", Key: "eth:eth2", PPPoE: session("eth2")},
		{If: "ppp-eth3", Link: "v7-eth3", Kind: "eth", Key: "eth:eth3", PPPoE: session("eth3")},
		{If: "eth4", Kind: "eth", Key: "eth:eth4"},
		{If: "eth5", Kind: "eth", Key: "eth:eth5"},
		{If: "usb0", Kind: "phone", Key: "phone"},
		{If: "usb1", Kind: "phone", Key: "phone"},
		{If: "wlan1", Kind: "wifi", Key: "wifi"},
	}
	wired := []shape.Source{
		{Key: "eth:eth0", If: "eth0", DownKbps: 95000, UpKbps: 19000},
		{Key: "eth:eth1", If: "v35-eth1", DownKbps: 50000, UpKbps: 5000, VLAN: true},
		{Key: "eth:eth2", If: "ppp-eth2", DownKbps: 40000, UpKbps: 10000, PPPoE: true},
		{Key: "eth:eth3", If: "ppp-eth3", DownKbps: 250000, UpKbps: 40000, PPPoE: true, VLAN: true},
		{Key: "phone", If: "usb0", DownKbps: 20000, UpKbps: 4000},
		{Key: "phone", If: "usb1", DownKbps: 20000, UpKbps: 4000},
	}
	for _, tc := range []struct {
		what, ssid string
		wifi       []shape.Source
	}{
		{"on the hotel's network", "Hotel", []shape.Source{{Key: "wifi", If: "wlan1", DownKbps: 8000, UpKbps: 2500}}},
		{"at home", "Home", []shape.Source{{Key: "wifi", If: "wlan1", DownKbps: 300000, UpKbps: 30000}}},
		{"on a network without the setting", "Cafe", nil},
		{"on no network yet", "", nil},
	} {
		joined := map[string]string{}
		if tc.ssid != "" {
			joined["wlan1"] = tc.ssid
		}
		want := append(append([]shape.Source{}, wired...), tc.wifi...)
		if got := shapeSources(cfg, uplinks, joined); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.what, got, want)
		}
	}
	if got := shapeSources(config.Default(), uplinks, nil); len(got) != 0 {
		t.Errorf("with nothing switched on: %+v", got)
	}
}
