// Package systemprompt owns the "base" section of the system prompt: a
// built-in default, or text or a file taken from the config.
package systemprompt

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	// BaseSection is the section name other modules can look up in LLMRequest.
	BaseSection = "base"
	// promptPriority is the enrichment band (§4.4), above the modules that
	// add their own sections, so "base" comes first.
	promptPriority = 900
)

// DefaultText is adapted from the Claude Code prompts collected in
// github.com/Piebald-AI/claude-code-system-prompts.
//
//go:embed default.md
var DefaultText string

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "context.system_prompt", New: func() aish.Module { return &SystemPrompt{} }})
}

type SystemPrompt struct {
	// Text is the prompt itself. Path, if set, names a file read at Init
	// instead. With neither, DefaultText is used.
	Text string
	Path string
}

// UnmarshalCaddyfile reads "context system_prompt [path] { text <text>; path <file> }".
func (s *SystemPrompt) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	switch args := d.RemainingArgs(); len(args) {
	case 0:
	case 1:
		s.Path = args[0]
	default:
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		var dst *string
		switch d.Val() {
		case "text":
			dst = &s.Text
		case "path":
			dst = &s.Path
		default:
			return d.Errf("unknown option %q", d.Val())
		}
		if !d.Args(dst) {
			return d.ArgErr()
		}
	}

	return nil
}

func (s *SystemPrompt) Validate() error {
	if s.Text != "" && s.Path != "" {
		return errors.New("aish: context.system_prompt: set either text or path, not both")
	}

	return nil
}

func (s *SystemPrompt) Init(_ context.Context, h *aish.Host) error {
	text, err := s.resolve()
	if err != nil {
		return err
	}
	h.Mutate("base", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		m.Event().Section(BaseSection).Text = text

		return nil
	}, aish.Priority(promptPriority))

	return nil
}

func (s *SystemPrompt) resolve() (string, error) {
	switch {
	case s.Path != "":
		b, err := os.ReadFile(s.Path)
		if err != nil {
			return "", fmt.Errorf("aish: context.system_prompt: %w", err)
		}

		return string(b), nil
	case s.Text != "":
		return s.Text, nil
	default:
		return DefaultText, nil
	}
}
