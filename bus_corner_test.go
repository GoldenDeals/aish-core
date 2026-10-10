package aish

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// Corner cases of hooks, Emit and transactions.

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	fn()
}

func TestHookFailureIsRecordedAndDeliveryContinues(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	boom := errors.New("boom")
	laterRan := false
	h.Mutate("fails", func(context.Context, *Mut[call]) error { return boom })
	h.Handle("panics", func(context.Context, call) error { panic("kaboom") })
	h.Handle("later", func(context.Context, call) error { laterRan = true; return nil }, Priority(0))

	err := h.Emit(ctx, call{})
	var he *HookError
	if !errors.Is(err, boom) || !errors.As(err, &he) || he.Hook != "m:fails" {
		t.Errorf("Emit = %v, want a *HookError of m:fails wrapping boom", err)
	}
	if err == nil || !strings.Contains(err.Error(), "panic: kaboom") {
		t.Errorf("Emit = %v, want the panic as an error", err)
	}
	if errors.Is(err, ErrUnhandled) {
		t.Error("failed hooks still count as delivery; got ErrUnhandled")
	}
	if !laterRan {
		t.Error("a hook after the failing ones did not run")
	}
	if got := s.lastTx(t).kinds(); !slices.Equal(got, []string{"test.call", "module.error", "module.error"}) {
		t.Errorf("events = %v, want the call and one module.error per failure", got)
	}
}

func TestWhereFilter(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	h.Handle("odd", func(context.Context, note) error { return nil }, Where(func(n note) bool { return n.N%2 == 1 }))
	h.Handle("broken", func(context.Context, note) error { return nil }, Where(func(n note) bool {
		if n.N == 3 {
			panic("bad filter")
		}

		return false
	}))

	if err := h.Emit(ctx, note{N: 1}); err != nil {
		t.Errorf("Emit(odd) = %v", err)
	}
	if err := h.Emit(ctx, note{N: 2}); !errors.Is(err, ErrUnhandled) {
		t.Errorf("Emit(even) = %v; an event no filter passes is unhandled", err)
	}
	err := h.Emit(ctx, note{N: 3}) // odd passes, broken panics
	var he *HookError
	if !errors.As(err, &he) || he.Hook != "m:broken" || errors.Is(err, ErrUnhandled) {
		t.Errorf("Emit with a panicking filter = %v, want a *HookError of m:broken only", err)
	}
}

func TestRemovedHooks(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var got []string
	var second, once Sub
	h.Handle("first", func(context.Context, note) error {
		got = append(got, "first")
		second.Remove() // removed while the event is being delivered

		return nil
	}, Priority(200))
	second = h.Handle("second", func(context.Context, note) error { got = append(got, "second"); return nil })
	once = h.Handle("once", func(context.Context, note) error {
		got = append(got, "once")
		once.Remove()

		return nil
	}, Priority(150))

	for range 2 {
		if err := h.Emit(ctx, note{}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(got, []string{"first", "once", "first"}) {
		t.Errorf("calls = %v, want removed hooks skipped at once", got)
	}
	for _, info := range h.Hooks[note]() {
		if info.Name != "first" {
			t.Errorf("Hooks lists removed hook %s", info.ID)
		}
	}
	second.Remove() // removing twice is harmless
}

func TestHookSubscribedDuringDeliveryGetsLaterEvents(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	late := 0
	subscribed := false
	h.Handle("a", func(context.Context, note) error {
		if !subscribed {
			subscribed = true
			h.Handle("b", func(context.Context, note) error { late++; return nil })
		}

		return nil
	}, Priority(200))

	for range 2 {
		if err := h.Emit(ctx, note{}); err != nil {
			t.Fatal(err)
		}
	}
	if late != 1 {
		t.Errorf("hook subscribed during delivery got %d events, want only the later one", late)
	}
}

func TestEmitOptsSelectHooks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts EmitOpts
		want []string
	}{
		{name: "all", opts: EmitOpts{}, want: []string{"p", "a", "b", "z"}},
		{name: "to", opts: To("m:a", "m:z"), want: []string{"a", "z"}},
		{name: "except", opts: Except("m:b", "m:p"), want: []string{"a", "z"}},
		{name: "below and except", opts: Below(500).Except("m:z"), want: []string{"a", "b"}},
		{name: "to and below", opts: To("m:p", "m:z").Below(100), want: []string{"z"}},
		{name: "below lowest", opts: Below(-1000), want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := newTestSession(t)
			h := s.host("m")

			var got []string
			observed := false
			record := func(name string) func(context.Context, note) error {
				return func(context.Context, note) error { got = append(got, name); return nil }
			}
			h.Mutate("p", func(context.Context, *Mut[note]) error { got = append(got, "p"); return nil }, Priority(500))
			h.Handle("a", record("a"))
			h.Handle("b", record("b"))
			h.Handle("z", record("z"), Priority(0))
			h.Observe("obs", func(context.Context, note) { observed = true })

			err := h.EmitWith(ctx, tc.opts, note{})
			if !slices.Equal(got, tc.want) {
				t.Errorf("delivered to %v, want %v", got, tc.want)
			}
			if (len(tc.want) == 0) != errors.Is(err, ErrUnhandled) {
				t.Errorf("Emit = %v", err)
			}
			if !observed {
				t.Error("EmitOpts kept the event from Observe hooks")
			}
		})
	}
}

func TestEmitNothing(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")
	started := false
	h.Observe("started", func(context.Context, TxStarted) { started = true })

	if err := h.Emit(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.close(context.Background())
	if started || s.journal.Len() != 0 {
		t.Error("an empty Emit opened a transaction")
	}
}

func TestEmitRejectsInvalidEvents(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	for name, ev := range map[string]Event{
		"nil":                    nil,
		"envelope without event": Envelope{},
		"nested envelope":        Envelope{Event: Envelope{Event: note{}}},
		"module.error":           ModuleError{},
		"wrapped module.error":   Envelope{Event: ModuleError{}},
		"tx.started":             TxStarted{},
		"tx.committed":           TxCommitted{},
		"session.started":        SessionStarted{},
		"empty kind":             emptyKind{},
	} {
		if err := h.Emit(ctx, note{}, ev); err == nil {
			t.Errorf("Emit of %s succeeded", name)
		}
	}
	_ = s.close(context.Background())
	if n := s.journal.Len(); n != 0 {
		t.Errorf("journal has %d transactions; a rejected Emit must not queue its valid events either", n)
	}
}

type emptyKind struct{}

func (emptyKind) Kind() string { return "" }

func TestEmitWithAnotherSessionsContext(t *testing.T) {
	t.Parallel()
	s, _ := newTestSession(t)
	_, otherCtx := newTestSession(t)

	if err := s.host("m").Emit(otherCtx, note{}); !errors.Is(err, ErrWouldDeadlock) {
		t.Errorf("Emit with a ctx of another session = %v, want ErrWouldDeadlock", err)
	}
}

func TestEnvelopeIDs(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")
	h.Handle("h", func(context.Context, note) error { return nil })

	if err := h.Emit(ctx, Envelope{Event: note{}}, Envelope{Meta: Meta{ID: "7.3", Tx: 7}, Event: note{}}); err != nil {
		t.Fatal(err)
	}
	tx := s.lastTx(t)
	if tx.Events[0].Meta.ID != "1.1" || tx.Events[1].Meta.ID != "7.3" || tx.Events[1].Meta.Tx != 1 {
		t.Errorf("metas = %+v, %+v; want a new ID for an empty Meta, the given ID kept and Tx reset",
			tx.Events[0].Meta, tx.Events[1].Meta)
	}
}

type otherEvent interface {
	Event
	Other()
}

func TestInvalidSubscriptionsPanic(t *testing.T) {
	t.Parallel()
	s, _ := newTestSession(t)
	h := s.host("m")
	nop := func(context.Context, note) error { return nil }
	h.Handle("taken", nop)

	for _, name := range []string{"", "a:b", "a b", "tab\tname", "taken"} {
		mustPanic(t, "hook name "+name, func() { h.Handle(name, nop) })
	}
	mustPanic(t, "Where of another type", func() {
		h.Handle("where", nop, Where(func(call) bool { return true }))
	})
	mustPanic(t, "Handle on all events", func() { h.Handle("all", func(context.Context, Event) error { return nil }) })
	mustPanic(t, "Mutate on all events", func() { h.Mutate("all", func(context.Context, *Mut[Event]) error { return nil }) })
	mustPanic(t, "subscription to Envelope", func() { h.Observe("env", func(context.Context, Envelope) {}) })
	mustPanic(t, "subscription to another interface", func() { h.Observe("iface", func(context.Context, otherEvent) {}) })
	mustPanic(t, "empty kind", func() { h.Observe("empty", func(context.Context, emptyKind) {}) })
	mustPanic(t, "Mutate on module.error", func() { h.Mutate("me", func(context.Context, *Mut[ModuleError]) error { return nil }) })
	mustPanic(t, "Handle on session.started", func() { h.Handle("ss", func(context.Context, SessionStarted) error { return nil }) })
}

func TestKindConflictOnEmitPanics(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")
	h.Handle("h", func(context.Context, note) error { return nil })

	mustPanic(t, "Emit of a second Go type of test.note", func() { _ = h.Emit(ctx, otherNote{}) })
}

type ptrEvent struct{ Items []int }

func (*ptrEvent) Kind() string { return "test.ptr" }

func TestPointerEventsAreCopiedForHandle(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	h.Handle("vandal", func(_ context.Context, ev *ptrEvent) error {
		ev.Items[0] = 99

		return nil
	})
	emitted := &ptrEvent{Items: []int{1}}
	if err := h.Emit(ctx, emitted); err != nil {
		t.Fatal(err)
	}
	if emitted.Items[0] != 1 {
		t.Error("a Handle hook changed the emitted pointer event")
	}
	if got := s.lastTx(t).Events[0].Event.(*ptrEvent).Items[0]; got != 1 {
		t.Errorf("journaled item = %d, want 1", got)
	}
}

func TestMutateMayReplaceTheWholeEvent(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	h.Mutate("swap", func(_ context.Context, m *Mut[call]) error {
		*m.Event() = call{Name: "replaced", Tags: []string{"x"}}

		return nil
	})
	var seen call
	h.Handle("h", func(_ context.Context, c call) error { seen = c; return nil })

	if err := h.Emit(ctx, call{Name: "orig"}); err != nil {
		t.Fatal(err)
	}
	if seen.Name != "replaced" || s.lastTx(t).Events[0].Event.(call).Name != "replaced" {
		t.Errorf("Handle saw %+v; want the replacement everywhere", seen)
	}
}

func TestMutateErrorAfterRemove(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	boom := errors.New("boom")
	h.Mutate("drop", func(_ context.Context, m *Mut[call]) error {
		m.Remove()

		return boom
	})
	handled := false
	h.Handle("h", func(context.Context, call) error { handled = true; return nil })

	err := h.Emit(ctx, call{})
	if !errors.Is(err, boom) || errors.Is(err, ErrUnhandled) {
		t.Errorf("Emit = %v, want only the hook error", err)
	}
	if handled {
		t.Error("a removed event reached Handle")
	}
	if got := s.lastTx(t).kinds(); !slices.Equal(got, []string{"module.error"}) {
		t.Errorf("events = %v, want only module.error", got)
	}
}

func TestEventsOfOneEmitRunInOrder(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var got []int
	h.Handle("h", func(_ context.Context, n note) error { got = append(got, n.N); return nil })

	err := h.Emit(ctx, note{N: 1}, result{}, note{N: 2}, result{})
	if !slices.Equal(got, []int{1, 2}) {
		t.Errorf("handled %v, want [1 2] in order", got)
	}
	if !errors.Is(err, ErrUnhandled) || strings.Count(err.Error(), "test.result") != 2 {
		t.Errorf("Emit = %v, want ErrUnhandled for each result", err)
	}
	if kinds := s.lastTx(t).kinds(); len(kinds) != 4 {
		t.Errorf("events = %v, want all four in one transaction", kinds)
	}
}

func TestDepthLimitBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		deepest int
		limited bool
	}{{maxDepth, false}, {maxDepth + 1, true}} {
		s, ctx := newTestSession(t)
		h := s.host("m")
		// Two hooks take turns so that no hook gets its own event; note N is
		// emitted at depth N.
		forward := func(ctx context.Context, n note) error {
			if n.N < tc.deepest {
				return h.Emit(ctx, note{N: n.N + 1})
			}

			return nil
		}
		h.Handle("odd", forward, Where(func(n note) bool { return n.N%2 == 1 }))
		h.Handle("even", forward, Where(func(n note) bool { return n.N%2 == 0 }))

		err := h.Emit(ctx, note{N: 1})
		tx := s.lastTx(t)
		if errors.Is(err, ErrLimit) != tc.limited || (tx.Status == StatusCancelled) != tc.limited {
			t.Errorf("depth %d: Emit = %v, status %s; limited=%v expected", tc.deepest, err, tx.Status, tc.limited)
		}
	}
}

// TestEventLimitBoundary: maxEvents emitted events fit; module.error events
// the core adds do not count.
func TestEventLimitBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		total   int
		limited bool
	}{{maxEvents, false}, {maxEvents + 1, true}} {
		s, ctx := newTestSession(t)
		h := s.host("m")
		h.Handle("flood", func(ctx context.Context, n note) error {
			if n.N != 0 {
				return nil
			}
			for range tc.total - 1 {
				if err := h.Emit(ctx, note{N: 1}); err != nil {
					return err
				}
			}

			return nil
		})
		h.Handle("sink", func(_ context.Context, n note) error {
			if n.N == 0 {
				return errors.New("adds a module.error")
			}

			return nil
		}, Priority(200))

		err := h.Emit(ctx, note{})
		if errors.Is(err, ErrLimit) != tc.limited || (s.lastTx(t).Status == StatusCancelled) != tc.limited {
			t.Errorf("%d events: Emit = %v; limited=%v expected", tc.total, err, tc.limited)
		}
	}
}

func TestCancelOnlyAffectsTheOpenTransaction(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")
	h.Cancel() // nothing open: no-op

	h.Handle("h", func(ctx context.Context, n note) error {
		if n.N == 1 {
			h.Cancel()
			<-ctx.Done()
		}

		return nil
	})
	if err := h.Emit(ctx, note{N: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Emit = %v, want context.Canceled", err)
	}
	if err := h.Emit(ctx, note{N: 2}); err != nil {
		t.Fatalf("next Emit = %v", err)
	}
	txs := s.journal.Txs()
	if txs[0].Status != StatusCancelled || txs[1].Status != StatusCommitted {
		t.Errorf("statuses = %s, %s; want cancelled, committed", txs[0].Status, txs[1].Status)
	}
}

// TestEmitIntoCancelledTransaction: what a hook emits after Ctrl+C is still
// journaled, but reaches no hook.
func TestEmitIntoCancelledTransaction(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var lateErr error
	resultHandled := false
	h.Handle("h", func(ctx context.Context, _ note) error {
		h.Cancel()
		lateErr = h.Emit(ctx, result{Text: "written anyway"})

		return nil
	})
	h.Handle("r", func(context.Context, result) error { resultHandled = true; return nil })

	if err := h.Emit(ctx, note{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Emit = %v, want context.Canceled", err)
	}
	if !errors.Is(lateErr, context.Canceled) || resultHandled {
		t.Errorf("late Emit = %v, handled=%v; want the cancellation and no delivery", lateErr, resultHandled)
	}
	tx := s.lastTx(t)
	if got := tx.kinds(); !slices.Equal(got, []string{"test.note", "test.result"}) {
		t.Errorf("events = %v, want the late result journaled", got)
	}
	if !slices.ContainsFunc(tx.Trace, func(e TraceEntry) bool { return e.Action == TraceCancelled && e.Kind == "test.result" }) {
		t.Error("trace does not mark the late result as cancelled")
	}
}

func TestAbandonedQueuedEmitNeverRuns(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	entered, release := make(chan struct{}), make(chan struct{})
	var ran []int
	h.Handle("h", func(_ context.Context, n note) error {
		ran = append(ran, n.N)
		if n.N == 1 {
			close(entered)
			<-release
		}

		return nil
	})

	first := make(chan error, 1)
	go func() { first <- h.Emit(ctx, note{N: 1}) }()
	<-entered
	queuedCtx, cancel := context.WithCancel(ctx)
	queued := make(chan error, 1)
	go func() { queued <- h.Emit(queuedCtx, note{N: 2}) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Errorf("abandoned Emit = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := s.waitIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ran, []int{1}) || s.journal.Len() != 1 {
		t.Errorf("ran %v with %d transactions; the abandoned Emit must not run", ran, s.journal.Len())
	}
}

func TestStoredObserveContextAfterClose(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var observeCtx context.Context
	h.Observe("keep", func(ctx context.Context, _ TxCommitted) { observeCtx = ctx })
	if err := h.Emit(ctx, note{}); !errors.Is(err, ErrUnhandled) {
		t.Fatal(err)
	}
	_ = s.close(context.Background())
	if err := h.Emit(observeCtx, note{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Emit after close = %v, want ErrClosed", err)
	}
}

func TestObserveHooksRunInPriorityOrderAndSurvivePanics(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var got []string
	h.Observe("b", func(context.Context, note) { got = append(got, "b") }, Priority(0))
	h.Observe("all", func(_ context.Context, ev Event) {
		if ev.Kind() == "test.note" {
			got = append(got, "all")
		}
	})
	h.Observe("c", func(context.Context, note) { got = append(got, "c"); panic("observer bug") }, Priority(200))

	for range 2 {
		if err := h.Emit(ctx, note{}); !errors.Is(err, ErrUnhandled) {
			t.Fatal(err)
		}
	}
	if want := []string{"c", "all", "b", "c", "all", "b"}; !slices.Equal(got, want) {
		t.Errorf("observers ran %v, want %v", got, want)
	}
}

func TestTxEventsCarryTheTransaction(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	h.Mutate("bump", func(_ context.Context, m *Mut[note]) error { m.Event().N++; return nil })
	var started TxStarted
	var committed TxCommitted
	h.Observe("started", func(_ context.Context, ev TxStarted) { started = ev })
	h.Observe("committed", func(_ context.Context, ev TxCommitted) { committed = ev })

	if err := h.Emit(ctx, note{N: 1}); err != nil {
		t.Fatal(err)
	}
	if started.Seq != 1 || len(started.Root) != 1 || started.Root[0].(note).N != 1 {
		t.Errorf("tx.started = %+v, want seq 1 and the root event before Mutate", started)
	}
	if committed.Tx.Seq != 1 || committed.Tx.Events[0].Event.(note).N != 2 {
		t.Errorf("tx.committed = %+v, want the committed transaction", committed.Tx)
	}
}

func TestNestedHookErrorIsRecordedWhenIgnored(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	h.Handle("outer", func(ctx context.Context, _ note) error {
		_ = h.Emit(ctx, result{}) // the error is dropped here

		return nil
	})
	h.Handle("inner", func(context.Context, result) error { return errors.New("inner failed") })

	if err := h.Emit(ctx, note{}); err != nil {
		t.Errorf("Emit = %v; the outer hook dropped the nested error", err)
	}
	if got := s.lastTx(t).kinds(); !slices.Contains(got, "module.error") {
		t.Errorf("events = %v, want the dropped error as module.error", got)
	}
}

func TestNestedHookUsingOuterContext(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var outerCtx context.Context
	var innerErr error
	h.Handle("outer", func(ctx context.Context, _ note) error {
		outerCtx = ctx

		return ignoreUnhandled(h.Emit(ctx, call{}))
	})
	h.Handle("inner", func(context.Context, call) error {
		innerErr = h.Emit(outerCtx, result{})

		return nil
	})

	if err := h.Emit(ctx, note{}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(innerErr, ErrConcurrentEmit) {
		t.Errorf("Emit with the outer hook's ctx = %v, want ErrConcurrentEmit", innerErr)
	}
}

func TestMetaOfAndSources(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s := newSession(sessionConfig{now: func() time.Time { return clock }})
	s.start()
	t.Cleanup(func() { _ = s.close(context.Background()) })
	ctx := s.startContext()
	h := s.host("m")

	var outer, inner, observed Meta
	h.Handle("outer", func(ctx context.Context, _ note) error {
		outer = MetaOf(ctx)

		return ignoreUnhandled(h.Emit(ctx, call{}))
	})
	h.Handle("inner", func(ctx context.Context, _ call) error { inner = MetaOf(ctx); return nil })
	h.Observe("obs", func(ctx context.Context, _ call) { observed = MetaOf(ctx) })

	if err := h.Emit(ctx, note{}); err != nil {
		t.Fatal(err)
	}
	if outer != (Meta{ID: "1.1", Tx: 1, Time: clock, Source: "m"}) {
		t.Errorf("outer meta = %+v, want the Start emit by instance m", outer)
	}
	if inner != (Meta{ID: "1.2", Tx: 1, Time: clock, Source: "m:outer"}) || observed != inner {
		t.Errorf("inner meta = %+v, observed %+v; want the nested emit by m:outer", inner, observed)
	}
	if MetaOf(context.Background()) != (Meta{}) || MetaOf(ctx) != (Meta{}) {
		t.Error("MetaOf outside a hook is not zero")
	}
}

func TestNoDeadlockWarningForAnotherInstance(t *testing.T) {
	t.Parallel()
	s := newSession(sessionConfig{warnAfter: 5 * time.Millisecond})
	s.start()
	t.Cleanup(func() { _ = s.close(context.Background()) })
	ctx := s.startContext()
	h, other := s.host("m"), s.host("other")

	entered, release := make(chan struct{}), make(chan struct{})
	h.Handle("slow", func(context.Context, note) error {
		close(entered)
		<-release

		return nil
	})
	first := make(chan error, 1)
	go func() { first <- h.Emit(ctx, note{}) }()
	<-entered
	second := make(chan error, 1)
	go func() { second <- other.Emit(ctx, result{}) }() // waits legitimately
	time.Sleep(30 * time.Millisecond)
	close(release)
	if err := errors.Join(<-first, ignoreUnhandled(<-second)); err != nil {
		t.Fatal(err)
	}

	for _, tx := range s.journal.Txs() {
		if slices.ContainsFunc(tx.Trace, func(e TraceEntry) bool { return e.Action == TraceWarn }) {
			t.Errorf("tx %d warns about a deadlock; the waiting Emit belongs to another instance", tx.Seq)
		}
	}
}

func TestSeedEdgeCases(t *testing.T) {
	t.Parallel()
	s := newSession(sessionConfig{})
	h := s.host("store")

	if err := h.Seed(); err != nil {
		t.Errorf("empty Seed = %v", err)
	}
	if err := h.Seed(Tx{Seq: 0}); err == nil {
		t.Error("Seed accepted Seq 0")
	}
	if err := h.Seed(Tx{Seq: 1}, Tx{Seq: 3}, Tx{Seq: 2}); err == nil {
		t.Error("Seed accepted decreasing sequence numbers")
	}
	if n := s.journal.Len(); n != 0 {
		t.Errorf("a rejected Seed appended %d transactions; it must be all or nothing", n)
	}
	if err := h.Seed(Tx{Seq: 1}, Tx{Seq: 5}); err != nil {
		t.Fatal(err)
	}
	if err := h.Seed(Tx{Seq: 5}); err == nil {
		t.Error("a second Seed repeated a sequence number")
	}
	s.start()
	t.Cleanup(func() { _ = s.close(context.Background()) })
	if err := h.Emit(s.startContext(), note{}); !errors.Is(err, ErrUnhandled) {
		t.Fatal(err)
	}
	if seq := s.lastTx(t).Seq; seq != 6 {
		t.Errorf("first new transaction has Seq %d, want 6", seq)
	}
}
