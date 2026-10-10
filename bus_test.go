package aish

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type call struct {
	Name string
	Args map[string]any
	Tags []string
}

type result struct {
	CallID string
	Text   string
}

type question struct{ ID string }

type answer struct {
	QuestionID string
	Yes        bool
}

type note struct{ N int }

func (call) Kind() string     { return "test.call" }
func (result) Kind() string   { return "test.result" }
func (question) Kind() string { return "test.question" }
func (answer) Kind() string   { return "test.answer" }
func (note) Kind() string     { return "test.note" }

// newTestSession returns a running session and its Start context.
func newTestSession(t *testing.T) (*session, context.Context) {
	t.Helper()
	s := newSession(sessionConfig{})
	s.start()
	t.Cleanup(func() { _ = s.close(context.Background()) })
	ctx := s.startContext()

	return s, ctx
}

func (s *session) lastTx(t *testing.T) Tx {
	t.Helper()
	tx, ok := s.journal.Last()
	if !ok {
		t.Fatal("journal is empty")
	}

	return tx
}

func (tx Tx) kinds() []string {
	out := make([]string, len(tx.Events))
	for i, env := range tx.Events {
		out[i] = env.Event.Kind()
	}

	return out
}

func TestDeliveryOrder(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var order []string
	mut := func(name, rename string) func(context.Context, *Mut[call]) error {
		return func(_ context.Context, m *Mut[call]) error {
			order = append(order, name)
			if rename != "" {
				m.Event().Name = rename
			}

			return nil
		}
	}
	handle := func(name string) func(context.Context, call) error {
		return func(_ context.Context, c call) error {
			order = append(order, name+"("+c.Name+")")

			return nil
		}
	}
	h.Handle("h", handle("h"))
	h.Mutate("b", mut("b", ""), Priority(1000))
	h.Handle("g", handle("g"), Priority(10))
	h.Mutate("a", mut("a", ""), Priority(1000))
	h.Mutate("z", mut("z", "changed"), Priority(2000))

	if err := h.Emit(ctx, call{Name: "orig"}); err != nil {
		t.Fatal(err)
	}

	want := []string{"z", "a", "b", "h(changed)", "g(changed)"}
	if !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	tx := s.lastTx(t)
	if got := tx.Events[0].Event.(call).Name; got != "changed" {
		t.Errorf("journaled Name = %q, want the mutated value", got)
	}
	if tx.Trace[1].Action != TraceChange || tx.Trace[2].Action != TraceMutate {
		t.Errorf("trace = %+v, want change by z, then mutate by a", tx.Trace)
	}
}

func TestRemoveStopsDelivery(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	handled := false
	h.Mutate("drop", func(_ context.Context, m *Mut[call]) error {
		m.Remove()

		return nil
	})
	h.Handle("exec", func(context.Context, call) error {
		handled = true

		return nil
	})

	if err := h.Emit(ctx, call{}); err != nil {
		t.Fatalf("Emit = %v; a removed event counts as handled", err)
	}
	if handled {
		t.Error("Handle hook got a removed event")
	}
	tx := s.lastTx(t)
	if len(tx.Events) != 0 {
		t.Errorf("events = %v, want none", tx.kinds())
	}
	if tx.Trace[1].Action != TraceRemove {
		t.Errorf("trace = %+v, want remove", tx.Trace)
	}
}

// TestBelowContinuesTheChain covers both uses of Below by a policy: re-emit
// an approved call past itself, and record a denied call past executors.
func TestBelowContinuesTheChain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		below int
		want  map[string]int
	}{
		{
			name:  "approved",
			below: 1000,
			want:  map[string]int{"normalize": 1, "policy": 1, "policy2": 1, "exec": 1, "display": 1},
		},
		{
			name:  "denied",
			below: DefaultPriority,
			want:  map[string]int{"normalize": 1, "policy": 1, "policy2": 0, "exec": 0, "display": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := newTestSession(t)
			h := s.host("m")

			got := map[string]int{}
			count := func(name string) func(context.Context, *Mut[call]) error {
				return func(context.Context, *Mut[call]) error {
					got[name]++

					return nil
				}
			}
			h.Mutate("normalize", count("normalize"), Priority(2000))
			h.Mutate("policy", func(ctx context.Context, m *Mut[call]) error {
				got["policy"]++
				m.Remove()

				return h.EmitWith(ctx, Below(tc.below), Envelope{Meta: m.Meta(), Event: *m.Event()})
			}, Priority(1000))
			h.Mutate("policy2", count("policy2"), Priority(900))
			h.Handle("exec", func(context.Context, call) error {
				got["exec"]++

				return nil
			})
			h.Handle("display", func(context.Context, call) error {
				got["display"]++

				return nil
			}, Priority(0))

			if err := h.Emit(ctx, call{Name: "x"}); err != nil {
				t.Fatal(err)
			}

			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("%s got the call %d times, want %d", name, got[name], want)
				}
			}
			tx := s.lastTx(t)
			if len(tx.Events) != 1 || tx.Events[0].Meta.ID != "1.1" {
				t.Errorf("events = %+v, want the re-emitted call journaled under its original ID 1.1", tx.Events)
			}
		})
	}
}

func TestSenderDoesNotGetItsOwnEvent(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var selfGot, otherGot []int
	var nestedErr error
	h.Handle("self", func(ctx context.Context, n note) error {
		selfGot = append(selfGot, n.N)
		if n.N == 1 {
			nestedErr = h.Emit(ctx, note{N: 2})
		}

		return nil
	})
	h.Handle("other", func(_ context.Context, n note) error {
		otherGot = append(otherGot, n.N)

		return nil
	}, Priority(-1))

	if err := h.Emit(ctx, note{N: 1}); err != nil {
		t.Fatal(err)
	}
	if nestedErr != nil {
		t.Errorf("nested Emit = %v", nestedErr)
	}
	if !slices.Equal(selfGot, []int{1}) || !slices.Equal(otherGot, []int{2, 1}) {
		t.Errorf("self got %v, other got %v; want [1] and [2 1]", selfGot, otherGot)
	}
}

func TestUnhandledIgnoresObserveHooks(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var observed []string
	h.Observe("typed", func(_ context.Context, _ note) { observed = append(observed, "typed") })
	h.Observe("all", func(_ context.Context, ev Event) { observed = append(observed, "all:"+ev.Kind()) })

	err := h.Emit(ctx, note{})
	if !errors.Is(err, ErrUnhandled) {
		t.Fatalf("Emit = %v, want ErrUnhandled", err)
	}
	if got := s.lastTx(t).kinds(); !slices.Equal(got, []string{"test.note"}) {
		t.Errorf("events = %v; an unhandled event is still journaled", got)
	}
	want := []string{"all:tx.started", "all:test.note", "typed", "all:tx.committed"}
	if !slices.Equal(observed, want) {
		t.Errorf("observed = %v, want %v", observed, want)
	}
}

func TestDuplicateDeliveryIsBlocked(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	bounce := func(ctx context.Context, _ note) error {
		_ = h.Emit(ctx, Envelope{Meta: MetaOf(ctx), Event: note{}})

		return nil
	}
	h.Handle("a", bounce)
	h.Handle("b", bounce)

	if err := h.Emit(ctx, note{}); err != nil {
		t.Fatal(err)
	}

	tx := s.lastTx(t)
	var dups int
	for _, e := range tx.Trace {
		if e.Action == TraceDuplicate {
			dups++
		}
	}
	if dups == 0 || tx.Status != StatusCommitted {
		t.Errorf("status %s, %d duplicate entries; want a committed tx with blocked duplicates", tx.Status, dups)
	}
	if !slices.Contains(tx.kinds(), "module.error") {
		t.Errorf("events = %v, want module.error for the duplicate", tx.kinds())
	}
}

func TestDepthLimitCancelsTransaction(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	forward := func(ctx context.Context, n note) error {
		return h.Emit(ctx, note{N: n.N + 1})
	}
	h.Handle("a", forward)
	h.Handle("b", forward)

	err := h.Emit(ctx, note{})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("Emit = %v, want ErrLimit", err)
	}
	if tx := s.lastTx(t); tx.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", tx.Status)
	}
}

func TestEventLimitCancelsTransaction(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	h.Handle("flood", func(ctx context.Context, n note) error {
		if n.N != 0 {
			return nil
		}
		for range maxEvents {
			if err := h.Emit(ctx, note{N: 1}); err != nil {
				return err
			}
		}

		return nil
	})
	h.Handle("sink", func(context.Context, note) error { return nil })

	if err := h.Emit(ctx, note{}); !errors.Is(err, ErrLimit) {
		t.Fatalf("Emit = %v, want ErrLimit", err)
	}
	if tx := s.lastTx(t); tx.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", tx.Status)
	}
}

func TestCancelledTransactionKeepsWhatHappened(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	laterRan := false
	h.Handle("exec", func(ctx context.Context, c call) error {
		if err := h.Emit(ctx, result{Text: "file written"}); !errors.Is(err, ErrUnhandled) {
			return err
		}
		h.Cancel() // Ctrl+C
		<-ctx.Done()

		return context.Cause(ctx)
	})
	h.Handle("later", func(context.Context, call) error {
		laterRan = true

		return nil
	}, Priority(0))

	err := h.Emit(ctx, call{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Emit = %v, want context.Canceled", err)
	}
	if laterRan {
		t.Error("a hook ran after the transaction was cancelled")
	}
	tx := s.lastTx(t)
	if tx.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", tx.Status)
	}
	if got := tx.kinds(); !slices.Equal(got[:2], []string{"test.call", "test.result"}) {
		t.Errorf("events = %v, want the call and the result written before cancellation", got)
	}
}

func TestTransactionsRunOneAtATime(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	release := make(chan struct{})
	entered := make(chan struct{})
	var started atomic.Int32
	h.Observe("started", func(context.Context, TxStarted) { started.Add(1) })
	h.Handle("slow", func(_ context.Context, n note) error {
		if n.N == 1 {
			close(entered)
			<-release
		}

		return nil
	})

	first := make(chan error, 1)
	go func() { first <- h.Emit(ctx, note{N: 1}) }()
	<-entered
	second := make(chan error, 1)
	go func() { second <- h.Emit(ctx, note{N: 2}) }()

	time.Sleep(20 * time.Millisecond)
	if n := started.Load(); n != 1 {
		t.Errorf("%d transactions started while the first is open, want 1", n)
	}
	close(release)
	if err := errors.Join(<-first, <-second); err != nil {
		t.Fatal(err)
	}

	txs := s.journal.Txs()
	if len(txs) != 2 || txs[0].Events[0].Event.(note).N != 1 || txs[1].Events[0].Event.(note).N != 2 {
		t.Errorf("journal = %+v, want the notes in tx 1 and 2", txs)
	}
}

func TestEmitRejectsForeignAndStaleContexts(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var inHook error
	var kept context.Context
	h.Handle("h", func(ctx context.Context, _ note) error {
		inHook = h.Emit(context.Background(), result{})
		kept = ctx

		return nil
	})

	if err := h.Emit(context.Background(), note{}); !errors.Is(err, ErrWouldDeadlock) {
		t.Errorf("Emit(Background) = %v, want ErrWouldDeadlock", err)
	}
	if err := h.Emit(ctx, note{}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(inHook, ErrWouldDeadlock) {
		t.Errorf("Emit(Background) inside a hook = %v, want ErrWouldDeadlock", inHook)
	}
	if err := h.Emit(kept, result{}); !errors.Is(err, ErrStaleContext) {
		t.Errorf("Emit with the ctx of a returned hook = %v, want ErrStaleContext", err)
	}
}

func TestConcurrentEmitFromOneHookIsRejected(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	entered := make(chan struct{})
	release := make(chan struct{})
	h.Handle("block", func(_ context.Context, n note) error {
		if n.N == 1 {
			close(entered)
			<-release
		}

		return nil
	})
	var second error
	h.Handle("fan", func(ctx context.Context, n note) error {
		if n.N != 0 {
			return nil
		}
		first := make(chan error, 1)
		go func() { first <- h.Emit(ctx, note{N: 1}) }()
		<-entered
		second = h.Emit(ctx, result{})
		close(release)

		return <-first
	}, Priority(0))

	if err := h.Emit(ctx, note{}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(second, ErrConcurrentEmit) {
		t.Errorf("second Emit while the first is delivering = %v, want ErrConcurrentEmit", second)
	}
}

// TestConcurrentEmitStress has no assertions beyond the error kinds: its job
// is to give the race detector a chance to catch unguarded transaction state.
func TestConcurrentEmitStress(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	errs := make(chan error, 8)
	h.Handle("fan", func(ctx context.Context, _ note) error {
		var wg sync.WaitGroup
		for range cap(errs) {
			wg.Go(func() { errs <- h.Emit(ctx, result{}) })
		}
		wg.Wait()

		return nil
	})

	if err := h.Emit(ctx, note{}); err != nil {
		t.Fatal(err)
	}
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrUnhandled) && !errors.Is(err, ErrConcurrentEmit) {
			t.Errorf("Emit = %v, want ErrUnhandled or ErrConcurrentEmit", err)
		}
	}
}

func TestDeadlockWarning(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := newSession(sessionConfig{
		log:       slog.New(slog.NewTextHandler(&logs, nil)),
		warnAfter: 10 * time.Millisecond,
	})
	s.start()
	t.Cleanup(func() { _ = s.close(context.Background()) })
	startCtx := s.startContext()
	h := s.host("m")

	var inner error
	h.Handle("h", func(context.Context, note) error {
		// The mistake under test: emitting with the stored Start context
		// instead of the hook's own. The timeout stands in for Ctrl+C.
		ctx, cancel := context.WithTimeout(startCtx, 200*time.Millisecond)
		defer cancel()
		inner = h.Emit(ctx, result{})

		return nil
	})

	if err := h.Emit(startCtx, note{}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(inner, context.DeadlineExceeded) {
		t.Errorf("inner Emit = %v, want it to wait until its ctx expires", inner)
	}
	var warned bool
	for _, e := range s.lastTx(t).Trace {
		if e.Action == TraceWarn && e.Hook == "m:h" {
			warned = true
		}
	}
	if !warned {
		t.Error("trace has no warn entry for m:h")
	}
	if !strings.Contains(logs.String(), "possible deadlock") {
		t.Errorf("log = %q, want a possible deadlock warning", logs.String())
	}
	if n := s.journal.Len(); n != 1 {
		t.Errorf("journal has %d transactions; the abandoned Emit must leave the queue", n)
	}
}

func TestObserveIsSynchronousAndOrdered(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var seqs []uint64
	observerDone := false
	var emitErr error
	var lenAfterEmit int
	h.Observe("loop", func(ctx context.Context, c TxCommitted) {
		seqs = append(seqs, c.Tx.Seq)
		if c.Tx.Seq == 1 {
			emitErr = h.Emit(ctx, note{N: 2})
			lenAfterEmit = s.journal.Len()
			time.Sleep(10 * time.Millisecond)
			observerDone = true
		}
	})
	var startedEarly bool
	h.Observe("started", func(_ context.Context, st TxStarted) {
		if st.Seq == 2 && !observerDone {
			startedEarly = true
		}
	})
	secondDone := make(chan struct{})
	h.Handle("h", func(_ context.Context, n note) error {
		if n.N == 2 {
			close(secondDone)
		}

		return nil
	})

	if err := h.Emit(ctx, note{N: 1}); err != nil {
		t.Fatal(err)
	}
	<-secondDone
	_ = s.close(context.Background()) // waits for the second transaction and its Observe hooks

	if emitErr != nil || lenAfterEmit != 1 {
		t.Errorf("Emit from Observe = %v with journal len %d; want nil and 1 (queued, not run)", emitErr, lenAfterEmit)
	}
	if startedEarly {
		t.Error("tx 2 started before the Observe hooks of tx 1 returned")
	}
	if !slices.Equal(seqs, []uint64{1, 2}) {
		t.Errorf("observed seqs %v, want [1 2]", seqs)
	}
}

func TestUnreturnedErrorsAreRecorded(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	h.Observe("o", func(ctx context.Context, n note) {
		if n.N == 0 {
			_ = h.Emit(ctx, result{}) // nobody handles results
		}
	})
	h.Handle("h", func(context.Context, note) error { return nil })
	second := make(chan struct{})
	h.Observe("committed", func(_ context.Context, c TxCommitted) {
		if c.Tx.Seq == 2 {
			close(second)
		}
	})

	if err := h.Emit(ctx, note{}); err != nil {
		t.Fatal(err)
	}
	<-second

	tx := s.lastTx(t)
	if got := tx.kinds(); !slices.Equal(got, []string{"test.result", "module.error"}) {
		t.Fatalf("events = %v, want the result and a module.error", got)
	}
	if me := tx.Events[1].Event.(ModuleError); !strings.Contains(me.Error, "no mutate or handle hook") {
		t.Errorf("module.error = %+v, want the unhandled error", me)
	}
}

func TestHooksGetDeepCopies(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	vandal := func(c call) {
		c.Args["path"] = "/etc/passwd"
		c.Tags[0] = "vandalized"
	}
	var seen call
	h.Handle("first", func(_ context.Context, c call) error { vandal(c); return nil })
	h.Handle("second", func(_ context.Context, c call) error { seen = c; return nil }, Priority(0))
	h.Observe("obs", func(_ context.Context, c call) { vandal(c) })

	if err := h.Emit(ctx, call{Args: map[string]any{"path": "a.txt"}, Tags: []string{"read"}}); err != nil {
		t.Fatal(err)
	}
	_ = s.close(context.Background())

	if seen.Args["path"] != "a.txt" || seen.Tags[0] != "read" {
		t.Errorf("second Handle hook saw %+v; changes of another hook leaked", seen)
	}
	if c := s.lastTx(t).Events[0].Event.(call); c.Args["path"] != "a.txt" || c.Tags[0] != "read" {
		t.Errorf("journal has %+v; changes of Handle or Observe hooks leaked", c)
	}
}

func TestHooksRunOneAtATimeAndMayReenter(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	var inFlight, maxInFlight int32
	var calls []string
	h.Handle("tool", func(ctx context.Context, c call) error {
		if n := atomic.AddInt32(&inFlight, 1); n > atomic.LoadInt32(&maxInFlight) {
			atomic.StoreInt32(&maxInFlight, n)
		}
		defer atomic.AddInt32(&inFlight, -1)
		calls = append(calls, c.Name)
		time.Sleep(time.Millisecond)
		if c.Name == "outer" {
			atomic.AddInt32(&inFlight, -1) // the nested call below is recursion, not concurrency
			defer atomic.AddInt32(&inFlight, 1)

			return h.Emit(ctx, note{})
		}

		return nil
	})
	h.Handle("relay", func(ctx context.Context, _ note) error {
		return h.Emit(ctx, call{Name: "inner"}) // reaches "tool" while it is still running
	})

	if err := h.Emit(ctx, call{Name: "a"}, call{Name: "b"}, call{Name: "outer"}); err != nil {
		t.Fatal(err)
	}

	if want := []string{"a", "b", "outer", "inner"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if maxInFlight != 1 {
		t.Errorf("%d hooks ran at once, want 1", maxInFlight)
	}
}

func TestKindBoundToOneGoType(t *testing.T) {
	t.Parallel()
	s, _ := newTestSession(t)
	h := s.host("m")
	h.Handle("h", func(context.Context, note) error { return nil })

	defer func() {
		if recover() == nil {
			t.Error("subscribing a second Go type to test.note did not panic")
		}
	}()
	h.Handle("other", func(context.Context, otherNote) error { return nil })
}

type otherNote struct{}

func (otherNote) Kind() string { return "test.note" }

func TestCoreEventsAreObserveOnly(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	if err := h.Emit(ctx, ModuleError{}); err == nil {
		t.Error("a module emitted module.error")
	}
	defer func() {
		if recover() == nil {
			t.Error("a Handle hook on tx.committed did not panic")
		}
	}()
	h.Handle("h", func(context.Context, TxCommitted) error { return nil })
}

func TestHooksListsSubscriptions(t *testing.T) {
	t.Parallel()
	s, _ := newTestSession(t)
	h := s.host("policy.ask")
	h.Mutate("authorize", func(context.Context, *Mut[call]) error { return nil }, Priority(1000))
	h.Handle("exec", func(context.Context, call) error { return nil })
	s.host("store.jsonl").Observe("save", func(context.Context, Event) {})
	sub := h.Handle("gone", func(context.Context, call) error { return nil })
	sub.Remove()

	infos := h.Hooks[call]()
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.Mode.String()+" "+info.ID)
	}
	want := []string{"observe store.jsonl:save", "mutate policy.ask:authorize", "handle policy.ask:exec"}
	if !slices.Equal(ids, want) {
		t.Errorf("Hooks[call] = %v, want %v", ids, want)
	}
}

// ignoreUnhandled drops ErrUnhandled: results usually have only Observe
// consumers, so their senders ignore it (§4.5).
func ignoreUnhandled(err error) error {
	if errors.Is(err, ErrUnhandled) {
		return nil
	}

	return err
}

// TestPolicyAsksAndReemits is a small version of the stage 0 scenario: a
// policy intercepts a call, asks through a frontend and re-emits the call.
func TestPolicyAsksAndReemits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		frontend bool
		want     string
	}{
		{name: "approved", frontend: true, want: "written"},
		{name: "nobody to ask", frontend: false, want: "denied: nobody to ask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := newTestSession(t)

			tool := s.host("tool.fs")
			toolRan := false
			tool.Mutate("effects", func(_ context.Context, m *Mut[call]) error {
				m.Event().Tags = []string{"fs.write"}

				return nil
			}, Priority(2000))
			tool.Handle("exec", func(ctx context.Context, _ call) error {
				toolRan = true

				return ignoreUnhandled(tool.Emit(ctx, result{CallID: MetaOf(ctx).ID, Text: "written"}))
			})

			policy := s.host("policy.ask")
			waiting := map[string]Envelope{}
			policy.Mutate("authorize", func(ctx context.Context, m *Mut[call]) error {
				if !slices.Contains(m.Event().Tags, "fs.write") {
					return nil
				}
				m.Remove()
				id := m.Meta().ID
				waiting[id] = Envelope{Meta: m.Meta(), Event: *m.Event()}
				err := policy.Emit(ctx, question{ID: id})
				if errors.Is(err, ErrUnhandled) {
					return ignoreUnhandled(policy.Emit(ctx, result{CallID: id, Text: "denied: nobody to ask"}))
				}

				return err
			}, Priority(1000))
			policy.Handle("answer", func(ctx context.Context, a answer) error {
				if a.Yes {
					return policy.EmitWith(ctx, Below(1000), waiting[a.QuestionID])
				}

				return ignoreUnhandled(policy.Emit(ctx, result{CallID: a.QuestionID, Text: "denied by user"}))
			})

			if tc.frontend {
				fe := s.host("frontend.test")
				fe.Handle("ask", func(ctx context.Context, q question) error {
					return fe.Emit(ctx, answer{QuestionID: q.ID, Yes: true})
				})
			}

			if err := tool.Emit(ctx, call{Name: "fs_edit"}); err != nil {
				t.Fatal(err)
			}

			tx := s.lastTx(t)
			var res result
			for _, env := range tx.Events {
				if r, ok := env.Event.(result); ok {
					res = r
				}
			}
			if res.Text != tc.want || res.CallID != "1.1" {
				t.Errorf("result = %+v, want %q for call 1.1; events %v", res, tc.want, tx.kinds())
			}
			if toolRan != tc.frontend {
				t.Errorf("tool ran = %v, want %v", toolRan, tc.frontend)
			}
		})
	}
}
