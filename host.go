package aish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
)

// Host is everything the core gives to one module instance. It is a concrete
// type because its subscription methods are generic.
type Host struct {
	sess     *session
	instance string
	log      *slog.Logger
	options  Options
}

// Instance returns the module instance name, for example "llm.anthropic/fast".
func (h *Host) Instance() string { return h.instance }

func (h *Host) Options() Options { return h.options }

func (h *Host) Session() SessionInfo { return h.sess.info }

// Log is the only place a module writes logs to: the terminal belongs to the
// frontend. Records carry session and instance; logged through a *Context
// method with a hook ctx they also carry tx, event and hook.
func (h *Host) Log() *slog.Logger { return h.log }

func (h *Host) Journal() *Journal { return &h.sess.journal }

// Seed appends restored transactions to the journal (§4.8). It runs no hooks
// and emits no tx.committed, so tools do not run again and a store does not
// write the transactions twice. It is allowed only in Init.
func (h *Host) Seed(txs ...Tx) error { return h.sess.seed(txs) }

// Cancel cancels the open transaction, if any (Ctrl+C). Its hooks see a
// cancelled ctx, and it commits with StatusCancelled and everything that
// happened before.
func (h *Host) Cancel() {
	h.sess.mu.Lock()
	tx := h.sess.cur
	h.sess.mu.Unlock()
	if tx != nil {
		tx.cancel(context.Canceled)
	}
}

// Mutate subscribes a hook that may change or remove events of type E
// before Handle hooks see them.
func (h *Host) Mutate[E Event](name string, fn func(context.Context, *Mut[E]) error, opts ...HookOpt) Sub {
	return h.subscribe[E](ModeMutate, name, opts, func(hk *hook) {
		hk.mutate = func(ctx context.Context, ev Event, meta Meta) (Event, bool, error) {
			e := as[E](ev)
			m := &Mut[E]{ev: &e, meta: meta}
			err := fn(ctx, m)

			return e, m.removed, err
		}
	})
}

// Handle subscribes a hook that gets a deep copy of the final event of type E
// and may emit new events.
func (h *Host) Handle[E Event](name string, fn func(context.Context, E) error, opts ...HookOpt) Sub {
	return h.subscribe[E](ModeHandle, name, opts, func(hk *hook) {
		hk.handle = func(ctx context.Context, ev Event) error { return fn(ctx, as[E](ev)) }
	})
}

// Observe subscribes a hook that gets a deep copy of every committed event of
// type E. With E = Event it gets events of all kinds. Emit from an Observe
// hook queues a new transaction and returns without waiting for it.
func (h *Host) Observe[E Event](name string, fn func(context.Context, E), opts ...HookOpt) Sub {
	return h.subscribe[E](ModeObserve, name, opts, func(hk *hook) {
		hk.observe = func(ctx context.Context, ev Event) { fn(ctx, as[E](ev)) }
	})
}

// Hooks lists the subscriptions that get events of type E, including
// Observe[Event] hooks. With E = Event it lists every subscription.
func (h *Host) Hooks[E Event]() []HookInfo {
	kind, all := kindOf[E]()

	return h.sess.hooks.infos(kind, all)
}

func (h *Host) Emit(ctx context.Context, evs ...Event) error {
	return h.EmitWith(ctx, EmitOpts{}, evs...)
}

// EmitWith emits events one after another. What happens depends on ctx:
//
//   - ctx of a running Mutate or Handle hook: the events join the open
//     transaction, and EmitWith returns after all their hooks ran;
//   - ctx of an Observe hook: a new transaction is queued and EmitWith
//     returns at once; errors are recorded in it as module.error;
//   - the Start context: a new transaction is queued and EmitWith waits
//     until it has committed and its Observe hooks have run;
//   - the Init context: ErrNotStarted;
//   - any other ctx: ErrWouldDeadlock.
//
// Errors (ErrUnhandled, *HookError, ErrLimit, cancellation) are joined with
// errors.Join.
func (h *Host) EmitWith(ctx context.Context, o EmitOpts, evs ...Event) error {
	info := infoOf(ctx)
	if info == nil || info.sess != h.sess {
		return ErrWouldDeadlock
	}
	if info.init {
		return ErrNotStarted
	}
	for _, ev := range evs {
		if err := h.sess.checkEmit(ev); err != nil {
			return err
		}
	}
	if len(evs) == 0 {
		return nil
	}

	source := h.instance
	if info.hook != nil {
		source = info.hook.info.ID
	}

	switch {
	case info.tx != nil:
		return info.tx.emitNested(info, source, o, evs)
	case info.observe:
		return h.sess.enqueue(ctx, &pending{events: slices.Clone(evs), opts: o, sender: info.hook, source: source})
	default:
		p := &pending{events: slices.Clone(evs), opts: o, source: source, done: make(chan error, 1)}

		return h.sess.enqueue(ctx, p)
	}
}

// checkEmit rejects events modules may not emit and binds the kind to the
// event's Go type (panicking on a conflict).
func (s *session) checkEmit(ev Event) error {
	if env, ok := ev.(Envelope); ok {
		ev = env.Event
	}
	if ev == nil {
		return errors.New("aish: emit of a nil event")
	}
	if _, ok := ev.(Envelope); ok {
		return errors.New("aish: emit of a nested Envelope")
	}
	if ev.Kind() == "" {
		return fmt.Errorf("aish: event type %T has an empty Kind", ev)
	}
	if allowedModes(ev) == ModeObserve {
		return fmt.Errorf("aish: %s events are emitted only by the core", ev.Kind())
	}
	s.hooks.checkType(ev.Kind(), reflect.TypeOf(ev))

	return nil
}
