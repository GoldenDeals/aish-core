package aish

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// Options are the settings of the run itself that every module can read
// through Host.Options. Settings of an agent belong to AgentSpec.
type Options struct {
	Resume string // session ID to restore, empty for a new session
}

func init() {
	RegisterEvent[SessionStarted]()
	RegisterEvent[ModuleError]()
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
	typ := reflect.TypeFor[E]()
	var zero E
	if typ.Kind() == reflect.Pointer {
		// Kind may have a value receiver, which a nil pointer cannot call.
		if v, ok := reflect.New(typ.Elem()).Interface().(E); ok {
			zero = v
		}
	}
	kind := zero.Kind()

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
