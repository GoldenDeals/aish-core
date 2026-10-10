// Package agentsmd adds AGENTS.md files to the system prompt: files matching
// configured glob patterns, the user-level ~/.agents/AGENTS.md, and the
// AGENTS.md files on the way from the home directory down to the working one.
package agentsmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	// SectionName is the system prompt section holding the files.
	SectionName = "agents_md"
	fileName    = "AGENTS.md"
	// enrichPriority is the enrichment band (§4.4), below "base" so it comes after it.
	enrichPriority = 500
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "context.agents_md", New: func() aish.Module { return &AgentsMD{} }})
}

type AgentsMD struct {
	// Patterns are globs, relative to the working directory unless absolute;
	// a leading ~ is the home directory.
	Patterns []string
	// Home adds ~/.agents/AGENTS.md.
	Home bool
	// Local adds AGENTS.md of the working directory only.
	Local bool
	// Walk adds the AGENTS.md of every directory from ~ down to the parent of
	// the working directory; nothing when the working directory is outside ~.
	Walk bool
}

// UnmarshalCaddyfile reads "context agents_md [pattern...] { pattern <glob>...; home; local; walk }".
func (a *AgentsMD) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	a.Patterns = d.RemainingArgs()
	for d.NextBlock(0) {
		switch d.Val() {
		case "pattern":
			a.Patterns = append(a.Patterns, d.RemainingArgs()...)
		case "home":
			a.Home = true
		case "local":
			a.Local = true
		case "walk":
			a.Walk = true
		default:
			return d.Errf("unknown option %q", d.Val())
		}
	}

	return nil
}

func (a *AgentsMD) Init(_ context.Context, h *aish.Host) error {
	text, err := a.render()
	if err != nil || text == "" {
		return err
	}
	h.Log().Debug("AGENTS.md sections loaded", "bytes", len(text))
	h.Mutate("agents_md", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		m.Event().Section(SectionName).Text = text

		return nil
	}, aish.Priority(enrichPriority))

	return nil
}

func (a *AgentsMD) render() (string, error) {
	files, err := a.files()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // found via configured patterns and well-known locations
		if err != nil {
			return "", fmt.Errorf("aish: context.agents_md: %w", err)
		}
		body := strings.TrimSpace(string(raw))
		if body == "" {
			continue
		}
		fmt.Fprintf(&b, "Instructions from %s:\n\n%s\n\n", f, body)
	}

	return strings.TrimSpace(b.String()), nil
}

// files returns existing files in prompt order, most general first, without duplicates.
func (a *AgentsMD) files() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("aish: context.agents_md: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("aish: context.agents_md: %w", err)
	}
	var cands []string
	if a.Home {
		cands = append(cands, filepath.Join(home, ".agents", fileName))
	}
	if a.Walk {
		cands = append(cands, walkFiles(home, cwd)...)
	}
	if a.Local {
		cands = append(cands, filepath.Join(cwd, fileName))
	}
	for _, p := range a.Patterns {
		if rest, ok := strings.CutPrefix(p, "~"); ok {
			p = filepath.Join(home, rest)
		} else if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("aish: context.agents_md: %w", err)
		}
		slices.Sort(matches)
		cands = append(cands, matches...)
	}

	var out []string
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}

	return out, nil
}

func walkFiles(home, cwd string) []string {
	rel, err := filepath.Rel(home, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	dir := home
	files := []string{filepath.Join(dir, fileName)}
	if rel != "." {
		parts := strings.Split(rel, string(filepath.Separator))
		for _, part := range parts[:len(parts)-1] {
			dir = filepath.Join(dir, part)
			files = append(files, filepath.Join(dir, fileName))
		}
	}

	return files
}
