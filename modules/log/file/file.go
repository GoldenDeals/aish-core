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
	"github.com/GoldenDeals/aish-core/internal/rotate"
	"github.com/GoldenDeals/aish-core/modules/log/internal/pathfmt"
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "log.file", New: func() aish.Module { return &Log{} }})
}

type Log struct {
	// Path is the log file, with pathfmt placeholders; ~/.aish/logs/%s.log when empty.
	Path  string
	Level slog.Level
	// Rotate starts a new file once this much time has passed, when the path
	// has time placeholders; zero never rotates.
	Rotate time.Duration
}

// UnmarshalCaddyfile reads "log file [path] [level <level>] [rotate <duration>]".
func (l *Log) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	positional, err := aish.ReadOptions(d, map[string]aish.Option{
		"rotate": rotate.Option(&l.Rotate),
		"level": func(d *caddyfile.Dispenser) error {
			var s string
			if !d.Args(&s) {
				return d.ArgErr()
			}
			if err := l.Level.UnmarshalText([]byte(s)); err != nil {
				return d.Errf("log level: %v", err)
			}

			return nil
		},
	})
	if err != nil {
		return err //nolint:wrapcheck // carries the Agentfile position
	}
	switch len(positional) {
	case 0:
	case 1:
		l.Path = positional[0]
	default:
		return d.ArgErr()
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
	w, err := rotate.Open(&rotate.Writer{
		Period: l.Rotate,
		Name:   func(now time.Time) string { return pathfmt.Format(path, session, now) },
		Create: rotate.CreateFile,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("aish: log.file: %w", err)
	}

	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: l.Level})), w, nil
}

// Init does nothing: the logger exists before the session does.
func (l *Log) Init(context.Context, *aish.Host) error { return nil }

// NoInherit keeps Log out of merging blocks: the session log belongs to the root agent.
func (*Log) NoInherit() {}
