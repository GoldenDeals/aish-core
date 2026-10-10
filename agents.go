package aish

import (
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"sync"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// Values holds one value per Go type. A module keeps its agent-level
// settings in it under its own settings type, so the core and the config
// loader never need to know what a setting means. The zero Values is empty.
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

// AgentSpec is one agent block of the config as modules see it. Global holds
// what the global block set, Own what the agent's block set; a module that
// owns a setting decides how the two combine.
type AgentSpec struct {
	Name   string
	Global Values
	Own    Values
}

// Scope tells a directive handler which block it is reading.
type Scope int

const (
	ScopeGlobal Scope = iota
	ScopeAgent
)

// DirectiveFunc reads one Agentfile directive that is not a module, such as
// "max_steps 50", into the values of the block. The Dispenser stands on the
// directive's first token.
type DirectiveFunc func(d *caddyfile.Dispenser, v *Values, scope Scope) error

var directives = struct {
	sync.RWMutex
	byName map[string]DirectiveFunc
}{byName: map[string]DirectiveFunc{}}

// RegisterDirective lets a module own an Agentfile directive that sets an
// agent option. It is meant to be called from init() and panics on an empty or
// duplicate name. A directive takes precedence over a module of the same
// namespace.
func RegisterDirective(name string, fn DirectiveFunc) {
	directives.Lock()
	defer directives.Unlock()

	if name == "" || fn == nil {
		panic("aish: RegisterDirective needs a name and a handler")
	}
	if _, dup := directives.byName[name]; dup {
		panic(fmt.Sprintf("aish: directive %q registered twice", name))
	}
	directives.byName[name] = fn
}

func LookupDirective(name string) (DirectiveFunc, bool) {
	directives.RLock()
	defer directives.RUnlock()

	fn, ok := directives.byName[name]

	return fn, ok
}

// Config is what a config loader produces: the agent blocks and a way to
// build fresh module instances of any of them.
type Config struct {
	// Agents are in config order.
	Agents []AgentSpec
	// Build creates new instances of the named agent's modules, in config
	// order. It is the SessionConfig.Spawner of every session.
	Build func(agent string) ([]Instance, error)
}

// ConfigLoader turns a config file into a Config. The Agentfile loader is
// config.agentfile; a binary links another one to read another format.
type ConfigLoader interface {
	Load(filename string, r io.Reader) (*Config, error)
}

var loaders = struct {
	sync.RWMutex
	byID map[string]func() ConfigLoader
}{byID: map[string]func() ConfigLoader{}}

// RegisterConfigLoader makes a loader available under id, "config.<name>".
// It is meant to be called from init() and panics on a duplicate.
func RegisterConfigLoader(id string, newLoader func() ConfigLoader) {
	loaders.Lock()
	defer loaders.Unlock()

	if _, dup := loaders.byID[id]; dup || newLoader == nil {
		panic(fmt.Sprintf("aish: invalid or duplicate config loader %q", id))
	}
	loaders.byID[id] = newLoader
}

func LookupConfigLoader(id string) (ConfigLoader, bool) {
	loaders.RLock()
	defer loaders.RUnlock()

	newLoader, ok := loaders.byID[id]
	if !ok {
		return nil, false
	}

	return newLoader(), true
}

// ConfigLoaders returns the IDs of the linked loaders, sorted.
func ConfigLoaders() []string {
	loaders.RLock()
	defer loaders.RUnlock()

	return slices.Sorted(maps.Keys(loaders.byID))
}
