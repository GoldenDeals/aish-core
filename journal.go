package aish

import (
	"sync"
	"time"
)

// TxStatus is the outcome of a transaction.
type TxStatus string

const (
	StatusCommitted TxStatus = "committed"
	// StatusCancelled transactions are still journaled with everything that
	// happened before the cancellation.
	StatusCancelled TxStatus = "cancelled"
)

// Tx is a committed transaction. It is immutable: readers must not modify
// its events.
type Tx struct {
	Seq    uint64
	Status TxStatus
	// Events are the final events in order of appearance. Removed events are
	// absent here but present in Trace.
	Events []Envelope
	Trace  []TraceEntry
}

// TraceAction says what happened to an event at one point of delivery.
type TraceAction string

const (
	TraceEmit      TraceAction = "emit"      // event entered the transaction
	TraceMutate    TraceAction = "mutate"    // Mutate hook received it and left it unchanged
	TraceChange    TraceAction = "change"    // Mutate hook changed it
	TraceRemove    TraceAction = "remove"    // Mutate hook removed it
	TraceHandle    TraceAction = "handle"    // Handle hook received it
	TraceDuplicate TraceAction = "duplicate" // not delivered: the hook already got this Meta.ID
	TraceUnhandled TraceAction = "unhandled" // reached no Mutate or Handle hook
	TraceCancelled TraceAction = "cancelled" // recorded without delivery: the transaction is cancelled
	TraceLimit     TraceAction = "limit"     // transaction cancelled by a limit
	TraceAbort     TraceAction = "abort"     // open stream closed with the cancellation error
	TraceWarn      TraceAction = "warn"      // suspected misuse, see Err
)

// TraceEntry is one step of event delivery inside a transaction. Entries are
// in the order the steps began: a hook call comes before the events it
// emitted, which have a greater Depth.
type TraceEntry struct {
	Event string // Meta.ID
	Kind  string
	// Hook is the hook ("<instance>:<name>") that got the event; for
	// TraceEmit it is Meta.Source, the emitter. Empty for core actions.
	Hook   string
	Action TraceAction
	Depth  int // nesting depth of the Emit, root events have depth 1
	Err    string
	Dur    time.Duration
}

// Journal is the append-only sequence of committed transactions of a
// session. Modules get read-only access to it.
type Journal struct {
	mu  sync.RWMutex
	txs []Tx
}

func (j *Journal) Len() int {
	j.mu.RLock()
	defer j.mu.RUnlock()

	return len(j.txs)
}

// Txs returns the transactions in commit order. The slice is a copy; the
// transactions are shared and must not be modified.
func (j *Journal) Txs() []Tx {
	j.mu.RLock()
	defer j.mu.RUnlock()

	return append([]Tx(nil), j.txs...)
}

func (j *Journal) Last() (Tx, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if len(j.txs) == 0 {
		return Tx{}, false
	}

	return j.txs[len(j.txs)-1], true
}

func (j *Journal) append(tx Tx) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.txs = append(j.txs, tx)
}
