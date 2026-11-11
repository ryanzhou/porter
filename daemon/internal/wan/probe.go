package wan

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// ProbeResult is the outcome of one reachability check.
type ProbeResult struct {
	State     string
	PortalURL string
	Detail    string
}

func bindDialer(ifname string, timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifname)
			}); err != nil {
				return err
			}
			return serr
		},
	}
}

// Probe fetches the probe URL through one specific uplink, using that
// uplink's own resolvers, exactly as a client device's captive portal
// check would. A response other than the expected one means something in
// between answered instead: a captive portal.
func Probe(ifname string, p config.Probe, dnsServers []string) ProbeResult {
	u, err := url.Parse(p.URL)
	if err != nil {
		return ProbeResult{State: StLimited, Detail: "bad probe URL"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	d := bindDialer(ifname, 4*time.Second)

	host := u.Hostname()
	var addrs []string
	if ip := net.ParseIP(host); ip != nil {
		addrs = []string{host}
	} else {
		addrs, err = resolve(ctx, d, host, dnsServers)
		if err != nil {
			return ProbeResult{State: StLimited, Detail: "dns: " + err.Error()}
		}
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	client := &http.Client{
		Timeout: 6 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var lastErr error
				for _, a := range addrs {
					c, err := d.DialContext(ctx, network, net.JoinHostPort(a, port))
					if err == nil {
						return c, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		},
		// A redirect is the answer we are looking for, not something to
		// follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	req.Header.Set("User-Agent", "CaptiveNetworkSupport/1.0 wispr")
	res, err := client.Do(req)
	if err != nil {
		return ProbeResult{State: StLimited, Detail: err.Error()}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if res.StatusCode == http.StatusOK && strings.Contains(string(body), p.Expect) {
		return ProbeResult{State: StOnline}
	}
	if res.StatusCode >= 500 {
		// The probe's own server, or a proxy on the way, having a bad
		// moment. A sign-in page does not look like this.
		return ProbeResult{State: StLimited, Detail: res.Status}
	}
	portal := p.URL
	if loc := res.Header.Get("Location"); loc != "" {
		if lu, err := u.Parse(loc); err == nil && (lu.Scheme == "http" || lu.Scheme == "https") {
			portal = lu.String()
		}
	}
	return ProbeResult{State: StCaptive, PortalURL: portal, Detail: res.Status}
}

func resolve(ctx context.Context, d *net.Dialer, host string, servers []string) ([]string, error) {
	if len(servers) == 0 {
		// An uplink with a fixed address and no resolver typed in. Its
		// reachability is still worth knowing.
		servers = []string{"1.1.1.1", "9.9.9.9"}
	}
	var lastErr error
	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(host), qtype)
		for _, s := range servers {
			c := &dns.Client{Net: "udp", Dialer: d, Timeout: 2 * time.Second}
			r, _, err := c.ExchangeContext(ctx, m, net.JoinHostPort(s, "53"))
			if err != nil {
				lastErr = err
				continue
			}
			var out []string
			for _, rr := range r.Answer {
				switch a := rr.(type) {
				case *dns.A:
					out = append(out, a.A.String())
				case *dns.AAAA:
					out = append(out, a.AAAA.String())
				}
			}
			if len(out) > 0 {
				return out, nil
			}
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no addresses for " + host)
	}
	return nil, lastErr
}
