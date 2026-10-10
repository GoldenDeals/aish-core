package test_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/test"
)

// asker asks one question per user message.
type asker struct{}

func (asker) Init(_ context.Context, h *aish.Host) error {
	h.Handle("ask", func(ctx context.Context, m aish.UserMessage) error {
		return h.Emit(ctx, aish.UserQuestion{ID: m.Text, Text: "Sure?"})
	})

	return nil
}

func TestFrontendRunsOutOfAnswers(t *testing.T) {
	t.Parallel()
	fe := &test.Frontend{Answers: []string{"yes"}}
	s := test.NewSession(t, aish.Instance{Name: "test.asker", Module: asker{}}, aish.Instance{Name: "frontend.test", Module: fe})

	if err := s.Emit(aish.UserMessage{Text: "q1"}); !errors.Is(err, aish.ErrUnhandled) {
		t.Errorf("first question = %v, want only ErrUnhandled for the answer nobody handles", err)
	}
	err := s.Emit(aish.UserMessage{Text: "q2"})
	if err == nil || !strings.Contains(err.Error(), "no answer scripted") {
		t.Errorf("second question = %v, want the frontend's error", err)
	}
	s.Wait()

	if len(fe.Asked) != 2 || fe.Asked[1].ID != "q2" {
		t.Errorf("Asked = %+v, want both questions", fe.Asked)
	}
	events := s.Last().Events
	kinds := make([]string, 0, len(events))
	for _, env := range events {
		kinds = append(kinds, env.Event.Kind())
	}
	if !slices.Contains(kinds, "module.error") {
		t.Errorf("events = %v, want the failure journaled", kinds)
	}
}

func TestSessionKeepsCallerInstances(t *testing.T) {
	t.Parallel()
	instances := make([]aish.Instance, 1, 4) // spare capacity NewSession must not write into
	instances[0] = aish.Instance{Name: "test.asker", Module: asker{}}
	test.NewSession(t, instances...)

	if extra := instances[:2][1]; extra.Module != nil {
		t.Errorf("NewSession wrote %q into the caller's slice", extra.Name)
	}
}
