// Package trace is the log.trace module (§2, principle 8): it appends the
// trace of every committed transaction to a file right after the commit, so a
// hung or killed session still leaves its trace. The trace shows who got,
// changed, removed and emitted which events.
package trace

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/internal/rotate"
	"github.com/GoldenDeals/aish-core/modules/log/internal/pathfmt"
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "log.trace", New: func() aish.Module { return &Trace{} }})
}

type Trace struct {
	// Path is the trace file, with pathfmt placeholders; ~/.aish/traces/%s.trace when empty.
	Path string

	// Rotate starts a new file once this much time has passed, when the path
	// has time placeholders; zero never rotates.
	Rotate time.Duration

	file io.Closer
}

// UnmarshalCaddyfile reads "log trace [path] [rotate <duration>]".
func (t *Trace) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	positional, err := aish.ReadOptions(d, map[string]aish.Option{"rotate": rotate.Option(&t.Rotate)})
	if err != nil {
		return err //nolint:wrapcheck // carries the Agentfile position
	}
	switch len(positional) {
	case 0:
	case 1:
		t.Path = positional[0]
	default:
		return d.ArgErr()
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
	session := h.Session().ID
	w, err := rotate.Open(&rotate.Writer{
		Period: t.Rotate,
		Name:   func(now time.Time) string { return pathfmt.Format(path, session, now) },
		Create: rotate.CreateFile,
	})
	if err != nil {
		return fmt.Errorf("aish: log.trace: %w", err)
	}
	t.file = w
	h.Observe("write", func(ctx context.Context, c aish.TxCommitted) {
		if err := Write(w, c.Tx); err != nil {
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

// Write prints tx as a tree: every step is indented by its Emit depth, so
// the events a hook emitted sit under the hook. Zero durations are left out,
// which keeps the output of a trace with Dur cleared stable for golden files.
//
//	tx 2 committed
//	  emit      2.1 tool.call llm.fake
//	  remove    2.1 tool.call policy.ask:authorize (40µs)
//	    emit      2.2 user.question policy.ask:authorize
//	events: 2.2 user.question
func Write(w io.Writer, tx aish.Tx) error {
	var b strings.Builder
	fmt.Fprintf(&b, "tx %d %s\n", tx.Seq, tx.Status)
	for _, e := range tx.Trace {
		b.WriteString(strings.Repeat("  ", max(e.Depth, 1)))
		fmt.Fprintf(&b, "%-9s", e.Action)
		for _, field := range []string{e.Event, e.Kind, e.Hook} {
			if field != "" {
				b.WriteString(" " + field)
			}
		}
		if e.Dur > 0 {
			fmt.Fprintf(&b, " (%s)", e.Dur)
		}
		if e.Err != "" {
			b.WriteString(": " + e.Err)
		}
		b.WriteByte('\n')
	}

	events := make([]string, len(tx.Events))
	for i, env := range tx.Events {
		events[i] = env.Meta.ID + " " + env.Event.Kind()
	}
	fmt.Fprintf(&b, "events: %s\n", strings.Join(events, ", "))

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("aish: log.trace: write: %w", err)
	}

	return nil
}
