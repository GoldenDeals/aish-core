// Package appendprompt adds text, written inline or read from files, to the
// end of the system prompt.
package appendprompt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	// SectionName is the system prompt section holding the text.
	SectionName = "append"
	// appendPriority is below the other modules that add sections (500), so
	// the section comes last.
	appendPriority = 400
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "context.append", New: func() aish.Module { return &Append{} }})
}

// Append is the module context.append.
type Append struct {
	// Parts are the texts and files, in the order they were written.
	Parts []Part
}

// Part is inline text, or the file to take the text from.
type Part struct {
	Text string
	Path string
}

// UnmarshalCaddyfile reads "context append [file...] [text <text>] [path <file>...]".
// The parts keep the order they were written in.
func (a *Append) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	positional, err := aish.ReadOptions(d, map[string]aish.Option{
		"text": func(d *caddyfile.Dispenser) error {
			var text string
			if !d.Args(&text) {
				return d.ArgErr()
			}
			a.Parts = append(a.Parts, Part{Text: text})

			return nil
		},
		"path": func(d *caddyfile.Dispenser) error {
			paths := d.RemainingArgs()
			if len(paths) == 0 {
				return d.ArgErr()
			}
			for _, path := range paths {
				a.Parts = append(a.Parts, Part{Path: path})
			}

			return nil
		},
	})
	if err != nil {
		return err //nolint:wrapcheck // carries the Agentfile position
	}
	files := make([]Part, 0, len(positional)+len(a.Parts))
	for _, path := range positional {
		files = append(files, Part{Path: path})
	}
	a.Parts = append(files, a.Parts...)

	return nil
}

func (a *Append) Validate() error {
	if len(a.Parts) == 0 {
		return errors.New("aish: context.append: give some text or a file")
	}
	for _, p := range a.Parts {
		if (p.Text == "") == (p.Path == "") {
			return errors.New("aish: context.append: a part is either text or a file")
		}
	}

	return nil
}

func (a *Append) Init(_ context.Context, h *aish.Host) error {
	text, err := a.render()
	if err != nil {
		return err
	}
	h.Log().Debug("system prompt addition resolved", "parts", len(a.Parts), "bytes", len(text), aish.PayloadAttr("text", text))
	h.Mutate("append", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		m.Event().Section(SectionName).Text = text

		return nil
	}, aish.Priority(appendPriority))

	return nil
}

// render reads the files once, so the section does not change between
// requests and the prompt cache stays valid.
func (a *Append) render() (string, error) {
	texts := make([]string, 0, len(a.Parts))
	for _, p := range a.Parts {
		if p.Path == "" {
			texts = append(texts, p.Text)

			continue
		}
		b, err := os.ReadFile(p.Path)
		if err != nil {
			return "", fmt.Errorf("aish: context.append: %w", err)
		}
		texts = append(texts, strings.TrimRight(string(b), "\n"))
	}

	return strings.Join(texts, "\n\n"), nil
}
