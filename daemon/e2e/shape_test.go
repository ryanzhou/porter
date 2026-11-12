//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Smoothing out heavy use, as a subtest of TestRouter: an internet source
// held to a speed in both directions, the queues that do it, and their
// life as the setting and the source change. It needs a kernel with CAKE
// and IFB interfaces (sch_cake, sch_ingress, cls_u32, act_mirred, ifb)
// and skips itself, saying which is missing, where there is none; where
// the router's image is tested a skip is a failure.

// shapedSource is how the status says the shaping of one source stands.
type shapedSource struct {
	Key   string `json:"key"`
	If    string `json:"if"`
	State string `json:"state"`
	Down  struct {
		Kbps    int    `json:"kbps"`
		Dropped uint64 `json:"dropped"`
		Marked  uint64 `json:"marked"`
	} `json:"down"`
	Up struct {
		Kbps int `json:"kbps"`
	} `json:"up"`
	Error string `json:"error"`
}

// shaped returns the status of the shaping of the source on an interface.
func (r *rig) shaped(t *testing.T, ifname string) (shapedSource, bool) {
	t.Helper()
	var st struct {
		Shaping []shapedSource `json:"shaping"`
	}
	if err := r.c.Call("status.get", nil, &st); err != nil {
		t.Fatalf("status.get: %v", err)
	}
	for _, s := range st.Shaping {
		if s.If == ifname {
			return s, true
		}
	}
	return shapedSource{}, false
}

// queue is one of an interface's queues, as tc lists it. Bandwidth is as
// tc prints it: a number of bytes a second.
type queue struct {
	Kind    string `json:"kind"`
	Handle  string `json:"handle"`
	Root    bool   `json:"root"`
	Options struct {
		Bandwidth json.RawMessage `json:"bandwidth"`
		Diffserv  string          `json:"diffserv"`
		Flowmode  string          `json:"flowmode"`
		NAT       bool            `json:"nat"`
		Ingress   bool            `json:"ingress"`
		Overhead  int             `json:"overhead"`
	} `json:"options"`
}

// queues lists the queues of one of the router's interfaces; none when
// the interface is not there.
func queues(ifname string) []queue {
	out, err := exec.Command("ip", "netns", "exec", nsRtr, "tc", "-j", "qdisc", "show", "dev", ifname).Output()
	if err != nil {
		return nil
	}
	var qs []queue
	json.Unmarshal(out, &qs)
	return qs
}

// cake finds the router's own queue among an interface's, and says
// whether the interface also has a queue for what arrives.
func cake(ifname string) (q queue, shaping, ingress bool) {
	for _, c := range queues(ifname) {
		if c.Kind == "cake" && c.Root {
			q, shaping = c, true
		}
		ingress = ingress || c.Kind == "ingress"
	}
	return
}

// noShaping says what this kernel lacks for shaping, or nothing.
func noShaping() string {
	if _, err := exec.LookPath("tc"); err != nil {
		return "tc is not installed"
	}
	if out, err := exec.Command("ip", "-n", nsISP, "link", "add", "pt-try", "type", "ifb").CombinedOutput(); err != nil {
		return fmt.Sprintf("this kernel cannot make IFB interfaces (no ifb): %s", bytes.TrimSpace(out))
	}
	defer try("ip -n %s link del pt-try", nsISP)
	for _, probe := range []struct{ what, cmd string }{
		{"has no CAKE queue (no sch_cake)", "qdisc add dev pt-try root cake"},
		{"has no queue for what arrives (no sch_ingress)", "qdisc add dev pt-try handle ffff: ingress"},
		{"cannot redirect what arrives (no cls_u32 or no act_mirred)", "filter add dev pt-try parent ffff: protocol all prio 1 u32 match u32 0 0 action mirred egress redirect dev pt-try"},
	} {
		argv := append([]string{"netns", "exec", nsISP, "tc"}, strings.Fields(probe.cmd)...)
		if out, err := exec.Command("ip", argv...).CombinedOutput(); err != nil {
			return fmt.Sprintf("this kernel %s: %s", probe.what, bytes.TrimSpace(out))
		}
	}
	return ""
}

// endless is a request body that goes on until its context ends.
type endless struct{ ctx context.Context }

func (e endless) Read(p []byte) (int, error) {
	if e.ctx.Err() != nil {
		return 0, io.EOF
	}
	clear(p)
	return len(p), nil
}

// nsClient is an HTTP client whose connections start inside a namespace.
func nsClient(ns string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (c net.Conn, err error) {
			inNS(ns, func() { c, err = (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr) })
			return
		},
	}}
}

// downloadRate fetches a URL from inside a namespace for a while and
// returns the rate the data arrived at, in Mbit/s, counted from the first
// byte of the answer.
func downloadRate(ns, url string, d time.Duration) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d+10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := nsClient(ns).Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	buf := make([]byte, 64*1024)
	var total int64
	start := time.Now()
	for time.Since(start) < d {
		n, err := res.Body.Read(buf)
		total += int64(n)
		if err != nil {
			break
		}
	}
	return float64(total) * 8 / time.Since(start).Seconds() / 1e6, nil
}

// uploadRate sends to a URL from inside a namespace for a while and
// returns the rate at which the data arrived at the far end, which
// counts it in received, in Mbit/s. What the sender has handed to its
// own kernel says little: much of it is still waiting there.
func uploadRate(ns, url string, received *atomic.Int64, d time.Duration) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, endless{ctx})
	req.ContentLength = -1
	before := received.Load()
	start := time.Now()
	res, err := nsClient(ns).Do(req)
	if res != nil {
		res.Body.Close()
	}
	got := received.Load() - before
	if got == 0 {
		return 0, fmt.Errorf("nothing arrived: %v", err)
	}
	return float64(got) * 8 / time.Since(start).Seconds() / 1e6, nil
}

func smoothing(t *testing.T, r *rig) {
	if why := noShaping(); why != "" {
		t.Skip(why)
	}
	const (
		downKbps, upKbps = 8000, 4000
		// The sites in this rig are a few metres of virtual cable away:
		// unshaped, a transfer runs at hundreds of Mbit/s or more. So the
		// band a shaped one has to land in can be wide and still tell
		// the two apart: from a third of the rate set to a little over
		// one and a half times it.
		low, high = 1.0 / 3, 1.6
		// Unshaped, it has to be at least this many times the rate set,
		// or the test could not tell anything.
		apart = 4
	)
	setEth := func(s config.Shaping) { r.port(t, func(e *config.EthPort) { e.Shaping = s }) }
	setPhone := func(s config.Shaping) { r.edit(t, func(cfg *config.Config) { cfg.WAN.Phone.Shaping = s }) }
	defer func() {
		try("ip -n %s link del usb0", nsRtr)
		setEth(config.Shaping{})
		setPhone(config.Shaping{})
	}()
	mark := r.logMark()
	tc := "ip netns exec " + nsRtr + " tc "
	defer r.explain(t, mark, nil, append([]string{
		tc + "-s qdisc show", tc + "-s filter show dev eth0 ingress", tc + "-s filter show dev usb0 ingress",
	}, showRouter...)...)

	// A site of this test's own, which sends for as long as it is asked
	// and counts what it is sent.
	var received atomic.Int64
	const site = "http://" + siteA + ":8091"
	go http.Serve(listen(t, nsISP, siteA+":8091"), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/down":
			chunk := make([]byte, 32*1024)
			for {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		case "/up":
			buf := make([]byte, 32*1024)
			for {
				n, err := req.Body.Read(buf)
				received.Add(int64(n))
				if err != nil {
					return
				}
			}
		}
	}))
	// measure runs one measurement and, if the result is outside the band
	// (ok says), once more: the machines these tests run on are shared
	// and do not always give a transfer its time.
	measure := func(what string, run func() (float64, error), ok func(mbps float64) bool) float64 {
		t.Helper()
		var mbps float64
		var err error
		for attempt := 1; attempt <= 2; attempt++ {
			mbps, err = run()
			if err == nil && ok(mbps) {
				t.Logf("%s: %.1f Mbit/s", what, mbps)
				return mbps
			}
			t.Logf("%s, attempt %d: %.1f Mbit/s (%v)", what, attempt, mbps, err)
		}
		t.Fatalf("%s: %.1f Mbit/s (%v), which is not what was expected of it, twice", what, mbps, err)
		return 0
	}
	// Held to a speed, a transfer is watched for a few seconds; running
	// free, two are enough to see that it is.
	down := func() (float64, error) { return downloadRate(nsC1, site+"/down", 4*time.Second) }
	up := func() (float64, error) { return uploadRate(nsC1, site+"/up", &received, 4*time.Second) }
	freeDown := func() (float64, error) { return downloadRate(nsC1, site+"/down", 2*time.Second) }
	freeUp := func() (float64, error) { return uploadRate(nsC1, site+"/up", &received, 2*time.Second) }
	within := func(kbps int) func(float64) bool {
		return func(mbps float64) bool {
			return mbps >= float64(kbps)/1000*low && mbps <= float64(kbps)/1000*high
		}
	}
	unshaped := func(kbps int) func(float64) bool {
		return func(mbps float64) bool { return mbps >= float64(kbps)/1000*apart }
	}

	// Before anything is set: nothing is shaped, nothing says it is, and
	// transfers run as fast as the rig lets them.
	eventually(t, "the first device to be online", 30*time.Second, func() bool { return reaches(nsC1, siteA) != "" })
	if _, listed := r.shaped(t, "eth0"); listed {
		t.Fatal("the status lists a source as shaped before any is")
	}
	if _, shaping, ingress := cake("eth0"); shaping || ingress {
		t.Fatal("the internet port has queues of the router's before any are asked for")
	}
	measure("a download with nothing shaped", freeDown, unshaped(downKbps))
	measure("an upload with nothing shaped", freeUp, unshaped(upKbps))

	// Switched on: a queue for what leaves on the port itself, one for
	// what arrives on the port's stand-in, and everything that arrives
	// redirected there. Both count by device behind the router's address
	// translation, each in its own direction.
	setEth(config.Shaping{Enabled: true, DownKbps: downKbps, UpKbps: upKbps})
	eventually(t, "the status to say the internet port is held to its speeds", 15*time.Second, func() bool {
		s, _ := r.shaped(t, "eth0")
		return s.State == "active" && s.Key == "eth:eth0" && s.Down.Kbps == downKbps && s.Up.Kbps == upKbps
	})
	bytesPerSecond := func(kbps int) string { return fmt.Sprint(kbps * 1000 / 8) }
	out, shaping, ingress := cake("eth0")
	if !shaping || !ingress {
		t.Fatalf("the internet port: a queue for what leaves: %v, one for what arrives: %v", shaping, ingress)
	}
	if o := out.Options; string(o.Bandwidth) != bytesPerSecond(upKbps) || o.Flowmode != "dual-srchost" || !o.NAT || o.Ingress || o.Overhead != 38 {
		t.Errorf("the queue for what leaves: %+v", out)
	}
	in, shaping, _ := cake("ifb-eth0")
	if !shaping {
		t.Fatal("the stand-in interface has no queue")
	}
	if o := in.Options; string(o.Bandwidth) != bytesPerSecond(downKbps) || o.Flowmode != "dual-dsthost" || !o.NAT || !o.Ingress || o.Diffserv != "besteffort" || o.Overhead != 38 {
		t.Errorf("the queue for what arrives: %+v", in)
	}
	if filter := output("%sfilter show dev eth0 ingress", tc); !strings.Contains(filter, "mirred") || !strings.Contains(filter, "ifb-eth0") {
		t.Errorf("what arrives on the internet port is not redirected to its stand-in:\n%s", filter)
	}

	// And it holds: a device's download and upload through the router
	// run near the rates set, where a moment ago they ran free.
	measure("a download with the source held to 8 Mbit/s", down, within(downKbps))
	measure("an upload with the source held to 4 Mbit/s", up, within(upKbps))
	// The router had to make the sender slow down, and says how often.
	eventually(t, "the status to count packets dropped or marked to slow a download down", 20*time.Second, func() bool {
		s, _ := r.shaped(t, "eth0")
		return s.Down.Dropped+s.Down.Marked > 0
	})

	// Measuring the source's speed: for as long as that takes the source
	// is not held to anything, says so, and what is measured is the line
	// and not the setting. Afterwards it is held again.
	type measured struct {
		DownMbps float64 `json:"down_mbps"`
		Error    string  `json:"error"`
	}
	done := make(chan error, 1)
	var res measured
	go func() { done <- r.c.Call("speedtest.run", map[string]string{"via": "source:eth:eth0"}, &res) }()
	sawMeasuring := false
	for running := true; running; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("measuring the internet port: %v", err)
			}
			running = false
		case <-time.After(100 * time.Millisecond):
			s, _ := r.shaped(t, "eth0")
			sawMeasuring = sawMeasuring || s.State == "measuring"
		}
	}
	if res.Error != "" || !unshaped(downKbps)(res.DownMbps) {
		t.Errorf("measured through the source while it is held to %d kbit/s: %+v; the measurement has to see the line itself", downKbps, res)
	}
	if !sawMeasuring {
		t.Error("the status never said the source was being measured")
	}
	if s, _ := r.shaped(t, "eth0"); s.State != "active" {
		t.Errorf("after the measurement: %+v", s)
	}
	if _, shaping, ingress := cake("eth0"); !shaping || !ingress {
		t.Errorf("after the measurement the queues are not back (leaving: %v, arriving: %v)", shaping, ingress)
	}
	var hist []struct {
		Via string `json:"via"`
	}
	if err := r.c.Call("speedtest.history", nil, &hist); err != nil {
		t.Fatal(err)
	}
	for _, h := range hist {
		if strings.HasPrefix(h.Via, "source:") {
			t.Errorf("a measurement for the setting is kept among the speed tests: %+v", hist)
		}
	}
	if err := r.c.Call("speedtest.run", map[string]string{"via": "source:phone"}, nil); err == nil {
		t.Error("the speed of a phone that is not plugged in was measured")
	}

	// Other rates: the queues are changed where they are.
	setEth(config.Shaping{Enabled: true, DownKbps: 2 * downKbps, UpKbps: 2 * upKbps})
	eventually(t, "both queues to have the new rates", 15*time.Second, func() bool {
		out, _, ingress := cake("eth0")
		in, _, _ := cake("ifb-eth0")
		return ingress && string(out.Options.Bandwidth) == bytesPerSecond(2*upKbps) && string(in.Options.Bandwidth) == bytesPerSecond(2*downKbps)
	})

	// Queues that have gone, as they do with an interface that is
	// unplugged and plugged in again, are put back.
	sh(t, "%sqdisc del dev eth0 root && %sqdisc del dev eth0 ingress", tc, tc)
	eventually(t, "the router to put back queues that were removed", 20*time.Second, func() bool {
		_, shaping, ingress := cake("eth0")
		return shaping && ingress
	})
	sh(t, "%sqdisc del dev eth0 ingress && ip -n %s link del ifb-eth0", tc, nsRtr)
	eventually(t, "the router to put back a stand-in interface that was removed", 20*time.Second, func() bool {
		_, in, _ := cake("ifb-eth0")
		_, _, ingress := cake("eth0")
		return in && ingress
	})
	measure("a download with the source held to 16 Mbit/s, after its queues were put back", down, within(2*downKbps))

	// A phone on USB has the setting too, and comes and goes: its queues
	// are there while it is, its stand-in is taken away when it is
	// unplugged, and plugged in again it is held as before.
	plug := func() { sh(t, "ip -n %s link add usb0 type veth peer name pt-ph netns %s", nsRtr, nsISP) }
	setPhone(config.Shaping{Enabled: true, DownKbps: 20000, UpKbps: 5000})
	plug()
	phoneHeld := func() bool {
		s, _ := r.shaped(t, "usb0")
		out, shaping, ingress := cake("usb0")
		in, standIn, _ := cake("ifb-usb0")
		return s.State == "active" && s.Key == "phone" && shaping && ingress && standIn &&
			string(out.Options.Bandwidth) == bytesPerSecond(5000) && string(in.Options.Bandwidth) == bytesPerSecond(20000)
	}
	eventually(t, "a phone that is plugged in to be held to its speeds", 20*time.Second, phoneHeld)
	sh(t, "ip -n %s link del usb0", nsRtr)
	eventually(t, "the phone's stand-in interface to go once the phone is unplugged", 20*time.Second, func() bool {
		_, listed := r.shaped(t, "usb0")
		return !listed && exec.Command("ip", "-n", nsRtr, "link", "show", "ifb-usb0").Run() != nil
	})
	if s, _ := r.shaped(t, "eth0"); s.State != "active" {
		t.Errorf("the internet port after the phone went: %+v", s)
	}
	plug()
	eventually(t, "the phone to be held to its speeds again when it is back", 20*time.Second, phoneHeld)
	sh(t, "ip -n %s link del usb0", nsRtr)
	setPhone(config.Shaping{})

	// Switched off: every trace goes, and transfers run free again.
	setEth(config.Shaping{DownKbps: 2 * downKbps, UpKbps: 2 * upKbps})
	eventually(t, "the queues and the stand-in interfaces to go once nothing is held", 20*time.Second, func() bool {
		_, shaping, ingress := cake("eth0")
		_, listed := r.shaped(t, "eth0")
		left := output("ip -n %s -o link show type ifb", nsRtr)
		return !shaping && !ingress && !listed && !strings.Contains(left, "ifb-")
	})
	measure("a download with shaping switched off again", freeDown, unshaped(downKbps))
}
