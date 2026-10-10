// Package agentfile is the config loader config.agentfile: it turns an
// Agentfile (Caddyfile syntax) into agent blocks and module instances. It
// knows no agent option itself; modules own them through
// aish.RegisterDirective.
package agentfile

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
)

const (
	mainAgent         = "main"
	idSeparator       = "."
	instanceSeparator = "/"
)

func init() {
	aish.RegisterConfigLoader("config.agentfile", func() aish.ConfigLoader { return Loader{} })
}

// Loader is the config loader for Agentfiles.
type Loader struct{}

// file is the parsed Agentfile; every agent is built from it again for each
// session.
type file struct {
	global *caddyfile.ServerBlock
	agents map[string]*caddyfile.ServerBlock
	order  []string
}

// Load reads the optional leading block "{ ... }" and the agent blocks. Every
// block is an independent agent; "main" is required. Global directives apply
// to every agent unless it configures the same module itself. A directive
// "<namespace> <name> [args] [{ ... }]" creates the module
// <namespace>.<name> and hands the directive to its UnmarshalCaddyfile; a
// directive registered with aish.RegisterDirective sets an agent option.
func (Loader) Load(filename string, r io.Reader) (*aish.Config, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("aish: config: %w", err)
	}
	blocks, err := caddyfile.Parse(filename, src)
	if err != nil {
		return nil, fmt.Errorf("aish: config: %w", err)
	}
	f := &file{agents: map[string]*caddyfile.ServerBlock{}}
	for i, b := range blocks {
		keys := b.GetKeysText()
		switch {
		case len(keys) == 0:
			f.global = &blocks[i]
		case len(keys) == 1 && f.agents[keys[0]] == nil:
			f.agents[keys[0]] = &blocks[i]
			f.order = append(f.order, keys[0])
		default:
			return nil, fmt.Errorf("aish: config: invalid or repeated agent block %v", keys)
		}
	}
	if f.agents[mainAgent] == nil {
		return nil, fmt.Errorf("aish: config: no %q block", mainAgent)
	}

	// Build every agent once, so config errors show up before the session.
	var errs []error
	specs := make([]aish.AgentSpec, 0, len(f.order))
	for _, name := range f.order {
		b, err := f.build(name)
		errs = append(errs, err)
		if err == nil {
			specs = append(specs, b.spec)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return &aish.Config{
		Agents: specs,
		Build: func(agent string) ([]aish.Instance, error) {
			b, err := f.build(agent)
			if err != nil {
				return nil, err
			}

			return b.instances, nil
		},
	}, nil
}

// built is one agent block turned into objects.
type built struct {
	spec      aish.AgentSpec
	instances []aish.Instance
}

// build creates new instances of the agent's modules, with the global
// directives underneath.
func (f *file) build(agent string) (*built, error) {
	block := f.agents[agent]
	if block == nil {
		return nil, fmt.Errorf("aish: config: unknown agent %q", agent)
	}
	var global, own built
	var errs []error
	if f.global != nil {
		for _, st := range f.global.Segments {
			errs = append(errs, global.add(st, &global.spec.Own, aish.ScopeGlobal))
		}
	}
	for _, st := range block.Segments {
		errs = append(errs, own.add(st, &own.spec.Own, aish.ScopeAgent))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// The agent block overrides a global instance of the same module.
	for _, g := range slices.Backward(global.instances) {
		if !slices.ContainsFunc(own.instances, func(i aish.Instance) bool { return i.Name == g.Name }) {
			own.instances = slices.Insert(own.instances, 0, g)
		}
	}
	own.spec.Name = agent
	own.spec.Global = global.spec.Own

	return &own, nil
}

func (b *built) add(st caddyfile.Segment, values *aish.Values, scope aish.Scope) error {
	d := caddyfile.NewDispenser(st)
	d.Next()
	dir := d.Val()

	if option, ok := aish.LookupDirective(dir); ok {
		return option(d, values, scope)
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
	b.instances = append(b.instances, aish.Instance{Name: id, Module: m})

	return nil
}
