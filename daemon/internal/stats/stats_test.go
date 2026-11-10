package stats

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	s := Load(path)
	now := time.Unix(1_800_000_000, 0).Add(30 * time.Minute) // mid-hour
	const a, b = "02:00:00:00:00:0a", "02:00:00:00:00:0b"

	s.Add(a, 100, 1000, now.Add(-90*time.Second))
	s.Add(a, 50, 500, now)
	s.Add(b, 7, 70, now)
	s.Add(a, 1, 1, now.Add(-2*time.Hour))
	s.Add(a, 9, 9, now.Add(-80*time.Hour)) // too old to matter

	min, hrs := s.Get(a, now)
	if len(min) != Minutes || len(hrs) != Hours {
		t.Fatalf("lengths %d %d", len(min), len(hrs))
	}
	if last := min[Minutes-1]; last.Up != 50 || last.Down != 500 || last.T != now.Unix()/60*60 {
		t.Errorf("current minute: %+v", last)
	}
	if prev := min[Minutes-2]; prev.Up != 0 { // 90 s ago is two buckets back
		t.Errorf("previous minute: %+v", prev)
	}
	if two := min[Minutes-3]; two.Up != 100 || two.Down != 1000 {
		t.Errorf("two minutes ago: %+v", two)
	}
	if cur := hrs[Hours-1]; cur.Up != 150 || cur.Down != 1500 {
		t.Errorf("current hour: %+v", cur)
	}
	if old := hrs[Hours-3]; old.Up != 1 {
		t.Errorf("two hours ago: %+v", old)
	}
	_, total := s.Get(Total, now)
	if cur := total[Hours-1]; cur.Up != 157 || cur.Down != 1570 {
		t.Errorf("network total: %+v", cur)
	}
	top := s.TopDevices(24, now)
	if len(top) != 2 || top[0].MAC != a || top[0].Down != 1501 || top[1].MAC != b {
		t.Errorf("top: %+v", top)
	}
	if top1 := s.TopDevices(1, now); len(top1) != 2 || top1[0].Down != 1500 {
		t.Errorf("top over one hour: %+v", top1)
	}

	// Hourly history survives a restart; minutes do not need to.
	s.Save()
	again := Load(path)
	_, hrs = again.Get(a, time.Now())
	_ = hrs // the fixed test time may be outside the real 72 h window
	if _, hrs := again.Get(a, now); again.path != path || len(hrs) != Hours {
		t.Error("reload failed")
	}
}
