// porterd is the travel router daemon.
//
// The same binary doubles as the hook helper that dhcpcd and dnsmasq call
// on lease events: invoked under the names porter-dhcp-hook or
// porter-lease-hook it forwards the event to the running daemon.
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
)

// Set at build time.
var (
	version   = "dev"
	buildTime = "0"
)

const (
	dhcpHook  = "porter-dhcp-hook"
	leaseHook = "porter-lease-hook"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case dhcpHook:
		hook("dhcpcd")
		return
	case leaseHook:
		hook("lease")
		return
	}

	dataDir := flag.String("data", "/data/porter", "persistent state directory")
	runDir := flag.String("run", "/run/porter", "runtime directory")
	noBLE := flag.Bool("no-ble", false, "do not start the Bluetooth control service")
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
	for _, name := range []string{dhcpHook, leaseHook} {
		link := filepath.Join(*runDir, name)
		os.Remove(link)
		if err := os.Symlink(self, link); err != nil {
			log.Error("installing hook", "err", err)
			os.Exit(1)
		}
		hooks[name] = link
	}

	bt, _ := strconv.ParseInt(buildTime, 10, 64)
	c, err := core.New(core.Options{
		DataDir: *dataDir, RunDir: *runDir,
		DHCPHook: hooks[dhcpHook], LeaseHook: hooks[leaseHook],
		WGUserspace: *wgImpl, Version: version, BuildTime: time.Unix(bt, 0),
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
func hook(kind string) {
	msg := core.HookMessage{Kind: kind, Args: os.Args[1:], Env: map[string]string{}}
	if kind == "dhcpcd" {
		for _, kv := range os.Environ() {
			k, v, _ := strings.Cut(kv, "=")
			if k == "interface" || k == "reason" || strings.HasPrefix(k, "new_") || strings.HasPrefix(k, "nd") {
				msg.Env[k] = v
			}
		}
	}
	// os.Args[0] is the symlink's path as the caller invoked it.
	sock := core.HookSocket(filepath.Dir(os.Args[0]))
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	_ = json.NewEncoder(conn).Encode(msg)
}
