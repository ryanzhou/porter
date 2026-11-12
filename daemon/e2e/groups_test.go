//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Groups of devices and their schedules, as a subtest of TestRouter: a
// device in a group goes offline when a schedule of the group starts and
// comes back when it ends, and in between a person can let the group, or
// the one device, online for now.
//
// It needs the time to be a few seconds before bedtime, and the machine
// it runs on is not to have its clock set for that. The router is started
// with a clock of its own (PORTER_VIRTUAL_CLOCK, see TestRouter): the call
// the app sets the router's clock with, system.set_time, then moves that
// clock and nothing else. Schedules, and the time in the status, go by it.

// groupView is a group as the status has it, heldView a device that is
// cut off.
type groupView struct {
	ID           string `json:"id"`
	Connected    int    `json:"connected"`
	Offline      bool   `json:"offline"`
	Why          string `json:"why"`
	Schedule     string `json:"schedule"`
	Until        int64  `json:"until"`
	AllowedUntil int64  `json:"allowed_until"`
	Next         int64  `json:"next"`
	NextSchedule string `json:"next_schedule"`
}

type heldView struct {
	MAC   string `json:"mac"`
	Why   string `json:"why"`
	Group string `json:"group"`
	Until int64  `json:"until"`
}

type groupsStatus struct {
	Time  int64 `json:"time"`
	Clock struct {
		Zone    string `json:"zone"`
		Trusted bool   `json:"trusted"`
	} `json:"clock"`
	Groups []groupView `json:"groups"`
	Held   []heldView  `json:"held"`
}

func (r *rig) groups(t *testing.T) groupsStatus {
	t.Helper()
	var st groupsStatus
	if err := r.c.Call("status.get", nil, &st); err != nil {
		t.Fatalf("status.get: %v", err)
	}
	return st
}

// setClock tells the router what time it is, as the app does.
func (r *rig) setClock(t *testing.T, to time.Time) {
	t.Helper()
	if err := r.c.Call("system.set_time", map[string]int64{"unix_ms": to.UnixMilli()}, nil); err != nil {
		t.Fatalf("system.set_time %s: %v", to, err)
	}
}

func groups(t *testing.T, r *rig) {
	const router = "10.83.0.1"
	mark := r.logMark()
	wanIP := r.wanAddr(t)
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	var before config.Config
	if err := r.c.Call("config.get", nil, &before); err != nil {
		t.Fatal(err)
	}
	defer r.edit(t, func(cfg *config.Config) {
		cfg.Groups, cfg.Devices, cfg.Policy.Rules, cfg.System.Timezone = before.Groups, before.Devices, before.Policy.Rules, before.System.Timezone
	})
	defer func() { r.setClock(t, time.Now()) }()
	defer r.explain(t, mark, nil, "ip netns exec "+nsRtr+" nft list table inet porter", "ip -n "+nsC1+" addr show", "ip -n "+nsC1+" route show")

	online := func(ns string) bool { return reaches(ns, siteA) == wanIP }
	// The second device may have changed networks a moment ago, in the
	// test before this one, and be finding its way out again.
	eventually(t, "both devices reaching the internet before anything is set up", 30*time.Second, func() bool { return online(nsC1) && online(nsC2) })
	if r.v6 {
		eventually(t, "the device reaching an IPv6 site before anything is set up", 20*time.Second, func() bool { return reaches(nsC1, site6) != "" })
	}

	// An evening well after this software was built (the router refuses
	// to have its clock set to before that). Bedtime is at nine, on the
	// day of the week that evening happens to be, until seven.
	evening := time.Date(2031, time.March, 4, 21, 0, 0, 0, loc)
	morning := time.Date(2031, time.March, 5, 7, 0, 0, 0, loc)
	r.setClock(t, evening.Add(-15*time.Second))
	r.edit(t, func(cfg *config.Config) {
		cfg.System.Timezone = "Europe/Berlin"
		cfg.Groups = []config.Group{{ID: "kids", Name: "Kids", Members: []string{r.mac1}, Schedules: []config.Schedule{
			{ID: "bed", Name: "Bedtime", Enabled: true, Days: []int{int(evening.Weekday())}, From: "21:00", To: "07:00"},
		}}}
	})
	st := r.groups(t)
	if st.Clock.Zone != "Europe/Berlin" || !st.Clock.Trusted {
		t.Errorf("the clock, after it was set as the app sets it: %+v", st.Clock)
	}
	if len(st.Groups) != 1 || st.Groups[0].Offline || st.Groups[0].Next != evening.Unix() || st.Groups[0].NextSchedule != "bed" || len(st.Held) != 0 {
		t.Fatalf("a few seconds before bedtime (the router's clock says %s): groups %+v, cut off %+v; want the group online with bedtime next at %d",
			time.Unix(st.Time, 0).In(loc), st.Groups, st.Held, evening.Unix())
	}
	if !online(nsC1) {
		t.Fatal("the device is cut off before its bedtime")
	}
	// A connection the device has open must go with the rest.
	var held net.Conn
	inNS(nsC1, func() { held, err = net.DialTimeout("tcp", siteA+":80", 3*time.Second) })
	if err != nil {
		t.Fatalf("opening a connection from the device before bedtime: %v", err)
	}
	defer held.Close()

	// Bedtime. Not before nine by the router's clock, and within a pass
	// of the control loop after it.
	var seen groupsStatus
	eventually(t, "the device being cut off when bedtime starts", 45*time.Second, func() bool {
		cut := !online(nsC1)
		seen = r.groups(t)
		return cut
	})
	if seen.Time < evening.Unix() {
		t.Errorf("the device was cut off at %s by the router's clock, before bedtime", time.Unix(seen.Time, 0).In(loc))
	}
	if late := seen.Time - evening.Unix(); late > 12 {
		t.Errorf("the device was still online %d seconds after bedtime; the control loop comes round every 5", late)
	}
	st = r.groups(t)
	if g := st.Groups[0]; !g.Offline || g.Why != "schedule" || g.Schedule != "bed" || g.Until != morning.Unix() {
		t.Errorf("the group at bedtime: %+v; want offline by the schedule until %d", g, morning.Unix())
	}
	if len(st.Held) != 1 || st.Held[0] != (heldView{MAC: r.mac1, Why: "schedule", Group: "kids", Until: morning.Unix()}) {
		t.Errorf("who is cut off at bedtime: %+v; want the first device, by the schedule", st.Held)
	}
	// Cut off as a paused device is: lookups refused, the connection it
	// had open gone, IPv6 no way round, and nobody else affected.
	if _, rcode, err := lookup(nsC1, router, "direct.test"); rcode != dns.RcodeRefused {
		t.Errorf("a lookup by the device at bedtime: rcode %d (%v), want it refused", rcode, err)
	}
	held.SetDeadline(time.Now().Add(4 * time.Second))
	fmt.Fprint(held, "GET /ip HTTP/1.0\r\nHost: "+siteA+"\r\n\r\n")
	if answer, _ := io.ReadAll(held); strings.Contains(string(answer), "200 OK") {
		t.Errorf("the connection the device had open before bedtime still works: %q", answer)
	}
	if r.v6 {
		if got := reaches(nsC1, site6); got != "" {
			t.Errorf("at bedtime the device still reaches an IPv6 site, from %s", got)
		}
		if got := reaches(nsC2, site6); got == "" {
			t.Error("at bedtime the device that is in no group has lost IPv6")
		}
	}
	if !online(nsC2) {
		t.Error("at bedtime the device that is in no group is cut off as well")
	}

	// Allowed until the schedule ends, as the app's button does it: with
	// the time the status gives.
	r.edit(t, func(cfg *config.Config) {
		cfg.Groups[0].Override = config.Override{Mode: config.OverrideAllow, Until: st.Groups[0].Until}
	})
	eventually(t, "the device coming back when the group is allowed online for tonight", 15*time.Second, func() bool { return online(nsC1) })
	if g := r.groups(t).Groups[0]; g.Offline || g.AllowedUntil != morning.Unix() {
		t.Errorf("the group, allowed until the morning: %+v", g)
	}
	if _, rcode, _ := lookup(nsC1, router, "direct.test"); rcode != dns.RcodeSuccess {
		t.Errorf("a lookup by the device once allowed: rcode %d", rcode)
	}
	r.edit(t, func(cfg *config.Config) { cfg.Groups[0].Override = config.Override{} })
	eventually(t, "the device being cut off again when the allowance is taken back", 15*time.Second, func() bool { return !online(nsC1) })

	// The one device let through, the group staying as it is.
	if err := r.c.Call("device.set", config.Device{MAC: r.mac1, Name: "One", AllowUntil: morning.Unix()}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the device coming back when it alone is let through", 15*time.Second, func() bool { return online(nsC1) })
	if st := r.groups(t); !st.Groups[0].Offline || len(st.Held) != 0 {
		t.Errorf("with the device let through: group %+v, cut off %+v; want the group offline and nobody cut off", st.Groups[0], st.Held)
	}
	if err := r.c.Call("device.set", config.Device{MAC: r.mac1, Name: "One"}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the device being cut off again without its allowance", 15*time.Second, func() bool { return !online(nsC1) })

	// The morning: back at seven, not before.
	r.setClock(t, morning.Add(-15*time.Second))
	if online(nsC1) {
		t.Fatalf("a few seconds before the schedule ends the device is online (the router's clock says %s)", time.Unix(r.groups(t).Time, 0).In(loc))
	}
	eventually(t, "the device coming back when the schedule ends", 45*time.Second, func() bool {
		back := online(nsC1)
		seen = r.groups(t)
		return back
	})
	if seen.Time < morning.Unix() {
		t.Errorf("the device was back at %s by the router's clock, before the schedule ended", time.Unix(seen.Time, 0).In(loc))
	}
	if late := seen.Time - morning.Unix(); late > 12 {
		t.Errorf("the device was still cut off %d seconds after the schedule ended", late)
	}
	if st := r.groups(t); st.Groups[0].Offline || len(st.Held) != 0 || st.Groups[0].Next != evening.AddDate(0, 0, 7).Unix() {
		t.Errorf("in the morning: group %+v, cut off %+v; want it online until the same evening next week (%d)", st.Groups[0], st.Held, evening.AddDate(0, 0, 7).Unix())
	}
	if _, rcode, _ := lookup(nsC1, router, "direct.test"); rcode != dns.RcodeSuccess {
		t.Errorf("a lookup by the device in the morning: rcode %d", rcode)
	}
	// Both edges are in the activity log, as the clock's doing.
	var events []struct {
		Kind string `json:"kind"`
		Msg  string `json:"msg"`
	}
	if err := r.c.Call("events.list", nil, &events); err != nil {
		t.Fatal(err)
	}
	said := map[string]bool{}
	for _, e := range events {
		if e.Kind == "group" {
			said[e.Msg] = true
		}
	}
	if !said["Kids is offline: Bedtime has started."] || !said["Kids is online again."] {
		t.Errorf("the activity log about the group: %v", said)
	}

	// A rule for the group: its members, and nobody else.
	r.edit(t, func(cfg *config.Config) {
		cfg.Policy.Rules = append(cfg.Policy.Rules, config.Rule{ID: "kidsb", Name: "Not for kids", Enabled: true,
			Src: config.Src{Groups: []string{"kids"}}, Dst: config.Dst{CIDRs: []string{siteB}}, Action: config.ActBlock})
	})
	eventually(t, "the rule for the group keeping its member from the second site", 15*time.Second, func() bool { return reaches(nsC1, siteB) == "" })
	if reaches(nsC2, siteB) != wanIP {
		t.Error("the rule for the group applies to a device that is not in it")
	}
	if !online(nsC1) {
		t.Error("the rule for the group keeps its member from more than the site it names")
	}

	// The group's switch, and its route for members without their own.
	r.edit(t, func(cfg *config.Config) {
		cfg.Policy.Rules = before.Policy.Rules
		cfg.Groups[0].Paused = true
	})
	eventually(t, "the device being cut off by its group's switch", 15*time.Second, func() bool { return !online(nsC1) })
	if h := r.groups(t).Held; len(h) != 1 || h[0].Why != "paused" || h[0].Group != "kids" || h[0].Until != 0 {
		t.Errorf("cut off by the group's switch: %+v", h)
	}
	r.edit(t, func(cfg *config.Config) { cfg.Groups[0].Paused, cfg.Groups[0].Route = false, config.ActBlock })
	eventually(t, "the group's switch off again, and its route in force", 15*time.Second, func() bool {
		_, rcode, _ := lookup(nsC1, router, "direct.test")
		return len(r.groups(t).Held) == 0 && !online(nsC1) && rcode == dns.RcodeRefused
	})
	if err := r.c.Call("device.set", config.Device{MAC: r.mac1, Name: "One", Route: config.ActDirect}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the device's own route winning over its group's", 15*time.Second, func() bool { return online(nsC1) })
	if !online(nsC2) {
		t.Error("the device that is in no group lost its connection along the way")
	}
}
