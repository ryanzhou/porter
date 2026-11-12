package dnsfwd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// upstreams talks to resolvers over plain DNS, DNS-over-TLS and
// DNS-over-HTTPS. Sockets carry the firewall mark of the path the query
// belongs to, so policy routing sends them out through the right uplink
// or tunnel.
type upstreams struct {
	mu    sync.Mutex
	tls   map[string]*tlsConn
	https map[string]*http.Client
	// roots overrides the system trust store (tests).
	roots *x509.CertPool
}

func newUpstreams() *upstreams {
	return &upstreams{tls: map[string]*tlsConn{}, https: map[string]*http.Client{}}
}

// egress is the way out of the router a query is given: the firewall mark
// of its path and, for a path that is one uplink, that uplink's
// interface. The mark alone finds the uplink's routing table; bound to
// the interface as well, a resolver inside the uplink's own network is
// reached through it even where another uplink is on a network numbered
// the same (the cable and the Wi-Fi of one hotel).
type egress struct {
	mark uint32
	dev  string
}

func dialer(via egress) *net.Dialer {
	d := &net.Dialer{Timeout: 3 * time.Second}
	if via.mark != 0 || via.dev != "" {
		d.Control = func(network, address string, c syscall.RawConn) error {
			var serr error
			err := c.Control(func(fd uintptr) {
				if via.mark != 0 {
					serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(via.mark))
				}
				if via.dev != "" && serr == nil {
					serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, via.dev)
				}
			})
			if err != nil {
				return err
			}
			return serr
		}
	}
	return d
}

func (u *upstreams) exchange(ctx context.Context, m *dns.Msg, srv config.Upstream, via egress) (*dns.Msg, error) {
	switch srv.Proto {
	case "udp":
		return exchangePlain(ctx, m, srv, via)
	case "tls":
		return u.exchangeTLS(ctx, m, srv, via)
	case "https":
		return u.exchangeHTTPS(ctx, m, srv, via)
	}
	return nil, fmt.Errorf("unknown upstream protocol %q", srv.Proto)
}

func exchangePlain(ctx context.Context, m *dns.Msg, srv config.Upstream, via egress) (*dns.Msg, error) {
	c := &dns.Client{Net: "udp", Dialer: dialer(via), Timeout: 2 * time.Second, UDPSize: 1232}
	resp, _, err := c.ExchangeContext(ctx, m, srv.Addr.String())
	if err != nil || !resp.Truncated {
		// A resolver that does not answer over UDP is not asked again
		// over TCP: that only doubles the wait before the next one is
		// tried.
		return resp, err
	}
	c = &dns.Client{Net: "tcp", Dialer: dialer(via), Timeout: 3 * time.Second}
	resp, _, err = c.ExchangeContext(ctx, m, srv.Addr.String())
	return resp, err
}

// tlsConn is one persistent DNS-over-TLS connection. Queries are
// serialised; a broken connection is re-established once per query.
type tlsConn struct {
	mu   sync.Mutex
	conn *dns.Conn
}

func (u *upstreams) exchangeTLS(ctx context.Context, m *dns.Msg, srv config.Upstream, via egress) (*dns.Msg, error) {
	key := fmt.Sprintf("%s#%s#%d#%s", srv.Addr, srv.ServerName, via.mark, via.dev)
	u.mu.Lock()
	tc := u.tls[key]
	if tc == nil {
		tc = &tlsConn{}
		u.tls[key] = tc
	}
	u.mu.Unlock()

	tc.mu.Lock()
	defer tc.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if tc.conn == nil {
			cfg := &tls.Config{ServerName: srv.ServerName, MinVersion: tls.VersionTLS12, RootCAs: u.roots}
			if srv.ServerName == "" {
				// No name to verify against was configured: verify the
				// certificate for the address itself.
				cfg.ServerName = srv.Addr.Addr().String()
			}
			td := &tls.Dialer{NetDialer: dialer(via), Config: cfg}
			raw, err := td.DialContext(ctx, "tcp", srv.Addr.String())
			if err != nil {
				return nil, err
			}
			tc.conn = &dns.Conn{Conn: raw}
		}
		// A connection that has been idle may be dead without either end
		// having noticed (the uplink changed, the resolver timed it out
		// behind a NAT). The wait for an answer is kept short so that the
		// second attempt, on a new connection, still fits in the time the
		// client is prepared to wait.
		deadline := time.Now().Add(2 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		tc.conn.SetDeadline(deadline)
		if err := tc.conn.WriteMsg(m); err == nil {
			resp, err := tc.conn.ReadMsg()
			if err == nil && resp.Id == m.Id {
				return resp, nil
			}
			lastErr = err
			if err == nil {
				lastErr = fmt.Errorf("mismatched response id")
			}
		} else {
			lastErr = err
		}
		tc.conn.Close()
		tc.conn = nil
	}
	return nil, lastErr
}

func (u *upstreams) exchangeHTTPS(ctx context.Context, m *dns.Msg, srv config.Upstream, via egress) (*dns.Msg, error) {
	key := fmt.Sprintf("%s#%s#%d#%s", srv.URL, srv.Addr, via.mark, via.dev)
	u.mu.Lock()
	hc := u.https[key]
	if hc == nil {
		d := dialer(via)
		target := srv.Addr.String()
		hc = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				// The resolver's own address is pinned, so reaching it
				// never needs a DNS lookup.
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return d.DialContext(ctx, network, target)
				},
				TLSClientConfig:     &tls.Config{ServerName: srv.ServerName, MinVersion: tls.VersionTLS12, RootCAs: u.roots},
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        2,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 4 * time.Second,
			},
		}
		u.https[key] = hc
	}
	u.mu.Unlock()

	q := m.Copy()
	id := q.Id
	q.Id = 0 // RFC 8484: cache-friendly
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 65535))
	if err != nil {
		return nil, err
	}
	resp := new(dns.Msg)
	if err := resp.Unpack(body); err != nil {
		return nil, err
	}
	resp.Id = id
	return resp, nil
}

// --- cache ---------------------------------------------------------------

type cacheKey struct {
	group string
	name  string
	qtype uint16
	do    bool
}

type cacheEntry struct {
	msg     *dns.Msg
	stored  time.Time
	expires time.Time
}

type cache struct {
	mu  sync.Mutex
	max int
	m   map[cacheKey]cacheEntry
}

func newCache(max int) *cache { return &cache{max: max, m: map[cacheKey]cacheEntry{}} }

func (c *cache) flush() {
	c.mu.Lock()
	c.m = map[cacheKey]cacheEntry{}
	c.mu.Unlock()
}

func (c *cache) get(k cacheKey) *dns.Msg {
	c.mu.Lock()
	e, ok := c.m[k]
	if ok && time.Now().After(e.expires) {
		delete(c.m, k)
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return nil
	}
	m := e.msg.Copy()
	age := uint32(time.Since(e.stored) / time.Second)
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			if rr.Header().Ttl > age {
				rr.Header().Ttl -= age
			} else {
				rr.Header().Ttl = 1
			}
		}
	}
	return m
}

func (c *cache) put(k cacheKey, m *dns.Msg) {
	if m.Truncated || (m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError) {
		return
	}
	ttl := uint32(3600)
	seen := false
	for _, rr := range m.Answer {
		if rr.Header().Ttl < ttl {
			ttl = rr.Header().Ttl
		}
		seen = true
	}
	if !seen {
		// Negative answer: honour the SOA minimum, but briefly.
		ttl = 30
		for _, rr := range m.Ns {
			if soa, ok := rr.(*dns.SOA); ok {
				ttl = min(soa.Minttl, soa.Hdr.Ttl, 60)
			}
		}
	}
	if ttl == 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		// Evict a random handful; cheap and good enough at this size.
		n := rand.IntN(8) + 8
		for key := range c.m {
			delete(c.m, key)
			if n--; n == 0 {
				break
			}
		}
	}
	c.m[k] = cacheEntry{msg: m.Copy(), stored: now, expires: now.Add(time.Duration(ttl) * time.Second)}
}
