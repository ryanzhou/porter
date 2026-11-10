// Package netx wraps the netlink operations the router needs.
package netx

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Exists reports whether an interface is present.
func Exists(name string) bool {
	_, err := netlink.LinkByName(name)
	return err == nil
}

// Up brings an interface up.
func Up(name string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(l)
}

func Down(name string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetDown(l)
}

// HardwareAddr returns the interface's current and permanent addresses.
func HardwareAddr(name string) (current, permanent string, err error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return "", "", err
	}
	current = l.Attrs().HardwareAddr.String()
	permanent = current
	if p := l.Attrs().PermHWAddr; len(p) == 6 {
		permanent = p.String()
	}
	return current, permanent, nil
}

// SetMAC changes an interface's address, taking it down if needed. An
// empty mac restores the permanent address.
func SetMAC(name, mac string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	if mac == "" {
		if p := l.Attrs().PermHWAddr; len(p) == 6 {
			mac = p.String()
		} else {
			return nil
		}
	}
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return err
	}
	if l.Attrs().HardwareAddr.String() == hw.String() {
		return nil
	}
	wasUp := l.Attrs().Flags&net.FlagUp != 0
	if wasUp {
		if err := netlink.LinkSetDown(l); err != nil {
			return err
		}
	}
	err = netlink.LinkSetHardwareAddr(l, hw)
	if wasUp {
		if uerr := netlink.LinkSetUp(l); err == nil {
			err = uerr
		}
	}
	return err
}

// EnsureBridge creates the bridge if missing and brings it up.
func EnsureBridge(name, mac string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if mac != "" {
			if hw, perr := net.ParseMAC(mac); perr == nil {
				br.HardwareAddr = hw
			}
		}
		if err := netlink.LinkAdd(br); err != nil {
			return fmt.Errorf("create bridge: %w", err)
		}
		l, err = netlink.LinkByName(name)
		if err != nil {
			return err
		}
	}
	return netlink.LinkSetUp(l)
}

// SetBridgePorts makes exactly the given interfaces (plus any whose name
// keep() accepts) members of the bridge.
func SetBridgePorts(bridge string, ports []string, keep func(name string) bool) error {
	br, err := netlink.LinkByName(bridge)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, p := range ports {
		want[p] = true
	}
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	var firstErr error
	for _, l := range links {
		name := l.Attrs().Name
		member := l.Attrs().MasterIndex == br.Attrs().Index
		switch {
		case want[name] && !member:
			if err := netlink.LinkSetMaster(l, br); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("add %s to bridge: %w", name, err)
			}
			netlink.LinkSetUp(l)
		case want[name]:
			netlink.LinkSetUp(l)
		case member && !(keep != nil && keep(name)):
			if err := netlink.LinkSetNoMaster(l); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// SyncAddrs makes the interface carry exactly the given global addresses
// of each family present in want (link-local addresses are left alone).
func SyncAddrs(name string, want []netip.Prefix) (changed bool, err error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return false, err
	}
	have, err := netlink.AddrList(l, unix.AF_UNSPEC)
	if err != nil {
		return false, err
	}
	wantSet := map[string]bool{}
	for _, p := range want {
		wantSet[p.String()] = true
	}
	for _, a := range have {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip.IsLinkLocalUnicast() {
			continue
		}
		ones, _ := a.Mask.Size()
		key := netip.PrefixFrom(ip, ones).String()
		if wantSet[key] {
			delete(wantSet, key)
			continue
		}
		a := a
		if err := netlink.AddrDel(l, &a); err != nil {
			return changed, err
		}
		changed = true
	}
	for _, p := range want {
		if !wantSet[p.String()] {
			continue
		}
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}}
		if p.Addr().Is6() {
			addr.Flags = unix.IFA_F_NODAD
		}
		if err := netlink.AddrAdd(l, addr); err != nil && !errors.Is(err, os.ErrExist) {
			return changed, fmt.Errorf("add %s to %s: %w", p, name, err)
		}
		changed = true
	}
	return changed, nil
}

// Addrs returns the interface's global addresses.
func Addrs(name string) (v4, v6 []netip.Prefix) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, nil
	}
	list, _ := netlink.AddrList(l, unix.AF_UNSPEC)
	for _, a := range list {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		ones, _ := a.Mask.Size()
		switch {
		case ip.Is4():
			v4 = append(v4, netip.PrefixFrom(ip, ones))
		case !ip.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DEPRECATED) == 0:
			v6 = append(v6, netip.PrefixFrom(ip, ones))
		}
	}
	return v4, v6
}

// Carrier reports whether the interface has link.
func Carrier(name string) bool {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return false
	}
	return l.Attrs().OperState == netlink.OperUp || (l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0 && l.Attrs().Flags&net.FlagUp != 0)
}

// DefaultRoute reports whether a default route via the interface exists in
// the main table for the family (unix.AF_INET / AF_INET6).
func DefaultRoute(name string, family int) (gw netip.Addr, ok bool) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return netip.Addr{}, false
	}
	routes, err := netlink.RouteListFiltered(family, &netlink.Route{LinkIndex: l.Attrs().Index, Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
	if err != nil {
		return netip.Addr{}, false
	}
	for _, r := range routes {
		if r.Dst == nil || r.Dst.IP.IsUnspecified() {
			if ones, _ := maskSize(r.Dst); ones == 0 {
				g, _ := netip.AddrFromSlice(r.Gw)
				return g.Unmap(), true
			}
		}
	}
	return netip.Addr{}, false
}

func maskSize(n *net.IPNet) (int, int) {
	if n == nil {
		return 0, 0
	}
	return n.Mask.Size()
}

// Neighbors returns the IP-to-MAC table of an interface.
func Neighbors(name string) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return out
	}
	own := l.Attrs().HardwareAddr.String()
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		list, err := netlink.NeighList(l.Attrs().Index, family)
		if err != nil {
			continue
		}
		for _, n := range list {
			// Only learned unicast neighbours are devices: the table also
			// holds static entries for multicast groups and broadcast.
			if len(n.HardwareAddr) != 6 || n.HardwareAddr[0]&1 != 0 ||
				n.State&(netlink.NUD_FAILED|netlink.NUD_INCOMPLETE|netlink.NUD_NOARP|netlink.NUD_PERMANENT) != 0 {
				continue
			}
			// Nor is the router itself (its own addresses can show up
			// here under IPv6).
			if n.HardwareAddr.String() == own {
				continue
			}
			if ip, ok := netip.AddrFromSlice(n.IP); ok && !ip.IsMulticast() {
				out[ip.Unmap()] = n.HardwareAddr.String()
			}
		}
	}
	return out
}

// Sysctl writes a value under /proc/sys, ignoring missing keys.
func Sysctl(path, value string) {
	_ = os.WriteFile("/proc/sys/"+path, []byte(value), 0)
}
