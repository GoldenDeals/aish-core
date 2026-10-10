package core_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/agent/loop"
	"github.com/GoldenDeals/aish-core/modules/subagents/core"
	"github.com/GoldenDeals/aish-core/test"
)

// specs are main, kid and a hidden agent, all with the depth limit maxDepth.
func specs(maxDepth int) []aish.AgentSpec {
	mk := func(name, desc string, hidden bool) aish.AgentSpec {
		a := aish.AgentSpec{Name: name}
		aish.Set(&a.Own, core.Settings{Description: desc, Hidden: hidden, Depth: core.Depth{Set: true, N: maxDepth}})

		return a
	}

	return []aish.AgentSpec{mk("main", "the boss", false), mk("kid", "does chores", false), mk("secret", "", true)}
}

type scenario struct {
	parent, child *test.FakeLLM
	session       *test.Session
}

func run(t *testing.T, maxDepth int, parent, child []test.Reply) *scenario {
	t.Helper()

	sc := &scenario{parent: &test.FakeLLM{Replies: parent}, child: &test.FakeLLM{Replies: child}}
	sc.session = test.NewSessionWith(t, aish.SessionConfig{
		Agents: specs(maxDepth),
		Instances: []aish.Instance{
			{Name: "agent.loop", Module: &loop.Loop{}},
			{Name: "llm.fake", Module: sc.parent},
			{Name: "subagents.core", Module: &core.Subagent{}},
		},
		Spawner: func(agent string) ([]aish.Instance, error) {
			if agent != "kid" {
				return nil, errors.New("no such block")
			}

			return []aish.Instance{
				{Name: "agent.loop", Module: &loop.Loop{}},
				{Name: "llm.fake", Module: sc.child},
				{Name: "subagents.core", Module: &core.Subagent{}},
			}, nil
		},
	})
	if err := sc.session.Emit(events.UserMessage{Text: "go"}); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		t.Fatal(err)
	}
	sc.session.Wait()

	return sc
}

func task(agent string) test.Reply {
	in, _ := json.Marshal(map[string]string{"agent": agent, "prompt": "do it"})

	return test.Reply{Calls: []events.ToolCall{{ID: "c1", Name: "task", Input: in}}}
}

// toolResult returns the text of the tool_result the second request carries.
func toolResult(t *testing.T, r events.LLMRequest) (string, bool) {
	t.Helper()

	for _, m := range r.Messages {
		for i := range m.Content {
			if b := &m.Content[i]; b.Type == events.BlockToolResult && len(b.Content) > 0 {
				return b.Content[0].Text, b.IsError
			}
		}
	}
	t.Fatalf("no tool_result in %+v", r.Messages)

	return "", false
}

func toolNames(r events.LLMRequest) []string {
	names := make([]string, 0, len(r.Tools))
	for _, tl := range r.Tools {
		names = append(names, tl.Name)
	}

	return names
}

func TestTaskStartsTheAgentAndItsAnswerReturnsLater(t *testing.T) {
	sc := run(t, 2,
		[]test.Reply{task("kid"), {Text: "waiting"}, {Text: "all done"}},
		[]test.Reply{{Text: "chores finished"}})

	if len(sc.child.Requests) != 1 || !strings.Contains(sc.child.Requests[0].Messages[0].Content[0].Text, "do it") {
		t.Fatalf("child requests = %+v", sc.child.Requests)
	}
	// The call is answered at once, not with the agent's answer.
	if got, isErr := toolResult(t, sc.parent.Requests[1]); !strings.Contains(got, "working in the background") || isErr {
		t.Fatalf("tool result = %q, error %t", got, isErr)
	}
	// The answer comes in a message of its own, and the parent answers it.
	if len(sc.parent.Requests) != 3 {
		t.Fatalf("parent made %d requests, want 3", len(sc.parent.Requests))
	}
	last := sc.parent.Requests[2].Messages[len(sc.parent.Requests[2].Messages)-1]
	if text := last.Content[len(last.Content)-1].Text; last.Role != events.RoleUser ||
		!strings.Contains(text, `<agent_return agent="kid"`) || !strings.Contains(text, "chores finished") {
		t.Fatalf("last message = %+v", last)
	}
	spec := sc.parent.Requests[0].Tools[0]
	if spec.Name != "task" || !strings.Contains(spec.Description, "kid: does chores") ||
		!strings.Contains(spec.Description, "main: the boss") || strings.Contains(spec.Description, "secret") {
		t.Fatalf("spec = %+v", spec)
	}

	var kinds []string
	for _, tx := range sc.session.Journal().Txs() {
		for _, e := range tx.Events {
			kinds = append(kinds, e.Event.Kind())
		}
	}
	for _, want := range []string{"subagent.started", "subagent.finished", "agent.return"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("events = %v, missing %s", kinds, want)
		}
	}
}

func TestChildFailureReturnsAsAnError(t *testing.T) {
	sc := run(t, 2, []test.Reply{task("kid"), {Text: "waiting"}, {Text: "ok"}}, nil)

	last := sc.parent.Requests[2].Messages[len(sc.parent.Requests[2].Messages)-1]
	if text := last.Content[len(last.Content)-1].Text; !strings.Contains(text, `error="true"`) {
		t.Fatalf("last message = %+v, want an error return", last)
	}
}

func TestTaskIsNotOfferedAtTheDepthLimit(t *testing.T) {
	sc := run(t, 1, []test.Reply{task("kid"), {Text: "ok"}, {Text: "done"}}, []test.Reply{{Text: "hi"}})

	if got := toolNames(sc.parent.Requests[0]); !slices.Contains(got, "task") {
		t.Errorf("parent tools = %v", got)
	}
	if got := toolNames(sc.child.Requests[0]); slices.Contains(got, "task") {
		t.Errorf("child at the limit still offers task: %v", got)
	}
}

func TestTaskErrorsGoToTheModel(t *testing.T) {
	for agent, want := range map[string]string{
		"secret": "not available",
		"nobody": "not available",
		"main":   "no such block",
	} {
		sc := run(t, 2, []test.Reply{task(agent), {Text: "ok"}}, nil)

		got, isErr := toolResult(t, sc.parent.Requests[1])
		if !isErr || !strings.Contains(got, want) {
			t.Errorf("%s: result %q, error %t, want %q", agent, got, isErr, want)
		}
	}
}
