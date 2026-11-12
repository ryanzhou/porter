package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
)

// Request is a call from the app.
type Request struct {
	ID     uint32          `json:"id"`
	Method string          `json:"m"`
	Params json.RawMessage `json:"p,omitempty"`
}

// Response answers the request with the same ID.
type Response struct {
	ID     uint32 `json:"id"`
	Result any    `json:"r,omitempty"`
	Error  *Error `json:"e,omitempty"`
}

// Event is an unsolicited message from the router.
type Event struct {
	Event string `json:"ev"`
	Data  any    `json:"d,omitempty"`
}

// Error codes are stable identifiers the app can act on.
type Error struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func Errf(code, msg string) *Error { return &Error{Code: code, Msg: msg} }

const (
	ErrUnpaired     = "unpaired"       // session is not from a paired phone
	ErrPairingClosd = "pairing_closed" // pairing window is not open
	ErrInvalid      = "invalid"        // bad parameters or config
	ErrNotFound     = "not_found"
	ErrConflict     = "conflict" // config changed since it was read
	ErrInternal     = "internal"
	ErrUnknown      = "unknown_method"
)

// StreamConn adapts a byte stream to a PacketConn with a 2-byte big-endian
// length prefix. It is used by the TCP listener and the local debug
// socket.
type StreamConn struct {
	c   net.Conn
	wmu sync.Mutex
}

func NewStreamConn(c net.Conn) *StreamConn { return &StreamConn{c: c} }

func (s *StreamConn) ReadPacket() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(s.c, hdr[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(s.c, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *StreamConn) WritePacket(p []byte) error {
	if len(p) > MaxPacket {
		return errors.New("packet too large")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	buf := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(buf, uint16(len(p)))
	copy(buf[2:], p)
	_, err := s.c.Write(buf)
	return err
}

func (s *StreamConn) Close() error { return s.c.Close() }

// --- BLE link framing ------------------------------------------------------
//
// A GATT write or notification carries one fragment:
//
//	byte 0-1  session tag, chosen by the app, echoed by the router
//	byte 2    flags: 0x80 first fragment of a packet, 0x40 last fragment
//	byte 3..  payload

const (
	FragFirst  = 0x80
	FragLast   = 0x40
	FragHeader = 3
)

// Fragment splits a packet into link fragments of at most mtu bytes.
func Fragment(tag uint16, packet []byte, mtu int) [][]byte {
	room := mtu - FragHeader
	if room < 1 {
		room = 1
	}
	var out [][]byte
	first := true
	for {
		n := min(len(packet), room)
		f := make([]byte, FragHeader+n)
		binary.BigEndian.PutUint16(f, tag)
		if first {
			f[2] |= FragFirst
		}
		if n == len(packet) {
			f[2] |= FragLast
		}
		copy(f[FragHeader:], packet[:n])
		out = append(out, f)
		packet = packet[n:]
		first = false
		if len(packet) == 0 {
			return out
		}
	}
}

// Reassembler rebuilds packets from fragments of one session.
type Reassembler struct {
	buf    []byte
	active bool
}

// Add consumes a fragment (without its tag check) and returns a complete
// packet when the fragment was the last one.
func (r *Reassembler) Add(frag []byte) ([]byte, error) {
	if len(frag) < FragHeader {
		return nil, errors.New("short fragment")
	}
	flags := frag[2]
	if flags&FragFirst != 0 {
		r.buf = r.buf[:0]
		r.active = true
	} else if !r.active {
		return nil, errors.New("fragment without start")
	}
	r.buf = append(r.buf, frag[FragHeader:]...)
	if len(r.buf) > MaxPacket {
		r.active = false
		return nil, errors.New("packet too large")
	}
	if flags&FragLast != 0 {
		r.active = false
		out := append([]byte(nil), r.buf...)
		return out, nil
	}
	return nil, nil
}

// FragTag returns the session tag of a fragment.
func FragTag(frag []byte) (uint16, bool) {
	if len(frag) < FragHeader {
		return 0, false
	}
	return binary.BigEndian.Uint16(frag), true
}
