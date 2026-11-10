// Package update installs a new version of the router's software.
//
// The SD card holds the system twice, as slot A and slot B: each a boot
// partition (kernel, device trees, kernel command line) and a root file
// system. One slot is running; an update is written to the other. The
// Raspberry Pi's boot loader is then asked to start the other slot once
// ("tryboot"). If the new system comes up and the router daemon runs, the
// daemon makes that slot the default. If it does not, the next restart,
// whatever causes it, goes back to the slot that worked.
//
// Settings live on a separate data partition both slots share, so an
// update keeps them, pairings included.
package update

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Layout is the version of the partition scheme this package installs
// into. An update built for another layout is refused.
const Layout = 2

// Part is one partition image of an update.
type Part struct {
	// Name is the file to download, gzip-compressed, next to the manifest.
	Name string `json:"name"`
	// Size and SHA256 describe the image once decompressed.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Bytes is the size of the download.
	Bytes int64 `json:"bytes"`
}

// Manifest describes one released version.
type Manifest struct {
	Version string `json:"version"`
	Built   int64  `json:"built"`
	Layout  int    `json:"layout"`
	Boot    Part   `json:"boot"`
	Root    Part   `json:"root"`
}

// Download is how much has to be fetched to install it.
func (m *Manifest) Download() int64 { return m.Boot.Bytes + m.Root.Bytes }

func (m *Manifest) check() error {
	if m.Version == "" {
		return errors.New("the update does not say which version it is")
	}
	if m.Layout != Layout {
		return fmt.Errorf("version %s needs a different card layout; flash its image instead", m.Version)
	}
	for _, p := range []Part{m.Boot, m.Root} {
		if p.Name == "" || p.Name != filepath.Base(p.Name) || strings.HasPrefix(p.Name, ".") {
			return errors.New("the update names a file it should not")
		}
		if p.Size <= 0 || len(p.SHA256) != 64 {
			return errors.New("the update's description is incomplete")
		}
	}
	return nil
}

// Slot is one of the two copies of the system.
type Slot struct {
	Name string // "A" or "B"
	Boot int    // partition number of its boot partition
	Root int    // partition number of its root file system
}

var (
	SlotA = Slot{Name: "A", Boot: 2, Root: 5}
	SlotB = Slot{Name: "B", Boot: 3, Root: 6}
)

func (s Slot) other() Slot {
	if s.Name == "A" {
		return SlotB
	}
	return SlotA
}

// System is the machine as the updater sees it. The fields are paths and
// actions so that tests can stand in files for partitions.
type System struct {
	// PartPrefix plus a two-digit partition number names a partition:
	// "/dev/disk/by-partuuid/504f5254-" on the router.
	PartPrefix string
	// DiskID is the eight hex digits of the disk identifier, as used in
	// root=PARTUUID=<DiskID>-NN on the kernel command line.
	DiskID string
	// Cmdline is the running kernel's command line: /proc/cmdline.
	Cmdline string
	// TryFlag is where the boot loader says this start was a trial:
	// /proc/device-tree/chosen/bootloader/tryboot.
	TryFlag string
	// Selector is the mounted first partition, which holds autoboot.txt.
	Selector string
	// Pending records an update that has been written and not yet
	// confirmed. It is on the data partition, which both slots see.
	Pending string
	// DataDir is the daemon's directory on the data partition, and
	// Backup a directory for a copy of its small files, taken before a
	// trial. Both versions use the same data partition, so a new version
	// may rewrite settings in a form the old one cannot read; if the
	// trial does not hold, the old version gets back what it had.
	DataDir string
	Backup  string
	// PublicKey is a file with the base64 Ed25519 key updates must be
	// signed with. If the file does not exist, updates are trusted on
	// the strength of the HTTPS connection they came over.
	PublicKey string

	// WritableSelector makes the selector partition writable for the
	// duration of fn.
	WritableSelector func(fn func() error) error
	// WithBoot mounts a boot partition for the duration of fn.
	WithBoot func(dev string, fn func(dir string) error) error
	// Restart restarts the machine, into the trial slot if try is set.
	Restart func(try bool) error
}

func (s *System) part(n int) string { return fmt.Sprintf("%s%02d", s.PartPrefix, n) }

var rootArg = regexp.MustCompile(`root=PARTUUID=([0-9a-fA-F]{8})-([0-9a-fA-F]{2})`)

// Current returns the slot the running system was started from.
func (s *System) Current() (Slot, error) {
	b, err := os.ReadFile(s.Cmdline)
	if err != nil {
		return Slot{}, err
	}
	m := rootArg.FindStringSubmatch(string(b))
	if m == nil {
		return Slot{}, errors.New("the kernel command line does not name a root partition")
	}
	n, _ := strconv.ParseInt(m[2], 16, 32)
	switch int(n) {
	case SlotA.Root:
		return SlotA, nil
	case SlotB.Root:
		return SlotB, nil
	}
	return Slot{}, errors.New("this card does not have the two-slot layout")
}

// Supported reports whether this machine can be updated in place: it was
// started from one of two slots and the other one exists.
func (s *System) Supported() bool {
	cur, err := s.Current()
	if err != nil {
		return false
	}
	for _, n := range []int{1, cur.other().Boot, cur.other().Root} {
		if _, err := os.Stat(s.part(n)); err != nil {
			return false
		}
	}
	return true
}

// Trial reports whether the boot loader started this system as a trial.
func (s *System) Trial() bool {
	b, err := os.ReadFile(s.TryFlag)
	if err != nil || len(b) < 4 {
		return false
	}
	return binary.BigEndian.Uint32(b) != 0
}

// selectorText is autoboot.txt: which boot partition to start normally,
// and which when asked for a trial.
func selectorText(normal, trial Slot) string {
	return fmt.Sprintf("[all]\ntryboot_a_b=1\nboot_partition=%d\n[tryboot]\nboot_partition=%d\n", normal.Boot, trial.Boot)
}

func (s *System) writeSelector(normal, trial Slot) error {
	return s.WritableSelector(func() error {
		path := filepath.Join(s.Selector, "autoboot.txt")
		tmp := filepath.Join(s.Selector, "autoboot.new")
		if err := writeSynced(tmp, []byte(selectorText(normal, trial))); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	})
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --- finding an update -----------------------------------------------------------

func get(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", filepath.Base(url), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than it should be", filepath.Base(url))
	}
	return b, nil
}

// Verify checks a manifest's signature against a base64 Ed25519 public
// key.
func Verify(manifest, signature []byte, publicKey string) error {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("this router's update key is not a valid key")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || !ed25519.Verify(ed25519.PublicKey(key), manifest, sig) {
		return errors.New("the update is not signed by the key this router trusts")
	}
	return nil
}

// Sign returns the base64 signature of a manifest for a base64 Ed25519
// private key (the 32-byte seed).
func Sign(manifest []byte, privateKey string) (string, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil || len(seed) != ed25519.SeedSize {
		return "", errors.New("not a valid signing key")
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), manifest)), nil
}

// Fetch downloads and checks the manifest of the newest release. base is
// the address its files are under, ending in a slash.
func (s *System) Fetch(ctx context.Context, client *http.Client, base string) (*Manifest, error) {
	raw, err := get(ctx, client, base+"update.json", 64<<10)
	if err != nil {
		return nil, err
	}
	if key, err := os.ReadFile(s.PublicKey); err == nil {
		sig, err := get(ctx, client, base+"update.json.sig", 1<<10)
		if err != nil {
			return nil, fmt.Errorf("the update's signature could not be fetched: %w", err)
		}
		if err := Verify(raw, sig, string(key)); err != nil {
			return nil, err
		}
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("the update's description could not be read")
	}
	if err := m.check(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Newer reports whether version latest should be offered to a router
// running current. Versions are tags like v1.2.3; a build that is not a
// release (a commit id) is offered whatever release exists.
func Newer(current, latest string) bool {
	c, okC := parseVersion(current)
	l, okL := parseVersion(latest)
	if !okL {
		return false
	}
	if !okC {
		return true
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	f := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(f) != 3 || !strings.HasPrefix(v, "v") {
		return out, false
	}
	for i, s := range f {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// --- installing -------------------------------------------------------------------

// retryWait is the pause before a failed download is tried again.
var retryWait = 3 * time.Second

// Progress is told how far an installation is: the phase, and a
// percentage of the whole.
type Progress func(phase string, percent int)

// counter reports bytes passing through it.
type counter struct {
	n    int64
	tick func(n int64)
}

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	c.tick(c.n)
	return len(p), nil
}

// writePart downloads one partition image and writes it, decompressed, to
// dev, checking size and hash on the way.
func writePart(ctx context.Context, client *http.Client, url string, p Part, dev string, tick func(written int64)) error {
	out, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer out.Close()
	if end, err := out.Seek(0, io.SeekEnd); err == nil && end > 0 && end < p.Size {
		return fmt.Errorf("%s does not fit its partition", p.Name)
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", p.Name, resp.Status)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}
	sum := sha256.New()
	count := &counter{tick: tick}
	// One byte more than expected is allowed through, so that an image
	// that is too long is noticed rather than cut off.
	n, err := io.CopyBuffer(io.MultiWriter(out, sum, count), io.LimitReader(zr, p.Size+1), make([]byte, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}
	if n != p.Size || hex.EncodeToString(sum.Sum(nil)) != p.SHA256 {
		return fmt.Errorf("%s arrived damaged", p.Name)
	}
	return out.Sync()
}

// readBack checks that what is now on dev is the image described by p.
func readBack(dev string, p Part, tick func(read int64)) error {
	in, err := os.Open(dev)
	if err != nil {
		return err
	}
	defer in.Close()
	dropCache(in)
	sum := sha256.New()
	count := &counter{tick: tick}
	if _, err := io.CopyBuffer(io.MultiWriter(sum, count), io.LimitReader(in, p.Size), make([]byte, 1<<20)); err != nil {
		return err
	}
	if hex.EncodeToString(sum.Sum(nil)) != p.SHA256 {
		return fmt.Errorf("%s was not written correctly; the card may be failing", p.Name)
	}
	return nil
}

// pending is the note left for the next start.
type pending struct {
	Version string `json:"version"`
	Slot    string `json:"slot"`
	From    string `json:"from"`
	At      int64  `json:"at"`
}

// Install writes the update m, found under base, to the slot that is not
// running, and restarts into it as a trial. It returns once the restart
// has been asked for.
func (s *System) Install(ctx context.Context, client *http.Client, base string, m *Manifest, from string, progress Progress) error {
	if err := m.check(); err != nil {
		return err
	}
	cur, err := s.Current()
	if err != nil {
		return err
	}
	if !s.Supported() {
		return errors.New("this card does not have the two-slot layout")
	}
	target := cur.other()
	total := m.Root.Size + m.Boot.Size
	// Writing is 80 % of the bar, reading back the rest.
	step := func(phase string, done int64, lo, hi int) {
		progress(phase, lo+int(done*int64(hi-lo)/total))
	}
	// The slot about to be overwritten must not be started from now on.
	if err := s.writeSelector(cur, cur); err != nil {
		return fmt.Errorf("preparing: %w", err)
	}
	_ = os.Remove(s.Pending)

	attempt := func(p Part, dev string, before int64) error {
		var err error
		for try := 0; try < 3; try++ {
			if err = writePart(ctx, client, base+p.Name, p, dev, func(n int64) { step("download", before+n, 0, 80) }); err == nil || ctx.Err() != nil {
				return err
			}
			time.Sleep(retryWait)
		}
		return err
	}
	if err := attempt(m.Root, s.part(target.Root), 0); err != nil {
		return err
	}
	if err := attempt(m.Boot, s.part(target.Boot), m.Root.Size); err != nil {
		return err
	}
	if err := readBack(s.part(target.Root), m.Root, func(n int64) { step("verify", n, 80, 98) }); err != nil {
		return err
	}
	if err := readBack(s.part(target.Boot), m.Boot, func(n int64) { step("verify", m.Root.Size+n, 80, 98) }); err != nil {
		return err
	}
	// The boot image is the same for both slots except for the root
	// partition its kernel command line names.
	err = s.WithBoot(s.part(target.Boot), func(dir string) error {
		path := filepath.Join(dir, "cmdline.txt")
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		want := fmt.Sprintf("root=PARTUUID=%s-%02x", s.DiskID, target.Root)
		if !rootArg.Match(b) {
			return errors.New("the update's kernel command line names no root partition")
		}
		return writeSynced(path, rootArg.ReplaceAll(b, []byte(want)))
	})
	if err != nil {
		return fmt.Errorf("finishing the boot partition: %w", err)
	}
	if err := s.snapshot(); err != nil {
		return fmt.Errorf("saving the settings: %w", err)
	}
	note, _ := json.Marshal(pending{Version: m.Version, Slot: target.Name, From: from, At: time.Now().Unix()})
	if err := writeSynced(s.Pending, note); err != nil {
		return err
	}
	if err := s.writeSelector(cur, target); err != nil {
		return err
	}
	progress("restart", 100)
	return s.Restart(true)
}

// kept reports whether a file in the data directory belongs in the copy
// taken before a trial: settings and other small state, not caches, not
// the clock, not the updater's own notes.
func (s *System) kept(e os.DirEntry) bool {
	name := e.Name()
	if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".bad") {
		return false
	}
	if name == filepath.Base(s.Pending) || name == "clock" {
		return false
	}
	info, err := e.Info()
	return err == nil && info.Size() <= 1<<20
}

func copySynced(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	tmp := to + ".tmp"
	if err := writeSynced(tmp, b); err != nil {
		return err
	}
	if info, err := os.Stat(from); err == nil {
		_ = os.Chmod(tmp, info.Mode().Perm())
	}
	return os.Rename(tmp, to)
}

// snapshot copies the settings aside before a trial.
func (s *System) snapshot() error {
	if s.DataDir == "" {
		return nil
	}
	if err := os.RemoveAll(s.Backup); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Backup, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.DataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !s.kept(e) {
			continue
		}
		if err := copySynced(filepath.Join(s.DataDir, e.Name()), filepath.Join(s.Backup, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Recover is called when the daemon starts, before it reads anything from
// the data directory. If an update was tried and the system that is
// starting is the old one, the settings go back to what they were before
// the trial: the new version ran for a while and may have rewritten them
// in a way this version does not understand. It reports whether it
// restored anything. Settle, later, does the reporting and tidies up.
func (s *System) Recover() (bool, error) {
	b, err := os.ReadFile(s.Pending)
	if err != nil || s.DataDir == "" {
		return false, nil
	}
	var note pending
	if json.Unmarshal(b, &note) != nil {
		return false, nil
	}
	cur, err := s.Current()
	if err != nil || cur.Name == note.Slot {
		return false, nil
	}
	entries, err := os.ReadDir(s.Backup)
	if err != nil {
		return false, nil
	}
	restored := false
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if err := copySynced(filepath.Join(s.Backup, e.Name()), filepath.Join(s.DataDir, e.Name())); err != nil {
			return restored, err
		}
		restored = true
	}
	// Done once: a second start must not undo changes made since.
	if err := os.RemoveAll(s.Backup); err != nil {
		return restored, err
	}
	return restored, nil
}

// Outcome is what Settle found.
type Outcome struct {
	// Updated is set when a trial of a new version is now the default;
	// Failed when a trial did not hold and the previous version is
	// running. Version is the version that was being installed.
	Updated bool
	Failed  bool
	Version string
}

// Settle is called once the router is up and working. If this start is
// the trial of an update, the update is made permanent. If an update was
// tried and this is the old system again, that is reported.
func (s *System) Settle() (Outcome, error) {
	b, err := os.ReadFile(s.Pending)
	if err != nil {
		return Outcome{}, nil
	}
	var note pending
	if json.Unmarshal(b, &note) != nil {
		_ = os.Remove(s.Pending)
		return Outcome{}, nil
	}
	cur, err := s.Current()
	if err != nil {
		return Outcome{}, err
	}
	if cur.Name != note.Slot {
		// Back on the old slot: the trial never got as far as this.
		_ = s.writeSelector(cur, cur)
		_ = os.Remove(s.Pending)
		_ = os.RemoveAll(s.Backup)
		return Outcome{Failed: true, Version: note.Version}, nil
	}
	// The old slot stays as it is, a working system to fall back on by
	// hand; a trial of it is not on offer until the next update.
	if err := s.writeSelector(cur, cur); err != nil {
		return Outcome{}, fmt.Errorf("making the update permanent: %w", err)
	}
	if err := os.Remove(s.Pending); err != nil {
		return Outcome{}, err
	}
	_ = os.RemoveAll(s.Backup)
	return Outcome{Updated: true, Version: note.Version}, nil
}

// Unsettled reports whether an update is waiting to be confirmed.
func (s *System) Unsettled() bool {
	_, err := os.Stat(s.Pending)
	return err == nil
}

// Describe builds a manifest for partition images on disk; the image
// build uses it. Each image is compressed next to itself as name.gz.
func Describe(version string, built int64, bootImage, rootImage string) (*Manifest, error) {
	part := func(path string) (Part, error) {
		in, err := os.Open(path)
		if err != nil {
			return Part{}, err
		}
		defer in.Close()
		name := filepath.Base(path) + ".gz"
		out, err := os.Create(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			return Part{}, err
		}
		defer out.Close()
		zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
		sum := sha256.New()
		n, err := io.CopyBuffer(io.MultiWriter(zw, sum), in, make([]byte, 1<<20))
		if err != nil {
			return Part{}, err
		}
		if err := zw.Close(); err != nil {
			return Part{}, err
		}
		st, err := out.Stat()
		if err != nil {
			return Part{}, err
		}
		return Part{Name: name, Size: n, SHA256: hex.EncodeToString(sum.Sum(nil)), Bytes: st.Size()}, nil
	}
	boot, err := part(bootImage)
	if err != nil {
		return nil, err
	}
	root, err := part(rootImage)
	if err != nil {
		return nil, err
	}
	return &Manifest{Version: version, Built: built, Layout: Layout, Boot: boot, Root: root}, nil
}

// Encode renders a manifest the way it is published and signed.
func (m *Manifest) Encode() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	_ = enc.Encode(m)
	return b.Bytes()
}
