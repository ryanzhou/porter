// Package sched works out, for a moment in time, which groups of devices
// are offline and which devices that cuts off: from each group's switch,
// its schedules, what was asked of it for now, and each device's own
// settings. It only computes. The control loop asks on every pass and
// hands the answer to the firewall and the DNS forwarder, which treat a
// device that is cut off through its group exactly like one that is
// paused itself.
package sched

import (
	"sort"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Why a group is offline, or a device cut off.
const (
	WhyDevice   = "device"   // the device's own switch
	WhyPaused   = "paused"   // the group's switch
	WhyTimer    = "timer"    // the group was paused until a time
	WhySchedule = "schedule" // one of the group's schedules
)

// State is how a group stands at one moment.
type State struct {
	Offline bool
	// Why says what keeps it offline: WhyPaused, WhyTimer or WhySchedule.
	// Schedule is then the id of the schedule, for WhySchedule.
	Why      string
	Schedule string
	// Until is when it comes back by itself: the end of everything that
	// keeps it offline one after another. Zero when nothing ends it (the
	// switch), or nothing within a week does.
	Until time.Time
	// AllowedUntil is set while a schedule would have the group offline
	// and it has been allowed online all the same, until then.
	AllowedUntil time.Time
	// Next is when the group next goes offline by itself, within a week,
	// and NextSchedule the schedule that does it. Zero while offline.
	Next         time.Time
	NextSchedule string
}

// Held says what cuts a device off.
type Held struct {
	// Why is WhyDevice, or what keeps its group offline; Group is then
	// the group's id, and Until as in State.
	Why   string
	Group string
	Until time.Time
}

// horizon is how far ahead the end of an offline time, or the start of
// the next, is looked for: every schedule comes round within a week.
const horizon = 8 * 24 * time.Hour

// wall is the moment a clock on the wall in loc shows minute (of the day)
// on the given day. A time the clocks skip, as daylight saving begins, is
// taken for the moment they jump; one they show twice, as it ends, for
// the first of the two.
func wall(loc *time.Location, y int, m time.Month, d, minute int) time.Time {
	t := time.Date(y, m, d, minute/60, minute%60, 0, 0, loc)
	start, end := t.ZoneBounds()
	if t.Hour()*60+t.Minute() != minute {
		// Skipped: Go answers with a time an hour to one side of the
		// jump, which is one end of the stretch that time is in.
		if !start.IsZero() && t.Sub(start) < 3*time.Hour {
			return start
		}
		if !end.IsZero() && end.Sub(t) < 3*time.Hour {
			return end
		}
		return t
	}
	if !start.IsZero() {
		// Shown twice, and this is the second: the clocks were set back
		// by what the two offsets differ, less than that long ago.
		_, before := start.Add(-time.Second).Zone()
		_, now := t.Zone()
		if back := time.Duration(before-now) * time.Second; back > 0 && t.Sub(start) < back {
			return t.Add(-back)
		}
	}
	return t
}

// span returns the offline time of a schedule that starts on the given
// day, if it starts on that day of the week at all.
func span(s config.Schedule, loc *time.Location, y int, m time.Month, d int) (start, end time.Time, ok bool) {
	from, okFrom := config.Minutes(s.From)
	to, okTo := config.Minutes(s.To)
	if !okFrom || !okTo {
		return start, end, false
	}
	// Noon, to ask which day of the week it is: never near a change of
	// the clocks.
	weekday := int(time.Date(y, m, d, 12, 0, 0, 0, loc).Weekday())
	on := false
	for _, day := range s.Days {
		on = on || day == weekday
	}
	if !on {
		return start, end, false
	}
	last := d
	if to <= from {
		last++
	}
	return wall(loc, y, m, d, from), wall(loc, y, m, last, to), true
}

// group evaluates one group.
type group struct {
	g   config.Group
	loc *time.Location
}

// scheduled reports whether a schedule has the group offline at t: which
// one (of several, the one that lasts longest) and until when.
func (e group) scheduled(t time.Time) (id string, end time.Time, ok bool) {
	if e.loc == nil {
		return "", end, false
	}
	y, m, d := t.In(e.loc).Date()
	for _, s := range e.g.Schedules {
		if !s.Enabled {
			continue
		}
		// What is in force now started today or yesterday.
		for back := 0; back <= 1; back++ {
			from, to, on := span(s, e.loc, y, m, d-back)
			if on && !t.Before(from) && t.Before(to) && to.After(end) {
				id, end, ok = s.ID, to, true
			}
		}
	}
	return id, end, ok
}

func (e group) override(mode string, t time.Time) bool {
	return e.g.Override.Mode == mode && t.Unix() < e.g.Override.Until
}

// at says whether the group is offline at t, why, and when what keeps it
// offline ends (the zero time for the switch, which nothing ends).
func (e group) at(t time.Time) (off bool, why, schedule string, end time.Time) {
	switch {
	case e.g.Paused:
		return true, WhyPaused, "", end
	case e.override(config.OverridePause, t):
		return true, WhyTimer, "", time.Unix(e.g.Override.Until, 0)
	case e.override(config.OverrideAllow, t):
		return false, "", "", end
	}
	if id, until, ok := e.scheduled(t); ok {
		return true, WhySchedule, id, until
	}
	return false, "", "", end
}

// Of works out how a group stands at now. loc is the household's time
// zone; without one (nil) no schedule applies.
func Of(g config.Group, now time.Time, loc *time.Location) State {
	e := group{g, loc}
	var st State
	off, why, id, end := e.at(now)
	if off {
		st.Offline, st.Why, st.Schedule = true, why, id
		// One thing after another may keep it offline: a timer that runs
		// into a schedule, a schedule that meets the next.
		for !end.IsZero() && end.Sub(now) < horizon {
			more, _, _, next := e.at(end)
			if !more {
				break
			}
			if !next.After(end) {
				end = time.Time{}
				break
			}
			end = next
		}
		if end.Sub(now) >= horizon {
			end = time.Time{}
		}
		st.Until = end
		return st
	}
	// When it next goes offline: when a schedule starts, or when what
	// allows it online for now runs out with a schedule still in force.
	var starts []time.Time
	if e.override(config.OverrideAllow, now) {
		until := time.Unix(g.Override.Until, 0)
		if _, _, held := e.scheduled(now); held {
			st.AllowedUntil = until
		}
		starts = append(starts, until)
	}
	if loc != nil {
		y, m, d := now.In(loc).Date()
		for _, s := range g.Schedules {
			if !s.Enabled {
				continue
			}
			for ahead := 0; ahead <= 8; ahead++ {
				if from, _, on := span(s, loc, y, m, d+ahead); on && from.After(now) {
					starts = append(starts, from)
				}
			}
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	for _, t := range starts {
		if t.Sub(now) >= horizon {
			break
		}
		if off, why, id, _ := e.at(t); off && why == WhySchedule {
			st.Next, st.NextSchedule = t, id
			break
		}
	}
	return st
}

// Evaluate works out every group and every device at now: how each group
// stands, by id, and which devices are cut off, by hardware address.
//
// This is the one place that says how a device's settings and its group's
// combine for being offline: a device is cut off if it is paused itself,
// or if its group is offline and the device has not been let through
// (Device.AllowUntil) for now. Being let through holds against a schedule
// or a timer, not against the group's switch, which like the device's own
// means off until somebody says otherwise.
func Evaluate(c *config.Config, now time.Time) (map[string]State, map[string]Held) {
	loc := c.Location()
	states := map[string]State{}
	held := map[string]Held{}
	for _, g := range c.Groups {
		st := Of(g, now, loc)
		states[g.ID] = st
		if !st.Offline {
			continue
		}
		for _, m := range g.Members {
			if d := c.Device(m); d != nil && now.Unix() < d.AllowUntil && st.Why != WhyPaused {
				continue
			}
			held[m] = Held{Why: st.Why, Group: g.ID, Until: st.Until}
		}
	}
	for _, d := range c.Devices {
		if d.Paused {
			held[d.MAC] = Held{Why: WhyDevice}
		}
	}
	return states, held
}
