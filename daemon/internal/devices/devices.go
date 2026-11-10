// Package devices tracks the clients on the LAN: who they are, whether
// they are present, and how much traffic they have moved.
package devices

import (
	"encoding/json"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/fw"
)

// Entry is one client device.
type Entry struct {
	MAC       string   `json:"mac"`
	Hostname  string   `json:"hostname,omitempty"`
	IPv4      string   `json:"ipv4,omitempty"`
	IPv6      []string `json:"ipv6,omitempty"`
	FirstSeen int64    `json:"first_seen"`
	LastSeen  int64    `json:"last_seen"`
	Online    bool     `json:"online"`
	// Link is the access point interface, or "wired".
	Link   string `json:"link,omitempty"`
	Signal int    `json:"signal,omitempty"`
	Up     uint64 `json:"up"`   // bytes sent by the device
	Down   uint64 `json:"down"` // bytes sent to the device
}

type Table struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry
	byIP    map[netip.Addr]string
	dirty   bool
	// Last raw counter values, to turn resets into deltas.
	lastUp   map[string]uint64
	lastDown map[netip.Addr]uint64
	// OnNew is called once for every device never seen before.
	OnNew func(e Entry)
}

type persisted struct {
	MAC       string `json:"mac"`
	Hostname  string `json:"hostname,omitempty"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	Up        uint64 `json:"up"`
	Down      uint64 `json:"down"`
}

func Load(path string) *Table {
	t := &Table{path: path, entries: map[string]*Entry{}, byIP: map[netip.Addr]string{},
		lastUp: map[string]uint64{}, lastDown: map[netip.Addr]uint64{}}
	if b, err := os.ReadFile(path); err == nil {
		var list []persisted
		if json.Unmarshal(b, &list) == nil {
			for _, p := range list {
				t.entries[p.MAC] = &Entry{MAC: p.MAC, Hostname: p.Hostname, FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, Up: p.Up, Down: p.Down}
			}
		}
	}
	return t
}

// Save writes the table if it changed. Called on a slow timer so that
// traffic counters do not wear the storage.
func (t *Table) Save() {
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return
	}
	list := make([]persisted, 0, len(t.entries))
	for _, e := range t.entries {
		list = append(list, persisted{e.MAC, e.Hostname, e.FirstSeen, e.LastSeen, e.Up, e.Down})
	}
	t.dirty = false
	t.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].MAC < list[j].MAC })
	b, _ := json.Marshal(list)
	_ = config.WriteFileAtomic(t.path, b, 0o600)
}

func (t *Table) get(mac string, now int64) *Entry {
	e := t.entries[mac]
	if e == nil {
		e = &Entry{MAC: mac, FirstSeen: now}
		t.entries[mac] = e
		t.dirty = true
		if t.OnNew != nil {
			cp := *e
			go t.OnNew(cp)
		}
	}
	return e
}

// Lease records a DHCP lease event ("add", "old", "del").
func (t *Table) Lease(action, mac, ip, hostname string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().Unix()
	e := t.get(mac, now)
	a, err := netip.ParseAddr(ip)
	if action == "del" {
		if err == nil && t.byIP[a] == mac {
			delete(t.byIP, a)
		}
		if e.IPv4 == ip {
			e.IPv4 = ""
		}
		return
	}
	if err == nil {
		t.byIP[a] = mac
		e.IPv4 = ip
	}
	if hostname != "" && hostname != e.Hostname {
		e.Hostname = hostname
		t.dirty = true
	}
	e.LastSeen = now
}

// Station is a wireless client reported by an access point.
type Station struct {
	MAC    string
	Iface  string
	Signal int
}

// Observe updates presence from the neighbour table and the access
// points' station lists.
func (t *Table) Observe(neigh map[netip.Addr]string, stations []Station) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().Unix()
	seen := map[string]bool{}
	v6 := map[string][]string{}
	for ip, mac := range neigh {
		seen[mac] = true
		t.byIP[ip] = mac
		e := t.get(mac, now)
		if ip.Is4() {
			if e.IPv4 == "" {
				e.IPv4 = ip.String()
			}
		} else if !ip.IsLinkLocalUnicast() {
			v6[mac] = append(v6[mac], ip.String())
		}
	}
	wireless := map[string]Station{}
	for _, s := range stations {
		wireless[s.MAC] = s
		seen[s.MAC] = true
		t.get(s.MAC, now)
	}
	for mac, e := range t.entries {
		was := e.Online
		e.Online = seen[mac]
		if e.Online {
			e.LastSeen = now
			if s, ok := wireless[mac]; ok {
				e.Link, e.Signal = s.Iface, s.Signal
			} else {
				e.Link, e.Signal = "wired", 0
			}
			sort.Strings(v6[mac])
			e.IPv6 = v6[mac]
		} else {
			e.Link, e.Signal, e.IPv6 = "", 0, nil
		}
		if was != e.Online {
			t.dirty = true
		}
	}
	// Forget address mappings of devices that are gone.
	for ip, mac := range t.byIP {
		if e := t.entries[mac]; e == nil || (!e.Online && e.IPv4 != ip.String()) {
			delete(t.byIP, ip)
		}
	}
}

// Account folds the firewall's accounting counters into per-device
// totals.
func (t *Table) Account(c *fw.Counters) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delta := func(cur, last uint64) uint64 {
		if cur >= last {
			return cur - last
		}
		return cur // counter was reset
	}
	for mac, v := range c.Up {
		if e := t.entries[mac]; e != nil {
			if d := delta(v, t.lastUp[mac]); d > 0 {
				e.Up += d
				t.dirty = true
			}
		}
	}
	t.lastUp = c.Up
	down := map[netip.Addr]uint64{}
	for _, m := range []map[netip.Addr]uint64{c.Down4, c.Down6} {
		for ip, v := range m {
			down[ip] = v
			if e := t.entries[t.byIP[ip]]; e != nil {
				if d := delta(v, t.lastDown[ip]); d > 0 {
					e.Down += d
					t.dirty = true
				}
			}
		}
	}
	t.lastDown = down
}

// LookupMAC maps a client address to its device.
func (t *Table) LookupMAC(ip netip.Addr) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byIP[ip]
}

// List returns all devices, online first, then most recently seen.
func (t *Table) List() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		return out[i].MAC < out[j].MAC
	})
	return out
}

// Forget removes a device that is not present.
func (t *Table) Forget(mac string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.entries[mac]; e != nil && !e.Online {
		delete(t.entries, mac)
		t.dirty = true
	}
}

// Hosts returns name-to-address data for the DNS forwarder.
func (t *Table) Hosts() map[string][]netip.Addr {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string][]netip.Addr{}
	for _, e := range t.entries {
		if e.Hostname == "" || e.IPv4 == "" {
			continue
		}
		if a, err := netip.ParseAddr(e.IPv4); err == nil {
			out[e.Hostname] = append(out[e.Hostname], a)
		}
	}
	return out
}
