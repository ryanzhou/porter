package core

import (
	"path/filepath"
	"sort"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/devices"
	"github.com/ryanzhou/porter/daemon/internal/sched"
	"github.com/ryanzhou/porter/daemon/internal/system"
)

// Groups of devices and their schedules. Which devices are cut off at a
// given moment is sched.Evaluate's to say; the control loop asks on every
// pass, so that the start or end of a schedule takes effect within one,
// and gives the answer to the firewall and the DNS forwarder as the
// devices that are paused. A change in who is cut off is a change to
// where connections go (fw.ClassKey), which ends the connections under
// way exactly as pausing a device does.
//
// Schedules need the time, and the router has no clock that keeps it
// while the power is off: it starts from the time it last saved. That is
// right after a restart and wrong by however long the router was off
// otherwise, so the clock is not trusted until something has confirmed
// it: a time server, the app (which sets the clock when it connects), or
// the site the connection is checked against, whose answer carries the
// time. Until then schedules go by the clock as it is, which carries on
// from where it stopped. The site is asked as soon as an uplink has an
// address, so the time is known within seconds of the router being
// online.
//
// What the site says is used for schedules and for nothing else. The
// machine's clock is left to the time server and the app: WireGuard must
// never see it go forward and then back, and one web server's word is not
// worth that risk. So where the site shows the clock to be behind,
// schedules go by a clock of their own, which counts on from the time the
// site gave (Core.heard), until the machine's clock has itself been put
// right.

// clockSlack is how far the router's clock and somebody else's may differ
// and still be taken to agree.
const clockSlack = 2 * time.Minute

// heardTime is a time somebody gave, and when that was. The time since is
// counted on the monotonic clock, which nobody sets: the machine's clock
// can be put right underneath without this one jumping.
type heardTime struct{ said, heard time.Time }

// now is the time schedules go by: the machine's clock, or the time a
// site gave and what has passed since, while the machine's clock has not
// been set by anything better. (In tests, see Core.virtualClock, it is
// whatever system.set_time last said.)
func (c *Core) now() time.Time {
	if h := c.heard.Load(); h != nil {
		return h.said.Add(time.Since(h.heard))
	}
	return time.Now()
}

// clockSet notes that the machine's clock has been set by something that
// is trusted with it, a time server or the app: schedules go by it from
// here on. It must run on the control loop.
func (c *Core) clockSet(by string) {
	if c.heard.Swap(nil) != nil {
		c.Kick()
	}
	c.trustClock(by)
}

// trustClock notes that the clock has been confirmed. It must run on the
// control loop.
func (c *Core) trustClock(by string) {
	if c.clockTrusted {
		return
	}
	c.clockTrusted = true
	c.log.Info("the clock is confirmed", "by", by, "time", c.now().UTC().Format(time.RFC3339))
	c.Kick()
}

// timeFromPhone takes the time the app sends at the start of a session.
func (c *Core) timeFromPhone(t time.Time) (adjusted bool, err error) {
	if c.virtualClock {
		c.heard.Store(&heardTime{said: t, heard: time.Now()})
		c.do(func() { c.trustClock("the app"); c.Kick() })
		return true, nil
	}
	if adjusted, err = system.TimeFromPhone(t); err != nil {
		return false, err
	}
	if adjusted {
		system.SaveClock(filepath.Join(c.opt.DataDir, "clock"))
	}
	// Set or left as it was, the phone's clock and the router's now agree.
	c.do(func() { c.clockSet("the app") })
	return adjusted, nil
}

// timeFromSite takes the time a site gave (said) as the router found the
// internet reachable through it, at the moment heard. While nothing has
// confirmed the clock, and the clock is behind the site's (as one
// restored from the last time saved is), schedules go by the site's time.
// It must run on the control loop.
func (c *Core) timeFromSite(said, heard time.Time) {
	if c.clockTrusted || c.virtualClock {
		return
	}
	switch behind := said.Sub(heard); {
	case behind > clockSlack:
		c.heard.Store(&heardTime{said: said, heard: heard})
		c.log.Info("the clock is behind the site the connection is checked against; schedules go by the site's time", "behind", behind.Round(time.Second))
	case behind < -clockSlack:
		// Ahead of the site's. Schedules are not turned back on the word
		// of one web server; a time server or the app will say.
		return
	}
	c.trustClock("the site the connection is checked against")
}

// ClockStatus is what the router knows of the time, for schedules.
type ClockStatus struct {
	// Zone is the time zone schedules are kept in, "" while none is set.
	Zone string `json:"zone"`
	// Trusted: the time schedules go by has been confirmed since the
	// router started.
	Trusted bool `json:"trusted"`
}

// GroupStatus is how a group stands.
type GroupStatus struct {
	ID string `json:"id"`
	// Connected counts the members that are on one of the networks now.
	Connected int `json:"connected"`
	// Offline: the group's devices are cut off. Why says by what:
	// "paused" (its switch), "timer" (paused until a time) or "schedule",
	// then with the schedule's id. Until is when it comes back by itself
	// (unix time), absent when it does not.
	Offline  bool   `json:"offline"`
	Why      string `json:"why,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	Until    int64  `json:"until,omitempty"`
	// AllowedUntil: a schedule would have it offline now, and it has been
	// allowed online until then.
	AllowedUntil int64 `json:"allowed_until,omitempty"`
	// Next is when it next goes offline by a schedule, within a week,
	// and NextSchedule which.
	Next         int64  `json:"next,omitempty"`
	NextSchedule string `json:"next_schedule,omitempty"`
}

// HeldDevice is a device that is cut off right now.
type HeldDevice struct {
	MAC string `json:"mac"`
	// Why: "device" (it is paused itself), or what keeps its group
	// offline, as in GroupStatus, with the group's id and until when.
	Why   string `json:"why"`
	Group string `json:"group,omitempty"`
	Until int64  `json:"until,omitempty"`
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// groupStatus puts what sched.Evaluate found into the form the status
// has it in.
func groupStatus(cfg *config.Config, states map[string]sched.State, held map[string]sched.Held, devs []devices.Entry) ([]GroupStatus, []HeldDevice) {
	online := map[string]bool{}
	for _, d := range devs {
		online[d.MAC] = d.Online
	}
	groups := []GroupStatus{}
	for _, g := range cfg.Groups {
		st := states[g.ID]
		gs := GroupStatus{
			ID: g.ID, Offline: st.Offline, Why: st.Why, Schedule: st.Schedule, Until: unixOrZero(st.Until),
			AllowedUntil: unixOrZero(st.AllowedUntil), Next: unixOrZero(st.Next), NextSchedule: st.NextSchedule,
		}
		for _, m := range g.Members {
			if online[m] {
				gs.Connected++
			}
		}
		groups = append(groups, gs)
	}
	cut := []HeldDevice{}
	for _, mac := range heldMACs(held) {
		h := held[mac]
		cut = append(cut, HeldDevice{MAC: mac, Why: h.Why, Group: h.Group, Until: unixOrZero(h.Until)})
	}
	return groups, cut
}

// heldMACs lists the devices that are cut off, sorted.
func heldMACs(held map[string]sched.Held) []string {
	out := make([]string, 0, len(held))
	for mac := range held {
		out = append(out, mac)
	}
	sort.Strings(out)
	return out
}

// groupEvents writes to the activity log what the clock did to a group
// since the last pass: a schedule starting, a group coming back. What a
// person has just changed themselves is no news to them.
func (c *Core) groupEvents(cfg *config.Config, states map[string]sched.State) {
	was, rev := c.groupWas, c.groupRev
	c.groupWas, c.groupRev = states, cfg.Rev
	if was == nil || rev != cfg.Rev {
		return
	}
	for _, g := range cfg.Groups {
		before, now := was[g.ID], states[g.ID]
		switch {
		case !before.Offline && now.Offline && now.Why == sched.WhySchedule:
			name := ""
			for _, s := range g.Schedules {
				if s.ID == now.Schedule {
					name = s.Name
				}
			}
			if name == "" {
				c.addEvent("group", g.Name+" is offline, as scheduled.", "")
			} else {
				c.addEvent("group", g.Name+" is offline: "+name+" has started.", "")
			}
		case before.Offline && !now.Offline:
			c.addEvent("group", g.Name+" is online again.", "")
		}
	}
}

// leaveGroups takes a device out of whatever group it is in.
func leaveGroups(cfg *config.Config, mac string) {
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		kept := g.Members[:0]
		for _, m := range g.Members {
			if m != mac {
				kept = append(kept, m)
			}
		}
		g.Members = kept
	}
}
