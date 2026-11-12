package mdns

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeConn stands in for one family's socket: the test hands it datagrams
// as if they had arrived on an interface, and it records what the relay
// sends out of which.
type fakeConn struct {
	mu     sync.Mutex
	joined map[int]bool
	sent   []sent
	in     chan arrival
	closed chan struct{}
}

type sent struct {
	ifindex int
	pkt     []byte
}

type arrival struct {
	ifindex int
	src     netip.AddrPort
	pkt     []byte
}

func newFakeConn() *fakeConn {
	return &fakeConn{joined: map[int]bool{}, in: make(chan arrival), closed: make(chan struct{})}
}

func (c *fakeConn) Join(i int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.joined[i] = true
	return nil
}

func (c *fakeConn) Leave(i int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.joined, i)
	return nil
}

func (c *fakeConn) Read(buf []byte) (int, int, netip.AddrPort, error) {
	select {
	case a := <-c.in:
		return copy(buf, a.pkt), a.ifindex, a.src, nil
	case <-c.closed:
		return 0, 0, netip.AddrPort{}, net.ErrClosed
	}
}

func (c *fakeConn) Write(pkt []byte, i int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, sent{i, append([]byte(nil), pkt...)})
	return nil
}

func (c *fakeConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// take returns what has been sent since the last call, by interface.
func (c *fakeConn) take() map[int][][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[int][][]byte{}
	for _, s := range c.sent {
		out[s.ifindex] = append(out[s.ifindex], s.pkt)
	}
	c.sent = nil
	return out
}

func (c *fakeConn) listening() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []int
	for i := range c.joined {
		out = append(out, i)
	}
	sort.Ints(out)
	return fmt.Sprint(out)
}

// The test's networks: three bridges, and where the router is on each.
const (
	lan, iot, guest = 11, 12, 13
)

var (
	routerOn = map[string][]netip.Addr{
		"br-lan":   {netip.MustParseAddr("10.83.0.1"), netip.MustParseAddr("fe80::1"), netip.MustParseAddr("fd00:0:0:1::1"), netip.MustParseAddr("2001:db8:1000:3::1")},
		"br-iot":   {netip.MustParseAddr("10.84.0.1"), netip.MustParseAddr("fe80::2"), netip.MustParseAddr("fd00:0:0:2::1")},
		"br-guest": {netip.MustParseAddr("10.85.0.1"), netip.MustParseAddr("fe80::3")},
	}
	indexOf = map[string]int{"br-lan": lan, "br-iot": iot, "br-guest": guest}
	phone   = netip.MustParseAddrPort("10.83.0.50:5353")
	printer = netip.MustParseAddrPort("10.84.0.60:5353")
	visitor = netip.MustParseAddrPort("10.85.0.70:5353")
)

type rig struct {
	*Relay
	conns  [2]*fakeConn
	opened [2]int
	clock  time.Time
}

func newRig() *rig {
	g := &rig{clock: time.Unix(1_800_000_000, 0)}
	g.Relay = &Relay{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		open: func(v6 bool) (conn, error) {
			family := 0
			if v6 {
				family = 1
			}
			g.opened[family]++
			g.conns[family] = newFakeConn()
			return g.conns[family], nil
		},
		lookup: func(name string) (int, []netip.Addr, error) {
			idx, ok := indexOf[name]
			if !ok {
				return 0, nil, errors.New("no such interface")
			}
			return idx, routerOn[name], nil
		},
		now: func() time.Time { return g.clock },
	}
	return g
}

// hear has a datagram arrive and returns what the relay then sent, by
// interface. The reader takes one datagram at a time, so once a second
// one has been handed over, the first has been dealt with.
func (g *rig) hear(family, ifindex int, src netip.AddrPort, pkt []byte) map[int][][]byte {
	c := g.conns[family]
	c.in <- arrival{ifindex, src, pkt}
	c.in <- arrival{0, netip.AddrPort{}, nil} // from no network: ignored
	return c.take()
}

func question(name string, unicast bool) []byte {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypePTR)
	m.Id, m.RecursionDesired = 0, false
	if unicast {
		m.Question[0].Qclass |= 0x8000
	}
	b, err := m.Pack()
	if err != nil {
		panic(err)
	}
	return b
}

func answer(name, target string) []byte {
	m := new(dns.Msg)
	m.Response, m.Authoritative = true, true
	m.Answer = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: dns.Fqdn(name), Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 4500}, Ptr: dns.Fqdn(target)}}
	b, err := m.Pack()
	if err != nil {
		panic(err)
	}
	return b
}

// both is the usual set-up: the things network lets itself be found, the
// guest network is kept apart and has not asked for anything.
var both = []Net{{If: "br-lan", Peers: []string{"br-iot"}}, {If: "br-iot", Peers: []string{"br-lan"}}}

func TestRelay(t *testing.T) {
	g := newRig()
	g.Apply(both, false)
	defer g.Stop()
	if g.conns[0] == nil || g.conns[1] != nil {
		t.Fatalf("sockets opened: %v", g.opened)
	}
	if got := g.conns[0].listening(); got != "[11 12]" {
		t.Errorf("listening on %s, want the two networks relayed between", got)
	}

	// A question from the main network goes to the things network, and
	// nowhere else: not back where it came from, not to the guests.
	q := question("_ipp._tcp.local", false)
	got := g.hear(0, lan, phone, q)
	if len(got) != 1 || len(got[iot]) != 1 || !bytes.Equal(got[iot][0], q) {
		t.Fatalf("a question from the main network was sent on: %v", got)
	}
	// The answer goes back the other way, as it is.
	a := answer("_ipp._tcp.local", "Printer._ipp._tcp.local")
	got = g.hear(0, iot, printer, a)
	if len(got) != 1 || len(got[lan]) != 1 || !bytes.Equal(got[lan][0], a) {
		t.Fatalf("an answer from the things network was sent on: %v", got)
	}
	// What is said on a network that takes no part stays there.
	if got := g.hear(0, guest, visitor, question("_airplay._tcp.local", false)); len(got) != 0 {
		t.Errorf("relayed from a network that takes no part: %v", got)
	}
	// So does what arrives on something that is no network of the
	// router's at all (an internet source).
	if got := g.hear(0, 99, netip.MustParseAddrPort("100.64.0.9:5353"), question("_ssh._tcp.local", false)); len(got) != 0 {
		t.Errorf("relayed from an interface that is not a network: %v", got)
	}
}

// Nothing goes round: not what the router sent itself, and not a message
// that comes back by another way.
func TestRelayDoesNotLoop(t *testing.T) {
	g := newRig()
	g.Apply(both, false)
	defer g.Stop()

	// From the router's own address on either network.
	for ifindex, name := range map[int]string{lan: "br-lan", iot: "br-iot"} {
		for _, own := range routerOn[name] {
			if !own.Is4() {
				continue
			}
			if got := g.hear(0, ifindex, netip.AddrPortFrom(own, Port), question("_own._tcp.local", false)); len(got) != 0 {
				t.Errorf("the router relayed what came from its own address %s: %v", own, got)
			}
		}
	}
	// Its address on one network, heard on the other (a cable joining
	// the two): no more relayed than at home.
	if got := g.hear(0, lan, netip.MustParseAddrPort("10.84.0.1:5353"), question("_crossed._tcp.local", false)); len(got) != 0 {
		t.Errorf("relayed what the router itself sent on the other network: %v", got)
	}

	// A second relay between the same two networks repeats on the things
	// network what the phone asked on the main one. Here that is the
	// same message again, and it stops: sent back, the two relays would
	// pass it to each other for ever.
	q := question("_hap._tcp.local", false)
	if got := g.hear(0, lan, phone, q); len(got[iot]) != 1 {
		t.Fatalf("first hearing: %v", got)
	}
	other := netip.MustParseAddrPort("10.84.0.2:5353")
	if got := g.hear(0, iot, other, q); len(got) != 0 {
		t.Errorf("a message heard a second time, from the other side, was relayed again: %v", got)
	}
	if got := g.hear(0, lan, netip.MustParseAddrPort("10.83.0.2:5353"), q); len(got) != 0 {
		t.Errorf("a message that came back to where it started was relayed again: %v", got)
	}
	// The device asking again a second later is another matter.
	g.clock = g.clock.Add(time.Second)
	if got := g.hear(0, lan, phone, q); len(got[iot]) != 1 {
		t.Errorf("the same question a second later: %v", got)
	}
	// The same bytes over IPv6 are a message of their own.
	g.Apply(both, true)
	if got := g.hear(1, lan, netip.MustParseAddrPort("[fe80::50]:5353"), q); len(got[iot]) != 1 {
		t.Errorf("the question over IPv6 right after it went over IPv4: %v", got)
	}
}

// A flood on one network is cut off, and the others are not made to wait
// for it.
func TestRelayLimitsRate(t *testing.T) {
	g := newRig()
	g.Apply(both, false)
	defer g.Stop()

	relayed := 0
	for i := 0; i < 5*burst; i++ {
		relayed += len(g.hear(0, iot, printer, question(fmt.Sprintf("_flood%d._tcp.local", i), false))[lan])
	}
	if relayed != burst {
		t.Errorf("%d of %d messages sent in one instant were relayed, want %d", relayed, 5*burst, burst)
	}
	if got := g.hear(0, lan, phone, question("_quiet._tcp.local", false)); len(got[iot]) != 1 {
		t.Errorf("the other network's question during the flood: %v", got)
	}
	// It recovers at its rate.
	g.clock = g.clock.Add(time.Second)
	relayed = 0
	for i := 0; i < 5*burst; i++ {
		relayed += len(g.hear(0, iot, printer, question(fmt.Sprintf("_again%d._tcp.local", i), false))[lan])
	}
	if relayed != perSecond {
		t.Errorf("%d messages relayed in the second after a flood, want %d", relayed, perSecond)
	}
	// What is remembered to tell duplicates by is bounded by the same.
	g.mu.Lock()
	remembered := len(g.seen)
	g.mu.Unlock()
	if remembered > 2*(burst+perSecond) {
		t.Errorf("%d messages remembered", remembered)
	}
}

func TestRelayIPv6(t *testing.T) {
	g := newRig()
	g.Apply(both, true)
	defer g.Stop()
	if g.conns[0] == nil || g.conns[1] == nil {
		t.Fatalf("sockets opened: %v", g.opened)
	}
	q := question("_airplay._tcp.local", false)
	if got := g.hear(1, lan, netip.MustParseAddrPort("[fe80::50%br-lan]:5353"), q); len(got) != 1 || len(got[iot]) != 1 {
		t.Errorf("an IPv6 question from the main network: %v", got)
	}
	if got := g.conns[0].take(); len(got) != 0 {
		t.Errorf("an IPv6 question was sent on over IPv4: %v", got)
	}
	// Whichever of its addresses the router's own message carries (the
	// link-local one, the private one, one out of a delegated prefix),
	// with or without the link named, it is the router's.
	for _, own := range []string{"fe80::1%br-lan", "fe80::1", "fd00:0:0:1::1", "2001:db8:1000:3::1", "fe80::2%11"} {
		src := netip.AddrPortFrom(netip.MustParseAddr(own), Port)
		if got := g.hear(1, lan, src, question("_own6._tcp.local", false)); len(got) != 0 {
			t.Errorf("the router relayed what came from its own address %s: %v", own, got)
		}
	}
	// IPv6 switched off for the networks: the socket goes, IPv4 stays.
	v6 := g.conns[1]
	g.Apply(both, false)
	select {
	case <-v6.closed:
	default:
		t.Error("the IPv6 socket was left open")
	}
	if g.opened != [2]int{1, 1} {
		t.Errorf("sockets opened: %v", g.opened)
	}
}

// The networks change under the relay: it follows, opening nothing anew
// that it has.
func TestRelayFollowsNetworks(t *testing.T) {
	g := newRig()
	g.Apply(nil, true)
	if g.conns[0] != nil || g.conns[1] != nil {
		t.Fatal("sockets opened with nothing to relay")
	}
	g.Apply(both, false)
	first := g.conns[0]
	g.Apply(both, false)
	if g.opened[0] != 1 {
		t.Errorf("the socket was opened %d times", g.opened[0])
	}
	// A third network joins in, paired with the main one only.
	three := []Net{{If: "br-lan", Peers: []string{"br-iot", "br-guest"}}, {If: "br-iot", Peers: []string{"br-lan"}}, {If: "br-guest", Peers: []string{"br-lan"}}}
	g.Apply(three, false)
	if got := first.listening(); got != "[11 12 13]" {
		t.Errorf("listening on %s", got)
	}
	got := g.hear(0, lan, phone, question("_a._tcp.local", false))
	if len(got[iot]) != 1 || len(got[guest]) != 1 {
		t.Errorf("from the main network with two others: %v", got)
	}
	// Between the two others there is no pairing: each hears the main
	// network and is heard there, and that is all.
	if got := g.hear(0, guest, visitor, question("_b._tcp.local", false)); len(got) != 1 || len(got[lan]) != 1 {
		t.Errorf("from the guest network: %v", got)
	}
	// A bridge that is not there yet is left out until it is.
	delete(indexOf, "br-guest")
	g.Apply(three, false)
	indexOf["br-guest"] = guest
	if got := first.listening(); got != "[11 12]" {
		t.Errorf("listening on %s with one bridge missing", got)
	}
	if got := g.hear(0, lan, phone, question("_c._tcp.local", false)); len(got) != 1 || len(got[iot]) != 1 {
		t.Errorf("with one bridge missing: %v", got)
	}
	// A bridge made anew has another index: the old one is let go.
	indexOf["br-iot"] = 42
	g.Apply(both, false)
	indexOf["br-iot"] = iot
	if got := first.listening(); got != "[11 42]" {
		t.Errorf("listening on %s after a bridge was made anew", got)
	}
	if got := g.hear(0, 42, printer, answer("_c._tcp.local", "x._c._tcp.local")); len(got[lan]) != 1 {
		t.Errorf("from the bridge made anew: %v", got)
	}
	// Nothing left to relay: everything is closed, and what still
	// arrives goes nowhere.
	g.Apply(nil, false)
	select {
	case <-first.closed:
	default:
		t.Error("the socket was left open with nothing to relay")
	}
	if out, to := g.route(false, lan, phone, question("_d._tcp.local", false)); out != nil || to != nil {
		t.Error("relayed after the relay was stopped")
	}
	// And started again, with a socket of its own.
	g.Apply(both, false)
	if g.conns[0] == first || g.opened[0] != 2 {
		t.Error("the relay did not start again")
	}
	g.Stop()
}

// A socket that cannot be opened is tried again on the next pass.
func TestRelayRetriesSocket(t *testing.T) {
	g := newRig()
	open := g.open
	fail := true
	g.open = func(v6 bool) (conn, error) {
		if fail {
			return nil, errors.New("address already in use")
		}
		return open(v6)
	}
	g.Apply(both, false)
	if g.conns[0] != nil {
		t.Fatal("a socket came from nowhere")
	}
	fail = false
	g.Apply(both, false)
	defer g.Stop()
	if g.conns[0] == nil || g.conns[0].listening() != "[11 12]" {
		t.Error("the relay did not recover once the port was free")
	}
}

func TestPrepare(t *testing.T) {
	// A question that wants its answer for itself is repeated as one
	// that does not: the answer has to come by the group to be relayed.
	in := question("_googlecast._tcp.local", true)
	out, ok := prepare(in)
	if !ok {
		t.Fatal("a question was refused")
	}
	var m dns.Msg
	if err := m.Unpack(out); err != nil || len(m.Question) != 1 || m.Question[0].Qclass != dns.ClassINET || m.Question[0].Name != "_googlecast._tcp.local." {
		t.Errorf("the question as repeated: %+v %v", m.Question, err)
	}
	if bytes.Equal(in, out) || in[len(in)-2]&0x80 == 0 {
		t.Error("the message was changed in place, or not at all")
	}
	// Several questions, the later ones with names that point back into
	// the first, and records after them that are left alone.
	multi := new(dns.Msg)
	multi.Compress = true
	multi.Question = []dns.Question{
		{Name: "_ipp._tcp.local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET | 0x8000},
		{Name: "_ipps._tcp.local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET | 0x8000},
		{Name: "local.", Qtype: dns.TypeSOA, Qclass: dns.ClassINET},
	}
	multi.Answer = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: "_ipp._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET | 0x8000, Ttl: 10}, Ptr: "known._ipp._tcp.local."}}
	packed, err := multi.Pack()
	if err != nil {
		t.Fatal(err)
	}
	out, ok = prepare(packed)
	if err := m.Unpack(out); !ok || err != nil || len(m.Question) != 3 || len(m.Answer) != 1 {
		t.Fatalf("several questions: %v %v %+v", ok, err, m)
	}
	for _, q := range m.Question {
		if q.Qclass != dns.ClassINET {
			t.Errorf("question %s repeated with class %#x", q.Name, q.Qclass)
		}
	}
	if m.Answer[0].Header().Class != dns.ClassINET|0x8000 {
		t.Error("a record after the questions was changed")
	}
	// An answer is repeated as it is, the same bit in its records (there
	// it means something else) included.
	a := answer("_ipp._tcp.local", "Printer._ipp._tcp.local")
	if out, ok := prepare(a); !ok || !bytes.Equal(out, a) {
		t.Error("an answer was changed")
	}
	// What is not a DNS message is not repeated, and nothing reads past
	// the end of one that breaks off.
	for i := 0; i < len(in); i++ {
		if _, ok := prepare(in[:i]); ok {
			t.Errorf("a question cut off after %d of %d bytes was accepted", i, len(in))
		}
	}
	for i := 12; i < len(packed)-len(packed)/3; i++ {
		prepare(packed[:i])
	}
	bad := append([]byte(nil), in...)
	bad[12] = 0x80 // a label type that does not exist
	if _, ok := prepare(bad); ok {
		t.Error("a name with an unknown kind of label was accepted")
	}
	if _, ok := prepare(nil); ok {
		t.Error("an empty datagram was accepted")
	}
}

// What the relay will not carry.
func TestRelayRefuses(t *testing.T) {
	g := newRig()
	g.Apply(both, false)
	defer g.Stop()
	// A question from another port than the mDNS one expects its answer
	// sent back there and nowhere else.
	if got := g.hear(0, lan, netip.MustParseAddrPort("10.83.0.50:49152"), question("_legacy._tcp.local", false)); len(got) != 0 {
		t.Errorf("a one-off question from another port was relayed: %v", got)
	}
	if got := g.hear(0, lan, phone, []byte("not DNS")); len(got) != 0 {
		t.Errorf("something that is no DNS message was relayed: %v", got)
	}
	// A question that asks for a private answer goes out without asking.
	got := g.hear(0, lan, phone, question("_raop._tcp.local", true))
	if len(got[iot]) != 1 || bytes.Equal(got[iot][0], question("_raop._tcp.local", true)) || !bytes.Equal(got[iot][0], question("_raop._tcp.local", false)) {
		t.Errorf("a question asking for a private answer: %v", got)
	}
}
