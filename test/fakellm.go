package test

import (
	"context"
	"errors"
	"fmt"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

// Reply is one scripted model turn: streamed text, then Calls in one Emit.
type Reply struct {
	Text  string
	Calls []events.ToolCall
}

// FakeLLM is a provider that plays Replies in order, one per llm.request.
// It stands in for llm.anthropic: the text goes through a stream written from
// its own goroutine. Read Requests after Session.Wait.
type FakeLLM struct {
	Provider string // provider name to serve, "default" when empty
	Replies  []Reply

	Requests []events.LLMRequest
}

func (f *FakeLLM) Init(_ context.Context, h *aish.Host) error {
	name := f.Provider
	if name == "" {
		name = "default"
	}
	h.Handle("respond", func(ctx context.Context, r events.LLMRequest) error {
		f.Requests = append(f.Requests, r)
		if len(f.Requests) > len(f.Replies) {
			return fmt.Errorf("aish: fake llm: no reply scripted for request %d", len(f.Requests))
		}

		return f.reply(ctx, h, f.Replies[len(f.Requests)-1])
	}, aish.Where(func(r events.LLMRequest) bool { return r.Provider == name }))

	return nil
}

func (f *FakeLLM) reply(ctx context.Context, h *aish.Host, r Reply) error {
	content, w := aish.NewStream[events.ContentDelta]()
	stop := "end_turn"
	if len(r.Calls) > 0 {
		stop = "tool_use"
	}
	go func() {
		w.Write(events.ContentDelta{Type: events.DeltaText, Text: r.Text})
		w.Write(events.ContentDelta{Type: events.DeltaStop, StopReason: stop})
		w.Close(nil)
	}()

	// No frontend means no reader: the message stays in the transaction.
	err := h.Emit(ctx, events.AssistantMessage{Provider: "fake", Model: "fake", Content: content})
	if err != nil && !errors.Is(err, aish.ErrUnhandled) {
		return fmt.Errorf("aish: fake llm: %w", err)
	}
	select {
	case <-content.Done():
	case <-ctx.Done():
		return fmt.Errorf("aish: fake llm: %w", context.Cause(ctx))
	}
	if len(r.Calls) == 0 {
		return nil
	}
	evs := make([]aish.Event, len(r.Calls))
	for i, c := range r.Calls {
		evs[i] = c
	}
	if err := h.Emit(ctx, evs...); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		return fmt.Errorf("aish: fake llm: %w", err)
	}

	return nil
}
