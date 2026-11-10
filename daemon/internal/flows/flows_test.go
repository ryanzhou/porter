package flows

import (
	"net"
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func ct(proto uint8, src string, sport uint16, dst string, dport uint16, replySrc string, up, down uint64, mark uint32) *netlink.ConntrackFlow {
	return &netlink.ConntrackFlow{
		Forward: netlink.IPTuple{SrcIP: net.ParseIP(src), DstIP: net.ParseIP(dst), SrcPort: sport, DstPort: dport, Protocol: proto, Bytes: up},
		Reverse: netlink.IPTuple{SrcIP: net.ParseIP(replySrc), DstIP: net.ParseIP("100.64.0.9"), SrcPort: dport, DstPort: sport, Protocol: proto, Bytes: down},
		Mark:    mark,
	}
}

func TestTracker(t *testing.T) {
	lan := netip.MustParsePrefix("10.83.0.0/24")
	ula := netip.MustParsePrefix("fd00:1::/64")
	tr := New(Hooks{
		LAN:       func(a netip.Addr) bool { return lan.Contains(a) || ula.Contains(a) },
		LookupMAC: func(a netip.Addr) string { return "mac-" + a.String() },
		NameFor: func(a netip.Addr) string {
			if a.String() == "198.51.100.10" {
				return "www.example.com"
			}
			return ""
		},
		Country: func(a netip.Addr) string { return "JP" },
		Path: func(m uint32) string {
			if m&0xff == 0x10 {
				return "vpn:home"
			}
			return "direct"
		},
	})
	table := []*netlink.ConntrackFlow{
		ct(unix.IPPROTO_TCP, "10.83.0.50", 40000, "198.51.100.10", 443, "198.51.100.10", 1000, 90000, 0x01),
		ct(unix.IPPROTO_UDP, "10.83.0.51", 40001, "203.0.113.5", 51820, "203.0.113.5", 10, 20, 0x10),
		ct(unix.IPPROTO_TCP, "fd00:1::50", 40002, "2001:db8::1", 443, "2001:db8::1", 5, 6, 0x01),
		// Not flows of interest: intercepted DNS (answered by the router),
		// LAN to LAN, and the router's own traffic.
		ct(unix.IPPROTO_UDP, "10.83.0.50", 40003, "8.8.8.8", 53, "10.83.0.1", 1, 1, 0x01),
		ct(unix.IPPROTO_TCP, "10.83.0.50", 40004, "10.83.0.60", 22, "10.83.0.60", 1, 1, 0),
		ct(unix.IPPROTO_UDP, "100.64.0.9", 40005, "1.1.1.1", 53, "1.1.1.1", 1, 1, 0),
	}
	tr.list = func() ([]*netlink.ConntrackFlow, error) { return table, nil }
	if err := tr.Poll(); err != nil {
		t.Fatal(err)
	}
	all := tr.List("")
	if len(all) != 3 {
		t.Fatalf("flows: %+v", all)
	}
	top := all[0]
	if top.Dst != "198.51.100.10" || top.Port != 443 || top.Proto != "tcp" || top.Name != "www.example.com" ||
		top.Country != "JP" || top.Path != "direct" || top.Up != 1000 || top.Down != 90000 || !top.Active || top.MAC != "mac-10.83.0.50" {
		t.Errorf("busiest flow: %+v", top)
	}
	if one := tr.List("mac-10.83.0.51"); len(one) != 1 || one[0].Path != "vpn:home" {
		t.Errorf("per-device list: %+v", one)
	}

	// The first connection ends; counters of the others move on.
	table = table[1:]
	table[0].Forward.Bytes = 500
	if err := tr.Poll(); err != nil {
		t.Fatal(err)
	}
	all = tr.List("")
	if len(all) != 3 {
		t.Fatalf("after close: %+v", all)
	}
	last := all[len(all)-1]
	if last.Active || last.Dst != "198.51.100.10" || last.Down != 90000 {
		t.Errorf("ended flow should be kept with its final counters: %+v", last)
	}
	if all[0].Up != 500 || !all[0].Active {
		t.Errorf("active flow not updated: %+v", all[0])
	}
}
