package vpn

import "testing"

func TestParseConf(t *testing.T) {
	wg, err := ParseConf(`
[Interface]
PrivateKey = aGVsbG8gd29ybGQgaGVsbG8gd29ybGQgaGVsbG8gd28=
Address = 10.2.0.2/32, fd00::2/128
DNS = 10.2.0.1, example.internal
MTU = 1380
Table = off   # ignored

[Peer]
# Provider exit
PublicKey = d29ybGQgaGVsbG8gd29ybGQgaGVsbG8gd29ybGQgaGU=
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = vpn.example.net:51820
PersistentKeepalive = 25
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(wg.Addresses) != 2 || wg.MTU != 1380 || len(wg.DNS) != 1 || wg.DNS[0] != "10.2.0.1" {
		t.Errorf("interface: %+v", wg)
	}
	if len(wg.Peers) != 1 || wg.Peers[0].Endpoint != "vpn.example.net:51820" || len(wg.Peers[0].AllowedIPs) != 2 || wg.Peers[0].Keepalive != 25 {
		t.Errorf("peer: %+v", wg.Peers)
	}
	if _, err := ParseConf("[Interface]\nAddress = 10.0.0.2/32\n"); err == nil {
		t.Error("config without key or peer accepted")
	}
	priv, pub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := PublicKey(priv); err != nil || got != pub {
		t.Errorf("public key derivation: %v %v", got, err)
	}
}
