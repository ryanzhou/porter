package core

import (
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/wifi"
)

func TestPlanRadios(t *testing.T) {
	builtin := wifi.Radio{ID: "builtin", Iface: "wlan0", AP: true, STA: true, APSTA: true, MaxAP: 1, MaxAPSTA: 1}
	usb := wifi.Radio{ID: "usb:00:11:22:33:44:55", Iface: "wlx001122334455", Product: "Fast Adapter", AP: true, STA: true, APSTA: true, MaxAP: 4, MaxAPSTA: 3}
	basic := wifi.Radio{ID: "usb:00:11:22:33:44:66", Iface: "wlx001122334466", AP: true, STA: true, MaxAP: 1}
	cfg := func(roles map[string]string, guest bool) *config.Config {
		c := config.Default()
		c.System.Country, c.Wifi[0].SSID, c.Wifi[0].Passphrase = "AU", "net", "password1"
		c.Radios = nil
		for _, id := range []string{builtin.ID, usb.ID, basic.ID} {
			if role, ok := roles[id]; ok {
				c.Radios = append(c.Radios, config.Radio{ID: id, Role: role})
			}
		}
		if guest {
			c.Networks = append(c.Networks, config.Network{ID: "guest", Name: "Guest"})
			c.Wifi = append(c.Wifi, config.SSID{ID: "g", SSID: "net-guest", Passphrase: "password2", Network: "guest", Enabled: true})
		}
		c.Normalize()
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	ids := func(list []config.SSID) string {
		var out []string
		for _, s := range list {
			out = append(out, s.ID)
		}
		return strings.Join(out, ",")
	}

	// The built-in radio alone carries one network; a second stays off
	// the air and the reason is given.
	p := planRadios(cfg(map[string]string{builtin.ID: config.RoleAP}, true), []wifi.Radio{builtin})
	if ids(p.ssids[builtin.ID]) != "main" || len(p.offAir) != 1 || p.offAir[0] != "g" || len(p.notes) != 1 || !strings.Contains(p.notes[0], "net-guest") {
		t.Errorf("built-in radio with two networks: %+v", p)
	}
	// An adapter that can carry several carries both.
	p = planRadios(cfg(map[string]string{usb.ID: config.RoleAP}, true), []wifi.Radio{usb})
	if ids(p.ssids[usb.ID]) != "main,g" || len(p.offAir) != 0 || len(p.notes) != 0 {
		t.Errorf("capable adapter with two networks: %+v", p)
	}
	// Two radios that carry one each share the two between them.
	p = planRadios(cfg(map[string]string{builtin.ID: config.RoleAP, basic.ID: config.RoleAP}, true), []wifi.Radio{builtin, basic})
	if got := ids(p.ssids[builtin.ID]) + "|" + ids(p.ssids[basic.ID]); got != "g|main" || len(p.offAir) != 0 {
		t.Errorf("two single-network radios: %s %+v", got, p)
	}
	// Doing both jobs: possible on the built-in radio, not on the basic
	// adapter, which then only joins.
	p = planRadios(cfg(map[string]string{builtin.ID: config.RoleAPWAN}, false), []wifi.Radio{builtin})
	if p.role[builtin.ID] != config.RoleAPWAN || ids(p.ssids[builtin.ID]) != "main" {
		t.Errorf("built-in radio doing both: %+v", p)
	}
	p = planRadios(cfg(map[string]string{basic.ID: config.RoleAPWAN}, false), []wifi.Radio{basic})
	if p.role[basic.ID] != config.RoleWAN || len(p.ssids[basic.ID]) != 0 || len(p.notes) != 1 {
		t.Errorf("basic adapter asked to do both: %+v", p)
	}
	// A radio that is configured but not plugged in takes no part.
	p = planRadios(cfg(map[string]string{builtin.ID: config.RoleAP, usb.ID: config.RoleWAN}, false), []wifi.Radio{builtin})
	if len(p.hw) != 1 || p.role[usb.ID] != "" {
		t.Errorf("absent radio: %+v", p)
	}
	// A Wi-Fi network that is switched off is on no radio and needs no
	// explaining.
	c := cfg(map[string]string{builtin.ID: config.RoleAP}, true)
	c.Wifi[1].Enabled = false
	p = planRadios(c, []wifi.Radio{builtin})
	if len(p.offAir) != 0 || len(p.notes) != 0 || ids(p.ssids[builtin.ID]) != "main" {
		t.Errorf("a network switched off: %+v", p)
	}
}

func TestIsPhone(t *testing.T) {
	for _, tc := range []struct {
		name, driver, address string
		want                  bool
	}{
		{"enx3a1f0c2299d4", "ipheth", "3a:1f:0c:22:99:d4", true},
		{"usb0", "rndis_host", "ee:01:02:03:04:05", true},
		{"enxa2b3c4d5e6f7", "cdc_ncm", "a2:b3:c4:d5:e6:f7", true},  // a phone's made-up address
		{"enx00e04c680001", "cdc_ncm", "00:e0:4c:68:00:01", false}, // an Ethernet adapter
		{"enx00e04c680002", "r8152", "00:e0:4c:68:00:02", false},
		{"end0", "macb", "2c:cf:67:1a:44:90", false},
		{"usb0", "", "02:00:00:00:00:01", true}, // test rig
		{"eth1", "", "02:00:00:00:00:02", false},
	} {
		if got := isPhone(tc.name, tc.driver, tc.address); got != tc.want {
			t.Errorf("isPhone(%s, %s, %s) = %v", tc.name, tc.driver, tc.address, got)
		}
	}
}
