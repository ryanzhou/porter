package mdns

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// lookupInterface returns an interface's index and all of its addresses:
// for a bridge with IPv6 the link-local one, the private one and, where
// the provider delegates a prefix, the one out of that.
func lookupInterface(name string) (int, []netip.Addr, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return 0, nil, err
	}
	var out []netip.Addr
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip)
			}
		}
	}
	return ifi.Index, out, nil
}

// openSocket binds the mDNS port for one address family on every
// interface. Which interfaces the group is listened to on is decided by
// joining it there; what goes out is sent with the hop limit of 255 that
// multicast DNS expects, and is not handed back to the router itself.
func openSocket(v6 bool) (conn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	if v6 {
		c, err := lc.ListenPacket(context.Background(), "udp6", net.JoinHostPort("::", strconv.Itoa(Port)))
		if err != nil {
			return nil, err
		}
		p := ipv6.NewPacketConn(c)
		err = p.SetControlMessage(ipv6.FlagInterface|ipv6.FlagDst, true)
		if err == nil {
			err = p.SetMulticastHopLimit(255)
		}
		if err == nil {
			err = p.SetMulticastLoopback(false)
		}
		if err != nil {
			c.Close()
			return nil, err
		}
		return &sock6{p}, nil
	}
	c, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(Port)))
	if err != nil {
		return nil, err
	}
	p := ipv4.NewPacketConn(c)
	err = p.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst, true)
	if err == nil {
		err = p.SetMulticastTTL(255)
	}
	if err == nil {
		err = p.SetMulticastLoopback(false)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return &sock4{p}, nil
}

// source turns the sender of a datagram into an address and port.
func source(a net.Addr) (netip.AddrPort, bool) {
	u, ok := a.(*net.UDPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ip, ok := netip.AddrFromSlice(u.IP)
	return netip.AddrPortFrom(ip.Unmap(), uint16(u.Port)), ok
}

type sock4 struct{ p *ipv4.PacketConn }

var group4 = &net.UDPAddr{IP: net.IP(Group4.AsSlice()), Port: Port}

func (s *sock4) Join(ifindex int) error {
	return s.p.JoinGroup(&net.Interface{Index: ifindex}, group4)
}

func (s *sock4) Leave(ifindex int) error {
	return s.p.LeaveGroup(&net.Interface{Index: ifindex}, group4)
}

func (s *sock4) Read(buf []byte) (int, int, netip.AddrPort, error) {
	for {
		n, cm, from, err := s.p.ReadFrom(buf)
		if err != nil {
			return 0, 0, netip.AddrPort{}, err
		}
		// The port also receives what is sent to the router's own
		// address, which is not for relaying.
		if src, ok := source(from); ok && cm != nil && cm.Dst.Equal(group4.IP) {
			return n, cm.IfIndex, src, nil
		}
	}
}

func (s *sock4) Write(pkt []byte, ifindex int) error {
	// Naming the interface is enough: the kernel sends from the address
	// the router has there.
	_, err := s.p.WriteTo(pkt, &ipv4.ControlMessage{IfIndex: ifindex}, group4)
	return err
}

func (s *sock4) Close() error { return s.p.Close() }

type sock6 struct{ p *ipv6.PacketConn }

var group6 = &net.UDPAddr{IP: net.IP(Group6.AsSlice()), Port: Port}

func (s *sock6) Join(ifindex int) error {
	return s.p.JoinGroup(&net.Interface{Index: ifindex}, group6)
}

func (s *sock6) Leave(ifindex int) error {
	return s.p.LeaveGroup(&net.Interface{Index: ifindex}, group6)
}

func (s *sock6) Read(buf []byte) (int, int, netip.AddrPort, error) {
	for {
		n, cm, from, err := s.p.ReadFrom(buf)
		if err != nil {
			return 0, 0, netip.AddrPort{}, err
		}
		if src, ok := source(from); ok && cm != nil && cm.Dst.Equal(group6.IP) {
			return n, cm.IfIndex, src, nil
		}
	}
}

func (s *sock6) Write(pkt []byte, ifindex int) error {
	// The kernel sends from the bridge's link-local address, whatever
	// others the bridge has.
	_, err := s.p.WriteTo(pkt, &ipv6.ControlMessage{IfIndex: ifindex}, group6)
	return err
}

func (s *sock6) Close() error { return s.p.Close() }
