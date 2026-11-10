// Package ble exposes the control protocol as a Bluetooth LE GATT service
// through BlueZ.
//
// The service has three characteristics:
//
//	RX    write / write-without-response   app -> router fragments
//	TX    notify                           router -> app fragments
//	Info  read                             unauthenticated JSON summary
//
// No BLE-level pairing or bonding is used. Authentication and encryption
// happen one layer up (package proto), which keeps the security model
// independent of each platform's Bluetooth pairing behaviour.
package ble

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"

	"github.com/ryanzhou/porter/daemon/internal/proto"
)

const (
	ServiceUUID = "7e0d0001-5c3a-4b8e-9f1d-706f72746572"
	RXUUID      = "7e0d0002-5c3a-4b8e-9f1d-706f72746572"
	TXUUID      = "7e0d0003-5c3a-4b8e-9f1d-706f72746572"
	InfoUUID    = "7e0d0004-5c3a-4b8e-9f1d-706f72746572"

	rootPath = dbus.ObjectPath("/org/porter")
	svcPath  = rootPath + "/service0"
	rxPath   = svcPath + "/char0"
	txPath   = svcPath + "/char1"
	infoPath = svcPath + "/char2"
	advPath  = rootPath + "/adv0"

	bluez        = "org.bluez"
	ifaceChar    = "org.bluez.GattCharacteristic1"
	ifaceService = "org.bluez.GattService1"
	ifaceAdv     = "org.bluez.LEAdvertisement1"
	ifaceProps   = "org.freedesktop.DBus.Properties"

	maxSessions = 4
	idleTimeout = 15 * time.Minute
)

// Server is the GATT peripheral.
type Server struct {
	Log *slog.Logger
	// Name is the advertised local name.
	Name func() string
	// Info returns the content of the Info characteristic.
	Info func() []byte
	// Handle serves one control session. It runs on its own goroutine
	// and should return when the PacketConn fails.
	Handle func(pc proto.PacketConn)

	conn *dbus.Conn

	mu       sync.Mutex
	sessions map[string]*session // device path + tag
	adapter  dbus.ObjectPath
	lastName string
}

type session struct {
	srv    *Server
	key    string
	device string
	tag    uint16
	mtu    int
	re     proto.Reassembler
	in     chan []byte
	closed chan struct{}
	once   sync.Once
	last   time.Time
}

func (s *session) ReadPacket() ([]byte, error) {
	select {
	case p := <-s.in:
		return p, nil
	case <-s.closed:
		return nil, io.EOF
	}
}

func (s *session) WritePacket(p []byte) error {
	select {
	case <-s.closed:
		return io.ErrClosedPipe
	default:
	}
	s.srv.mu.Lock()
	mtu := s.mtu
	s.last = time.Now()
	s.srv.mu.Unlock()
	// An ATT notification carries MTU-3 bytes.
	for _, frag := range proto.Fragment(s.tag, p, mtu-3) {
		if err := s.srv.notify(frag); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Close() error {
	s.once.Do(func() {
		close(s.closed)
		s.srv.mu.Lock()
		if s.srv.sessions[s.key] == s {
			delete(s.srv.sessions, s.key)
		}
		s.srv.mu.Unlock()
	})
	return nil
}

// Dialer opens the bus connection; dbus.ConnectSystemBus in production.
type Dialer func(opts ...dbus.ConnOption) (*dbus.Conn, error)

// Run connects to the bus, registers with BlueZ and keeps the
// registration alive until stop is closed.
func (s *Server) Run(dial Dialer, stop <-chan struct{}) error {
	s.sessions = map[string]*session{}
	// The bus library dispatches each incoming method call on its own
	// goroutine, which would let fragments overtake each other. The
	// interceptor runs synchronously in arrival order, so fragments are
	// consumed there and the exported WriteValue only acknowledges.
	conn, err := dial(dbus.WithIncomingInterceptor(s.intercept))
	if err != nil {
		return err
	}
	defer conn.Close()
	s.conn = conn
	if err := s.export(); err != nil {
		return err
	}
	sigs := make(chan *dbus.Signal, 32)
	conn.Signal(sigs)
	_ = conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, bluez))
	_ = conn.AddMatchSignal(dbus.WithMatchSender(bluez), dbus.WithMatchInterface(ifaceProps), dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchArg(0, "org.bluez.Device1"))

	registered := false
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	try := func() {
		if !registered {
			if err := s.register(); err != nil {
				s.Log.Debug("bluetooth not ready", "err", err)
				return
			}
			registered = true
			s.Log.Info("bluetooth control service registered", "adapter", s.adapter)
			return
		}
		// Follow name changes.
		if name := s.Name(); name != s.lastName {
			s.unregisterAdv()
			if err := s.registerAdv(); err != nil {
				s.Log.Warn("re-advertising failed", "err", err)
			}
		}
		s.reap()
	}
	try()
	for {
		select {
		case <-stop:
			s.unregisterAdv()
			return nil
		case <-tick.C:
			try()
		case sig := <-sigs:
			if sig == nil {
				return errors.New("bus connection lost")
			}
			switch sig.Name {
			case "org.freedesktop.DBus.NameOwnerChanged":
				// BlueZ restarted (or went away): everything must be
				// registered again.
				registered = false
				s.closeAll("")
				try()
			case ifaceProps + ".PropertiesChanged":
				if len(sig.Body) >= 2 {
					if changed, ok := sig.Body[1].(map[string]dbus.Variant); ok {
						if v, ok := changed["Connected"]; ok {
							if c, _ := v.Value().(bool); !c {
								s.closeAll(string(sig.Path))
							}
						}
					}
				}
			}
		}
	}
}

func (s *Server) closeAll(device string) {
	s.mu.Lock()
	var list []*session
	for _, se := range s.sessions {
		if device == "" || se.device == device {
			list = append(list, se)
		}
	}
	s.mu.Unlock()
	for _, se := range list {
		se.Close()
	}
}

func (s *Server) reap() {
	s.mu.Lock()
	var list []*session
	for _, se := range s.sessions {
		if time.Since(se.last) > idleTimeout {
			list = append(list, se)
		}
	}
	s.mu.Unlock()
	for _, se := range list {
		se.Close()
	}
}

// --- D-Bus objects -----------------------------------------------------------

type objectManager struct{ s *Server }

func (o objectManager) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	char := func(uuid string, flags []string) map[string]map[string]dbus.Variant {
		return map[string]map[string]dbus.Variant{ifaceChar: {
			"UUID":    dbus.MakeVariant(uuid),
			"Service": dbus.MakeVariant(svcPath),
			"Flags":   dbus.MakeVariant(flags),
		}}
	}
	return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		svcPath: {ifaceService: {
			"UUID":    dbus.MakeVariant(ServiceUUID),
			"Primary": dbus.MakeVariant(true),
		}},
		rxPath:   char(RXUUID, []string{"write", "write-without-response"}),
		txPath:   char(TXUUID, []string{"notify"}),
		infoPath: char(InfoUUID, []string{"read"}),
	}, nil
}

type characteristic struct {
	s    *Server
	kind string
}

func notSupported() *dbus.Error {
	return dbus.NewError("org.bluez.Error.NotSupported", nil)
}

func (c characteristic) ReadValue(opts map[string]dbus.Variant) ([]byte, *dbus.Error) {
	if c.kind != "info" {
		return nil, notSupported()
	}
	data := c.s.Info()
	off := 0
	if v, ok := opts["offset"]; ok {
		if o, ok := v.Value().(uint16); ok {
			off = int(o)
		}
	}
	if off > len(data) {
		return nil, dbus.NewError("org.bluez.Error.InvalidOffset", nil)
	}
	return data[off:], nil
}

func (c characteristic) WriteValue(value []byte, opts map[string]dbus.Variant) *dbus.Error {
	if c.kind != "rx" {
		return notSupported()
	}
	// The value was already consumed, in arrival order, by intercept.
	return nil
}

// intercept sees every incoming bus message, in order, before dispatch.
func (s *Server) intercept(msg *dbus.Message) {
	if msg.Type != dbus.TypeMethodCall || len(msg.Body) != 2 {
		return
	}
	if p, _ := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath); p != rxPath {
		return
	}
	if m, _ := msg.Headers[dbus.FieldMember].Value().(string); m != "WriteValue" {
		return
	}
	value, ok := msg.Body[0].([]byte)
	opts, ok2 := msg.Body[1].(map[string]dbus.Variant)
	if !ok || !ok2 {
		return
	}
	device, mtu := "", 23
	if v, ok := opts["device"]; ok {
		if p, ok := v.Value().(dbus.ObjectPath); ok {
			device = string(p)
		}
	}
	if v, ok := opts["mtu"]; ok {
		if m, ok := v.Value().(uint16); ok && m >= 23 {
			mtu = int(m)
		}
	}
	s.receive(device, mtu, value)
}

func (c characteristic) StartNotify() *dbus.Error {
	if c.kind != "tx" {
		return notSupported()
	}
	return nil
}

func (c characteristic) StopNotify() *dbus.Error { return nil }

type advertisement struct{}

func (advertisement) Release() *dbus.Error { return nil }

func (s *Server) export() error {
	if err := s.conn.Export(objectManager{s}, rootPath, "org.freedesktop.DBus.ObjectManager"); err != nil {
		return err
	}
	chars := []struct {
		path  dbus.ObjectPath
		kind  string
		uuid  string
		flags []string
	}{
		{rxPath, "rx", RXUUID, []string{"write", "write-without-response"}},
		{txPath, "tx", TXUUID, []string{"notify"}},
		{infoPath, "info", InfoUUID, []string{"read"}},
	}
	if _, err := prop.Export(s.conn, svcPath, prop.Map{ifaceService: {
		"UUID":    {Value: ServiceUUID},
		"Primary": {Value: true},
	}}); err != nil {
		return err
	}
	for _, c := range chars {
		if err := s.conn.Export(characteristic{s, c.kind}, c.path, ifaceChar); err != nil {
			return err
		}
		if _, err := prop.Export(s.conn, c.path, prop.Map{ifaceChar: {
			"UUID":    {Value: c.uuid},
			"Service": {Value: svcPath},
			"Flags":   {Value: c.flags},
		}}); err != nil {
			return err
		}
	}
	return s.conn.Export(advertisement{}, advPath, ifaceAdv)
}

func (s *Server) findAdapter() (dbus.ObjectPath, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := s.conn.Object(bluez, "/").Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs); err != nil {
		return "", err
	}
	var best dbus.ObjectPath
	for path, ifaces := range objs {
		_, gatt := ifaces["org.bluez.GattManager1"]
		_, adv := ifaces["org.bluez.LEAdvertisingManager1"]
		if gatt && adv && (best == "" || path < best) {
			best = path
		}
	}
	if best == "" {
		return "", errors.New("no Bluetooth LE adapter")
	}
	return best, nil
}

func (s *Server) register() error {
	adapter, err := s.findAdapter()
	if err != nil {
		return err
	}
	obj := s.conn.Object(bluez, adapter)
	set := func(name string, v any) {
		if err := obj.Call(ifaceProps+".Set", 0, "org.bluez.Adapter1", name, dbus.MakeVariant(v)).Err; err != nil {
			s.Log.Debug("adapter property", "name", name, "err", err)
		}
	}
	set("Powered", true)
	set("Pairable", false)
	set("Discoverable", false)
	if err := obj.Call("org.bluez.GattManager1.RegisterApplication", 0, rootPath, map[string]dbus.Variant{}).Err; err != nil {
		if !strings.Contains(err.Error(), "AlreadyExists") {
			return fmt.Errorf("register GATT application: %w", err)
		}
	}
	s.mu.Lock()
	s.adapter = adapter
	s.mu.Unlock()
	return s.registerAdv()
}

// registerAdv starts advertising. The 128-bit service UUID alone takes 18
// of the 31 advertising bytes; if BlueZ cannot fit the name next to it,
// advertise without (apps find the router by service UUID and read its
// name from the Info characteristic).
func (s *Server) registerAdv() error {
	s.mu.Lock()
	adapter := s.adapter
	s.mu.Unlock()
	name := s.Name()
	obj := s.conn.Object(bluez, adapter)
	_ = obj.Call(ifaceProps+".Set", 0, "org.bluez.Adapter1", "Alias", dbus.MakeVariant(name)).Err
	var lastErr error
	for _, withName := range []bool{true, false} {
		props := map[string]*prop.Prop{
			"Type":         {Value: "peripheral"},
			"ServiceUUIDs": {Value: []string{ServiceUUID}},
		}
		if withName {
			// At most 8 bytes of name fit beside the UUID and flags.
			short := name
			if len(short) > 8 {
				short = short[:8]
			}
			props["LocalName"] = &prop.Prop{Value: short}
		}
		if _, err := prop.Export(s.conn, advPath, prop.Map{ifaceAdv: props}); err != nil {
			return err
		}
		err := obj.Call("org.bluez.LEAdvertisingManager1.RegisterAdvertisement", 0, advPath, map[string]dbus.Variant{}).Err
		if err == nil || strings.Contains(err.Error(), "AlreadyExists") {
			s.lastName = name
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("register advertisement: %w", lastErr)
}

func (s *Server) unregisterAdv() {
	s.mu.Lock()
	adapter := s.adapter
	s.mu.Unlock()
	if adapter != "" {
		_ = s.conn.Object(bluez, adapter).Call("org.bluez.LEAdvertisingManager1.UnregisterAdvertisement", 0, advPath).Err
	}
}

// notify sends one fragment to subscribed centrals.
func (s *Server) notify(frag []byte) error {
	return s.conn.Emit(txPath, ifaceProps+".PropertiesChanged", ifaceChar,
		map[string]dbus.Variant{"Value": dbus.MakeVariant(frag)}, []string{})
}

// receive routes an incoming fragment to its session, creating the
// session on a first fragment with an unknown tag.
func (s *Server) receive(device string, mtu int, frag []byte) {
	tag, ok := proto.FragTag(frag)
	if !ok {
		return
	}
	key := fmt.Sprintf("%s#%04x", device, tag)
	s.mu.Lock()
	se := s.sessions[key]
	if se == nil {
		if frag[2]&proto.FragFirst == 0 {
			s.mu.Unlock()
			return
		}
		if len(s.sessions) >= maxSessions {
			var oldest *session
			for _, c := range s.sessions {
				if oldest == nil || c.last.Before(oldest.last) {
					oldest = c
				}
			}
			s.mu.Unlock()
			oldest.Close()
			s.mu.Lock()
		}
		se = &session{srv: s, key: key, device: device, tag: tag, mtu: mtu,
			in: make(chan []byte, 256), closed: make(chan struct{}), last: time.Now()}
		s.sessions[key] = se
		go func() {
			defer se.Close()
			s.Handle(se)
		}()
	}
	se.mtu = mtu
	se.last = time.Now()
	pkt, err := se.re.Add(frag)
	s.mu.Unlock()
	if err != nil {
		s.Log.Debug("bad fragment", "err", err)
		return
	}
	if pkt != nil {
		select {
		case se.in <- pkt:
		case <-se.closed:
		default:
			// The handler is not keeping up. This runs on the bus
			// reader, which must never block: drop the session.
			se.Close()
		}
	}
}
