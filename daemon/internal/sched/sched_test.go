package sched

import (
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// at reads a time as a clock on the wall in loc shows it.
func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The days of the week as schedules number them.
const (
	sun = iota
	mon
	tue
	wed
	thu
	fri
	sat
)

func bedtime() config.Schedule {
	return config.Schedule{ID: "bed", Name: "Bedtime", Enabled: true, Days: []int{sun, mon, tue, wed, thu}, From: "21:00", To: "07:00"}
}

// A schedule that runs past midnight belongs to the day it starts on: it
// is in force on the morning after, and not on the morning of a day whose
// evening has one.
func TestAcrossMidnight(t *testing.T) {
	loc := zone(t, "Europe/Berlin")
	g := config.Group{ID: "kids", Schedules: []config.Schedule{bedtime()}}
	// 2026-11-12 is a Thursday.
	for _, tc := range []struct {
		now     string
		offline bool
		until   string // when offline
		next    string // when online
	}{
		{"2026-11-12 20:59:59", false, "", "2026-11-12 21:00:00"},
		{"2026-11-12 21:00:00", true, "2026-11-13 07:00:00", ""},
		{"2026-11-12 23:59:59", true, "2026-11-13 07:00:00", ""},
		{"2026-11-13 00:00:00", true, "2026-11-13 07:00:00", ""},
		{"2026-11-13 06:59:59", true, "2026-11-13 07:00:00", ""},
		// Friday morning: Thursday's is over, and neither Friday nor
		// Saturday starts one. The next is Sunday's.
		{"2026-11-13 07:00:00", false, "", "2026-11-15 21:00:00"},
		{"2026-11-13 22:00:00", false, "", "2026-11-15 21:00:00"},
		{"2026-11-14 03:00:00", false, "", "2026-11-15 21:00:00"},
		// Sunday morning is not the end of a Saturday night's schedule:
		// there is none.
		{"2026-11-15 03:00:00", false, "", "2026-11-15 21:00:00"},
		{"2026-11-15 21:30:00", true, "2026-11-16 07:00:00", ""},
	} {
		st := Of(g, at(t, loc, tc.now), loc)
		if st.Offline != tc.offline {
			t.Errorf("%s: offline %v, want %v", tc.now, st.Offline, tc.offline)
			continue
		}
		if tc.offline {
			if st.Why != WhySchedule || st.Schedule != "bed" || !st.Until.Equal(at(t, loc, tc.until)) || !st.Next.IsZero() {
				t.Errorf("%s: %+v, want offline by the schedule until %s", tc.now, st, tc.until)
			}
		} else if !st.Next.Equal(at(t, loc, tc.next)) || st.NextSchedule != "bed" || !st.Until.IsZero() || st.Why != "" {
			t.Errorf("%s: %+v, want online with the next start at %s", tc.now, st, tc.next)
		}
	}
}

// From and To the same is a whole day; a schedule that is switched off,
// or a group without a time zone, keeps nothing offline.
func TestWholeDayAndOff(t *testing.T) {
	loc := zone(t, "Europe/Berlin")
	day := config.Schedule{ID: "sat", Enabled: true, Days: []int{sat}, From: "00:00", To: "00:00"}
	g := config.Group{ID: "g", Schedules: []config.Schedule{day}}
	// 2026-11-14 is a Saturday.
	if st := Of(g, at(t, loc, "2026-11-14 00:00:00"), loc); !st.Offline || !st.Until.Equal(at(t, loc, "2026-11-15 00:00:00")) {
		t.Errorf("start of the day: %+v", st)
	}
	if st := Of(g, at(t, loc, "2026-11-14 23:59:59"), loc); !st.Offline {
		t.Errorf("end of the day: %+v", st)
	}
	if st := Of(g, at(t, loc, "2026-11-15 00:00:00"), loc); st.Offline || !st.Next.Equal(at(t, loc, "2026-11-21 00:00:00")) {
		t.Errorf("the day after: %+v", st)
	}
	if st := Of(g, at(t, loc, "2026-11-14 12:00:00"), nil); st.Offline || !st.Next.IsZero() {
		t.Errorf("without a time zone: %+v", st)
	}
	g.Schedules[0].Enabled = false
	if st := Of(g, at(t, loc, "2026-11-14 12:00:00"), loc); st.Offline || !st.Next.IsZero() {
		t.Errorf("switched off: %+v", st)
	}
}

// The times are those of the clock on the wall. On the night daylight
// saving begins the offline time is an hour shorter, on the night it ends
// an hour longer, and both edges are where the clock says.
func TestDaylightSaving(t *testing.T) {
	loc := zone(t, "Europe/Berlin")
	g := config.Group{ID: "kids", Schedules: []config.Schedule{{ID: "bed", Enabled: true, Days: []int{sat}, From: "21:00", To: "07:00"}}}
	utc := func(s string) time.Time { return at(t, time.UTC, s) }

	// Clocks go forward: Sunday 29 March 2026, 02:00 becomes 03:00.
	st := Of(g, at(t, loc, "2026-03-28 21:00:00"), loc)
	if !st.Offline || !st.Until.Equal(utc("2026-03-29 05:00:00")) {
		t.Errorf("spring: %+v, want offline until 07:00 summer time (05:00 UTC)", st)
	}
	if got := st.Until.Sub(at(t, loc, "2026-03-28 21:00:00")); got != 9*time.Hour {
		t.Errorf("spring: the night lasts %v, want 9h", got)
	}
	if st := Of(g, utc("2026-03-29 04:59:59"), loc); !st.Offline {
		t.Errorf("spring, a second before 07:00: %+v", st)
	}
	if st := Of(g, utc("2026-03-29 05:00:00"), loc); st.Offline {
		t.Errorf("spring, at 07:00: %+v", st)
	}

	// Clocks go back: Sunday 25 October 2026, 03:00 becomes 02:00.
	st = Of(g, at(t, loc, "2026-10-24 21:00:00"), loc)
	if !st.Offline || !st.Until.Equal(utc("2026-10-25 06:00:00")) {
		t.Errorf("autumn: %+v, want offline until 07:00 winter time (06:00 UTC)", st)
	}
	if got := st.Until.Sub(at(t, loc, "2026-10-24 21:00:00")); got != 11*time.Hour {
		t.Errorf("autumn: the night lasts %v, want 11h", got)
	}
	// Through the hour the clocks show twice it stays offline: no edge
	// at either 02:00.
	for _, s := range []string{"2026-10-24 23:59:59", "2026-10-25 00:00:00", "2026-10-25 00:59:59", "2026-10-25 01:00:00", "2026-10-25 01:59:59", "2026-10-25 05:59:59"} {
		if st := Of(g, utc(s), loc); !st.Offline {
			t.Errorf("autumn, %s UTC: %+v", s, st)
		}
	}
	if st := Of(g, utc("2026-10-25 06:00:00"), loc); st.Offline {
		t.Errorf("autumn, at 07:00: %+v", st)
	}
}

// A time the clocks skip is taken for the moment they jump; a time they
// show twice for the first of the two. Either way a schedule has one
// start and one end.
func TestTimesTheClocksSkipOrRepeat(t *testing.T) {
	loc := zone(t, "Europe/Berlin")
	utc := func(s string) time.Time { return at(t, time.UTC, s) }
	if got, want := wall(loc, 2026, time.March, 29, 2*60+30), utc("2026-03-29 01:00:00"); !got.Equal(want) {
		t.Errorf("02:30 on the day the clocks skip it: %s, want %s (03:00 summer time)", got.UTC(), want)
	}
	if got, want := wall(loc, 2026, time.October, 25, 2*60+30), utc("2026-10-25 00:30:00"); !got.Equal(want) {
		t.Errorf("02:30 on the day the clocks show it twice: %s, want %s (the first)", got.UTC(), want)
	}
	// Times that exist once are what they say, on those days too.
	for _, tc := range []struct {
		m      time.Month
		d, min int
		want   string
	}{
		{time.March, 29, 60 + 59, "2026-03-29 00:59:00"},
		{time.March, 29, 3 * 60, "2026-03-29 01:00:00"},
		{time.October, 25, 60 + 59, "2026-10-24 23:59:00"},
		{time.October, 25, 3 * 60, "2026-10-25 02:00:00"},
		{time.July, 1, 12 * 60, "2026-07-01 10:00:00"},
	} {
		if got := wall(loc, 2026, tc.m, tc.d, tc.min); !got.Equal(utc(tc.want)) {
			t.Errorf("%s %d, minute %d: %s, want %s", tc.m, tc.d, tc.min, got.UTC(), tc.want)
		}
	}
	// A zone whose clocks move by half an hour (Lord Howe Island: on 4
	// October 2026, 02:00 becomes 02:30).
	lh := zone(t, "Australia/Lord_Howe")
	if got, want := wall(lh, 2026, time.October, 4, 2*60+15), utc("2026-10-03 15:30:00"); !got.Equal(want) {
		t.Errorf("Lord Howe, 02:15 skipped: %s, want %s", got.UTC(), want)
	}

	// A schedule that starts inside the skipped hour starts with the
	// jump, and one that ends inside the repeated hour ends once.
	start := config.Group{Schedules: []config.Schedule{{ID: "s", Enabled: true, Days: []int{sun}, From: "02:30", To: "06:00"}}}
	if st := Of(start, utc("2026-03-29 00:59:59"), loc); st.Offline || !st.Next.Equal(utc("2026-03-29 01:00:00")) {
		t.Errorf("before the jump: %+v", st)
	}
	if st := Of(start, utc("2026-03-29 01:00:00"), loc); !st.Offline || !st.Until.Equal(utc("2026-03-29 04:00:00")) {
		t.Errorf("at the jump: %+v", st)
	}
	end := config.Group{Schedules: []config.Schedule{{ID: "s", Enabled: true, Days: []int{sat}, From: "22:00", To: "02:30"}}}
	flips, was := 0, false
	for now := utc("2026-10-24 19:00:00"); now.Before(utc("2026-10-25 04:00:00")); now = now.Add(time.Minute) {
		if st := Of(end, now, loc); st.Offline != was {
			flips, was = flips+1, st.Offline
			if want := map[int]string{1: "2026-10-24 20:00:00", 2: "2026-10-25 00:30:00"}[flips]; want == "" || !now.Equal(utc(want)) {
				t.Errorf("edge %d at %s UTC", flips, now)
			}
		}
	}
	if flips != 2 {
		t.Errorf("the schedule changed state %d times in the night the clocks go back, want once on and once off", flips)
	}
}

// The other side of the world, where daylight saving ends in April and
// the zone is behind UTC or far ahead of it.
func TestOtherZones(t *testing.T) {
	g := config.Group{Schedules: []config.Schedule{bedtime()}}
	ny := zone(t, "America/New_York")
	// 2026-11-01 is the Sunday the clocks go back in New York.
	if st := Of(g, at(t, ny, "2026-11-01 21:00:00"), ny); !st.Offline || !st.Until.Equal(at(t, ny, "2026-11-02 07:00:00")) {
		t.Errorf("New York: %+v", st)
	}
	syd := zone(t, "Australia/Sydney")
	// 2026-04-05 is the Sunday the clocks go back in Sydney: Saturday
	// has no bedtime, Sunday's starts at 21:00.
	if st := Of(g, at(t, syd, "2026-04-05 02:30:00"), syd); st.Offline || !st.Next.Equal(at(t, syd, "2026-04-05 21:00:00")) {
		t.Errorf("Sydney: %+v", st)
	}
	// The same moment is a different time of day in each: a schedule
	// goes by the zone it is given.
	now := at(t, time.UTC, "2026-11-12 21:30:00")
	if Of(g, now, zone(t, "Europe/London")).Offline == Of(g, now, zone(t, "America/Los_Angeles")).Offline {
		t.Error("21:30 in London and 13:30 in Los Angeles came out the same")
	}
}

// Several schedules: any of them keeps the group offline, and "until" is
// when the last of those that follow one another ends.
func TestOverlappingSchedules(t *testing.T) {
	loc := zone(t, "Europe/Berlin")
	all := []int{sun, mon, tue, wed, thu, fri, sat}
	g := config.Group{Schedules: []config.Schedule{
		{ID: "homework", Enabled: true, Days: all, From: "17:00", To: "19:00"},
		{ID: "dinner", Enabled: true, Days: all, From: "18:30", To: "19:30"},
		{ID: "quiet", Enabled: true, Days: all, From: "19:30", To: "20:00"},
		{ID: "bed", Enabled: true, Days: all, From: "21:00", To: "07:00"},
		{ID: "off", Enabled: false, Days: all, From: "20:00", To: "21:00"},
	}}
	for _, tc := range []struct{ now, schedule, until string }{
		{"2026-11-12 17:00:00", "homework", "2026-11-12 20:00:00"},
		{"2026-11-12 18:45:00", "dinner", "2026-11-12 20:00:00"},
		{"2026-11-12 19:15:00", "dinner", "2026-11-12 20:00:00"},
		{"2026-11-12 19:30:00", "quiet", "2026-11-12 20:00:00"},
		{"2026-11-12 22:00:00", "bed", "2026-11-13 07:00:00"},
	} {
		st := Of(g, at(t, loc, tc.now), loc)
		if !st.Offline || st.Schedule != tc.schedule || !st.Until.Equal(at(t, loc, tc.until)) {
			t.Errorf("%s: %+v, want %s until %s", tc.now, st, tc.schedule, tc.until)
		}
	}
	if st := Of(g, at(t, loc, "2026-11-12 20:00:00"), loc); st.Offline || st.NextSchedule != "bed" || !st.Next.Equal(at(t, loc, "2026-11-12 21:00:00")) {
		t.Errorf("in the gap: %+v", st)
	}
	// Schedules that leave no gap all week: offline, and no end to name.
	always := config.Group{Schedules: []config.Schedule{
		{ID: "day", Enabled: true, Days: all, From: "07:00", To: "21:00"},
		{ID: "night", Enabled: true, Days: all, From: "21:00", To: "07:00"},
	}}
	if st := Of(always, at(t, loc, "2026-11-12 12:00:00"), loc); !st.Offline || !st.Until.IsZero() {
		t.Errorf("no gap all week: %+v", st)
	}
}

// What was asked for now: "allow" holds a schedule off until its time,
// "pause" takes the group offline until its time, the switch beats both,
// and one that has passed means nothing.
func TestOverride(t *testing.T) {
	loc := zone(t, "Europe/Berlin")
	now := at(t, loc, "2026-11-12 21:30:00") // Thursday, in bedtime
	g := config.Group{ID: "kids", Schedules: []config.Schedule{bedtime()}}
	allow := func(until string) config.Override {
		return config.Override{Mode: config.OverrideAllow, Until: at(t, loc, until).Unix()}
	}
	pause := func(until string) config.Override {
		return config.Override{Mode: config.OverridePause, Until: at(t, loc, until).Unix()}
	}

	// Half an hour more: online until 22:00, and offline again from then.
	g.Override = allow("2026-11-12 22:00:00")
	st := Of(g, now, loc)
	if st.Offline || !st.AllowedUntil.Equal(at(t, loc, "2026-11-12 22:00:00")) || !st.Next.Equal(at(t, loc, "2026-11-12 22:00:00")) || st.NextSchedule != "bed" {
		t.Errorf("allowed for half an hour: %+v", st)
	}
	if st := Of(g, at(t, loc, "2026-11-12 22:00:00"), loc); !st.Offline || st.Why != WhySchedule || !st.Until.Equal(at(t, loc, "2026-11-13 07:00:00")) {
		t.Errorf("when the half hour is over: %+v", st)
	}
	// Until the schedule ends: tonight is skipped, and the next start is
	// Sunday's.
	g.Override = allow("2026-11-13 07:00:00")
	st = Of(g, now, loc)
	if st.Offline || !st.AllowedUntil.Equal(at(t, loc, "2026-11-13 07:00:00")) || !st.Next.Equal(at(t, loc, "2026-11-15 21:00:00")) {
		t.Errorf("allowed until the schedule ends: %+v", st)
	}
	// Allowed before the schedule has started: nothing is being held off
	// yet, and its start is passed over.
	if st := Of(g, at(t, loc, "2026-11-12 20:00:00"), loc); st.Offline || !st.AllowedUntil.IsZero() || !st.Next.Equal(at(t, loc, "2026-11-15 21:00:00")) {
		t.Errorf("allowed ahead of the schedule: %+v", st)
	}
	// One that has passed means nothing.
	g.Override = allow("2026-11-12 21:29:59")
	if st := Of(g, now, loc); !st.Offline || st.Why != WhySchedule {
		t.Errorf("an allowance that has run out: %+v", st)
	}

	// Paused until a time, outside any schedule.
	day := at(t, loc, "2026-11-12 15:00:00")
	g.Override = pause("2026-11-12 16:00:00")
	if st := Of(g, day, loc); !st.Offline || st.Why != WhyTimer || !st.Until.Equal(at(t, loc, "2026-11-12 16:00:00")) {
		t.Errorf("paused for an hour: %+v", st)
	}
	if st := Of(g, at(t, loc, "2026-11-12 16:00:00"), loc); st.Offline || !st.Next.Equal(at(t, loc, "2026-11-12 21:00:00")) {
		t.Errorf("when the hour is over: %+v", st)
	}
	// Paused until a time that is inside a schedule: it comes back when
	// the schedule ends.
	g.Override = pause("2026-11-12 21:15:00")
	if st := Of(g, at(t, loc, "2026-11-12 20:30:00"), loc); !st.Offline || st.Why != WhyTimer || !st.Until.Equal(at(t, loc, "2026-11-13 07:00:00")) {
		t.Errorf("paused into a schedule: %+v", st)
	}

	// The group's switch: offline with no end, whatever else is set.
	g.Paused = true
	for _, o := range []config.Override{{}, allow("2026-11-13 07:00:00"), pause("2026-11-12 16:00:00")} {
		g.Override = o
		if st := Of(g, day, loc); !st.Offline || st.Why != WhyPaused || !st.Until.IsZero() || !st.Next.IsZero() {
			t.Errorf("switched off, with %+v: %+v", o, st)
		}
	}
	// A timer that runs into the switch has no end either.
	g.Paused = false
	g.Override = config.Override{}
	if st := Of(g, day, loc); st.Offline {
		t.Errorf("nothing set, in the afternoon: %+v", st)
	}
}

// How a device's own settings and its group's combine.
func TestEvaluate(t *testing.T) {
	const (
		tablet  = "02:00:00:00:00:01"
		console = "02:00:00:00:00:02"
		laptop  = "02:00:00:00:00:03"
		tv      = "02:00:00:00:00:04"
		printer = "02:00:00:00:00:05"
	)
	c := config.Default()
	c.System.Timezone = "Europe/Berlin"
	loc := zone(t, "Europe/Berlin")
	now := at(t, loc, "2026-11-12 21:30:00")
	c.Groups = []config.Group{
		{ID: "kids", Name: "Kids", Members: []string{tablet, console, laptop}, Schedules: []config.Schedule{bedtime()}},
		{ID: "media", Name: "Media", Members: []string{tv}},
	}
	c.Devices = []config.Device{
		// Let through for homework, until ten.
		{MAC: laptop, AllowUntil: at(t, loc, "2026-11-12 22:00:00").Unix()},
		// Paused itself, and in a group that is offline as well.
		{MAC: console, Paused: true},
		{MAC: printer, Paused: true},
		{MAC: tv, AllowUntil: at(t, loc, "2026-11-12 22:00:00").Unix()},
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	states, held := Evaluate(c, now)
	if !states["kids"].Offline || states["media"].Offline {
		t.Fatalf("groups: %+v", states)
	}
	until := at(t, loc, "2026-11-13 07:00:00")
	want := map[string]Held{
		tablet:  {Why: WhySchedule, Group: "kids", Until: until},
		console: {Why: WhyDevice},
		printer: {Why: WhyDevice},
	}
	if len(held) != len(want) {
		t.Errorf("held: %+v", held)
	}
	for mac, w := range want {
		if got, ok := held[mac]; !ok || got.Why != w.Why || got.Group != w.Group || !got.Until.Equal(w.Until) {
			t.Errorf("%s: %+v (held %v), want %+v", mac, got, ok, w)
		}
	}
	// At ten the laptop's hour is up, and it is offline with the rest.
	_, held = Evaluate(c, at(t, loc, "2026-11-12 22:00:00"))
	if h := held[laptop]; h.Why != WhySchedule || h.Group != "kids" {
		t.Errorf("the laptop after its allowance: %+v", h)
	}
	// In the morning only the devices that are paused themselves are.
	_, held = Evaluate(c, at(t, loc, "2026-11-13 07:00:00"))
	if len(held) != 2 || held[console].Why != WhyDevice || held[printer].Why != WhyDevice {
		t.Errorf("in the morning: %+v", held)
	}
	// Paused for a while, in the afternoon: the laptop is let through
	// that as well.
	c.Groups[0].Override = config.Override{Mode: config.OverridePause, Until: at(t, loc, "2026-11-12 16:00:00").Unix()}
	_, held = Evaluate(c, at(t, loc, "2026-11-12 15:00:00"))
	if held[tablet].Why != WhyTimer || !held[tablet].Until.Equal(at(t, loc, "2026-11-12 16:00:00")) {
		t.Errorf("the tablet with the group paused for an hour: %+v", held[tablet])
	}
	if _, cut := held[laptop]; cut {
		t.Errorf("the laptop, let through until ten, is cut off by the group's timer: %+v", held[laptop])
	}
	c.Groups[0].Override = config.Override{}
	// The group's switch cuts off every member, let through or not: it
	// means off until somebody switches it back.
	c.Groups[0].Paused = true
	_, held = Evaluate(c, now)
	for _, mac := range []string{tablet, laptop} {
		if held[mac].Why != WhyPaused || held[mac].Group != "kids" || !held[mac].Until.IsZero() {
			t.Errorf("%s with the group switched off: %+v", mac, held[mac])
		}
	}
	// Without a time zone no schedule applies.
	c.Groups[0].Paused = false
	c.System.Timezone = ""
	if states, held := Evaluate(c, now); states["kids"].Offline || len(held) != 2 {
		t.Errorf("without a time zone: %+v %+v", states, held)
	}
}
