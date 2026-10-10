// Package file is the log module: it decides where the session's slog records go.
package file

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/modules/log/internal/pathfmt"
)

const (
	dirMode  = 0o755
	fileMode = 0o600
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "log.file", New: func() aish.Module { return &Log{} }})
}

type Log struct {
	// Path is the log file, with pathfmt placeholders; ~/.aish/logs/%s.log when empty.
	Path  string
	Level slog.Level
}

// UnmarshalCaddyfile reads "log file [path] { level <level> }".
func (l *Log) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	switch args := d.RemainingArgs(); len(args) {
	case 0:
	case 1:
		l.Path = args[0]
	default:
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "level":
			var s string
			if !d.Args(&s) {
				return d.ArgErr()
			}
			if err := l.Level.UnmarshalText([]byte(s)); err != nil {
				return d.Errf("log level: %v", err)
			}
		default:
			return d.Errf("unknown log option %q", d.Val())
		}
	}

	return nil
}

// Logger opens the log file for the session. It runs before the session
// exists, so the app calls it instead of the module's Init; the caller closes
// the returned Closer. The terminal belongs to the frontend, so logs never go
// to stdout or stderr.
func (l *Log) Logger(session string) (*slog.Logger, io.Closer, error) {
	path := l.Path
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("aish: log.file: %w", err)
		}
		path = filepath.Join(home, ".aish", "logs", "%s.log")
	}
	path = pathfmt.Format(path, session, time.Now())
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, nil, fmt.Errorf("aish: log.file: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode) //nolint:gosec // path comes from the Agentfile or the checked session ID
	if err != nil {
		return nil, nil, fmt.Errorf("aish: log.file: %w", err)
	}

	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: l.Level})), f, nil
}

// Init does nothing: the logger exists before the session does.
func (l *Log) Init(context.Context, *aish.Host) error { return nil }
