package dnsfwd

import (
	"net/netip"
	"sort"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/sched"
)

// Groups, as the forwarder decides by them: a member whose group is
// offline by its schedule is refused exactly like a device that is paused
// itself; otherwise its lookups go by its own route, else its group's;
// and a rule that names a group applies to its members and to nobody
// else. The policy is put together the way the control loop does it.
func TestGroups(t *testing.T) {
	const (
		tablet  = "02:00:00:00:00:21"
		laptop  = "02:00:00:00:00:23"
		paused  = "02:00:00:00:00:30"
		visitor = "02:00:00:00:00:40"
	)
	c := config.Default()
	c.System.Timezone = "Europe/Berlin"
	c.DNS.Adblock.Enabled = true
	c.Groups = []config.Group{
		{ID: "kids", Name: "Kids", Members: []string{tablet, laptop}, Route: config.ActBlock, NoAdblock: true,
			Schedules: []config.Schedule{{ID: "bed", Enabled: true, Days: []int{0, 1, 2, 3, 4, 5, 6}, From: "21:00", To: "07:00"}}},
		{ID: "empty", Name: "Nobody yet"},
	}
	c.Devices = []config.Device{{MAC: laptop, Route: config.ActDirect}, {MAC: paused, Paused: true}}
	c.Policy.Rules = []config.Rule{
		{ID: "school", Name: "School", Enabled: true, Src: config.Src{Groups: []string{"kids"}}, Dst: config.Dst{Domains: []string{"school.example"}}, Action: config.ActDirect},
		{ID: "nobody", Name: "Nobody", Enabled: true, Src: config.Src{Groups: []string{"empty"}}, Action: config.ActBlock},
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	policyAt := func(hour int) *Policy {
		_, held := sched.Evaluate(c, time.Date(2026, 11, 12, hour, 0, 0, 0, loc))
		var macs []string
		for mac := range held {
			macs = append(macs, mac)
		}
		sort.Strings(macs)
		p := &Policy{Rules: RulesFrom(c, nil), Default: c.Policy.Default, Adblock: true}
		p.SetDevices(c, macs)
		return p
	}
	kind := func(p *Policy, mac, name string) string { return p.decide(mac, netip.Addr{}, name).kind }

	day := policyAt(15)
	for _, tc := range []struct{ who, mac, name, want string }{
		// The group's route (block) for the member that has none.
		{"the tablet", tablet, "www.example.com", "refuse"},
		// The device's own route comes first.
		{"the laptop", laptop, "www.example.com", "direct"},
		// The rule for the group applies to its members.
		{"the tablet, to the school", tablet, "www.school.example", "direct"},
		// A rule for a group without members applies to nobody: least of
		// all to everybody.
		{"a visitor", visitor, "www.example.com", "direct"},
		{"a visitor, to the school", visitor, "www.school.example", "direct"},
		{"the paused device", paused, "www.example.com", "refuse"},
	} {
		if got := kind(day, tc.mac, tc.name); got != tc.want {
			t.Errorf("by day, %s: %s, want %s", tc.who, got, tc.want)
		}
	}
	if !day.AdExempt[tablet] || !day.AdExempt[laptop] || day.AdExempt[visitor] {
		t.Errorf("exempt from ad blocking: %v", day.AdExempt)
	}
	if day.Paused[tablet] || day.Paused[laptop] || !day.Paused[paused] {
		t.Errorf("cut off by day: %v", day.Paused)
	}

	// At night every member is refused outright, whatever its route and
	// whatever rule would fit: the same answer the paused device gets.
	night := policyAt(22)
	for _, mac := range []string{tablet, laptop, paused} {
		for _, name := range []string{"www.example.com", "www.school.example"} {
			if got, want := night.decide(mac, netip.Addr{}, name), night.decide(paused, netip.Addr{}, name); got.kind != "refuse" || got.kind != want.kind {
				t.Errorf("by night, %s asking for %s: %+v, and the paused device %+v", mac, name, got, want)
			}
		}
	}
	if got := kind(night, visitor, "www.example.com"); got != "direct" {
		t.Errorf("by night, a visitor: %s", got)
	}
}
