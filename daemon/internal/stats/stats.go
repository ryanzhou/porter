// Package stats keeps traffic history: bytes per minute for the last
// hour and per hour for the last three days, for each device and for the
// network as a whole.
package stats

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

const (
	Minutes = 60
	Hours   = 72
	// Total is the pseudo-device holding the sum over all devices.
	Total = ""
)

// Sample is the traffic of one time bucket starting at T (unix seconds).
type Sample struct {
	T    int64  `json:"t"`
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
}

type series struct {
	minutes map[int64]*Sample
	hours   map[int64]*Sample
}

type Store struct {
	mu    sync.Mutex
	path  string
	dev   map[string]*series
	dirty bool
}

// Load reads the hourly history saved by a previous run.
func Load(path string) *Store {
	s := &Store{path: path, dev: map[string]*series{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var saved map[string][]Sample
	if json.Unmarshal(b, &saved) != nil {
		return s
	}
	cutoff := time.Now().Add(-Hours * time.Hour).Unix()
	for mac, list := range saved {
		se := s.get(mac)
		for _, sm := range list {
			if sm.T >= cutoff {
				c := sm
				se.hours[sm.T] = &c
			}
		}
	}
	return s
}

func (s *Store) get(mac string) *series {
	se := s.dev[mac]
	if se == nil {
		se = &series{minutes: map[int64]*Sample{}, hours: map[int64]*Sample{}}
		s.dev[mac] = se
	}
	return se
}

func add(m map[int64]*Sample, t int64, up, down uint64) {
	b := m[t]
	if b == nil {
		b = &Sample{T: t}
		m[t] = b
	}
	b.Up += up
	b.Down += down
}

// Add records traffic of a device at the given time.
func (s *Store) Add(mac string, up, down uint64, now time.Time) {
	if up == 0 && down == 0 {
		return
	}
	minute := now.Unix() / 60 * 60
	hour := now.Unix() / 3600 * 3600
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range []string{mac, Total} {
		se := s.get(key)
		add(se.minutes, minute, up, down)
		add(se.hours, hour, up, down)
		if key == Total && mac == Total {
			break
		}
	}
	s.dirty = true
}

// prune drops buckets that have aged out.
func (s *Store) prune(now time.Time) {
	minCut := now.Add(-Minutes*time.Minute).Unix() / 60 * 60
	hourCut := now.Add(-Hours*time.Hour).Unix() / 3600 * 3600
	for mac, se := range s.dev {
		for t := range se.minutes {
			if t <= minCut {
				delete(se.minutes, t)
			}
		}
		for t := range se.hours {
			if t <= hourCut {
				delete(se.hours, t)
			}
		}
		if len(se.minutes) == 0 && len(se.hours) == 0 && mac != Total {
			delete(s.dev, mac)
		}
	}
}

func fill(m map[int64]*Sample, last, step int64, n int) []Sample {
	out := make([]Sample, n)
	for i := 0; i < n; i++ {
		t := last - int64(n-1-i)*step
		out[i].T = t
		if b := m[t]; b != nil {
			out[i].Up, out[i].Down = b.Up, b.Down
		}
	}
	return out
}

// Get returns a device's history (Total for the network), oldest first,
// with empty buckets included so the result is evenly spaced.
func (s *Store) Get(mac string, now time.Time) (minutes, hours []Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	se := s.dev[mac]
	if se == nil {
		se = &series{}
	}
	return fill(se.minutes, now.Unix()/60*60, 60, Minutes), fill(se.hours, now.Unix()/3600*3600, 3600, Hours)
}

// Top is a device's traffic over a period.
type Top struct {
	MAC  string `json:"mac"`
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
}

// TopDevices ranks devices by traffic over the last n hours.
func (s *Store) TopDevices(n int, now time.Time) []Top {
	if n < 1 {
		n = 1
	}
	if n > Hours {
		n = Hours
	}
	cut := now.Add(-time.Duration(n)*time.Hour).Unix() / 3600 * 3600
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	out := []Top{}
	for mac, se := range s.dev {
		if mac == Total {
			continue
		}
		t := Top{MAC: mac}
		for ts, b := range se.hours {
			if ts > cut {
				t.Up += b.Up
				t.Down += b.Down
			}
		}
		if t.Up+t.Down > 0 {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Up+out[i].Down, out[j].Up+out[j].Down
		if a != b {
			return a > b
		}
		return out[i].MAC < out[j].MAC
	})
	return out
}

// Save writes the hourly history if it changed.
func (s *Store) Save() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	s.prune(time.Now())
	saved := map[string][]Sample{}
	for mac, se := range s.dev {
		for _, b := range se.hours {
			saved[mac] = append(saved[mac], *b)
		}
		sort.Slice(saved[mac], func(i, j int) bool { return saved[mac][i].T < saved[mac][j].T })
	}
	s.dirty = false
	s.mu.Unlock()
	b, _ := json.Marshal(saved)
	_ = config.WriteFileAtomic(s.path, b, 0o600)
}
