package aish

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Mode is how a hook takes part in event delivery (§4.4).
type Mode uint8

const (
	// ModeMutate hooks run first, one after another, and may change or
	// remove the event.
	ModeMutate Mode = 1 << iota
	// ModeHandle hooks run after all Mutate hooks and get a deep copy of the
	// final event.
	ModeHandle
	// ModeObserve hooks run after commit, before the next transaction, and
	// get a deep copy of the frozen event.
	ModeObserve
)

func (m Mode) String() string {
	switch m {
	case ModeMutate:
		return "mutate"
	case ModeHandle:
		return "handle"
	case ModeObserve:
		return "observe"
	default:
		return fmt.Sprintf("Mode(%d)", uint8(m))
	}
}

// modeRestricted is implemented by core event types that support only some
// modes, and through an embedded Stream by stream events, which forbid Mutate.
type modeRestricted interface {
	allowedModes() Mode
}

// HookOpt is not generic, unlike in §4.2: Priority must apply to any E, and
// Go cannot infer E from the call site of a generic option constructor.
type HookOpt interface {
	applyHook(o *hookOpts)
}

type hookOpts struct {
	priority int
	where    any // func(E) bool
}

type priorityOpt int

func (p priorityOpt) applyHook(o *hookOpts) { o.priority = int(p) }

// DefaultPriority is the priority of hooks subscribed without Priority. It
// is the executor band; hooks below it run after executors and get calls
// that a policy recorded as denied (see Below).
const DefaultPriority = 100

// Priority sets the hook priority. Hooks run in descending priority; equal
// priorities run in order of hook ID. Conventional bands: 2000 description
// and normalization, 1000 policies, 500 enrichment, 100 executors (default),
// below 100 after executors (display, audit).
func Priority(n int) HookOpt { return priorityOpt(n) }

type whereOpt struct{ fn any }

func (w whereOpt) applyHook(o *hookOpts) { o.where = w.fn }

// Where filters events. An event the filter rejects is not delivered to the
// hook and does not count as handled by it. E must match the hook's type.
func Where[E Event](fn func(E) bool) HookOpt { return whereOpt{fn: fn} }

type HookInfo struct {
	// ID is "<instance>:<name>". It is unique within a session and is what
	// To and Except refer to.
	ID       string
	Instance string
	Name     string
	// Kind is the event kind, or empty for Observe[Event] (all kinds).
	Kind     string
	Mode     Mode
	Priority int
}

// Sub is a subscription. Remove may be called from any goroutine.
type Sub interface {
	Remove()
}

type Mut[E Event] struct {
	ev      *E
	meta    Meta
	removed bool
}

// Event returns a pointer to the working event. Changes are seen by later
// Mutate hooks, Handle hooks and the journal.
func (m *Mut[E]) Event() *E { return m.ev }

func (m *Mut[E]) Meta() Meta { return m.meta }

// Remove drops the event: no further hook gets it and it is not journaled.
func (m *Mut[E]) Remove() { m.removed = true }

func (m *Mut[E]) Removed() bool { return m.removed }

type hook struct {
	info    HookInfo
	where   func(Event) bool
	mutate  func(ctx context.Context, ev Event, meta Meta) (out Event, removed bool, err error)
	handle  func(ctx context.Context, ev Event) error
	observe func(ctx context.Context, ev Event)
	removed atomic.Bool
}

func (hk *hook) Remove() { hk.removed.Store(true) }

// compare orders hooks for delivery: descending priority, then hook ID.
func (hk *hook) compare(other *hook) int {
	return cmp.Or(cmp.Compare(other.info.Priority, hk.info.Priority), strings.Compare(hk.info.ID, other.info.ID))
}

// hookRegistry holds the subscriptions of one session.
type hookRegistry struct {
	mu     sync.RWMutex
	byKind map[string][]*hook
	all    []*hook // Observe[Event]
	ids    map[string]bool
	types  map[string]reflect.Type // Kind -> the one Go type of that kind
}

func (r *hookRegistry) add(hk *hook) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ids == nil {
		r.ids = map[string]bool{}
		r.byKind = map[string][]*hook{}
	}
	if r.ids[hk.info.ID] {
		panic(fmt.Sprintf("aish: hook %s registered twice", hk.info.ID))
	}
	r.ids[hk.info.ID] = true

	if hk.info.Kind == "" {
		r.all = append(r.all, hk)
	} else {
		r.byKind[hk.info.Kind] = append(r.byKind[hk.info.Kind], hk)
	}
}

// checkType panics if kind is already bound to a different Go type.
func (r *hookRegistry) checkType(kind string, t reflect.Type) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.types == nil {
		r.types = map[string]reflect.Type{}
	}
	if prev, ok := r.types[kind]; ok && prev != t {
		panic(fmt.Sprintf("aish: kind %q is used by two Go types: %v and %v", kind, prev, t))
	}
	r.types[kind] = t
}

// snapshot returns the live hooks of a mode for a kind in delivery order.
// Observe snapshots include Observe[Event] hooks.
func (r *hookRegistry) snapshot(kind string, mode Mode) []*hook {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*hook
	for _, hk := range r.byKind[kind] {
		if hk.info.Mode == mode && !hk.removed.Load() {
			out = append(out, hk)
		}
	}
	if mode == ModeObserve {
		for _, hk := range r.all {
			if !hk.removed.Load() {
				out = append(out, hk)
			}
		}
	}
	slices.SortFunc(out, (*hook).compare)

	return out
}

// infos lists live hooks for a kind (including Observe[Event] hooks), or
// every live hook when all is set, ordered by kind, mode and delivery order.
func (r *hookRegistry) infos(kind string, all bool) []HookInfo {
	r.mu.RLock()
	var hooks []*hook
	if all {
		for _, hs := range r.byKind {
			hooks = append(hooks, hs...)
		}
	} else {
		hooks = append(hooks, r.byKind[kind]...)
	}
	hooks = append(hooks, r.all...)
	r.mu.RUnlock()

	hooks = slices.DeleteFunc(hooks, func(hk *hook) bool { return hk.removed.Load() })
	slices.SortFunc(hooks, func(a, b *hook) int {
		return cmp.Or(strings.Compare(a.info.Kind, b.info.Kind), cmp.Compare(a.info.Mode, b.info.Mode), a.compare(b))
	})

	out := make([]HookInfo, len(hooks))
	for i, hk := range hooks {
		out[i] = hk.info
	}

	return out
}

var eventType = reflect.TypeFor[Event]()

// kindOf returns the kind of E, or all=true when E is the Event interface.
func kindOf[E Event]() (kind string, all bool) {
	t := reflect.TypeFor[E]()
	if t == eventType {
		return "", true
	}
	if t.Kind() == reflect.Interface {
		panic(fmt.Sprintf("aish: cannot subscribe to interface type %v; use a concrete event type or aish.Event", t))
	}

	return zeroEvent[E]().Kind(), false
}

// zeroEvent returns a zero E that is safe to call methods on: pointer types
// get a pointer to a zero value instead of nil.
func zeroEvent[E Event]() E {
	var e E
	if t := reflect.TypeFor[E](); t.Kind() == reflect.Pointer {
		e, _ = reflect.New(t.Elem()).Interface().(E)
	}

	return e
}

// as converts an event delivered to a hook of type E. The registry binds
// every kind to one Go type, so a mismatch is a core bug.
func as[E Event](ev Event) E {
	e, ok := ev.(E)
	if !ok {
		panic(fmt.Sprintf("aish: internal error: event %T delivered to hook of %v", ev, reflect.TypeFor[E]()))
	}

	return e
}

func allowedModes(ev Event) Mode {
	if r, ok := ev.(modeRestricted); ok {
		return r.allowedModes()
	}

	return ModeMutate | ModeHandle | ModeObserve
}

// subscribe builds and registers a hook of type E. bind fills in the
// mode-specific function.
func (h *Host) subscribe[E Event](mode Mode, name string, opts []HookOpt, bind func(hk *hook)) Sub {
	if name == "" || strings.ContainsAny(name, ": \t\n") {
		panic(fmt.Sprintf("aish: invalid hook name %q", name))
	}

	kind, all := kindOf[E]()
	if all && mode != ModeObserve {
		panic(fmt.Sprintf("aish: hook %s: only Observe hooks may subscribe to all events", name))
	}
	if !all {
		if allowedModes(zeroEvent[E]())&mode == 0 {
			panic(fmt.Sprintf("aish: hook %s: kind %s does not support %s hooks", name, kind, mode))
		}
		h.sess.hooks.checkType(kind, reflect.TypeFor[E]())
	}

	o := hookOpts{priority: DefaultPriority}
	for _, opt := range opts {
		opt.applyHook(&o)
	}

	hk := &hook{info: HookInfo{
		ID:       h.instance + ":" + name,
		Instance: h.instance,
		Name:     name,
		Kind:     kind,
		Mode:     mode,
		Priority: o.priority,
	}}
	if o.where != nil {
		fn, ok := o.where.(func(E) bool)
		if !ok {
			panic(fmt.Sprintf("aish: hook %s: Where filter is %T, want func(%v) bool", hk.info.ID, o.where, reflect.TypeFor[E]()))
		}
		hk.where = func(ev Event) bool { return fn(as[E](ev)) }
	}
	bind(hk)
	h.sess.hooks.add(hk)

	return hk
}
