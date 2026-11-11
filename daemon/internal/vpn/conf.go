package vpn

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// parseConf reads the [Interface]/[Peer] format used by wg-quick and by
// every VPN provider's WireGuard export. Keys that only make sense for
// wg-quick on a single host (Table, PreUp, ...) are ignored.
func parseConf(text string) (config.WireGuard, error) {
	var wg config.WireGuard
	section := ""
	var peer *config.WGPeer
	flush := func() {
		if peer != nil {
			wg.Peers = append(wg.Peers, *peer)
			peer = nil
		}
	}
	list := func(v string) []string {
		var out []string
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
		return out
	}
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			section = strings.ToLower(strings.Trim(line, "[]"))
			if section == "peer" {
				peer = &config.WGPeer{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return wg, fmt.Errorf("line %d: expected key = value", n+1)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch section {
		case "interface":
			switch k {
			case "privatekey":
				wg.PrivateKey = v
			case "address":
				wg.Addresses = append(wg.Addresses, list(v)...)
			case "dns":
				for _, d := range list(v) {
					// wg-quick allows search domains here; keep servers.
					if _, err := netip.ParseAddr(d); err == nil {
						wg.DNS = append(wg.DNS, d)
					}
				}
			case "mtu":
				wg.MTU, _ = strconv.Atoi(v)
			}
		case "peer":
			switch k {
			case "publickey":
				peer.PublicKey = v
			case "presharedkey":
				peer.PresharedKey = v
			case "endpoint":
				peer.Endpoint = v
			case "allowedips":
				peer.AllowedIPs = append(peer.AllowedIPs, list(v)...)
			case "persistentkeepalive":
				peer.Keepalive, _ = strconv.Atoi(v)
			}
		default:
			return wg, fmt.Errorf("line %d: outside of [Interface] or [Peer]", n+1)
		}
	}
	flush()
	if wg.PrivateKey == "" {
		return wg, fmt.Errorf("no PrivateKey in [Interface]")
	}
	if len(wg.Peers) == 0 {
		return wg, fmt.Errorf("no [Peer] section")
	}
	// Lists that are empty are sent as empty lists, which is what the app
	// expects of every list.
	if wg.Addresses == nil {
		wg.Addresses = []string{}
	}
	if wg.DNS == nil {
		wg.DNS = []string{}
	}
	for i := range wg.Peers {
		if wg.Peers[i].AllowedIPs == nil {
			wg.Peers[i].AllowedIPs = []string{}
		}
	}
	return wg, nil
}
