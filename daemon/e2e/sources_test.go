//go:build e2e

package e2e

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Routes through a chosen internet source, and sources with a data limit,
// as subtests of TestRouter; and, for the tests of what comes in from
// outside, a second internet source to come in by.

const (
	// The second source's own network, beside the wired one's
	// (100.64.0.0/24) and the one the device away is on (100.66.0.0/24).
	phoneGW   = "100.67.0.1"
	phoneGW6  = "2001:db8:300::1"
	phoneNet6 = "2001:db8:300:"
	// What the second source's own resolver says of a name the wired
	// source's resolver says something else about (203.0.113.1).
	phoneSays = "203.0.113.3"
)

// dataView is what the status says of an internet source's data limit.
type dataView struct {
	Key     string `json:"key"`
	Period  string `json:"period"`
	Limit   uint64 `json:"limit"`
	Used    uint64 `json:"used"`
	Since   int64  `json:"since"`
	Resets  int64  `json:"resets"`
	Reached bool   `json:"reached"`
	Lifted  bool   `json:"lifted"`
}

// data returns how the data limit of the source with the given key
// stands, and the router's time.
func (r *rig) data(t *testing.T, key string) (dataView, int64) {
	t.Helper()
	var st struct {
		Time int64      `json:"time"`
		Data []dataView `json:"data"`
	}
	if err := r.c.Call("status.get", nil, &st); err != nil {
		t.Fatalf("status.get: %v", err)
	}
	for _, d := range st.Data {
		if d.Key == key {
			return d, st.Time
		}
	}
	return dataView{}, st.Time
}

// plugPhone gives the router a second internet source, a stand-in for a
// phone on USB: an interface the router takes for one, on a network of
// its own with its own DHCP, its own resolver and, where there is IPv6,
// its own prefix by router advertisement. It waits until the router has
// it online behind the wired source, and returns the router's address on
// it and what unplugs it again.
func (r *rig) plugPhone(t *testing.T) (addr string, unplug func()) {
	t.Helper()
	try("ip -n %s link del usb0", nsRtr)
	sh(t, "ip -n %s link add usb0 type veth peer name isp2 netns %s", nsRtr, nsISP)
	sh(t, "ip -n %s addr add %s/24 dev isp2 && ip -n %s link set isp2 up", nsISP, phoneGW, nsISP)
	lines := []string{
		"interface=isp2", "except-interface=lo", "bind-interfaces", "no-resolv", "no-hosts", "user=root", "keep-in-foreground",
		"pid-file=" + filepath.Join(r.dir, "phone2-dnsmasq.pid"),
		"dhcp-range=100.67.0.50,100.67.0.99,255.255.255.0,10m", "dhcp-option=option:router," + phoneGW,
		"dhcp-option=option:dns-server," + phoneGW, "dhcp-leasefile=" + filepath.Join(r.dir, "phone2.leases"),
		"address=/probe.test/" + siteA, "address=/direct.test/" + siteA, "address=/dnspath.test/" + phoneSays,
	}
	if r.v6 {
		sh(t, "ip -n %s addr add %s/64 dev isp2 nodad", nsISP, phoneGW6)
		lines = append(lines, "enable-ra", "dhcp-range=::,constructor:isp2,ra-only,10m")
	}
	conf := filepath.Join(r.dir, "phone2-dnsmasq.conf")
	os.WriteFile(conf, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	dnsmasq := r.start("second source dnsmasq", "phone2-dnsmasq.log", "ip", "netns", "exec", nsISP, "dnsmasq", "--conf-file="+conf)
	unplug = func() {
		try("ip -n %s link del usb0", nsRtr)
		if dnsmasq.Process != nil {
			dnsmasq.Process.Kill()
		}
	}
	eventually(t, "the second internet source online, behind the wired one", 60*time.Second, func() bool {
		src, st := r.source(t, "phone")
		addr = strings.Split(src.IPv4, "/")[0]
		return src.If == "usb0" && src.Internet == "online" && addr != "" && st.Active == "eth0" && (!r.v6 || src.HasV6)
	})
	return addr, unplug
}

// strictISP makes the upstream network refuse a packet that arrives from
// the router by the wrong one of its two sources: with the address of one
// and through the cable of the other. That is what a provider does with
// such a packet, and without it a test of answers leaving by the source
// the connection came in by would pass whichever way they left.
func strictISP(t *testing.T) (restore func()) {
	t.Helper()
	was := map[string]string{}
	for _, name := range []string{"all", "isp0", "isp2"} {
		was[name] = strings.TrimSpace(output("ip netns exec %s sysctl -n net.ipv4.conf.%s.rp_filter", nsISP, name))
		sh(t, "ip netns exec %s sysctl -qw net.ipv4.conf.%s.rp_filter=1", nsISP, name)
	}
	return func() {
		for name, v := range was {
			try("ip netns exec %s sysctl -qw net.ipv4.conf.%s.rp_filter=%s", nsISP, name, v)
		}
	}
}

// getFrom fetches a URL from inside a namespace, from one of the
// namespace's own addresses.
func getFrom(ns, local, url string) (string, error) {
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (c net.Conn, err error) {
			d := &net.Dialer{Timeout: 3 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(local)}}
			inNS(ns, func() { c, err = d.DialContext(ctx, "tcp", addr) })
			return
		},
	}
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return strings.TrimSpace(string(b)), nil
}

var showSources = []string{
	"ip netns exec " + nsRtr + " nft list table inet porter",
	"ip -n " + nsRtr + " addr show", "ip -n " + nsRtr + " rule show", "ip -n " + nsRtr + " route show table all",
	"ip -n " + nsRtr + " -6 rule show", "ip -n " + nsRtr + " -s link show usb0",
	"ip -n " + nsISP + " addr show", "ip -n " + nsISP + " route show",
}

func sourceRoutes(t *testing.T, r *rig) {
	const router = "10.83.0.1"
	mark := r.logMark()
	wanIP := r.wanAddr(t)
	var before config.Config
	if err := r.c.Call("config.get", nil, &before); err != nil {
		t.Fatal(err)
	}
	phoneIP, unplug := r.plugPhone(t)
	defer func() {
		unplug()
		// The router ends as it began, whatever became of the test, and
		// with the time it is.
		r.setClock(t, time.Now())
		r.edit(t, func(cfg *config.Config) {
			cfg.Devices, cfg.Policy, cfg.WAN.Phone, cfg.WAN.Order = before.Devices, before.Policy, before.WAN.Phone, before.WAN.Order
		})
	}()
	defer r.explain(t, mark, []string{"phone2-dnsmasq.log"}, showSources...)

	route := func(mac, action string) {
		t.Helper()
		r.edit(t, func(cfg *config.Config) {
			for i := range cfg.Devices {
				if cfg.Devices[i].MAC == mac {
					cfg.Devices[i].Route = action
					return
				}
			}
			cfg.Devices = append(cfg.Devices, config.Device{MAC: mac, Route: action})
		})
	}
	says := func(ns string) string {
		a, _, err := lookup(ns, router, "dnspath.test")
		if err != nil {
			return ""
		}
		return a
	}
	refusedLookup := func(ns string) bool {
		_, rcode, err := lookup(ns, router, "dnspath.test")
		return err == nil && rcode == 5 // REFUSED
	}

	// One device is routed through the second source and leaves by it,
	// with its lookups; the other leaves by the source in use as before.
	route(r.mac1, "source:phone")
	eventually(t, "the device routed through the second source leaving by it", 20*time.Second, func() bool {
		return reaches(nsC1, siteA) == phoneIP
	})
	if got := reaches(nsC2, siteA); got != wanIP {
		t.Errorf("the device without a route left from %q, want the wired source's %s", got, wanIP)
	}
	eventually(t, "the routed device's lookups answered by the second source's resolver", 10*time.Second, func() bool {
		return says(nsC1) == phoneSays
	})
	if got := says(nsC2); got != "203.0.113.1" {
		t.Errorf("the other device's lookup was answered %q, want the wired source's resolver", got)
	}
	if _, st := r.source(t, "phone"); st.Active != "eth0" {
		t.Errorf("a route through the second source made it the source in use: %q", st.Active)
	}
	if r.v6 {
		// Over IPv6 as well: masqueraded to the address the router has
		// there, whichever addresses the device holds.
		eventually(t, "the routed device leaving by the second source over IPv6", 40*time.Second, func() bool {
			return strings.HasPrefix(reaches(nsC1, site6), phoneNet6)
		})
		if got := reaches(nsC2, site6); strings.HasPrefix(got, phoneNet6) || got == "" {
			t.Errorf("the device without a route left over IPv6 from %q", got)
		}
	}

	// A rule for everybody, to one site, by the second source alone.
	r.edit(t, func(cfg *config.Config) {
		cfg.Policy.Rules = append(cfg.Policy.Rules, config.Rule{ID: "bysrc", Name: "By source", Enabled: true,
			Dst: config.Dst{CIDRs: []string{siteB}}, Action: "only:phone"})
	})
	eventually(t, "a rule routing one site through the second source", 15*time.Second, func() bool {
		return reaches(nsC2, siteB) == phoneIP && reaches(nsC2, siteA) == wanIP
	})

	// The second source goes away. What prefers it goes by the source in
	// use, lookups included; what is for it alone goes nowhere.
	sh(t, "ip -n %s link set isp2 down", nsISP)
	eventually(t, "the routed device falling back on the source in use", 20*time.Second, func() bool {
		return reaches(nsC1, siteA) == wanIP && says(nsC1) == "203.0.113.1"
	})
	if got := reaches(nsC2, siteB); got != "" {
		t.Errorf("with the second source away, what a rule routes to it alone left from %q", got)
	}
	route(r.mac1, "only:phone")
	eventually(t, "the device for the second source alone held while it is away", 15*time.Second, func() bool {
		return reaches(nsC1, siteA) == "" && refusedLookup(nsC1)
	})
	if got := reaches(nsC2, siteA); got != wanIP {
		t.Errorf("with one device held, the other left from %q", got)
	}
	// Still part of its network, and the router's own names resolve.
	if a, _, err := lookup(nsC1, router, "porter.lan"); err != nil || a != router {
		t.Errorf("a held device asking for the router's own name: %q %v", a, err)
	}
	sh(t, "ip -n %s link set isp2 up", nsISP)
	eventually(t, "the device leaving by the second source once that is back", 60*time.Second, func() bool {
		src, _ := r.source(t, "phone")
		phoneIP = strings.Split(src.IPv4, "/")[0]
		return phoneIP != "" && reaches(nsC1, siteA) == phoneIP && reaches(nsC2, siteB) == phoneIP
	})

	// A data limit of one megabyte a day on the second source. The device
	// uses it up.
	r.edit(t, func(cfg *config.Config) {
		cfg.WAN.Phone.Data = config.DataLimit{Enabled: true, Period: config.PeriodDay, MB: 1}
	})
	eventually(t, "the second source's data being counted", 15*time.Second, func() bool {
		d, _ := r.data(t, "phone")
		return d.Key == "phone" && d.Limit == 1_000_000 && d.Period == "day" && !d.Reached && d.Resets > d.Since
	})
	for i := 0; i < 5; i++ {
		if _, err := httpGet(nsC1, "http://"+siteA+"/big"); err != nil {
			break // cut off in the middle of it: the limit
		}
	}
	eventually(t, "the limit reached, and the device for that source alone offline", 20*time.Second, func() bool {
		d, _ := r.data(t, "phone")
		return d.Reached && !d.Lifted && d.Used >= d.Limit && reaches(nsC1, siteA) == "" && refusedLookup(nsC1)
	})
	if got := reaches(nsC2, siteB); got != "" {
		t.Errorf("past the limit, what a rule routes to the second source alone left from %q", got)
	}
	if got := reaches(nsC2, siteA); got != wanIP {
		t.Errorf("past the second source's limit, a device on the wired source left from %q", got)
	}
	var evs []struct {
		Kind string `json:"kind"`
		Msg  string `json:"msg"`
	}
	if err := r.c.Call("events.list", nil, &evs); err != nil {
		t.Fatal(err)
	}
	told := 0
	for _, e := range evs {
		if e.Kind == "data" && strings.Contains(e.Msg, "has used its 1 MB") {
			told++
		}
	}
	if told != 1 {
		t.Errorf("the limit being reached is in the activity log %d times: %+v", told, evs)
	}
	// What merely prefers the source goes by the one in use.
	route(r.mac1, "source:phone")
	eventually(t, "a device that prefers the spent source falling back", 15*time.Second, func() bool {
		return reaches(nsC1, siteA) == wanIP
	})
	// Ranked first, the source is still not used: stepped past, as one
	// that is down.
	r.edit(t, func(cfg *config.Config) {
		order := []string{"phone"}
		for _, k := range cfg.WAN.Order {
			if k != "phone" {
				order = append(order, k)
			}
		}
		cfg.WAN.Order = order
	})
	for i := 0; i < 4; i++ {
		time.Sleep(2 * time.Second)
		if _, st := r.source(t, "phone"); st.Active != "eth0" || reaches(nsC2, siteA) != wanIP {
			t.Fatalf("a source past its data limit is in use once ranked first: %q", st.Active)
		}
	}
	used, _ := r.data(t, "phone")

	// Lifted until the period ends: in use, and counted on.
	r.edit(t, func(cfg *config.Config) { cfg.WAN.Phone.Data.LiftUntil = used.Resets })
	eventually(t, "the source in use once its limit is lifted", 40*time.Second, func() bool {
		d, _ := r.data(t, "phone")
		_, st := r.source(t, "phone")
		return d.Reached && d.Lifted && st.Active == "usb0" && reaches(nsC2, siteA) == phoneIP && reaches(nsC1, siteA) == phoneIP
	})
	// And taken out of use again when that is taken back.
	r.edit(t, func(cfg *config.Config) { cfg.WAN.Phone.Data.LiftUntil = 0 })
	eventually(t, "the source out of use again", 20*time.Second, func() bool {
		_, st := r.source(t, "phone")
		return st.Active == "eth0" && reaches(nsC2, siteA) == wanIP
	})

	// The next day: nothing used, and in use without anybody doing
	// anything.
	r.setClock(t, time.Unix(used.Resets+60, 0))
	eventually(t, "the source in use again when the next day begins", 40*time.Second, func() bool {
		d, now := r.data(t, "phone")
		_, st := r.source(t, "phone")
		return !d.Reached && d.Used < d.Limit && d.Since == used.Resets && now > used.Resets && st.Active == "usb0" && reaches(nsC1, siteA) == phoneIP
	})

	// Unplugged with a limit set: the count is kept and still shown.
	unplug()
	eventually(t, "the wired source in use with the second one unplugged", 20*time.Second, func() bool {
		d, _ := r.data(t, "phone")
		return r.status(t).Active == "eth0" && d.Key == "phone" && reaches(nsC1, siteA) == wanIP
	})
	if t.Failed() {
		t.Logf("the second source's address was %s, the wired one's %s", phoneIP, wanIP)
	}
}
