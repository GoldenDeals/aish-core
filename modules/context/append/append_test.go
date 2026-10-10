package appendprompt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	appendprompt "github.com/GoldenDeals/aish-core/modules/context/append"
	systemprompt "github.com/GoldenDeals/aish-core/modules/context/system_prompt"
	"github.com/GoldenDeals/aish-core/test"
)

func system(t *testing.T, mods ...aish.Instance) []*events.Section {
	t.Helper()

	s := test.NewSession(t, mods...)
	if err := s.Emit(events.LLMRequest{}); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	for _, env := range s.Last().Events {
		if r, ok := env.Event.(events.LLMRequest); ok {
			return r.System
		}
	}
	t.Fatal("no llm.request")

	return nil
}

func TestPartsKeepTheirOrderAndComeLast(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "rules.md")
	if err := os.WriteFile(file, []byte("from file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &appendprompt.Append{}
	d := caddyfile.NewTestDispenser("context append " + file + " {\n text \"inline text\"\n}")
	d.Next()
	if err := a.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}

	got := system(t,
		aish.Instance{Name: "context.append", Module: a},
		aish.Instance{Name: "context.system_prompt", Module: &systemprompt.SystemPrompt{Text: "base text"}},
	)
	if len(got) != 2 || got[0].Name != systemprompt.BaseSection || got[1].Name != appendprompt.SectionName {
		t.Fatalf("sections = %+v, want base then append", got)
	}
	if want := "from file\n\ninline text"; got[1].Text != want {
		t.Errorf("text = %q, want %q", got[1].Text, want)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	for name, a := range map[string]*appendprompt.Append{
		"empty":    {},
		"both":     {Parts: []appendprompt.Part{{Text: "a", Path: "b"}}},
		"no value": {Parts: []appendprompt.Part{{}}},
	} {
		if a.Validate() == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
	if err := (&appendprompt.Append{Parts: []appendprompt.Part{{Path: "/missing/file"}}}).Validate(); err != nil {
		t.Errorf("a missing file is found at Init, not by Validate: %v", err)
	}
}

func TestMissingFileFailsInit(t *testing.T) {
	t.Parallel()

	s, err := aish.NewSession(aish.SessionConfig{Instances: []aish.Instance{
		{Name: "context.append", Module: &appendprompt.Append{Parts: []appendprompt.Part{{Path: "/missing/file"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil || !strings.Contains(err.Error(), "/missing/file") {
		t.Fatalf("Start = %v, want the missing file in the error", err)
	}
}
