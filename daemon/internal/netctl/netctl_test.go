package netctl

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/client"
	"github.com/ryanzhou/porter/daemon/internal/proto"
)

type rig struct {
	t      *testing.T
	srv    *Server
	router proto.Identity
	mu     sync.Mutex
	paired [][]byte
	// served counts the sessions handed to Serve so far.
	served atomic.Int32
}

// newRig starts a server on a port of its own. setup may change the
// server before it listens.
func newRig(t *testing.T, setup ...func(*Server)) *rig {
	t.Helper()
	r := &rig{t: t}
	var err error
	if r.router, err = proto.NewIdentity(); err != nil {
		t.Fatal(err)
	}
	r.srv = &Server{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Identity: r.router,
		Known: func(key []byte) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			for _, k := range r.paired {
				if bytes.Equal(k, key) {
					return true
				}
			}
			return false
		},
		// Every call is answered with an empty result.
		Serve: func(s *proto.Session) {
			r.served.Add(1)
			for {
				msg, err := s.Recv()
				if err != nil {
					return
				}
				var req proto.Request
				if json.Unmarshal(msg, &req) != nil {
					return
				}
				b, _ := json.Marshal(proto.Response{ID: req.ID, Result: struct{}{}})
				if s.Send(b) != nil {
					return
				}
			}
		},
	}
	for _, f := range setup {
		f(r.srv)
	}
	if err := r.srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) phone(paired bool) proto.Identity {
	r.t.Helper()
	id, err := proto.NewIdentity()
	if err != nil {
		r.t.Fatal(err)
	}
	if paired {
		r.mu.Lock()
		r.paired = append(r.paired, id.Public)
		r.mu.Unlock()
	}
	return id
}

func (r *rig) dial() net.Conn {
	r.t.Helper()
	conn, err := net.Dial("tcp", r.srv.Addr().String())
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { conn.Close() })
	return conn
}

// connect opens a session as the phone with the given identity.
func (r *rig) connect(id proto.Identity) *client.Client {
	r.t.Helper()
	c, err := client.New(proto.NewStreamConn(r.dial()), id)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { c.Close() })
	return c
}

// closedWithin reports whether the far end closes conn within d without
// having sent anything.
func closedWithin(conn net.Conn, d time.Duration) bool {
	conn.SetReadDeadline(time.Now().Add(d))
	_, err := conn.Read(make([]byte, 1))
	return err == io.EOF
}

func TestPairedPhoneGetsASession(t *testing.T) {
	r := newRig(t)
	c := r.connect(r.phone(true))
	if !bytes.Equal(c.RouterKey(), r.router.Public) {
		t.Error("the session is not with the router's key")
	}
	for i := 0; i < 3; i++ {
		if err := c.Call("ping", nil, nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestUnknownKeyIsClosedAfterTheHandshake(t *testing.T) {
	r := newRig(t)
	// The handshake itself completes: it is what shows the key.
	c := r.connect(r.phone(false))
	done := make(chan error, 1)
	go func() { done <- c.Call("hello", nil, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a phone that is not paired had a call answered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session of a phone that is not paired stayed open")
	}
	if n := r.served.Load(); n != 0 {
		t.Errorf("%d sessions were served to a phone that is not paired", n)
	}
}

func TestHandshakeDeadline(t *testing.T) {
	r := newRig(t, func(s *Server) { s.HandshakeTimeout = 300 * time.Millisecond })
	// Connects and says nothing.
	silent := r.dial()
	start := time.Now()
	if !closedWithin(silent, 3*time.Second) {
		t.Fatal("a connection that never started the handshake was kept")
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Errorf("closed after %v, before the deadline", took)
	}
	// Starts the handshake and stops halfway.
	half := r.dial()
	// The first handshake message is an ephemeral public key, and any
	// public key will do for one.
	first := append([]byte{0, 32}, r.phone(false).Public...)
	if _, err := half.Write(first); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2+96)
	half.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(half, reply); err != nil {
		t.Fatalf("no second handshake message: %v", err)
	}
	if !closedWithin(half, 3*time.Second) {
		t.Error("a connection that stopped halfway through the handshake was kept")
	}
	// An established session has no deadline.
	c := r.connect(r.phone(true))
	time.Sleep(2 * r.srv.HandshakeTimeout)
	if err := c.Call("ping", nil, nil); err != nil {
		t.Errorf("an established session ended with the handshake deadline: %v", err)
	}
}

func TestHandshakeCap(t *testing.T) {
	r := newRig(t)
	// As many silent connections as may wait at once...
	for i := 0; i < MaxHandshakes; i++ {
		if closedWithin(r.dial(), 100*time.Millisecond) {
			t.Fatalf("connection %d was closed while there was room", i)
		}
	}
	// ...and the next is turned away at once, whoever it is.
	if !closedWithin(r.dial(), 2*time.Second) {
		t.Error("a connection beyond the cap was left waiting")
	}
}

func TestSessionCaps(t *testing.T) {
	r := newRig(t)
	alive := func(c *client.Client) bool {
		done := make(chan error, 1)
		go func() { done <- c.Call("ping", nil, nil) }()
		select {
		case err := <-done:
			return err == nil
		case <-time.After(3 * time.Second):
			return false
		}
	}
	// One phone: a third session takes the place of its oldest.
	phone := r.phone(true)
	first, second := r.connect(phone), r.connect(phone)
	if !alive(first) || !alive(second) {
		t.Fatal("a phone's two sessions do not both work")
	}
	third := r.connect(phone)
	if !alive(third) || !alive(second) {
		t.Error("the phone's newer sessions do not work")
	}
	if alive(first) {
		t.Error("the phone's oldest session was kept beside two newer ones")
	}
	// Other phones fill what is left, and then no more are let in.
	open := []*client.Client{second, third}
	for len(open) < MaxSessions {
		c := r.connect(r.phone(true))
		if !alive(c) {
			t.Fatalf("session %d does not work", len(open))
		}
		open = append(open, c)
	}
	if alive(r.connect(r.phone(true))) {
		t.Error("a session beyond the cap was served")
	}
	for i, c := range open {
		if !alive(c) {
			t.Errorf("session %d was lost to one that was refused", i)
		}
	}
	// A session that ends makes room again.
	open[0].Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if alive(r.connect(r.phone(true))) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no room after a session ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestOnlyFromAllowedAddresses(t *testing.T) {
	var asked atomic.Int32
	allow := atomic.Bool{}
	r := newRig(t, func(s *Server) {
		s.Allowed = func(local, remote netip.Addr) bool {
			asked.Add(1)
			if local != netip.MustParseAddr("127.0.0.1") || remote != netip.MustParseAddr("127.0.0.1") {
				t.Errorf("asked about %v -> %v", remote, local)
			}
			return allow.Load()
		}
	})
	if !closedWithin(r.dial(), 2*time.Second) {
		t.Error("a connection from an address that is not allowed was kept")
	}
	allow.Store(true)
	if err := r.connect(r.phone(true)).Call("ping", nil, nil); err != nil {
		t.Errorf("from an allowed address: %v", err)
	}
	if asked.Load() != 2 {
		t.Errorf("asked %d times about 2 connections", asked.Load())
	}
}

func TestCloseEndsEverything(t *testing.T) {
	r := newRig(t)
	c := r.connect(r.phone(true))
	if err := c.Call("ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	waiting := r.dial()
	addr := r.srv.Addr().String()
	r.srv.Close()
	if err := c.Call("ping", nil, nil); err == nil {
		t.Error("a session outlived the listener")
	}
	if !closedWithin(waiting, 2*time.Second) {
		t.Error("a connection in its handshake outlived the listener")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Error("something still listens")
	}
}
