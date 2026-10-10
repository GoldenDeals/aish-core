package aish_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/modules/agent/loop"
	"github.com/GoldenDeals/aish-core/modules/llm/anthropic"
	"github.com/GoldenDeals/aish-core/modules/log/file"
	_ "github.com/GoldenDeals/aish-core/modules/standard"
	"github.com/GoldenDeals/aish-core/modules/store/jsonl"
	"github.com/GoldenDeals/aish-core/modules/subagents/core"
	"github.com/GoldenDeals/aish-core/modules/tool/fs"
	"github.com/GoldenDeals/aish-core/modules/tool/mcp"
)

func load(t testing.TB, src string) (*aish.Config, error) {
	t.Helper()

	return aish.LoadConfig("Agentfile", strings.NewReader(src))
}

func build(t testing.TB, cfg *aish.Config, agent string) []aish.Instance {
	t.Helper()

	insts, err := cfg.Build(agent)
	if err != nil {
		t.Fatal(err)
	}

	return insts
}

func names(insts []aish.Instance) string {
	out := make([]string, len(insts))
	for i, inst := range insts {
		out[i] = inst.Name
	}

	return strings.Join(out, " ")
}

func TestLoadRoot(t *testing.T) {
	t.Setenv("TEST_AISH_STORE", "/var/aish")

	cfg, err := load(t, `
(files) {
    tool fs_read
    tool fs_edit {
        root /work
    }
}
{
    max_steps 7
    agent loop
    frontend cli
    import files
    policy ask
    store jsonl {$TEST_AISH_STORE}
}`)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := aish.Get[loop.Settings](cfg.Agents[0].Values); cfg.Agents[0].Name != "main" || got.MaxSteps != 7 {
		t.Errorf("root = %+v, steps %+v", cfg.Agents[0], got)
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
		"no root":            "reviewer {\n    policy ask\n}",
		"two roots":          "{\n    policy ask\n}\n{\n    policy ask\n}",
		"unknown module":     "{\n    tool nope\n}",
		"extra argument":     "{\n    policy ask now\n}",
		"unknown option":     "{\n    tool fs_read {\n        depth 3\n    }\n}",
		"missing name":       "{\n    tool\n}",
		"bad max_steps":      "{\n    agent loop\n    max_steps zero\n}",
		"directive no owner": "{\n    max_steps 5\n}",
		"block no owner":     "{\n    agent loop\n}\nreviewer {\n    agent loop\n}",
		"bad max_depth":      "{\n    subagents core\n    max_depth deep\n}",
		"bad inject":         "{\n    subagents core\n}\nr {\n    inject maybe\n}",
		"inject root":        "{\n    subagents core\n    inject off\n}",
		"main block":         "{\n    subagents core\n}\nmain {\n    policy ask\n}",
		"repeated block":     "{\n    subagents core\n}\nr {\n    policy ask\n}\nr {\n    policy ask\n}",
		"bad policy":         "{\n    subagents core\n}\nr sticky {\n    policy ask\n}",
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

	f, err := os.Open("examples/Agentfile")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cfg, err := aish.LoadConfig("examples/Agentfile", f)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := aish.Get[loop.Settings](cfg.Agents[0].Values); got.MaxSteps != 50 || len(cfg.Agents) != 6 || cfg.Agents[1].Name != "reviewer" {
		t.Fatalf("agents %+v", cfg.Agents)
	}
	if got := len(build(t, cfg, "main")); got != 16 {
		t.Errorf("main has %d instances: %s", got, names(build(t, cfg, "main")))
	}
}

func TestNamedInstances(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, `{
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
	if got := names(build(t, cfg, "main")); got != "tool.mcp/github tool.mcp/docs" {
		t.Errorf("instances = %s", got)
	}
}

const agentsFile = `{
    max_depth 3
    max_steps 9
    agent loop
    subagents core
    store jsonl /g
    log file /g.log {
        level debug
    }
    policy ask
}
reviewer {
    description "reviews diffs"
    max_depth -1
    max_steps 4
    log file /a.log
    tool fs_read
}
helper clean {
    max_depth 5
    inject off
    policy cedar ./x.cedar
}
`

func TestAgentSpecs(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, agentsFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 3 {
		t.Fatalf("agents %+v", cfg.Agents)
	}
	root, reviewer, helper := cfg.Agents[0], cfg.Agents[1], cfg.Agents[2]

	if s, _ := aish.Get[core.Settings](root.Values); s.MaxDepth != 3 {
		t.Errorf("root settings %+v", s)
	}
	s, _ := aish.Get[core.Settings](reviewer.Values)
	steps, _ := aish.Get[loop.Settings](reviewer.Values)
	if reviewer.Owner != "subagents.core" || s.Description != "reviews diffs" || s.MaxDepth != 2 || steps.MaxSteps != 4 {
		t.Errorf("merged reviewer %+v, %+v, %+v", reviewer, s, steps)
	}
	s, _ = aish.Get[core.Settings](helper.Values)
	steps, _ = aish.Get[loop.Settings](helper.Values)
	if s.MaxDepth != 5 || !s.Hidden || steps.MaxSteps != 0 {
		t.Errorf("clean helper %+v, %+v", s, steps)
	}
}

func TestMergeAndClean(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, agentsFile)
	if err != nil {
		t.Fatal(err)
	}
	// The root's frontend-like modules stay behind; its own log is replaced.
	merged := build(t, cfg, "reviewer")
	if got := names(merged); got != "agent.loop subagents.core policy.ask log.file tool.fs_read" {
		t.Errorf("merged = %s", got)
	}
	if l := merged[3].Module.(*file.Log); l.Path != "/a.log" {
		t.Errorf("block log must replace the root's, got %+v", l)
	}
	if got := names(build(t, cfg, "helper")); got != "policy.cedar" {
		t.Errorf("clean = %s", got)
	}
	if got := names(build(t, cfg, "main")); got != "agent.loop subagents.core store.jsonl log.file policy.ask" {
		t.Errorf("root = %s", got)
	}
	if _, err := cfg.Build("nobody"); err == nil {
		t.Error("Build of an unknown agent succeeded")
	}
}

func TestBuildMakesFreshInstances(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, agentsFile)
	if err != nil {
		t.Fatal(err)
	}
	a, b := build(t, cfg, "reviewer"), build(t, cfg, "reviewer")
	if a[0].Module == b[0].Module {
		t.Fatal("two builds share a module instance")
	}
}

// ext is a module of another package that adds a directive and a block kind,
// to show that nothing about them is known to the core.
type ext struct{}

func (ext) Init(context.Context, *aish.Host) error { return nil }

func (ext) ExtendConfig(c *aish.ConfigExt) {
	c.Directive("flavor", func(d *caddyfile.Dispenser, v *aish.Values, _ aish.Section) error {
		var f string
		if !d.Args(&f) {
			return d.ArgErr()
		}
		aish.Set(v, flavor(f))

		return nil
	})
	c.Block(func(b *aish.Block) (bool, error) {
		if len(b.Flags) > 0 && b.Flags[0] == "plugin" {
			return true, b.Declare(false)
		}

		return false, nil
	})
}

type flavor string

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "cfgtest.ext", New: func() aish.Module { return ext{} }})
}

func TestAnyModuleExtendsConfig(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, "{\n    cfgtest ext\n    flavor mint\n}\np plugin {\n    flavor lemon\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := aish.Get[flavor](cfg.Agents[0].Values); !ok || f != "mint" {
		t.Errorf("root flavor %q", f)
	}
	if f, ok := aish.Get[flavor](cfg.Agents[1].Values); !ok || f != "lemon" || cfg.Agents[1].Owner != "cfgtest.ext" {
		t.Errorf("block %+v", cfg.Agents[1])
	}
	if _, err := load(t, "{\n    flavor mint\n}\n"); err == nil {
		t.Error("the directive works without its module")
	}
	if _, err := load(t, "{\n    cfgtest ext\n}\nq other {\n}\n"); err == nil {
		t.Error("a block nobody accepts must fail")
	}
}

func TestOneLineOptions(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, "{\n    llm anthropic model claude-x max_tokens 7 {\n        base_url http://h\n    }\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	insts := build(t, cfg, "main")
	a, ok := insts[0].Module.(*anthropic.Anthropic)
	if !ok || a.Model != "claude-x" || a.MaxTokens != 7 || a.BaseURL != "http://h" {
		t.Errorf("llm = %+v", a)
	}
	for _, in := range []string{"llm anthropic model", "llm anthropic nope 1", "llm anthropic max_tokens zero"} {
		if _, err := load(t, "{\n    "+in+"\n}\n"); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestOneLineOptionsOfEveryModule(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, `{
    tool fs_edit root /work
    tool fs_read /r
    store jsonl /s rotate 24h
    log file /l.log level debug rotate 1h
    log trace rotate 2h
    context system_prompt text hello
    context agents_md AGENTS.md home local pattern CLAUDE.md
    context append a.md text hi path b.md
    tool mcp exa url http://h
    tool mcp git command git-mcp stdio
}`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, inst := range build(t, cfg, "main") {
		got[inst.Name] = inst.Module
	}
	if e, _ := got["tool.fs_edit"].(*fs.Edit); e == nil || e.Root != "/work" {
		t.Errorf("fs_edit = %+v", got["tool.fs_edit"])
	}
	if r, _ := got["tool.fs_read"].(*fs.Read); r == nil || r.Root != "/r" {
		t.Errorf("fs_read = %+v", got["tool.fs_read"])
	}
	if st, _ := got["store.jsonl"].(*jsonl.Store); st == nil || st.Dir != "/s" || st.Rotate != 24*time.Hour {
		t.Errorf("store = %+v", got["store.jsonl"])
	}
	if l, _ := got["log.file"].(*file.Log); l == nil || l.Path != "/l.log" || l.Level != slog.LevelDebug || l.Rotate != time.Hour {
		t.Errorf("log = %+v", got["log.file"])
	}
	if m, _ := got["tool.mcp/git"].(*mcp.MCP); m == nil || strings.Join(m.Command, " ") != "git-mcp stdio" {
		t.Errorf("mcp = %+v", got["tool.mcp/git"])
	}
	if _, err := load(t, "{\n    store jsonl /a /b\n}"); err == nil {
		t.Error("two positional arguments accepted")
	}
	if _, err := load(t, "{\n    log file level\n}"); err == nil {
		t.Error("an option without a value accepted")
	}
}

func TestShortForms(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, `(base) {
    subagents core communication
}
{
    import base
    tool {
        fs_read
        fs_edit root /w

        mcp exa url http://h
    }
    context {
        agents_md AGENTS.md home pattern CLAUDE.md
        skills ./a ./b
    }
    log {
        file /l.log level debug {
            rotate 1h
        }
        trace
    }
    description "the boss"
}
helper {
    description "helps"
}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := "subagents.core subagents.communication tool.fs_read tool.fs_edit tool.mcp/exa context.agents_md context.skills log.file log.trace"
	if got := names(build(t, cfg, "main")); got != want {
		t.Errorf("main = %s", got)
	}
	// The description belongs to the root; the merging block has its own.
	root, _ := aish.Get[core.Settings](cfg.Agents[0].Values)
	helper, _ := aish.Get[core.Settings](cfg.Agents[1].Values)
	if root.Description != "the boss" || helper.Description != "helps" {
		t.Errorf("descriptions %q, %q", root.Description, helper.Description)
	}
	if _, err := load(t, "{\n    tool {\n        fs_read\n        nope\n    }\n}"); err == nil {
		t.Error("an unknown module in a namespace block accepted")
	}
}

func TestRepeatedModuleMergesOptions(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, `{
    llm anthropic api_key k model m1
    llm anthropic model m2
    subagents core
}
helper {
    llm anthropic max_tokens 5
}
`)
	if err != nil {
		t.Fatal(err)
	}
	root := build(t, cfg, "main")
	helper := build(t, cfg, "helper")
	a, ok1 := root[0].Module.(*anthropic.Anthropic)
	h, ok2 := helper[0].Module.(*anthropic.Anthropic)
	if len(root) != 2 || !ok1 || a.APIKey != "k" || a.Model != "m2" {
		t.Errorf("root llm = %+v (%d instances)", a, len(root))
	}
	if !ok2 || h.APIKey != "k" || h.Model != "m2" || h.MaxTokens != 5 || h == a {
		t.Errorf("helper llm = %+v", h)
	}
}

func TestPragmaDeclaresAModuleOnce(t *testing.T) {
	t.Parallel()

	cfg, err := load(t, `(shared) {
    agent loop
    pragma frontend cli
    pragma store jsonl /s
    pragma tool {
        mcp exa url http://h
    }
    pragma subagents core communication
}
{
    import shared
}
reviewer clean {
    import shared
    tool fs_read
}
helper {
    import shared
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(build(t, cfg, "main")); got != "agent.loop frontend.cli store.jsonl tool.mcp/exa subagents.core subagents.communication" {
		t.Errorf("main = %s", got)
	}
	for _, agent := range []string{"reviewer", "helper"} {
		// The root declared the pragma modules first, so the agents do not; a
		// merging one still inherits them, except the frontend and the store.
		want := "agent.loop tool.fs_read"
		if agent == "helper" {
			want = "agent.loop tool.mcp/exa subagents.core subagents.communication"
		}
		if got := names(build(t, cfg, agent)); got != want {
			t.Errorf("%s = %s, want %s", agent, got, want)
		}
	}
	if _, err := load(t, "{\n    pragma max_steps 3\n}\n"); err == nil {
		t.Error("pragma on a plain directive accepted")
	}
}
