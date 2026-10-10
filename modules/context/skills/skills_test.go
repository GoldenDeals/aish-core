package skills_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/context/skills"
	"github.com/GoldenDeals/aish-core/test"
)

func write(t *testing.T, root, dir, text string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dir, "SKILL.md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSkillCatalogAndTool(t *testing.T) {
	t.Parallel()

	root, override := t.TempDir(), t.TempDir()
	write(t, root, "pdf", "---\nname: pdf\ndescription: Work with PDF files\n---\n# PDF\nUse pdftotext.\n")
	write(t, root, "folded", "---\nname: folded\ndescription: >\n  Long text\n  on two lines\n---\nbody\n")
	write(t, root, "plain", "---\ndescription: \"No name\"\n---\nplain body\n")
	write(t, override, "pdf", "---\nname: pdf\ndescription: Overridden\n---\nnew pdf\n")

	s := test.NewSession(t,
		aish.Instance{Name: "context.skills", Module: &skills.Skills{Dirs: []string{root, filepath.Join(root, "missing"), override}}},
	)

	if err := s.Emit(events.LLMRequest{Provider: "x"}); err != nil {
		t.Fatal(err)
	}
	req, ok := s.Last().Events[0].Event.(events.LLMRequest)
	if !ok {
		t.Fatalf("event = %T", s.Last().Events[0].Event)
	}
	cat := req.Section(skills.SectionName).Text
	for _, want := range []string{"- pdf: Overridden", "- folded: Long text on two lines", "- plain: No name"} {
		if !strings.Contains(cat, want) {
			t.Errorf("catalog lacks %q:\n%s", want, cat)
		}
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "skill" {
		t.Fatalf("tools = %+v", req.Tools)
	}

	got := call(t, s, `{"name":"pdf"}`)
	if got.IsError || !strings.Contains(got.Content[0].Text, "new pdf") {
		t.Errorf("result = %+v", got)
	}
	if got := call(t, s, `{"name":"nope"}`); !got.IsError {
		t.Errorf("unknown skill: %+v", got)
	}
}

func call(t *testing.T, s *test.Session, input string) events.ToolResult {
	t.Helper()

	if err := s.Emit(events.ToolCall{ID: "c", Name: "skill", Input: json.RawMessage(input)}); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		t.Fatal(err)
	}
	for _, e := range s.Last().Events {
		if r, ok := e.Event.(events.ToolResult); ok {
			return r
		}
	}
	t.Fatal("no tool.result")

	return events.ToolResult{}
}
