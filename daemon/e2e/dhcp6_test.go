//go:build e2e

package e2e

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A provider's DHCPv6 server, as far as a router asking for an address
// and a prefix needs one (RFC 8415): it hands every client the same
// address and delegates every client the same prefix, and routes the
// prefix to whoever it was delegated to, as a provider's router does.
// It is written out here rather than taken from a package so that the
// test depends on nothing the router's image does not have, and so that
// its log says exactly what was asked and answered.

// DHCPv6 message types and options.
const (
	d6Solicit   = 1
	d6Advertise = 2
	d6Request   = 3
	d6Confirm   = 4
	d6Renew     = 5
	d6Rebind    = 6
	d6Reply     = 7
	d6Release   = 8
	d6Decline   = 9
	d6InfoReq   = 11

	d6ClientID   = 1
	d6ServerID   = 2
	d6IANA       = 3
	d6IAAddr     = 5
	d6Preference = 7
	d6IAPD       = 25
	d6IAPrefix   = 26
)

// pdLease is what the server gives out.
type pdLease struct {
	Addr   netip.Addr   // for IA_NA
	Prefix netip.Prefix // for IA_PD
	// T1 and T2 are when the client is to renew and to rebind; Preferred
	// and Valid the lifetimes of the address and the prefix. In seconds.
	T1, T2, Preferred, Valid uint32
}

// The server's own identifier: a DUID of the link-layer type.
var pdServerID = []byte{0, 3, 0, 1, 0x02, 0x70, 0x64, 0x00, 0x00, 0x01}

func d6opt(code uint16, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	out := binary.BigEndian.AppendUint16(nil, code)
	out = binary.BigEndian.AppendUint16(out, uint16(len(body)))
	return append(out, body...)
}

func d6u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// d6options splits a run of options. ok is false when it does not parse.
func d6options(b []byte) (opts []struct {
	Code uint16
	Data []byte
}, ok bool) {
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, false
		}
		code, n := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+n {
			return nil, false
		}
		opts = append(opts, struct {
			Code uint16
			Data []byte
		}{code, b[4 : 4+n]})
		b = b[4+n:]
	}
	return opts, true
}

// answer makes the reply to one message from a client, or nil for a
// message that gets none. bound says that the reply commits the lease to
// the client; what describes the exchange for the log.
func (l pdLease) answer(req []byte) (reply []byte, bound bool, what string) {
	if len(req) < 4 {
		return nil, false, "too short to be a DHCPv6 message"
	}
	opts, ok := d6options(req[4:])
	if !ok {
		return nil, false, "options do not parse"
	}
	var client []byte
	var ianas, iapds [][]byte // the identifiers (IAIDs) asked about
	for _, o := range opts {
		switch o.Code {
		case d6ClientID:
			client = o.Data
		case d6ServerID:
			if !bytes.Equal(o.Data, pdServerID) {
				return nil, false, "meant for another server"
			}
		case d6IANA, d6IAPD:
			if len(o.Data) < 12 {
				return nil, false, "identity association too short"
			}
			if o.Code == d6IANA {
				ianas = append(ianas, o.Data[:4])
			} else {
				iapds = append(iapds, o.Data[:4])
			}
		}
	}
	if client == nil {
		return nil, false, "no client identifier"
	}
	names := map[byte]string{d6Solicit: "SOLICIT", d6Request: "REQUEST", d6Confirm: "CONFIRM", d6Renew: "RENEW",
		d6Rebind: "REBIND", d6Release: "RELEASE", d6Decline: "DECLINE", d6InfoReq: "INFORMATION-REQUEST"}
	name, known := names[req[0]]
	if !known {
		return nil, false, fmt.Sprintf("message type %d", req[0])
	}
	what = fmt.Sprintf("%s asking about %d address and %d prefix association(s)", name, len(ianas), len(iapds))
	out := []byte{d6Reply, req[1], req[2], req[3]}
	out = append(out, d6opt(d6ClientID, client)...)
	out = append(out, d6opt(d6ServerID, pdServerID)...)
	switch req[0] {
	case d6Solicit:
		// Always the two-step exchange, whatever the client offers: an
		// advertisement first, with the highest preference so that the
		// client does not wait for a better one.
		out[0] = d6Advertise
		out = append(out, d6opt(d6Preference, []byte{255})...)
	case d6Request, d6Renew, d6Rebind:
		bound = true
	default:
		// Nothing to hand out: the bare reply says the message was heard.
		return out, false, what
	}
	addr, prefix := l.Addr.As16(), l.Prefix.Masked().Addr().As16()
	for _, iaid := range ianas {
		out = append(out, d6opt(d6IANA, iaid, d6u32(l.T1), d6u32(l.T2),
			d6opt(d6IAAddr, addr[:], d6u32(l.Preferred), d6u32(l.Valid)))...)
	}
	for _, iaid := range iapds {
		out = append(out, d6opt(d6IAPD, iaid, d6u32(l.T1), d6u32(l.T2),
			d6opt(d6IAPrefix, d6u32(l.Preferred), d6u32(l.Valid), []byte{byte(l.Prefix.Bits())}, prefix[:]))...)
	}
	return out, bound, what
}

// pdServer is a running server.
type pdServer struct {
	conn  *net.UDPConn
	lease pdLease
	ns    string
	ifnam string
	log   *os.File
	// Bound counts the leases committed (REQUEST), Renewed the times one
	// was extended (RENEW, REBIND).
	Bound, Renewed atomic.Int32

	mu     sync.Mutex
	routed string // the client the prefix is routed to
}

// startPDServer starts the server on an interface inside a namespace. Its
// log goes to the named file.
func startPDServer(t *testing.T, ns, ifname string, lease pdLease, logPath string) *pdServer {
	t.Helper()
	s := &pdServer{lease: lease, ns: ns, ifnam: ifname}
	var err error
	if s.log, err = os.Create(logPath); err != nil {
		t.Fatal(err)
	}
	inNS(ns, func() {
		var ifi *net.Interface
		if ifi, err = net.InterfaceByName(ifname); err != nil {
			return
		}
		// All DHCPv6 servers and relays, on that link. Go binds the
		// socket to the port on every address and joins the group on the
		// interface.
		s.conn, err = net.ListenMulticastUDP("udp6", ifi, &net.UDPAddr{IP: net.ParseIP("ff02::1:2"), Port: 547})
	})
	if err != nil {
		t.Fatalf("DHCPv6 server on %s in %s: %v", ifname, ns, err)
	}
	fmt.Fprintf(s.log, "%s serving %s and %s on %s\n", time.Now().Format("15:04:05.000"), lease.Addr, lease.Prefix, ifname)
	go s.serve()
	return s
}

func (s *pdServer) serve() {
	buf := make([]byte, 2048)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		reply, bound, what := s.lease.answer(buf[:n])
		stamp := time.Now().Format("15:04:05.000")
		if reply == nil {
			fmt.Fprintf(s.log, "%s from %s: ignored (%s)\n", stamp, from, what)
			continue
		}
		if bound {
			// The prefix is the client's from here on: the way to it is
			// through the client.
			s.route(from)
			if buf[0] == d6Request {
				s.Bound.Add(1)
			} else {
				s.Renewed.Add(1)
			}
		}
		_, err = s.conn.WriteToUDP(reply, from)
		fmt.Fprintf(s.log, "%s from %s: %s; answered with message type %d, %d bytes (%v)\n", stamp, from, what, reply[0], len(reply), err)
	}
}

// route sends the delegated prefix the client's way.
func (s *pdServer) route(client *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	via := client.IP.String()
	if via == s.routed {
		return
	}
	out, err := exec.Command("ip", "-n", s.ns, "-6", "route", "replace", s.lease.Prefix.String(), "via", via, "dev", s.ifnam).CombinedOutput()
	fmt.Fprintf(s.log, "route %s via %s dev %s: %v %s\n", s.lease.Prefix, via, s.ifnam, err, strings.TrimSpace(string(out)))
	if err == nil {
		s.routed = via
	}
}

// Close stops the server and takes the route to the prefix away.
func (s *pdServer) Close() {
	s.conn.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.routed != "" {
		exec.Command("ip", "-n", s.ns, "-6", "route", "del", s.lease.Prefix.String(), "dev", s.ifnam).Run()
		s.routed = ""
	}
	s.log.Close()
}

// TestPDServerAnswers checks the server's answers by themselves: what the
// end-to-end test needs of them, and (where dhcpcd is installed) that
// dhcpcd reads out of a reply what the server put into it.
func TestPDServerAnswers(t *testing.T) {
	lease := pdLease{Addr: netip.MustParseAddr("2001:db8:100::d6"), Prefix: netip.MustParsePrefix("2001:db8:1000::/60"),
		T1: 20, T2: 32, Preferred: 120, Valid: 180}
	client := []byte{0, 1, 0, 1, 0x2a, 0x3b, 0x4c, 0x5d, 2, 0, 0, 0, 0, 1}
	message := func(typ byte, extra ...[]byte) []byte {
		m := []byte{typ, 0xab, 0xcd, 0xef}
		m = append(m, d6opt(d6ClientID, client)...)
		m = append(m, d6opt(8, []byte{0, 0})...)         // elapsed time
		m = append(m, d6opt(6, []byte{0, 23, 0, 24})...) // option request
		m = append(m, d6opt(14)...)                      // rapid commit, which the server does not take up
		m = append(m, d6opt(d6IANA, []byte{0xaa, 0xbb, 0xcc, 0xdd}, d6u32(0), d6u32(0))...)
		m = append(m, d6opt(d6IAPD, []byte{0, 0, 0, 1}, d6u32(0), d6u32(0))...)
		return append(m, bytes.Join(extra, nil)...)
	}
	has := func(msg []byte, code uint16) []byte {
		opts, ok := d6options(msg[4:])
		if !ok {
			t.Fatalf("the server's answer does not parse: %x", msg)
		}
		for _, o := range opts {
			if o.Code == code {
				return o.Data
			}
		}
		return nil
	}

	adv, bound, _ := lease.answer(message(d6Solicit))
	if adv == nil || adv[0] != d6Advertise || bound || !bytes.Equal(adv[1:4], []byte{0xab, 0xcd, 0xef}) {
		t.Fatalf("answer to a solicitation: %x (bound %v)", adv, bound)
	}
	if !bytes.Equal(has(adv, d6ClientID), client) || !bytes.Equal(has(adv, d6ServerID), pdServerID) || has(adv, d6IAPD) == nil || has(adv, d6IANA) == nil {
		t.Errorf("advertisement lacks an identifier or an association: %x", adv)
	}
	for _, typ := range []byte{d6Request, d6Renew, d6Rebind} {
		rep, bound, what := lease.answer(message(typ, d6opt(d6ServerID, pdServerID)))
		if rep == nil || rep[0] != d6Reply || !bound {
			t.Fatalf("answer to message type %d: %x (bound %v, %s)", typ, rep, bound, what)
		}
		pd := has(rep, d6IAPD)
		if len(pd) != 12+4+25 || !bytes.Equal(pd[:4], []byte{0, 0, 0, 1}) || binary.BigEndian.Uint32(pd[4:]) != 20 || binary.BigEndian.Uint32(pd[8:]) != 32 {
			t.Fatalf("prefix association in the reply: %x", pd)
		}
		prefix := pd[12+4:]
		want := lease.Prefix.Addr().As16()
		if binary.BigEndian.Uint32(prefix) != 120 || binary.BigEndian.Uint32(prefix[4:]) != 180 || prefix[8] != 60 || !bytes.Equal(prefix[9:], want[:]) {
			t.Errorf("delegated prefix in the reply: %x", prefix)
		}
		if na := has(rep, d6IANA); len(na) != 12+4+24 || !bytes.Equal(na[:4], []byte{0xaa, 0xbb, 0xcc, 0xdd}) {
			t.Errorf("address association in the reply: %x", na)
		}
	}
	// Not for this server, or not a message at all: no answer.
	if rep, _, _ := lease.answer(message(d6Request, d6opt(d6ServerID, []byte{0, 3, 0, 1, 9, 9, 9, 9, 9, 9}))); rep != nil {
		t.Errorf("answered a request meant for another server: %x", rep)
	}
	for _, junk := range [][]byte{nil, {1, 2}, {d6Solicit, 0, 0, 0}, {d6Solicit, 0, 0, 0, 0, 1, 0, 9, 1}, {d6Reply, 0, 0, 0, 0, 1, 0, 1, 7}} {
		if rep, _, _ := lease.answer(junk); rep != nil {
			t.Errorf("answered %x with %x", junk, rep)
		}
	}
	// A release is acknowledged and gives nothing out.
	if rep, bound, _ := lease.answer(message(d6Release, d6opt(d6ServerID, pdServerID))); rep == nil || rep[0] != d6Reply || bound || has(rep, d6IAPD) != nil {
		t.Errorf("answer to a release: %x (bound %v)", rep, bound)
	}

	// What dhcpcd makes of a reply, in the words it uses towards its
	// hook. It leaves without flushing what it printed unless every line
	// is written as it is made, hence stdbuf.
	for _, tool := range []string{"dhcpcd", "stdbuf"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: dhcpcd's reading of the reply is not checked", tool)
		}
	}
	reply, _, _ := lease.answer(message(d6Request, d6opt(d6ServerID, pdServerID)))
	cmd := exec.Command("stdbuf", "-oL", "dhcpcd", "-6", "-U", "-")
	cmd.Stdin = bytes.NewReader(reply)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		t.Skipf("this dhcpcd does not describe a lease given to it (%v)", err)
	}
	for _, want := range []string{
		"dhcp6_ia_pd1_prefix1=2001:db8:1000::", "dhcp6_ia_pd1_prefix1_length=60",
		"dhcp6_ia_pd1_prefix1_pltime=120", "dhcp6_ia_pd1_prefix1_vltime=180",
		"dhcp6_ia_pd1_t1=20", "dhcp6_ia_pd1_t2=32", "dhcp6_ia_na1_ia_addr1=2001:db8:100::d6",
	} {
		if !strings.Contains(string(out), want+"\n") {
			t.Errorf("dhcpcd does not read %q out of the server's reply:\n%s", want, out)
		}
	}
}
