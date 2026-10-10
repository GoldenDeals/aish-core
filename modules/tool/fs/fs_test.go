package fs_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/tool/fs"
	"github.com/GoldenDeals/aish-core/test"
)

// run calls tool name with input in a session and returns the call and result
// as the journal holds them.
func run(t *testing.T, root, name, input string) (events.ToolCall, events.ToolResult) {
	t.Helper()

	rd, ed := &fs.Read{}, &fs.Edit{}
	rd.Root, ed.Root = root, root
	s := test.NewSession(t, aish.Instance{Name: "tool.fs_read", Module: rd}, aish.Instance{Name: "tool.fs_edit", Module: ed})

	if err := s.Emit(events.ToolCall{ID: "c1", Name: name, Input: json.RawMessage(input)}); err != nil {
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

func TestRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "one\ntwo\nthree\n")
	write(t, filepath.Join(root, "bin"), "a\x00b")

	for _, tc := range []struct {
		name, input, want string
		isErr             bool
	}{
		{"whole", `{"path":"a.txt"}`, "one\ntwo\nthree\n", false},
		{"range", `{"path":"a.txt","offset":2,"limit":1}`, "two\n\n[lines 2-2 of 3]", false},
		{"binary", `{"path":"bin"}`, "binary", true},
		{"escape", `{"path":"../x"}`, "outside the root", true},
		{"absolute_escape", `{"path":"/etc/passwd"}`, "outside the root", true},
		{"missing", `{"path":"nope"}`, "nope", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			call, res := run(t, root, "fs_read", tc.input)
			if res.IsError != tc.isErr || !strings.Contains(res.Content[0].Text, tc.want) {
				t.Fatalf("result = %+v, want error=%v containing %q", res, tc.isErr, tc.want)
			}
			if !call.Effects.ReadOnly || len(call.Effects.Access) != 1 || call.Effects.Access[0].Action != "fs.read" {
				t.Fatalf("effects = %+v", call.Effects)
			}
		})
	}
}

func TestEdit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "dup.txt"), "x x")
	write(t, filepath.Join(root, "f.txt"), "hello world")

	call, res := run(t, root, "fs_edit", `{"command":"write","path":"sub/new.txt","content":"hi"}`)
	if res.IsError || read(t, filepath.Join(root, "sub", "new.txt")) != "hi" {
		t.Fatalf("write failed: %+v", res)
	}
	if call.Effects.ReadOnly || call.Effects.Access[0].Action != "fs.write" {
		t.Fatalf("effects = %+v", call.Effects)
	}

	_, res = run(t, root, "fs_edit", `{"command":"str_replace","path":"f.txt","old_string":"world","new_string":"go"}`)
	if res.IsError || read(t, filepath.Join(root, "f.txt")) != "hello go" {
		t.Fatalf("replace failed: %+v", res)
	}

	_, res = run(t, root, "fs_edit", `{"command":"str_replace","path":"dup.txt","old_string":"x","new_string":"y"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "2 times") || read(t, filepath.Join(root, "dup.txt")) != "x x" {
		t.Fatalf("ambiguous replace: %+v", res)
	}

	_, res = run(t, root, "fs_edit", `{"command":"write","path":"../out.txt","content":"no"}`)
	if !res.IsError {
		t.Fatalf("write outside root succeeded: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "out.txt")); err == nil {
		t.Fatal("file written outside the root")
	}
}

func TestSymlinkEscape(t *testing.T) {
	t.Parallel()

	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "secret"), "s")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	_, res := run(t, root, "fs_read", `{"path":"link/secret"}`)
	if !res.IsError {
		t.Fatalf("read through symlink succeeded: %+v", res)
	}
}

func TestAdvertise(t *testing.T) {
	t.Parallel()

	rd := &fs.Read{}
	s := test.NewSession(t, aish.Instance{Name: "tool.fs_read", Module: rd})
	_ = s.Emit(events.LLMRequest{Provider: "x"})
	s.Wait()

	for _, env := range s.Last().Events {
		if r, ok := env.Event.(events.LLMRequest); ok && len(r.Tools) == 1 && r.Tools[0].Name == "fs_read" {
			return
		}
	}
	t.Fatal("fs_read not advertised in llm.request")
}

func write(t *testing.T, path, data string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func TestReadHugeLimitAndLargeFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "one\ntwo\nthree\n")
	write(t, filepath.Join(root, "big.txt"), strings.Repeat("x", 1<<20+10))

	_, res := run(t, root, "fs_read", `{"path":"a.txt","offset":2,"limit":9223372036854775807}`)
	if res.IsError || !strings.HasPrefix(res.Content[0].Text, "two\nthree\n") {
		t.Errorf("huge limit: %+v", res)
	}
	_, res = run(t, root, "fs_read", `{"path":"big.txt"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "limit") {
		t.Errorf("large file: %+v", res.Content[0].Text[:min(80, len(res.Content[0].Text))])
	}
}
