package agentfile_test

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/modules/agent/loop"
	"github.com/GoldenDeals/aish-core/modules/config/agentfile"
	"github.com/GoldenDeals/aish-core/modules/log/file"
	_ "github.com/GoldenDeals/aish-core/modules/standard"
	"github.com/GoldenDeals/aish-core/modules/store/jsonl"
	"github.com/GoldenDeals/aish-core/modules/subagents/core"
	"github.com/GoldenDeals/aish-core/modules/tool/fs"
)

func load(t testing.TB, src string) (*aish.Config, error) {
	t.Helper()

	return agentfile.Loader{}.Load("Agentfile", strings.NewReader(src))
}

func build(t testing.TB, cfg *aish.Config, agent string) []aish.Instance {
	t.Helper()

	insts, err := cfg.Build(agent)
	if err != nil {
		t.Fatal(err)
	}

	return insts
}

func names(insts []aish.Instance) []string {
	out := make([]string, len(insts))
	for i, inst := range insts {
		out[i] = inst.Name
	}

	return out
}

const caddyfile = `
(files) {
    tool fs_read
    tool fs_edit {
        root /work
    }
}

main {
    max_steps 7
    agent loop
    frontend cli
    import files
    policy ask
    store jsonl {$TEST_AISH_STORE}
}
`

func TestLoad(t *testing.T) {
	t.Setenv("TEST_AISH_STORE", "/var/aish")

	cfg, err := load(t, caddyfile)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := aish.Get[loop.Settings](cfg.Agents[0].Own); got.MaxSteps != 7 {
		t.Errorf("max steps = %+v", got)
	}
	got := map[string]any{}
	for _, inst := range build(t, cfg, "main") {
		got[inst.Name] = inst.Module
	}
	for _, id := range []string{"agent.loop", "frontend.cli", "tool.fs_read", "tool.fs_edit", "policy.ask", "store.jsonl"} {
		if got[id] == nil {
			t.Errorf("instance %s missing, have %v", id, got)
		}
	}
	if edit, _ := got["tool.fs_edit"].(*fs.Edit); edit == nil || edit.Root != "/work" {
		t.Errorf("fs_edit = %+v", got["tool.fs_edit"])
	}
	if store, _ := got["store.jsonl"].(*jsonl.Store); store == nil || store.Dir != "/var/aish" {
		t.Errorf("store = %+v", got["store.jsonl"])
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"unknown module": "main {\n    tool nope\n}",
		"extra argument": "main {\n    policy ask now\n}",
		"unknown option": "main {\n    tool fs_read {\n        depth 3\n    }\n}",
		"twice":          "main {\n    policy ask\n}\nmain {\n    policy ask\n}",
		"no main":        "other {\n    policy ask\n}",
		"bad max_steps":  "main {\n    max_steps zero\n}",
		"bad max_depth":  "main {\n    max_depth deep\n}",
		"bad inject":     "main {\n    inject maybe\n}",
		"global rel":     "{\n    max_depth +1\n}\nmain {\n    policy ask\n}",
		"global inject":  "{\n    inject off\n}\nmain {\n    policy ask\n}",
		"missing name":   "main {\n    tool\n}",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := load(t, in); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestExampleLoads(t *testing.T) {
	t.Parallel()

	f, err := os.Open("../../../examples/Agentfile")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cfg, err := agentfile.Loader{}.Load("Agentfile", f)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := aish.Get[loop.Settings](cfg.Agents[0].Own); got.MaxSteps != 50 || len(build(t, cfg, "main")) != 15 {
		t.Fatalf("agents %+v, %d instances", cfg.Agents, len(build(t, cfg, "main")))
	}
}

func TestNamedInstances(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, `main {
    tool mcp github {
        command github-mcp-server stdio
    }
    tool mcp docs {
        url http://localhost:1/mcp
    }
}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(build(t, cfg, "main")), " "); got != "tool.mcp/github tool.mcp/docs" {
		t.Errorf("instances = %s", got)
	}
}

func TestGlobalBlock(t *testing.T) {
	cfg, err := load(t, "{\n log file /g.log {\n  level debug\n }\n store jsonl /g\n}\nmain {\n log file /a.log\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	insts := build(t, cfg, "main")
	if got := strings.Join(names(insts), ","); got != "store.jsonl,log.file" {
		t.Fatalf("instances = %s", got)
	}
	if l := insts[1].Module.(*file.Log); l.Path != "/a.log" || l.Level != slog.LevelInfo {
		t.Fatalf("agent log must replace the global one, got %+v", l)
	}
}

const agentsFile = `{
    max_depth 3
    max_steps 9
    store jsonl /g
}
main {
    description "the boss"
    policy ask
}
reviewer {
    description "reviews diffs"
    max_depth -1
    max_steps 4
    inject off
    policy cedar ./x.cedar
}
helper {
    max_depth 5
    policy ask
}
`

func TestAgentSpecs(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, agentsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		own, _ := aish.Get[core.Settings](a.Own)
		global, _ := aish.Get[core.Settings](a.Global)
		steps, _ := aish.Get[loop.Settings](a.Own)
		gsteps, _ := aish.Get[loop.Settings](a.Global)
		got = append(got, fmt.Sprintf("%s|%s|hidden=%t|depth=%+v|global=%d|steps=%d/%d", a.Name, own.Description, own.Hidden, own.Depth, global.Depth.N, steps.MaxSteps, gsteps.MaxSteps))
	}
	want := []string{
		"main|the boss|hidden=false|depth={Set:false Relative:false N:0}|global=3|steps=0/9",
		"reviewer|reviews diffs|hidden=true|depth={Set:true Relative:true N:-1}|global=3|steps=4/9",
		"helper||hidden=false|depth={Set:true Relative:false N:5}|global=3|steps=0/9",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("agents:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestBuildMakesFreshInstances(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, agentsFile)
	if err != nil {
		t.Fatal(err)
	}
	a, b := build(t, cfg, "helper"), build(t, cfg, "helper")
	if a[1].Module == b[1].Module {
		t.Fatal("two builds share a module instance")
	}
	if a[0].Name != "store.jsonl" {
		t.Errorf("the global store must come first, got %s", a[0].Name)
	}
	if _, err := cfg.Build("nobody"); err == nil {
		t.Error("Build of an unknown agent succeeded")
	}
}
