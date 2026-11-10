package ble

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/ryanzhou/porter/daemon/internal/proto"
)

// The test stands in for BlueZ on a private bus: it accepts the
// registrations, checks the exported object tree, then plays the part of
// a connected phone by calling WriteValue and collecting notifications.

type fakeBluez struct {
	t    *testing.T
	conn *dbus.Conn
	mu   sync.Mutex
	app  map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	adv  map[string]dbus.Variant
	from string
	set  map[string]any
}

func (f *fakeBluez) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci0": {"org.bluez.Adapter1": {}, "org.bluez.GattManager1": {}, "org.bluez.LEAdvertisingManager1": {}},
	}, nil
}

func (f *fakeBluez) RegisterApplication(sender dbus.Sender, path dbus.ObjectPath, _ map[string]dbus.Variant) *dbus.Error {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := f.conn.Object(string(sender), path).Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs); err != nil {
		return dbus.MakeFailedError(err)
	}
	f.mu.Lock()
	f.app, f.from = objs, string(sender)
	f.mu.Unlock()
	return nil
}

func (f *fakeBluez) RegisterAdvertisement(sender dbus.Sender, path dbus.ObjectPath, _ map[string]dbus.Variant) *dbus.Error {
	var props map[string]dbus.Variant
	if err := f.conn.Object(string(sender), path).Call("org.freedesktop.DBus.Properties.GetAll", 0, ifaceAdv).Store(&props); err != nil {
		return dbus.MakeFailedError(err)
	}
	f.mu.Lock()
	f.adv = props
	f.mu.Unlock()
	return nil
}

func (f *fakeBluez) UnregisterAdvertisement(dbus.ObjectPath) *dbus.Error { return nil }

func (f *fakeBluez) Set(iface, name string, v dbus.Variant) *dbus.Error {
	f.mu.Lock()
	f.set[name] = v.Value()
	f.mu.Unlock()
	return nil
}

// phoneLink is the fake phone's side of the GATT link.
type phoneLink struct {
	f      *fakeBluez
	tag    uint16
	mtu    int
	device dbus.ObjectPath
	sigs   chan *dbus.Signal
	re     proto.Reassembler
}

func (p *phoneLink) WritePacket(pkt []byte) error {
	for _, frag := range proto.Fragment(p.tag, pkt, p.mtu-3) {
		call := p.f.conn.Object(p.f.from, rxPath).Call(ifaceChar+".WriteValue", 0, frag,
			map[string]dbus.Variant{"device": dbus.MakeVariant(p.device), "mtu": dbus.MakeVariant(uint16(p.mtu))})
		if call.Err != nil {
			return call.Err
		}
	}
	return nil
}

func (p *phoneLink) ReadPacket() ([]byte, error) {
	for {
		select {
		case sig := <-p.sigs:
			if sig.Path != txPath || len(sig.Body) < 2 {
				continue
			}
			changed, _ := sig.Body[1].(map[string]dbus.Variant)
			frag, _ := changed["Value"].Value().([]byte)
			if tag, ok := proto.FragTag(frag); !ok || tag != p.tag {
				continue
			}
			if len(frag) > p.mtu-3 {
				return nil, errors.New("notification exceeds MTU")
			}
			pkt, err := p.re.Add(frag)
			if err != nil {
				return nil, err
			}
			if pkt != nil {
				return pkt, nil
			}
		case <-time.After(5 * time.Second):
			return nil, errors.New("timeout waiting for notification")
		}
	}
}

func (p *phoneLink) Close() error { return nil }

func startBus(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon not installed")
	}
	cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start dbus-daemon:", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func TestGATTServer(t *testing.T) {
	addr := startBus(t)
	bz, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer bz.Close()
	f := &fakeBluez{t: t, conn: bz, set: map[string]any{}}
	bz.Export(f, "/", "org.freedesktop.DBus.ObjectManager")
	bz.Export(f, "/org/bluez/hci0", "org.bluez.GattManager1")
	bz.Export(f, "/org/bluez/hci0", "org.bluez.LEAdvertisingManager1")
	bz.Export(f, "/org/bluez/hci0", ifaceProps)
	if r, err := bz.RequestName(bluez, 0); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatal("cannot own org.bluez:", err)
	}

	router, _ := proto.NewIdentity()
	srv := &Server{
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Name: func() string { return "Porter" },
		Info: func() []byte { return []byte(`{"v":1,"name":"Porter"}`) },
		Handle: func(pc proto.PacketConn) {
			s, err := proto.Accept(pc, router)
			if err != nil {
				return
			}
			for {
				m, err := s.Recv()
				if err != nil {
					return
				}
				s.Send(append([]byte("echo:"), m...))
			}
		},
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- srv.Run(func(opts ...dbus.ConnOption) (*dbus.Conn, error) { return dbus.Connect(addr, opts...) }, stop)
	}()
	t.Cleanup(func() { close(stop); <-done })

	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		ready := f.app != nil && f.adv != nil
		f.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not register")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// What BlueZ would see.
	f.mu.Lock()
	if got := f.app[svcPath][ifaceService]["UUID"].Value(); got != ServiceUUID {
		t.Errorf("service UUID: %v", got)
	}
	for path, want := range map[dbus.ObjectPath][]string{
		rxPath: {"write", "write-without-response"}, txPath: {"notify"}, infoPath: {"read"},
	} {
		c := f.app[path][ifaceChar]
		flags, _ := c["Flags"].Value().([]string)
		if strings.Join(flags, ",") != strings.Join(want, ",") || c["Service"].Value() != svcPath {
			t.Errorf("%s: flags %v service %v", path, flags, c["Service"].Value())
		}
	}
	if f.adv["Type"].Value() != "peripheral" {
		t.Errorf("advertisement type: %v", f.adv["Type"].Value())
	}
	if u, _ := f.adv["ServiceUUIDs"].Value().([]string); len(u) != 1 || u[0] != ServiceUUID {
		t.Errorf("advertised UUIDs: %v", f.adv["ServiceUUIDs"].Value())
	}
	if f.set["Powered"] != true || f.set["Alias"] != "Porter" {
		t.Errorf("adapter properties: %v", f.set)
	}
	from := f.from
	f.mu.Unlock()

	// Info characteristic.
	var info []byte
	if err := bz.Object(from, infoPath).Call(ifaceChar+".ReadValue", 0, map[string]dbus.Variant{}).Store(&info); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if json.Unmarshal(info, &doc) != nil || doc["name"] != "Porter" {
		t.Errorf("info: %s", info)
	}

	// A full secure session over the GATT link, at a small MTU so that
	// every packet is fragmented, with a payload spanning many packets.
	sigs := make(chan *dbus.Signal, 4096)
	bz.Signal(sigs)
	if err := bz.AddMatchSignal(dbus.WithMatchInterface(ifaceProps), dbus.WithMatchMember("PropertiesChanged")); err != nil {
		t.Fatal(err)
	}
	link := &phoneLink{f: f, tag: 0x1234, mtu: 64, device: "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF", sigs: sigs}
	app, _ := proto.NewIdentity()
	sess, err := proto.Dial(link, app)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sess.Remote, router.Public) {
		t.Fatal("router key not authenticated")
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 1500) // 24 kB
	for _, msg := range [][]byte{[]byte("ping"), big} {
		if err := sess.Send(msg); err != nil {
			t.Fatal(err)
		}
		got, err := sess.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, append([]byte("echo:"), msg...)) {
			t.Fatalf("echo mismatch (%d bytes)", len(got))
		}
	}

	// Disconnecting the device ends its session.
	srv.mu.Lock()
	n := len(srv.sessions)
	srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("sessions: %d", n)
	}
	bz.Emit(link.device, ifaceProps+".PropertiesChanged", "org.bluez.Device1",
		map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)}, []string{})
	for i := 0; ; i++ {
		srv.mu.Lock()
		n = len(srv.sessions)
		srv.mu.Unlock()
		if n == 0 {
			break
		}
		if i > 100 {
			t.Fatal("session survived disconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
