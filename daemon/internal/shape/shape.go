// Package shape holds an uplink a little below the speed of the line
// behind it, so that what has to wait does its waiting in the router and
// not in the modem, where one upload makes everything else stand in line
// behind it. The queue here is CAKE (sch_cake): it sends at the rate it
// is told, shares that rate fairly between the devices at home and then
// between each device's connections, and signals a sender to slow down
// long before the queue is long.
//
// What leaves by the uplink is queued on the uplink's own interface. What
// arrives cannot be queued where it arrives, so it is redirected to a
// stand-in interface (an IFB device, one per shaped uplink) and queued
// there on its way in. Both queues are told to look connections up in
// connection tracking ("nat"): after the router's address translation
// every packet on the uplink carries the router's own address, and it is
// the device behind it that fairness is about.
//
// The queues and the redirect are made with the tc program. CAKE's
// options, and the keywords for what a line adds to every packet, are
// tc's to know; a netlink library would have to be taught each of them.
// Only the stand-in interfaces are made over netlink, like the router's
// other interfaces.
package shape

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/ryanzhou/porter/daemon/internal/netx"
)

// Source is one uplink to hold to a speed.
type Source struct {
	// Key is the uplink's entry in the configured order, and If the
	// interface its traffic really leaves by: a PPPoE session's or a
	// VLAN's, where the uplink has one, not the port under it.
	Key string
	If  string
	// UpKbps and DownKbps are the rates, in kbit/s.
	UpKbps, DownKbps int
	// PPPoE: the uplink is a PPPoE session. VLAN: its frames carry a
	// VLAN tag. Both add to what every packet costs on the line.
	PPPoE bool
	VLAN  bool
	// Bypass leaves the uplink unshaped for now, while its speed is being
	// measured: the line's own speed is what the measurement is after.
	Bypass bool
}

// States of a source that is to be shaped.
const (
	StActive    = "active"
	StWaiting   = "waiting"   // its interface is not there: nothing plugged in, no session yet
	StMeasuring = "measuring" // left alone while its speed is measured
	StError     = "error"
)

// Direction is one way through a shaped source: the rate it is held to,
// and how many packets the queue has dropped or marked (ECN) to make a
// sender slow down, since the queue was put there. Counters that move
// show that the queue here is the one that fills.
type Direction struct {
	Kbps    int    `json:"kbps"`
	Dropped uint64 `json:"dropped"`
	Marked  uint64 `json:"marked"`
}

// Status is how the shaping of one source stands.
type Status struct {
	Key   string    `json:"key"`
	If    string    `json:"if"`
	State string    `json:"state"`
	Down  Direction `json:"down"`
	Up    Direction `json:"up"`
	// Error is what tc said, when State is StError.
	Error string `json:"error,omitempty"`
}

const (
	// handle names the queues this package attaches, so that one it finds
	// can be told from anything else's.
	handle = "1ca:"
	// ifbPrefix begins the name of every stand-in interface made here.
	ifbPrefix = "ifb-"
)

// IFBName is the stand-in interface on which what arrives on an uplink's
// interface is queued. An interface whose name leaves no room for the
// prefix in 15 bytes is named by a hash instead.
func IFBName(ifname string) string {
	if len(ifbPrefix)+len(ifname) <= 15 {
		return ifbPrefix + ifname
	}
	sum := sha256.Sum256([]byte(ifname))
	return (ifbPrefix + hex.EncodeToString(sum[:]))[:15]
}

// overhead is what the source's line adds to every packet, in CAKE's
// keywords, so that small packets are counted for what they really take.
// The router knows the framing only as far as its own interface goes:
//
//   - A PPPoE session is taken to run over VDSL2, where most are
//     ("pppoe-ptm": the session's and Ethernet's headers, and the line's
//     64/65 coding). Over fibre that asks for 1.5 % more room than
//     needed, which costs that much speed and nothing else.
//   - Anything else is counted as Ethernet with its preamble and gap.
//     For a cable to a modem that is what it is; behind a Wi-Fi network
//     or a phone the real line is unknown, and no line adds less.
//   - A VLAN tag is four bytes more, either way.
func (s Source) overhead() []string {
	base := "ethernet"
	if s.PPPoE {
		base = "pppoe-ptm"
	}
	if s.VLAN {
		return []string{base, "ether-vlan"}
	}
	return []string{base}
}

// egress is the tc command for the queue of what leaves by the source:
// fair between the devices at home first, which are the senders here.
// "replace" with the handle changes a queue of this package's in place,
// and puts one where there is none.
func (s Source) egress() []string {
	args := []string{"qdisc", "replace", "dev", s.If, "root", "handle", handle, "cake", "bandwidth", fmt.Sprintf("%dkbit", s.UpKbps)}
	args = append(args, s.overhead()...)
	return append(args, "nat", "dual-srchost")
}

// ingress is the tc command for the queue of what arrives, on the
// source's stand-in interface: fair between the devices at home, which
// are the receivers. "ingress" tells CAKE that what it drops has already
// used the line, so that it counts it against the rate. The priority
// marks on a packet from the internet were set by a stranger; they are
// not followed ("besteffort").
func (s Source) ingress() []string {
	args := []string{"qdisc", "replace", "dev", IFBName(s.If), "root", "handle", handle, "cake", "bandwidth", fmt.Sprintf("%dkbit", s.DownKbps)}
	args = append(args, s.overhead()...)
	return append(args, "nat", "dual-dsthost", "ingress", "besteffort")
}

// redirect is the tc command that sends everything arriving on the
// source to its stand-in interface.
func (s Source) redirect() []string {
	return []string{"filter", "add", "dev", s.If, "parent", "ffff:", "protocol", "all", "prio", "1",
		"u32", "match", "u32", "0", "0", "action", "mirred", "egress", "redirect", "dev", IFBName(s.If)}
}

// qdisc is what is read of an entry of "tc -s -j qdisc show".
type qdisc struct {
	Kind   string `json:"kind"`
	Handle string `json:"handle"`
	Dev    string `json:"dev"`
	Root   bool   `json:"root"`
	Drops  uint64 `json:"drops"`
	Tins   []struct {
		Marked uint64 `json:"ecn_mark"`
	} `json:"tins"`
}

// ours finds this package's queue among an interface's.
func ours(qs []qdisc) (qdisc, bool) {
	for _, q := range qs {
		if q.Root && q.Kind == "cake" && q.Handle == handle {
			return q, true
		}
	}
	return qdisc{}, false
}

func hasIngress(qs []qdisc) bool {
	for _, q := range qs {
		if q.Kind == "ingress" {
			return true
		}
	}
	return false
}

func counters(qs []qdisc) (dropped, marked uint64) {
	q, _ := ours(qs)
	for _, t := range q.Tins {
		marked += t.Marked
	}
	return q.Drops, marked
}

// links is what the manager needs of the kernel's interfaces.
type links interface {
	Index(name string) int
	EnsureIFB(name string) (made bool, err error)
	IFBs(prefix string) []string
	Delete(name string)
}

type netlinks struct{}

func (netlinks) Index(name string) int               { return netx.Index(name) }
func (netlinks) EnsureIFB(name string) (bool, error) { return netx.EnsureIFB(name) }
func (netlinks) IFBs(prefix string) []string         { return netx.IFBs(prefix) }
func (netlinks) Delete(name string)                  { netx.DeleteLink(name) }

func runTC(args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("tc", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// tc's first line says what the kernel made of it.
		if msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); msg != "" {
			err = errors.New(msg)
		}
		return out, fmt.Errorf("tc %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// applied is what was last put in place on an interface: for which
// source, and on which interface of that name.
type applied struct {
	src   Source
	index int
}

// Manager keeps the queues of the shaped uplinks as they are wanted. It
// is driven from the control loop and from nowhere else.
//
// The queues live in the kernel and outlive the daemon, as the firewall
// does: a router whose daemon restarts goes on shaping, and the first
// pass afterwards takes over what it finds.
type Manager struct {
	Log *slog.Logger

	// tc runs the tc program and returns what it printed; links reaches
	// the interfaces. Both are replaced in tests.
	tc    func(args ...string) ([]byte, error)
	links links

	applied map[string]applied
	// clean: the last pass wanted nothing and left nothing behind, so
	// that a router that shapes nothing does not run tc to find that out
	// again every few seconds.
	clean  bool
	status []Status
	// said is the last problem logged, by interface ("" for reading the
	// queues at all), so that each is logged once and not on every pass.
	said map[string]string
}

func (m *Manager) init() {
	if m.applied != nil {
		return
	}
	m.applied, m.said = map[string]applied{}, map[string]string{}
	if m.tc == nil {
		m.tc = runTC
	}
	if m.links == nil {
		m.links = netlinks{}
	}
}

// say logs a problem with an interface once, however often it is met.
func (m *Manager) say(ifname string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if m.said[ifname] == msg {
		return
	}
	m.said[ifname] = msg
	switch {
	case err == nil:
	case ifname == "":
		m.Log.Warn("the queues of the internet sources could not be read", "err", err)
	default:
		m.Log.Warn("an internet source could not be held to its speeds", "if", ifname, "err", err)
	}
}

// Status is how each source given to the last Apply stands, in the same
// order.
func (m *Manager) Status() []Status {
	return append([]Status{}, m.status...)
}

// Apply makes the kernel's queues what the sources ask for: puts them on
// the sources that are there, changes their rates, takes them off what is
// no longer a source to shape, and puts back what went with an interface
// that was unplugged and plugged in again. It is called on every pass of
// the control loop and changes nothing that is already as wanted.
func (m *Manager) Apply(sources []Source) {
	m.init()
	want := map[string]Source{}
	status := make([]Status, 0, len(sources))
	for _, s := range sources {
		st := Status{Key: s.Key, If: s.If, State: StWaiting, Down: Direction{Kbps: s.DownKbps}, Up: Direction{Kbps: s.UpKbps}}
		if s.Bypass {
			st.State = StMeasuring
		} else {
			want[s.If] = s
		}
		status = append(status, st)
	}
	m.status = status
	if len(want) == 0 && m.clean {
		return
	}

	// What is there now. Without that (tc missing, or printing something
	// unreadable) nothing can be checked: what this run put in place is
	// then trusted to be there still, as long as its interface is.
	now, known := m.list()
	removed := m.sweep(want, now, known)
	for i := range status {
		st := &status[i]
		s, wanted := want[st.If]
		if !wanted {
			continue
		}
		err := m.ensure(s, now, known)
		switch {
		case err != nil:
			st.State, st.Error = StError, err.Error()
			m.say(s.If, err)
		case m.applied[s.If].index != 0:
			st.State = StActive
			st.Up.Dropped, st.Up.Marked = counters(now[s.If])
			st.Down.Dropped, st.Down.Marked = counters(now[IFBName(s.If)])
			m.say(s.If, nil)
		}
	}
	// Nothing wanted, and nothing found that had to go: a pass that took
	// something away is followed by one that sees whether it went.
	m.clean = known && len(want) == 0 && !removed
}

// list reads every queue there is, by interface.
func (m *Manager) list() (map[string][]qdisc, bool) {
	out, err := m.tc("-s", "-j", "qdisc", "show")
	var all []qdisc
	if err == nil {
		if err = json.Unmarshal(out, &all); err != nil {
			err = fmt.Errorf("reading tc's list of queues: %w", err)
		}
	}
	m.say("", err)
	if err != nil {
		return nil, false
	}
	by := map[string][]qdisc{}
	for _, q := range all {
		by[q.Dev] = append(by[q.Dev], q)
	}
	return by, true
}

// undo takes this package's queues off an interface. What tc says is not
// looked at: the interface may be gone, and its queues with it.
func (m *Manager) undo(ifname string) {
	m.tc("qdisc", "del", "dev", ifname, "ingress")
	m.tc("qdisc", "del", "dev", ifname, "root")
}

// sweep takes away what is not wanted: the queues of a source that is no
// longer shaped, and whatever an earlier run of the daemon left on an
// interface that is not shaped now. A redirect always goes before the
// stand-in interface it leads to: what is redirected to an interface that
// is gone is lost, and that would be everything arriving on the uplink.
// So while the queues cannot be read (known is false), the only stand-in
// interfaces removed are those of sources this run has just undone.
func (m *Manager) sweep(want map[string]Source, now map[string][]qdisc, known bool) (removed bool) {
	keep := map[string]bool{}
	for dev := range want {
		if m.links.Index(dev) != 0 {
			keep[IFBName(dev)] = true
		}
	}
	undone := map[string]bool{}
	for dev := range m.applied {
		if _, wanted := want[dev]; !wanted {
			m.undo(dev)
			delete(m.applied, dev)
			delete(m.said, dev)
			undone[IFBName(dev)], removed = true, true
			m.Log.Info("internet source no longer held to a speed", "if", dev)
		}
	}
	stale := map[string]bool{}
	for _, name := range m.links.IFBs(ifbPrefix) {
		if !keep[name] && (known || undone[name]) {
			stale[name] = true
		}
	}
	for dev, qs := range now {
		if _, wanted := want[dev]; wanted || undone[IFBName(dev)] || strings.HasPrefix(dev, ifbPrefix) {
			continue
		}
		if hasIngress(qs) && stale[IFBName(dev)] {
			m.tc("qdisc", "del", "dev", dev, "ingress")
			removed = true
		}
		if _, left := ours(qs); left {
			m.tc("qdisc", "del", "dev", dev, "root")
			removed = true
		}
	}
	for name := range stale {
		m.links.Delete(name)
		removed = true
	}
	return removed
}

// ensure puts a source's queues in place, or leaves them when they are.
// A source whose interface is not there is left for a later pass.
func (m *Manager) ensure(s Source, now map[string][]qdisc, known bool) error {
	index := m.links.Index(s.If)
	if index == 0 {
		delete(m.applied, s.If)
		return nil
	}
	ifb := IFBName(s.If)
	was, had := m.applied[s.If]
	// In place: put there by this run, on this very interface (one that
	// was unplugged and plugged in again has the name and nothing else),
	// and, as far as can be seen, still there.
	inPlace := had && was.index == index
	if inPlace && known {
		_, out := ours(now[s.If])
		_, in := ours(now[ifb])
		inPlace = out && in && hasIngress(now[s.If])
	}
	made, err := m.links.EnsureIFB(ifb)
	if err != nil {
		return err
	}
	if inPlace && !made && was.src == s {
		return nil
	}
	// The stand-in interface and its queue come first, the redirect to it
	// last: there is never a redirect that leads nowhere. A change of
	// rates leaves the redirect alone, so that nothing passes unshaped
	// while it is made.
	fail := func(err error) error {
		m.undo(s.If)
		delete(m.applied, s.If)
		return err
	}
	if _, err := m.tc(s.egress()...); err != nil {
		return fail(err)
	}
	if _, err := m.tc(s.ingress()...); err != nil {
		return fail(err)
	}
	if !inPlace || made {
		if !known || hasIngress(now[s.If]) {
			m.tc("qdisc", "del", "dev", s.If, "ingress")
		}
		if _, err := m.tc("qdisc", "add", "dev", s.If, "handle", "ffff:", "ingress"); err != nil {
			return fail(err)
		}
		if _, err := m.tc(s.redirect()...); err != nil {
			return fail(err)
		}
	}
	m.applied[s.If] = applied{src: s, index: index}
	m.Log.Info("internet source held to a speed", "if", s.If, "down_kbps", s.DownKbps, "up_kbps", s.UpKbps)
	return nil
}
