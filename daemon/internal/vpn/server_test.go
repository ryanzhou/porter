package vpn

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// The configuration package makes the server's key and derives its public
// half without WireGuard's own code; both have to agree with it.
func TestServerKey(t *testing.T) {
	c := config.Default()
	priv := c.Inbound.Server.PrivateKey
	want, err := PublicKey(priv)
	if err != nil {
		t.Fatalf("the server's key is not one WireGuard takes: %v", err)
	}
	if got := c.Inbound.Server.PublicKey(); got != want {
		t.Errorf("public key %q, WireGuard derives %q", got, want)
	}
	k, _ := key(priv)
	if k[0]&7 != 0 || k[31]&0x80 != 0 || k[31]&0x40 == 0 {
		t.Errorf("the key is not in the form WireGuard keeps keys in: %x", k[:])
	}
	for i := 0; i < 20; i++ {
		p, pub, _ := GenerateKey()
		if got, err := config.WGPublicKey(p); err != nil || got != pub {
			t.Fatalf("public key of %s: %q %v, want %q", p, got, err, pub)
		}
	}
}

// What a device is given has to be a configuration: the WireGuard apps
// read the same format this package's own parser does.
func TestClientConf(t *testing.T) {
	priv, _, _ := GenerateKey()
	_, serverPub, _ := GenerateKey()
	pfx := netip.MustParsePrefix
	conf := ClientConf{
		PrivateKey: priv,
		Addrs:      []netip.Prefix{pfx("10.89.0.2/32"), pfx("fd00:0:0:89::2/128")},
		DNS:        []netip.Addr{netip.MustParseAddr("10.89.0.1"), netip.MustParseAddr("fd00:0:0:89::1")},
		ServerKey:  serverPub, Endpoint: "home.example.com:51820",
		Allowed: []netip.Prefix{pfx("0.0.0.0/0"), pfx("::/0")},
	}
	text := conf.String()
	wg, err := ParseConf(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if wg.PrivateKey != priv || strings.Join(wg.Addresses, " ") != "10.89.0.2/32 fd00:0:0:89::2/128" || strings.Join(wg.DNS, " ") != "10.89.0.1 fd00:0:0:89::1" || len(wg.Peers) != 1 {
		t.Fatalf("read back: %+v\n%s", wg, text)
	}
	p := wg.Peers[0]
	if p.PublicKey != serverPub || p.Endpoint != "home.example.com:51820" || strings.Join(p.AllowedIPs, " ") != "0.0.0.0/0 ::/0" || p.Keepalive != 25 {
		t.Errorf("peer read back: %+v", p)
	}
	// And a profile the router itself would accept, as a client of
	// another router like it.
	c := config.Default()
	c.VPNs = []config.VPN{{ID: "home", Name: "Home", Enabled: true, WireGuard: wg}}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Errorf("the configuration as a profile: %v", err)
	}
	// An IPv6 endpoint is written the way an address with a port is.
	conf.Endpoint = "[2001:db8::1]:51820"
	if wg, err := ParseConf(conf.String()); err != nil || wg.Peers[0].Endpoint != "[2001:db8::1]:51820" {
		t.Errorf("IPv6 endpoint: %+v %v", wg, err)
	}
}
