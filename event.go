package aish

import (
	"context"
	"time"
)

// Event is any value that flows through the bus. Kind must return the same
// string for every value of the type: hooks subscribe by Kind, and one Kind
// maps to exactly one Go type within a session.
//
// Shared core types have no prefix ("tool.call"); module types start with
// the module ID ("pty.command").
type Event interface {
	Kind() string
}

type Meta struct {
	// ID is assigned on the first Emit and kept when the event is re-emitted
	// as an Envelope.
	ID string
	// Tx is the sequence number of the transaction the event belongs to.
	Tx   uint64
	Time time.Time
	// Source is the hook ("<instance>:<name>") or module instance that
	// emitted the event, or "core".
	Source string
}

// Envelope is an event together with its metadata.
//
// Emitting an Envelope re-emits its Event under the original Meta.ID. This
// is how a policy that removed a call puts it back after the user approved
// it: EmitWith(ctx, Below(1000), Envelope{Meta: m.Meta(), Event: call}).
type Envelope struct {
	Meta  Meta
	Event Event
}

func (e Envelope) Kind() string { return e.Event.Kind() }

// MetaOf returns the metadata of the event being delivered to the hook that
// owns ctx. Outside a hook it returns the zero Meta.
func MetaOf(ctx context.Context) Meta {
	if info := infoOf(ctx); info != nil {
		return info.meta
	}

	return Meta{}
}

const sourceCore = "core"

// sourceExternal is the Source of events emitted through Session.Emit.
const sourceExternal = "session.emit"
