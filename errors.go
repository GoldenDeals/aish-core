package aish

import "errors"

var (
	// ErrUnhandled means an emitted event reached no Mutate or Handle hook of
	// its kind. The event is still recorded in the transaction.
	ErrUnhandled = errors.New("aish: event reached no mutate or handle hook")

	// ErrWouldDeadlock is returned by Emit when ctx was not issued by the
	// core (for example context.Background() inside a hook). Such an Emit
	// would queue a new transaction behind the one the hook is running in
	// and wait for it forever.
	ErrWouldDeadlock = errors.New("aish: emit with a context not issued by the core would deadlock")

	// ErrStaleContext is returned by Emit when a hook context is used after
	// its hook returned or while a nested hook is running.
	ErrStaleContext = errors.New("aish: hook context used outside its running hook")

	// ErrConcurrentEmit is returned by Emit when the hook that owns ctx is
	// still inside an Emit: called from another goroutine, or by a nested
	// hook that captured the outer hook's ctx. Hooks of a session run one at
	// a time, so a hook emits one Emit at a time too.
	ErrConcurrentEmit = errors.New("aish: hook context is already emitting")

	// ErrLimit is the cause of a transaction cancelled because it exceeded the
	// nesting depth or the number of events.
	ErrLimit = errors.New("aish: transaction limit exceeded")

	// ErrNotStarted is returned by Emit with the Init context: modules emit
	// only once every module has subscribed.
	ErrNotStarted = errors.New("aish: emit before the session started")

	// ErrClosed is returned by Emit after the session was closed.
	ErrClosed = errors.New("aish: session closed")
)

// HookError is an error returned (or a panic raised) by a Mutate or Handle
// hook. The core records it as module.error and returns it from Emit.
type HookError struct {
	Hook string // "<instance>:<name>"
	Err  error
}

func (e *HookError) Error() string { return "aish: hook " + e.Hook + ": " + e.Err.Error() }

func (e *HookError) Unwrap() error { return e.Err }
