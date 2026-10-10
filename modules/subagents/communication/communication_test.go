package communication_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/agent/loop"
	"github.com/GoldenDeals/aish-core/modules/subagents/communication"
	"github.com/GoldenDeals/aish-core/test"
)

func instances(llm *test.FakeLLM) []aish.Instance {
	return []aish.Instance{
		{Name: "agent.loop", Module: &loop.Loop{}},
		{Name: "llm.fake", Module: llm},
		{Name: "subagents.communication", Module: &communication.Communication{}},
	}
}

func TestMessageReachesAnotherSession(t *testing.T) {
	other := &test.FakeLLM{Replies: []test.Reply{{Text: "got it"}}}
	otherSession := test.NewSessionWith(t, aish.SessionConfig{Instances: instances(other)})

	in, _ := json.Marshal(map[string]string{"to": otherSession.Info().ID, "text": "hello <b>"})
	main := &test.FakeLLM{Replies: []test.Reply{
		{Calls: []events.ToolCall{{ID: "c1", Name: "send_message", Input: in}}},
		{Text: "sent"},
	}}
	mainSession := test.NewSessionWith(t, aish.SessionConfig{Instances: instances(main)})
	if err := mainSession.Emit(events.UserMessage{Text: "go"}); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		t.Fatal(err)
	}
	mainSession.Wait()
	otherSession.Wait()

	if len(other.Requests) != 1 {
		t.Fatalf("other made %d requests, want 1", len(other.Requests))
	}
	got := other.Requests[0].Messages[0].Content[0].Text
	if !strings.Contains(got, "<agent_message") || !strings.Contains(got, "hello <b>") {
		t.Fatalf("message = %q", got)
	}
}

func TestMutualMessagesDoNotDeadlock(t *testing.T) {
	var sessions [2]*test.Session
	var llms [2]*test.FakeLLM
	for i := range sessions {
		llms[i] = &test.FakeLLM{}
		sessions[i] = test.NewSessionWith(t, aish.SessionConfig{Instances: instances(llms[i])})
	}
	for i := range sessions {
		in, _ := json.Marshal(map[string]string{"to": sessions[1-i].Info().ID, "text": "hi"})
		llms[i].Replies = []test.Reply{{Calls: []events.ToolCall{{ID: "c", Name: "send_message", Input: in}}}, {Text: "ok"}, {Text: "ok"}}
	}
	for i := range sessions {
		// Both tools run while the other session has its transaction open.
		go func() { _ = sessions[i].Emit(events.UserMessage{Text: "go"}) }()
	}
	done := make(chan struct{})
	go func() {
		for _, s := range sessions {
			s.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock")
	}
}
