package update

import (
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// dropCache makes later reads of f come from the card, not from memory.
func dropCache(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}

func mounted(dir string) bool {
	var here, above unix.Stat_t
	if unix.Stat(dir, &here) != nil || unix.Stat(filepath.Dir(dir), &above) != nil {
		return false
	}
	return here.Dev != above.Dev
}

// Router is the System of the router image. dataDir is the daemon's
// directory on the data partition, runDir its directory in RAM.
func Router(dataDir, runDir string) *System {
	s := &System{
		PartPrefix: "/dev/disk/by-partuuid/504f5254-",
		DiskID:     "504f5254",
		Cmdline:    "/proc/cmdline",
		TryFlag:    "/proc/device-tree/chosen/bootloader/tryboot",
		Selector:   "/boot/firmware",
		Pending:    filepath.Join(dataDir, "update-pending"),
		PublicKey:  "/usr/lib/porter/update.pub",
	}
	s.WritableSelector = func(fn func() error) error {
		// The partition is normally mounted read-only. If it is not
		// mounted at all, mount it.
		if mounted(s.Selector) {
			if err := unix.Mount("", s.Selector, "", unix.MS_REMOUNT, ""); err != nil {
				return err
			}
			defer unix.Mount("", s.Selector, "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
		} else {
			if err := unix.Mount(s.part(1), s.Selector, "vfat", 0, ""); err != nil {
				return err
			}
			defer unix.Unmount(s.Selector, 0)
		}
		err := fn()
		unix.Sync()
		return err
	}
	s.WithBoot = func(dev string, fn func(dir string) error) error {
		dir := filepath.Join(runDir, "update-boot")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := unix.Mount(dev, dir, "vfat", 0, ""); err != nil {
			return err
		}
		err := fn(dir)
		unix.Sync()
		if uerr := unix.Unmount(dir, 0); err == nil {
			err = uerr
		}
		return err
	}
	s.Restart = func(try bool) error {
		unix.Sync()
		if try {
			// The argument reaches the boot loader, which starts the
			// [tryboot] partition of autoboot.txt this once.
			return exec.Command("systemctl", "reboot", "--reboot-argument=0 tryboot").Run()
		}
		return exec.Command("systemctl", "reboot").Run()
	}
	return s
}
