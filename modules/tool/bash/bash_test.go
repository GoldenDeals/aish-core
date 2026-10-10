package bash_test

import (
	"encoding/json"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/tool/bash"
	"github.com/GoldenDeals/aish-core/test"
)

func run(t *testing.T, input string) (events.ToolCall, events.ToolResult) {
	t.Helper()

	s := test.NewSession(t, aish.Instance{Name: "tool.bash", Module: &bash.Bash{}})
	if err := s.Emit(events.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(input)}); err != nil {
		t.Fatal(err)
	}
	s.Wait()

	var call events.ToolCall
	var res events.ToolResult
	for _, env := range s.Last().Events {
		switch e := env.Event.(type) {
		case events.ToolCall:
			call = e
		case events.ToolResult:
			res = e
		}
	}

	return call, res
}

func TestBash(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, input, want string
		isErr             bool
	}{
		{"ok", `{"command":"echo hi; echo err >&2"}`, "hi\nerr\n", false},
		{"exit", `{"command":"echo out; exit 3"}`, "exit status 3", true},
		{"timeout", `{"command":"sleep 5","timeout":1}`, "timed out", true},
		{"big", `{"command":"head -c 2000000 /dev/zero"}`, "output cut", false},
		{"empty", `{}`, "command is required", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			call, res := run(t, tc.input)
			if res.IsError != tc.isErr || !strings.Contains(res.Content[0].Text, tc.want) {
				t.Fatalf("result = %q (len %d), isErr=%v, want error=%v containing %q", res.Content[0].Text[max(0, len(res.Content[0].Text)-80):], len(res.Content[0].Text), res.IsError, tc.isErr, tc.want)
			}
			if tc.name == "ok" && (call.Effects.ReadOnly || call.Effects.Access[0].Resource != "cmd:echo hi; echo err >&2") {
				t.Fatalf("effects = %+v", call.Effects)
			}
		})
	}
}
