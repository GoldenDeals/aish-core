package test_test

import (
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/test"
)

func TestFakeLLM(t *testing.T) {
	t.Parallel()

	llm := &test.FakeLLM{Replies: []test.Reply{{
		Text:  "hi",
		Calls: []events.ToolCall{{ID: "c1", Name: "fs_read"}},
	}}}
	s := test.NewSession(t, aish.Instance{Name: "llm.fake", Module: llm})

	if err := s.Emit(events.LLMRequest{Provider: "default"}); err != nil {
		t.Fatal(err)
	}
	s.Wait()

	var msg events.AssistantMessage
	var call events.ToolCall
	for _, env := range s.Last().Events {
		switch e := env.Event.(type) {
		case events.AssistantMessage:
			msg = e
		case events.ToolCall:
			call = e
		}
	}
	if msg.Text() != "hi" || call.ID != "c1" || len(llm.Requests) != 1 {
		t.Fatalf("text %q, call %q, requests %d", msg.Text(), call.ID, len(llm.Requests))
	}
}
