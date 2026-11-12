package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	tablet  = "02:00:00:00:00:21"
	console = "02:00:00:00:00:22"
	laptop  = "02:00:00:00:00:23"
	tv      = "02:00:00:00:00:24"
)

func groupsConfig() *Config {
	c := Default()
	c.System.Timezone = "Europe/Berlin"
	c.Groups = []Group{
		{ID: "kids", Name: "Kids", Icon: "child", Members: []string{tablet, console, laptop}, Route: ActBlock, NoAdblock: true,
			Schedules: []Schedule{{ID: "bed", Name: "Bedtime", Enabled: true, Days: []int{0, 1, 2, 3, 4}, From: "21:00", To: "07:00"}}},
		{ID: "media", Name: "Media", Members: []string{tv}},
	}
	c.Devices = []Device{{MAC: laptop, Route: ActDirect}, {MAC: tv, NoAdblock: true}}
	return c
}

func TestGroupsNormalize(t *testing.T) {
	c := Default()
	c.System.Timezone = " Europe/Berlin "
	c.Groups = []Group{{Name: " Kids ", Members: []string{" 02:00:00:00:00:AA "},
		Schedules: []Schedule{{Name: " Bedtime ", Enabled: true, Days: []int{4, 0, 4, 2, 0}, From: " 21:00", To: "07:00 "}},
		Override:  Override{Until: 99}}}
	c.Policy.Rules = []Rule{{ID: "r", Name: "R", Enabled: true, Action: ActBlock}}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	g := c.Groups[0]
	if c.System.Timezone != "Europe/Berlin" || g.Name != "Kids" || !reID.MatchString(g.ID) || g.Members[0] != "02:00:00:00:00:aa" {
		t.Errorf("group: %+v, zone %q", g, c.System.Timezone)
	}
	s := g.Schedules[0]
	if s.Name != "Bedtime" || !reID.MatchString(s.ID) || !reflect.DeepEqual(s.Days, []int{0, 2, 4}) || s.From != "21:00" || s.To != "07:00" {
		t.Errorf("schedule: %+v", s)
	}
	// An override without a mode is none at all.
	if g.Override != (Override{}) {
		t.Errorf("override: %+v", g.Override)
	}
	// Lists are lists, also the new ones.
	b, _ := json.Marshal(c)
	for _, want := range []string{`"groups":[{`, `"schedules":[{`, `"groups":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("configuration JSON lacks %s", want)
		}
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("configuration JSON contains null: %s", b)
	}
	// A configuration from before there were groups has none, and no time
	// zone, and is as valid as it was.
	old := Default()
	old.Normalize()
	if err := old.Validate(); err != nil || old.Groups == nil || len(old.Groups) != 0 || old.System.Timezone != "" || old.Location() != nil {
		t.Errorf("without groups: %v, %+v", err, old.Groups)
	}
}

func TestGroupsValidate(t *testing.T) {
	if err := groupsConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	many := func(n int) []Group {
		var out []Group
		for i := 0; i < n; i++ {
			out = append(out, Group{ID: "g" + string(rune('a'+i)), Name: "Group " + string(rune('A'+i))})
		}
		return out
	}
	for name, tc := range map[string]struct {
		mutate func(c *Config)
		says   string
	}{
		"a zone nobody knows":         {func(c *Config) { c.System.Timezone = "Europe/Atlantis" }, "does not know the time zone"},
		"a zone that is a path":       {func(c *Config) { c.System.Timezone = "../../etc/passwd" }, "does not know the time zone"},
		"the machine's zone":          {func(c *Config) { c.System.Timezone = "Local" }, "does not know the time zone"},
		"a zone with a control char":  {func(c *Config) { c.System.Timezone = "Europe/Berlin\n" }, "does not know the time zone"},
		"too many groups":             {func(c *Config) { c.Groups = many(MaxGroups + 1) }, "room for 16 groups"},
		"bad group id":                {func(c *Config) { c.Groups[0].ID = "Kids!" }, "invalid group id"},
		"duplicate group id":          {func(c *Config) { c.Groups[1].ID = "kids" }, "duplicate group id"},
		"no name":                     {func(c *Config) { c.Groups[0].Name = "" }, "name needs 1 to 40"},
		"long name":                   {func(c *Config) { c.Groups[0].Name = strings.Repeat("x", 41) }, "name needs 1 to 40"},
		"name with a newline":         {func(c *Config) { c.Groups[0].Name = "Kids\n}" }, "name needs 1 to 40"},
		"two groups of one name":      {func(c *Config) { c.Groups[1].Name = "Kids" }, "are named “Kids”"},
		"icon that is not a key":      {func(c *Config) { c.Groups[0].Icon = "figure.2; drop" }, "invalid icon"},
		"member that is no address":   {func(c *Config) { c.Groups[0].Members = []string{"02:00:00:00:00:zz"} }, "invalid device"},
		"member in upper case":        {func(c *Config) { c.Groups[0].Members = []string{"02:00:00:00:00:AA"} }, "invalid device"},
		"device in two groups":        {func(c *Config) { c.Groups[1].Members = []string{tv, tablet} }, "is in two groups, Kids and Media"},
		"device twice in a group":     {func(c *Config) { c.Groups[0].Members = []string{tablet, tablet} }, "is in Kids twice"},
		"route to a VPN that is gone": {func(c *Config) { c.Groups[0].Route = "vpn:gone" }, "unknown VPN"},
		"route that is not one":       {func(c *Config) { c.Groups[0].Route = "sideways" }, "invalid action"},
		"too many schedules": {func(c *Config) {
			for i := 0; i < MaxSchedules; i++ {
				c.Groups[0].Schedules = append(c.Groups[0].Schedules, Schedule{ID: "s" + string(rune('a'+i)), Days: []int{1}, From: "10:00", To: "11:00"})
			}
		}, "room for 8 schedules"},
		"bad schedule id":       {func(c *Config) { c.Groups[0].Schedules[0].ID = "" }, "invalid schedule id"},
		"duplicate schedule id": {func(c *Config) { c.Groups[0].Schedules = append(c.Groups[0].Schedules, c.Groups[0].Schedules[0]) }, "duplicate schedule id"},
		"time that is not one":  {func(c *Config) { c.Groups[0].Schedules[0].From = "24:00" }, "written like 21:00"},
		"time without a zero":   {func(c *Config) { c.Groups[0].Schedules[0].To = "7:00" }, "written like 21:00"},
		"time with seconds":     {func(c *Config) { c.Groups[0].Schedules[0].To = "07:00:00" }, "written like 21:00"},
		"no days":               {func(c *Config) { c.Groups[0].Schedules[0].Days = nil }, "at least one day"},
		"an eighth day":         {func(c *Config) { c.Groups[0].Schedules[0].Days = []int{1, 7} }, "numbered 0 (Sunday) to 6"},
		"a day twice":           {func(c *Config) { c.Groups[0].Schedules[0].Days = []int{1, 1} }, "numbered 0 (Sunday) to 6"},
		"a schedule without a zone": {func(c *Config) { c.System.Timezone = "" },
			"Choose your time zone under Settings before using a schedule"},
		"override that is neither":   {func(c *Config) { c.Groups[0].Override = Override{Mode: "maybe", Until: 1} }, "invalid override"},
		"override without a time":    {func(c *Config) { c.Groups[0].Override = Override{Mode: OverrideAllow} }, "is not a time"},
		"override in the year 3000":  {func(c *Config) { c.Groups[0].Override = Override{Mode: OverridePause, Until: 32503680000} }, "is not a time"},
		"device allowed before 1970": {func(c *Config) { c.Devices[0].AllowUntil = -1 }, "is not a time"},
		"rule for a group that is gone": {func(c *Config) {
			c.Policy.Rules = []Rule{{ID: "r", Name: "Kids' games", Enabled: true, Src: Src{Groups: []string{"gone"}}, Action: ActBlock}}
		}, "The rule “Kids' games” applies to a group that no longer exists."},
	} {
		c := groupsConfig()
		tc.mutate(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: got %v, want an error saying %q", name, err, tc.says)
		}
	}
	// A schedule that is switched off needs no time zone yet, and a group
	// can go without any.
	c := groupsConfig()
	c.System.Timezone = ""
	c.Groups[0].Schedules[0].Enabled = false
	if err := c.Validate(); err != nil {
		t.Errorf("a schedule that is off, without a time zone: %v", err)
	}
	// What is valid: every zone the phone might name, a rule by group.
	for _, zone := range []string{"UTC", "Europe/Berlin", "America/Argentina/Buenos_Aires", "Australia/Lord_Howe", "Asia/Kolkata", "Etc/GMT+5"} {
		c := groupsConfig()
		c.System.Timezone = zone
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", zone, err)
		}
		if loc := c.Location(); loc == nil || loc.String() != zone {
			t.Errorf("%s: location %v", zone, loc)
		}
	}
	c = groupsConfig()
	c.Policy.Rules = []Rule{{ID: "r", Name: "R", Enabled: true, Src: Src{Groups: []string{"kids"}}, Action: ActBlock}}
	if err := c.Validate(); err != nil {
		t.Errorf("rule by group: %v", err)
	}
	if c.Policy.Rules[0].Src.Any() {
		t.Error("a rule whose source is a group counts as a rule for everybody")
	}
}

// Where a device and its group both say something, the device's own
// setting comes first.
func TestGroupPrecedence(t *testing.T) {
	c := groupsConfig()
	if got, want := c.DeviceRoutes(), map[string]string{tablet: ActBlock, console: ActBlock, laptop: ActDirect}; !reflect.DeepEqual(got, want) {
		t.Errorf("routes: %v, want %v", got, want)
	}
	if got, want := c.AdExempt(), map[string]bool{tablet: true, console: true, laptop: true, tv: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("exempt from ad blocking: %v, want %v", got, want)
	}
	if g := c.GroupOf(console); g == nil || g.ID != "kids" {
		t.Errorf("the console's group: %+v", g)
	}
	if g := c.GroupOf("02:00:00:00:00:99"); g != nil {
		t.Errorf("a stranger's group: %+v", g)
	}
	src := Src{Devices: []string{tv, "02:00:00:00:00:01"}, Groups: []string{"media", "kids", "gone"}}
	if got, want := c.SrcDevices(src), []string{"02:00:00:00:00:01", tablet, console, laptop, tv}; !reflect.DeepEqual(got, want) {
		t.Errorf("a rule's devices: %v, want %v", got, want)
	}
	if got := c.SrcDevices(Src{Groups: []string{"gone"}}); len(got) != 0 {
		t.Errorf("the devices of a group that is gone: %v", got)
	}
	for hhmm, want := range map[string]int{"00:00": 0, "07:05": 425, "21:00": 1260, "23:59": 1439} {
		if got, ok := Minutes(hhmm); !ok || got != want {
			t.Errorf("Minutes(%s) = %d, %v", hhmm, got, ok)
		}
	}
	for _, bad := range []string{"", "24:00", "12:60", "1:00", "12.30", "12:3", " 12:30", "١٢:٣٠"} {
		if _, ok := Minutes(bad); ok {
			t.Errorf("Minutes(%q) accepted", bad)
		}
	}
}
