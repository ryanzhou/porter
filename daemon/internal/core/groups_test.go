package core

import (
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/devices"
	"github.com/ryanzhou/porter/daemon/internal/sched"
)

const (
	tablet  = "02:00:00:00:00:21"
	console = "02:00:00:00:00:22"
	laptop  = "02:00:00:00:00:23"
	tv      = "02:00:00:00:00:24"
)

func kidsConfig(t *testing.T) (*config.Config, *time.Location) {
	t.Helper()
	cfg := config.Default()
	cfg.System.Timezone = "Europe/Berlin"
	cfg.Groups = []config.Group{
		{ID: "kids", Name: "Kids", Members: []string{tablet, console, laptop},
			Schedules: []config.Schedule{
				{ID: "bed", Name: "Bedtime", Enabled: true, Days: []int{0, 1, 2, 3, 4}, From: "21:00", To: "07:00"},
				{ID: "hw", Enabled: true, Days: []int{1, 2, 3, 4, 5}, From: "17:00", To: "18:00"},
			}},
		{ID: "media", Name: "Media", Members: []string{tv}},
	}
	cfg.Devices = []config.Device{{MAC: console, Paused: true}}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	return cfg, loc
}

// What the status says of groups and of the devices that are cut off.
func TestGroupStatus(t *testing.T) {
	cfg, loc := kidsConfig(t)
	devs := []devices.Entry{{MAC: tablet, Online: true}, {MAC: console, Online: true}, {MAC: laptop}, {MAC: tv, Online: true}}
	night := time.Date(2026, 11, 12, 21, 30, 0, 0, loc) // a Thursday
	morning := time.Date(2026, 11, 13, 7, 0, 0, 0, loc)
	states, held := sched.Evaluate(cfg, night)
	groups, cut := groupStatus(cfg, states, held, devs)
	wantGroups := []GroupStatus{
		{ID: "kids", Connected: 2, Offline: true, Why: "schedule", Schedule: "bed", Until: morning.Unix()},
		{ID: "media", Connected: 1},
	}
	if !reflect.DeepEqual(groups, wantGroups) {
		t.Errorf("groups by night:\n got %+v\nwant %+v", groups, wantGroups)
	}
	wantCut := []HeldDevice{
		{MAC: tablet, Why: "schedule", Group: "kids", Until: morning.Unix()},
		{MAC: console, Why: "device"},
		{MAC: laptop, Why: "schedule", Group: "kids", Until: morning.Unix()},
	}
	if !reflect.DeepEqual(cut, wantCut) {
		t.Errorf("cut off by night:\n got %+v\nwant %+v", cut, wantCut)
	}
	if got := heldMACs(held); !reflect.DeepEqual(got, []string{tablet, console, laptop}) {
		t.Errorf("handed to the firewall: %v", got)
	}

	// By day: when it next goes offline, and by which schedule.
	states, held = sched.Evaluate(cfg, time.Date(2026, 11, 12, 12, 0, 0, 0, loc))
	groups, cut = groupStatus(cfg, states, held, devs)
	if g := groups[0]; g.Offline || g.Next != time.Date(2026, 11, 12, 17, 0, 0, 0, loc).Unix() || g.NextSchedule != "hw" || g.Until != 0 {
		t.Errorf("kids by day: %+v", g)
	}
	if len(cut) != 1 || cut[0].MAC != console {
		t.Errorf("cut off by day: %+v", cut)
	}

	// Nothing is null where there is nothing, and what does not apply
	// is left out.
	cfg.Groups, cfg.Devices = nil, nil
	states, held = sched.Evaluate(cfg, night)
	groups, cut = groupStatus(cfg, states, held, devs)
	b, _ := json.Marshal(map[string]any{"groups": groups, "held": cut})
	if string(b) != `{"groups":[],"held":[]}` {
		t.Errorf("without groups: %s", b)
	}
	b, _ = json.Marshal(wantGroups[1])
	if string(b) != `{"id":"media","connected":1,"offline":false}` {
		t.Errorf("a group that is online: %s", b)
	}
}

func testCore() *Core {
	return &Core{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		kick:     make(chan struct{}, 1),
		sessions: map[*apiSession]struct{}{},
	}
}

// The activity log has what the clock did to a group, and not what a
// person did to it.
func TestGroupEvents(t *testing.T) {
	cfg, loc := kidsConfig(t)
	c := testCore()
	pass := func(hour, minute int) {
		states, _ := sched.Evaluate(cfg, time.Date(2026, 11, 12, hour, minute, 0, 0, loc))
		c.groupEvents(cfg, states)
	}
	said := func() string {
		var out []string
		for _, e := range c.events {
			if e.Kind != "group" {
				t.Errorf("event of kind %q", e.Kind)
			}
			out = append(out, e.Msg)
		}
		c.events = nil
		return strings.Join(out, " | ")
	}
	// The router starts in the middle of homework time: how things stand
	// at the start is not news.
	pass(17, 30)
	if got := said(); got != "" {
		t.Errorf("at the start: %q", got)
	}
	pass(17, 59)
	pass(18, 0)
	if got := said(); got != "Kids is online again." {
		t.Errorf("when homework time ends: %q", got)
	}
	pass(20, 59)
	pass(21, 0)
	pass(21, 0)
	if got := said(); got != "Kids is offline: Bedtime has started." {
		t.Errorf("when bedtime starts: %q", got)
	}
	// A schedule without a name.
	cfg.Groups[0].Schedules[0].Name = ""
	pass(20, 59)
	if got := said(); got != "Kids is online again." {
		t.Errorf("the clock going back: %q", got)
	}
	pass(21, 0)
	if got := said(); got != "Kids is offline, as scheduled." {
		t.Errorf("an unnamed schedule starting: %q", got)
	}
	// The parent allows another half hour, and later switches the group
	// off by hand: both are changes to the configuration, which they
	// made themselves.
	cfg.Groups[0].Override = config.Override{Mode: config.OverrideAllow, Until: time.Date(2026, 11, 12, 21, 40, 0, 0, loc).Unix()}
	cfg.Rev++
	pass(21, 10)
	cfg.Groups[1].Paused = true
	cfg.Rev++
	pass(21, 11)
	if got := said(); got != "" {
		t.Errorf("after changes made by hand: %q", got)
	}
	// The half hour running out is the clock's doing again.
	pass(21, 40)
	if got := said(); got != "Kids is offline, as scheduled." {
		t.Errorf("when the allowance runs out: %q", got)
	}
}

// What confirms the clock, short of setting it (which a test must not).
func TestClockTrust(t *testing.T) {
	c := testCore()
	if c.clockTrusted {
		t.Fatal("trusted from the start")
	}
	// A site whose clock is far behind the router's says nothing for it.
	c.timeFromSite(time.Now().Add(-3*time.Hour), time.Now())
	if c.clockTrusted || c.heard.Load() != nil {
		t.Error("confirmed, or turned back, by a site whose clock is three hours behind")
	}
	// One that agrees to within a minute or two confirms it, and the
	// control loop is asked to look at the schedules again.
	c.timeFromSite(time.Now().Add(-40*time.Second), time.Now())
	if !c.clockTrusted {
		t.Error("not confirmed by a site that agrees")
	}
	select {
	case <-c.kick:
	default:
		t.Error("no pass of the control loop asked for")
	}
	if c.heard.Load() != nil {
		t.Error("a clock that agrees with the site's is not the one schedules go by")
	}
	// A clock that is hours behind, as after a night without power: the
	// site's time is what schedules go by from then on, and the machine's
	// clock is not touched (this test would be setting it).
	late := testCore()
	before := time.Now()
	late.timeFromSite(before.Add(8*time.Hour), before)
	if d := late.now().Sub(before.Add(8 * time.Hour)); !late.clockTrusted || d < 0 || d > time.Second {
		t.Errorf("eight hours behind the site: trusted %v, and schedules go by a time %v from the site's", late.clockTrusted, d)
	}
	if d := time.Since(before); d > time.Minute {
		t.Errorf("the machine's clock moved by %v", d)
	}
	// Later answers of the site change nothing more.
	late.timeFromSite(time.Now().Add(-5*time.Hour), time.Now())
	if d := late.now().Sub(before.Add(8 * time.Hour)); d < 0 || d > time.Second {
		t.Errorf("a later answer of the site moved the time schedules go by: %v", d)
	}
	// Once a time server or the app has set the machine's clock, that is
	// the time, with nothing added.
	<-late.kick
	late.clockSet("a time server")
	if d := late.now().Sub(time.Now()); d < -time.Second || d > time.Second || !late.clockTrusted {
		t.Errorf("after the clock was set: schedules go by a time %v from it", d)
	}
	select {
	case <-late.kick:
	default:
		t.Error("no pass of the control loop asked for when the time changed")
	}
	// With a clock of its own (the end-to-end tests), the daemon takes
	// no time from anywhere but system.set_time, and the machine's clock
	// is left alone.
	v := testCore()
	v.virtualClock = true
	v.timeFromSite(time.Now(), time.Now())
	if v.clockTrusted {
		t.Error("a virtual clock confirmed by a site")
	}
	bedtime := time.Date(2031, 3, 4, 21, 0, 0, 0, time.UTC)
	v.heard.Store(&heardTime{said: bedtime, heard: time.Now()})
	if d := v.now().Sub(bedtime); d < 0 || d > time.Second {
		t.Errorf("the virtual clock is %v from where it was set", d)
	}
	if d := time.Until(v.now()); d < 24*time.Hour {
		t.Error("the virtual clock did not move")
	}
	// A clock that nothing has confirmed is still the clock schedules go
	// by: it carries on from the time the router last knew, and a group
	// that was offline then stays offline through a restart.
	cfg, loc := kidsConfig(t)
	u := testCore()
	u.virtualClock = true
	u.heard.Store(&heardTime{said: time.Date(2031, 3, 4, 22, 0, 0, 0, loc), heard: time.Now()}) // a Tuesday night
	if states, held := sched.Evaluate(cfg, u.now()); u.clockTrusted || !states["kids"].Offline || held[tablet].Why != sched.WhySchedule {
		t.Errorf("at ten at night by a clock nothing has confirmed (trusted: %v): %+v, %+v", u.clockTrusted, states["kids"], held)
	}
}

func TestLeaveGroups(t *testing.T) {
	cfg, _ := kidsConfig(t)
	leaveGroups(cfg, console)
	leaveGroups(cfg, tv)
	leaveGroups(cfg, "02:00:00:00:00:99")
	if got := cfg.Groups[0].Members; !reflect.DeepEqual(got, []string{tablet, laptop}) {
		t.Errorf("kids: %v", got)
	}
	if got := cfg.Groups[1].Members; got == nil || len(got) != 0 {
		t.Errorf("media: %#v", got)
	}
	// The device view names the group, and what is set for the device.
	c := testCore()
	cfg.Devices = []config.Device{{MAC: laptop, Name: "Laptop", AllowUntil: 1900000000}}
	c.cfg = cfg
	if v := c.deviceView(devices.Entry{MAC: laptop}); v.Group != "kids" || v.AllowUntil != 1900000000 || v.Name != "Laptop" {
		t.Errorf("laptop: %+v", v)
	}
	if v := c.deviceView(devices.Entry{MAC: console}); v.Group != "" || v.AllowUntil != 0 {
		t.Errorf("console, out of the group: %+v", v)
	}
}
