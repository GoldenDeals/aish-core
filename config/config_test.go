package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/GoldenDeals/aish-core/config"
	_ "github.com/GoldenDeals/aish-core/modules/standard"
	"github.com/GoldenDeals/aish-core/modules/store/jsonl"
	"github.com/GoldenDeals/aish-core/modules/tool/fs"
)

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

	cfg, err := config.Load("Agentfile", strings.NewReader(caddyfile))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Options.MaxSteps != 7 {
		t.Errorf("max steps = %d", cfg.Options.MaxSteps)
	}
	got := map[string]any{}
	for _, inst := range cfg.Instances {
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
		"subagent block": "main {\n    policy ask\n}\nreviewer {\n    policy ask\n}",
		"no main":        "other {\n    policy ask\n}",
		"bad max_steps":  "main {\n    max_steps zero\n}",
		"missing name":   "main {\n    tool\n}",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := config.Load("Agentfile", strings.NewReader(in)); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestExampleLoads(t *testing.T) {
	t.Parallel()

	f, err := os.Open("../examples/Agentfile")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cfg, err := config.Load("Agentfile", f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Options.MaxSteps != 50 || len(cfg.Instances) != 10 {
		t.Fatalf("options %+v, %d instances", cfg.Options, len(cfg.Instances))
	}
}

func TestNamedInstances(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load("Agentfile", strings.NewReader(`main {
    tool mcp github {
        command github-mcp-server stdio
    }
    tool mcp docs {
        url http://localhost:1/mcp
    }
}`))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cfg.Instances))
	for _, inst := range cfg.Instances {
		names = append(names, inst.Name)
	}
	if got := strings.Join(names, " "); got != "tool.mcp/github tool.mcp/docs" {
		t.Errorf("instances = %s", got)
	}
}
