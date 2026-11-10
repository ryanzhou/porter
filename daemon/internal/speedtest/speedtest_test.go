package speedtest

import (
	"context"
	"io"
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
