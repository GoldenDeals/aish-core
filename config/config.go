// Package config turns an Agentfile (Caddyfile syntax) into the options and module instances of
// one session.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/coredns/caddy/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
)

const (
	mainAgent         = "main"
	maxStepsOpt       = "max_steps"
	idSeparator       = "."
	instanceSeparator = "/"
)

type Config struct {
	Options aish.Options
	// Instances are in file order, named by module ID.
	Instances []aish.Instance
}

// Load reads the agent block "main": every directive "<namespace> <name>
// [args] [{ ... }]" creates the module <namespace>.<name> and hands the
// directive to its UnmarshalCaddyfile. The directive "max_steps N" sets
// Options.MaxSteps. Subagent blocks are not supported
// yet; a module names its instance via InstanceName (§7 is under revision).
func Load(filename string, r io.Reader) (*Config, error) {
	blocks, err := caddyfile.Parse(filename, r, nil)
	if err != nil {
		return nil, fmt.Errorf("aish: config: %w", err)
	}
	var main *caddyfile.ServerBlock
	for i, b := range blocks {
		if len(b.Keys) != 1 || b.Keys[0] != mainAgent {
			return nil, fmt.Errorf("aish: config: agent block %v is not supported, only %q", b.Keys, mainAgent)
		}
		main = &blocks[i]
	}
	if main == nil {
		return nil, fmt.Errorf("aish: config: no %q block", mainAgent)
	}

	var cfg Config
	var errs []error
	for _, st := range statements(main.Tokens) {
		if err := cfg.add(filename, st); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// statements splits the tokens the parser grouped by directive name back
// into directives and puts them in file order.
//
// shortcut: order comes from line numbers, so directives pulled in by a
// snippet import sort by their place in the snippet; replace the parser if
// that order starts to matter.
func statements(byDirective map[string][]caddyfile.Token) [][]caddyfile.Token {
	var out [][]caddyfile.Token
	for _, toks := range byDirective {
		start, depth := 0, 0
		for i, t := range toks {
			if i > start && depth == 0 && t.Line != toks[i-1].Line {
				out = append(out, toks[start:i])
				start = i
			}
			switch t.Text {
			case "{":
				depth++
			case "}":
				depth--
			}
		}
		out = append(out, toks[start:])
	}
	slices.SortStableFunc(out, func(a, b []caddyfile.Token) int { return cmp.Compare(a[0].Line, b[0].Line) })

	return out
}

func (c *Config) add(filename string, st []caddyfile.Token) error {
	d := caddyfile.NewDispenserTokens(filename, st)
	d.Next()
	dir := d.Val()

	if dir == maxStepsOpt {
		var n string
		if !d.Args(&n) || len(d.RemainingArgs()) > 0 {
			return d.ArgErr()
		}
		steps, err := strconv.Atoi(n)
		if err != nil || steps < 1 {
			return d.Errf("%s wants a positive number, got %q", maxStepsOpt, n)
		}
		c.Options.MaxSteps = steps

		return nil
	}

	var name string
	if !d.NextArg() {
		return d.Errf("directive %q needs a module name", dir)
	}
	name = d.Val()
	id := dir + idSeparator + name
	info, ok := aish.LookupModule(id)
	if !ok {
		return d.Errf("unknown module %s", id)
	}
	m := info.New()

	// The module reads its own directive; the dispenser stands on its name.
	if u, ok := m.(aish.CaddyfileUnmarshaler); ok {
		d = caddyfile.NewDispenserTokens(filename, st)
		d.Next()
		if err := u.UnmarshalCaddyfile(&d); err != nil {
			return fmt.Errorf("aish: config: %s: %w", id, err)
		}
	} else if d.NextArg() || d.NextBlock() {
		return d.Errf("module %s takes no arguments", id)
	}
	// A module that is configured once per server and not once per session
	// names its instances itself: "tool mcp github" is tool.mcp/github.
	if n, ok := m.(interface{ InstanceName() string }); ok && n.InstanceName() != "" {
		id += instanceSeparator + n.InstanceName()
	}
	c.Instances = append(c.Instances, aish.Instance{Name: id, Module: m})

	return nil
}
