package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A route can name one uplink, to be preferred ("source:") or to be the
// only way out ("only:"), wherever a route can be set.
func TestSourceActions(t *testing.T) {
	for action, want := range map[string]struct {
		key  string
		only bool
		ok   bool
	}{
		"source:phone":    {"phone", false, true},
		"only:phone":      {"phone", true, true},
		"source:wifi":     {"wifi", false, true},
		"only:eth:eth0":   {"eth:eth0", true, true},
		"source:eth:eth0": {"eth:eth0", false, true},
		"direct":          {"", false, false},
		"vpn:home":        {"", false, false},
		"":                {"", false, false},
	} {
		key, only, ok := SourceOf(action)
		if key != want.key || only != want.only || ok != want.ok {
			t.Errorf("SourceOf(%q) = %q, %v, %v", action, key, only, ok)
		}
	}
	if ActSource(UplinkPhone) != "source:phone" || ActOnly(UplinkEth("eth0")) != "only:eth:eth0" {
		t.Error("actions are not written as they are read")
	}

	set := func(f func(c *Config)) error {
		c := Default()
		c.Devices = []Device{{MAC: "02:00:00:00:00:01"}}
		c.Groups = []Group{{ID: "work", Name: "Work", Members: []string{"02:00:00:00:00:01"}}}
		c.Policy.Rules = []Rule{{ID: "r1", Name: "Video", Enabled: true, Action: ActDirect}}
		f(c)
		c.Normalize()
		return c.Validate()
	}
	places := map[string]func(c *Config, action string){
		"a rule":      func(c *Config, a string) { c.Policy.Rules[0].Action = a },
		"a device":    func(c *Config, a string) { c.Devices[0].Route = a },
		"a group":     func(c *Config, a string) { c.Groups[0].Route = a },
		"a network":   func(c *Config, a string) { c.Networks[0].Route = a },
		"the default": func(c *Config, a string) { c.Policy.Default = a },
	}
	for where, put := range places {
		for _, action := range []string{"source:phone", "only:phone", "source:wifi", "only:wifi", "source:eth:eth0", "only:eth:eth0"} {
			if err := set(func(c *Config) { put(c, action) }); err != nil {
				t.Errorf("%s routed %q: %v", where, action, err)
			}
		}
		// Nothing but the keys the order of uplinks has, and no port
		// there is none of: what is in an action ends up in firewall
		// rules.
		for _, action := range []string{
			"source:", "only:", "source:eth:eth9", "only:eth:", "source:eth", "source:Phone", "only:phone ", "source:usb0",
			`source:eth:eth0" accept`, "source:eth:eth0;drop", "only:wifi/Hotel", "sources:phone", "only", "source:vpn:home",
		} {
			if err := set(func(c *Config) { put(c, action) }); err == nil {
				t.Errorf("%s routed %q was accepted", where, action)
			}
		}
	}

	// Every uplink a route names is listed once; a rule that is switched
	// off names nothing.
	c := Default()
	c.Policy.Default = "source:phone"
	c.Policy.Rules = []Rule{
		{ID: "r1", Enabled: true, Action: "only:phone"},
		{ID: "r2", Enabled: false, Action: "source:wifi"},
	}
	c.Devices = []Device{{MAC: "02:00:00:00:00:01", Route: "only:eth:eth0"}, {MAC: "02:00:00:00:00:02", Route: "vpn:x"}}
	c.Networks[0].Route = "source:eth:eth0"
	if got, want := c.RoutedSources(), []string{"phone", "eth:eth0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("uplinks that routes name: %v, want %v", got, want)
	}
}

func TestDataLimit(t *testing.T) {
	// A limit that has never been set is a month from the first, switched
	// off, and is written out in full: the app decodes it strictly.
	c := Default()
	c.WAN.Networks = []WifiNetwork{{SSID: "Hotspot"}}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	want := DataLimit{Period: PeriodMonth, StartDay: 1}
	if c.Ethernet[0].Data != want || c.WAN.Networks[0].Data != want || c.WAN.Phone.Data != want {
		t.Errorf("limits as normalised: %+v %+v %+v", c.Ethernet[0].Data, c.WAN.Networks[0].Data, c.WAN.Phone.Data)
	}
	b, _ := json.Marshal(c.WAN.Phone)
	if !strings.Contains(string(b), `"data":{"enabled":false,"period":"month","mb":0,"start_day":1,"lift_until":0}`) {
		t.Errorf("a phone's settings as written: %s", b)
	}

	// Each place a limit is kept has its own.
	c.Ethernet[0].Data = DataLimit{Enabled: true, Period: PeriodMonth, MB: 200_000, StartDay: 15}
	c.WAN.Networks[0].Data = DataLimit{Enabled: true, Period: PeriodDay, MB: 500, StartDay: 1}
	c.WAN.Phone.Data = DataLimit{Enabled: true, Period: " Month ", MB: 5000, LiftUntil: 1_800_000_000}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.DataLimitOf(UplinkEth("eth0"), ""); got.MB != 200_000 || got.StartDay != 15 {
		t.Errorf("the port's limit: %+v", got)
	}
	if got := c.DataLimitOf(UplinkWifi, "Hotspot"); got.Period != PeriodDay || got.MB != 500 {
		t.Errorf("the saved network's limit: %+v", got)
	}
	if got := c.DataLimitOf(UplinkWifi, "Hotel"); got.Enabled {
		t.Errorf("a network that is not saved has a limit: %+v", got)
	}
	if got := c.DataLimitOf(UplinkPhone, ""); got.Period != PeriodMonth || got.StartDay != 1 || got.Bytes() != 5_000_000_000 {
		t.Errorf("the phone's limit: %+v", got)
	}

	for what, l := range map[string]DataLimit{
		"enabled without an amount": {Enabled: true, Period: PeriodMonth, StartDay: 1},
		"a negative amount":         {Period: PeriodMonth, MB: -1, StartDay: 1},
		"more than ten terabytes":   {Enabled: true, Period: PeriodMonth, MB: MaxDataMB + 1, StartDay: 1},
		"a period of a week":        {Enabled: true, Period: "week", MB: 100, StartDay: 1},
		"the 32nd of the month":     {Enabled: true, Period: PeriodMonth, MB: 100, StartDay: 32},
		"a negative day":            {Enabled: true, Period: PeriodMonth, MB: 100, StartDay: -1},
		"lifted until before 1970":  {Enabled: true, Period: PeriodMonth, MB: 100, StartDay: 1, LiftUntil: -5},
		"lifted until after 2100":   {Enabled: true, Period: PeriodMonth, MB: 100, StartDay: 1, LiftUntil: maxUnix},
	} {
		for where, put := range map[string]func(c *Config){
			"a port":    func(c *Config) { c.Ethernet[0].Data = l },
			"a network": func(c *Config) { c.WAN.Networks[0].Data = l },
			"the phone": func(c *Config) { c.WAN.Phone.Data = l },
		} {
			bad := Default()
			bad.WAN.Networks = []WifiNetwork{{SSID: "Hotspot"}}
			put(bad)
			bad.Normalize()
			if err := bad.Validate(); err == nil {
				t.Errorf("%s for %s was accepted", what, where)
			}
		}
	}
}
