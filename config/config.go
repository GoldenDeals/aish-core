// Package config turns an Agentfile (Caddyfile syntax) into the options and module instances of
// one session.
package config

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

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

// Load reads the optional leading block "{ ... }" and the agent block "main".
// Global directives apply to the agent unless it configures the same module
// itself. Every directive "<namespace> <name>
// [args] [{ ... }]" creates the module <namespace>.<name> and hands the
// directive to its UnmarshalCaddyfile. The directive "max_steps N" sets
// Options.MaxSteps. Subagent blocks are not supported
// yet; a module names its instance via InstanceName (§7 is under revision).
func Load(filename string, r io.Reader) (*Config, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("aish: config: %w", err)
	}
	blocks, err := caddyfile.Parse(filename, src)
	if err != nil {
		return nil, fmt.Errorf("aish: config: %w", err)
	}
	var global, main *caddyfile.ServerBlock
	for i, b := range blocks {
		keys := b.GetKeysText()
		switch {
		case len(keys) == 0:
			global = &blocks[i]
		case len(keys) == 1 && keys[0] == mainAgent:
			main = &blocks[i]
		default:
			return nil, fmt.Errorf("aish: config: agent block %v is not supported, only %q", keys, mainAgent)
		}
	}
	if main == nil {
		return nil, fmt.Errorf("aish: config: no %q block", mainAgent)
	}

	var globalCfg, cfg Config
	var errs []error
	if global != nil {
		for _, st := range global.Segments {
			errs = append(errs, globalCfg.add(st))
		}
	}
	for _, st := range main.Segments {
		errs = append(errs, cfg.add(st))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// The agent block overrides a global instance of the same module.
	for _, g := range slices.Backward(globalCfg.Instances) {
		if !slices.ContainsFunc(cfg.Instances, func(i aish.Instance) bool { return i.Name == g.Name }) {
			cfg.Instances = slices.Insert(cfg.Instances, 0, g)
		}
	}
	if globalCfg.Options.MaxSteps > 0 && cfg.Options.MaxSteps == 0 {
		cfg.Options.MaxSteps = globalCfg.Options.MaxSteps
	}

	return &cfg, nil
}

func (c *Config) add(st caddyfile.Segment) error {
	d := caddyfile.NewDispenser(st)
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
		d = caddyfile.NewDispenser(st)
		d.Next()
		if err := u.UnmarshalCaddyfile(d); err != nil {
			return fmt.Errorf("aish: config: %s: %w", id, err)
		}
	} else if d.NextArg() || d.NextBlock(0) {
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
