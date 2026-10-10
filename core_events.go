package aish

// Events emitted only by the core. They support Observe hooks only, and
// modules cannot emit them.

// TxStarted is delivered when a transaction begins, before its root events.
// It is not written to the journal.
type TxStarted struct {
	Seq  uint64
	Root []Event // root events as passed to Emit, before any Mutate hook
}

// TxCommitted is delivered after a transaction is appended to the journal.
// It is not written to the journal.
type TxCommitted struct {
	Tx Tx
}

// ModuleError records a failed hook. It is a regular event of the
// transaction in which the hook failed.
type ModuleError struct {
	Instance string
	Hook     string // empty when the error is not tied to a hook
	Error    string
}

func (TxStarted) Kind() string   { return "tx.started" }
func (TxCommitted) Kind() string { return "tx.committed" }
func (ModuleError) Kind() string { return "module.error" }

func (TxStarted) allowedModes() Mode   { return ModeObserve }
func (TxCommitted) allowedModes() Mode { return ModeObserve }
func (ModuleError) allowedModes() Mode { return ModeObserve }
