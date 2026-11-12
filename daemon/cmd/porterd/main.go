// porterd is the travel router daemon.
//
// The same binary doubles as the hook helper that dhcpcd and dnsmasq call
// on lease events, and pppd when a session's addresses come and go:
// invoked under the names porter-dhcp-hook, porter-lease-hook or
// porter-ppp-<event> it forwards the event to the running daemon.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/ryanzhou/porter/daemon/internal/core"
	"github.com/ryanzhou/porter/daemon/internal/geoip"
	"github.com/ryanzhou/porter/daemon/internal/update"
)

// Set at build time.
var (
	version   = "dev"
	buildTime = "0"
	// releases is where the newest release's files are downloaded from.
	// The image build points it at the repository the image is built
	// from, so a fork updates from its own releases.
	releases = "https://github.com/ryanzhou/porter/releases/latest/download/"
)

// mpquicPrograms lists where the mpquic program is looked for. A copy on
// the data partition comes first: that is how a newer mpquic is tried
// without a new image.
func mpquicPrograms(dataDir string) []string {
	list := []string{filepath.Join(dataDir, "bin", "mpquic"), "/usr/lib/porter/mpquic"}
	if p := os.Getenv("PORTER_MPQUIC"); p != "" {
		list = append([]string{p}, list...)
	}
	return list
}

func releaseURL() string {
	if u := os.Getenv("PORTER_UPDATE_URL"); u != "" {
		return u
	}
	return releases
}

const (
	dhcpHook  = "porter-dhcp-hook"
	leaseHook = "porter-lease-hook"
	// pppd is given one script per event and tells a script nothing
	// about which event it is run for, so the name has to.
	pppHook = "porter-ppp-"
)

// pppEvents are pppd's events that the daemon wants to hear about.
var pppEvents = []string{"ip-up", "ip-down", "ipv6-up", "ipv6-down"}

func main() {
	switch name := filepath.Base(os.Args[0]); {
	case name == dhcpHook:
		hook("dhcpcd", nil)
		return
	case name == leaseHook:
		hook("lease", os.Args[1:])
		return
	case strings.HasPrefix(name, pppHook):
		hook("ppp", append([]string{strings.TrimPrefix(name, pppHook)}, os.Args[1:]...))
		return
	}

	dataDir := flag.String("data", "/data/porter", "persistent state directory")
	runDir := flag.String("run", "/run/porter", "runtime directory")
	noBLE := flag.Bool("no-ble", false, "do not start the Bluetooth control service")
	geoPath := flag.String("geoip", geoip.DefaultPath, "country database (DB-IP country lite CSV, optionally gzipped)")
	wgImpl := flag.String("wg-userspace", "", "wireguard-go binary to use instead of the kernel module")
	debug := flag.Bool("debug", false, "verbose logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	ring := core.NewLogRing(500)
	log := slog.New(ring.Handler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := os.MkdirAll(*runDir, 0o755); err != nil {
		log.Error("runtime directory", "err", err)
		os.Exit(1)
	}
	// The hooks are symlinks to this binary placed next to the socket
	// they report to, so they need no configuration of their own.
	self, err := os.Executable()
	if err != nil {
		log.Error("locating executable", "err", err)
		os.Exit(1)
	}
	hooks := map[string]string{}
	pppHooks := map[string]string{}
	names := []string{dhcpHook, leaseHook}
	for _, event := range pppEvents {
		names = append(names, pppHook+event)
		pppHooks[event] = filepath.Join(*runDir, pppHook+event)
	}
	for _, name := range names {
		link := filepath.Join(*runDir, name)
		os.Remove(link)
		if err := os.Symlink(self, link); err != nil {
			log.Error("installing hook", "err", err)
			os.Exit(1)
		}
		hooks[name] = link
	}

	bt, _ := strconv.ParseInt(buildTime, 10, 64)
	// Before anything reads the settings: if an update was tried and did
	// not hold, this is the old version starting again, and it gets back
	// the settings it had before the new one touched them.
	updater := update.Router(*dataDir, *runDir)
	if restored, err := updater.Recover(); err != nil {
		log.Error("restoring the settings from before the update", "err", err)
	} else if restored {
		log.Warn("an update did not hold; the settings are as they were before it")
	}
	c, err := core.New(core.Options{
		DataDir: *dataDir, RunDir: *runDir,
		DHCPHook: hooks[dhcpHook], LeaseHook: hooks[leaseHook], PPPHooks: pppHooks,
		WGUserspace: *wgImpl, GeoIPPath: *geoPath, Version: version, BuildTime: time.Unix(bt, 0),
		Update: updater, UpdateURL: releaseURL(), UpdateSettle: 45 * time.Second,
		MPQUIC: mpquicPrograms(*dataDir), PhoneRecords: phoneRecords(*dataDir),
		Log: log, Logs: ring,
	})
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if !*noBLE {
		go c.RunBLE(ctx, dbus.ConnectSystemBus)
	}
	log.Info("porterd starting", "version", version)
	if err := c.Run(ctx); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// hook forwards one dhcpcd or dnsmasq event to the daemon. It must never
// fail loudly or block: the calling daemon waits for it.
// phoneRecords is where usbmuxd keeps its records of trusted iPhones on
// the router itself. Anywhere else (a development machine, a test) there
// is no such directory that would be the daemon's to empty.
func phoneRecords(dataDir string) string {
	if filepath.Clean(dataDir) == "/data/porter" {
		return "/data/lockdown"
	}
	return ""
}

func hook(kind string, args []string) {
	msg := core.HookMessage{Kind: kind, Args: args, Env: map[string]string{}}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch kind {
		case "dhcpcd":
			if k == "interface" || k == "reason" || strings.HasPrefix(k, "new_") || strings.HasPrefix(k, "nd") {
				msg.Env[k] = v
			}
		case "ppp":
			// The name servers the provider gave with the session.
			if k == "DNS1" || k == "DNS2" {
				msg.Env[k] = v
			}
		}
	}
	// The daemon says where it listens through the environment of the
	// program that calls the hook. The hook's own location is the
	// fallback, and only works for a caller that starts it by its full
	// path: dhcpcd and pppd do, dnsmasq uses the bare name.
	sock := os.Getenv(core.HookSocketEnv)
	if sock == "" {
		sock = core.HookSocket(filepath.Dir(os.Args[0]))
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	_ = json.NewEncoder(conn).Encode(msg)
}
