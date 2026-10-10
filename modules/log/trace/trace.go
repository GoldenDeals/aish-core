// Package trace is the log.trace module: it appends the trace of every
// committed transaction to a file right after the commit, so a hung or killed
// session still leaves its trace.
package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/modules/log/internal/pathfmt"
	"github.com/GoldenDeals/aish-core/trace"
)

const (
	dirMode  = 0o755
	fileMode = 0o600
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "log.trace", New: func() aish.Module { return &Trace{} }})
}

type Trace struct {
	// Path is the trace file, with pathfmt placeholders; ~/.aish/traces/%s.trace when empty.
	Path string

	file *os.File
}

// UnmarshalCaddyfile reads "log trace [path]".
func (t *Trace) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	switch args := d.RemainingArgs(); len(args) {
	case 0:
	case 1:
		t.Path = args[0]
	default:
		return d.ArgErr()
	}
	if d.NextBlock(0) {
		return d.Err("log trace takes no block")
	}

	return nil
}

func (t *Trace) Init(_ context.Context, h *aish.Host) error {
	path := t.Path
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("aish: log.trace: %w", err)
		}
		path = filepath.Join(home, ".aish", "traces", "%s.trace")
	}
	path = pathfmt.Format(path, h.Session().ID, time.Now())
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("aish: log.trace: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode) //nolint:gosec // path comes from the Agentfile or the checked session ID
	if err != nil {
		return fmt.Errorf("aish: log.trace: %w", err)
	}
	t.file = f
	h.Observe("write", func(ctx context.Context, c aish.TxCommitted) {
		if err := trace.Write(f, c.Tx); err != nil {
			h.Log().ErrorContext(ctx, "cannot write trace", "err", err)
		}
	})

	return nil
}

func (t *Trace) Stop(context.Context) error {
	if t.file == nil {
		return nil
	}
	if err := t.file.Close(); err != nil {
		return fmt.Errorf("aish: log.trace: %w", err)
	}

	return nil
}
