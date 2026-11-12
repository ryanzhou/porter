// Package proto implements the control protocol spoken between the router
// and its apps: an authenticated, encrypted channel (Noise XX) carrying
// JSON requests, responses and events. It is transport-agnostic; the BLE
// GATT link, the TCP listener on the router's own networks and the local
// debug socket each provide a PacketConn.
//
// The wire format is specified in docs/PROTOCOL.md.
package proto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/flynn/noise"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Prologue binds the handshake to this protocol and version.
const Prologue = "porter/1"

// ControlPort is the TCP port the protocol is served on inside the
// router's own networks. Nothing else on the router uses it: DNS has 53,
// DHCP 67 and 547, SSH 22, and the tunnels only make outgoing connections.
const ControlPort = 7678

const (
	// MaxPacket is the largest packet a PacketConn has to carry.
	MaxPacket = 65535
	// maxChunk is the plaintext carried by one transport packet.
	maxChunk = 8000
	// MaxMessage bounds a reassembled message.
	MaxMessage = 1 << 20
)

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// PacketConn carries whole packets, in order, without loss.
type PacketConn interface {
	ReadPacket() ([]byte, error)
	WritePacket([]byte) error
	Close() error
}

// Identity is a long-term X25519 key pair.
type Identity struct {
	Private []byte
	Public  []byte
}

func NewIdentity() (Identity, error) {
	k, err := suite.GenerateKeypair(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Private: k.Private, Public: k.Public}, nil
}

// LoadIdentity reads the key at path, creating it on first use.
func LoadIdentity(path string) (Identity, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		var f struct {
			Private string `json:"private"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return Identity{}, err
		}
		priv, err := base64.StdEncoding.DecodeString(f.Private)
		if err != nil || len(priv) != 32 {
			return Identity{}, errors.New("identity file is corrupt")
		}
		return IdentityFromPrivate(priv)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	id, err := NewIdentity()
	if err != nil {
		return Identity{}, err
	}
	out, _ := json.Marshal(map[string]string{"private": base64.StdEncoding.EncodeToString(id.Private)})
	return id, config.WriteFileAtomic(path, out, 0o600)
}

// IdentityFromPrivate derives the public half of a private key.
func IdentityFromPrivate(priv []byte) (Identity, error) {
	k, err := suite.GenerateKeypair(&fixedReader{priv})
	if err != nil {
		return Identity{}, err
	}
	return Identity{Private: k.Private, Public: k.Public}, nil
}

type fixedReader struct{ b []byte }

func (f *fixedReader) Read(p []byte) (int, error) {
	if len(f.b) < len(p) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, f.b)
	f.b = f.b[n:]
	return n, nil
}

// Session is an established secure channel.
type Session struct {
	pc PacketConn
	// Remote is the peer's static public key, authenticated by the
	// handshake.
	Remote []byte

	wmu  sync.Mutex
	send *noise.CipherState
	recv *noise.CipherState
}

func handshake(pc PacketConn, id Identity, initiator bool, random io.Reader) (*Session, error) {
	if random == nil {
		random = rand.Reader
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   suite,
		Random:        random,
		Pattern:       noise.HandshakeXX,
		Initiator:     initiator,
		Prologue:      []byte(Prologue),
		StaticKeypair: noise.DHKey{Private: id.Private, Public: id.Public},
	})
	if err != nil {
		return nil, err
	}
	var c1, c2 *noise.CipherState
	write := func() error {
		var msg []byte
		msg, c1, c2, err = hs.WriteMessage(nil, nil)
		if err != nil {
			return err
		}
		return pc.WritePacket(msg)
	}
	read := func() error {
		msg, err := pc.ReadPacket()
		if err != nil {
			return err
		}
		_, c1, c2, err = hs.ReadMessage(nil, msg)
		return err
	}
	steps := []func() error{read, write, read}
	if initiator {
		steps = []func() error{write, read, write}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, fmt.Errorf("handshake: %w", err)
		}
	}
	if c1 == nil || c2 == nil {
		return nil, errors.New("handshake: incomplete")
	}
	s := &Session{pc: pc, Remote: hs.PeerStatic()}
	// c1 encrypts initiator -> responder, c2 the reverse.
	if initiator {
		s.send, s.recv = c1, c2
	} else {
		s.send, s.recv = c2, c1
	}
	return s, nil
}

// Accept runs the responder (router) side of the handshake.
func Accept(pc PacketConn, id Identity) (*Session, error) { return handshake(pc, id, false, nil) }

// Dial runs the initiator (app) side of the handshake.
func Dial(pc PacketConn, id Identity) (*Session, error) { return handshake(pc, id, true, nil) }

// Send encrypts and sends one message, split over as many packets as
// needed. Each packet's plaintext starts with a flag byte: 1 when more
// chunks follow, 0 on the last.
func (s *Session) Send(msg []byte) error {
	if len(msg) > MaxMessage {
		return errors.New("message too large")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	for {
		n := min(len(msg), maxChunk)
		rec := make([]byte, 0, 1+n)
		if n < len(msg) {
			rec = append(rec, 1)
		} else {
			rec = append(rec, 0)
		}
		rec = append(rec, msg[:n]...)
		ct, err := s.send.Encrypt(nil, nil, rec)
		if err != nil {
			return err
		}
		if err := s.pc.WritePacket(ct); err != nil {
			return err
		}
		msg = msg[n:]
		if len(msg) == 0 {
			return nil
		}
	}
}

// Recv returns the next complete message.
func (s *Session) Recv() ([]byte, error) {
	var msg []byte
	for {
		ct, err := s.pc.ReadPacket()
		if err != nil {
			return nil, err
		}
		rec, err := s.recv.Decrypt(nil, nil, ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt: %w", err)
		}
		if len(rec) < 1 {
			return nil, errors.New("empty record")
		}
		msg = append(msg, rec[1:]...)
		if len(msg) > MaxMessage {
			return nil, errors.New("message too large")
		}
		if rec[0] == 0 {
			return msg, nil
		}
	}
}

func (s *Session) Close() error { return s.pc.Close() }
