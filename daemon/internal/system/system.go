// Package system covers the router's dealings with the machine itself:
// the clock, power, the status LED and the power button.
package system

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// The Pi has no battery-backed clock by default. WireGuard needs time to
// never run backwards between sessions (its handshake carries a
// timestamp the peer insists is increasing), so the last known time is
// kept on disk and restored at boot when the clock is behind it.

// RestoreClock moves the clock forward to the saved time if it is behind.
func RestoreClock(path string, floor time.Time, log *slog.Logger) {
	saved := floor
	if b, err := os.ReadFile(path); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && time.Unix(v, 0).After(saved) {
			saved = time.Unix(v, 0)
		}
	}
	if time.Now().Before(saved) {
		if err := SetClock(saved); err != nil {
			log.Warn("restoring clock failed", "err", err)
		} else {
			log.Info("clock restored from last known time", "time", saved.UTC().Format(time.RFC3339))
		}
	}
}

func SaveClock(path string) {
	_ = config.WriteFileAtomic(path, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
}

func SetClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

// TimeFromPhone applies the time sent by the app. The phone's clock is
// network-synchronised and far better than an RTC-less board's guess, but
// a clock that NTP has already disciplined is left alone unless it is
// clearly wrong.
func TimeFromPhone(t time.Time) (adjusted bool, err error) {
	diff := time.Since(t)
	if diff < 0 {
		diff = -diff
	}
	limit := 2 * time.Second
	if ntpSynced() {
		limit = 5 * time.Minute
	}
	if diff < limit {
		return false, nil
	}
	return true, SetClock(t)
}

func ntpSynced() bool {
	_, err := os.Stat("/run/systemd/timesync/synchronized")
	return err == nil
}

// SetNTP starts or stops network time synchronisation.
func SetNTP(on bool) {
	verb := "stop"
	if on {
		verb = "start"
	}
	_ = exec.Command("systemctl", "--no-block", verb, "systemd-timesyncd.service").Run()
}

// SetSSH starts or stops the SSH server and installs the authorized
// keys. Host keys live on the data partition and are created on first
// use.
func SetSSH(dataDir string, on bool, keys []string) error {
	if !on {
		_ = exec.Command("systemctl", "--no-block", "stop", "ssh.service").Run()
		return nil
	}
	dir := filepath.Join(dataDir, "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key := filepath.Join(dir, "ssh_host_ed25519_key")
	if _, err := os.Stat(key); err != nil {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen: %s", strings.TrimSpace(string(out)))
		}
	}
	if err := config.WriteFileAtomic(filepath.Join(dir, "authorized_keys"), []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return exec.Command("systemctl", "--no-block", "restart", "ssh.service").Run()
}

func Reboot()   { _ = exec.Command("systemctl", "--no-block", "reboot").Run() }
func Poweroff() { _ = exec.Command("systemctl", "--no-block", "poweroff").Run() }

// CPUTemp returns the SoC temperature in degrees Celsius, 0 if unknown.
func CPUTemp() float64 {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return v / 1000
}

// --- status LED -----------------------------------------------------------

const ledDir = "/sys/class/leds/ACT"

// LED patterns.
const (
	LEDNormal  = "normal"  // steady on
	LEDPairing = "pairing" // fast blink
	LEDSetup   = "setup"   // slow blink: waiting for first-time setup
)

func SetLED(pattern string) {
	write := func(name, v string) { _ = os.WriteFile(filepath.Join(ledDir, name), []byte(v), 0) }
	switch pattern {
	case LEDPairing:
		write("trigger", "timer")
		write("delay_on", "120")
		write("delay_off", "120")
	case LEDSetup:
		write("trigger", "timer")
		write("delay_on", "1000")
		write("delay_off", "1000")
	default:
		write("trigger", "default-on")
	}
}

// --- power button ----------------------------------------------------------

// WatchPowerButton reports presses of the board's power button: short is
// true for a press under one second. Holding the button for several
// seconds is left to the PMIC, which cuts power by itself.
func WatchPowerButton(log *slog.Logger, onPress func(short bool)) {
	paths, _ := filepath.Glob("/dev/input/by-path/*pwr_button*event*")
	if len(paths) == 0 {
		paths = findPowerInput()
	}
	if len(paths) == 0 {
		log.Info("no power button found; pairing can only be opened from a paired phone")
		return
	}
	f, err := os.Open(paths[0])
	if err != nil {
		log.Warn("power button unavailable", "err", err)
		return
	}
	go func() {
		defer f.Close()
		// struct input_event on 64-bit: timeval (16 bytes), type, code, value.
		buf := make([]byte, 24)
		var down time.Time
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			typ := binary.LittleEndian.Uint16(buf[16:])
			code := binary.LittleEndian.Uint16(buf[18:])
			val := int32(binary.LittleEndian.Uint32(buf[20:]))
			if typ != 1 || code != 116 { // EV_KEY, KEY_POWER
				continue
			}
			switch val {
			case 1:
				down = time.Now()
			case 0:
				if !down.IsZero() {
					onPress(time.Since(down) < time.Second)
					down = time.Time{}
				}
			}
		}
	}()
}

func findPowerInput() []string {
	names, _ := filepath.Glob("/sys/class/input/event*/device/name")
	for _, n := range names {
		b, _ := os.ReadFile(n)
		if strings.Contains(string(b), "pwr_button") {
			return []string{"/dev/input/" + filepath.Base(filepath.Dir(filepath.Dir(n)))}
		}
	}
	return nil
}

// --- service manager notifications ------------------------------------------

// Notify sends a state string ("READY=1", "WATCHDOG=1") to systemd.
func Notify(state string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	addr := &unix.SockaddrUnix{Name: sock}
	if strings.HasPrefix(sock, "@") {
		addr.Name = "\x00" + sock[1:]
	}
	_ = unix.Sendto(fd, []byte(state), 0, addr)
}
