package aish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aish "github.com/GoldenDeals/aish-core"
)

type ping struct{ N int }

func (ping) Kind() string { return "test.ping" }

// calls records lifecycle calls of every module of a test, in order.
type calls struct {
	mu  sync.Mutex
	log []string
}

func (c *calls) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.log = append(c.log, s)
}

func (c *calls) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.log)
}

// full implements every lifecycle interface; nil funcs succeed.
type full struct {
	name     string
	calls    *calls
	validate func() error
	init     func(context.Context, *aish.Host) error
	start    func(context.Context) error
}

func (m *full) Validate() error {
	m.calls.add("validate " + m.name)
	if m.validate != nil {
		return m.validate()
	}

	return nil
}

func (m *full) Init(ctx context.Context, h *aish.Host) error {
	m.calls.add("init " + m.name)
	if m.init != nil {
		return m.init(ctx, h)
	}

	return nil
}

func (m *full) Start(ctx context.Context) error {
	m.calls.add("start " + m.name)
	if m.start != nil {
		return m.start(ctx)
	}

	return nil
}

func (m *full) Stop(context.Context) error {
	m.calls.add("stop " + m.name)

	return nil
}

// stopOnly opens something in Init and closes it in Stop, without Start.
type stopOnly struct {
	calls *calls
}

func (m stopOnly) Init(context.Context, *aish.Host) error { m.calls.add("init store"); return nil }

func (m stopOnly) Stop(context.Context) error { m.calls.add("stop store"); return nil }

func mustStart(t *testing.T, cfg aish.SessionConfig) *aish.Session {
	t.Helper()
	s, err := aish.NewSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	return s
}

func TestLifecycleOrder(t *testing.T) {
	t.Parallel()
	c := &calls{}
	var started []aish.SessionStarted
	observer := &full{name: "a", calls: c, init: func(_ context.Context, h *aish.Host) error {
		h.Observe("started", func(_ context.Context, ev aish.SessionStarted) { started = append(started, ev) })

		return nil
	}}

	s, err := aish.NewSession(aish.SessionConfig{ID: "s1", Agent: "main", Instances: []aish.Instance{
		{Name: "test.a", Module: observer},
		{Name: "store.jsonl", Module: stopOnly{calls: c}},
		{Name: "test.b/second", Module: &full{name: "b", calls: c}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.get(); !slices.Equal(got, []string{"validate a", "validate b"}) {
		t.Errorf("NewSession ran %v, want only Validate", got)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Errorf("second Close = %v", err)
	}

	want := []string{
		"validate a", "validate b",
		"init a", "init store", "init b",
		"start a", "start b",
		"stop b", "stop store", "stop a",
	}
	if got := c.get(); !slices.Equal(got, want) {
		t.Errorf("calls = %v\nwant    %v", got, want)
	}
	if len(started) != 1 || started[0] != (aish.SessionStarted{ID: "s1", Agent: "main"}) {
		t.Errorf("session.started observed %+v, want once with the session info", started)
	}
	if txs := s.Journal().Txs(); len(txs) != 1 || len(txs[0].Events) != 1 {
		t.Errorf("journal = %+v, want one transaction with session.started and no module.error", txs)
	}
}

func TestNewSessionRejectsBadConfig(t *testing.T) {
	t.Parallel()
	c := &calls{}
	bad := errors.New("no api_key")

	_, err := aish.NewSession(aish.SessionConfig{Instances: []aish.Instance{
		{Name: "llm.anthropic", Module: &full{name: "llm", calls: c, validate: func() error { return bad }}},
		{Name: "llm.anthropic", Module: &full{name: "dup", calls: c}},
		{Name: "Bad Name", Module: &full{name: "bad", calls: c}},
		{Name: "tool.fs"},
	}})
	if !errors.Is(err, bad) {
		t.Errorf("NewSession = %v, want the Validate error among others", err)
	}
	for _, part := range []string{"configured twice", "invalid instance name", "has no module"} {
		if err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("NewSession error lacks %q: %v", part, err)
		}
	}
	if slices.ContainsFunc(c.get(), func(s string) bool { return strings.HasPrefix(s, "init") }) {
		t.Error("Init ran despite a config error")
	}
}

func TestInitMaySeedButNotEmit(t *testing.T) {
	t.Parallel()
	var emitErr, seedErr, lateSeed error
	var host *aish.Host
	m := &full{name: "store", calls: &calls{}, init: func(ctx context.Context, h *aish.Host) error {
		host = h
		emitErr = h.Emit(ctx, ping{})
		seedErr = h.Seed(aish.Tx{Seq: 7, Status: aish.StatusCommitted})

		return nil
	}}
	s := mustStart(t, aish.SessionConfig{Instances: []aish.Instance{{Name: "store.jsonl", Module: m}}})
	if err := s.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	lateSeed = host.Seed(aish.Tx{Seq: 100})

	if !errors.Is(emitErr, aish.ErrNotStarted) {
		t.Errorf("Emit in Init = %v, want ErrNotStarted", emitErr)
	}
	if seedErr != nil || lateSeed == nil {
		t.Errorf("Seed in Init = %v, after Start = %v; want nil and an error", seedErr, lateSeed)
	}
	if txs := s.Journal().Txs(); len(txs) != 2 || txs[1].Seq != 8 {
		t.Errorf("journal = %+v, want the seeded tx 7 and session.started as tx 8", txs)
	}
}

func TestStartFailureStopsWhatPassedInit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		b    *full
		want []string
	}{
		{
			name: "init fails",
			b:    &full{name: "b", init: func(context.Context, *aish.Host) error { return errors.New("boom") }},
			want: []string{"validate a", "validate b", "init a", "init b", "stop a"},
		},
		{
			name: "start fails",
			b:    &full{name: "b", start: func(context.Context) error { panic("boom") }},
			want: []string{"validate a", "validate b", "init a", "init b", "start a", "start b", "stop b", "stop a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &calls{}
			tc.b.calls = c
			s, err := aish.NewSession(aish.SessionConfig{Instances: []aish.Instance{
				{Name: "test.a", Module: &full{name: "a", calls: c}},
				{Name: "test.b", Module: tc.b},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Start(); err == nil || !strings.Contains(err.Error(), "test.b") {
				t.Errorf("Start = %v, want an error naming test.b", err)
			}
			if got := c.get(); !slices.Equal(got, tc.want) {
				t.Errorf("calls = %v\nwant    %v", got, tc.want)
			}
		})
	}
}

// TestWaitCoversObserveChains: a loop-like module keeps queueing the next
// step from Observe; Wait returns only after the chain ends.
func TestWaitCoversObserveChains(t *testing.T) {
	t.Parallel()
	loop := &full{name: "loop", calls: &calls{}}
	loop.init = func(_ context.Context, h *aish.Host) error {
		h.Observe("next", func(ctx context.Context, p ping) {
			time.Sleep(time.Millisecond)
			if p.N < 5 {
				_ = h.Emit(ctx, ping{N: p.N + 1})
			}
		})
		loop.start = func(ctx context.Context) error {
			go func() { _ = h.Emit(ctx, ping{N: 1}) }()

			return nil
		}

		return nil
	}
	s := mustStart(t, aish.SessionConfig{Instances: []aish.Instance{{Name: "agent.loop", Module: loop}}})

	deadline := time.Now().Add(5 * time.Second)
	for s.Journal().Len() < 2 && time.Now().Before(deadline) { // the Start goroutine got its first ping in
		time.Sleep(time.Millisecond)
	}
	if err := s.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := s.Journal().Len(); n != 6 {
		t.Errorf("journal has %d transactions after Wait, want session.started and 5 pings", n)
	}
}

func TestCloseCancelsTheOpenTransaction(t *testing.T) {
	t.Parallel()
	var host *aish.Host
	var startCtx context.Context
	inHook := make(chan struct{})
	m := &full{name: "fe", calls: &calls{}}
	m.init = func(_ context.Context, h *aish.Host) error {
		host = h
		h.Handle("ask", func(ctx context.Context, _ ping) error {
			close(inHook)
			<-ctx.Done() // waits for an answer that never comes

			return context.Cause(ctx)
		})

		return nil
	}
	m.start = func(ctx context.Context) error { startCtx = ctx; return nil }
	s := mustStart(t, aish.SessionConfig{Instances: []aish.Instance{{Name: "frontend.test", Module: m}}})

	emitted := make(chan error, 1)
	go func() { emitted <- host.Emit(startCtx, ping{}) }()
	<-inHook
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := <-emitted; !errors.Is(err, context.Canceled) {
		t.Errorf("Emit of the open transaction = %v, want context.Canceled", err)
	}
	if tx, _ := s.Journal().Last(); tx.Status != aish.StatusCancelled {
		t.Errorf("last transaction is %s, want cancelled", tx.Status)
	}
	if err := host.Emit(startCtx, ping{}); !errors.Is(err, aish.ErrClosed) {
		t.Errorf("Emit after Close = %v, want ErrClosed", err)
	}
}

func TestLogCarriesSessionInstanceAndHook(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(lockedWriter{&mu, &buf}, nil))
	m := &full{name: "tool", calls: &calls{}}
	m.init = func(_ context.Context, h *aish.Host) error {
		h.Log().Info("init")
		h.Handle("exec", func(ctx context.Context, _ ping) error {
			h.Log().InfoContext(ctx, "exec")

			return nil
		})
		m.start = func(ctx context.Context) error {
			go func() { _ = h.Emit(ctx, ping{}) }()

			return nil
		}

		return nil
	}
	s := mustStart(t, aish.SessionConfig{ID: "s1", Instances: []aish.Instance{{Name: "tool.fs", Module: m}}, Log: logger})
	for s.Journal().Len() < 2 {
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("log has %d records, want 2:\n%s", len(lines), buf.String())
	}
	var initRec, execRec map[string]any
	if err := errors.Join(json.Unmarshal([]byte(lines[0]), &initRec), json.Unmarshal([]byte(lines[1]), &execRec)); err != nil {
		t.Fatal(err)
	}
	if initRec["session"] != "s1" || initRec["instance"] != "tool.fs" || initRec["hook"] != nil {
		t.Errorf("init record = %v, want session and instance only", initRec)
	}
	if execRec["hook"] != "tool.fs:exec" || execRec["event"] == nil || execRec["tx"] == nil {
		t.Errorf("exec record = %v, want tx, event and hook", execRec)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.w.Write(p)
}
