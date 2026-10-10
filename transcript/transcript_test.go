package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

func tx(seq uint64, evs ...aish.Event) aish.Tx {
	t := aish.Tx{Seq: seq, Status: aish.StatusCommitted}
	for i, ev := range evs {
		meta := aish.Meta{ID: fmt.Sprintf("%d.%d", seq, i+1), Tx: seq}
		t.Events = append(t.Events, aish.Envelope{Meta: meta, Event: ev})
	}

	return t
}

func assistant(deltas ...events.ContentDelta) events.AssistantMessage {
	s, w := aish.NewStream[events.ContentDelta]()
	for _, d := range deltas {
		w.Write(d)
	}
	w.Close(nil)

	return events.AssistantMessage{Provider: "default", Content: s}
}

func text(s string) events.ContentDelta { return events.ContentDelta{Type: events.DeltaText, Text: s} }

func call(id string) events.ToolCall {
	return events.ToolCall{ID: id, Name: "fs_read", Input: json.RawMessage(`{"path":"` + id + `"}`)}
}

func result(id, out string) events.ToolResult {
	return events.ToolResult{CallID: id, Content: []events.Block{{Type: events.BlockText, Text: out}}}
}

// render prints messages compactly: "user[text:hi tool_result:a]".
func render(msgs []events.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]string, 0, len(m.Content))
		for i := range m.Content {
			blocks = append(blocks, renderBlock(&m.Content[i]))
		}
		parts = append(parts, m.Role+"["+strings.Join(blocks, " ")+"]")
	}

	return strings.Join(parts, " ")
}

func renderBlock(b *events.Block) string {
	switch b.Type {
	case events.BlockText:
		return "text:" + b.Text
	case events.BlockThinking:
		return "thinking:" + b.Text + "/" + b.Signature
	case events.BlockRedactedThinking:
		return "redacted:" + string(b.Data)
	case events.BlockToolUse:
		return "tool_use:" + b.ID
	case events.BlockImage:
		return "image:" + b.MediaType
	case events.BlockToolResult:
		out := "tool_result:" + b.ToolUseID + "="
		if len(b.Content) > 0 {
			out += b.Content[0].Text
		}
		if b.IsError {
			out += "!"
		}

		return out
	default:
		return b.Type
	}
}

// checkInvariants is what the provider API requires of any request.
func checkInvariants(t *testing.T, msgs []events.Message) {
	t.Helper()
	for i, m := range msgs {
		if len(m.Content) == 0 {
			t.Errorf("message %d is empty", i)
		}
		if i == 0 && m.Role != events.RoleUser {
			t.Errorf("first message is %s, want user", m.Role)
		}
		if i > 0 && msgs[i-1].Role == m.Role {
			t.Errorf("messages %d and %d are both %s", i-1, i, m.Role)
		}
		for j := range m.Content {
			if use := &m.Content[j]; use.Type == events.BlockToolUse && countResults(msgs, i+1, use.ID) != 1 {
				t.Errorf("tool_use %s needs exactly one result in the next message", use.ID)
			}
		}
	}
}

func countResults(msgs []events.Message, i int, id string) int {
	if i >= len(msgs) {
		return 0
	}
	n := 0
	for j := range msgs[i].Content {
		if r := &msgs[i].Content[j]; r.Type == events.BlockToolResult && r.ToolUseID == id {
			n++
		}
	}

	return n
}

func TestBuild(t *testing.T) {
	t.Parallel()

	denied := call("b")
	denied.Denied = true
	deniedResult := result("b", "Tool call denied: nobody to ask")
	deniedResult.IsError = true

	for _, tc := range []struct {
		name string
		txs  []aish.Tx
		want string
	}{
		{
			name: "user message with attachments",
			txs: []aish.Tx{tx(1, events.UserMessage{
				Text:   "hi",
				Blocks: []events.Block{{Type: events.BlockText, Text: "$ make"}},
			})},
			want: "user[text:hi text:$ make]",
		},
		{
			name: "step with calls, a denial and a message typed during the step",
			txs: []aish.Tx{
				tx(1, events.UserMessage{Text: "read a and b"}),
				tx(2,
					events.LLMRequest{Provider: "default"},
					assistant(text("Reading "), text("both."), events.ContentDelta{Type: events.DeltaStop, StopReason: "tool_use"}),
					call("a"), denied,
					events.UserQuestion{ID: "q"},
					deniedResult, result("a", "A"),
				),
				tx(3, events.UserMessage{Text: "skip b"}),
			},
			want: "user[text:read a and b] " +
				"assistant[text:Reading both. tool_use:a tool_use:b] " +
				"user[tool_result:a=A tool_result:b=Tool call denied: nobody to ask! text:skip b]",
		},
		{
			name: "call without result, orphan and duplicate results",
			txs: []aish.Tx{
				tx(1, events.UserMessage{Text: "go"}),
				tx(2, assistant(), call("a"), call("b"), result("a", "first"), result("a", "second"), result("x", "orphan")),
			},
			want: "user[text:go] assistant[tool_use:a tool_use:b] user[tool_result:a=first tool_result:b=" + MissingResult + "!]",
		},
		{
			name: "cancelled before any output",
			txs: []aish.Tx{
				tx(1, events.UserMessage{Text: "one"}),
				tx(2, events.LLMRequest{}, assistant(), aish.ModuleError{Error: "boom"}),
				tx(3, events.UserMessage{Text: "two"}),
			},
			want: "user[text:one text:two]",
		},
		{
			name: "compaction replaces earlier transactions",
			txs: []aish.Tx{
				tx(1, events.UserMessage{Text: "old"}),
				tx(2, assistant(text("old answer"))),
				tx(3, events.Compacted{UpTo: 1, Summary: "first summary"}),
				tx(4, events.Compacted{UpTo: 2, Summary: "we talked"}, events.UserMessage{Text: "new"}),
				tx(5, assistant(text("new answer"))),
			},
			want: "user[text:we talked text:new] assistant[text:new answer]",
		},
		{
			name: "thinking is kept signed, in stream order",
			txs: []aish.Tx{
				tx(1, events.UserMessage{Text: "think"}),
				tx(2, assistant(
					events.ContentDelta{Type: events.DeltaThinking, Text: "hmm "},
					events.ContentDelta{Type: events.DeltaThinking, Text: "ok"},
					events.ContentDelta{Type: events.DeltaSignature, Signature: "sig1"},
					events.ContentDelta{Type: events.DeltaRedactedThinking, Data: []byte("xyz")},
					text("Answer"),
					events.ContentDelta{Type: events.DeltaThinking, Text: "cut off"},
				), call("a"), result("a", "A")),
			},
			want: "user[text:think] assistant[thinking:hmm ok/sig1 redacted:xyz text:Answer tool_use:a] user[tool_result:a=A]",
		},
		{name: "empty journal", want: ""},
		{
			name: "user message before the answer in one transaction",
			txs:  []aish.Tx{tx(1, events.UserMessage{Text: "q"}, assistant(text("a")))},
			want: "user[text:q] assistant[text:a]",
		},
		{
			name: "two assistant messages in one transaction merge",
			txs:  []aish.Tx{tx(1, events.UserMessage{Text: "q"}), tx(2, assistant(text("a")), assistant(text("b")))},
			want: "user[text:q] assistant[text:a text:b]",
		},
		{
			name: "denied call without a result",
			txs:  []aish.Tx{tx(1, events.UserMessage{Text: "q"}), tx(2, denied)},
			want: "user[text:q] assistant[tool_use:b] user[tool_result:b=" + MissingResult + "!]",
		},
		{
			name: "result with no content",
			txs:  []aish.Tx{tx(1, events.UserMessage{Text: "q"}), tx(2, call("a"), events.ToolResult{CallID: "a"})},
			want: "user[text:q] assistant[tool_use:a] user[tool_result:a=]",
		},
		{
			name: "result in a later transaction is not matched",
			txs:  []aish.Tx{tx(1, events.UserMessage{Text: "q"}), tx(2, call("a")), tx(3, result("a", "late"))},
			want: "user[text:q] assistant[tool_use:a] user[tool_result:a=" + MissingResult + "!]",
		},
		{
			name: "compaction with an empty summary",
			txs:  []aish.Tx{tx(1, events.UserMessage{Text: "old"}), tx(2, events.Compacted{UpTo: 1}, events.UserMessage{Text: "new"})},
			want: "user[text:new]",
		},
		{
			name: "compaction past the last transaction",
			txs:  []aish.Tx{tx(1, events.UserMessage{Text: "old"}, events.Compacted{UpTo: 99, Summary: "all"})},
			want: "user[text:all]",
		},
		{
			name: "empty user message and image attachment",
			txs: []aish.Tx{
				tx(1, events.UserMessage{}),
				tx(2, events.UserMessage{Blocks: []events.Block{{Type: events.BlockImage, MediaType: "image/png", Data: []byte{1}}}}),
			},
			want: "user[image:image/png]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msgs := Build(tc.txs)
			if got := render(msgs); got != tc.want {
				t.Errorf("Build =\n  %s\nwant\n  %s", got, tc.want)
			}
			checkInvariants(t, msgs)
		})
	}
}

func TestBuildSharesNothingWithTheJournal(t *testing.T) {
	t.Parallel()

	txs := []aish.Tx{
		tx(1, events.UserMessage{Text: "go", Blocks: []events.Block{{Type: events.BlockText, Text: "ctx"}}}),
		tx(2, call("a"), result("a", "A")),
	}
	msgs := Build(txs)
	msgs[0].Content[1].Text = "changed"
	msgs[1].Content[0].Input[2] = 'X'
	msgs[2].Content[0].Content[0].Text = "changed"

	if got := render(Build(txs)); got != "user[text:go text:ctx] assistant[tool_use:a] user[tool_result:a=A]" {
		t.Errorf("journal changed through Build output: %s", got)
	}
	if in := string(txs[1].Events[0].Event.(events.ToolCall).Input); in != `{"path":"a"}` {
		t.Errorf("call input in the journal = %s", in)
	}
}

func TestAgentReturnIsAUserMessage(t *testing.T) {
	t.Parallel()

	msgs := Build([]aish.Tx{
		tx(1, events.UserMessage{Text: "go"}),
		tx(2, assistant(text("started")), call("t1"), result("t1", "started")),
		tx(3, events.AgentReturn{CallID: "t1", Agent: "kid", Text: "done", IsError: true}),
	})

	last := msgs[len(msgs)-1]
	if last.Role != events.RoleUser || len(last.Content) == 0 {
		t.Fatalf("last message = %+v", last)
	}
	want := "<agent_return agent=\"kid\" call=\"t1\" error=\"true\">\ndone\n</agent_return>"
	if got := last.Content[len(last.Content)-1].Text; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}
