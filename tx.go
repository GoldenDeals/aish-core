package aish

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/GoldenDeals/aish-core/internal/deepcopy"
)

var (
	errTxDone    = errors.New("aish: transaction finished")
	errCancelled = errors.New("aish: transaction cancelled")
)

// txState is the open transaction. Everything except stack, trace, closed
// and frame.emitting is touched only by the goroutine running hooks;
// frame.emitting keeps it that way when a hook emits from several goroutines.
type txState struct {
	s      *session
	seq    uint64
	ctx    context.Context
	cancel context.CancelCauseFunc

	records []*record
	emitted int // events delivered so far, re-emits included; module.error is not counted
	seen    map[seenKey]struct{}
	lastID  int

	mu     sync.Mutex
	stack  []*frame // running hook invocations, innermost last
	trace  []TraceEntry
	closed bool
}

type frame struct {
	hook     *hook
	depth    int
	emitting bool // guarded by txState.mu
}

type record struct {
	env     Envelope
	removed bool
}

type seenKey struct {
	hook *hook
	id   string
}

func (tx *txState) newID() string {
	tx.lastID++

	return fmt.Sprintf("%d.%d", tx.seq, tx.lastID)
}

// addTrace appends an entry and returns its index for finishTrace.
func (tx *txState) addTrace(e TraceEntry) int {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	tx.trace = append(tx.trace, e)

	return len(tx.trace) - 1
}

// finishTrace completes the entry of a hook call. The entry is added when
// the hook starts, so the events it emits follow it in the trace.
func (tx *txState) finishTrace(i int, action TraceAction, err error, dur time.Duration) {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	if i >= len(tx.trace) {
		return
	}
	tx.trace[i].Action, tx.trace[i].Err, tx.trace[i].Dur = action, errText(err), dur
}

func (tx *txState) emitNested(info *ctxInfo, source string, opts EmitOpts, evs []Event) error {
	if err := tx.claim(info.frame); err != nil {
		return err
	}
	defer func() {
		tx.mu.Lock()
		info.frame.emitting = false
		tx.mu.Unlock()
	}()

	errs := make([]error, 0, len(evs))
	for _, ev := range evs {
		errs = append(errs, tx.deliver(ev, opts, info.hook, source, info.frame.depth+1))
	}

	return errors.Join(errs...)
}

// claim lets the hook that owns f emit, one Emit at a time.
func (tx *txState) claim(f *frame) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	switch {
	case tx.closed:
		return ErrStaleContext
	case f.emitting:
		return ErrConcurrentEmit
	case len(tx.stack) == 0 || tx.stack[len(tx.stack)-1] != f:
		return ErrStaleContext
	}
	f.emitting = true

	return nil
}

func (tx *txState) deliver(ev Event, opts EmitOpts, sender *hook, source string, depth int) error {
	meta := Meta{Tx: tx.seq, Time: tx.s.now(), Source: source}
	if env, ok := ev.(Envelope); ok {
		meta.ID, ev = env.Meta.ID, env.Event
	}
	if meta.ID == "" {
		meta.ID = tx.newID()
	}
	kind := ev.Kind()

	switch {
	case tx.emitted >= maxEvents:
		return tx.limit(meta.ID, kind, depth, fmt.Errorf("%w: more than %d events", ErrLimit, maxEvents))
	case depth > maxDepth:
		return tx.limit(meta.ID, kind, depth, fmt.Errorf("%w: emit nested deeper than %d", ErrLimit, maxDepth))
	}

	// The journal must not share memory with the emitter.
	rec := &record{env: Envelope{Meta: meta, Event: deepcopy.Copy(ev)}}
	tx.records = append(tx.records, rec)
	tx.emitted++
	if err := tx.cancelled(); err != nil {
		tx.addTrace(TraceEntry{Event: meta.ID, Kind: kind, Action: TraceCancelled, Depth: depth})

		return err
	}
	tx.addTrace(TraceEntry{Event: meta.ID, Kind: kind, Hook: source, Action: TraceEmit, Depth: depth})

	var errs []error
	handled := false

	for _, hk := range tx.s.hooks.snapshot(kind, ModeMutate) {
		if tx.ctx.Err() != nil {
			break
		}
		ok, err := tx.admit(hk, rec, rec.env.Event, opts, sender, depth)
		errs = append(errs, err)
		if !ok {
			continue
		}
		handled = true
		errs = append(errs, tx.runMutate(hk, rec, depth))
		if rec.removed {
			break
		}
	}

	if !rec.removed {
		for _, hk := range tx.s.hooks.snapshot(kind, ModeHandle) {
			if tx.ctx.Err() != nil {
				break
			}
			ok, err := tx.admit(hk, rec, rec.env.Event, opts, sender, depth)
			errs = append(errs, err)
			if !ok {
				continue
			}
			c := deepcopy.Copy(rec.env.Event)
			handled = true
			errs = append(errs, tx.runHandle(hk, rec, c, depth))
		}
	}

	switch err := tx.cancelled(); {
	case err != nil:
		errs = append(errs, err)
	case !handled && allowedModes(ev) != ModeObserve: // core events have no Mutate or Handle hooks by design
		tx.addTrace(TraceEntry{Event: meta.ID, Kind: kind, Action: TraceUnhandled, Depth: depth})
		errs = append(errs, fmt.Errorf("%w: %s", ErrUnhandled, kind))
	}

	return errors.Join(errs...)
}

// admit decides whether hk gets the event: emit options, the hook's filter
// and the duplicate delivery guard.
func (tx *txState) admit(hk *hook, rec *record, ev Event, opts EmitOpts, sender *hook, depth int) (bool, error) {
	if !opts.admits(hk, sender) || hk.removed.Load() {
		return false, nil
	}
	if hk.where != nil {
		var pass bool
		if err := safeCall(func() error { pass = hk.where(ev); return nil }); err != nil {
			return false, tx.hookFailed(hk, fmt.Errorf("where: %w", err))
		}
		if !pass {
			return false, nil
		}
	}

	key := seenKey{hook: hk, id: rec.env.Meta.ID}
	if _, dup := tx.seen[key]; dup {
		kind := rec.env.Event.Kind()
		tx.addTrace(TraceEntry{Event: key.id, Kind: kind, Hook: hk.info.ID, Action: TraceDuplicate, Depth: depth})
		tx.moduleError(hk, fmt.Sprintf("event %s (%s) delivered to the hook twice in one transaction; dropped", key.id, kind))

		return false, nil
	}
	tx.seen[key] = struct{}{}

	return true, nil
}

func (tx *txState) runMutate(hk *hook, rec *record, depth int) error {
	before := deepcopy.Copy(rec.env.Event)
	var removed bool
	entry := tx.addTrace(TraceEntry{
		Event: rec.env.Meta.ID, Kind: rec.env.Event.Kind(), Hook: hk.info.ID, Action: TraceMutate, Depth: depth,
	})
	dur, err := tx.invoke(hk, rec.env.Meta, depth, func(ctx context.Context) error {
		out, rm, err := hk.mutate(ctx, rec.env.Event, rec.env.Meta)
		rec.env.Event, removed = out, rm

		return err
	})

	action := TraceMutate
	switch {
	case removed:
		action = TraceRemove
	case !deepcopy.Equal(before, rec.env.Event):
		action = TraceChange
	}
	tx.finishTrace(entry, action, err, dur)
	rec.removed = removed
	if err != nil {
		return tx.hookFailed(hk, err)
	}

	return nil
}

func (tx *txState) runHandle(hk *hook, rec *record, ev Event, depth int) error {
	entry := tx.addTrace(TraceEntry{
		Event: rec.env.Meta.ID, Kind: rec.env.Event.Kind(), Hook: hk.info.ID, Action: TraceHandle, Depth: depth,
	})
	dur, err := tx.invoke(hk, rec.env.Meta, depth, func(ctx context.Context) error {
		return hk.handle(ctx, ev)
	})
	tx.finishTrace(entry, TraceHandle, err, dur)
	if err != nil {
		return tx.hookFailed(hk, err)
	}

	return nil
}

// invoke runs a hook with its own ctx and keeps the stack of running hooks
// that nested Emit and the deadlock warning look at.
func (tx *txState) invoke(hk *hook, meta Meta, depth int, fn func(ctx context.Context) error) (time.Duration, error) {
	f := &frame{hook: hk, depth: depth}
	tx.mu.Lock()
	tx.stack = append(tx.stack, f)
	tx.mu.Unlock()
	defer func() {
		tx.mu.Lock()
		tx.stack = tx.stack[:len(tx.stack)-1]
		tx.mu.Unlock()
	}()

	ctx := context.WithValue(tx.ctx, ctxKey{}, &ctxInfo{sess: tx.s, tx: tx, frame: f, hook: hk, meta: meta})
	start := time.Now()
	err := safeCall(func() error { return fn(ctx) })

	return time.Since(start), err
}

func (tx *txState) hookFailed(hk *hook, err error) error {
	tx.moduleError(hk, err.Error())

	return &HookError{Hook: hk.info.ID, Err: err}
}

// moduleError records a module.error event. It is not delivered to Mutate or
// Handle hooks and does not count towards the event limit.
func (tx *txState) moduleError(hk *hook, msg string) {
	ev := ModuleError{Error: msg}
	if hk != nil {
		ev.Instance, ev.Hook = hk.info.Instance, hk.info.ID
	}
	tx.appendCoreEvent(ev)
}

func (tx *txState) appendCoreEvent(ev Event) {
	meta := Meta{ID: tx.newID(), Tx: tx.seq, Time: tx.s.now(), Source: sourceCore}
	tx.records = append(tx.records, &record{env: Envelope{Meta: meta, Event: ev}})
}

// recordUnreturned records errors of a transaction queued from an Observe
// hook: nobody waits for its Emit. Hook errors are already module.error, and
// cancellation is the transaction status.
func (tx *txState) recordUnreturned(source string, err error) {
	for _, leaf := range errLeaves(err) {
		_, isHook := errors.AsType[*HookError](leaf)
		if !isHook && !errors.Is(leaf, errCancelled) {
			tx.appendCoreEvent(ModuleError{Instance: source, Error: leaf.Error()})
		}
	}
}

func (tx *txState) limit(id, kind string, depth int, err error) error {
	tx.addTrace(TraceEntry{Event: id, Kind: kind, Action: TraceLimit, Depth: depth, Err: err.Error()})
	tx.cancel(err)

	return err
}

func (tx *txState) cancelled() error {
	if tx.ctx.Err() == nil {
		return nil
	}

	return fmt.Errorf("%w: %w", errCancelled, context.Cause(tx.ctx))
}

// awaitStreams blocks until every stream of the journaled events is closed.
// If the transaction is cancelled first, it closes the open streams with the
// cancellation error and returns that error. The cancellation may come from
// the stream timeout.
func (tx *txState) awaitStreams() error {
	if d := tx.s.streamTimeout; d > 0 {
		stall := time.AfterFunc(d, func() { tx.cancel(fmt.Errorf("%w within %s", ErrStreamStalled, d)) })
		defer stall.Stop()
	}

	var aborted bool
	for _, rec := range tx.records {
		if rec.removed {
			continue
		}
		for _, b := range streamsIn(rec.env.Event) {
			select {
			case <-b.finished():
				continue
			case <-tx.ctx.Done():
			}
			select {
			case <-b.finished():
				continue
			default:
			}
			err := tx.cancelled()
			b.abort(err)
			aborted = true
			tx.addTrace(TraceEntry{
				Event: rec.env.Meta.ID, Kind: rec.env.Event.Kind(), Action: TraceAbort, Err: err.Error(),
			})
		}
	}
	if aborted {
		return tx.cancelled()
	}

	return nil
}

func (tx *txState) commit() Tx {
	status := StatusCommitted
	if tx.ctx.Err() != nil {
		status = StatusCancelled
	}

	events := make([]Envelope, 0, len(tx.records))
	for _, rec := range tx.records {
		if !rec.removed {
			events = append(events, rec.env)
		}
	}

	tx.mu.Lock()
	tx.closed = true
	committed := Tx{Seq: tx.seq, Status: status, Events: events, Trace: slices.Clone(tx.trace)}
	tx.mu.Unlock()

	tx.s.journal.append(committed)
	tx.s.mu.Lock()
	tx.s.cur = nil
	tx.s.mu.Unlock()
	tx.cancel(errTxDone)

	return committed
}

func errLeaves(err error) []error {
	if err == nil {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var out []error
	for _, e := range joined.Unwrap() {
		out = append(out, errLeaves(e)...)
	}

	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
