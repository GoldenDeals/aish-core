package aish_test

import (
	"context"
	"errors"
	"testing"
	"time"

	aish "github.com/GoldenDeals/aish-core"
)

// spawner is a module that spawns an agent from a Start goroutine's ctx and
// keeps the child.
type spawner struct {
	h     *aish.Host
	child *aish.Session
	err   error
}

func (m *spawner) Init(_ context.Context, h *aish.Host) error { m.h = h; return nil }

func (m *spawner) Start(ctx context.Context) error {
	m.child, m.err = m.h.Spawn(ctx, "kid")

	return nil
}

// idle is a module that does nothing.
type idle struct{}

func (idle) Init(context.Context, *aish.Host) error { return nil }

func newSpawnParent(t *testing.T, sp *spawner, depth int) *aish.Session {
	t.Helper()

	s, err := aish.NewSession(aish.SessionConfig{
		ID:        "parent",
		Agent:     "main",
		Depth:     depth,
		Instances: []aish.Instance{{Name: "test.spawner", Module: sp}},
		Spawner: func(agent string) ([]aish.Instance, error) {
			if agent != "kid" {
				return nil, errors.New("unknown agent")
			}

			return []aish.Instance{{Name: "test.kid", Module: idle{}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestSpawnChildLinksToParentAndClosesWithIt(t *testing.T) {
	sp := &spawner{}
	s := newSpawnParent(t, sp, 1)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if sp.err != nil {
		t.Fatal(sp.err)
	}

	info := sp.child.Info()
	if info.Parent != "parent" || info.Agent != "kid" || info.Depth != 2 || info.ID == "parent" {
		t.Fatalf("child info = %+v", info)
	}
	if err := sp.child.Emit(t.Context(), ping{}); !errors.Is(err, aish.ErrUnhandled) {
		t.Fatalf("Emit to the child = %v, want ErrUnhandled (nobody handles ping)", err)
	}
	if n := len(sp.child.Journal().Txs()); n < 2 {
		t.Fatalf("child journal has %d txs, want session.started and the ping", n)
	}

	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := sp.child.Emit(t.Context(), ping{}); !errors.Is(err, aish.ErrClosed) {
		t.Fatalf("Emit to the child after the parent closed = %v, want ErrClosed", err)
	}
}

func TestSpawnUnknownAgent(t *testing.T) {
	sp := &spawner{}
	s := newSpawnParent(t, sp, 0)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	if _, err := sp.h.Spawn(t.Context(), "nobody"); err == nil {
		t.Fatal("Spawn of an unknown agent succeeded")
	}
}

func TestHoldKeepsWaitWaiting(t *testing.T) {
	t.Parallel()

	sp := &spawner{}
	s := newSpawnParent(t, sp, 0)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	release := sp.h.Hold()
	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := s.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with a hold = %v, want it to keep waiting", err)
	}

	release()
	release() // twice is fine
	if err := s.Wait(t.Context()); err != nil {
		t.Fatalf("Wait after release = %v", err)
	}
}

// blocker holds a transaction open until its ctx is cancelled.
type blocker struct{ entered chan struct{} }

func (b blocker) Init(_ context.Context, h *aish.Host) error {
	h.Handle("block", func(ctx context.Context, _ ping) error {
		close(b.entered)
		<-ctx.Done()

		return nil
	})

	return nil
}

func TestCancelCascadesToSubagents(t *testing.T) {
	t.Parallel()

	kid := blocker{entered: make(chan struct{})}
	sp := &spawner{}
	s, err := aish.NewSession(aish.SessionConfig{
		Agent:     "main",
		Instances: []aish.Instance{{Name: "test.spawner", Module: sp}},
		Spawner: func(string) ([]aish.Instance, error) {
			return []aish.Instance{{Name: "test.kid", Module: kid}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	emitted := make(chan error, 1)
	go func() { emitted <- sp.child.Emit(context.Background(), ping{}) }()
	<-kid.entered

	sp.h.Cancel() // the parent has no open transaction; the subagent has

	select {
	case err := <-emitted:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Emit to the subagent = %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the subagent's transaction was not cancelled")
	}
	if tx, _ := sp.child.Journal().Last(); tx.Status != aish.StatusCancelled {
		t.Errorf("subagent's last transaction is %v, want cancelled", tx.Status)
	}
}

func TestSubagentContextEndsWithItsParent(t *testing.T) {
	t.Parallel()

	kid := blocker{entered: make(chan struct{})}
	sp := &spawner{}
	s, err := aish.NewSession(aish.SessionConfig{
		Agent:     "main",
		Instances: []aish.Instance{{Name: "test.spawner", Module: sp}},
		Spawner: func(string) ([]aish.Instance, error) {
			return []aish.Instance{{Name: "test.kid", Module: kid}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	emitted := make(chan error, 1)
	go func() { emitted <- sp.child.Emit(context.Background(), ping{}) }()
	<-kid.entered

	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the parent left the subagent running")
	}
}
