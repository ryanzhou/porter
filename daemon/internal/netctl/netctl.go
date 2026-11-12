// Package netctl serves the control protocol over TCP inside the router's
// own networks: the same Noise session and the same calls as over
// Bluetooth, for a phone that is on the router's Wi-Fi and not within
// Bluetooth range of it.
//
// Being on the network proves less than being next to the router, so this
// way in is for phones that are already paired, and for nothing else: a
// session whose key is not a pairing ends as soon as the handshake has
// shown the key. Pairing stays with Bluetooth.
package netctl

import (
	"bytes"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/proto"
)

const (
	// HandshakeTimeout is how long a connection has to show its key.
	HandshakeTimeout = 10 * time.Second
	// MaxHandshakes bounds the connections that have not shown a key yet,
	// which anything on the network can open.
	MaxHandshakes = 4
	// MaxSessions bounds the sessions of paired phones.
	MaxSessions = 8
	// MaxPerPhone bounds the sessions of one phone. A phone needs one; it
	// has a second while it moves from a connection that went quiet to a
	// new one.
	MaxPerPhone = 2

	// A connection whose far end has gone without saying so (a phone out
	// of range) is found out by keepalive probes after this long idle...
	keepAliveIdle     = 15 * time.Second
	keepAliveInterval = 5 * time.Second
	keepAliveCount    = 3
	// ...and, when something was being sent to it, after this long
	// without an acknowledgement.
	unackedTimeout = 30 * time.Second
)

// Server is the listener and the sessions that came in through it.
type Server struct {
	Log      *slog.Logger
	Identity proto.Identity
	// Known reports whether a key belongs to a paired phone.
	Known func(key []byte) bool
	// Allowed reports whether a connection from remote to the router's
	// address local may control the router: the firewall's rule, asked
	// again here because the listener exists before the firewall's first
	// ruleset does.
	Allowed func(local, remote netip.Addr) bool
	// Serve runs the session of a paired phone until it ends.
	Serve func(s *proto.Session)
	// HandshakeTimeout replaces the constant of that name (tests).
	HandshakeTimeout time.Duration

	mu sync.Mutex
	ln net.Listener
	// closed: Close has been called, and a connection accepted just
	// before it is not to be taken up any more.
	closed     bool
	handshakes map[net.Conn]struct{}
	sessions   []*session // oldest first
}

type session struct {
	key  []byte
	conn net.Conn
}

// Listen starts accepting connections on addr.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.handshakes = map[net.Conn]struct{}{}
	s.mu.Unlock()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	return nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close stops listening and ends every connection that came in.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.ln.Close()
	for conn := range s.handshakes {
		conn.Close()
	}
	for _, se := range s.sessions {
		se.conn.Close()
	}
}

func addrOf(a net.Addr) netip.Addr {
	if tcp, ok := a.(*net.TCPAddr); ok {
		ip, _ := netip.AddrFromSlice(tcp.IP)
		return ip.Unmap()
	}
	return netip.Addr{}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	if s.Allowed != nil && !s.Allowed(addrOf(conn.LocalAddr()), addrOf(conn.RemoteAddr())) {
		s.Log.Debug("control connection from outside the router's networks refused", "from", conn.RemoteAddr())
		return
	}
	s.mu.Lock()
	if s.closed || len(s.handshakes) >= MaxHandshakes {
		s.mu.Unlock()
		return
	}
	s.handshakes[conn] = struct{}{}
	s.mu.Unlock()

	keepAlive(conn)
	timeout := s.HandshakeTimeout
	if timeout == 0 {
		timeout = HandshakeTimeout
	}
	conn.SetDeadline(time.Now().Add(timeout))
	sess, err := proto.Accept(proto.NewStreamConn(conn), s.Identity)

	s.mu.Lock()
	delete(s.handshakes, conn)
	if err != nil {
		s.mu.Unlock()
		s.Log.Debug("control handshake over the network failed", "from", conn.RemoteAddr(), "err", err)
		return
	}
	if !s.Known(sess.Remote) {
		s.mu.Unlock()
		s.Log.Debug("control connection over the network from a phone that is not paired", "from", conn.RemoteAddr())
		return
	}
	// A phone that already has as many sessions as one phone gets has
	// lost the oldest without the router having noticed yet.
	var own []*session
	for _, se := range s.sessions {
		if bytes.Equal(se.key, sess.Remote) {
			own = append(own, se)
		}
	}
	if len(own) >= MaxPerPhone {
		own[0].conn.Close()
		s.remove(own[0])
	}
	if len(s.sessions) >= MaxSessions {
		s.mu.Unlock()
		s.Log.Warn("too many control sessions over the network; a new one was refused")
		return
	}
	me := &session{key: sess.Remote, conn: conn}
	s.sessions = append(s.sessions, me)
	s.mu.Unlock()

	conn.SetDeadline(time.Time{})
	s.Serve(sess)

	s.mu.Lock()
	s.remove(me)
	s.mu.Unlock()
}

// remove must be called with the lock held.
func (s *Server) remove(se *session) {
	for i, other := range s.sessions {
		if other == se {
			s.sessions = append(s.sessions[:i], s.sessions[i+1:]...)
			return
		}
	}
}

func keepAlive(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	tcp.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: keepAliveIdle, Interval: keepAliveInterval, Count: keepAliveCount})
	if raw, err := tcp.SyscallConn(); err == nil {
		raw.Control(func(fd uintptr) {
			unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(unackedTimeout/time.Millisecond))
		})
	}
}
