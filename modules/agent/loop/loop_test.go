package loop_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/agent/loop"
	"github.com/GoldenDeals/aish-core/modules/policy/ask"
	"github.com/GoldenDeals/aish-core/modules/tool/fs"
	"github.com/GoldenDeals/aish-core/test"
)

// echoTool answers every tool.call with a result.
type echoTool struct{}

func (echoTool) Init(_ context.Context, h *aish.Host) error {
	h.Handle("exec", func(ctx context.Context, c events.ToolCall) error {
		return h.Emit(ctx, events.ToolResult{
			CallID:  c.ID,
			Content: []events.Block{{Type: events.BlockText, Text: "done"}},
		})
	})

	return nil
}

func call(id string) events.ToolCall {
	return events.ToolCall{ID: id, Name: "echo", Input: json.RawMessage(`{}`)}
}

func session(t *testing.T, maxSteps int, replies ...test.Reply) (*test.Session, *test.FakeLLM) {
	t.Helper()

	llm := &test.FakeLLM{Replies: replies}
	s := test.NewSession(t,
		aish.Instance{Name: "agent.loop", Module: &loop.Loop{MaxSteps: maxSteps}},
		aish.Instance{Name: "llm.fake", Module: llm},
		aish.Instance{Name: "tool.echo", Module: echoTool{}},
	)

	return s, llm
}

// say emits a user message; nobody handles it except the loop's Observe hook.
func say(t *testing.T, s *test.Session, text string) {
	t.Helper()

	if err := s.Emit(events.UserMessage{Text: text}); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		t.Fatal(err)
	}
}

func TestToolRoundTrip(t *testing.T) {
	t.Parallel()

	s, llm := session(t, 0,
		test.Reply{Text: "reading", Calls: []events.ToolCall{call("c1")}},
		test.Reply{Text: "all done"},
	)
	say(t, s, "go")
	s.Wait()

	if len(llm.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(llm.Requests))
	}
	msgs := llm.Requests[1].Messages
	if len(msgs) != 3 || msgs[2].Content[0].Type != events.BlockToolResult {
		t.Fatalf("second request messages = %+v", msgs)
	}
}

func TestMaxSteps(t *testing.T) {
	t.Parallel()

	s, llm := session(t, 2,
		test.Reply{Calls: []events.ToolCall{call("c1")}},
		test.Reply{Calls: []events.ToolCall{call("c2")}},
		test.Reply{Calls: []events.ToolCall{call("c3")}},
	)
	say(t, s, "go")
	s.Wait()

	if len(llm.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(llm.Requests))
	}
	var stopped bool
	for _, tx := range s.Journal().Txs() {
		for _, env := range tx.Events {
			if e, ok := env.Event.(events.LoopStopped); ok && e.Reason == "max_steps" {
				stopped = true
			}
		}
	}
	if !stopped {
		t.Fatal("no loop.stopped in the journal")
	}
}

// warnings runs a session with the instances and returns what the loop logged
// at Start.
func warnings(t *testing.T, instances ...aish.Instance) string {
	t.Helper()

	var buf strings.Builder
	s := test.NewSessionWith(t, aish.SessionConfig{
		Instances: append([]aish.Instance{{Name: "agent.loop", Module: &loop.Loop{}}}, instances...),
		Log:       slog.New(slog.NewTextHandler(&buf, nil)),
	})
	s.Wait()

	return buf.String()
}

func TestWarnsWithoutPolicy(t *testing.T) {
	t.Parallel()

	// fs_read's effects hook has priority 2000: a normalizer, not a policy.
	reader := &fs.Read{}
	reader.Root = t.TempDir()
	tool := aish.Instance{Name: "tool.fs_read", Module: reader}

	if got := warnings(t, tool); !strings.Contains(got, "no policy") {
		t.Errorf("no warning without a policy:\n%s", got)
	}
	if got := warnings(t, tool, aish.Instance{Name: "policy.ask", Module: &ask.Policy{}}); strings.Contains(got, "no policy") {
		t.Errorf("warning despite a policy:\n%s", got)
	}
}
