package main

import (
	"strings"
	"testing"
)

var std = []string{
	corePath + "/modules/agent/loop",
	corePath + "/modules/llm/anthropic",
	corePath + "/modules/policy/ask",
	corePath + "/modules/tool/fs",
}

func TestGenerateStandard(t *testing.T) {
	t.Parallel()

	files, err := generate(options{}, std)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"modules/agent/loop", "modules/llm/anthropic", "modules/tool/fs", "app.Run()"} {
		if !strings.Contains(files["main.go"], want) {
			t.Errorf("main.go lacks %s:\n%s", want, files["main.go"])
		}
	}
	if strings.Contains(files["go.mod"], "require") || strings.Contains(files["go.mod"], "replace") {
		t.Errorf("go.mod pins nothing by default:\n%s", files["go.mod"])
	}
}

func TestGenerateWithAndWithout(t *testing.T) {
	t.Parallel()

	files, err := generate(options{
		With:    []string{"example.com/web@v0.2.0", "example.com/local/plugin=/work/plugin"},
		Without: []string{"llm/anthropic", "tool/fs"},
		Core:    "/src/aish",
	}, std)
	if err != nil {
		t.Fatal(err)
	}
	mainGo, goMod := files["main.go"], files["go.mod"]
	if strings.Contains(mainGo, "llm/anthropic") || strings.Contains(mainGo, "tool/fs") {
		t.Errorf("excluded modules are imported:\n%s", mainGo)
	}
	for _, want := range []string{`_ "example.com/web"`, `_ "example.com/local/plugin"`, "modules/policy/ask"} {
		if !strings.Contains(mainGo, want) {
			t.Errorf("main.go lacks %s:\n%s", want, mainGo)
		}
	}
	for _, want := range []string{
		"require example.com/web v0.2.0",
		"require example.com/local/plugin v0.0.0",
		"replace example.com/local/plugin => /work/plugin",
		"replace github.com/GoldenDeals/aish-core => /src/aish",
	} {
		if !strings.Contains(goMod, want) {
			t.Errorf("go.mod lacks %q:\n%s", want, goMod)
		}
	}
}

func TestGenerateErrors(t *testing.T) {
	t.Parallel()

	for name, o := range map[string]options{
		"unknown package": {Without: []string{"tool/mcp"}},
		"empty with":      {With: []string{"@v1"}},
	} {
		if _, err := generate(o, std); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
