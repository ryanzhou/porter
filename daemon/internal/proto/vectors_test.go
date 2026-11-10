package proto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

// The vectors file is shared with the iOS implementation's tests: both
// sides must produce exactly these bytes from these keys.
const vectorsPath = "../../../ios/PorterKit/Tests/PorterKitTests/Resources/vectors.json"

var update = flag.Bool("update", false, "rewrite the protocol test vectors")

type vectors struct {
	Prologue      string       `json:"prologue"`
	AppStatic     string       `json:"app_static_private"`
	AppPublic     string       `json:"app_static_public"`
	AppEphemeral  string       `json:"app_ephemeral_private"`
	RtrStatic     string       `json:"router_static_private"`
	RtrPublic     string       `json:"router_static_public"`
	RtrEphemeral  string       `json:"router_ephemeral_private"`
	Handshake     []string     `json:"handshake"` // msg1 (app), msg2 (router), msg3 (app)
	AppToRouter   []vectorMsg  `json:"app_to_router"`
	RouterToApp   []vectorMsg  `json:"router_to_app"`
	Fragmentation vectorFrames `json:"fragmentation"`
}

type vectorMsg struct {
	Plaintext string   `json:"plaintext"` // hex of the message
	Packets   []string `json:"packets"`   // hex of each encrypted packet
}

type vectorFrames struct {
	Tag       uint16   `json:"tag"`
	MTU       int      `json:"mtu"`
	Packet    string   `json:"packet"`
	Fragments []string `json:"fragments"`
}

// recorder captures packets while relaying them to a peer queue.
type recorder struct {
	in  chan []byte
	out chan []byte
	log *[]string
}

func (r *recorder) ReadPacket() ([]byte, error) { return <-r.in, nil }
func (r *recorder) WritePacket(p []byte) error {
	*r.log = append(*r.log, hex.EncodeToString(p))
	r.out <- append([]byte(nil), p...)
	return nil
}
func (r *recorder) Close() error { return nil }

func seq(start byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func generateVectors(t *testing.T) vectors {
	appStatic, _ := IdentityFromPrivate(seq(0x00))
	rtrStatic, _ := IdentityFromPrivate(seq(0x40))
	appEph, rtrEph := seq(0x80), seq(0xc0)

	a2r, r2a := make(chan []byte, 64), make(chan []byte, 64)
	var appLog, rtrLog []string
	appConn := &recorder{in: r2a, out: a2r, log: &appLog}
	rtrConn := &recorder{in: a2r, out: r2a, log: &rtrLog}

	type res struct {
		s   *Session
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := handshake(rtrConn, rtrStatic, false, bytes.NewReader(rtrEph))
		ch <- res{s, err}
	}()
	app, err := handshake(appConn, appStatic, true, bytes.NewReader(appEph))
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	v := vectors{
		Prologue:  Prologue,
		AppStatic: hex.EncodeToString(appStatic.Private), AppPublic: hex.EncodeToString(appStatic.Public),
		AppEphemeral: hex.EncodeToString(appEph),
		RtrStatic:    hex.EncodeToString(rtrStatic.Private), RtrPublic: hex.EncodeToString(rtrStatic.Public),
		RtrEphemeral: hex.EncodeToString(rtrEph),
		Handshake:    []string{appLog[0], rtrLog[0], appLog[1]},
	}
	appLog, rtrLog = nil, nil

	long := make([]byte, 8500) // spans two packets
	for i := range long {
		long[i] = byte(i * 7)
	}
	for _, msg := range [][]byte{[]byte(`{"id":1,"m":"hello"}`), {}, long} {
		before := len(appLog)
		if err := app.Send(msg); err != nil {
			t.Fatal(err)
		}
		got, err := r.s.Recv()
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("round trip: %v", err)
		}
		v.AppToRouter = append(v.AppToRouter, vectorMsg{hex.EncodeToString(msg), append([]string(nil), appLog[before:]...)})
	}
	for _, msg := range [][]byte{[]byte(`{"id":1,"r":{}}`), long[:300]} {
		before := len(rtrLog)
		if err := r.s.Send(msg); err != nil {
			t.Fatal(err)
		}
		got, err := app.Recv()
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("round trip: %v", err)
		}
		v.RouterToApp = append(v.RouterToApp, vectorMsg{hex.EncodeToString(msg), append([]string(nil), rtrLog[before:]...)})
	}

	pkt := long[:300]
	v.Fragmentation = vectorFrames{Tag: 0xa1b2, MTU: 100, Packet: hex.EncodeToString(pkt)}
	for _, f := range Fragment(0xa1b2, pkt, 100) {
		v.Fragmentation.Fragments = append(v.Fragmentation.Fragments, hex.EncodeToString(f))
	}
	return v
}

func TestVectors(t *testing.T) {
	v := generateVectors(t)
	got, _ := json.MarshalIndent(v, "", " ")
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(vectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/proto -run TestVectors -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("protocol output no longer matches the shared test vectors; if the change is intended, regenerate them with -update and update the iOS implementation")
	}
}
