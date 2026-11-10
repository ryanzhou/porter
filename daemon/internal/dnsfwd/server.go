package dnsfwd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// Hooks connect the forwarder to the rest of the router.
type Hooks struct {
	// LookupMAC maps a client address to its device, "" when unknown.
	LookupMAC func(netip.Addr) string
	// Learn installs resolved addresses for a domain rule before the
	// answer is released.
	Learn func(ruleID string, addrs []netip.Addr, ttl time.Duration) error
}

// Query is one entry of the recent-queries log.
type Query struct {
	Time   int64  `json:"t"`
	MAC    string `json:"mac"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Path   string `json:"path"` // "direct", "wan", "vpn:<id>", "block", "local", "refuse"
	Failed bool   `json:"failed,omitempty"`
}

type Server struct {
	hooks  Hooks
	policy atomic.Pointer[Policy]
	cache  *cache
	up     *upstreams
	log    *slog.Logger

	mu      sync.Mutex // guards servers
	servers []*dns.Server

	// The query log has its own lock: handlers append to it while a
	// shutdown (which holds mu and waits for handlers) is in progress.
	recMu  sync.Mutex
	recent []Query
	next   int

	// loopbackClients makes loopback sources subject to policy (tests).
	loopbackClients bool
}

const recentSize = 400

// learnTTL bounds how long a learned address stays in a rule's set.
// Clients keep using an address for a while after its TTL, so short TTLs
// are stretched; very long ones are capped so stale mappings fade.
const (
	learnMin = 10 * time.Minute
	learnMax = 24 * time.Hour
)

func New(hooks Hooks, log *slog.Logger) *Server {
	s := &Server{hooks: hooks, cache: newCache(4096), up: newUpstreams(), log: log}
	s.policy.Store(&Policy{})
	return s
}

// SetPolicy swaps the policy snapshot. Cached answers are dropped: they
// may have come from a resolver that is no longer the right one (most
// importantly a captive portal's).
func (s *Server) SetPolicy(p *Policy) {
	s.policy.Store(p)
	s.cache.flush()
}

// Listen starts serving on the given addresses (host:port), UDP and TCP.
// Addresses that are already served are kept; others are closed.
func (s *Server) Listen(addrs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	for _, a := range addrs {
		want[a] = true
	}
	var keep []*dns.Server
	have := map[string]int{}
	for _, srv := range s.servers {
		if want[srv.Addr] {
			keep = append(keep, srv)
			have[srv.Addr]++
		} else {
			go srv.Shutdown()
		}
	}
	s.servers = keep
	var firstErr error
	for _, a := range addrs {
		if have[a] > 0 {
			continue
		}
		for _, network := range []string{"udp", "tcp"} {
			started := make(chan error, 1)
			srv := &dns.Server{Addr: a, Net: network, Handler: s, ReusePort: true,
				NotifyStartedFunc: func() { started <- nil }}
			go func() {
				if err := srv.ListenAndServe(); err != nil {
					select {
					case started <- err:
					default:
					}
				}
			}()
			select {
			case err := <-started:
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
			case <-time.After(3 * time.Second):
				if firstErr == nil {
					firstErr = errors.New("dns listener start timed out: " + a)
				}
				continue
			}
			s.servers = append(s.servers, srv)
		}
	}
	return firstErr
}

func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.servers {
		srv.Shutdown()
	}
	s.servers = nil
}

// Recent returns the query log, oldest first, optionally for one device.
func (s *Server) Recent(mac string) []Query {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	out := make([]Query, 0, len(s.recent))
	for i := 0; i < len(s.recent); i++ {
		q := s.recent[(s.next+i)%len(s.recent)]
		if mac == "" || q.MAC == mac {
			out = append(out, q)
		}
	}
	return out
}

func (s *Server) record(q Query) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	if len(s.recent) < recentSize {
		s.recent = append(s.recent, q)
		return
	}
	s.recent[s.next] = q
	s.next = (s.next + 1) % recentSize
}

func clientAddr(w dns.ResponseWriter) netip.Addr {
	switch a := w.RemoteAddr().(type) {
	case *net.UDPAddr:
		ip, _ := netip.AddrFromSlice(a.IP)
		return ip.Unmap().WithZone("")
	case *net.TCPAddr:
		ip, _ := netip.AddrFromSlice(a.IP)
		return ip.Unmap().WithZone("")
	}
	return netip.Addr{}
}

// ServeDNS implements dns.Handler.
func (s *Server) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) != 1 || req.Opcode != dns.OpcodeQuery {
		reply(w, req, dns.RcodeNotImplemented)
		return
	}
	q := req.Question[0]
	if q.Qclass != dns.ClassINET {
		reply(w, req, dns.RcodeRefused)
		return
	}
	name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
	ip := clientAddr(w)
	mac := ""
	own := !ip.IsValid() || (ip.IsLoopback() && !s.loopbackClients)
	if s.hooks.LookupMAC != nil && !own {
		mac = s.hooks.LookupMAC(ip)
	}
	p := s.policy.Load()
	entry := Query{Time: time.Now().Unix(), MAC: mac, Name: name, Type: dns.TypeToString[q.Qtype]}
	defer func() { s.record(entry) }()

	if m := p.local(req, name, q.Qtype); m != nil {
		entry.Path = "local"
		w.WriteMsg(m)
		return
	}
	if p.Blocked.Match(name) {
		entry.Path = "block"
		reply(w, req, dns.RcodeNameError)
		return
	}
	var pt path
	if own {
		// The router's own lookups always leave directly.
		pt = path{kind: "direct", servers: p.Direct}
	} else {
		pt = p.decide(mac, ip, name)
	}
	entry.Path = pt.kind
	switch pt.kind {
	case "block":
		reply(w, req, dns.RcodeNameError)
		return
	case "refuse":
		reply(w, req, dns.RcodeRefused)
		return
	case "vpn":
		entry.Path = "vpn:" + pt.vpn
	}
	if len(pt.servers) == 0 {
		entry.Failed = true
		reply(w, req, dns.RcodeServerFailure)
		return
	}

	resp, err := s.resolve(req, pt)
	if err != nil {
		entry.Failed = true
		s.log.Debug("dns upstream failed", "name", name, "path", entry.Path, "err", err)
		reply(w, req, dns.RcodeServerFailure)
		return
	}

	// Teach the classifier before the client can act on the answer.
	if ids := p.matchingRules(name); len(ids) > 0 && s.hooks.Learn != nil {
		addrs, ttl := answers(resp)
		if len(addrs) > 0 {
			for _, id := range ids {
				if err := s.hooks.Learn(id, addrs, ttl); err != nil {
					s.log.Warn("learning rule addresses failed", "rule", id, "err", err)
				}
			}
		}
	}
	resp.Id = req.Id
	// Echo the question exactly as asked (the cache is case-insensitive,
	// clients using 0x20 randomisation are not).
	resp.Question = req.Question
	if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP {
		size := 512
		if o := req.IsEdns0(); o != nil && int(o.UDPSize()) > size {
			size = int(o.UDPSize())
		}
		resp.Truncate(size)
	}
	w.WriteMsg(resp)
}

func (s *Server) resolve(req *dns.Msg, pt path) (*dns.Msg, error) {
	q := req.Question[0]
	do := false
	if o := req.IsEdns0(); o != nil {
		do = o.Do()
	}
	key := cacheKey{group: groupKey(pt), name: strings.ToLower(q.Name), qtype: q.Qtype, do: do}
	if m := s.cache.get(key); m != nil {
		return m, nil
	}
	out := req.Copy()
	out.Id = dns.Id()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var lastErr error
	for _, srv := range pt.servers {
		resp, err := s.up.exchange(ctx, out, srv, pt.mark)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Rcode == dns.RcodeServerFailure || resp.Rcode == dns.RcodeRefused {
			lastErr = errors.New("upstream answered " + dns.RcodeToString[resp.Rcode])
			continue
		}
		s.cache.put(key, resp)
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream")
	}
	return nil, lastErr
}

func groupKey(pt path) string {
	var b strings.Builder
	b.WriteString(pt.kind)
	b.WriteString(pt.vpn)
	for _, s := range pt.servers {
		b.WriteByte('|')
		b.WriteString(s.Proto)
		b.WriteString(s.Addr.String())
	}
	return b.String()
}

// answers extracts the addresses in a response and the lifetime to give
// them in a rule's set.
func answers(m *dns.Msg) ([]netip.Addr, time.Duration) {
	var addrs []netip.Addr
	minTTL := uint32(0)
	first := true
	for _, rr := range m.Answer {
		var ip net.IP
		switch r := rr.(type) {
		case *dns.A:
			ip = r.A
		case *dns.AAAA:
			ip = r.AAAA
		default:
			continue
		}
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		a = a.Unmap()
		if a.IsUnspecified() || a.IsLoopback() {
			continue
		}
		addrs = append(addrs, a)
		if first || rr.Header().Ttl < minTTL {
			minTTL = rr.Header().Ttl
			first = false
		}
	}
	ttl := time.Duration(minTTL) * time.Second * 2
	if ttl < learnMin {
		ttl = learnMin
	}
	if ttl > learnMax {
		ttl = learnMax
	}
	return addrs, ttl
}

func reply(w dns.ResponseWriter, req *dns.Msg, rcode int) {
	m := new(dns.Msg)
	m.SetRcode(req, rcode)
	m.RecursionAvailable = true
	w.WriteMsg(m)
}

var privateReverse = []string{
	"10.in-addr.arpa", "168.192.in-addr.arpa", "254.169.in-addr.arpa",
	"16.172.in-addr.arpa", "17.172.in-addr.arpa", "18.172.in-addr.arpa", "19.172.in-addr.arpa",
	"20.172.in-addr.arpa", "21.172.in-addr.arpa", "22.172.in-addr.arpa", "23.172.in-addr.arpa",
	"24.172.in-addr.arpa", "25.172.in-addr.arpa", "26.172.in-addr.arpa", "27.172.in-addr.arpa",
	"28.172.in-addr.arpa", "29.172.in-addr.arpa", "30.172.in-addr.arpa", "31.172.in-addr.arpa",
	"d.f.ip6.arpa", "c.f.ip6.arpa", "8.e.f.ip6.arpa", "9.e.f.ip6.arpa", "a.e.f.ip6.arpa", "b.e.f.ip6.arpa",
}

var privateReverseSet = NewSuffixSet(privateReverse)

// local answers names the router is authoritative for: its own, its DHCP
// clients', and reverse lookups of private addresses (which must never be
// sent upstream).
func (p *Policy) local(req *dns.Msg, name string, qtype uint16) *dns.Msg {
	if qtype == dns.TypePTR {
		if !privateReverseSet.Match(name) {
			return nil
		}
		m := new(dns.Msg)
		m.SetReply(req)
		m.Authoritative = true
		m.RecursionAvailable = true
		if a, ok := addrFromReverse(name); ok {
			if host, ok := p.Reverse[a]; ok {
				m.Answer = append(m.Answer, &dns.PTR{
					Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60},
					Ptr: dns.Fqdn(host),
				})
				return m
			}
		}
		m.Rcode = dns.RcodeNameError
		return m
	}
	addrs, ok := p.Hosts[name]
	if !ok {
		return nil
	}
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	m.RecursionAvailable = true
	for _, a := range addrs {
		hdr := dns.RR_Header{Name: req.Question[0].Name, Class: dns.ClassINET, Ttl: 30}
		switch {
		case qtype == dns.TypeA && a.Is4():
			hdr.Rrtype = dns.TypeA
			m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: a.AsSlice()})
		case qtype == dns.TypeAAAA && a.Is6():
			hdr.Rrtype = dns.TypeAAAA
			m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: a.AsSlice()})
		}
	}
	return m
}

func addrFromReverse(name string) (netip.Addr, bool) {
	labels := strings.Split(name, ".")
	switch {
	case strings.HasSuffix(name, ".in-addr.arpa") && len(labels) == 6:
		a, err := netip.ParseAddr(labels[3] + "." + labels[2] + "." + labels[1] + "." + labels[0])
		return a, err == nil
	case strings.HasSuffix(name, ".ip6.arpa") && len(labels) == 34:
		var sb strings.Builder
		for i := 31; i >= 0; i-- {
			if len(labels[i]) != 1 {
				return netip.Addr{}, false
			}
			sb.WriteString(labels[i])
			if i%4 == 0 && i != 0 {
				sb.WriteByte(':')
			}
		}
		a, err := netip.ParseAddr(sb.String())
		return a, err == nil
	}
	return netip.Addr{}, false
}
