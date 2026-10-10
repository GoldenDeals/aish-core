package cedar

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoldenDeals/aish-core/events"
)

func TestRewriteAsk(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, in, want string }{
		{"effect", "ask(principal, action, resource);", `@decision("ask") permit(principal, action, resource);`},
		{"space", "ask (principal, action, resource);", `@decision("ask") permit (principal, action, resource);`},
		{"after annotation", "@id(\"x\")\nask(principal, action, resource);", "@id(\"x\")\n@decision(\"ask\") permit(principal, action, resource);"},
		{"string", `permit(principal, action, resource) when { resource.cmd == "ask(" };`, `permit(principal, action, resource) when { resource.cmd == "ask(" };`},
		{"comment", "// ask(\npermit(principal, action, resource);", "// ask(\npermit(principal, action, resource);"},
		{"attribute", "permit(principal, action, resource) when { context.ask(1) };", "permit(principal, action, resource) when { context.ask(1) };"},
		{"longer word", "task(principal);", "task(principal);"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := rewriteAsk(tc.in); got != tc.want {
				t.Errorf("got\n%s\nwant\n%s", got, tc.want)
			}
			if strings.Count(tc.in, "\n") != strings.Count(rewriteAsk(tc.in), "\n") {
				t.Error("line count changed")
			}
		})
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()

	const rules = `permit(principal, action == Action::"fs.read", resource);
ask(principal, action == Action::"fs.write", resource);
permit(principal, action == Action::"fs.write", resource) when { resource.path like "/tmp/*" };
forbid(principal, action, resource) when { resource.path like "*/.env*" };
permit(principal, action == Action::"shell.exec", resource) when { context.input.command == "ls" };
permit(principal, action == Action::"tool", resource == Tool::"skill");
`
	file := filepath.Join(t.TempDir(), "p.cedar")
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := load([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	p := &Policy{set: set, agent: "main", log: slog.New(slog.DiscardHandler)}

	access := func(action, resource string) events.Effects {
		return events.Effects{Access: []events.Access{{Action: action, Resource: resource}}}
	}
	tests := []struct {
		name string
		call events.ToolCall
		want decision
	}{
		{"read permitted", events.ToolCall{Effects: access("fs.read", "file:/src/a.go")}, permit},
		{"write asks", events.ToolCall{Effects: access("fs.write", "file:/src/a.go")}, ask},
		{"ask beats permit", events.ToolCall{Effects: access("fs.write", "file:/tmp/a.go")}, ask},
		{"forbid beats all", events.ToolCall{Effects: access("fs.read", "file:/src/.env")}, forbid},
		{"input in context", events.ToolCall{Input: json.RawMessage(`{"command":"ls"}`), Effects: access("shell.exec", "cmd:ls")}, permit},
		{"other input", events.ToolCall{Input: json.RawMessage(`{"command":"rm"}`), Effects: access("shell.exec", "cmd:rm")}, forbid},
		{"no access uses Tool", events.ToolCall{Name: "skill"}, permit},
		{"no access, no rule", events.ToolCall{Name: "other"}, forbid},
		{"worst access wins", events.ToolCall{Effects: events.Effects{Access: []events.Access{
			{Action: "fs.read", Resource: "file:/a"}, {Action: "fs.write", Resource: "file:/b"},
		}}}, ask},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, reason, err := p.decide(t.Context(), tc.call)
			if err != nil || got != tc.want {
				t.Errorf("got %v (%s), %v; want %v", got, reason, err, tc.want)
			}
		})
	}
}
