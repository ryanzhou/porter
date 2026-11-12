// Package adblock blocks advertising and tracking domains at the DNS
// level, from block lists the router downloads and refreshes itself.
//
// Lists are cached on the data partition, so blocking works from boot,
// before (or without) an internet connection.
package adblock

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

const (
	// Refresh is how often lists are fetched again.
	Refresh = 24 * time.Hour
	// retry is the wait after a failed fetch.
	retry = 30 * time.Minute
	// maxListBytes bounds one downloaded list.
	maxListBytes = 64 << 20
	// maxDomains bounds the combined set (memory is the limit).
	maxDomains = 2_000_000
)

// Set is an immutable collection of blocked names. Hosts-style entries
// block exactly the host they name; Adblock-style and wildcard entries
// block a domain with all its subdomains.
type Set struct {
	exact  map[string]struct{}
	suffix map[string]struct{}
}

func newSet() *Set { return &Set{exact: map[string]struct{}{}, suffix: map[string]struct{}{}} }

// Len is the number of entries.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.exact) + len(s.suffix)
}

// Match reports whether name (lower case, no trailing dot) is blocked.
func (s *Set) Match(name string) bool {
	if s == nil {
		return false
	}
	if _, ok := s.exact[name]; ok {
		return true
	}
	for {
		if _, ok := s.suffix[name]; ok {
			return true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return false
		}
		name = name[i+1:]
	}
}

// parse adds the entries of one list. It understands the three formats
// block lists come in:
//
//	0.0.0.0 ads.example.com      hosts file (also 127.0.0.1, ::)
//	ads.example.com              one domain per line
//	||ads.example.com^           Adblock syntax; *.ads.example.com
//
// Comments, exceptions (@@...) and rules that are not plain domains are
// skipped. It returns the number of entries added.
func (s *Set) parse(r io.Reader) int {
	added := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if s.Len() >= maxDomains {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '!' || line[0] == '[' || strings.HasPrefix(line, "@@") {
			continue
		}
		if i := strings.IndexAny(line, "#"); i > 0 {
			line = strings.TrimSpace(line[:i])
		}
		wild := false
		switch {
		case strings.HasPrefix(line, "||"):
			line = strings.TrimPrefix(line, "||")
			end := strings.IndexAny(line, "^$/")
			if end < 0 {
				end = len(line)
			} else if rest := line[end:]; rest != "^" && !strings.HasPrefix(rest, "^$") {
				continue // a path or other qualifier: not a whole-domain rule
			}
			line, wild = line[:end], true
		case strings.HasPrefix(line, "*."):
			line, wild = line[2:], true
		default:
			f := strings.Fields(line)
			switch len(f) {
			case 1:
				line = f[0]
			case 2:
				if f[0] != "0.0.0.0" && f[0] != "127.0.0.1" && f[0] != "::" && f[0] != "::1" {
					continue
				}
				line = f[1]
			default:
				continue
			}
		}
		d := strings.ToLower(strings.Trim(line, "."))
		if !validName(d) || d == "localhost" || d == "localhost.localdomain" || d == "broadcasthost" || d == "local" {
			continue
		}
		target := s.exact
		if wild {
			target = s.suffix
		}
		if _, dup := target[d]; !dup {
			target[d] = struct{}{}
			added++
		}
	}
	return added
}

func validName(d string) bool {
	if len(d) < 3 || len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return false
		}
	}
	if strings.Contains(d, "..") {
		return false
	}
	// An IP address is not a domain: the last label must have a letter.
	tld := d[strings.LastIndexByte(d, '.')+1:]
	return strings.ContainsAny(tld, "abcdefghijklmnopqrstuvwxyz")
}

// ListStatus describes one configured list.
type ListStatus struct {
	URL     string `json:"url"`
	Domains int    `json:"domains"`
	Updated int64  `json:"updated,omitempty"` // unix time of the copy in use
	Error   string `json:"error,omitempty"`
}

// Status is what the app shows.
type Status struct {
	Enabled bool         `json:"enabled"`
	Domains int          `json:"domains"`
	Blocked uint64       `json:"blocked"` // queries blocked since start
	Lists   []ListStatus `json:"lists"`
}

type Manager struct {
	Log    *slog.Logger
	Dir    string // cache directory
	Client *http.Client

	set     atomic.Pointer[Set]
	blocked atomic.Uint64

	mu      sync.Mutex
	enabled bool
	urls    []string
	lists   map[string]*ListStatus
	nextTry map[string]time.Time
	wake    chan struct{}
}

func (m *Manager) init() {
	if m.lists == nil {
		m.lists = map[string]*ListStatus{}
		m.nextTry = map[string]time.Time{}
		m.wake = make(chan struct{}, 1)
	}
}

func (m *Manager) cachePath(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(m.Dir, hex.EncodeToString(sum[:8])+".list")
}

// Configure sets whether blocking is on and which lists to use. Cached
// copies take effect immediately; missing or stale ones are fetched by
// Run.
func (m *Manager) Configure(enabled bool, urls []string) {
	m.mu.Lock()
	m.init()
	same := enabled == m.enabled && strings.Join(urls, "\n") == strings.Join(m.urls, "\n")
	m.enabled, m.urls = enabled, append([]string(nil), urls...)
	m.mu.Unlock()
	if same {
		return
	}
	m.rebuild()
	m.kick()
}

func (m *Manager) kick() {
	m.mu.Lock()
	m.init()
	wake := m.wake
	m.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

// RefreshNow makes Run fetch every list again without waiting.
func (m *Manager) RefreshNow() {
	m.mu.Lock()
	m.init()
	for u := range m.nextTry {
		delete(m.nextTry, u)
	}
	for _, u := range m.urls {
		m.nextTry[u] = time.Time{}
	}
	m.mu.Unlock()
	m.kick()
}

// rebuild assembles the active set from the cached lists.
func (m *Manager) rebuild() {
	m.mu.Lock()
	m.init()
	enabled, urls := m.enabled, append([]string(nil), m.urls...)
	m.mu.Unlock()
	if !enabled {
		m.set.Store(nil)
		return
	}
	set := newSet()
	statuses := map[string]*ListStatus{}
	for _, u := range urls {
		st := &ListStatus{URL: u}
		statuses[u] = st
		f, err := os.Open(m.cachePath(u))
		if err != nil {
			continue // not downloaded yet
		}
		st.Domains = set.parse(f)
		if fi, err := f.Stat(); err == nil {
			st.Updated = fi.ModTime().Unix()
		}
		f.Close()
	}
	m.mu.Lock()
	for u, st := range statuses {
		if old := m.lists[u]; old != nil {
			st.Error = old.Error
		}
	}
	m.lists = statuses
	m.mu.Unlock()
	m.set.Store(set)
}

// Blocked reports whether a name is on the block lists, counting hits.
func (m *Manager) Blocked(name string) bool {
	if !m.set.Load().Match(name) {
		return false
	}
	m.blocked.Add(1)
	return true
}

// Match is Blocked without the counting.
func (m *Manager) Match(name string) bool { return m.set.Load().Match(name) }

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	st := Status{Enabled: m.enabled, Domains: m.set.Load().Len(), Blocked: m.blocked.Load(), Lists: []ListStatus{}}
	for _, u := range m.urls {
		if l := m.lists[u]; l != nil {
			st.Lists = append(st.Lists, *l)
		} else {
			st.Lists = append(st.Lists, ListStatus{URL: u})
		}
	}
	return st
}

// Run keeps the lists fresh until ctx ends. online reports whether a
// download is worth attempting, and sparing that data is short at the
// moment: a list the router has a copy of is then left as it is, unless
// RefreshNow asked for it.
func (m *Manager) Run(ctx context.Context, online, sparing func() bool) {
	m.mu.Lock()
	m.init()
	wake := m.wake
	m.mu.Unlock()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-wake:
		}
		if !online() {
			continue
		}
		m.refreshDue(ctx, sparing())
	}
}

func (m *Manager) refreshDue(ctx context.Context, sparing bool) {
	m.mu.Lock()
	enabled, urls := m.enabled, append([]string(nil), m.urls...)
	m.mu.Unlock()
	if !enabled {
		return
	}
	changed := false
	now := time.Now()
	for _, u := range urls {
		m.mu.Lock()
		next, scheduled := m.nextTry[u]
		m.mu.Unlock()
		_, cachedErr := os.Stat(m.cachePath(u))
		if sparing && cachedErr == nil && !(scheduled && next.IsZero()) {
			// The copy there is will do until data is not short. A list
			// there is no copy of is fetched all the same, or switching
			// ad blocking on would do nothing; so is one that somebody
			// asked to have fetched now.
			continue
		}
		if !scheduled {
			// First look at this list: due when the cached copy is old.
			if fi, err := os.Stat(m.cachePath(u)); err == nil {
				next = fi.ModTime().Add(Refresh)
			}
		}
		if now.Before(next) {
			continue
		}
		updated, err := m.fetch(ctx, u)
		m.mu.Lock()
		if st := m.lists[u]; st != nil {
			st.Error = ""
			if err != nil {
				st.Error = err.Error()
			}
		}
		if err != nil {
			m.nextTry[u] = now.Add(retry)
		} else {
			m.nextTry[u] = now.Add(Refresh)
		}
		m.mu.Unlock()
		if err != nil {
			m.Log.Warn("block list download failed", "url", u, "err", err)
		} else if updated {
			m.Log.Info("block list updated", "url", u)
			changed = true
		}
	}
	if changed {
		m.rebuild()
	}
}

// fetch downloads one list into the cache. A list that has not changed
// since the cached copy is not downloaded again.
func (m *Manager) fetch(ctx context.Context, url string) (updated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	path := m.cachePath(url)
	if fi, err := os.Stat(path); err == nil {
		req.Header.Set("If-Modified-Since", fi.ModTime().UTC().Format(http.TimeFormat))
	}
	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotModified:
		// Still current: note that it was checked.
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("server answered %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxListBytes+1))
	if err != nil {
		return false, err
	}
	if len(body) > maxListBytes {
		return false, fmt.Errorf("list is larger than %d MB", maxListBytes>>20)
	}
	// Refuse something that is not a block list (an error page, say)
	// rather than replace a good copy with it.
	if n := newSet().parse(strings.NewReader(string(body))); n == 0 {
		return false, fmt.Errorf("no domains found; not a block list?")
	}
	if err := config.WriteFileAtomic(path, body, 0o644); err != nil {
		return false, err
	}
	return true, nil
}
