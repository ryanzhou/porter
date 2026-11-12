// Package mdns relays multicast DNS between the router's own networks, so
// that a phone on one finds the printer or the television on another.
//
// Multicast DNS (what Bonjour, AirPlay, AirPrint, Chromecast and HomeKit
// find each other with) is sent to a group address that never leaves the
// network it was sent on. The relay listens to that group on the bridge
// of every network that takes part and sends what it hears on one out
// again on the others it is paired with, from the router's address there:
// a reflector. It does not answer questions itself and keeps no records.
//
// Only what was sent to the group, from the mDNS port, is repeated. A
// question from any other port (a "legacy" one-off lookup) expects its
// answer sent back to that port alone, which would take remembering who
// asked on the other network; those are not relayed.
package mdns

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"log/slog"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// Port is the UDP port multicast DNS uses, for questions and answers alike.
const Port = 5353

// The groups multicast DNS is sent to.
var (
	Group4 = netip.MustParseAddr("224.0.0.251")
	Group6 = netip.MustParseAddr("ff02::fb")
)

const (
	// maxPacket is more than any multicast DNS message is allowed to be.
	maxPacket = 9000
	// A message is repeated once in dupWindow, wherever it is heard again:
	// that is what stops two relays on the same pair of networks (or a
	// cable that joins them) from handing a message back and forth for
	// ever. Devices repeat themselves a second apart at the closest, apart
	// from the three probes for a new name, of which one is enough.
	dupWindow = 500 * time.Millisecond
	// What is repeated from one network: perSecond messages, after a
	// burst of burst. A home's worth of devices stays far below it; a
	// device gone wrong, or a storm the duplicate check does not see,
	// stays on its own network.
	burst     = 100
	perSecond = 50
)

// Net is one network that takes part: its bridge, and the bridges of the
// networks that what is heard on it is repeated on.
type Net struct {
	If    string
	Peers []string
}

// conn is the socket for one address family, bound to the mDNS port.
type conn interface {
	// Join and Leave say on which interfaces the group is listened to.
	Join(ifindex int) error
	Leave(ifindex int) error
	// Read returns the next datagram that was sent to the group: its
	// size, the interface it arrived on and who sent it.
	Read(buf []byte) (n, ifindex int, src netip.AddrPort, err error)
	// Write sends a datagram to the group out of an interface, from the
	// router's address there.
	Write(pkt []byte, ifindex int) error
	Close() error
}

// Relay is the reflector. The zero value with a Log is ready; Apply starts
// and stops it as the networks change.
type Relay struct {
	Log *slog.Logger

	// What the system provides, replaced in tests: a socket for a family,
	// an interface's index and addresses, and the time.
	open   func(v6 bool) (conn, error)
	lookup func(name string) (index int, addrs []netip.Addr, err error)
	now    func() time.Time

	mu     sync.Mutex
	conns  [2]conn         // IPv4, IPv6
	joined [2]map[int]bool // interfaces the group is joined on, per family
	failed [2]bool         // a socket could not be opened; said once
	// peers holds, by the index of the interface a message was heard on,
	// the interfaces it is repeated on; own the router's addresses on all
	// of them, which nothing is relayed from.
	peers map[int][]int
	own   map[netip.Addr]bool
	// seen is when each message last repeated was first heard, by family
	// and a hash of it; buckets is how much more each network may send.
	seed    maphash.Seed
	seen    map[seenKey]time.Time
	swept   time.Time
	buckets map[int]*bucket
	// limited counts what was not repeated for being too much, since it
	// was last said.
	limited     int
	limitedSaid time.Time
	topology    string
}

type seenKey struct {
	v6   bool
	hash uint64
}

// bucket is a token bucket: one token per message repeated.
type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) take(now time.Time) bool {
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*perSecond)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *Relay) init() {
	if r.seen != nil {
		return
	}
	r.seed, r.seen, r.buckets = maphash.MakeSeed(), map[seenKey]time.Time{}, map[int]*bucket{}
	if r.open == nil {
		r.open = openSocket
	}
	if r.lookup == nil {
		r.lookup = lookupInterface
	}
	if r.now == nil {
		r.now = time.Now
	}
}

// Apply makes the relay run between exactly the given networks, over IPv4
// and, with v6, over IPv6 as well; with none it stops. It is called on
// every pass of the control loop and changes only what differs: a bridge
// that is not there yet, or has been made anew, is taken up when it is.
func (r *Relay) Apply(nets []Net, v6 bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()

	index := map[string]int{}
	own := map[netip.Addr]bool{}
	for _, n := range nets {
		idx, addrs, err := r.lookup(n.If)
		if err != nil {
			continue
		}
		index[n.If] = idx
		for _, a := range addrs {
			own[a.Unmap().WithZone("")] = true
		}
	}
	peers := map[int][]int{}
	var pairs []string
	for _, n := range nets {
		from, ok := index[n.If]
		if !ok {
			continue
		}
		for _, p := range n.Peers {
			if to, ok := index[p]; ok && to != from {
				peers[from] = append(peers[from], to)
				if n.If < p {
					pairs = append(pairs, n.If+" and "+p)
				}
			}
		}
	}
	r.peers, r.own = peers, own
	for idx := range r.buckets {
		if _, ok := peers[idx]; !ok {
			delete(r.buckets, idx)
		}
	}
	r.sync(0, len(peers) > 0)
	r.sync(1, len(peers) > 0 && v6)

	sort.Strings(pairs)
	topology := ""
	if len(pairs) > 0 {
		topology = fmt.Sprint(pairs, v6)
	}
	if topology != r.topology {
		r.topology = topology
		if len(pairs) > 0 {
			r.Log.Info("multicast DNS relayed", "between", pairs, "ipv6", v6)
		} else {
			r.Log.Info("multicast DNS no longer relayed")
		}
	}
	if now := r.now(); r.limited > 0 && now.Sub(r.limitedSaid) >= time.Minute {
		r.Log.Warn("more multicast DNS than is relayed; the rest was left on its own network", "messages", r.limited)
		r.limited, r.limitedSaid = 0, now
	}
}

// sync opens or closes one family's socket and brings the interfaces it
// listens on in line with r.peers. Called with the lock held.
func (r *Relay) sync(family int, want bool) {
	c := r.conns[family]
	if !want {
		if c != nil {
			// Its reader ends with it.
			c.Close()
			r.conns[family], r.joined[family] = nil, nil
		}
		return
	}
	if c == nil {
		var err error
		if c, err = r.open(family == 1); err != nil {
			// Tried again on every pass; said once.
			if !r.failed[family] {
				r.Log.Warn("listening for multicast DNS", "ipv6", family == 1, "err", err)
			}
			r.failed[family] = true
			return
		}
		r.conns[family], r.joined[family], r.failed[family] = c, map[int]bool{}, false
		go r.read(family, c)
	}
	joined := r.joined[family]
	for idx := range joined {
		if _, ok := r.peers[idx]; !ok {
			// A bridge that has gone has left the group by itself.
			_ = c.Leave(idx)
			delete(joined, idx)
		}
	}
	for idx := range r.peers {
		if joined[idx] {
			continue
		}
		if err := c.Join(idx); err != nil {
			r.Log.Debug("joining the multicast DNS group", "ifindex", idx, "ipv6", family == 1, "err", err)
			continue
		}
		joined[idx] = true
	}
}

// Stop ends the relay.
func (r *Relay) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers = nil
	for family, c := range r.conns {
		if c != nil {
			c.Close()
			r.conns[family], r.joined[family] = nil, nil
		}
	}
}

// read repeats what arrives on one socket until the socket is closed.
func (r *Relay) read(family int, c conn) {
	buf := make([]byte, maxPacket)
	for {
		n, in, src, err := c.Read(buf)
		if err != nil {
			r.mu.Lock()
			if r.conns[family] == c {
				// Not closed from here: the socket has failed. The next
				// pass opens another.
				r.Log.Warn("multicast DNS socket", "ipv6", family == 1, "err", err)
				c.Close()
				r.conns[family], r.joined[family] = nil, nil
			}
			r.mu.Unlock()
			return
		}
		out, to := r.route(family == 1, in, src, buf[:n])
		for _, idx := range to {
			if err := c.Write(out, idx); err != nil {
				r.Log.Debug("repeating multicast DNS", "ifindex", idx, "err", err)
			}
		}
	}
}

// route decides what becomes of one message heard on an interface: the
// message to send and the interfaces to send it out of, or nothing.
func (r *Relay) route(v6 bool, in int, src netip.AddrPort, pkt []byte) (out []byte, to []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	peers := r.peers[in]
	if len(peers) == 0 {
		// Not one of the networks relayed between: an uplink, or a
		// network that has just been taken out.
		return nil, nil
	}
	// What the router sent itself is not heard again (the sockets do not
	// loop their own messages back), and should it be, by way of a cable
	// that joins two networks, it is known by its address.
	if r.own[src.Addr().Unmap().WithZone("")] || src.Port() != Port {
		return nil, nil
	}
	out, ok := prepare(pkt)
	if !ok {
		return nil, nil
	}
	now := r.now()
	key := seenKey{v6, maphash.Bytes(r.seed, pkt)}
	if first, dup := r.seen[key]; dup && now.Sub(first) < dupWindow {
		return nil, nil
	}
	b := r.buckets[in]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		r.buckets[in] = b
	}
	if !b.take(now) {
		r.limited++
		return nil, nil
	}
	// Only what is repeated is remembered, so the rate limit is also the
	// limit on how much there is to remember.
	if now.Sub(r.swept) >= dupWindow {
		r.swept = now
		for k, first := range r.seen {
			if now.Sub(first) >= dupWindow {
				delete(r.seen, k)
			}
		}
	}
	r.seen[key] = now
	return out, append([]int(nil), peers...)
}

// prepare returns a message as it is to be repeated, or false for
// something that is not a DNS message. Answers go out as they came. A
// question may ask for its answer to be sent to the asker alone (the top
// bit of the question's class); repeated, the asker is the router, and
// the answer would end there. The bit is cleared, so that the answer goes
// to the group and is relayed back like any other.
func prepare(pkt []byte) ([]byte, bool) {
	const header = 12
	if len(pkt) < header {
		return nil, false
	}
	out := append([]byte(nil), pkt...)
	if out[2]&0x80 != 0 {
		return out, true
	}
	off := header
	for q := int(binary.BigEndian.Uint16(out[4:6])); q > 0; q-- {
		var ok bool
		if off, ok = skipName(out, off); !ok || off+4 > len(out) {
			return nil, false
		}
		out[off+2] &^= 0x80
		off += 4
	}
	return out, true
}

// skipName returns the offset just past the domain name at off.
func skipName(msg []byte, off int) (int, bool) {
	for off < len(msg) {
		switch l := int(msg[off]); {
		case l == 0:
			return off + 1, true
		case l&0xc0 == 0xc0:
			// A pointer to the rest of the name ends it here.
			return off + 2, off+2 <= len(msg)
		case l&0xc0 != 0:
			return 0, false
		default:
			off += 1 + l
		}
	}
	return 0, false
}
