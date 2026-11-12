// Package speedtest measures the uplink from the router itself: latency,
// download and upload throughput against a configurable HTTP server,
// optionally through a VPN tunnel or through one chosen uplink.
package speedtest

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Result of one test.
type Result struct {
	Time      int64   `json:"t"`
	Via       string  `json:"via"`
	LatencyMs float64 `json:"latency_ms"`
	DownMbps  float64 `json:"down_mbps"`
	UpMbps    float64 `json:"up_mbps"`
	Error     string  `json:"error,omitempty"`
}

// Options tune a run; the zero value uses the defaults.
type Options struct {
	// Mark is the firewall mark for the test's sockets: policy routing
	// sends them through the matching VPN. 0 means the default route.
	Mark uint32
	// Device is the interface the test's sockets are bound to: the test
	// then goes out by that uplink, whichever is in use. Empty: wherever
	// routing sends it.
	Device string
	// Duration of each of the download and upload phases.
	Duration time.Duration
	Streams  int
	// Progress receives the phase ("latency", "download", "upload") and
	// the running average in Mbit/s.
	Progress func(phase string, mbps float64)
}

// sent keeps the connections of a test, to tell how much of what was
// uploaded has arrived. What the transport has taken from the test is
// not that: on a slow line much of it is still waiting in the kernel
// when the test ends, and a figure that counts it comes out above what
// the line carries. The other end's acknowledgements are what counts.
type sent struct {
	mu   sync.Mutex
	open map[*conn]struct{}
	// done is what was acknowledged on connections that have been closed
	// since. unknown: a connection could not be asked.
	done    uint64
	unknown bool
}

type conn struct {
	net.Conn
	of *sent
}

// acked asks the kernel how many bytes the other end has acknowledged.
func (c *conn) acked() (uint64, bool) {
	tcp, ok := c.Conn.(*net.TCPConn)
	if !ok {
		return 0, false
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return 0, false
	}
	var n uint64
	ok = false
	raw.Control(func(fd uintptr) {
		if info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			n, ok = info.Bytes_acked, true
		}
	})
	return n, ok
}

func (c *conn) Close() error {
	s := c.of
	s.mu.Lock()
	if _, open := s.open[c]; open {
		n, ok := c.acked()
		s.done, s.unknown = s.done+n, s.unknown || !ok
		delete(s.open, c)
	}
	s.mu.Unlock()
	return c.Conn.Close()
}

// acked is what the other end has acknowledged on all of the test's
// connections so far, or false when that cannot be told.
func (s *sent) acked() (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.done
	for c := range s.open {
		n, ok := c.acked()
		total, s.unknown = total+n, s.unknown || !ok
	}
	return total, !s.unknown
}

func client(mark uint32, device string) (*http.Client, *sent) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if mark != 0 || device != "" {
		d.Control = func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				if mark != 0 {
					serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark))
				}
				if device != "" && serr == nil {
					serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device)
				}
			}); err != nil {
				return err
			}
			return serr
		}
	}
	s := &sent{open: map[*conn]struct{}{}}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tracked := &conn{Conn: c, of: s}
		s.mu.Lock()
		s.open[tracked] = struct{}{}
		s.mu.Unlock()
		return tracked, nil
	}
	return &http.Client{Transport: &http.Transport{
		DialContext:           dial,
		ForceAttemptHTTP2:     false, // separate connections, one per stream
		MaxIdleConnsPerHost:   16,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DisableCompression:    true,
	}}, s
}

// Run performs a test. It always returns a Result; failures are reported
// in Result.Error with whatever was measured before them.
func Run(ctx context.Context, cfg config.SpeedTest, via string, opt Options) Result {
	if opt.Duration == 0 {
		opt.Duration = 8 * time.Second
	}
	if opt.Streams == 0 {
		opt.Streams = 4
	}
	if opt.Progress == nil {
		opt.Progress = func(string, float64) {}
	}
	res := Result{Time: time.Now().Unix(), Via: via}
	hc, conns := client(opt.Mark, opt.Device)
	defer hc.CloseIdleConnections()

	opt.Progress("latency", 0)
	lat, err := latency(ctx, hc, cfg.DownloadURL)
	if err != nil {
		res.Error = "the test server could not be reached: " + err.Error()
		return res
	}
	res.LatencyMs = lat

	res.DownMbps, err = measure(ctx, opt, "download", nil, func(ctx context.Context, count *atomic.Int64) error {
		return download(ctx, hc, cfg.DownloadURL, count)
	})
	if err != nil {
		res.Error = "download: " + err.Error()
		return res
	}
	// An upload is counted by what the server has acknowledged since the
	// phase began (with the little that the requests' own lines and any
	// encryption add), where the kernel says.
	before, _ := conns.acked()
	arrived := func() (int64, bool) {
		n, ok := conns.acked()
		return int64(n - before), ok && n >= before
	}
	res.UpMbps, err = measure(ctx, opt, "upload", arrived, func(ctx context.Context, count *atomic.Int64) error {
		return upload(ctx, hc, cfg.UploadURL, count)
	})
	if err != nil {
		res.Error = "upload: " + err.Error()
	}
	return res
}

// latency is the median time to open a connection to the server.
func latency(ctx context.Context, hc *http.Client, raw string) (float64, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, err
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	dial := hc.Transport.(*http.Transport).DialContext
	var samples []float64
	var lastErr error
	for i := 0; i < 5; i++ {
		start := time.Now()
		c, err := dial(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
		if err != nil {
			lastErr = err
			continue
		}
		samples = append(samples, float64(time.Since(start).Microseconds())/1000)
		c.Close()
	}
	if len(samples) == 0 {
		return 0, lastErr
	}
	sort.Float64s(samples)
	return samples[len(samples)/2], nil
}

// measure runs the worker on several streams for the configured time and
// returns the average rate in Mbit/s. The bytes are those the workers
// count, unless arrived knows better.
func measure(ctx context.Context, opt Options, phase string, arrived func() (int64, bool), worker func(context.Context, *atomic.Int64) error) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, opt.Duration)
	defer cancel()
	var count atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, opt.Streams)
	start := time.Now()
	for i := 0; i < opt.Streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := worker(ctx, &count); err != nil && ctx.Err() == nil {
					errs <- err
					return
				}
			}
		}()
	}
	bytes := func() int64 {
		if arrived != nil {
			if n, ok := arrived(); ok {
				return n
			}
		}
		return count.Load()
	}
	rate := func() float64 {
		secs := time.Since(start).Seconds()
		if secs <= 0 {
			return 0
		}
		return float64(bytes()) * 8 / secs / 1e6
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			opt.Progress(phase, rate())
		case <-done:
			elapsed := time.Since(start).Seconds()
			if elapsed > opt.Duration.Seconds() {
				elapsed = opt.Duration.Seconds()
			}
			if count.Load() == 0 {
				select {
				case err := <-errs:
					return 0, err
				default:
					return 0, fmt.Errorf("no data was transferred")
				}
			}
			return float64(bytes()) * 8 / elapsed / 1e6, nil
		}
	}
}

type countingWriter struct{ n *atomic.Int64 }

func (c countingWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

func download(ctx context.Context, hc *http.Client, raw string, count *atomic.Int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("server answered %s", res.Status)
	}
	n, err := io.Copy(countingWriter{count}, res.Body)
	if n == 0 && err == nil {
		return fmt.Errorf("server sent an empty response")
	}
	return nil // a body cut short by the deadline is expected
}

// payload is an endless stream of incompressible data that counts what
// the transport takes from it and ends when the context does.
type payload struct {
	ctx   context.Context
	n     *atomic.Int64
	block []byte
	left  int64
}

func (p *payload) Read(b []byte) (int, error) {
	if p.ctx.Err() != nil || p.left <= 0 {
		return 0, io.EOF
	}
	n := copy(b, p.block)
	if int64(n) > p.left {
		n = int(p.left)
	}
	p.left -= int64(n)
	p.n.Add(int64(n))
	return n, nil
}

var block = func() []byte {
	b := make([]byte, 64*1024)
	rand.Read(b)
	return b
}()

func upload(ctx context.Context, hc *http.Client, raw string, count *atomic.Int64) error {
	// 25 MB per request: big enough to reach full speed, small enough for
	// servers that cap request size.
	body := &payload{ctx: ctx, n: count, block: block, left: 25 << 20}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, raw, body)
	if err != nil {
		return err
	}
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("server answered %s", res.Status)
	}
	return nil
}
