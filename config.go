package aish

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

const (
	// RootAgent is the name of the agent the root of the config describes.
	RootAgent         = "main"
	idSeparator       = "."
	instanceSeparator = "/"
)

// Values holds one value per Go type. A module keeps its settings in it under
// its own settings type, so the core never needs to know what a setting
// means. Store values, not pointers: Values is copied shallowly, and a block
// that merges with the root gets Inherited() any of a value that has the method
// instead of the value itself, for settings that belong to one agent. The zero
// Values is empty.
type Values struct {
	byType map[reflect.Type]any
}

// Set stores v under its type, replacing the previous value.
func Set[T any](vs *Values, v T) {
	if vs.byType == nil {
		vs.byType = map[reflect.Type]any{}
	}
	vs.byType[reflect.TypeFor[T]()] = v
}

// Get returns the value stored under T, if any.
func Get[T any](vs Values) (T, bool) {
	v, ok := vs.byType[reflect.TypeFor[T]()].(T)

	return v, ok
}

// AgentSpec is one agent of the config as modules see it: the root, called
// RootAgent, or a block that a module (Owner) accepted. Values holds what the
// agent's directives set, including what a merging block inherited.
type AgentSpec struct {
	Name   string
	Owner  string
	Values Values
}

// Section tells a directive handler which part of the config it reads: the
// root (the first block of the file, the agent RootAgent) or a named block.
type Section struct {
	Name string
	Root bool
}

// DirectiveFunc reads one directive that a module added to the config, such as
// "max_steps 50", into the values of the section. The Dispenser stands on the
// directive's first token.
type DirectiveFunc func(d *caddyfile.Dispenser, v *Values, s Section) error

// BlockFunc is offered every named block "<name> [flags] { ... }" of the file.
// It returns false if the block is not its kind; otherwise it reads the
// header and calls Block.Declare.
type BlockFunc func(b *Block) (claimed bool, err error)

// ConfigExtender is implemented by a module that adds directives or block
// kinds to the config. The config understands them only in a section that
// contains the module, and in the named blocks next to it, so a config
// without the module fails on them. ExtendConfig runs on a throwaway
// instance: it only registers and must not keep the instance in a closure.
type ConfigExtender interface {
	ExtendConfig(c *ConfigExt)
}

// ConfigExt collects what the modules of a section add to the config.
type ConfigExt struct {
	owner      string
	directives map[string]DirectiveFunc
	owners     map[string]string
	blocks     []blockKind
	errs       []error
}

type blockKind struct {
	owner string
	fn    BlockFunc
}

// Directive adds the directive name to the config.
func (c *ConfigExt) Directive(name string, fn DirectiveFunc) {
	prev, dup := c.owners[name]
	if dup && prev == c.owner {
		// A named block repeats a module of the root, or two instances of one module.
		return
	}
	if dup || name == "" || fn == nil {
		c.errs = append(c.errs, fmt.Errorf("aish: config: %s: directive %q is invalid or already added by %s", c.owner, name, prev))

		return
	}
	c.directives[name] = fn
	c.owners[name] = c.owner
}

// Block lets the module accept named blocks.
func (c *ConfigExt) Block(fn BlockFunc) {
	c.blocks = append(c.blocks, blockKind{owner: c.owner, fn: fn})
}

// NoInheritor is implemented by modules that a merging block does not take
// over from the root: a subagent has no frontend or store of its own.
type NoInheritor interface {
	NoInherit()
}

// Block is one named block of the file while a BlockFunc looks at it.
type Block struct {
	// Name is the first key of the block header, Flags are the others.
	Name  string
	Flags []string

	f        *configFile
	segments []caddyfile.Segment
	owner    string
	declared bool
}

// Declare adds the block to the config as an agent. With inherit the agent
// starts from the root's instances and values and the block overrides them
// (a module of the same instance name replaces the root's); without it the
// agent has only what the block lists. Modules that implement NoInheritor are
// never taken over.
func (b *Block) Declare(inherit bool) error {
	if b.declared {
		return fmt.Errorf("aish: config: block %q declared twice", b.Name)
	}
	b.declared = true
	b.f.blocks[b.Name] = &blockDef{segments: b.segments, owner: b.owner, inherit: inherit}
	b.f.order = append(b.f.order, b.Name)

	return nil
}

// Config is what LoadConfig produces: the agents and a way to build fresh
// module instances of any of them.
type Config struct {
	// Agents are in config order; the first is the root.
	Agents []AgentSpec
	// Build creates new instances of the named agent's modules, in config
	// order. It is the SessionConfig.Spawner of every session.
	Build func(agent string) ([]Instance, error)
}

// configFile is the parsed file; every agent is built from it again for each
// session.
type configFile struct {
	root   []caddyfile.Segment
	blocks map[string]*blockDef
	order  []string
	ext    *ConfigExt // what the root's modules add; named blocks get it too
}

type blockDef struct {
	segments []caddyfile.Segment
	owner    string
	inherit  bool
}

// built is one section turned into objects.
type built struct {
	spec      AgentSpec
	instances []Instance
	ids       []string // module ID of each instance
	extend    []bool   // whether the instance's module still has to extend the config
	ext       *ConfigExt
}

// LoadConfig reads an Agentfile (Caddyfile syntax). The first block, the one
// without keys, is the root: the settings and modules of the agent RootAgent.
// Every other block "<name> [flags] { ... }" is a named block, which only a
// module that added a block kind to the root can accept.
//
// The core understands one thing in a section: the directive
// "<namespace> <name> [args] [{ ... }]" creates the module
// <namespace>.<name> and hands the directive to its UnmarshalCaddyfile. All
// other directives and blocks come from ConfigExtender modules.
func LoadConfig(filename string, r io.Reader) (*Config, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("aish: config: %w", err)
	}
	parsed, err := caddyfile.Parse(filename, src)
	if err != nil {
		return nil, fmt.Errorf("aish: config: %w", err)
	}

	f := &configFile{blocks: map[string]*blockDef{}}
	var named []caddyfile.ServerBlock
	hasRoot := false
	for _, b := range parsed {
		if len(b.GetKeysText()) > 0 {
			named = append(named, b)

			continue
		}
		if hasRoot {
			return nil, errors.New("aish: config: more than one block without a name")
		}
		hasRoot = true
		f.root = b.Segments
	}
	if !hasRoot {
		return nil, errors.New("aish: config: no root block: start the file with \"{ ... }\"")
	}

	root, err := f.parseRoot()
	if err != nil {
		return nil, err
	}
	f.ext = root.ext
	specs := []AgentSpec{root.spec}

	for _, b := range named {
		if err := f.claim(b); err != nil {
			return nil, err
		}
	}
	for _, name := range f.order {
		bl, err := f.build(name)
		if err != nil {
			return nil, err
		}
		specs = append(specs, bl.spec)
	}

	return &Config{
		Agents: specs,
		Build: func(agent string) ([]Instance, error) {
			b, err := f.build(agent)
			if err != nil {
				return nil, err
			}

			return b.instances, nil
		},
	}, nil
}

// claim offers a named block to the block kinds of the root's modules.
func (f *configFile) claim(sb caddyfile.ServerBlock) error {
	keys := sb.GetKeysText()
	name := keys[0]
	if name == RootAgent || f.blocks[name] != nil {
		return fmt.Errorf("aish: config: block %q is reserved or repeated", name)
	}
	for _, kind := range f.ext.blocks {
		b := &Block{Name: name, Flags: keys[1:], f: f, segments: sb.Segments, owner: kind.owner}
		claimed, err := kind.fn(b)
		if err != nil {
			return fmt.Errorf("aish: config: %s: block %q: %w", kind.owner, name, err)
		}
		if claimed && b.declared {
			return nil
		}
		if claimed {
			return fmt.Errorf("aish: config: %s accepted block %q without declaring it", kind.owner, name)
		}
	}

	return fmt.Errorf("aish: config: unknown block %q: no module of the root accepts named blocks", name)
}

func (f *configFile) parseRoot() (*built, error) {
	return f.parse(f.root, Section{Name: RootAgent, Root: true}, nil)
}

// build creates new instances of the agent's modules.
func (f *configFile) build(agent string) (*built, error) {
	if agent == RootAgent {
		return f.parseRoot()
	}
	def := f.blocks[agent]
	if def == nil {
		return nil, fmt.Errorf("aish: config: unknown agent %q", agent)
	}
	var base *built
	if def.inherit {
		var err error
		if base, err = f.parseRoot(); err != nil {
			return nil, err
		}
	}
	b, err := f.parse(def.segments, Section{Name: agent}, base)
	if err != nil {
		return nil, err
	}
	b.spec.Owner = def.owner

	return b, nil
}

// parse reads the segments of a section in two phases, so the order of
// directives in the file does not matter: first the modules, which tell what
// else the section accepts, then the remaining directives. A section starts
// from the instances and values of base, if given. A module directive that
// names an instance already there, inherited or written earlier in the
// section, is read into that instance: the later options win and the others
// stay, so "llm anthropic model x" changes only the model.
func (f *configFile) parse(segs []caddyfile.Segment, sec Section, base *built) (*built, error) {
	b := &built{spec: AgentSpec{Name: sec.Name}}
	b.ext = &ConfigExt{directives: map[string]DirectiveFunc{}, owners: map[string]string{}}
	if f.ext != nil {
		for name, fn := range f.ext.directives {
			b.ext.directives[name] = fn
			b.ext.owners[name] = f.ext.owners[name]
		}
	}
	if base != nil {
		b.inherit(base)
	}

	var errs []error
	var pending []caddyfile.Segment
	for _, st := range expandSegments(segs) {
		isModule, err := b.addModule(st)
		switch {
		case err != nil:
			errs = append(errs, err)
		case !isModule:
			pending = append(pending, st)
		}
	}
	for i, inst := range b.instances {
		if x, ok := inst.Module.(ConfigExtender); ok && b.extend[i] {
			b.ext.owner = b.ids[i]
			x.ExtendConfig(b.ext)
		}
	}
	errs = append(errs, b.ext.errs...)

	for _, st := range pending {
		d := caddyfile.NewDispenser(st)
		d.Next()
		fn, ok := b.ext.directives[d.Val()]
		if !ok {
			errs = append(errs, d.Errf("unknown directive or module %q: is the module that provides it in the section?", d.Val()))

			continue
		}
		errs = append(errs, fn(d, &b.spec.Values, sec))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return b, nil
}

// inherit takes over the instances and values of base, except the modules that
// are NoInheritor.
func (b *built) inherit(base *built) {
	for i, inst := range base.instances {
		if _, skip := inst.Module.(NoInheritor); skip {
			continue
		}
		// The root's modules already extended the config of every block.
		b.add(inst, base.ids[i], false)
	}

	b.spec.Values = Values{byType: map[reflect.Type]any{}}
	for typ, v := range base.spec.Values.byType {
		if h, ok := v.(interface{ Inherited() any }); ok {
			v = h.Inherited()
		}
		b.spec.Values.byType[typ] = v
	}
}

func (b *built) add(inst Instance, id string, extend bool) {
	b.instances = append(b.instances, inst)
	b.ids = append(b.ids, id)
	b.extend = append(b.extend, extend)
}

// addModule creates the module of a "<namespace> <name>" directive, or reads
// the directive into the existing instance. It returns false if the directive
// is not a linked module.
func (b *built) addModule(st caddyfile.Segment) (bool, error) {
	d := caddyfile.NewDispenser(st)
	d.Next()
	dir := d.Val()
	if !d.NextArg() {
		return false, nil
	}
	id := dir + idSeparator + d.Val()
	info, ok := LookupModule(id)
	if !ok {
		return false, nil
	}
	m := info.New()
	_, named := m.(interface{ InstanceName() string })
	existing := slices.IndexFunc(b.instances, func(i Instance) bool { return i.Name == id })
	if existing >= 0 && !named {
		m = b.instances[existing].Module
	}

	// The module reads its own directive; the dispenser stands on its name.
	if u, ok := m.(CaddyfileUnmarshaler); ok {
		d = caddyfile.NewDispenser(st)
		d.Next()
		if err := u.UnmarshalCaddyfile(d); err != nil {
			return true, fmt.Errorf("aish: config: %s: %w", id, err)
		}
	} else if d.NextArg() || d.NextBlock(0) {
		return true, d.Errf("module %s takes no arguments", id)
	}
	if existing >= 0 && !named {
		return true, nil
	}
	// A module that is configured once per server and not once per session
	// names its instances itself: "tool mcp github" is tool.mcp/github.
	name := id
	if n, ok := m.(interface{ InstanceName() string }); ok && n.InstanceName() != "" {
		name += instanceSeparator + n.InstanceName()
	}
	if i := slices.IndexFunc(b.instances, func(i Instance) bool { return i.Name == name }); i >= 0 {
		b.instances[i] = Instance{Name: name, Module: m}

		return true, nil
	}
	b.add(Instance{Name: name, Module: m}, id, true)

	return true, nil
}

// expandSegments rewrites the two short forms of module directives into the
// plain "<namespace> <name> [args]" the rest of the parser reads:
//
//	tool {              tool fs_read
//	    fs_read    ->   tool fs_edit root .
//	    fs_edit root .
//	}
//
//	subagents core communication  ->  subagents core
//	                                  subagents communication
//
// A line holds several modules when its words after the namespace start with a
// module name; every later word that is a module name of the namespace begins
// the next one, the others are arguments. A trailing block goes to the last.
func expandSegments(segs []caddyfile.Segment) []caddyfile.Segment {
	var out []caddyfile.Segment
	for _, st := range segs {
		if len(st) >= 3 && st[1].Text == "{" && st[len(st)-1].Text == "}" {
			for _, line := range splitLines(st[2 : len(st)-1]) {
				out = append(out, splitModules(st[0], line)...)
			}

			continue
		}
		out = append(out, splitModules(st[0], st[1:])...)
	}

	return out
}

// splitLines cuts the body of a namespace block into directives: a token on a
// new line outside nested braces starts the next one.
func splitLines(body []caddyfile.Token) [][]caddyfile.Token {
	var lines [][]caddyfile.Token
	depth := 0
	for i, t := range body {
		if depth == 0 && (i == 0 || t.Line != body[i-1].Line) {
			lines = append(lines, nil)
		}
		switch t.Text {
		case "{":
			depth++
		case "}":
			depth--
		}
		lines[len(lines)-1] = append(lines[len(lines)-1], t)
	}

	return lines
}

// splitModules turns "<ns> a b c" into one directive per module name; a line
// that does not start with a module name stays as it is.
func splitModules(ns caddyfile.Token, rest []caddyfile.Token) []caddyfile.Segment {
	if len(rest) > 0 {
		// The Dispenser reads arguments from the namespace's own line.
		ns.Line = rest[0].Line
	}
	whole := append(caddyfile.Segment{ns}, rest...)
	words := rest
	var block []caddyfile.Token
	if i := slices.IndexFunc(rest, func(t caddyfile.Token) bool { return t.Text == "{" }); i >= 0 {
		words, block = rest[:i], rest[i:]
	}
	isModule := func(t caddyfile.Token) bool {
		_, ok := LookupModule(ns.Text + idSeparator + t.Text)

		return ok
	}
	if len(words) == 0 || !isModule(words[0]) {
		return []caddyfile.Segment{whole}
	}

	var out []caddyfile.Segment
	for _, w := range words {
		if isModule(w) || len(out) == 0 {
			out = append(out, caddyfile.Segment{ns})
		}
		last := &out[len(out)-1]
		*last = append(*last, w)
	}
	last := &out[len(out)-1]
	*last = append(*last, block...)

	return out
}
