package speedtest

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

func TestRun(t *testing.T) {
	var received atomic.Int64
	chunk := make([]byte, 32*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/down":
			for i := 0; i < 4096; i++ { // 128 MB at most
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		case "/up":
			n, _ := io.Copy(io.Discard, r.Body)
			received.Add(n)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var phases []string
	res := Run(context.Background(), config.SpeedTest{DownloadURL: srv.URL + "/down", UploadURL: srv.URL + "/up"}, "direct",
		Options{Duration: 1200 * time.Millisecond, Streams: 2, Progress: func(phase string, mbps float64) {
			if len(phases) == 0 || phases[len(phases)-1] != phase {
				phases = append(phases, phase)
			}
		}})
	if res.Error != "" {
		t.Fatal(res.Error)
	}
	// Loopback is fast; the point is that all three numbers were measured.
	if res.DownMbps < 50 || res.UpMbps < 50 || res.LatencyMs <= 0 || res.LatencyMs > 500 {
		t.Errorf("implausible result: %+v", res)
	}
	if received.Load() == 0 {
		t.Error("server received no upload")
	}
	if len(phases) != 3 || phases[0] != "latency" || phases[1] != "download" || phases[2] != "upload" {
		t.Errorf("progress phases: %v", phases)
	}

	// Failures are reported, not hidden behind zeros.
	bad := Run(context.Background(), config.SpeedTest{DownloadURL: srv.URL + "/missing", UploadURL: srv.URL + "/up"}, "direct",
		Options{Duration: 500 * time.Millisecond})
	if bad.Error == "" || bad.DownMbps != 0 {
		t.Errorf("missing download URL: %+v", bad)
	}
	gone := Run(context.Background(), config.SpeedTest{DownloadURL: "http://127.0.0.1:1/down", UploadURL: "http://127.0.0.1:1/up"}, "direct",
		Options{Duration: 500 * time.Millisecond})
	if gone.Error == "" {
		t.Errorf("unreachable server: %+v", gone)
	}
}

// An upload is counted by what the other end has acknowledged, on
// connections that are open and on those closed since, and not by what
// was handed to the kernel on this side.
func TestUploadCountsWhatArrived(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()
	hc, conns := client(0, "")
	dial := hc.Transport.(*http.Transport).DialContext
	const size = 300_000
	arrived := func(want uint64) bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			// The opening of a connection counts as a byte, where it is
			// counted at all.
			if n, ok := conns.acked(); ok && n >= want && n <= want+4 {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	first, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if !arrived(size) {
		n, ok := conns.acked()
		t.Fatalf("after sending %d bytes: %d acknowledged (known: %v)", size, n, ok)
	}
	second, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if !arrived(2 * size) {
		t.Fatal("a second connection's bytes were not added")
	}
	// Closed, a connection's count is kept.
	first.Close()
	first.Close()
	if n, ok := conns.acked(); !ok || n < 2*size || n > 2*size+4 {
		t.Errorf("after closing a connection: %d acknowledged (known: %v)", n, ok)
	}
}
