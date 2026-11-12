package core

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/devices"
	"github.com/ryanzhou/porter/daemon/internal/vpn"
)

// The configuration a device is given, read back the way a WireGuard app
// reads it.
func TestClientConf(t *testing.T) {
	cfg := config.Default()
	cfg.Networks = append(cfg.Networks, config.Network{ID: "guest", Name: "Guest", Address: "10.84.0.1/24", ULAPrefix: "fd00:0:0:84::/64", Isolated: true})
	cfg.Networks[0].ULAPrefix = "fd00:0:0:83::/64"
	s := &cfg.Inbound.Server
	s.Enabled, s.Port, s.Address, s.ULAPrefix = true, 51000, "10.89.0.1/24", "fd00:0:0:89::/64"
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	priv, pub, _ := vpn.GenerateKey()
	peer := config.WGServerPeer{ID: "p1", Name: "Phone", PublicKey: pub, Address: "10.89.0.7", Enabled: true}

	// Only what is for the networks at home, the kept-apart one included,
	// and for the server's own range, where the router's DNS is.
	text := ClientConf(cfg, peer, priv, "home.example.com").String()
	wg, err := vpn.ParseConf(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	got := strings.Join(wg.Peers[0].AllowedIPs, " ")
	if got != "10.89.0.0/24 10.83.0.0/24 10.84.0.0/24 fd00:0:0:89::/64 fd00:0:0:83::/64 fd00:0:0:84::/64" {
		t.Errorf("what a device sends home: %s", got)
	}
	if wg.PrivateKey != priv || strings.Join(wg.Addresses, " ") != "10.89.0.7/32 fd00:0:0:89::7/128" || strings.Join(wg.DNS, " ") != "10.89.0.1 fd00:0:0:89::1" ||
		wg.Peers[0].PublicKey != s.PublicKey() || wg.Peers[0].Endpoint != "home.example.com:51000" {
		t.Errorf("the device's configuration:\n%s", text)
	}
	// The names are looked up at an address the device has a route to.
	for _, d := range wg.DNS {
		routed := false
		for _, a := range wg.Peers[0].AllowedIPs {
			routed = routed || netip.MustParsePrefix(a).Contains(netip.MustParseAddr(d))
		}
		if !routed {
			t.Errorf("the device is told to ask %s, which it has no route to", d)
		}
	}

	// Everything, to an address: both families, and brackets where an
	// IPv6 address needs them.
	peer.Full = true
	wg, err = vpn.ParseConf(ClientConf(cfg, peer, priv, "2001:db8::1").String())
	if err != nil || strings.Join(wg.Peers[0].AllowedIPs, " ") != "0.0.0.0/0 ::/0" || wg.Peers[0].Endpoint != "[2001:db8::1]:51000" {
		t.Errorf("everything through home: %+v %v", wg, err)
	}

	// With IPv6 off at home the device has no IPv6 address there, and
	// still sends everything: what went past the router would be seen.
	cfg.LAN.IPv6 = false
	wg, _ = vpn.ParseConf(ClientConf(cfg, peer, priv, "203.0.113.7").String())
	if strings.Join(wg.Addresses, " ") != "10.89.0.7/32" || strings.Join(wg.DNS, " ") != "10.89.0.1" || strings.Join(wg.Peers[0].AllowedIPs, " ") != "0.0.0.0/0 ::/0" || wg.Peers[0].Endpoint != "203.0.113.7:51000" {
		t.Errorf("without IPv6: %+v", wg)
	}
	peer.Full = false
	wg, _ = vpn.ParseConf(ClientConf(cfg, peer, priv, "203.0.113.7").String())
	if got := strings.Join(wg.Peers[0].AllowedIPs, " "); got != "10.89.0.0/24 10.83.0.0/24 10.84.0.0/24" {
		t.Errorf("without IPv6, what a device sends home: %s", got)
	}
}

func TestForwardAddrs6(t *testing.T) {
	cfg := config.Default()
	cfg.Inbound.Forwards = []config.PortForward{{Device: "02:00:00:00:00:10"}, {Device: "02:00:00:00:00:10"}, {Device: "02:00:00:00:00:11"}}
	devs := []devices.Entry{
		{MAC: "02:00:00:00:00:10", IPv6: []string{"2001:db8:1::10", "fd00::10"}},
		{MAC: "02:00:00:00:00:12", IPv6: []string{"2001:db8:1::12"}},
	}
	got := forwardAddrs6(cfg, devs)
	if len(got) != 1 || len(got["02:00:00:00:00:10"]) != 2 {
		t.Errorf("addresses of the devices ports are forwarded to: %v", got)
	}
	if forwardedTo(cfg, "02:00:00:00:00:12") != "" {
		t.Error("a device without a forward is said to have one")
	}
}
