package usage

import (
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata"

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

func TestPeriods(t *testing.T) {
	berlin := zone(t, "Europe/Berlin")
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, berlin)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	month := func(day int) config.DataLimit {
		return config.DataLimit{Enabled: true, Period: config.PeriodMonth, MB: 1000, StartDay: day}
	}
	day := config.DataLimit{Enabled: true, Period: config.PeriodDay, MB: 100}
	for _, tc := range []struct {
		what       string
		limit      config.DataLimit
		now        string
		start, end string
	}{
		{"a month from the first", month(1), "2026-11-12 16:00", "2026-11-01 00:00", "2026-12-01 00:00"},
		{"the first instant of a month", month(1), "2026-12-01 00:00", "2026-12-01 00:00", "2027-01-01 00:00"},
		{"the last instant before it", month(1), "2026-11-30 23:59", "2026-11-01 00:00", "2026-12-01 00:00"},
		{"from the 15th, after it", month(15), "2026-11-20 09:00", "2026-11-15 00:00", "2026-12-15 00:00"},
		{"from the 15th, before it", month(15), "2026-11-12 09:00", "2026-10-15 00:00", "2026-11-15 00:00"},
		{"across the turn of the year", month(15), "2027-01-03 09:00", "2026-12-15 00:00", "2027-01-15 00:00"},
		// A month without the day begins its period on its last day, and
		// the day comes back in the months that have it.
		{"from the 31st, in a month of 30 days", month(31), "2026-11-30 12:00", "2026-11-30 00:00", "2026-12-31 00:00"},
		{"from the 31st, the day before that", month(31), "2026-11-29 12:00", "2026-10-31 00:00", "2026-11-30 00:00"},
		{"from the 31st, in February", month(31), "2027-02-28 08:00", "2027-02-28 00:00", "2027-03-31 00:00"},
		{"from the 31st, in March before it", month(31), "2027-03-30 08:00", "2027-02-28 00:00", "2027-03-31 00:00"},
		{"from the 30th, in a leap February", month(30), "2028-02-29 08:00", "2028-02-29 00:00", "2028-03-30 00:00"},
		{"from the 29th, in a February of 28 days", month(29), "2027-02-28 00:00", "2027-02-28 00:00", "2027-03-29 00:00"},
		{"a day", day, "2026-11-12 16:00", "2026-11-12 00:00", "2026-11-13 00:00"},
		{"a day, at midnight", day, "2026-11-13 00:00", "2026-11-13 00:00", "2026-11-14 00:00"},
		// The days the clocks change are 23 and 25 hours long, and still
		// one day each.
		{"the day the clocks go forward", day, "2027-03-28 12:00", "2027-03-28 00:00", "2027-03-29 00:00"},
		{"the day the clocks go back", day, "2026-10-25 02:30", "2026-10-25 00:00", "2026-10-26 00:00"},
	} {
		start := PeriodStart(tc.limit, at(tc.now), berlin)
		end := PeriodEnd(tc.limit, start, berlin)
		if !start.Equal(at(tc.start)) || !end.Equal(at(tc.end)) {
			t.Errorf("%s (%s): from %s to %s, want %s to %s", tc.what, tc.now, start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), tc.start, tc.end)
		}
		// The end of a period is the beginning of the next, whichever
		// moment of the period is asked about.
		if next := PeriodStart(tc.limit, end, berlin); !next.Equal(end) {
			t.Errorf("%s: the period after ends at %s but the next is said to begin at %s", tc.what, end, next)
		}
		if again := PeriodStart(tc.limit, end.Add(-time.Second), berlin); !again.Equal(start) {
			t.Errorf("%s: a second before its end the period began at %s, not %s", tc.what, again, start)
		}
	}
	if d := PeriodEnd(day, PeriodStart(day, at("2027-03-28 12:00"), berlin), berlin).Sub(at("2027-03-28 00:00")); d != 23*time.Hour {
		t.Errorf("the day the clocks go forward is %s long", d)
	}
	// What Normalize has not seen is a month from the first.
	if got := PeriodStart(config.DataLimit{}, at("2026-11-12 16:00"), berlin); !got.Equal(at("2026-11-01 00:00")) {
		t.Errorf("a limit with nothing filled in began at %s", got)
	}
	// The same moment is another day elsewhere.
	if got := PeriodStart(day, at("2026-11-12 00:30"), time.UTC); !got.Equal(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("00:30 in Berlin is the 11th in UTC; the day began at %s", got)
	}
}

func TestCount(t *testing.T) {
	loc := time.UTC
	limit := config.DataLimit{Enabled: true, Period: config.PeriodMonth, MB: 10, StartDay: 1}
	now := time.Date(2026, 11, 12, 16, 0, 0, 0, loc)
	path := filepath.Join(t.TempDir(), "usage.json")
	s := Load(path)
	s.boot = "boot-1"
	read := func(ifname string, index int, bytes uint64) *Reading {
		return &Reading{If: ifname, Index: index, Bytes: bytes}
	}
	count := func(line string, cur *Reading) State {
		s.Begin()
		return s.Count(line, limit, cur, now, loc, true)
	}

	// What the interface had carried before the limit was set is not
	// counted; what it carries from then on is.
	if st := count("phone", read("usb0", 7, 5_000_000)); st.Used != 0 || st.Reached {
		t.Errorf("first reading: %+v", st)
	}
	if st := count("phone", read("usb0", 7, 8_000_000)); st.Used != 3_000_000 || st.Reached {
		t.Errorf("after 3 MB: %+v", st)
	}
	// Unplugged: nothing to read, and nothing forgotten.
	if st := count("phone", nil); st.Used != 3_000_000 {
		t.Errorf("while unplugged: %+v", st)
	}
	// Plugged in again: a new interface, counting from nothing. All of
	// what it has carried is the phone's.
	if st := count("phone", read("usb0", 9, 2_000_000)); st.Used != 5_000_000 {
		t.Errorf("after plugging in again: %+v", st)
	}
	// The limit is reached at the amount, not after it.
	st := count("phone", read("usb0", 9, 7_000_000))
	if st.Used != 10_000_000 || !st.Reached || !st.Spent() {
		t.Errorf("at 10 of 10 MB: %+v", st)
	}
	if !st.Start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, loc)) || !st.End.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("the period: %s to %s", st.Start, st.End)
	}
	// Said once.
	if !s.Tell("phone") || s.Tell("phone") {
		t.Error("reaching the limit is to be told once in a period")
	}

	// Lifted until the period ends: used all the same, and counted on.
	limit.LiftUntil = st.End.Unix()
	if st := count("phone", read("usb0", 9, 8_000_000)); !st.Reached || !st.Lifted || st.Spent() || st.Used != 11_000_000 {
		t.Errorf("lifted: %+v", st)
	}
	// A lift that has passed means nothing.
	limit.LiftUntil = now.Unix() - 1
	if st := count("phone", read("usb0", 9, 8_000_000)); !st.Spent() {
		t.Errorf("a lift that has passed: %+v", st)
	}
	limit.LiftUntil = 0

	// Across a restart of the daemon the count is kept, and so is what
	// the interface carried while nobody was reading.
	s.Save()
	s = Load(path)
	s.boot = "boot-1"
	if st := count("phone", read("usb0", 9, 8_500_000)); st.Used != 11_500_000 || !st.Reached {
		t.Errorf("after a restart of the daemon: %+v", st)
	}
	if s.Tell("phone") {
		t.Error("told again after a restart")
	}
	// Across a restart of the router the counters begin again: the same
	// name and number, and a counter that happens to be higher, are
	// still a new interface.
	s.Save()
	s = Load(path)
	s.boot = "boot-2"
	if st := count("phone", read("usb0", 9, 9_000_000)); st.Used != 20_500_000 {
		t.Errorf("after a restart of the router: %+v", st)
	}
	// A counter that went backwards is a new interface too.
	if st := count("phone", read("usb0", 9, 100_000)); st.Used != 20_600_000 {
		t.Errorf("after the counter began again: %+v", st)
	}

	// The next month begins: nothing used, not reached, and to be told
	// anew when it is.
	now = time.Date(2026, 12, 1, 0, 0, 5, 0, loc)
	st = count("phone", read("usb0", 9, 150_000))
	if st.Used != 50_000 || st.Reached || !st.Start.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("in the new month: %+v", st)
	}
	count("phone", read("usb0", 9, 20_000_000))
	if !s.Tell("phone") {
		t.Error("the limit reached in a new period is not told")
	}

	// A limit that is switched off, or a network that is forgotten, is
	// forgotten here, and begins from nothing if it comes back.
	s.Keep(map[string]bool{})
	if st := count("phone", read("usb0", 9, 30_000_000)); st.Used != 0 {
		t.Errorf("a line that was forgotten: %+v", st)
	}
}

// One radio is on one network after another; each network has its own
// count, and what the radio carried on one is not the other's.
func TestCountPerNetwork(t *testing.T) {
	limit := config.DataLimit{Enabled: true, Period: config.PeriodMonth, MB: 100, StartDay: 1}
	now := time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC)
	s := Load(filepath.Join(t.TempDir(), "usage.json"))
	s.boot = "b"
	pass := func(line string, bytes uint64, others ...string) State {
		s.Begin()
		for _, o := range others {
			s.Count(o, limit, nil, now, time.UTC, true)
		}
		return s.Count(line, limit, &Reading{If: "wlan1", Index: 4, Bytes: bytes}, now, time.UTC, true)
	}
	hotspot, hotel := LineWifi("Hotspot"), LineWifi("Hotel")
	if hotspot == hotel || Line(config.UplinkWifi, "Hotspot") != hotspot || Line(config.UplinkPhone, "x") != config.UplinkPhone {
		t.Fatal("lines are not told apart")
	}
	pass(hotspot, 1_000_000, hotel)
	if st := pass(hotspot, 3_000_000, hotel); st.Used != 2_000_000 {
		t.Errorf("on the hotspot: %+v", st)
	}
	// On to the hotel's network, for a long while and a lot of data.
	pass(hotel, 3_500_000, hotspot)
	if st := pass(hotel, 900_000_000, hotspot); st.Used != 896_500_000 {
		t.Errorf("on the hotel's network: %+v", st)
	}
	// Back on the hotspot: none of that is the hotspot's.
	if st := pass(hotspot, 900_100_000, hotel); st.Used != 2_000_000 {
		t.Errorf("back on the hotspot: %+v, want what it had", st)
	}
	if st := pass(hotspot, 901_100_000, hotel); st.Used != 3_000_000 {
		t.Errorf("a megabyte later: %+v", st)
	}
}

func TestClockJumps(t *testing.T) {
	loc := time.UTC
	limit := config.DataLimit{Enabled: true, Period: config.PeriodMonth, MB: 10, StartDay: 1}
	s := Load(filepath.Join(t.TempDir(), "usage.json"))
	s.boot = "b"
	var bytes uint64
	use := func(now time.Time, trusted bool, more uint64) State {
		bytes += more
		s.Begin()
		return s.Count("phone", limit, &Reading{If: "usb0", Index: 3, Bytes: bytes}, now, loc, trusted)
	}
	nov := time.Date(2026, 11, 20, 12, 0, 0, 0, loc)
	use(nov, true, 0)
	use(nov, true, 4_000_000)

	// After a power cut the clock starts from the time last saved, which
	// may be in the month before. That is no new period, and what is
	// used meanwhile is counted into the one that was current.
	oct := time.Date(2026, 10, 28, 12, 0, 0, 0, loc)
	st := use(oct, false, 1_000_000)
	if st.Used != 5_000_000 || !st.Start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("with the clock behind and not confirmed: %+v", st)
	}
	// The time is told: nothing changes.
	if st := use(nov.Add(time.Hour), true, 1_000_000); st.Used != 6_000_000 {
		t.Errorf("once the time is known again: %+v", st)
	}

	// A clock that runs ahead by mistake begins a new period, as it must:
	// nothing tells it from time passing.
	ahead := time.Date(2027, 3, 5, 0, 0, 0, 0, loc)
	if st := use(ahead, true, 2_000_000); st.Used != 2_000_000 || !st.Start.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("with the clock ahead: %+v", st)
	}
	// Put right, and confirmed: the period is the real one again, and
	// what was counted since is kept. Without that, the count would stand
	// still until March.
	st = use(nov.Add(2*time.Hour), true, 3_000_000)
	if st.Used != 5_000_000 || !st.Start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, loc)) || !st.End.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("after the clock was put right: %+v", st)
	}
	// And December is a new period when it comes.
	if st := use(time.Date(2026, 12, 2, 0, 0, 0, 0, loc), true, 1_000_000); st.Used != 1_000_000 {
		t.Errorf("in December: %+v", st)
	}

	// Another kind of period is not time passing: what was used today
	// has been used.
	daily := limit
	daily.Period = config.PeriodDay
	s.Begin()
	bytes += 500_000
	st = s.Count("phone", daily, &Reading{If: "usb0", Index: 3, Bytes: bytes}, time.Date(2026, 12, 2, 9, 0, 0, 0, loc), loc, true)
	if st.Used != 1_500_000 || !st.Start.Equal(time.Date(2026, 12, 2, 0, 0, 0, 0, loc)) {
		t.Errorf("after the limit became a daily one: %+v", st)
	}
	s.Begin()
	st = s.Count("phone", daily, &Reading{If: "usb0", Index: 3, Bytes: bytes}, time.Date(2026, 12, 3, 0, 0, 1, 0, loc), loc, true)
	if st.Used != 0 {
		t.Errorf("the day after: %+v", st)
	}
}
