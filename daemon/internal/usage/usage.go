// Package usage counts the data each uplink with a data limit has
// carried in the current day or month, and says when the limit is
// reached.
//
// The count comes from the interface's own byte counters, read every few
// seconds by the control loop, and is kept across restarts in a small
// file that is written as rarely as the traffic history is. It belongs to
// the line, not to the interface: a phone that is unplugged and plugged
// in again, or a PPPoE session that is made anew, goes on from where it
// was.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// PeriodStart is the beginning of the period that now falls in: midnight
// for a daily limit, and for a monthly one midnight on the day of the
// month it starts on, or on the month's last day where the month is
// shorter than that.
func PeriodStart(l config.DataLimit, now time.Time, loc *time.Location) time.Time {
	now = now.In(loc)
	y, m, d := now.Date()
	if l.Period == config.PeriodDay {
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	start := monthStart(l, y, m, loc)
	if now.Before(start) {
		start = monthStart(l, y, m-1, loc)
	}
	return start
}

// PeriodEnd is when the period that began at start ends and the next
// begins.
func PeriodEnd(l config.DataLimit, start time.Time, loc *time.Location) time.Time {
	y, m, d := start.In(loc).Date()
	if l.Period == config.PeriodDay {
		return time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	}
	// Counted from the month the period began in, not from its day: a
	// period that began on 28 February because the month has no 31st is
	// followed by one that begins on 31 March.
	return monthStart(l, y, m+1, loc)
}

// monthStart is the day a monthly period begins on in the given month. A
// month outside 1 to 12 is taken as time.Date takes it.
func monthStart(l config.DataLimit, y int, m time.Month, loc *time.Location) time.Time {
	first := time.Date(y, m, 1, 0, 0, 0, 0, loc)
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(max(l.StartDay, 1), last), 0, 0, 0, 0, loc)
}

// record is what is kept of one line.
type record struct {
	// Start is the beginning of the period Used was counted in (unix
	// time), and Shape the kind of period that was, so that a limit which
	// is changed to another kind does not look like time having passed.
	Start int64  `json:"start"`
	Shape string `json:"shape"`
	Used  uint64 `json:"used"`
	// Told: the activity log has an entry for the limit being reached in
	// this period.
	Told bool `json:"told,omitempty"`
	// If, Index, Boot and Last say where the interface's counter stood
	// when it was last read: the sum of bytes received and sent, of the
	// interface with that name and number, since the system started with
	// that boot id. A counter that carries on from there is read as what
	// was carried since; anything else starts from nothing.
	If    string `json:"if,omitempty"`
	Index int    `json:"index,omitempty"`
	Boot  string `json:"boot,omitempty"`
	Last  uint64 `json:"last"`
	// seen is the pass of the control loop (Store.Begin) on which the
	// line's interface was last read by this process, 0 for never.
	seen uint64
}

// Store holds the counts, by line: an uplink's key, or for a Wi-Fi
// network LineWifi of its name.
type Store struct {
	mu    sync.Mutex
	path  string
	boot  string
	recs  map[string]*record
	dirty bool
	pass  uint64
}

// Begin starts a pass of the control loop. A line whose interface is read
// on one pass and was not on the pass before has been away meanwhile.
func (s *Store) Begin() {
	s.mu.Lock()
	s.pass++
	s.mu.Unlock()
}

// LineWifi is the line a saved Wi-Fi network is counted as.
func LineWifi(ssid string) string { return config.UplinkWifi + "/" + ssid }

// Line is what an uplink is counted as: its key, or for Wi-Fi the network
// it has joined.
func Line(key, ssid string) string {
	if key == config.UplinkWifi {
		return LineWifi(ssid)
	}
	return key
}

// Load reads the counts saved by an earlier run.
func Load(path string) *Store {
	s := &Store{path: path, recs: map[string]*record{}}
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		s.boot = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(path); err == nil {
		var saved map[string]*record
		if json.Unmarshal(b, &saved) == nil {
			for line, r := range saved {
				if r != nil {
					s.recs[line] = r
				}
			}
		}
	}
	return s
}

// Reading is an interface's counter at this moment: the bytes it has
// received and sent since it came to be, and the number the kernel knows
// it by, which a new interface of the same name does not share.
type Reading struct {
	If    string
	Index int
	Bytes uint64
}

// State is how a line stands in its period.
type State struct {
	Used  uint64
	Start time.Time
	End   time.Time
	// Reached: the amount is spent. Lifted: it is used all the same, for
	// this period.
	Reached bool
	Lifted  bool
}

// Spent reports whether the line is out of use for the rest of the period.
func (st State) Spent() bool { return st.Reached && !st.Lifted }

// Count brings one line up to date and says how it stands. cur is its
// interface's counter, nil while the uplink is not there. trusted says
// that the clock has been confirmed since the router started.
//
// A period only ever follows the one before. A clock that is behind (the
// router was off, and has not been told the time yet) therefore counts
// into the period that was current when it was last right. A clock that
// had run ahead, and has now been confirmed to be earlier than the period
// that was begun, moves the period back and keeps what was counted: more
// than was used in it, never less.
func (s *Store) Count(line string, l config.DataLimit, cur *Reading, now time.Time, loc *time.Location, trusted bool) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := PeriodStart(l, now, loc)
	shape := l.Period
	if l.Period == config.PeriodMonth {
		shape = fmt.Sprintf("%s/%d", l.Period, l.StartDay)
	}
	r := s.recs[line]
	switch {
	case r == nil:
		r = &record{Start: start.Unix(), Shape: shape}
		s.recs[line] = r
		s.dirty = true
	case r.Shape != shape:
		// Another kind of period: what has been used has still been used.
		r.Start, r.Shape, r.Told, s.dirty = start.Unix(), shape, false, true
	case start.Unix() > r.Start:
		r.Start, r.Used, r.Told, s.dirty = start.Unix(), 0, false, true
	case start.Unix() < r.Start && trusted:
		r.Start, s.dirty = start.Unix(), true
	}
	if cur != nil {
		same := r.If == cur.If && r.Index == cur.Index && r.Boot == s.boot && cur.Bytes >= r.Last
		carried := cur.Bytes
		switch {
		case r.If == "":
			// The first reading of a line: what its interface carried
			// before anybody asked for a count is not counted.
			carried = 0
		case same && r.seen != 0 && r.seen+1 < s.pass:
			// The interface is the one it was, and for a while it was not
			// this line's: a radio that was on another network in between.
			// What it carried then is not this line's either.
			carried = 0
		case same:
			carried = cur.Bytes - r.Last
		}
		r.seen = s.pass
		if carried > 0 || r.If != cur.If || r.Index != cur.Index || r.Boot != s.boot {
			s.dirty = true
		}
		r.Used += carried
		r.If, r.Index, r.Boot, r.Last = cur.If, cur.Index, s.boot, cur.Bytes
	}
	begun := time.Unix(r.Start, 0)
	st := State{Used: r.Used, Start: begun, End: PeriodEnd(l, begun, loc)}
	st.Reached = l.Enabled && l.MB > 0 && r.Used >= l.Bytes()
	st.Lifted = st.Reached && now.Unix() < l.LiftUntil
	return st
}

// Tell reports, once in a period, that a line's limit has been reached
// and nobody has been told yet.
func (s *Store) Tell(line string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[line]
	if r == nil || r.Told {
		return false
	}
	r.Told, s.dirty = true, true
	return true
}

// Keep forgets every line that is not among those given: a limit that was
// switched off, a network that was forgotten.
func (s *Store) Keep(lines map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for line := range s.recs {
		if !lines[line] {
			delete(s.recs, line)
			s.dirty = true
		}
	}
}

// Save writes the counts if they changed.
func (s *Store) Save() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	b, _ := json.Marshal(s.recs)
	s.dirty = false
	s.mu.Unlock()
	_ = config.WriteFileAtomic(s.path, b, 0o600)
}
