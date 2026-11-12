//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Two more subtests of TestRouter: devices finding each other across the
// router's networks (multicast DNS relayed between them), and several
// networks on one cable (a wired port that carries them under VLAN tags).
// The second needs a kernel with VLANs and skips itself, saying so, where
// there is none; where the router's image is tested (in-image-userland.sh)
// a skip is a failure. Like the tests in home_test.go, both explain
// themselves when they fail.

// --- finding devices across networks -------------------------------------------

// mdnsPeer is what a device that speaks multicast DNS has: a socket on the
// mDNS port, inside its namespace, that has joined the group on its
// interface. heard keeps a line for everything that arrived on it, for
// the log of a test that failed.
type mdnsPeer struct {
	who   string
	v6    bool
	c     net.PacketConn
	p4    *ipv4.PacketConn
	p6    *ipv6.PacketConn
	group *net.UDPAddr
	heard []string
}

// mdnsMessage is one message a peer received.
type mdnsMessage struct {
	msg  *dns.Msg
	from netip.Addr
	// hops is the TTL or hop limit it arrived with. Multicast DNS is sent
	// with 255, and some devices drop what arrives with less.
	hops int
}

func mdnsJoin(t *testing.T, who, ns, ifname string, v6 bool) *mdnsPeer {
	t.Helper()
	m := &mdnsPeer{who: who, v6: v6}
	var err error
	inNS(ns, func() {
		lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
			return rc.Control(func(fd uintptr) { unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1) })
		}}
		var ifi *net.Interface
		if ifi, err = net.InterfaceByName(ifname); err != nil {
			return
		}
		if v6 {
			// The link the group's address is meant on is the one the
			// socket sends multicast out of (set below). It cannot be
			// named with the address: the name would be looked up among
			// the interfaces of the test's own namespace.
			m.group = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}
			if m.c, err = lc.ListenPacket(context.Background(), "udp6", "[::]:5353"); err != nil {
				return
			}
			m.p6 = ipv6.NewPacketConn(m.c)
			for _, step := range []func() error{
				func() error { return m.p6.JoinGroup(ifi, m.group) },
				func() error { return m.p6.SetMulticastInterface(ifi) },
				func() error { return m.p6.SetMulticastHopLimit(255) },
				func() error { return m.p6.SetMulticastLoopback(false) },
				func() error { return m.p6.SetControlMessage(ipv6.FlagHopLimit|ipv6.FlagDst, true) },
			} {
				if err = step(); err != nil {
					return
				}
			}
			return
		}
		m.group = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
		if m.c, err = lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:5353"); err != nil {
			return
		}
		m.p4 = ipv4.NewPacketConn(m.c)
		for _, step := range []func() error{
			func() error { return m.p4.JoinGroup(ifi, m.group) },
			func() error { return m.p4.SetMulticastInterface(ifi) },
			func() error { return m.p4.SetMulticastTTL(255) },
			func() error { return m.p4.SetMulticastLoopback(false) },
			func() error { return m.p4.SetControlMessage(ipv4.FlagTTL|ipv4.FlagDst, true) },
		} {
			if err = step(); err != nil {
				return
			}
		}
	})
	if err != nil {
		t.Fatalf("%s: a multicast DNS socket on %s in %s (IPv6: %v): %v", who, ifname, ns, v6, err)
	}
	t.Cleanup(func() { m.c.Close() })
	return m
}

// say sends a message to the group, as a device does.
func (m *mdnsPeer) say(t *testing.T, msg *dns.Msg) {
	t.Helper()
	b, err := msg.Pack()
	if err == nil && m.v6 {
		_, err = m.p6.WriteTo(b, nil, m.group)
	} else if err == nil {
		_, err = m.p4.WriteTo(b, nil, m.group)
	}
	if err != nil {
		t.Fatalf("%s sending to the multicast DNS group: %v", m.who, err)
	}
}

// hears waits up to d for a message that match accepts, among the
// messages that were sent to the group.
func (m *mdnsPeer) hears(d time.Duration, match func(mdnsMessage) bool) (mdnsMessage, bool) {
	deadline := time.Now().Add(d)
	buf := make([]byte, 9000)
	for {
		m.c.SetReadDeadline(deadline)
		var n, hops int
		var from net.Addr
		var dst net.IP
		var err error
		if m.v6 {
			var cm *ipv6.ControlMessage
			if n, cm, from, err = m.p6.ReadFrom(buf); err == nil && cm != nil {
				hops, dst = cm.HopLimit, cm.Dst
			}
		} else {
			var cm *ipv4.ControlMessage
			if n, cm, from, err = m.p4.ReadFrom(buf); err == nil && cm != nil {
				hops, dst = cm.TTL, cm.Dst
			}
		}
		if err != nil {
			return mdnsMessage{}, false
		}
		got := mdnsMessage{msg: new(dns.Msg), hops: hops}
		if u, ok := from.(*net.UDPAddr); ok {
			a, _ := netip.AddrFromSlice(u.IP)
			got.from = a.Unmap()
		}
		if err := got.msg.Unpack(buf[:n]); err != nil {
			m.heard = append(m.heard, fmt.Sprintf("from %s to %s: %d bytes that are no DNS message (%v)", got.from, dst, n, err))
			continue
		}
		m.heard = append(m.heard, fmt.Sprintf("from %s to %s, %d hops left: response=%v questions=%v answers=%v", got.from, dst, hops, got.msg.Response, got.msg.Question, got.msg.Answer))
		if dst.Equal(m.group.IP) && match(got) {
			return got, true
		}
	}
}

// mdnsQuestion is a question for the services of one kind; private asks
// for the answer to be sent to the asker alone, as a device's first
// question does.
func mdnsQuestion(service string, private bool) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(service, dns.TypePTR)
	q.Id, q.RecursionDesired = 0, false
	if private {
		q.Question[0].Qclass |= 0x8000
	}
	return q
}

func mdnsAnswer(service, instance string) *dns.Msg {
	a := new(dns.Msg)
	a.Response, a.Authoritative = true, true
	a.Answer = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: service, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 120}, Ptr: instance}}
	return a
}

func asks(service string) func(mdnsMessage) bool {
	return func(m mdnsMessage) bool {
		return !m.msg.Response && len(m.msg.Question) == 1 && m.msg.Question[0].Name == service
	}
}

func answers(service string) func(mdnsMessage) bool {
	return func(m mdnsMessage) bool {
		return m.msg.Response && len(m.msg.Answer) == 1 && m.msg.Answer[0].Header().Name == service
	}
}

func discovery(t *testing.T, r *rig) {
	const (
		things       = "things" // the id of the network the printer is on
		mainRouter   = "10.83.0.1"
		thingsRouter = "10.84.0.1"
	)
	rejoin := func() {
		sh(t, "ip -n %s link set pt-c2if down && sleep 1 && ip -n %s link set pt-c2if up", nsC2, nsC2)
	}
	where := func(mac string) (ip, network string) {
		var devs []device
		r.c.Call("devices.list", nil, &devs)
		for _, d := range devs {
			if d.MAC == mac {
				return d.IPv4, d.Network
			}
		}
		return "", ""
	}
	input := func() string { return output("ip netns exec %s nft list chain inet porter input", nsRtr) }

	var peers []*mdnsPeer
	// Whatever happens, the router ends with its one network and both
	// clients on it.
	defer func() {
		r.edit(t, func(cfg *config.Config) {
			cfg.Networks = cfg.Networks[:1]
			cfg.Networks[0].Discovery = false
			for i := range cfg.Ethernet {
				cfg.Ethernet[i].Network = config.MainNetwork
			}
		})
		rejoin()
		eventually(t, "the second client back in the main network", 60*time.Second, func() bool {
			ip, network := where(r.mac2)
			return strings.HasPrefix(ip, "10.83.0.") && network == config.MainNetwork
		})
	}()
	defer func() {
		if t.Failed() {
			for _, p := range peers {
				t.Logf("everything %s received on the multicast DNS port:\n%s", p.who, strings.Join(p.heard, "\n"))
			}
		}
	}()
	defer r.explain(t, r.logMark(), nil, append(append([]string{}, showRouter...),
		"ip -n "+nsRtr+" maddr show", "ip netns exec "+nsRtr+" ss -ulnp", "ip netns exec "+nsRtr+" nft list chain inet porter input",
		"ip netns exec "+nsRtr+" nft list chain inet porter lan_lan", "ip -n "+nsC1+" addr show", "ip -n "+nsC2+" addr show",
		"ip -n "+nsC1+" maddr show", "ip -n "+nsC2+" maddr show")...)

	// A network for things, kept apart, with the second client on it as
	// the printer; the first client is the phone, on the main network.
	r.edit(t, func(cfg *config.Config) {
		cfg.Devices, cfg.Policy.Rules, cfg.Policy.Default = nil, nil, config.ActDirect
		cfg.Networks = append(cfg.Networks, config.Network{ID: things, Name: "Things", Address: thingsRouter + "/24", Isolated: true})
		for i := range cfg.Ethernet {
			if cfg.Ethernet[i].Name == "eth2" {
				cfg.Ethernet[i].Network = things
			}
		}
	})
	rejoin()
	var phoneIP, printerIP string
	eventually(t, "the second client on the network for things", 60*time.Second, func() bool {
		var n1, n2 string
		phoneIP, n1 = where(r.mac1)
		printerIP, n2 = where(r.mac2)
		// The router knows of a lease before the device has taken the
		// address into use, so the device is asked as well.
		return strings.HasPrefix(printerIP, "10.84.0.") && n2 == things && strings.HasPrefix(phoneIP, "10.83.0.") && n1 == config.MainNetwork &&
			strings.Contains(output("ip -n %s -4 addr show dev pt-c2if", nsC2), "inet "+printerIP+"/")
	})
	phone := mdnsJoin(t, "the phone", nsC1, "pt-c1if", false)
	printer := mdnsJoin(t, "the printer", nsC2, "pt-c2if", false)
	peers = append(peers, phone, printer)

	// Not asked for: what the phone asks stays on its own network, and
	// the router does not listen for it.
	phone.say(t, mdnsQuestion("_before._tcp.local.", false))
	if got, ok := printer.hears(2*time.Second, asks("_before._tcp.local.")); ok {
		t.Fatalf("with nothing switched on, a question from the main network arrived on the other: %+v", got)
	}
	if rules := input(); strings.Contains(rules, "5353") {
		t.Errorf("with nothing switched on, the router lets multicast DNS in:\n%s", rules)
	}

	// The network for things lets itself be found. The phone asks, the
	// way a device asks first: for an answer to itself alone.
	r.edit(t, func(cfg *config.Config) { cfg.Network(things).Discovery = true })
	const ipp = "_ipp._tcp.local."
	var q mdnsMessage
	eventually(t, "the phone's question heard on the network for things", 30*time.Second, func() bool {
		phone.say(t, mdnsQuestion(ipp, true))
		var ok bool
		q, ok = printer.hears(1500*time.Millisecond, asks(ipp))
		return ok
	})
	// It comes from the router's address on that network, as a message
	// of that network (255 hops), and no longer asks for a private
	// answer: that would go to the router and end there.
	if q.from.String() != thingsRouter || q.hops != 255 || q.msg.Question[0].Qclass != dns.ClassINET {
		t.Errorf("the question as relayed: from %s with %d hops left, class %#x; want from %s, 255, %#x",
			q.from, q.hops, q.msg.Question[0].Qclass, thingsRouter, dns.ClassINET)
	}
	if rules := input(); !strings.Contains(rules, "224.0.0.251") || !strings.Contains(rules, `"br-`+things+`"`) {
		t.Errorf("the router's input rules with a network that lets itself be found:\n%s", rules)
	}
	// The printer answers to the group, and the phone hears it, from the
	// router's address on its own network.
	const instance = "Printer-upstairs._ipp._tcp.local."
	var a mdnsMessage
	eventually(t, "the printer's answer heard on the main network", 15*time.Second, func() bool {
		printer.say(t, mdnsAnswer(ipp, instance))
		var ok bool
		a, ok = phone.hears(1500*time.Millisecond, answers(ipp))
		return ok
	})
	if ptr, _ := a.msg.Answer[0].(*dns.PTR); ptr == nil || ptr.Ptr != instance || a.from.String() != mainRouter || a.hops != 255 {
		t.Errorf("the answer as relayed: %v from %s with %d hops left; want %s from %s, 255", a.msg.Answer, a.from, a.hops, instance, mainRouter)
	}
	// Nothing comes back to where it was said, and nothing goes round:
	// one question, a moment of quiet, and neither side has heard it
	// again (the printer hears it the once it was relayed).
	const once = "_once._tcp.local."
	time.Sleep(time.Second)
	phone.say(t, mdnsQuestion(once, false))
	if _, ok := printer.hears(3*time.Second, asks(once)); !ok {
		t.Errorf("a question asked once did not arrive")
	}
	if got, ok := printer.hears(2*time.Second, asks(once)); ok {
		t.Errorf("a question asked once arrived twice on the other network: %+v", got)
	}
	if got, ok := phone.hears(time.Second, asks(once)); ok {
		t.Errorf("the phone's own question came back to its network: %+v", got)
	}
	// What the answer leads to: the phone opens a connection to the
	// printer, although the printer's network is kept apart; the printer
	// cannot open one to the phone.
	hello := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "printing") })
	l1, l2 := listen(t, nsC1, phoneIP+":8631"), listen(t, nsC2, printerIP+":8631")
	defer l1.Close()
	defer l2.Close()
	go http.Serve(l1, hello)
	go http.Serve(l2, hello)
	if got, err := httpGet(nsC1, "http://"+printerIP+":8631/"); err != nil || got != "printing" {
		t.Errorf("the phone connecting to the printer it found: %q %v", got, err)
	}
	if got, err := httpGet(nsC2, "http://"+phoneIP+":8631/"); err == nil {
		t.Errorf("the printer, on a network kept apart, connected to the phone: %q", got)
	}

	if r.v6 {
		// The same over IPv6, which the devices use among themselves with
		// the addresses every interface has of its own.
		phone6 := mdnsJoin(t, "the phone (IPv6)", nsC1, "pt-c1if", true)
		printer6 := mdnsJoin(t, "the printer (IPv6)", nsC2, "pt-c2if", true)
		peers = append(peers, phone6, printer6)
		const airplay = "_airplay._tcp.local."
		var q6 mdnsMessage
		eventually(t, "the phone's IPv6 question heard on the network for things", 30*time.Second, func() bool {
			phone6.say(t, mdnsQuestion(airplay, true))
			var ok bool
			q6, ok = printer6.hears(1500*time.Millisecond, asks(airplay))
			return ok
		})
		if !q6.from.IsLinkLocalUnicast() || q6.hops != 255 || q6.msg.Question[0].Qclass != dns.ClassINET {
			t.Errorf("the IPv6 question as relayed: from %s with %d hops left, class %#x", q6.from, q6.hops, q6.msg.Question[0].Qclass)
		}
		// From the router's own address on that network's bridge, not
		// the phone's.
		if bridge := output("ip -n %s -6 addr show dev br-%s", nsRtr, things); !strings.Contains(bridge, q6.from.String()+"/") {
			t.Errorf("the IPv6 question came from %s, which is not on the router's bridge:\n%s", q6.from, bridge)
		}
		eventually(t, "the printer's IPv6 answer heard on the main network", 15*time.Second, func() bool {
			printer6.say(t, mdnsAnswer(airplay, "Television._airplay._tcp.local."))
			_, ok := phone6.hears(1500*time.Millisecond, answers(airplay))
			return ok
		})
	}

	// Switched off again: quiet, and the router's ear closed.
	r.edit(t, func(cfg *config.Config) { cfg.Network(things).Discovery = false })
	eventually(t, "the router no longer letting multicast DNS in", 15*time.Second, func() bool { return !strings.Contains(input(), "5353") })
	time.Sleep(time.Second)
	phone.say(t, mdnsQuestion("_after._tcp.local.", false))
	if got, ok := printer.hears(2*time.Second, asks("_after._tcp.local.")); ok {
		t.Errorf("switched off again, a question from the main network still arrived: %+v", got)
	}
}

// --- networks on one cable -------------------------------------------------------

// The far end of the cable: a namespace that does what a managed switch
// does, and a device behind each of its three kinds of port.
const (
	nsSwitch   = "pt-sw"
	nsPlain    = "pt-t0" // on a port without a tag: the main network
	nsThing    = "pt-t1" // on a port for the things' VLAN
	nsCamera   = "pt-t2" // on a port for the cameras' VLAN
	trunkPort  = "eth3"  // the router's end of the cable
	switchPort = "sw0"   // the switch's
)

func trunk(t *testing.T, r *rig) {
	const (
		things, cams             = "things", "cams"
		thingsRouter, camsRouter = "10.84.0.1", "10.85.0.1"
		thingsTag, camsTag       = 20, 30
		newThingsTag             = 21
	)
	all := []string{nsSwitch, nsPlain, nsThing, nsCamera}
	clients := []string{nsPlain, nsThing, nsCamera}
	ifOf := func(ns string) string { return ns + "if" }
	down := func() {
		try("ip -n %s link del %s", nsRtr, trunkPort)
		for _, ns := range all {
			try("ip netns pids %s 2>/dev/null | xargs -r kill -9", ns)
			try("ip netns del %s", ns)
		}
	}
	down() // what an earlier run that was cut short may have left
	for _, ns := range all {
		sh(t, "ip netns add %s && ip -n %s link set lo up", ns, ns)
	}
	defer down()
	// A switch passes on what its devices say and says nothing itself.
	// Left alone, each of its interfaces here would speak IPv6 for itself
	// and be taken by the router for one more device on the cable.
	try("ip netns exec %s sh -c 'for f in /proc/sys/net/ipv6/conf/all/disable_ipv6 /proc/sys/net/ipv6/conf/default/disable_ipv6; do echo 1 > $f; done' 2>/dev/null", nsSwitch)

	// cable plugs the cable in: the router's port and the switch's, and
	// on the switch a sub-interface for every VLAN, each joined to the
	// ports of that VLAN. Unplugged (the port deleted), all of that is
	// gone with it.
	cable := func(tags map[string]int) error {
		script := fmt.Sprintf("ip -n %s link add %s type veth peer name %s netns %s && ip -n %s link set %s master swbr0 && ip -n %s link set %s up",
			nsRtr, trunkPort, switchPort, nsSwitch, nsSwitch, switchPort, nsSwitch, switchPort)
		for bridge, tag := range tags {
			sub := fmt.Sprintf("%s.%d", switchPort, tag)
			script += fmt.Sprintf(" && ip -n %s link add link %s name %s type vlan id %d && ip -n %s link set %s master %s && ip -n %s link set %s up",
				nsSwitch, switchPort, sub, tag, nsSwitch, sub, bridge, nsSwitch, sub)
		}
		if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", script, err, bytes.TrimSpace(out))
		}
		return nil
	}
	// The switch's ports for devices: one bridge per VLAN (swbr0 for
	// what is not tagged), each with the port of its device.
	bridgeOf := map[string]string{nsPlain: "swbr0", nsThing: "swbr-things", nsCamera: "swbr-cams"}
	for _, ns := range clients {
		port := "to-" + ns
		sh(t, "ip -n %s link add %s type bridge && ip -n %s link set %s up", nsSwitch, bridgeOf[ns], nsSwitch, bridgeOf[ns])
		sh(t, "ip -n %s link add %s type veth peer name %s netns %s", nsSwitch, port, ifOf(ns), ns)
		sh(t, "ip -n %s link set %s master %s && ip -n %s link set %s up && ip -n %s link set %s up", nsSwitch, port, bridgeOf[ns], nsSwitch, port, ns, ifOf(ns))
	}
	// Without VLANs in the kernel there is nothing to test, and the
	// router is not shown a port it would then remember.
	if out, err := exec.Command("ip", "-n", nsSwitch, "link", "add", "link", "to-"+nsPlain, "name", "probe", "type", "vlan", "id", "2").CombinedOutput(); err != nil {
		t.Skipf("this kernel cannot make VLAN interfaces (no 8021q): %s", bytes.TrimSpace(out))
	}
	sh(t, "ip -n %s link del probe", nsSwitch)
	if err := cable(map[string]int{"swbr-things": thingsTag, "swbr-cams": camsTag}); err != nil {
		t.Fatalf("plugging the cable in: %v", err)
	}
	macs := map[string]string{}
	for _, ns := range clients {
		macs[ns] = strings.TrimSpace(sh(t, "ip netns exec %s cat /sys/class/net/%s/address", ns, ifOf(ns)))
		// New devices, as far as the router's activity log is concerned.
		r.joined = append(r.joined, macs[ns])
	}

	sub := func(tag int) string { return config.VLANIfName(trunkPort, tag) }
	link := func(name string) string { return output("ip -n %s -d link show %s 2>&1", nsRtr, name) }
	exists := func(name string) bool { return exec.Command("ip", "-n", nsRtr, "link", "show", name).Run() == nil }
	port := func(f func(e *config.EthPort)) {
		t.Helper()
		r.edit(t, func(cfg *config.Config) {
			for i := range cfg.Ethernet {
				if cfg.Ethernet[i].Name == trunkPort {
					f(&cfg.Ethernet[i])
				}
			}
		})
	}
	// Whatever happens, the router ends with its one network and no use
	// for the port, which is then taken away.
	defer func() {
		r.edit(t, func(cfg *config.Config) {
			cfg.Networks = cfg.Networks[:1]
			cfg.Devices = nil
			for i := range cfg.Ethernet {
				cfg.Ethernet[i].Network, cfg.Ethernet[i].Tagged = config.MainNetwork, nil
				if cfg.Ethernet[i].Name == trunkPort {
					cfg.Ethernet[i].Role = config.EthOff
				}
			}
		})
	}()
	var show []string
	for _, ns := range all {
		show = append(show, "ip -n "+ns+" -d link show", "ip -n "+ns+" addr show", "ip -n "+ns+" route show")
	}
	defer r.explain(t, r.logMark(), []string{"pt-t0-dhcpcd.log", "pt-t1-dhcpcd.log", "pt-t2-dhcpcd.log"}, append(append(append([]string{}, showRouter...), show...),
		"ip -n "+nsRtr+" neigh show", "ip netns exec "+nsRtr+" nft list chain inet porter input", "ip netns exec "+nsRtr+" nft list chain inet porter forward",
		"ip netns exec "+nsRtr+" nft list chain inet porter lan_lan", "ip netns exec "+nsRtr+" nft list chain inet porter classify",
		"cat "+filepath.Join(r.run, "dnsmasq.conf"), "cat "+filepath.Join(r.run, "dnsmasq.leases"))...)

	// The router finds the new port by itself. It becomes a port for
	// devices that carries the main network as it is and the two others
	// tagged: things kept apart, cameras not.
	eventually(t, "the router listing the new port", 30*time.Second, func() bool {
		var cfg config.Config
		r.c.Call("config.get", nil, &cfg)
		for _, e := range cfg.Ethernet {
			if e.Name == trunkPort {
				return true
			}
		}
		return false
	})
	r.edit(t, func(cfg *config.Config) {
		cfg.Devices, cfg.Policy.Rules, cfg.Policy.Default = nil, nil, config.ActDirect
		cfg.Networks = append(cfg.Networks,
			config.Network{ID: things, Name: "Things", Address: thingsRouter + "/24", Isolated: true, VLAN: thingsTag},
			config.Network{ID: cams, Name: "Cameras", Address: camsRouter + "/24", VLAN: camsTag})
		for i := range cfg.Ethernet {
			if e := &cfg.Ethernet[i]; e.Name == trunkPort {
				e.Role, e.Network, e.Tagged = config.EthLAN, config.MainNetwork, []string{things, cams}
			}
		}
	})
	// checks asks the router what a change to its configuration would
	// do, without making it: why it would be refused, or what would be
	// noticed.
	checks := func(change func(cfg *config.Config)) (refused, effects string) {
		t.Helper()
		var draft config.Config
		if err := r.c.Call("config.get", nil, &draft); err != nil {
			t.Fatal(err)
		}
		change(&draft)
		var check struct {
			Error   string   `json:"error"`
			Effects []string `json:"effects"`
		}
		if err := r.c.Call("config.check", map[string]any{"config": &draft}, &check); err != nil {
			t.Fatalf("config.check: %v", err)
		}
		return check.Error, strings.Join(check.Effects, "\n")
	}
	// What the router is asked to do with a network it cannot tag is
	// refused, in words for the person.
	if refused, _ := checks(func(cfg *config.Config) { cfg.Network(cams).VLAN = 0 }); !strings.Contains(refused, "Cameras has no VLAN ID") {
		t.Errorf("config.check on a tagged network without a VLAN ID: %q", refused)
	}

	// The router's side: the port in the main network's bridge, and a
	// sub-interface of it for each tag in the bridge of that network.
	eventually(t, "the port's interfaces for the two VLANs, each in its network's bridge", 30*time.Second, func() bool {
		return strings.Contains(link(sub(thingsTag)), "master br-"+things) && strings.Contains(link(sub(camsTag)), "master br-"+cams) &&
			strings.Contains(link(trunkPort), "master br-lan")
	})
	for tag, name := range map[int]string{thingsTag: sub(thingsTag), camsTag: sub(camsTag)} {
		if l := link(name); !strings.Contains(l, name+"@"+trunkPort) || !strings.Contains(l, fmt.Sprintf("vlan protocol 802.1Q id %d", tag)) {
			t.Errorf("%s is not VLAN %d of %s:\n%s", name, tag, trunkPort, l)
		}
	}

	// The devices behind the switch ask for addresses, and each gets one
	// from the network its port on the switch belongs to.
	conf := filepath.Join(r.dir, "client-dhcpcd.conf")
	for _, ns := range clients {
		r.start(ns+" dhcp", ns+"-dhcpcd.log", "ip", "netns", "exec", ns, "dhcpcd", "--nobackground", "--config", conf, ifOf(ns))
	}
	want := map[string][2]string{nsPlain: {"10.83.0.", config.MainNetwork}, nsThing: {"10.84.0.", things}, nsCamera: {"10.85.0.", cams}}
	routerOf := map[string]string{nsPlain: "10.83.0.1", nsThing: thingsRouter, nsCamera: camsRouter}
	resolves := func(ns string) bool {
		a, _, err := lookup(ns, routerOf[ns], "direct.test")
		return err == nil && a == siteA
	}
	addr := map[string]string{}
	settled := func() bool {
		var devs []device
		r.c.Call("devices.list", nil, &devs)
		n := 0
		for _, ns := range clients {
			// The router knows of a lease before the device has taken
			// the address into use; a name resolved by the router's
			// address on that network shows that it has.
			if !resolves(ns) {
				continue
			}
			for _, d := range devs {
				if d.MAC == macs[ns] && strings.HasPrefix(d.IPv4, want[ns][0]) && d.Network == want[ns][1] && d.Link == "wired" && d.Online {
					addr[ns] = d.IPv4
					n++
				}
			}
		}
		return n == len(clients)
	}
	eventually(t, "each device behind the switch listed in the network of its VLAN, with an address from it, as a wired device", 90*time.Second, settled)
	uplink, _ := r.source(t, "eth:eth0")
	wanIP := strings.Split(uplink.IPv4, "/")[0]
	// Each has what a device on that network has: the router's name
	// service at its address there (above), and the internet.
	for _, ns := range clients {
		ns := ns
		eventually(t, ns+" reaching the internet", 20*time.Second, func() bool { return reaches(ns, siteA) == wanIP })
	}
	if r.v6 {
		// And IPv6: the router's advertisements for a network reach the
		// devices that arrive tagged, which make their addresses from
		// that network's prefix.
		var cfg config.Config
		if err := r.c.Call("config.get", nil, &cfg); err != nil {
			t.Fatal(err)
		}
		for ns, id := range map[string]string{nsThing: things, nsCamera: cams} {
			_, prefix := cfg.Network(id).ULA()
			ns := ns
			eventually(t, ns+" with an IPv6 address out of "+prefix.String(), 40*time.Second, func() bool {
				_, _, ok := globalIn(ns, ifOf(ns), prefix)
				return ok
			})
		}
	}
	// Kept apart as configured: anyone may open a connection to a thing,
	// a thing to nobody; the main network and the cameras to each other.
	hello := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello") })
	for _, ns := range clients {
		l := listen(t, ns, addr[ns]+":8088")
		defer l.Close()
		go http.Serve(l, hello)
	}
	opens := func(from, to string) bool {
		got, err := httpGet(from, "http://"+addr[to]+":8088/")
		return err == nil && got == "hello"
	}
	for _, pair := range [][2]string{{nsPlain, nsThing}, {nsCamera, nsThing}, {nsPlain, nsCamera}, {nsCamera, nsPlain}} {
		pair := pair
		eventually(t, pair[0]+" opening a connection to "+pair[1], 15*time.Second, func() bool { return opens(pair[0], pair[1]) })
	}
	for _, to := range []string{nsPlain, nsCamera} {
		if opens(nsThing, to) {
			t.Errorf("a device on the network kept apart opened a connection to %s", to)
		}
	}
	// A route for a network holds for the devices that arrive tagged.
	r.edit(t, func(cfg *config.Config) { cfg.Network(things).Route = config.ActBlock })
	eventually(t, "the things' network blocked, the cameras' not", 15*time.Second, func() bool {
		return reaches(nsThing, siteA) == "" && reaches(nsCamera, siteA) == wanIP
	})
	r.edit(t, func(cfg *config.Config) { cfg.Network(things).Route = "" })
	eventually(t, "the things' network online again", 15*time.Second, func() bool { return reaches(nsThing, siteA) == wanIP })
	// The router's own interfaces for the tags are not ports to set up.
	var cfg config.Config
	if err := r.c.Call("config.get", nil, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, e := range cfg.Ethernet {
		if config.MadeIf(e.Name) {
			t.Errorf("%s is listed among the ports", e.Name)
		}
	}
	var ports struct {
		Ports []struct {
			Name string `json:"name"`
		} `json:"ports"`
	}
	if err := r.c.Call("status.get", nil, &ports); err != nil {
		t.Fatal(err)
	}
	for _, p := range ports.Ports {
		if config.MadeIf(p.Name) {
			t.Errorf("%s is listed among the plugged-in ports", p.Name)
		}
	}

	// Another VLAN ID for the things. The router says beforehand what
	// that will be noticed as, then makes the interface for the new ID
	// and removes the one for the old; the thing is cut off until the
	// switch follows.
	if refused, effects := checks(func(cfg *config.Config) { cfg.Network(things).VLAN = newThingsTag }); refused != "" ||
		!strings.Contains(effects, fmt.Sprintf("Things changes from VLAN %d to VLAN %d on %s", thingsTag, newThingsTag, trunkPort)) {
		t.Errorf("config.check on a new VLAN ID: refused with %q, effects %q", refused, effects)
	}
	r.edit(t, func(cfg *config.Config) { cfg.Network(things).VLAN = newThingsTag })
	eventually(t, "the interface for the old ID replaced by one for the new, in the same bridge", 30*time.Second, func() bool {
		return !exists(sub(thingsTag)) && strings.Contains(link(sub(newThingsTag)), "master br-"+things)
	})
	if resolves(nsThing) {
		t.Errorf("the thing still reaches the router under the old VLAN ID")
	}
	sh(t, "ip -n %s link del %s.%d && ip -n %s link add link %s name %s.%d type vlan id %d && ip -n %s link set %s.%d master swbr-things && ip -n %s link set %s.%d up",
		nsSwitch, switchPort, thingsTag, nsSwitch, switchPort, switchPort, newThingsTag, newThingsTag, nsSwitch, switchPort, newThingsTag, nsSwitch, switchPort, newThingsTag)
	eventually(t, "the thing back once the switch uses the new ID", 20*time.Second, func() bool { return resolves(nsThing) })
	if !resolves(nsCamera) || !resolves(nsPlain) {
		t.Errorf("the other networks on the cable after one changed its VLAN ID: cameras %v, main %v", resolves(nsCamera), resolves(nsPlain))
	}

	// The cable's adapter is unplugged and plugged in again: its
	// interfaces went with it, and the router makes them anew.
	sh(t, "ip -n %s link del %s", nsRtr, trunkPort)
	eventually(t, "the port and its VLAN interfaces gone with the adapter", 10*time.Second, func() bool {
		return !exists(trunkPort) && !exists(sub(newThingsTag)) && !exists(sub(camsTag))
	})
	if resolves(nsThing) || resolves(nsPlain) {
		t.Errorf("devices reach the router with the cable unplugged")
	}
	if err := cable(map[string]int{"swbr-things": newThingsTag, "swbr-cams": camsTag}); err != nil {
		t.Fatalf("plugging the cable back in: %v", err)
	}
	eventually(t, "the port's VLAN interfaces made anew, in their bridges", 30*time.Second, func() bool {
		return strings.Contains(link(sub(newThingsTag)), "master br-"+things) && strings.Contains(link(sub(camsTag)), "master br-"+cams) &&
			strings.Contains(link(trunkPort), "master br-lan")
	})
	for _, ns := range clients {
		ns := ns
		eventually(t, ns+" reaching the router again after the cable was plugged back in", 30*time.Second, func() bool { return resolves(ns) })
	}

	// A port for tagged networks only. Said beforehand; then what is not
	// tagged goes nowhere, and what is tagged goes on.
	if refused, effects := checks(func(cfg *config.Config) {
		for i := range cfg.Ethernet {
			if cfg.Ethernet[i].Name == trunkPort {
				cfg.Ethernet[i].Network = ""
			}
		}
	}); refused != "" || !strings.Contains(effects, trunkPort+" carries tagged networks only") {
		t.Errorf("config.check on a port for tagged networks only: refused with %q, effects %q", refused, effects)
	}
	port(func(e *config.EthPort) { e.Network = "" })
	eventually(t, "the port out of the main network's bridge, the device without a tag cut off", 30*time.Second, func() bool {
		return !strings.Contains(link(trunkPort), "master") && !resolves(nsPlain)
	})
	if !resolves(nsThing) || !resolves(nsCamera) {
		t.Errorf("the tagged networks on a port for tagged networks only: things %v, cameras %v", resolves(nsThing), resolves(nsCamera))
	}
	if l := link(trunkPort); !strings.Contains(l, "UP") {
		t.Errorf("the port under the VLAN interfaces is not up:\n%s", l)
	}

	// One network taken off the cable: its interface goes, the other's
	// stays.
	port(func(e *config.EthPort) { e.Tagged = []string{things} })
	eventually(t, "the interface for the network taken off the cable removed", 30*time.Second, func() bool {
		return !exists(sub(camsTag)) && !resolves(nsCamera)
	})
	if !resolves(nsThing) {
		t.Errorf("the network left on the cable after another was taken off it")
	}
	// The port no longer for devices at all: nothing of it is left.
	port(func(e *config.EthPort) { e.Role = config.EthOff })
	eventually(t, "every VLAN interface of the port removed", 30*time.Second, func() bool {
		return !exists(sub(newThingsTag)) && !resolves(nsThing)
	})
	if rest := output("ip -n %s -o link show type vlan 2>/dev/null", nsRtr); strings.TrimSpace(rest) != "" {
		t.Errorf("VLAN interfaces left on the router:\n%s", rest)
	}
}
