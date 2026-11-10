// Package proc supervises the helper daemons the router drives (hostapd,
// wpa_supplicant, dnsmasq, dhcpcd).
package proc

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Proc is a supervised process. It is restarted with backoff whenever it
// exits until Stop is called.
type Proc struct {
	Name string
	Argv []string
	// Env is added to the environment the process inherits ("K=V").
	Env []string
	// OnLine receives every output line (stdout and stderr).
	OnLine func(line string)
	// OnExit is called after each exit, before any restart.
	OnExit func(err error)
	// NoRestart leaves the process down after it exits.
	NoRestart bool
	Log       *slog.Logger

	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool
	done    chan struct{}
}

// Start launches the process and its supervision loop.
func (p *Proc) Start() {
	p.mu.Lock()
	if p.done != nil {
		p.mu.Unlock()
		return
	}
	p.done = make(chan struct{})
	p.stopped = false
	p.mu.Unlock()
	go p.loop()
}

func (p *Proc) loop() {
	defer close(p.done)
	backoff := time.Second
	for {
		started := time.Now()
		err := p.runOnce()
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if p.OnExit != nil {
			p.OnExit(err)
		}
		if stopped || p.NoRestart {
			return
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		p.Log.Warn("process exited, restarting", "proc", p.Name, "err", err, "in", backoff)
		t := time.NewTimer(backoff)
		for waiting := true; waiting; {
			select {
			case <-t.C:
				waiting = false
			case <-time.After(100 * time.Millisecond):
				p.mu.Lock()
				s := p.stopped
				p.mu.Unlock()
				if s {
					t.Stop()
					return
				}
			}
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (p *Proc) runOnce() error {
	cmd := exec.Command(p.Argv[0], p.Argv[1:]...)
	if len(p.Env) > 0 {
		cmd.Env = append(os.Environ(), p.Env...)
	}
	// Own process group, and die with the router daemon.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		pw.Close()
		return err
	}
	p.cmd = cmd
	p.mu.Unlock()

	lines := make(chan struct{})
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			p.Log.Debug(line, "proc", p.Name)
			if p.OnLine != nil {
				p.OnLine(line)
			}
		}
	}()
	err := cmd.Wait()
	pw.Close()
	<-lines
	p.mu.Lock()
	p.cmd = nil
	p.mu.Unlock()
	return err
}

// Signal sends a signal to the running process, if any.
func (p *Proc) Signal(sig syscall.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Signal(sig)
	}
}

// Stop terminates the process and waits for it to be gone.
func (p *Proc) Stop() {
	p.mu.Lock()
	done := p.done
	if done == nil {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Signal(syscall.SIGTERM)
	}
	p.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.mu.Lock()
		if p.cmd != nil && p.cmd.Process != nil {
			syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		p.mu.Unlock()
		<-done
	}
	p.mu.Lock()
	p.done = nil
	p.mu.Unlock()
}

// Running reports whether the process is currently alive.
func (p *Proc) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil
}
