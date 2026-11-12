package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	// The zones are part of the program: the router's image has no time
	// zone files, and a daemon that carries its own knows the same zones
	// wherever it runs.
	_ "time/tzdata"
)

// Group is a set of devices looked after together: "the kids' devices",
// "work". It has the settings that make sense for several devices at once,
// and times when all of them are offline.
//
// Where a device and its group both say something, the order is the same
// for everything: what the device says itself comes first. Its own route
// wins over the group's (which wins over its network's); it skips ad
// blocking if either says so; it is offline if it is paused itself or its
// group is offline, unless the device has been let through (AllowUntil)
// what a schedule or a timer does to the group.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Icon is the picture the app shows for the group. The key is the
	// app's; the router only keeps it.
	Icon string `json:"icon"`
	// Members are the hardware addresses of the group's devices. A device
	// is in one group at most: its route and whether it is offline then
	// have one answer, and one place it comes from.
	Members []string `json:"members"`
	// Route is the route of members that have none of their own: "",
	// "direct", "vpn:<id>" or "block".
	Route string `json:"route"`
	// Paused cuts every member off until it is switched back.
	Paused bool `json:"paused"`
	// NoAdblock exempts every member from ad blocking.
	NoAdblock bool `json:"no_adblock"`
	// Schedules are the times the group is offline.
	Schedules []Schedule `json:"schedules"`
	// Override is what was asked for now, against the schedules.
	Override Override `json:"override"`
}

// Schedule is a time of day, on some days of the week, during which a
// group is offline: "Bedtime: Sunday to Thursday, 21:00 to 07:00". The
// times are those of a clock on the wall in System.Timezone.
type Schedule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Days are the days of the week it starts on, 0 for Sunday to 6 for
	// Saturday.
	Days []int `json:"days"`
	// From and To are times of day, "21:00". A To that is not after From
	// is on the following day; the two the same makes a whole day.
	From string `json:"from"`
	To   string `json:"to"`
}

// Override modes.
const (
	OverrideAllow = "allow"
	OverridePause = "pause"
)

// Override is what a person asked of a group for now: "allow" lets it
// online although a schedule says otherwise, "pause" takes it offline,
// each until a moment given as unix time. One that has passed means
// nothing, and is left where it is until the next one replaces it.
type Override struct {
	Mode  string `json:"mode"`
	Until int64  `json:"until"`
}

// Limits on groups.
const (
	MaxGroups       = 16
	MaxGroupMembers = 64
	MaxSchedules    = 8
	// maxUnix is the year 2100: later than any moment a person means.
	maxUnix = 4102444800
)

var (
	reTimeOfDay = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	reIcon      = regexp.MustCompile(`^[a-z0-9-]{0,20}$`)
	reZone      = regexp.MustCompile(`^[A-Za-z0-9_+/-]{1,64}$`)
)

// Minutes returns a time of day ("21:00") as minutes since midnight.
func Minutes(hhmm string) (int, bool) {
	if !reTimeOfDay.MatchString(hhmm) {
		return 0, false
	}
	return (int(hhmm[0]-'0')*10+int(hhmm[1]-'0'))*60 + int(hhmm[3]-'0')*10 + int(hhmm[4]-'0'), true
}

// Location returns the time zone the household's clocks show, nil while
// none is set.
func (c *Config) Location() *time.Location {
	if c.System.Timezone == "" {
		return nil
	}
	loc, err := loadZone(c.System.Timezone)
	if err != nil {
		return nil
	}
	return loc
}

// loadZone looks a time zone up by its IANA name. "Local" is a name Go
// knows too, and means whatever the machine is set to: not a zone.
func loadZone(name string) (*time.Location, error) {
	if !reZone.MatchString(name) || name == "Local" {
		return nil, fmt.Errorf("unknown time zone %q", name)
	}
	return time.LoadLocation(name)
}

// Group returns the group with the given id.
func (c *Config) Group(id string) *Group {
	for i := range c.Groups {
		if c.Groups[i].ID == id {
			return &c.Groups[i]
		}
	}
	return nil
}

// GroupOf returns the group a device is in, nil for none.
func (c *Config) GroupOf(mac string) *Group {
	for i := range c.Groups {
		for _, m := range c.Groups[i].Members {
			if m == mac {
				return &c.Groups[i]
			}
		}
	}
	return nil
}

// DeviceRoutes returns the route of every device that has one, by
// hardware address: its own, or else its group's.
func (c *Config) DeviceRoutes() map[string]string {
	out := map[string]string{}
	for _, g := range c.Groups {
		if g.Route == "" {
			continue
		}
		for _, m := range g.Members {
			out[m] = g.Route
		}
	}
	for _, d := range c.Devices {
		if d.Route != "" {
			out[d.MAC] = d.Route
		}
	}
	return out
}

// AdExempt returns the devices that skip ad blocking, because they or
// their group are set to.
func (c *Config) AdExempt() map[string]bool {
	out := map[string]bool{}
	for _, g := range c.Groups {
		if !g.NoAdblock {
			continue
		}
		for _, m := range g.Members {
			out[m] = true
		}
	}
	for _, d := range c.Devices {
		if d.NoAdblock {
			out[d.MAC] = true
		}
	}
	return out
}

// SrcDevices returns the devices a rule's source names, one by one or as
// members of a group, sorted.
func (c *Config) SrcDevices(s Src) []string {
	seen := map[string]bool{}
	for _, m := range s.Devices {
		seen[m] = true
	}
	for _, id := range s.Groups {
		if g := c.Group(id); g != nil {
			for _, m := range g.Members {
				seen[m] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (c *Config) normalizeGroups() {
	c.System.Timezone = strings.TrimSpace(c.System.Timezone)
	for i := range c.Groups {
		g := &c.Groups[i]
		g.ID, g.Name, g.Icon = strings.TrimSpace(g.ID), strings.TrimSpace(g.Name), strings.TrimSpace(g.Icon)
		if g.ID == "" {
			g.ID = NewID()
		}
		for j := range g.Members {
			g.Members[j] = strings.ToLower(strings.TrimSpace(g.Members[j]))
		}
		for j := range g.Schedules {
			s := &g.Schedules[j]
			s.ID, s.Name = strings.TrimSpace(s.ID), strings.TrimSpace(s.Name)
			if s.ID == "" {
				s.ID = NewID()
			}
			s.From, s.To = strings.TrimSpace(s.From), strings.TrimSpace(s.To)
			// Each day once, in the order of the week.
			days := []int{}
			for _, d := range s.Days {
				at := sort.SearchInts(days, d)
				if at == len(days) || days[at] != d {
					days = append(days, 0)
					copy(days[at+1:], days[at:])
					days[at] = d
				}
			}
			s.Days = days
		}
		g.Override.Mode = strings.ToLower(strings.TrimSpace(g.Override.Mode))
		if g.Override.Mode == "" {
			g.Override.Until = 0
		}
	}
	for i := range c.Policy.Rules {
		for j := range c.Policy.Rules[i].Src.Groups {
			c.Policy.Rules[i].Src.Groups[j] = strings.TrimSpace(c.Policy.Rules[i].Src.Groups[j])
		}
	}
}

func (c *Config) validateGroups() error {
	if tz := c.System.Timezone; tz != "" {
		if _, err := loadZone(tz); err != nil {
			return fmt.Errorf("The router does not know the time zone “%s”. Choose one like Europe/Berlin.", tz)
		}
	}
	if len(c.Groups) > MaxGroups {
		return fmt.Errorf("The router has room for %d groups.", MaxGroups)
	}
	ids, names := map[string]bool{}, map[string]bool{}
	in := map[string]string{}
	for _, g := range c.Groups {
		if !reID.MatchString(g.ID) {
			return fmt.Errorf("invalid group id %q", g.ID)
		}
		if ids[g.ID] {
			return fmt.Errorf("duplicate group id %q", g.ID)
		}
		ids[g.ID] = true
		if g.Name == "" || len(g.Name) > 40 || !printable(g.Name) {
			return fmt.Errorf("A group's name needs 1 to 40 characters.")
		}
		if names[g.Name] {
			return fmt.Errorf("Two of your groups are named “%s”.", g.Name)
		}
		names[g.Name] = true
		if !reIcon.MatchString(g.Icon) {
			return fmt.Errorf("%s: invalid icon %q", g.Name, g.Icon)
		}
		if len(g.Members) > MaxGroupMembers {
			return fmt.Errorf("A group has room for %d devices.", MaxGroupMembers)
		}
		for _, m := range g.Members {
			if !validMAC(m) {
				return fmt.Errorf("%s: invalid device %q", g.Name, m)
			}
			if other, taken := in[m]; taken {
				if other == g.Name {
					return fmt.Errorf("%s is in %s twice.", c.called(m), g.Name)
				}
				return fmt.Errorf("%s is in two groups, %s and %s. A device can be in one group.", c.called(m), other, g.Name)
			}
			in[m] = g.Name
		}
		if err := c.validAction(g.Route, true); err != nil {
			return fmt.Errorf("%s: %w", g.Name, err)
		}
		if err := c.validateSchedules(g); err != nil {
			return err
		}
		switch o := g.Override; o.Mode {
		case "":
		case OverrideAllow, OverridePause:
			if o.Until <= 0 || o.Until >= maxUnix {
				return fmt.Errorf("%s: for how long it is allowed or paused is not a time", g.Name)
			}
		default:
			return fmt.Errorf("%s: invalid override %q", g.Name, o.Mode)
		}
	}
	return nil
}

func (c *Config) validateSchedules(g Group) error {
	if len(g.Schedules) > MaxSchedules {
		return fmt.Errorf("A group has room for %d schedules.", MaxSchedules)
	}
	ids := map[string]bool{}
	for _, s := range g.Schedules {
		if !reID.MatchString(s.ID) {
			return fmt.Errorf("%s: invalid schedule id %q", g.Name, s.ID)
		}
		if ids[s.ID] {
			return fmt.Errorf("%s: duplicate schedule id %q", g.Name, s.ID)
		}
		ids[s.ID] = true
		if len(s.Name) > 40 || !printable(s.Name) {
			return fmt.Errorf("A schedule's name can be 40 characters long at most.")
		}
		if _, ok := Minutes(s.From); !ok {
			return fmt.Errorf("%s: a schedule's times are written like 21:00.", g.Name)
		}
		if _, ok := Minutes(s.To); !ok {
			return fmt.Errorf("%s: a schedule's times are written like 21:00.", g.Name)
		}
		if len(s.Days) == 0 {
			return fmt.Errorf("%s: choose at least one day for the schedule.", g.Name)
		}
		for i, d := range s.Days {
			// Normalize has put them in order, each once.
			if d < 0 || d > 6 || (i > 0 && s.Days[i-1] >= d) {
				return fmt.Errorf("%s: a schedule's days are numbered 0 (Sunday) to 6 (Saturday)", g.Name)
			}
		}
		// A schedule is kept in the household's time. Without knowing
		// which that is the router could only guess, an hour or ten out.
		if s.Enabled && c.System.Timezone == "" {
			return fmt.Errorf("Choose your time zone under Settings before using a schedule: the router has to know what time it is where you are.")
		}
	}
	return nil
}
