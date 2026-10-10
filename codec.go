package aish

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// Options are the global settings every module can read through
// Host.Options.
type Options struct {
	MaxSteps int    // agent steps per user message; zero means the module default
	Resume   string // session ID to restore, empty for a new session
	Trace    bool
}

var decoders = struct {
	sync.RWMutex
	byKind map[string]eventDecoder
}{byKind: map[string]eventDecoder{}}

type eventDecoder struct {
	typ    reflect.Type
	decode func(json.RawMessage) (Event, error)
}

// RegisterEvent makes events of type E restorable by DecodeEvent. The owner of
// an event type registers it from init(); E must be JSON-decodable. It panics
// if the Kind is already taken by another Go type.
func RegisterEvent[E Event]() {
	var zero E
	kind, typ := zero.Kind(), reflect.TypeFor[E]()

	decoders.Lock()
	defer decoders.Unlock()

	if prev, ok := decoders.byKind[kind]; ok {
		if prev.typ == typ {
			return
		}
		panic(fmt.Sprintf("aish: event kind %q registered for both %s and %s", kind, prev.typ, typ))
	}
	decoders.byKind[kind] = eventDecoder{typ: typ, decode: func(raw json.RawMessage) (Event, error) {
		var e E
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("aish: decode %s: %w", kind, err)
		}

		return e, nil
	}}
}

// DecodeEvent decodes an event of the given kind. ok is false when no module
// registered the kind, for example one removed from the build.
func DecodeEvent(kind string, raw json.RawMessage) (ev Event, ok bool, err error) {
	decoders.RLock()
	d, ok := decoders.byKind[kind]
	decoders.RUnlock()
	if !ok {
		return nil, false, nil
	}
	ev, err = d.decode(raw)

	return ev, true, err
}
