package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rig is a pretend router: files for partitions, a directory for the
// selector partition, and a web server with a release on it.
type rig struct {
	t        *testing.T
	dir      string
	sys      *System
	srv      *httptest.Server
	manifest *Manifest
	bootImg  []byte
	rootImg  []byte
	restarts []bool
	cmdline  string // what ended up in the target boot partition
	damage   string // a file name to serve damaged
}

const slotSize = 3 << 20

func init() { retryWait = 0 }

func newRig(t *testing.T, runningRoot int) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir()}
	parts := filepath.Join(r.dir, "part-")
	for _, n := range []int{1, 2, 3, 5, 6, 7} {
		if err := os.WriteFile(fmt.Sprintf("%s%02d", parts, n), bytes.Repeat([]byte{0xee}, slotSize), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selector := filepath.Join(r.dir, "selector")
	os.MkdirAll(selector, 0o755)
	os.WriteFile(filepath.Join(r.dir, "cmdline"), []byte(fmt.Sprintf("console=tty1 root=PARTUUID=504f5254-%02x ro rootwait\n", runningRoot)), 0o644)
	r.sys = &System{
		PartPrefix: parts, DiskID: "504f5254",
		Cmdline:          filepath.Join(r.dir, "cmdline"),
		TryFlag:          filepath.Join(r.dir, "tryboot"),
		Selector:         selector,
		Pending:          filepath.Join(r.dir, "pending"),
		PublicKey:        filepath.Join(r.dir, "update.pub"),
		WritableSelector: func(fn func() error) error { return fn() },
		WithBoot: func(dev string, fn func(dir string) error) error {
			d := filepath.Join(r.dir, "bootmnt")
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, "cmdline.txt"), []byte("console=serial0,115200 root=PARTUUID=504f5254-05 rootfstype=ext4 ro quiet\n"), 0o644)
			err := fn(d)
			b, _ := os.ReadFile(filepath.Join(d, "cmdline.txt"))
			r.cmdline = string(b)
			return err
		},
		Restart: func(try bool) error { r.restarts = append(r.restarts, try); return nil },
	}

	// A release: two images, compressed and described the way the image
	// build does it.
	rel := filepath.Join(r.dir, "release")
	os.MkdirAll(rel, 0o755)
	r.bootImg = bytes.Repeat([]byte("boot-image "), 40000)
	r.rootImg = bytes.Repeat([]byte("root file system "), 150000)
	os.WriteFile(filepath.Join(rel, "porter-v1.2.0-boot.vfat"), r.bootImg, 0o644)
	os.WriteFile(filepath.Join(rel, "porter-v1.2.0-root.ext4"), r.rootImg, 0o644)
	m, err := Describe("v1.2.0", 1700000000, filepath.Join(rel, "porter-v1.2.0-boot.vfat"), filepath.Join(rel, "porter-v1.2.0-root.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	r.manifest = m
	os.WriteFile(filepath.Join(rel, "update.json"), m.Encode(), 0o644)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/")
		b, err := os.ReadFile(filepath.Join(rel, name))
		if err != nil {
			http.NotFound(w, req)
			return
		}
		if name == r.damage {
			b = b[:len(b)/2]
		}
		w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) base() string { return r.srv.URL + "/" }

func (r *rig) part(n int) []byte {
	b, err := os.ReadFile(r.sys.part(n))
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *rig) selector() string {
	b, _ := os.ReadFile(filepath.Join(r.sys.Selector, "autoboot.txt"))
	return string(b)
}

func TestInstallAndSettle(t *testing.T) {
	r := newRig(t, 5) // running from slot A
	if !r.sys.Supported() {
		t.Fatal("the two-slot layout was not recognised")
	}
	m, err := r.sys.Fetch(context.Background(), r.srv.Client(), r.base())
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	last := -1
	err = r.sys.Install(context.Background(), r.srv.Client(), r.base(), m, "v1.1.0", func(phase string, percent int) {
		if percent < last {
			t.Errorf("progress went backwards: %d after %d", percent, last)
		}
		last = percent
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(phases, ",") != "download,verify,restart" || last != 100 {
		t.Errorf("progress: %v, ended at %d", phases, last)
	}
	// The update is in slot B, and only there.
	if got := r.part(6); !bytes.Equal(got[:len(r.rootImg)], r.rootImg) || got[len(r.rootImg)] != 0xee {
		t.Error("root image not written to slot B as it is")
	}
	if got := r.part(3); !bytes.Equal(got[:len(r.bootImg)], r.bootImg) {
		t.Error("boot image not written to slot B")
	}
	for _, n := range []int{1, 2, 5, 7} {
		if !bytes.Equal(r.part(n), bytes.Repeat([]byte{0xee}, slotSize)) {
			t.Errorf("partition %d was touched", n)
		}
	}
	if want := "console=serial0,115200 root=PARTUUID=504f5254-06 rootfstype=ext4 ro quiet\n"; r.cmdline != want {
		t.Errorf("kernel command line of slot B: %q", r.cmdline)
	}
	// Slot A stays the default; B is the one to try.
	if want := "[all]\ntryboot_a_b=1\nboot_partition=2\n[tryboot]\nboot_partition=3\n"; r.selector() != want {
		t.Errorf("selector before the trial:\n%s", r.selector())
	}
	if len(r.restarts) != 1 || !r.restarts[0] {
		t.Errorf("restarts: %v", r.restarts)
	}
	if !r.sys.Unsettled() {
		t.Error("nothing left for the next start to confirm")
	}

	// The trial start: slot B is running and the boot loader says so.
	os.WriteFile(r.sys.Cmdline, []byte("root=PARTUUID=504f5254-06 ro\n"), 0o644)
	flag := make([]byte, 4)
	binary.BigEndian.PutUint32(flag, 1)
	os.WriteFile(r.sys.TryFlag, flag, 0o644)
	if !r.sys.Trial() {
		t.Error("trial start not recognised")
	}
	out, err := r.sys.Settle()
	if err != nil || !out.Updated || out.Failed || out.Version != "v1.2.0" {
		t.Fatalf("settle: %+v, %v", out, err)
	}
	if want := "[all]\ntryboot_a_b=1\nboot_partition=3\n[tryboot]\nboot_partition=3\n"; r.selector() != want {
		t.Errorf("selector after confirming:\n%s", r.selector())
	}
	if r.sys.Unsettled() {
		t.Error("still waiting to be confirmed")
	}
	if out, _ := r.sys.Settle(); out.Updated || out.Failed {
		t.Errorf("settling twice: %+v", out)
	}
}

func TestTrialThatDidNotHold(t *testing.T) {
	r := newRig(t, 6) // running from slot B this time
	m, _ := r.sys.Fetch(context.Background(), r.srv.Client(), r.base())
	if err := r.sys.Install(context.Background(), r.srv.Client(), r.base(), m, "v1.1.0", func(string, int) {}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.part(5)[:len(r.rootImg)], r.rootImg) || !strings.Contains(r.cmdline, "root=PARTUUID=504f5254-05 ") {
		t.Error("update not written to slot A")
	}
	// The new system never came up; the machine restarted and is on
	// slot B again.
	out, err := r.sys.Settle()
	if err != nil || !out.Failed || out.Updated || out.Version != "v1.2.0" {
		t.Fatalf("settle: %+v, %v", out, err)
	}
	if want := "[all]\ntryboot_a_b=1\nboot_partition=3\n[tryboot]\nboot_partition=3\n"; r.selector() != want {
		t.Errorf("selector after a failed trial:\n%s", r.selector())
	}
}

func TestDamagedDownload(t *testing.T) {
	r := newRig(t, 5)
	m, _ := r.sys.Fetch(context.Background(), r.srv.Client(), r.base())
	r.damage = m.Root.Name
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := r.sys.Install(ctx, r.srv.Client(), r.base(), m, "v1.1.0", func(string, int) {})
	if err == nil {
		t.Fatal("a damaged image was installed")
	}
	if len(r.restarts) != 0 || r.sys.Unsettled() {
		t.Error("the router would restart into a damaged update")
	}
	// The half-written slot must not be startable, even as a trial.
	if want := "[all]\ntryboot_a_b=1\nboot_partition=2\n[tryboot]\nboot_partition=2\n"; r.selector() != want {
		t.Errorf("selector after a failed installation:\n%s", r.selector())
	}

	// Same when the image is intact but not the one described.
	r.damage = ""
	wrong := *m
	wrong.Root.SHA256 = strings.Repeat("0", 64)
	if err := r.sys.Install(context.Background(), r.srv.Client(), r.base(), &wrong, "v1.1.0", func(string, int) {}); err == nil || len(r.restarts) != 0 {
		t.Errorf("an image with the wrong hash was installed: %v", err)
	}
	// And when it would not fit.
	big := *m
	big.Root.Size = slotSize + 1
	if err := r.sys.Install(context.Background(), r.srv.Client(), r.base(), &big, "v1.1.0", func(string, int) {}); err == nil {
		t.Error("an image larger than its partition was accepted")
	}
}

func TestSignature(t *testing.T) {
	r := newRig(t, 5)
	pub, priv, _ := ed25519.GenerateKey(nil)
	seed := base64.StdEncoding.EncodeToString(priv.Seed())
	rel := filepath.Join(r.dir, "release")
	raw, _ := os.ReadFile(filepath.Join(rel, "update.json"))

	// A router with a key refuses an update that is not signed.
	os.WriteFile(r.sys.PublicKey, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644)
	if _, err := r.sys.Fetch(context.Background(), r.srv.Client(), r.base()); err == nil {
		t.Fatal("an unsigned update was accepted by a router with a key")
	}
	sig, err := Sign(raw, seed)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rel, "update.json.sig"), []byte(sig+"\n"), 0o644)
	if _, err := r.sys.Fetch(context.Background(), r.srv.Client(), r.base()); err != nil {
		t.Fatalf("a correctly signed update was refused: %v", err)
	}
	// Signed by someone else.
	_, other, _ := ed25519.GenerateKey(nil)
	sig, _ = Sign(raw, base64.StdEncoding.EncodeToString(other.Seed()))
	os.WriteFile(filepath.Join(rel, "update.json.sig"), []byte(sig), 0o644)
	if _, err := r.sys.Fetch(context.Background(), r.srv.Client(), r.base()); err == nil {
		t.Error("an update signed with another key was accepted")
	}
	// Changed after signing.
	sig, _ = Sign(raw, seed)
	os.WriteFile(filepath.Join(rel, "update.json.sig"), []byte(sig), 0o644)
	os.WriteFile(filepath.Join(rel, "update.json"), bytes.Replace(raw, []byte("v1.2.0"), []byte("v9.9.9"), 1), 0o644)
	if _, err := r.sys.Fetch(context.Background(), r.srv.Client(), r.base()); err == nil {
		t.Error("a manifest changed after signing was accepted")
	}
}

func TestLayoutAndVersions(t *testing.T) {
	// A card flashed before there were two slots.
	r := newRig(t, 2)
	if r.sys.Supported() {
		t.Error("the old one-slot layout was taken for the two-slot one")
	}
	if _, err := r.sys.Current(); err == nil {
		t.Error("no error for a root partition that is not a slot")
	}
	// An update for another layout is refused.
	r = newRig(t, 5)
	other := *r.manifest
	other.Layout = Layout + 1
	if err := r.sys.Install(context.Background(), r.srv.Client(), r.base(), &other, "", func(string, int) {}); err == nil {
		t.Error("an update for another card layout was accepted")
	}
	bad := *r.manifest
	bad.Root.Name = "../../etc/passwd"
	if bad.check() == nil {
		t.Error("a file name with a path in it was accepted")
	}

	for _, c := range []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.10.0", "v0.9.5", false},
		{"v0.9.5", "v0.10.0", true},
		{"v1.0.0", "v0.9.9", false},
		{"73f9795", "v0.1.0", true},           // a build from a commit
		{"v0.1.0-3-g73f9795", "v0.1.0", true}, // a build after a release
		{"v0.1.0", "nightly", false},
		{"dev", "", false},
	} {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.current, c.latest, got)
		}
	}
}
