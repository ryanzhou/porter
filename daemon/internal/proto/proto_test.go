package proto

import (
	"bytes"
	"crypto/rand"
	"net"
	"path/filepath"
	"testing"
)

func pipePair() (PacketConn, PacketConn) {
	a, b := net.Pipe()
	return NewStreamConn(a), NewStreamConn(b)
}

func TestSessionRoundTrip(t *testing.T) {
	router, _ := NewIdentity()
	app, _ := NewIdentity()
	ca, cb := pipePair()
	type res struct {
		s   *Session
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := Accept(cb, router)
		ch <- res{s, err}
	}()
	cs, err := Dial(ca, app)
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !bytes.Equal(cs.Remote, router.Public) || !bytes.Equal(r.s.Remote, app.Public) {
		t.Fatal("static keys not authenticated")
	}
	// A message larger than one chunk and one packet.
	big := make([]byte, 200_000)
	rand.Read(big)
	go func() {
		cs.Send([]byte("hello"))
		cs.Send(big)
	}()
	if m, err := r.s.Recv(); err != nil || string(m) != "hello" {
		t.Fatalf("recv: %q %v", m, err)
	}
	if m, err := r.s.Recv(); err != nil || !bytes.Equal(m, big) {
		t.Fatalf("large message mismatch: %v", err)
	}
	go r.s.Send([]byte("back"))
	if m, err := cs.Recv(); err != nil || string(m) != "back" {
		t.Fatalf("recv: %q %v", m, err)
	}
}

func TestFragmentation(t *testing.T) {
	pkt := make([]byte, 1000)
	rand.Read(pkt)
	for _, mtu := range []int{20, 23, 182, 244, 509, 2000} {
		frags := Fragment(0xbeef, pkt, mtu)
		var r Reassembler
		var got []byte
		for i, f := range frags {
			if len(f) > mtu {
				t.Fatalf("mtu %d: fragment of %d bytes", mtu, len(f))
			}
			if tag, ok := FragTag(f); !ok || tag != 0xbeef {
				t.Fatal("tag lost")
			}
			out, err := r.Add(f)
			if err != nil {
				t.Fatal(err)
			}
			if (out != nil) != (i == len(frags)-1) {
				t.Fatalf("mtu %d: packet completed at fragment %d of %d", mtu, i, len(frags))
			}
			got = out
		}
		if !bytes.Equal(got, pkt) {
			t.Fatalf("mtu %d: reassembly mismatch", mtu)
		}
	}
	// A continuation without a start is rejected; a new start resyncs.
	var r Reassembler
	if _, err := r.Add([]byte{0, 0, 0, 1}); err == nil {
		t.Error("orphan fragment accepted")
	}
	if out, err := r.Add([]byte{0, 0, FragFirst | FragLast, 7}); err != nil || !bytes.Equal(out, []byte{7}) {
		t.Errorf("resync failed: %v %v", out, err)
	}
}

func TestIdentityAndPairings(t *testing.T) {
	dir := t.TempDir()
	id1, err := LoadIdentity(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := LoadIdentity(filepath.Join(dir, "identity.json"))
	if err != nil || !bytes.Equal(id1.Public, id2.Public) {
		t.Fatal("identity not stable across loads")
	}
	p, _ := LoadPairings(filepath.Join(dir, "pairings.json"))
	key := make([]byte, 32)
	rand.Read(key)
	if p.Known(key) {
		t.Fatal("unknown key accepted")
	}
	if err := p.Add(key, "Phone"); err != nil {
		t.Fatal(err)
	}
	p2, _ := LoadPairings(filepath.Join(dir, "pairings.json"))
	if !p2.Known(key) || p2.Count() != 1 {
		t.Fatal("pairing not persisted")
	}
	if ok, _ := p2.Remove(p2.List()[0].Key); !ok || p2.Known(key) {
		t.Fatal("pairing not removed")
	}
}
