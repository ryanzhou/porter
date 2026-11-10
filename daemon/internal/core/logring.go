package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LogRing keeps the most recent log lines in memory so the app can show
// them; nothing is written to storage.
type LogRing struct {
	mu    sync.Mutex
	lines []string
	next  int
	max   int
}

func NewLogRing(max int) *LogRing { return &LogRing{max: max} }

func (r *LogRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) < r.max {
		r.lines = append(r.lines, line)
		return
	}
	r.lines[r.next] = line
	r.next = (r.next + 1) % r.max
}

// Lines returns the buffered lines, oldest first.
func (r *LogRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.lines))
	for i := 0; i < len(r.lines); i++ {
		out = append(out, r.lines[(r.next+i)%len(r.lines)])
	}
	return out
}

// Handler tees records at Info and above into the ring.
func (r *LogRing) Handler(next slog.Handler) slog.Handler { return &ringHandler{r: r, next: next} }

type ringHandler struct {
	r     *LogRing
	next  slog.Handler
	attrs []slog.Attr
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.next.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelInfo {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s %s", rec.Time.UTC().Format(time.RFC3339), rec.Level, rec.Message)
		add := func(a slog.Attr) bool {
			fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
			return true
		}
		for _, a := range h.attrs {
			add(a)
		}
		rec.Attrs(add)
		h.r.add(b.String())
	}
	if h.next.Enabled(ctx, rec.Level) {
		return h.next.Handle(ctx, rec)
	}
	return nil
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &ringHandler{r: h.r, next: h.next.WithAttrs(as), attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{r: h.r, next: h.next.WithGroup(name), attrs: h.attrs}
}
