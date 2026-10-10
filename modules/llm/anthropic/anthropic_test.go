package anthropic_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/llm/anthropic"
	"github.com/GoldenDeals/aish-core/test"
	"github.com/GoldenDeals/aish-core/transcript"
)

const sse = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"fs_read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`

// server answers every request with reply and records the request bodies.
type server struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (s *server) start(t *testing.T, status int, reply string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()

		if status == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

func request() events.LLMRequest {
	return events.LLMRequest{
		Provider: "default",
		System:   []*events.Section{{Name: "base", Text: "Be brief."}},
		Tools: []events.ToolSpec{{
			Name: "fs_read", Description: "Read a file",
			Schema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
		}},
		Messages: []events.Message{
			{Role: events.RoleUser, Content: []events.Block{{Type: events.BlockText, Text: "read a.txt"}}},
		},
	}
}

func TestStreamsMessageAndToolCall(t *testing.T) {
	t.Parallel()

	srv := &server{}
	mod := &anthropic.Anthropic{APIKey: "k", BaseURL: srv.start(t, http.StatusOK, sse)}
	s := test.NewSession(t, aish.Instance{Name: "llm.anthropic", Module: mod})

	if err := s.Emit(request()); err != nil {
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
	body := msg.Content.Body()
	stop := body[len(body)-1]
	if msg.Text() != "Hello" || stop.Type != events.DeltaStop || stop.StopReason != "tool_use" || stop.Usage.OutputTokens != 20 {
		t.Errorf("message %q, last delta %+v", msg.Text(), stop)
	}
	if call.ID != "toolu_1" || call.Name != "fs_read" || string(call.Input) != `{"path":"a.txt"}` {
		t.Errorf("call = %+v", call)
	}

	got := srv.bodies[0]
	tools, _ := got["tools"].([]any)
	system, _ := got["system"].([]any)
	if got["model"] != "claude-opus-5-5" || got["stream"] != true || len(tools) != 1 || len(system) != 1 {
		t.Errorf("request body = %v", got)
	}
	if fmt.Sprint(system[0]) == "" || fmt.Sprint(tools[0]) == "" {
		t.Errorf("empty system or tool in %v", got)
	}
}

func TestAPIErrorFailsTheHook(t *testing.T) {
	t.Parallel()

	srv := &server{}
	reply := `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`
	mod := &anthropic.Anthropic{APIKey: "k", BaseURL: srv.start(t, http.StatusBadRequest, reply)}
	s := test.NewSession(t, aish.Instance{Name: "llm.anthropic", Module: mod})

	var hookErr *aish.HookError
	if err := s.Emit(request()); !errors.As(err, &hookErr) {
		t.Fatalf("Emit = %v, want a *HookError", err)
	}
}

// interleaved is a reply whose thinking blocks sit between tool calls and
// carry no thinking text (display omitted): only a signature each.
const interleaved = `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sigA"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_a","name":"fs_read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":"sigB"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_b","name":"fs_read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":3}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`

// The provider rejects a replayed assistant message whose thinking blocks
// moved or vanished, so the history must keep the order of the reply.
func TestHistoryKeepsBlockOrder(t *testing.T) {
	t.Parallel()

	srv := &server{}
	mod := &anthropic.Anthropic{APIKey: "k", BaseURL: srv.start(t, http.StatusOK, interleaved)}
	s := test.NewSession(t, aish.Instance{Name: "llm.anthropic", Module: mod})
	if err := s.Emit(request()); err != nil {
		t.Fatal(err)
	}
	s.Wait()

	msgs := transcript.Build(s.Journal().Txs())
	if len(msgs) == 0 || msgs[0].Role != events.RoleAssistant {
		t.Fatalf("messages = %+v", msgs)
	}
	var got []string
	for _, b := range msgs[0].Content {
		switch b.Type {
		case events.BlockThinking:
			got = append(got, "thinking:"+b.Signature)
		case events.BlockToolUse:
			got = append(got, "tool_use:"+b.ID+":"+b.Name)
		default:
			got = append(got, b.Type)
		}
	}
	want := "thinking:sigA tool_use:toolu_a:fs_read thinking:sigB tool_use:toolu_b:fs_read"
	if strings.Join(got, " ") != want {
		t.Errorf("assistant blocks = %v, want %s", got, want)
	}
}
