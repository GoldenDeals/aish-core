// Package test runs modules in a real session for tests (§10). It uses only
// the public API of the core.
package test

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	aish "github.com/GoldenDeals/aish-core"
)

// DriverInstance is the instance through which Session.Emit emits on
// behalf of the test, as a frontend would.
const DriverInstance = "test.driver"

// waitTimeout bounds Session.Wait: a hung session fails the test instead of
// hanging it.
const waitTimeout = 10 * time.Second

type Session struct {
	*aish.Session

	t      testing.TB
	driver *driver
}

// NewSession starts a session of the instances plus the driver instance and
// closes it when the test ends. Logs go to the test output.
func NewSession(t testing.TB, instances ...aish.Instance) *Session {
	t.Helper()

	d := &driver{}
	s, err := aish.NewSession(aish.SessionConfig{
		Agent:     "main",
		Instances: slices.Concat(instances, []aish.Instance{{Name: DriverInstance, Module: d}}),
		Log:       slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("close session: %v", err)
		}
	})

	return &Session{Session: s, t: t, driver: d}
}

// Emit emits events in a new transaction and waits until it has committed,
// like a frontend emitting from its Start goroutine.
func (s *Session) Emit(evs ...aish.Event) error {
	if err := s.driver.host.Emit(s.driver.ctx, evs...); err != nil {
		return fmt.Errorf("aish: test emit: %w", err)
	}

	return nil
}

// Wait waits until the session is idle and fails the test if it does not
// get there in time.
func (s *Session) Wait() {
	s.t.Helper()

	ctx, cancel := context.WithTimeout(s.t.Context(), waitTimeout)
	defer cancel()

	if err := s.Session.Wait(ctx); err != nil {
		s.t.Fatal(err)
	}
}

// Last returns the last committed transaction.
func (s *Session) Last() aish.Tx {
	s.t.Helper()

	tx, ok := s.Journal().Last()
	if !ok {
		s.t.Fatal("journal is empty")
	}

	return tx
}

type driver struct {
	host *aish.Host
	ctx  context.Context //nolint:containedctx // the Start context is how a module emits outside hooks
}

func (d *driver) Init(_ context.Context, h *aish.Host) error {
	d.host = h

	return nil
}

func (d *driver) Start(ctx context.Context) error {
	d.ctx = ctx

	return nil
}

// Frontend is a scripted frontend for scenario and conformance tests: it
// answers each user.question with the next of Answers and records what it
// was shown. Read its fields after Session.Wait.
type Frontend struct {
	Answers []string // UserAnswer.Choice, in order

	Asked  []aish.UserQuestion
	Denied []aish.ToolCall // denied calls, which reach hooks below executors
}

func (f *Frontend) Init(_ context.Context, h *aish.Host) error {
	h.Handle("ask", func(ctx context.Context, q aish.UserQuestion) error {
		f.Asked = append(f.Asked, q)
		if len(f.Asked) > len(f.Answers) {
			return fmt.Errorf("aish: test frontend: no answer scripted for question %q", q.Text)
		}

		return h.Emit(ctx, aish.UserAnswer{QuestionID: q.ID, Choice: f.Answers[len(f.Asked)-1]})
	})
	h.Handle("show-denied", func(_ context.Context, c aish.ToolCall) error {
		f.Denied = append(f.Denied, c)

		return nil
	}, aish.Priority(0), aish.Where(func(c aish.ToolCall) bool { return c.Denied }))

	return nil
}
