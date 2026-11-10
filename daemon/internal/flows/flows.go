// Package flows lists the connections LAN devices have open, and the
// ones that ended recently, from the kernel's connection tracking table.
package flows

import (
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Flow is one connection from a LAN device to somewhere else.
type Flow struct {
	MAC     string `json:"mac"`
	Proto   string `json:"proto"`
	Dst     string `json:"dst"`
	Port    int    `json:"port"`
	Name    string `json:"name,omitempty"`
	Country string `json:"country,omitempty"`
	// Path is "direct", "vpn:<id>" or "block".
	Path    string `json:"path"`
	Up      uint64 `json:"up"`
	Down    uint64 `json:"down"`
	Started int64  `json:"started"`
	Last    int64  `json:"last"`
	Active  bool   `json:"active"`
}

type key struct {
	proto    uint8
	src, dst netip.Addr
	sport    uint16
	dport    uint16
}

// Hooks supply what the tracker cannot know by itself.
type Hooks struct {
	// LAN reports whether an address belongs to the local network.
	LAN func(netip.Addr) bool
	// LookupMAC maps a LAN address to its device.
	LookupMAC func(netip.Addr) string
	// NameFor returns the name an address was resolved as, if any.
	NameFor func(netip.Addr) string
	// Country returns the address's country code, if known.
	Country func(netip.Addr) string
	// Path names the route a connection mark stands for.
	Path func(mark uint32) string
}

const (
	maxRecent = 400
	maxListed = 300
)

type Tracker struct {
	hooks Hooks

	mu                    sync.Mutex
	active                map[key]*Flow
	recent                []Flow // oldest first
	lastTable, lastActive int
	// list is replaceable for tests.
	list func() ([]*netlink.ConntrackFlow, error)
}

func New(hooks Hooks) *Tracker {
	return &Tracker{hooks: hooks, active: map[key]*Flow{}, list: listConntrack}
}

func listConntrack() ([]*netlink.ConntrackFlow, error) {
	var out []*netlink.ConntrackFlow
	var firstErr error
	for _, family := range []netlink.InetFamily{unix.AF_INET, unix.AF_INET6} {
		flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, family)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, flows...)
	}
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

func protoName(p uint8) string {
	switch p {
	case unix.IPPROTO_TCP:
		return "tcp"
	case unix.IPPROTO_UDP:
		return "udp"
	case unix.IPPROTO_ICMP:
		return "icmp"
	case unix.IPPROTO_ICMPV6:
		return "icmpv6"
	}
	return "other"
}

// Poll reads the connection table and updates the flow lists.
func (t *Tracker) Poll() error {
	table, err := t.list()
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[key]bool{}
	for _, c := range table {
		src, ok1 := netip.AddrFromSlice(c.Forward.SrcIP)
		dst, ok2 := netip.AddrFromSlice(c.Forward.DstIP)
		if !ok1 || !ok2 {
			continue
		}
		src, dst = src.Unmap(), dst.Unmap()
		// Only connections leaving the LAN: not LAN-to-LAN, not the
		// router's own, and not ones the router answered itself (DNS).
		if !t.hooks.LAN(src) || t.hooks.LAN(dst) || dst.IsMulticast() || dst.IsLinkLocalUnicast() {
			continue
		}
		if rs, ok := netip.AddrFromSlice(c.Reverse.SrcIP); ok && t.hooks.LAN(rs.Unmap()) {
			continue
		}
		k := key{c.Forward.Protocol, src, dst, c.Forward.SrcPort, c.Forward.DstPort}
		seen[k] = true
		f := t.active[k]
		if f == nil {
			f = &Flow{
				MAC: t.hooks.LookupMAC(src), Proto: protoName(c.Forward.Protocol),
				Dst: dst.String(), Port: int(c.Forward.DstPort),
				Name: t.hooks.NameFor(dst), Country: t.hooks.Country(dst),
				Started: now, Active: true,
			}
			t.active[k] = f
		}
		if f.MAC == "" {
			f.MAC = t.hooks.LookupMAC(src)
		}
		if f.Name == "" {
			f.Name = t.hooks.NameFor(dst)
		}
		f.Path = t.hooks.Path(c.Mark)
		f.Up, f.Down = c.Forward.Bytes, c.Reverse.Bytes
		f.Last = now
	}
	for k, f := range t.active {
		if seen[k] {
			continue
		}
		f.Active = false
		t.recent = append(t.recent, *f)
		delete(t.active, k)
	}
	if n := len(t.recent) - maxRecent; n > 0 {
		t.recent = append([]Flow(nil), t.recent[n:]...)
	}
	t.lastTable, t.lastActive = len(table), len(t.active)
	return nil
}

// Counts reports the size of the connection table at the last poll and
// how many of its entries were LAN devices' flows.
func (t *Tracker) Counts() (table, active int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastTable, t.lastActive
}

// List returns active flows (busiest first) followed by recently ended
// ones (latest first), for one device or, with mac "", for all.
func (t *Tracker) List(mac string) []Flow {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Flow, 0, len(t.active))
	for _, f := range t.active {
		if mac == "" || f.MAC == mac {
			out = append(out, *f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Up+out[i].Down, out[j].Up+out[j].Down
		if a != b {
			return a > b
		}
		if out[i].Dst != out[j].Dst {
			return out[i].Dst < out[j].Dst
		}
		return out[i].Port < out[j].Port
	})
	if len(out) > maxListed {
		out = out[:maxListed]
	}
	for i := len(t.recent) - 1; i >= 0 && len(out) < 2*maxListed; i-- {
		if mac == "" || t.recent[i].MAC == mac {
			out = append(out, t.recent[i])
		}
	}
	return out
}
