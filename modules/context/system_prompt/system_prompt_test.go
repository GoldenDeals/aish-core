package systemprompt_test

import (
	"os"
	"path/filepath"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	systemprompt "github.com/GoldenDeals/aish-core/modules/context/system_prompt"
	"github.com/GoldenDeals/aish-core/test"
)

func base(t *testing.T, m *systemprompt.SystemPrompt) string {
	t.Helper()

	s := test.NewSession(t, aish.Instance{Name: "context.system_prompt", Module: m})
	if err := s.Emit(events.LLMRequest{}); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	for _, env := range s.Last().Events {
		if r, ok := env.Event.(events.LLMRequest); ok && len(r.System) == 1 && r.System[0].Name == systemprompt.BaseSection {
			return r.System[0].Text
		}
	}
	t.Fatal("no base section")

	return ""
}

func TestSources(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(file, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		m    systemprompt.SystemPrompt
		want string
	}{
		{"default", systemprompt.SystemPrompt{}, systemprompt.DefaultText},
		{"text", systemprompt.SystemPrompt{Text: "inline"}, "inline"},
		{"path", systemprompt.SystemPrompt{Path: file}, "from file"},
	} {
		if got := base(t, &tc.m); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if (&systemprompt.SystemPrompt{Text: "a", Path: "b"}).Validate() == nil {
		t.Error("text and path together must fail validation")
	}
}
