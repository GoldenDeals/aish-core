// Package rotate is the writer behind the optional "rotate <duration>"
// setting of the log, trace and session files.
package rotate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
)

const (
	dirMode  = 0o755
	fileMode = 0o600
)

// Writer writes to the file Name gives for the current time and, when Period
// is set, looks the name up again once Period has passed since the file was
// opened. A new name means a new file; the same name keeps the open one, so a
// path without time placeholders never rotates.
type Writer struct {
	Period time.Duration
	Name   func(now time.Time) string
	Create func(name string) (io.WriteCloser, error)

	mu     sync.Mutex
	cur    io.WriteCloser
	name   string
	opened time.Time
}

// Open creates the first file now, so a bad path fails at Init.
func Open(w *Writer) (*Writer, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.reopen(time.Now()); err != nil {
		return nil, err
	}

	return w, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if now := time.Now(); w.Period > 0 && now.Sub(w.opened) >= w.Period {
		if err := w.reopen(now); err != nil {
			return 0, err
		}
	}
	n, err := w.cur.Write(p)
	if err != nil {
		return n, fmt.Errorf("aish: write: %w", err)
	}

	return n, nil
}

func (w *Writer) reopen(now time.Time) error {
	name := w.Name(now)
	if w.cur != nil && name == w.name {
		w.opened = now

		return nil
	}
	f, err := w.Create(name)
	if err != nil {
		return err
	}
	var closeErr error
	if w.cur != nil {
		closeErr = w.cur.Close()
	}
	w.cur, w.name, w.opened = f, name, now

	if closeErr != nil {
		return fmt.Errorf("aish: rotate: %w", closeErr)
	}

	return nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.cur == nil {
		return nil
	}
	if err := w.cur.Close(); err != nil {
		return fmt.Errorf("aish: close: %w", err)
	}

	return nil
}

// CreateFile opens name for appending, creating its directory.
func CreateFile(name string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(name), dirMode); err != nil {
		return nil, fmt.Errorf("aish: %w", err)
	}
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode) //nolint:gosec // the name comes from the Agentfile or the checked session ID
	if err != nil {
		return nil, fmt.Errorf("aish: %w", err)
	}

	return f, nil
}

// Period reads the argument of "rotate <duration>".
func Period(d *caddyfile.Dispenser) (time.Duration, error) {
	var s string
	if !d.Args(&s) {
		return 0, d.ArgErr()
	}
	p, err := time.ParseDuration(s)
	if err != nil || p <= 0 {
		return 0, errors.Join(d.Errf("rotate wants a positive duration like 24h, got %q", s), err)
	}

	return p, nil
}

// Option is the module option "rotate <duration>", stored in dst.
func Option(dst *time.Duration) aish.Option {
	return func(d *caddyfile.Dispenser) error {
		p, err := Period(d)
		if err != nil {
			return err
		}
		*dst = p

		return nil
	}
}
