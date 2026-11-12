package shape

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// kernel stands in for the kernel's interfaces and queues: it carries out
// the tc commands the manager runs, as far as they go here, and complains
// when one of them would lose traffic.
type kernel struct {
	t    *testing.T
	next int
	// index of every interface that is there, and whether an IFB is up.
	ifs map[string]int
	ifb map[string]bool
	// cake holds, by interface, the options of a root CAKE queue with the
	// manager's handle; redirect where an interface's ingress queue sends
	// what arrives ("" for a queue without a redirect).
	cake     map[string]string
	redirect map[string]string
	// drops and marks are what the queues report.
	drops, marks map[string]uint64
	// ran lists the tc commands that changed something, in order, and
	// lists counts the times the queues were read.
	ran   []string
	lists int
	// refuse makes a command that contains it fail, and garbled makes
	// the list of queues unreadable.
	refuse  string
	garbled bool
}

func newKernel(t *testing.T, ifs ...string) *kernel {
	k := &kernel{t: t, ifs: map[string]int{}, ifb: map[string]bool{}, cake: map[string]string{}, redirect: map[string]string{},
		drops: map[string]uint64{}, marks: map[string]uint64{}}
	for _, name := range ifs {
		k.plug(name)
	}
	return k
}

// plug adds an interface, with a new index and no queues.
func (k *kernel) plug(name string) {
	k.next++
	k.ifs[name] = k.next
}

// unplug removes an interface and what was attached to it.
func (k *kernel) unplug(name string) {
	delete(k.ifs, name)
	delete(k.cake, name)
	delete(k.redirect, name)
}

func (k *kernel) Index(name string) int { return k.ifs[name] }

func (k *kernel) EnsureIFB(name string) (bool, error) {
	if k.ifs[name] != 0 && k.ifb[name] {
		return false, nil
	}
	if k.ifs[name] == 0 {
		k.plug(name)
	}
	k.ifb[name] = true
	return true, nil
}

func (k *kernel) IFBs(prefix string) []string {
	var out []string
	for name := range k.ifb {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (k *kernel) Delete(name string) {
	// The kernel lets this happen, and from then on drops everything
	// that arrives where the redirect is.
	for dev, to := range k.redirect {
		if to == name {
			k.t.Errorf("%s removed while what arrives on %s is still redirected to it", name, dev)
		}
	}
	k.ran = append(k.ran, "link del "+name)
	k.unplug(name)
	delete(k.ifb, name)
}

func (k *kernel) tc(args ...string) ([]byte, error) {
	line := strings.Join(args, " ")
	if line == "-s -j qdisc show" {
		k.lists++
		if k.garbled {
			return []byte("qdisc noqueue 0: dev lo root refcnt 2\n"), nil
		}
		return k.list(), nil
	}
	fail := func(msg string) ([]byte, error) { return nil, fmt.Errorf("tc %s: %s", line, msg) }
	if k.refuse != "" && strings.Contains(line, k.refuse) {
		return fail("Error: Specified qdisc kind is unknown.")
	}
	if len(args) < 4 || args[2] != "dev" {
		k.t.Fatalf("a tc command the test does not know: %s", line)
	}
	dev := args[3]
	if k.ifs[dev] == 0 {
		return fail("Cannot find device \"" + dev + "\"")
	}
	rest := strings.Join(args[4:], " ")
	switch args[0] + " " + args[1] {
	case "qdisc replace":
		opts, ok := strings.CutPrefix(rest, "root handle "+handle+" cake ")
		if !ok {
			k.t.Fatalf("a queue the test does not know: %s", line)
		}
		k.cake[dev] = opts
	case "qdisc add":
		if rest != "handle ffff: ingress" {
			k.t.Fatalf("a queue the test does not know: %s", line)
		}
		if _, there := k.redirect[dev]; there {
			return fail("Error: Exclusivity flag on, cannot modify.")
		}
		k.redirect[dev] = ""
	case "qdisc del":
		switch rest {
		case "ingress":
			if _, there := k.redirect[dev]; !there {
				return fail("Error: Cannot find specified qdisc on specified device.")
			}
			delete(k.redirect, dev)
		case "root":
			if _, there := k.cake[dev]; !there {
				return fail("Error: Cannot delete qdisc with handle of zero.")
			}
			delete(k.cake, dev)
		default:
			k.t.Fatalf("a tc command the test does not know: %s", line)
		}
	case "filter add":
		to, ok := strings.CutPrefix(rest, "parent ffff: protocol all prio 1 u32 match u32 0 0 action mirred egress redirect dev ")
		if !ok {
			k.t.Fatalf("a filter the test does not know: %s", line)
		}
		if _, there := k.redirect[dev]; !there {
			return fail("Error: Parent Qdisc doesn't exists.")
		}
		if k.ifs[to] == 0 || !k.ifb[to] {
			k.t.Errorf("what arrives on %s is redirected to %s, which is not there or not up", dev, to)
		}
		k.redirect[dev] = to
	default:
		k.t.Fatalf("a tc command the test does not know: %s", line)
	}
	k.ran = append(k.ran, line)
	return nil, nil
}

// list prints the queues the way "tc -s -j qdisc show" does, as far as
// the manager reads it.
func (k *kernel) list() []byte {
	var out []map[string]any
	names := make([]string, 0, len(k.ifs))
	for name := range k.ifs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, dev := range names {
		if _, shaped := k.cake[dev]; shaped {
			out = append(out, map[string]any{"kind": "cake", "handle": handle, "dev": dev, "root": true, "options": map[string]any{"bandwidth": 1000},
				"drops": k.drops[dev], "tins": []map[string]any{{"ecn_mark": k.marks[dev]}, {"ecn_mark": 0}, {"ecn_mark": 0}}})
		} else {
			out = append(out, map[string]any{"kind": "fq_codel", "handle": "0:", "dev": dev, "root": true, "drops": 7})
		}
		if _, there := k.redirect[dev]; there {
			out = append(out, map[string]any{"kind": "ingress", "handle": "ffff:", "dev": dev, "parent": "ffff:fff1"})
		}
	}
	b, _ := json.Marshal(out)
	return b
}

// took returns the commands run since the last call.
func (k *kernel) took() []string {
	out := k.ran
	k.ran = nil
	return out
}

func manager(k *kernel) *Manager {
	return &Manager{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), tc: k.tc, links: k}
}

func expect(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: ran\n  %s\nwanted\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func states(m *Manager) string {
	var out []string
	for _, st := range m.Status() {
		out = append(out, st.Key+"="+st.State)
	}
	return strings.Join(out, " ")
}

var wired = Source{Key: "eth:eth0", If: "eth0", UpKbps: 19000, DownKbps: 95000}

// The commands for each kind of source: the rates, fairness by device in
// the right direction, and what the line adds to a packet.
func TestCommands(t *testing.T) {
	cases := []struct {
		name            string
		src             Source
		egress, ingress string
	}{
		{"a wired port", wired,
			"qdisc replace dev eth0 root handle 1ca: cake bandwidth 19000kbit ethernet nat dual-srchost",
			"qdisc replace dev ifb-eth0 root handle 1ca: cake bandwidth 95000kbit ethernet nat dual-dsthost ingress besteffort"},
		{"a tagged port", Source{Key: "eth:eth0", If: "v35-eth0", UpKbps: 5000, DownKbps: 50000, VLAN: true},
			"qdisc replace dev v35-eth0 root handle 1ca: cake bandwidth 5000kbit ethernet ether-vlan nat dual-srchost",
			"qdisc replace dev ifb-v35-eth0 root handle 1ca: cake bandwidth 50000kbit ethernet ether-vlan nat dual-dsthost ingress besteffort"},
		{"a PPPoE session", Source{Key: "eth:eth0", If: "ppp-eth0", UpKbps: 10000, DownKbps: 40000, PPPoE: true},
			"qdisc replace dev ppp-eth0 root handle 1ca: cake bandwidth 10000kbit pppoe-ptm nat dual-srchost",
			"qdisc replace dev ifb-ppp-eth0 root handle 1ca: cake bandwidth 40000kbit pppoe-ptm nat dual-dsthost ingress besteffort"},
		{"a PPPoE session under a tag", Source{Key: "eth:eth0", If: "ppp-eth0", UpKbps: 10000, DownKbps: 40000, PPPoE: true, VLAN: true},
			"qdisc replace dev ppp-eth0 root handle 1ca: cake bandwidth 10000kbit pppoe-ptm ether-vlan nat dual-srchost",
			"qdisc replace dev ifb-ppp-eth0 root handle 1ca: cake bandwidth 40000kbit pppoe-ptm ether-vlan nat dual-dsthost ingress besteffort"},
		{"a Wi-Fi network", Source{Key: "wifi", If: "wlan1", UpKbps: 2500, DownKbps: 8000},
			"qdisc replace dev wlan1 root handle 1ca: cake bandwidth 2500kbit ethernet nat dual-srchost",
			"qdisc replace dev ifb-wlan1 root handle 1ca: cake bandwidth 8000kbit ethernet nat dual-dsthost ingress besteffort"},
		{"a phone", Source{Key: "phone", If: "usb0", UpKbps: 100, DownKbps: 10_000_000},
			"qdisc replace dev usb0 root handle 1ca: cake bandwidth 100kbit ethernet nat dual-srchost",
			"qdisc replace dev ifb-usb0 root handle 1ca: cake bandwidth 10000000kbit ethernet nat dual-dsthost ingress besteffort"},
	}
	for _, c := range cases {
		if got := strings.Join(c.src.egress(), " "); got != c.egress {
			t.Errorf("%s, leaving:\n  %s\nwanted\n  %s", c.name, got, c.egress)
		}
		if got := strings.Join(c.src.ingress(), " "); got != c.ingress {
			t.Errorf("%s, arriving:\n  %s\nwanted\n  %s", c.name, got, c.ingress)
		}
	}
	if got, want := strings.Join(wired.redirect(), " "), "filter add dev eth0 parent ffff: protocol all prio 1 u32 match u32 0 0 action mirred egress redirect dev ifb-eth0"; got != want {
		t.Errorf("redirect:\n  %s\nwanted\n  %s", got, want)
	}
}

// A stand-in interface has a name the kernel takes, its own for every
// interface, and one that begins the way the sweep looks for.
func TestIFBName(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"eth0", "ppp-eth0", "v4094-eth0", "wlan0", "usb0", "enx00e04c6801a7", "enx00e04c6801a8", "wlx9cefd5123456", "ppp-6f1e2a9c0b7"} {
		got := IFBName(name)
		if len(got) > 15 || !strings.HasPrefix(got, ifbPrefix) {
			t.Errorf("%s: %q is not a name for a stand-in interface", name, got)
		}
		if other, taken := seen[got]; taken {
			t.Errorf("%s and %s share %q", name, other, got)
		}
		seen[got] = name
	}
	if got := IFBName("eth0"); got != "ifb-eth0" {
		t.Errorf("eth0: %q", got)
	}
}

// A source through its life: it appears, is left alone while nothing
// changes, has its rates changed, is measured, is switched off, and its
// interface goes and comes back.
func TestLifeCycle(t *testing.T) {
	k := newKernel(t, "eth0", "eth1")
	m := manager(k)

	// Nothing to shape, on a router that never shaped: one look, and
	// after that not even that.
	m.Apply(nil)
	m.Apply(nil)
	if k.lists != 1 || len(k.took()) != 0 {
		t.Errorf("with nothing to shape the queues were read %d times and changed: %v", k.lists, k.ran)
	}

	m.Apply([]Source{wired})
	expect(t, "a source appears", k.took(), []string{
		"qdisc replace dev eth0 root handle 1ca: cake bandwidth 19000kbit ethernet nat dual-srchost",
		"qdisc replace dev ifb-eth0 root handle 1ca: cake bandwidth 95000kbit ethernet nat dual-dsthost ingress besteffort",
		"qdisc add dev eth0 handle ffff: ingress",
		"filter add dev eth0 parent ffff: protocol all prio 1 u32 match u32 0 0 action mirred egress redirect dev ifb-eth0",
	})
	want := Status{Key: "eth:eth0", If: "eth0", State: StActive, Down: Direction{Kbps: 95000}, Up: Direction{Kbps: 19000}}
	if st := m.Status(); len(st) != 1 || st[0] != want {
		t.Errorf("status: %+v", st)
	}

	// The counters are the queues': what leaves is counted on the
	// interface, what arrives on its stand-in.
	k.drops["eth0"], k.marks["eth0"], k.drops["ifb-eth0"], k.marks["ifb-eth0"] = 3, 4, 50, 60
	m.Apply([]Source{wired})
	expect(t, "nothing changed", k.took(), nil)
	want.Up.Dropped, want.Up.Marked, want.Down.Dropped, want.Down.Marked = 3, 4, 50, 60
	if st := m.Status(); st[0] != want {
		t.Errorf("status with counters: %+v", st[0])
	}

	// New rates change the two queues where they are; the redirect stays,
	// so that nothing arrives unshaped meanwhile.
	faster := wired
	faster.UpKbps, faster.DownKbps = 38000, 190000
	m.Apply([]Source{faster})
	expect(t, "the rates change", k.took(), []string{
		"qdisc replace dev eth0 root handle 1ca: cake bandwidth 38000kbit ethernet nat dual-srchost",
		"qdisc replace dev ifb-eth0 root handle 1ca: cake bandwidth 190000kbit ethernet nat dual-dsthost ingress besteffort",
	})
	if st := m.Status()[0]; st.State != StActive || st.Down.Kbps != 190000 || st.Up.Kbps != 38000 {
		t.Errorf("status after a change of rates: %+v", st)
	}

	// Measured, it is left unshaped, and says so; afterwards it is as it
	// was. The redirect goes before the interface it leads to.
	measured := faster
	measured.Bypass = true
	m.Apply([]Source{measured})
	expect(t, "its speed is measured", k.took(), []string{
		"qdisc del dev eth0 ingress", "qdisc del dev eth0 root", "link del ifb-eth0",
	})
	if got := states(m); got != "eth:eth0=measuring" {
		t.Errorf("while measured: %s", got)
	}
	m.Apply([]Source{faster})
	if got := k.took(); len(got) != 4 || states(m) != "eth:eth0=active" {
		t.Errorf("after the measurement: %s, %v", states(m), got)
	}

	// A second source next to it, and the first switched off.
	phone := Source{Key: "phone", If: "usb0", UpKbps: 4000, DownKbps: 20000}
	m.Apply([]Source{faster, phone})
	expect(t, "a phone that is not plugged in", k.took(), nil)
	if got := states(m); got != "eth:eth0=active phone=waiting" {
		t.Errorf("with a phone that is not there: %s", got)
	}
	k.plug("usb0")
	m.Apply([]Source{faster, phone})
	if got := k.took(); len(got) != 4 || !strings.Contains(got[0], "dev usb0 ") || states(m) != "eth:eth0=active phone=active" {
		t.Errorf("the phone plugged in: %s, %v", states(m), got)
	}
	m.Apply([]Source{phone})
	expect(t, "shaping switched off for one", k.took(), []string{
		"qdisc del dev eth0 ingress", "qdisc del dev eth0 root", "link del ifb-eth0",
	})
	if _, shaped := k.cake["usb0"]; !shaped || k.redirect["usb0"] != "ifb-usb0" {
		t.Errorf("the other source lost its queues: %v %v", k.cake, k.redirect)
	}

	// The phone is unplugged: its queues went with it, its stand-in is
	// taken away. Plugged in again it is another interface of the same
	// name, without queues, and gets them.
	k.unplug("usb0")
	m.Apply([]Source{phone})
	expect(t, "the source is gone", k.took(), []string{"link del ifb-usb0"})
	if got := states(m); got != "phone=waiting" {
		t.Errorf("unplugged: %s", got)
	}
	k.plug("usb0")
	m.Apply([]Source{phone})
	expect(t, "the source is back", k.took(), []string{
		"qdisc replace dev usb0 root handle 1ca: cake bandwidth 4000kbit ethernet nat dual-srchost",
		"qdisc replace dev ifb-usb0 root handle 1ca: cake bandwidth 20000kbit ethernet nat dual-dsthost ingress besteffort",
		"qdisc add dev usb0 handle ffff: ingress",
		"filter add dev usb0 parent ffff: protocol all prio 1 u32 match u32 0 0 action mirred egress redirect dev ifb-usb0",
	})

	// Unplugged and plugged in again between two passes: the name is
	// there, the queues are not.
	k.unplug("usb0")
	k.plug("usb0")
	m.Apply([]Source{phone})
	if got := k.took(); len(got) != 4 || k.redirect["usb0"] != "ifb-usb0" || k.cake["usb0"] == "" {
		t.Errorf("replugged between passes: %v", got)
	}

	// Nothing shaped any more: everything goes, and then the kernel is
	// left alone.
	m.Apply(nil)
	expect(t, "nothing shaped any more", k.took(), []string{"qdisc del dev usb0 ingress", "qdisc del dev usb0 root", "link del ifb-usb0"})
	if len(k.cake)+len(k.redirect)+len(k.ifb) != 0 {
		t.Errorf("left behind: %v %v %v", k.cake, k.redirect, k.ifb)
	}
	lists := k.lists
	m.Apply(nil)
	m.Apply(nil)
	if k.lists != lists+1 {
		t.Errorf("a router that shapes nothing read the queues %d times more; once is enough", k.lists-lists)
	}
}

// What is missing is put back: a queue someone removed, a stand-in
// interface that is gone or down.
func TestRepair(t *testing.T) {
	k := newKernel(t, "eth0")
	m := manager(k)
	m.Apply([]Source{wired})
	k.took()

	delete(k.cake, "eth0")
	m.Apply([]Source{wired})
	if got := k.took(); k.cake["eth0"] == "" || k.redirect["eth0"] != "ifb-eth0" {
		t.Errorf("a queue that was removed is not back: %v", got)
	}

	// The stand-in interface gone: the redirect that led to it leads
	// nowhere, and is made again once the interface is.
	k.unplug("ifb-eth0")
	delete(k.ifb, "ifb-eth0")
	m.Apply([]Source{wired})
	expect(t, "the stand-in interface was gone", k.took(), []string{
		"qdisc replace dev eth0 root handle 1ca: cake bandwidth 19000kbit ethernet nat dual-srchost",
		"qdisc replace dev ifb-eth0 root handle 1ca: cake bandwidth 95000kbit ethernet nat dual-dsthost ingress besteffort",
		"qdisc del dev eth0 ingress",
		"qdisc add dev eth0 handle ffff: ingress",
		"filter add dev eth0 parent ffff: protocol all prio 1 u32 match u32 0 0 action mirred egress redirect dev ifb-eth0",
	})

	k.ifb["ifb-eth0"] = false // taken down
	m.Apply([]Source{wired})
	if got := k.took(); !k.ifb["ifb-eth0"] || len(got) != 5 {
		t.Errorf("a stand-in interface that was down: up=%v, %v", k.ifb["ifb-eth0"], got)
	}
	m.Apply([]Source{wired})
	expect(t, "all in place again", k.took(), nil)
}

// A daemon that starts finds what the one before it left: it takes over
// what is still wanted and removes the rest, a redirect always before
// the interface it leads to.
func TestTakesOver(t *testing.T) {
	k := newKernel(t, "eth0", "usb0", "eth1")
	before := manager(k)
	phone := Source{Key: "phone", If: "usb0", UpKbps: 4000, DownKbps: 20000}
	before.Apply([]Source{wired, phone})
	// A queue of somebody else's is nobody's to remove.
	k.redirect["eth1"] = ""
	k.took()

	m := manager(k)
	slower := wired
	slower.DownKbps = 50000
	m.Apply([]Source{slower})
	expect(t, "taking over", k.took(), []string{
		"qdisc del dev usb0 ingress", "qdisc del dev usb0 root", "link del ifb-usb0",
		"qdisc replace dev eth0 root handle 1ca: cake bandwidth 19000kbit ethernet nat dual-srchost",
		"qdisc replace dev ifb-eth0 root handle 1ca: cake bandwidth 50000kbit ethernet nat dual-dsthost ingress besteffort",
		"qdisc del dev eth0 ingress",
		"qdisc add dev eth0 handle ffff: ingress",
		"filter add dev eth0 parent ffff: protocol all prio 1 u32 match u32 0 0 action mirred egress redirect dev ifb-eth0",
	})
	if _, there := k.redirect["eth1"]; !there {
		t.Error("an ingress queue that is not the router's was removed")
	}
	if got := states(m); got != "eth:eth0=active" {
		t.Errorf("after taking over: %s", got)
	}

	// And with nothing wanted at all, a daemon that starts clears up.
	fresh := manager(k)
	fresh.Apply(nil)
	expect(t, "nothing wanted after a restart", k.took(), []string{"qdisc del dev eth0 ingress", "qdisc del dev eth0 root", "link del ifb-eth0"})
}

// A kernel that cannot do it: the source says so, nothing is left half
// done, and it works once it can.
func TestRefused(t *testing.T) {
	k := newKernel(t, "eth0")
	m := manager(k)
	k.refuse = " cake "
	m.Apply([]Source{wired})
	st := m.Status()[0]
	if st.State != StError || !strings.Contains(st.Error, "Specified qdisc kind is unknown") {
		t.Errorf("status: %+v", st)
	}
	if len(k.cake)+len(k.redirect) != 0 {
		t.Errorf("left half done: %v %v", k.cake, k.redirect)
	}

	// The redirect refused, with the queues in place: all of it is taken
	// back, so that nothing is shaped one way only without saying so.
	k.refuse = "filter add"
	m.Apply([]Source{wired})
	if st := m.Status()[0]; st.State != StError || len(k.cake["eth0"]) != 0 {
		t.Errorf("after a refused redirect: %+v, queues %v", st, k.cake)
	}
	if _, there := k.redirect["eth0"]; there {
		t.Error("an ingress queue without its redirect was left")
	}

	k.refuse = ""
	m.Apply([]Source{wired})
	if st := m.Status()[0]; st.State != StActive || st.Error != "" || k.redirect["eth0"] != "ifb-eth0" {
		t.Errorf("once it works: %+v", st)
	}
}

// When tc's list cannot be read, what this run put in place is taken to
// be there: it is neither set up again on every pass nor reported as
// broken.
func TestUnreadableList(t *testing.T) {
	k := newKernel(t, "eth0")
	k.garbled = true
	m := manager(k)
	m.Apply([]Source{wired})
	if got := k.took(); len(got) != 4 || states(m) != "eth:eth0=active" {
		t.Errorf("first pass: %s, %v", states(m), got)
	}
	m.Apply([]Source{wired})
	expect(t, "second pass", k.took(), nil)
	if states(m) != "eth:eth0=active" {
		t.Errorf("second pass: %s", states(m))
	}
	// An interface that was unplugged and plugged in again is told by its
	// index, the list or no list.
	k.unplug("eth0")
	k.plug("eth0")
	m.Apply([]Source{wired})
	if got := k.took(); len(got) != 4 || k.cake["eth0"] == "" || k.redirect["eth0"] != "ifb-eth0" {
		t.Errorf("replugged, with the list unreadable: %v", got)
	}
	// Taking away works without the list too, for what this run made,
	// and for nothing else: a stand-in interface left by an earlier run
	// may still have a redirect leading to it that cannot be seen.
	k.plug("eth1")
	k.plug("ifb-eth1")
	k.ifb["ifb-eth1"], k.redirect["eth1"] = true, "ifb-eth1"
	m.Apply(nil)
	expect(t, "switched off", k.took(), []string{"qdisc del dev eth0 ingress", "qdisc del dev eth0 root", "link del ifb-eth0"})
	k.garbled = false
	m.Apply(nil)
	expect(t, "readable again", k.took(), []string{"qdisc del dev eth1 ingress", "link del ifb-eth1"})
}
