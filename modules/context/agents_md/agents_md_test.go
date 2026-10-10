package agentsmd_test

import (
	"os"
	"path/filepath"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	agentsmd "github.com/GoldenDeals/aish-core/modules/context/agents_md"
	"github.com/GoldenDeals/aish-core/test"
)

func write(t *testing.T, path, text string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func section(t *testing.T, m *agentsmd.AgentsMD) string {
	t.Helper()

	s := test.NewSession(t, aish.Instance{Name: "context.agents_md", Module: m})
	if err := s.Emit(events.LLMRequest{}); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	for _, env := range s.Last().Events {
		if r, ok := env.Event.(events.LLMRequest); ok && len(r.System) == 1 {
			return r.System[0].Text
		}
	}

	return ""
}

func TestSection(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "work", "proj")
	write(t, filepath.Join(home, ".agents", "AGENTS.md"), "user-level")
	write(t, filepath.Join(home, "AGENTS.md"), "in-home")
	write(t, filepath.Join(proj, "AGENTS.md"), "in-proj")
	write(t, filepath.Join(proj, "docs", "rules-a.md"), "rules-a")
	t.Setenv("HOME", home)
	t.Chdir(proj)

	got := section(t, &agentsmd.AgentsMD{Patterns: []string{"docs/rules-*.md", "missing/*.md"}, Home: true, Walk: true, Local: true})
	want := "Instructions from " + filepath.Join(home, ".agents", "AGENTS.md") + ":\n\nuser-level\n\n" +
		"Instructions from " + filepath.Join(home, "AGENTS.md") + ":\n\nin-home\n\n" +
		"Instructions from " + filepath.Join(proj, "AGENTS.md") + ":\n\nin-proj\n\n" +
		"Instructions from " + filepath.Join(proj, "docs", "rules-a.md") + ":\n\nrules-a"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestLocal(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "proj")
	write(t, filepath.Join(home, "AGENTS.md"), "in-home")
	write(t, filepath.Join(proj, "AGENTS.md"), "in-proj")
	t.Setenv("HOME", home)
	t.Chdir(proj)

	want := "Instructions from " + filepath.Join(proj, "AGENTS.md") + ":\n\nin-proj"
	if got := section(t, &agentsmd.AgentsMD{Local: true}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
