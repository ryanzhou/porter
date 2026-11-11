package fw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Manager owns the kernel ruleset and the addresses learned for domain
// rules. The ruleset is always replaced as a whole, so the learned
// addresses are kept here and re-installed with every replacement.
type Manager struct {
	mu      sync.Mutex
	learned map[string]map[netip.Addr]time.Time // set -> addr -> expiry
	sets    map[string]bool                     // name sets in the live ruleset
	now     func() time.Time
}

func NewManager() *Manager {
	return &Manager{learned: map[string]map[netip.Addr]time.Time{}, sets: map[string]bool{}, now: time.Now}
}

func nft(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Apply installs the plan. Learned addresses for sets that no longer exist
// are forgotten.
func (m *Manager) Apply(p *Plan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	sets := map[string]bool{}
	for _, r := range p.Rules {
		if r.Domains {
			sets[SetName(r.ID, false)] = true
			sets[SetName(r.ID, true)] = true
		}
	}
	p.Learned = map[string][]Elem{}
	for name, addrs := range m.learned {
		if !sets[name] {
			delete(m.learned, name)
			continue
		}
		for a, exp := range addrs {
			ttl := int(exp.Sub(now) / time.Second)
			if ttl <= 0 {
				delete(addrs, a)
				continue
			}
			p.Learned[name] = append(p.Learned[name], Elem{Addr: a, TTL: ttl})
		}
	}
	if err := nft(Render(p)); err != nil {
		return err
	}
	m.sets = sets
	return nil
}

// Learn adds resolved addresses to a domain rule's set. It returns only
// after the kernel has them, so the DNS answer can be released knowing the
// client's first packet will already match.
func (m *Manager) Learn(ruleID string, addrs []netip.Addr, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var script strings.Builder
	var pending []learnedEntry
	for _, v6 := range []bool{false, true} {
		name := SetName(ruleID, v6)
		if !m.sets[name] {
			continue
		}
		var items, stale []string
		for _, a := range addrs {
			a = a.Unmap()
			if a.Is6() != v6 {
				continue
			}
			// Skip the kernel round trip while a comfortably fresh entry
			// exists.
			if exp, ok := m.learned[name][a]; ok && exp.Sub(now) > ttl/2 {
				continue
			}
			if _, known := m.learned[name][a]; known {
				stale = append(stale, a.String())
			}
			pending = append(pending, learnedEntry{name, a, now.Add(ttl)})
			items = append(items, fmt.Sprintf("%s timeout %ds", a, int(ttl/time.Second)))
		}
		// Adding an element that is already there leaves its lifetime as
		// it was, so one that is being renewed is taken out first; both
		// happen in the one transaction.
		if len(stale) > 0 {
			fmt.Fprintf(&script, "destroy element inet %s %s { %s }\n", Table, name, strings.Join(stale, ", "))
		}
		if len(items) > 0 {
			fmt.Fprintf(&script, "add element inet %s %s { %s }\n", Table, name, strings.Join(items, ", "))
		}
	}
	if script.Len() == 0 {
		return nil
	}
	if err := nft(script.String()); err != nil {
		return err
	}
	// Recorded only once the kernel has them: a record of something the
	// kernel does not hold would keep it from being added later.
	for _, e := range pending {
		if m.learned[e.set] == nil {
			m.learned[e.set] = map[netip.Addr]time.Time{}
		}
		m.learned[e.set][e.addr] = e.expiry
	}
	return nil
}

type learnedEntry struct {
	set    string
	addr   netip.Addr
	expiry time.Time
}

// Remove deletes the ruleset (used on shutdown so that a stopped daemon
// does not leave a half-configured router forwarding traffic).
func (m *Manager) Remove() error {
	return nft(fmt.Sprintf("table inet %s {}\ndelete table inet %s\n", Table, Table))
}

// Counters holds byte totals read from the accounting sets.
type Counters struct {
	Up    map[string]uint64 // by MAC
	Down4 map[netip.Addr]uint64
	Down6 map[netip.Addr]uint64
}

// ReadCounters returns the current accounting counters. They reset
// whenever the ruleset is replaced; callers accumulate deltas.
func ReadCounters() (*Counters, error) {
	c := &Counters{Up: map[string]uint64{}, Down4: map[netip.Addr]uint64{}, Down6: map[netip.Addr]uint64{}}
	for _, name := range []string{"acct_up", "acct_down4", "acct_down6"} {
		out, err := exec.Command("nft", "-j", "list", "set", "inet", Table, name).Output()
		if err != nil {
			return nil, fmt.Errorf("nft list set %s: %w", name, err)
		}
		var doc struct {
			Nftables []struct {
				Set *struct {
					Elem []struct {
						Elem struct {
							Val     string `json:"val"`
							Counter struct {
								Bytes uint64 `json:"bytes"`
							} `json:"counter"`
						} `json:"elem"`
					} `json:"elem"`
				} `json:"set"`
			} `json:"nftables"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			return nil, err
		}
		for _, n := range doc.Nftables {
			if n.Set == nil {
				continue
			}
			for _, e := range n.Set.Elem {
				if name == "acct_up" {
					c.Up[e.Elem.Val] = e.Elem.Counter.Bytes
					continue
				}
				a, err := netip.ParseAddr(e.Elem.Val)
				if err != nil {
					continue
				}
				if name == "acct_down4" {
					c.Down4[a] = e.Elem.Counter.Bytes
				} else {
					c.Down6[a] = e.Elem.Counter.Bytes
				}
			}
		}
	}
	return c, nil
}
