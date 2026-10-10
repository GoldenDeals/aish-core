package aish_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	aish "github.com/GoldenDeals/aish-core"
)

// Corner cases of the session lifecycle, through the public API only.

func TestStartAndCloseMisuse(t *testing.T) {
	t.Parallel()

	c := &calls{}
	closedFirst, err := aish.NewSession(aish.SessionConfig{Instances: []aish.Instance{{Name: "test.a", Module: &full{name: "a", calls: c}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := closedFirst.Close(t.Context()); err != nil {
		t.Errorf("Close before Start = %v", err)
	}
	if err := closedFirst.Start(); !errors.Is(err, aish.ErrClosed) {
		t.Errorf("Start after Close = %v, want ErrClosed", err)
	}
	if got := c.get(); !slices.Equal(got, []string{"validate a"}) {
		t.Errorf("calls = %v; a session closed before Start runs no Init or Stop", got)
	}

	s := mustStart(t, aish.SessionConfig{})
	if err := s.Start(); err == nil {
		t.Error("second Start succeeded")
	}
}

func TestEmptySession(t *testing.T) {
	t.Parallel()
	s := mustStart(t, aish.SessionConfig{})
	if err := s.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := s.Journal().Len(); n != 1 {
		t.Errorf("journal has %d transactions, want only session.started", n)
	}
}

type failingStop struct {
	name  string
	calls *calls
	err   error
	panic bool
}

func (m *failingStop) Init(context.Context, *aish.Host) error { return nil }

func (m *failingStop) Stop(context.Context) error {
	m.calls.add("stop " + m.name)
	if m.panic {
		panic("stop bug")
	}

	return m.err
}

func TestCloseRunsEveryStopAndJoinsErrors(t *testing.T) {
	t.Parallel()
	c := &calls{}
	boom := errors.New("flush failed")
	s := mustStart(t, aish.SessionConfig{Instances: []aish.Instance{
		{Name: "test.a", Module: &failingStop{name: "a", calls: c, err: boom}},
		{Name: "test.b", Module: &failingStop{name: "b", calls: c, panic: true}},
		{Name: "test.c", Module: &failingStop{name: "c", calls: c}},
	}})

	err := s.Close(t.Context())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "stop test.b") || !strings.Contains(err.Error(), "stop bug") {
		t.Errorf("Close = %v, want both failures with instance names", err)
	}
	if got := c.get(); !slices.Equal(got, []string{"stop c", "stop b", "stop a"}) {
		t.Errorf("stops = %v, want all of them in reverse order", got)
	}
}

func TestValidatePanicIsAConfigError(t *testing.T) {
	t.Parallel()
	_, err := aish.NewSession(aish.SessionConfig{Instances: []aish.Instance{
		{Name: "llm.anthropic", Module: &full{name: "llm", calls: &calls{}, validate: func() error { panic("nil map") }}},
	}})
	if err == nil || !strings.Contains(err.Error(), "llm.anthropic") || !strings.Contains(err.Error(), "nil map") {
		t.Errorf("NewSession = %v, want the panic as an error naming the instance", err)
	}
}

func TestInstanceNames(t *testing.T) {
	t.Parallel()

	for name, valid := range map[string]bool{
		"llm.anthropic":         true,
		"llm.anthropic/fast":    true,
		"tool.mcp/github-1.x_y": true,
		"tool.fs_read":          true,
		"llm":                   false,
		".anthropic":            false,
		"llm.":                  false,
		"LLM.anthropic":         false,
		"llm.anthropic/":        false,
		"llm.anthropic/a/b":     false,
		"llm.anthropic/a b":     false,
		"llm.anthropic:x":       false,
		"a.b.c":                 false,
	} {
		_, err := aish.NewSession(aish.SessionConfig{Instances: []aish.Instance{{Name: name, Module: &full{name: name, calls: &calls{}}}}})
		if (err == nil) != valid {
			t.Errorf("instance name %q: NewSession = %v, valid=%v expected", name, err, valid)
		}
	}
}

func TestWaitEdges(t *testing.T) {
	t.Parallel()

	notStarted, err := aish.NewSession(aish.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := notStarted.Wait(t.Context()); err != nil {
		t.Errorf("Wait before Start = %v, want an idle session", err)
	}

	var host *aish.Host
	var startCtx context.Context
	entered, release := make(chan struct{}), make(chan struct{})
	m := &full{name: "m", calls: &calls{}}
	m.init = func(_ context.Context, h *aish.Host) error {
		host = h
		h.Handle("slow", func(context.Context, ping) error {
			close(entered)
			<-release

			return nil
		})

		return nil
	}
	m.start = func(ctx context.Context) error { startCtx = ctx; return nil }
	s := mustStart(t, aish.SessionConfig{Instances: []aish.Instance{{Name: "test.m", Module: m}}})

	go func() { _ = host.Emit(startCtx, ping{}) }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := s.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait on a busy session = %v, want its ctx error", err)
	}
	close(release)
	if err := s.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(t.Context()); err != nil {
		t.Errorf("Wait after Close = %v", err)
	}
	select {
	case <-startCtx.Done():
	default:
		t.Error("the Start context is still alive after Close")
	}
}

func TestStoredInitContextNeverEmits(t *testing.T) {
	t.Parallel()
	var host *aish.Host
	var initCtx context.Context
	m := &full{name: "m", calls: &calls{}, init: func(ctx context.Context, h *aish.Host) error {
		host, initCtx = h, ctx

		return nil
	}}
	mustStart(t, aish.SessionConfig{Instances: []aish.Instance{{Name: "test.m", Module: m}}})

	if err := host.Emit(initCtx, ping{}); !errors.Is(err, aish.ErrNotStarted) {
		t.Errorf("Emit with the Init ctx after Start = %v, want ErrNotStarted", err)
	}
}

func TestEmitInsideStartRunsBeforeSessionStarted(t *testing.T) {
	t.Parallel()
	var emitErr error
	m := &full{name: "m", calls: &calls{}}
	m.init = func(_ context.Context, h *aish.Host) error {
		h.Handle("h", func(context.Context, ping) error { return nil })
		m.start = func(ctx context.Context) error {
			emitErr = h.Emit(ctx, ping{}) // synchronous: the bus already runs

			return nil
		}

		return nil
	}
	s := mustStart(t, aish.SessionConfig{Instances: []aish.Instance{{Name: "test.m", Module: m}}})
	if err := s.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}

	if emitErr != nil {
		t.Fatalf("Emit in Start = %v", emitErr)
	}
	txs := s.Journal().Txs()
	if len(txs) != 2 || txs[0].Events[0].Event.Kind() != "test.ping" || txs[1].Events[0].Event.Kind() != "session.started" {
		t.Errorf("journal = %+v, want the ping, then session.started", txs)
	}
}

func TestSessionIDs(t *testing.T) {
	t.Parallel()
	var seen aish.SessionInfo
	m := &full{name: "m", calls: &calls{}, init: func(_ context.Context, h *aish.Host) error {
		seen = h.Session()

		return nil
	}}
	a := mustStart(t, aish.SessionConfig{Agent: "main", Instances: []aish.Instance{{Name: "test.m", Module: m}}})
	b := mustStart(t, aish.SessionConfig{Agent: "reviewer", Parent: a.Info().ID})

	if a.Info().ID == "" || a.Info().ID == b.Info().ID {
		t.Errorf("generated IDs %q and %q, want non-empty and distinct", a.Info().ID, b.Info().ID)
	}
	if seen != a.Info() || b.Info().Parent != a.Info().ID {
		t.Errorf("Host.Session = %+v, child %+v; want the session info", seen, b.Info())
	}
}

// hookSession starts a session whose module hands out its Host and Start ctx.
func hookSession(t *testing.T, cfg aish.SessionConfig, init func(*aish.Host)) (s *aish.Session, h *aish.Host, startCtx context.Context) {
	t.Helper()
	m := &full{name: "m", calls: &calls{}}
	m.init = func(_ context.Context, host *aish.Host) error {
		h = host
		init(host)

		return nil
	}
	m.start = func(ctx context.Context) error { startCtx = ctx; return nil }
	cfg.Instances = []aish.Instance{{Name: "test.m", Module: m}}

	return mustStart(t, cfg), h, startCtx
}

func TestCloseFromHookDoesNotHang(t *testing.T) {
	t.Parallel()
	var sess *aish.Session
	var closeErr error
	var closeCalled bool
	hookDone := make(chan struct{})
	s, host, startCtx := hookSession(t, aish.SessionConfig{}, func(h *aish.Host) {
		h.Handle("quit", func(ctx context.Context, _ ping) error {
			defer close(hookDone)
			closeCalled = true
			closeErr = sess.Close(ctx)

			return nil
		})
	})
	sess = s

	done := make(chan struct{})
	go func() { defer close(done); _ = host.Emit(startCtx, ping{}) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close from a hook hangs")
	}
	// Close cancels the transaction, so Emit may return before the hook does.
	<-hookDone
	if !closeCalled || closeErr != nil {
		t.Errorf("Close from a hook: called %v, err %v", closeCalled, closeErr)
	}
	if err := host.Emit(startCtx, ping{}); !errors.Is(err, aish.ErrClosed) {
		t.Errorf("Emit after Close from a hook = %v, want ErrClosed", err)
	}
}

func TestCloseHonoursItsDeadlineWithStuckHook(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	s, host, startCtx := hookSession(t, aish.SessionConfig{}, func(h *aish.Host) {
		h.Handle("stuck", func(context.Context, ping) error {
			close(entered)
			<-release // ignores cancellation

			return nil
		})
	})
	t.Cleanup(func() { close(release) })

	go func() { _ = host.Emit(startCtx, ping{}) }()
	<-entered

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close with a stuck hook = %v, want the ctx error", err)
	}
}

func TestStreamTimeoutCancelsStalledTransaction(t *testing.T) {
	t.Parallel()
	s, host, startCtx := hookSession(t, aish.SessionConfig{StreamTimeout: 50 * time.Millisecond}, func(h *aish.Host) {
		h.Handle("producer", func(context.Context, streamed) error { return nil })
	})
	stream, _ := aish.NewStream[string]() // the producer forgets to Close

	err := host.Emit(startCtx, streamed{Text: stream})
	if !errors.Is(err, aish.ErrStreamStalled) {
		t.Errorf("Emit = %v, want ErrStreamStalled", err)
	}
	if !errors.Is(stream.Err(), aish.ErrStreamStalled) {
		t.Errorf("stream error = %v, want ErrStreamStalled", stream.Err())
	}
	if tx, _ := s.Journal().Last(); tx.Status != aish.StatusCancelled {
		t.Errorf("status = %v, want cancelled", tx.Status)
	}
}

type streamed struct{ Text aish.Stream[string] }

func (streamed) Kind() string { return "test.streamed" }

type listed struct{ Items []string }

func (listed) Kind() string { return "test.listed" }

func TestJournalDoesNotShareMemoryWithEmitter(t *testing.T) {
	t.Parallel()
	s, host, startCtx := hookSession(t, aish.SessionConfig{}, func(h *aish.Host) {
		h.Handle("sink", func(context.Context, listed) error { return nil })
	})
	items := []string{"a"}
	if err := host.Emit(startCtx, listed{Items: items}); err != nil {
		t.Fatal(err)
	}
	items[0] = "CHANGED"

	tx, _ := s.Journal().Last()
	if got := tx.Events[0].Event.(listed).Items[0]; got != "a" {
		t.Errorf("journaled item = %q, want the value at Emit time", got)
	}
}

func TestResubscribeAfterRemove(t *testing.T) {
	t.Parallel()
	var sub aish.Sub
	var calls int
	_, host, startCtx := hookSession(t, aish.SessionConfig{}, func(h *aish.Host) {
		sub = h.Handle("h", func(context.Context, ping) error { calls++; return nil })
		h.Handle("other", func(context.Context, ping) error { return nil })
	})
	sub.Remove()
	sub.Remove() // idempotent: must not drop the new subscription below
	sub = host.Handle("h", func(context.Context, ping) error { calls += 10; return nil })

	if err := host.Emit(startCtx, ping{}); err != nil {
		t.Fatal(err)
	}
	if calls != 10 {
		t.Errorf("calls = %d, want only the new hook (10)", calls)
	}
	if n := len(host.Hooks[ping]()); n != 2 {
		t.Errorf("%d hooks for test.ping, want 2", n)
	}
}

func TestObserveCtxEndsWithHook(t *testing.T) {
	t.Parallel()
	var obsCtx context.Context
	s, host, startCtx := hookSession(t, aish.SessionConfig{}, func(h *aish.Host) {
		h.Handle("sink", func(context.Context, ping) error { return nil })
		h.Observe("obs", func(ctx context.Context, _ ping) { obsCtx = ctx })
	})
	if err := host.Emit(startCtx, ping{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if obsCtx == nil || obsCtx.Err() == nil {
		t.Error("the Observe hook ctx is still alive after the hook returned")
	}
}
