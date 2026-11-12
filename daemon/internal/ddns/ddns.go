// Package ddns keeps a name pointing at the router's public address, with
// one of a few providers' update protocols, and finds out what that
// address is.
package ddns

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// How the router can be reached from the internet over IPv4.
const (
	// ReachDirect: its address is a public one, and its own.
	ReachDirect = "direct"
	// ReachNAT: it is behind another router, which has the public address
	// and would have to pass on what arrives there.
	ReachNAT = "nat"
	// ReachCGNAT: the provider shares one public address among customers.
	// Nothing arrives from outside, whatever is set up at home.
	ReachCGNAT = "cgnat"
	// ReachOffline: there is no internet source in use, or it has no
	// IPv4 address.
	ReachOffline = "offline"
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Classify says how the router can be reached, from the address its
// internet source has (own) and the address the internet sees its
// requests come from (seen; the zero value when that is not known).
// What was seen decides where it is known. Where it is not, an address
// from the private ranges means another router, one from the range set
// aside for providers means the provider's translation, and any other is
// taken for what it looks like.
func Classify(own, seen netip.Addr) string {
	switch {
	case !own.IsValid():
		return ReachOffline
	case seen.IsValid() && seen == own:
		return ReachDirect
	case own.IsPrivate() || own.IsLinkLocalUnicast() || own.IsLoopback():
		return ReachNAT
	case seen.IsValid() || cgnat.Contains(own):
		return ReachCGNAT
	}
	return ReachDirect
}

// PublicAddr asks the address check (config.Inbound.CheckURL) which IPv4
// address the router's requests arrive from. The answer is either the
// address and nothing else, or lines of which one is "ip=<address>".
func PublicAddr(ctx context.Context, rawURL string) (netip.Addr, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		// Over IPv4, whatever the name resolves to: it is the IPv4
		// address that is in question.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp4", addr) },
	}}
	body, status, err := fetch(ctx, client, http.MethodGet, rawURL, nil, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	if status/100 != 2 {
		return netip.Addr{}, fmt.Errorf("HTTP %d", status)
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "ip=")
		if a, err := netip.ParseAddr(line); err == nil && a.Is4() && !a.IsUnspecified() {
			return a, nil
		}
	}
	return netip.Addr{}, errors.New("no address in the answer")
}

// fetch makes one request and returns what came back. Its errors never
// carry the URL, which for most providers has a secret in it.
func fetch(ctx context.Context, client *http.Client, method, rawURL string, header map[string]string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("not a web address")
	}
	req.Header.Set("User-Agent", "Porter-router/1 (dynamic DNS)")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 256*1024))
	return b, res.StatusCode, nil
}

// Error is why an update did not happen, in words for the person whose
// name it is.
type Error struct {
	Msg string
	// Permanent: asking again changes nothing until the settings do. A
	// provider that says the password is wrong is not asked every minute.
	Permanent bool
}

func (e *Error) Error() string { return e.Msg }

func refused(format string, a ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, a...), Permanent: true}
}
func failed(format string, a ...any) *Error { return &Error{Msg: fmt.Sprintf(format, a...)} }

func unreachable(err error) *Error {
	return failed("The provider could not be reached (%v).", err)
}

// Endpoints are where the providers with a fixed address are found, and
// how a dyndns2 server is spoken to. Tests point them at servers of
// their own.
type Endpoints struct {
	Dyndns2Scheme string
	DuckDNS       string
	Cloudflare    string
}

// DefaultEndpoints are the real ones.
var DefaultEndpoints = Endpoints{Dyndns2Scheme: "https", DuckDNS: "https://www.duckdns.org", Cloudflare: "https://api.cloudflare.com/client/v4"}

// Update tells the provider the addresses once. An address that is not
// valid is one the router does not have, and is left as the provider has
// it.
func Update(ctx context.Context, client *http.Client, ep Endpoints, d config.DDNS, ip4, ip6 netip.Addr) error {
	str := func(a netip.Addr) string {
		if !a.IsValid() {
			return ""
		}
		return a.String()
	}
	switch d.Provider {
	case config.DDNSDyndns2:
		ips := []string{}
		for _, a := range []netip.Addr{ip4, ip6} {
			if a.IsValid() {
				ips = append(ips, a.String())
			}
		}
		u := url.URL{Scheme: ep.Dyndns2Scheme, Host: d.Server, Path: "/nic/update",
			RawQuery: url.Values{"hostname": {d.Hostname}, "myip": {strings.Join(ips, ",")}}.Encode()}
		auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(d.Username+":"+d.Password))
		body, status, err := fetch(ctx, client, http.MethodGet, u.String(), map[string]string{"Authorization": auth}, nil)
		if err != nil {
			return unreachable(err)
		}
		return dyndns2Answer(d.Hostname, status, string(body))

	case config.DDNSDuckDNS:
		q := url.Values{"domains": {d.DuckName()}, "token": {d.Password}}
		if ip4.IsValid() {
			q.Set("ip", ip4.String())
		}
		if ip6.IsValid() {
			q.Set("ipv6", ip6.String())
		}
		body, status, err := fetch(ctx, client, http.MethodGet, ep.DuckDNS+"/update?"+q.Encode(), nil, nil)
		switch {
		case err != nil:
			return unreachable(err)
		case strings.HasPrefix(strings.TrimSpace(string(body)), "OK"):
			return nil
		case strings.HasPrefix(strings.TrimSpace(string(body)), "KO"):
			return refused("DuckDNS did not accept the name or the token.")
		}
		return failed("DuckDNS answered with an error (HTTP %d).", status)

	case config.DDNSCloudflare:
		return cloudflare(ctx, client, ep.Cloudflare, d, ip4, ip6)

	case config.DDNSCustom:
		u := strings.NewReplacer("{ip}", str(ip4), "{ip6}", str(ip6)).Replace(d.URL)
		_, status, err := fetch(ctx, client, http.MethodGet, u, nil, nil)
		switch {
		case err != nil:
			return unreachable(err)
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return refused("The address for dynamic DNS did not accept what it was sent (HTTP %d).", status)
		case status/100 != 2:
			return failed("The address for dynamic DNS answered with an error (HTTP %d).", status)
		}
		return nil
	}
	return refused("No provider is chosen.")
}

// dyndns2Answer reads the one word a dyndns2 server answers with.
func dyndns2Answer(name string, status int, body string) error {
	word := ""
	if f := strings.Fields(body); len(f) > 0 {
		word = strings.ToLower(f[0])
	}
	switch word {
	case "good", "nochg":
		return nil
	case "badauth":
		return refused("The provider did not accept the user name or the password.")
	case "nohost":
		return refused("The provider does not know the name %s in this account.", name)
	case "notfqdn":
		return refused("The provider does not take %s for a complete name.", name)
	case "numhost":
		return refused("The provider says %s stands for several hosts, which it does not update this way.", name)
	case "abuse":
		return refused("The provider has blocked updates for %s. It says how to lift that on its website.", name)
	case "!donator", "badagent":
		return refused("The provider does not offer this to your account (it answered “%s”).", word)
	case "dnserr", "911":
		return failed("The provider has a problem of its own at the moment (it answered “%s”).", word)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return refused("The provider did not accept the user name or the password.")
	}
	return failed("The provider answered in a way the router does not understand (HTTP %d).", status)
}

// cloudflare sets the A and AAAA records of the name through Cloudflare's
// API: it finds the zone the name is in, then each record, and changes or
// creates it unless it says the right thing already.
func cloudflare(ctx context.Context, client *http.Client, base string, d config.DDNS, ip4, ip6 netip.Addr) error {
	type record struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	call := func(method, path string, in any, out any) error {
		var body []byte
		if in != nil {
			body, _ = json.Marshal(in)
		}
		b, status, err := fetch(ctx, client, method, base+path, map[string]string{"Authorization": "Bearer " + d.Password, "Content-Type": "application/json"}, body)
		if err != nil {
			return unreachable(err)
		}
		var res struct {
			Success bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(b, &res) != nil {
			return failed("Cloudflare answered in a way the router does not understand (HTTP %d).", status)
		}
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return refused("Cloudflare did not accept the API token, or the token may not edit the DNS records of this domain.")
		case !res.Success && len(res.Errors) > 0:
			return failed("Cloudflare answered: %s", res.Errors[0].Message)
		case !res.Success:
			return failed("Cloudflare answered with an error (HTTP %d).", status)
		}
		if out != nil && json.Unmarshal(res.Result, out) != nil {
			return failed("Cloudflare answered in a way the router does not understand.")
		}
		return nil
	}
	// The zone is the name itself or one of the names above it.
	zone := ""
	for name := d.Hostname; strings.Contains(name, ".") && zone == ""; _, name, _ = strings.Cut(name, ".") {
		var zones []record
		if err := call(http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
			return err
		}
		if len(zones) > 0 {
			zone = zones[0].ID
		}
	}
	if zone == "" {
		return refused("Cloudflare has no domain that %s belongs to in the account of this API token.", d.Hostname)
	}
	for _, r := range []struct {
		typ string
		ip  netip.Addr
	}{{"A", ip4}, {"AAAA", ip6}} {
		if !r.ip.IsValid() {
			continue
		}
		var have []record
		if err := call(http.MethodGet, "/zones/"+zone+"/dns_records?type="+r.typ+"&name="+url.QueryEscape(d.Hostname), nil, &have); err != nil {
			return err
		}
		var err error
		switch {
		case len(have) == 0:
			// Not behind Cloudflare's proxy, which would answer for the
			// name with its own addresses; and not remembered for long.
			err = call(http.MethodPost, "/zones/"+zone+"/dns_records", map[string]any{"type": r.typ, "name": d.Hostname, "content": r.ip.String(), "ttl": 120, "proxied": false}, nil)
		case have[0].Content != r.ip.String():
			err = call(http.MethodPatch, "/zones/"+zone+"/dns_records/"+have[0].ID, map[string]any{"content": r.ip.String()}, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Status is how the name stands.
type Status struct {
	// State is "off", "waiting" (nothing to publish, or the first update
	// not made yet), "ok" or "error".
	State string `json:"state"`
	// Updated is when the provider last took an update (unix time), and
	// Address and Address6 what it was told.
	Updated  int64  `json:"updated,omitempty"`
	Address  string `json:"address,omitempty"`
	Address6 string `json:"address6,omitempty"`
	// Error is why the last attempt failed, for people.
	Error string `json:"error,omitempty"`
}

// How often the provider is told what it already knows, so that a name
// is not dropped for silence, and how failed attempts are spaced.
const (
	reassert   = 24 * time.Hour
	firstRetry = time.Minute
	lastRetry  = time.Hour
)

// Manager keeps the name up to date: it updates when the address or the
// settings change, once a day otherwise, and after a failure with a
// growing pause.
type Manager struct {
	Log    *slog.Logger
	Client *http.Client
	// Endpoints is DefaultEndpoints unless a test says otherwise.
	Endpoints Endpoints
	// OnChange is called when the status has changed.
	OnChange func()
	// Now is the clock; time.Now unless a test says otherwise.
	Now func() time.Time

	mu       sync.Mutex
	cfg      config.DDNS
	ip4, ip6 netip.Addr
	force    bool
	st       Status
	done     string // what the provider was last told with success, and when
	doneAt   time.Time
	tried    string // what failed last, how often in a row, and when to try again
	fails    int
	retryAt  time.Time
	wake     chan struct{}
	wakeOnce sync.Once
}

func (m *Manager) init() {
	m.wakeOnce.Do(func() {
		m.wake = make(chan struct{}, 1)
		if m.Now == nil {
			m.Now = time.Now
		}
		if m.Client == nil {
			m.Client = &http.Client{Timeout: 20 * time.Second}
		}
		if m.Endpoints == (Endpoints{}) {
			m.Endpoints = DefaultEndpoints
		}
		m.st.State = "off"
	})
}

// Want says what is to be kept up to date and with which addresses. It
// returns at once; the work is done by Run.
func (m *Manager) Want(cfg config.DDNS, ip4, ip6 netip.Addr) {
	m.init()
	m.mu.Lock()
	changed := cfg != m.cfg || ip4 != m.ip4 || ip6 != m.ip6
	m.cfg, m.ip4, m.ip6 = cfg, ip4, ip6
	m.mu.Unlock()
	if changed {
		m.kick()
	}
}

// Refresh makes the next step an update whether or not one is due.
func (m *Manager) Refresh() {
	m.init()
	m.mu.Lock()
	m.force = true
	m.mu.Unlock()
	m.kick()
}

func (m *Manager) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) Status() Status {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

// Run does the updates until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	m.init()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.wake:
		}
		m.step(ctx)
	}
}

// step makes an update if one is due.
func (m *Manager) step(ctx context.Context) {
	m.init()
	m.mu.Lock()
	cfg, ip4, ip6, force := m.cfg, m.ip4, m.ip6, m.force
	m.force = false
	if !cfg.IPv6 {
		ip6 = netip.Addr{}
	}
	now := m.Now()
	before := m.st
	key := fmt.Sprintf("%+v|%v|%v", cfg, ip4, ip6)
	due := false
	switch {
	case !cfg.Enabled:
		m.st = Status{State: "off"}
		m.done, m.tried, m.fails = "", "", 0
	case !ip4.IsValid() && !ip6.IsValid():
		// Nothing to tell the provider. What it was last told stays in
		// the status: the name still says that.
		m.st.State, m.st.Error = "waiting", "The router has no public address to give the name at the moment."
	case force:
		due = true
	case key == m.done:
		due = now.Sub(m.doneAt) >= reassert
	case key == m.tried:
		due = !now.Before(m.retryAt)
	default:
		due = true
	}
	changed := m.st != before
	m.mu.Unlock()
	if due {
		err := Update(ctx, m.Client, m.Endpoints, cfg, ip4, ip6)
		m.mu.Lock()
		if err == nil {
			m.done, m.doneAt, m.tried, m.fails = key, now, "", 0
			m.st = Status{State: "ok", Updated: now.Unix()}
			if ip4.IsValid() {
				m.st.Address = ip4.String()
			}
			if ip6.IsValid() {
				m.st.Address6 = ip6.String()
			}
			m.Log.Info("dynamic DNS updated", "name", cfg.Hostname, "ipv4", m.st.Address, "ipv6", m.st.Address6)
		} else {
			if key != m.tried {
				m.fails = 0
			}
			m.tried, m.done = key, ""
			m.fails++
			pause := lastRetry
			if m.fails < 7 {
				pause = min(firstRetry<<(m.fails-1), lastRetry)
			}
			var pe *Error
			if errors.As(err, &pe) && pe.Permanent {
				pause = reassert
			}
			m.retryAt = now.Add(pause)
			m.st.State, m.st.Error = "error", err.Error()
			m.Log.Warn("dynamic DNS update failed", "name", cfg.Hostname, "err", err, "again_in", pause)
		}
		changed = changed || m.st != before
		m.mu.Unlock()
	}
	if changed && m.OnChange != nil {
		m.OnChange()
	}
}
