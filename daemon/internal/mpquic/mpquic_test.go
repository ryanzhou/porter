package mpquic

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

func TestPathCount(t *testing.T) {
	for line, want := range map[string]int{
		`time=2026-11-10T10:00:00Z level=INFO msg="path up" path=203.0.113.10:8388 count=1`:                                 1,
		`time=2026-11-10T10:00:01Z level=INFO msg="path up" path=relay.example.net:9000 count=3`:                            3,
		`time=2026-11-10T10:00:09Z level=INFO msg="path down" path=203.0.113.10:8388 err="timeout" count=0`:                 0,
		`time=2026-11-10T10:00:09Z level=INFO msg="path down" path=a err="x count=9 y" count=2 extra=1`:                     2,
		`time=2026-11-10T10:00:00Z level=INFO msg="mpquic local started" version=v0.6.0 listen="" wireguard=false tun=true`: 0,
	} {
		if got, ok := pathCount(line); !ok || got != want {
			t.Errorf("%s: got %d (%v), want %d", line, got, ok, want)
		}
	}
	for _, line := range []string{
		`time=2026-11-10T10:00:00Z level=INFO msg="path suspect" path=a unanswered=600ms`,
		`time=2026-11-10T10:00:00Z level=INFO msg=stats streams=3 paths="a count=2"`,
		`time=2026-11-10T10:00:00Z level=INFO msg="path up" path=a`,
		``,
	} {
		if n, ok := pathCount(line); ok {
			t.Errorf("%q was read as a path count of %d", line, n)
		}
	}
}

func TestEffective(t *testing.T) {
	m := &Manager{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), RunDir: t.TempDir()}
	vpns := []config.VPN{
		{ID: "wg", Name: "Plain", Enabled: true, Type: config.VPNWireGuard, WireGuard: config.WireGuard{PrivateKey: "k", Addresses: []string{"10.0.0.2/32"}}},
		{ID: "mq", Name: "Multi", Enabled: true, KillSwitch: true, Type: config.VPNMPQUIC, MPQUIC: config.MPQUIC{
			Servers: []string{"203.0.113.10", "relay.example.net:9000"}, Port: 8388, Password: "long-random-password",
			DNS: []string{"1.1.1.1"}, Up: "19mbit", ReliableUDP: true,
		}},
		{ID: "off", Name: "Off", Enabled: false, Type: config.VPNMPQUIC, MPQUIC: config.MPQUIC{Servers: []string{"a.example"}, Port: 8388, Password: "long-random-password"}},
	}
	// No mpquic program anywhere: the profile has a place but cannot run.
	m.Apply(vpns)
	if paths, managed := m.Health("mq"); !managed || paths != 0 {
		t.Errorf("health without a program: %d %v", paths, managed)
	}
	if _, managed := m.Health("wg"); managed {
		t.Error("a WireGuard profile is treated as MPQUIC")
	}
	if _, managed := m.Health("off"); managed {
		t.Error("a disabled profile is running")
	}

	eff := Effective(vpns)
	if len(eff) != 3 || eff[0].WireGuard.PrivateKey != "k" || !eff[0].Enabled {
		t.Fatalf("the WireGuard profile was changed: %+v", eff[0])
	}
	if eff[2].Enabled {
		t.Error("the disabled profile came out enabled")
	}
	// The interface of an MPQUIC profile: addresses to send from, the
	// profile's DNS servers, full-size packets, and nothing of WireGuard.
	w := eff[1].WireGuard
	if !eff[1].Enabled || !eff[1].KillSwitch || len(w.Peers) != 0 || w.PrivateKey != "" || w.MTU != 1500 ||
		strings.Join(w.Addresses, " ") != "10.66.1.2/32 fd66:1::2/128" || strings.Join(w.DNS, " ") != "1.1.1.1" {
		t.Errorf("the interface of the MPQUIC profile: %+v", eff[1])
	}
	// The stored profiles are untouched.
	if len(vpns[1].WireGuard.Addresses) != 0 {
		t.Error("the stored profile was given interface settings")
	}

	// What mpquic is told: the interface to attach to, and nothing about
	// addresses or routes, which are the router's.
	m.mu.Lock()
	in := m.inst["mq"]
	m.mu.Unlock()
	raw, _ := json.Marshal(file(vpns[1]))
	var f struct {
		Server     []string       `json:"server"`
		ServerPort int            `json:"server_port"`
		Mode       string         `json:"mode"`
		NoSOCKS    bool           `json:"no_socks"`
		Up         string         `json:"up"`
		Reliable   bool           `json:"reliable_udp"`
		TUN        map[string]any `json:"tun"`
		WireGuard  any            `json:"wireguard"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Server) != 2 || f.ServerPort != 8388 || f.Mode != "tcp_and_udp" || !f.NoSOCKS || f.Up != "19mbit" || !f.Reliable ||
		f.WireGuard != nil || len(f.TUN) != 2 || f.TUN["name"] != "wg-mq" || f.TUN["mtu"] != float64(1500) {
		t.Errorf("mpquic's configuration: %s", raw)
	}
	// Other DNS servers for the profile are no reason to restart mpquic.
	other := vpns[1]
	other.MPQUIC.DNS = []string{"9.9.9.9"}
	if spec(other) != spec(vpns[1]) {
		t.Error("the DNS servers are part of what mpquic is started with")
	}
	other.MPQUIC.Up = "5mbit"
	if spec(other) == spec(vpns[1]) {
		t.Error("a changed rate is not")
	}

	// Paths coming and going, as mpquic reports them.
	changes := 0
	m.OnChange = func() { changes++ }
	m.onLine("mq", in, `level=INFO msg="path up" path=a count=1`)
	m.onLine("mq", in, `level=INFO msg="path up" path=b count=2`)
	if paths, _ := m.Health("mq"); paths != 2 || changes != 1 {
		t.Errorf("after two paths: %d paths, %d changes", paths, changes)
	}
	m.onLine("mq", in, `level=INFO msg="path down" path=a err=x count=1`)
	m.onLine("mq", in, `level=INFO msg="path down" path=b err=x count=0`)
	if paths, _ := m.Health("mq"); paths != 0 || changes != 2 {
		t.Errorf("after losing both: %d paths, %d changes", paths, changes)
	}

	// Removing the profile stops it.
	m.Apply(vpns[:1])
	if _, managed := m.Health("mq"); managed {
		t.Error("still there after the profile was removed")
	}
}
