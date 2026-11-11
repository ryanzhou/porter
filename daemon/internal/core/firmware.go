package core

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/proto"
	"github.com/ryanzhou/porter/daemon/internal/update"
)

// FirmwareInfo is what the router knows about its own software and the
// newest release.
type FirmwareInfo struct {
	Current string `json:"current"`
	// Supported is false on a card without the two-slot layout, which
	// can only be updated by flashing.
	Supported bool   `json:"supported"`
	Latest    string `json:"latest,omitempty"`
	Newer     bool   `json:"newer"`
	// Download is the size of the update in bytes.
	Download int64 `json:"download,omitempty"`
	Built    int64 `json:"built,omitempty"`
}

// FirmwareProgress is the state of an installation: "idle", "installing"
// (with phase "download", "verify" or "restart"), or "failed".
type FirmwareProgress struct {
	State   string `json:"state"`
	Phase   string `json:"phase,omitempty"`
	Percent int    `json:"percent"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type firmware struct {
	mu       sync.Mutex
	progress FirmwareProgress
}

// updateClient downloads releases. No overall timeout: an image over a
// hotel's Wi-Fi takes as long as it takes. A connection that goes silent
// is another matter.
var updateClient = &http.Client{Transport: &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}}

func (c *Core) updatable() bool {
	return c.opt.Update != nil && c.opt.UpdateURL != "" && c.opt.Update.Supported()
}

func (c *Core) firmwareCheck() (*FirmwareInfo, *update.Manifest, *proto.Error) {
	info := &FirmwareInfo{Current: c.opt.Version, Supported: c.updatable()}
	if c.opt.Update == nil || c.opt.UpdateURL == "" {
		return info, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	m, err := c.opt.Update.Fetch(ctx, updateClient, c.opt.UpdateURL)
	if err != nil {
		return nil, nil, proto.Errf(proto.ErrInternal, "Could not check for an update: "+err.Error())
	}
	info.Latest, info.Download, info.Built = m.Version, m.Download(), m.Built
	info.Newer = update.Newer(c.opt.Version, m.Version)
	return info, m, nil
}

func (c *Core) firmwareProgress() FirmwareProgress {
	c.firmware.mu.Lock()
	defer c.firmware.mu.Unlock()
	p := c.firmware.progress
	if p.State == "" {
		p.State = "idle"
	}
	return p
}

func (c *Core) setFirmwareProgress(p FirmwareProgress) {
	c.firmware.mu.Lock()
	changed := c.firmware.progress != p
	c.firmware.progress = p
	c.firmware.mu.Unlock()
	if changed {
		c.broadcast("firmware", p)
	}
}

// firmwareInstall starts installing the newest release and returns at
// once; progress goes out as "firmware" events.
func (c *Core) firmwareInstall() *proto.Error {
	if !c.updatable() {
		return proto.Errf(proto.ErrInvalid, "This card was flashed before updates existed. Flash the newest image once; after that the router updates itself.")
	}
	info, m, perr := c.firmwareCheck()
	if perr != nil {
		return perr
	}
	if !info.Newer {
		// Asked twice, or by an app that looked a while ago.
		return proto.Errf(proto.ErrInvalid, "The router already runs the newest version.")
	}
	c.firmware.mu.Lock()
	if c.firmware.progress.State == "installing" {
		c.firmware.mu.Unlock()
		return proto.Errf("busy", "An update is already being installed.")
	}
	c.firmware.progress = FirmwareProgress{State: "installing", Phase: "download", Version: m.Version}
	c.firmware.mu.Unlock()
	c.broadcast("firmware", c.firmwareProgress())
	c.log.Info("installing update", "version", m.Version, "from", c.opt.Version)
	go func() {
		// What survives a restart is written before the new system is
		// started.
		c.do(func() {
			c.devs.Save()
			c.stats.Save()
		})
		err := c.opt.Update.Install(context.Background(), updateClient, c.opt.UpdateURL, m, c.opt.Version, func(phase string, percent int) {
			c.setFirmwareProgress(FirmwareProgress{State: "installing", Phase: phase, Percent: percent, Version: m.Version})
		})
		if err != nil {
			c.log.Error("update failed", "version", m.Version, "err", err)
			c.addEvent("update", "Update to "+m.Version+" failed: "+err.Error(), "")
			c.setFirmwareProgress(FirmwareProgress{State: "failed", Version: m.Version, Error: err.Error()})
		}
	}()
	return nil
}

// settleUpdate runs once the router has been up for a little while. A
// new version that got this far is kept; one that did not is reported.
func (c *Core) settleUpdate(ctx context.Context) {
	if c.opt.Update == nil || !c.opt.Update.Unsettled() {
		return
	}
	if c.cfgUnreadable {
		// A version that cannot read the settings it was handed is not
		// one to keep. Left unconfirmed, the router goes back to the
		// version before, and to its settings.
		c.log.Error("this version could not read the settings; the update is not confirmed")
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(c.opt.UpdateSettle):
	}
	out, err := c.opt.Update.Settle()
	switch {
	case err != nil:
		c.log.Error("confirming the update", "err", err)
	case out.Updated:
		c.log.Info("update confirmed", "version", out.Version)
		c.addEvent("update", "Updated to "+out.Version, "")
	case out.Failed:
		c.log.Warn("the update did not start; still on the previous version", "tried", out.Version, "running", c.opt.Version)
		c.addEvent("update", "The update to "+out.Version+" did not start. Still on "+c.opt.Version+".", "")
	}
}
