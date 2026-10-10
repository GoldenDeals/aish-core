// Package fs provides the file tools fs_read and fs_edit. Both are confined
// to a root directory, whatever the policy says.
package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	describePriority = 2000
	// toolsPriority is the enrichment band (§4.4).
	toolsPriority = 500
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "tool.fs_read", New: func() aish.Module { return &Read{} }})
	aish.RegisterModule(aish.ModuleInfo{ID: "tool.fs_edit", New: func() aish.Module { return &Edit{} }})
}

// tool is what Read and Edit share: the root, the spec and the call hooks.
type tool struct {
	// Root confines every path; the working directory when empty.
	Root string

	root     *os.Root
	absRoot  string
	spec     events.ToolSpec
	readOnly bool
	action   string // Access.Action
	run      func(input json.RawMessage) (string, error)
}

// UnmarshalCaddyfile reads "tool <name> [root] { root <dir> }".
func (t *tool) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	switch args := d.RemainingArgs(); len(args) {
	case 0:
	case 1:
		t.Root = args[0]
	default:
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		if d.Val() != "root" {
			return d.Errf("unknown option %q", d.Val())
		}
		if !d.Args(&t.Root) {
			return d.ArgErr()
		}
	}

	return nil
}

func (t *tool) init(h *aish.Host) error {
	abs, err := filepath.Abs(t.Root)
	if err != nil {
		return fmt.Errorf("aish: %s: %w", h.Instance(), err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("aish: %s: %w", h.Instance(), err)
	}
	t.root, t.absRoot = root, abs

	h.Mutate("advertise", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		m.Event().AppendTool(t.spec)

		return nil
	}, aish.Priority(toolsPriority))
	h.Mutate("effects", func(_ context.Context, m *aish.Mut[events.ToolCall]) error {
		c := m.Event()
		// Unparsable input leaves Access empty and the call not read-only,
		// so a policy treats it as unknown.
		if path, ok := t.path(c.Input); ok {
			c.Effects = events.Effects{
				ReadOnly: t.readOnly,
				Access:   []events.Access{{Action: t.action, Resource: "file:" + path}},
			}
		}

		return nil
	}, aish.Priority(describePriority), aish.Where(t.is))
	h.Handle("exec", func(ctx context.Context, c events.ToolCall) error {
		h.Log().InfoContext(ctx, "running tool", "tool", c.Name, "call", c.ID)
		out, err := t.run(c.Input)
		if err != nil {
			h.Log().WarnContext(ctx, "tool failed", "tool", c.Name, "call", c.ID, "output_bytes", len(out), "err", err)
		} else {
			h.Log().InfoContext(ctx, "tool finished", "tool", c.Name, "call", c.ID, "output_bytes", len(out))
		}
		res := events.ToolResult{CallID: c.ID, Content: []events.Block{{Type: events.BlockText, Text: out}}}
		if err != nil {
			res.IsError, res.Content[0].Text = true, err.Error()
		}
		if err := h.Emit(ctx, res); err != nil && !errors.Is(err, aish.ErrUnhandled) {
			return fmt.Errorf("aish: %s: %w", h.Instance(), err)
		}

		return nil
	}, aish.Where(t.is))

	return nil
}

func (t *tool) Stop(context.Context) error {
	if t.root == nil {
		return nil
	}
	if err := t.root.Close(); err != nil {
		return fmt.Errorf("aish: close root: %w", err)
	}

	return nil
}

func (t *tool) is(c events.ToolCall) bool { return c.Name == t.spec.Name }

// path returns the absolute path of the call's input.
func (t *tool) path(input json.RawMessage) (string, bool) {
	var in struct{ Path string }
	if json.Unmarshal(input, &in) != nil || in.Path == "" {
		return "", false
	}
	if filepath.IsAbs(in.Path) {
		return filepath.Clean(in.Path), true
	}

	return filepath.Join(t.absRoot, in.Path), true
}

// rel turns an absolute path into one relative to the root. os.Root then
// refuses everything that leaves it, symlinks included.
func (t *tool) rel(path string) (string, error) {
	rel, err := filepath.Rel(t.absRoot, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside the root %s", path, t.absRoot)
	}

	return rel, nil
}
