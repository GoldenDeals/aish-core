package test_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/agent/loop"
	"github.com/GoldenDeals/aish-core/modules/frontend/cli"
	"github.com/GoldenDeals/aish-core/modules/policy/ask"
	"github.com/GoldenDeals/aish-core/modules/store/jsonl"
	"github.com/GoldenDeals/aish-core/modules/tool/fs"
	"github.com/GoldenDeals/aish-core/test"
)

// The stage 1 acceptance scenario (§11): the agent reads a file without
// asking, edits it after the user's answer, and a resumed session restores the
// conversation without running the tools again. The conformance test (§10)
// runs the same scenario with frontend.cli and with test.Frontend.

type output struct {
	mu sync.Mutex
	sb strings.Builder
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.sb.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.sb.String()
}

func (o *output) waitFor(t *testing.T, want string) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if strings.Contains(o.String(), want) {
			return
		}
	}
	t.Fatalf("output never contained %q:\n%s", want, o.String())
}

// frontend is a frontend under test: how to add it to a session, how it takes
// the user's message, and how it answers the one question.
type frontend struct {
	name     string
	instance func(t *testing.T, answer string) (aish.Instance, *driver)
}

// driver sends the user's text and answers the question, for one frontend.
type driver struct {
	say   func(s *test.Session, text string) error
	reply func(t *testing.T) // answers the question once it has been asked
	asked func() int
	done  func(t *testing.T)
}

func emitMessage(s *test.Session, text string) error {
	err := s.Emit(events.UserMessage{Text: text})
	if errors.Is(err, aish.ErrUnhandled) {
		return nil
	}

	return err
}

var frontends = []frontend{
	{name: "test.Frontend", instance: func(_ *testing.T, answer string) (aish.Instance, *driver) {
		f := &test.Frontend{Answers: []string{answer}}

		return aish.Instance{Name: "frontend.test", Module: f}, &driver{
			say:   emitMessage,
			reply: func(*testing.T) {},
			asked: func() int { return len(f.Asked) },
			done:  func(*testing.T) {},
		}
	}},
	{name: "frontend.cli", instance: func(_ *testing.T, answer string) (aish.Instance, *driver) {
		pr, pw := io.Pipe()
		out := &output{}
		c := &cli.CLI{In: pr, Out: out}

		return aish.Instance{Name: "frontend.cli", Module: c}, &driver{
			say: func(_ *test.Session, text string) error {
				_, err := io.WriteString(pw, text+"\n")

				return err
			},
			reply: func(t *testing.T) {
				t.Helper()
				out.waitFor(t, "Allow fs_edit")
				if _, err := io.WriteString(pw, answer+"\n"); err != nil {
					t.Fatal(err)
				}
			},
			asked: func() int { return strings.Count(out.String(), "Allow fs_edit") },
			done: func(t *testing.T) {
				t.Helper()
				out.waitFor(t, "all done")
				if err := pw.Close(); err != nil {
					t.Fatal(err)
				}
				<-c.Done()
			},
		}
	}},
}

func edit() json.RawMessage {
	return json.RawMessage(`{"command":"str_replace","path":"a.txt","old_string":"hello","new_string":"world"}`)
}

func llmScript() *test.FakeLLM {
	return &test.FakeLLM{Replies: []test.Reply{
		{Text: "reading", Calls: []events.ToolCall{{ID: "c1", Name: "fs_read", Input: json.RawMessage(`{"path":"a.txt"}`)}}},
		{Text: "editing", Calls: []events.ToolCall{{ID: "c2", Name: "fs_edit", Input: edit()}}},
		{Text: "all done"},
	}}
}

// session wires the stage 1 modules around fe. Every call builds fresh module
// instances, as a restart does.
func session(t *testing.T, id, resume, dir, files string, llm *test.FakeLLM, fe aish.Instance) *test.Session {
	t.Helper()

	return sessionWith(t, aish.Instance{Name: "policy.ask", Module: &ask.Policy{}}, id, resume, dir, files, llm, fe)
}

func sessionWith(t *testing.T, policy aish.Instance, id, resume, dir, files string, llm *test.FakeLLM, fe aish.Instance) *test.Session {
	t.Helper()

	reader, editor := &fs.Read{}, &fs.Edit{}
	reader.Root, editor.Root = files, files

	return test.NewSessionWith(t, aish.SessionConfig{
		ID:      id,
		Options: aish.Options{Resume: resume},
		Instances: []aish.Instance{
			{Name: "agent.loop", Module: &loop.Loop{}},
			{Name: "llm.fake", Module: llm},
			{Name: "tool.fs_read", Module: reader},
			{Name: "tool.fs_edit", Module: editor},
			policy,
			{Name: "store.jsonl", Module: &jsonl.Store{Dir: dir}},
			fe,
		},
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func TestStage1Conformance(t *testing.T) {
	t.Parallel()

	for _, fe := range frontends {
		for _, tc := range []struct{ name, answer, want string }{
			{"approved", "yes", "world"},
			{"refused", "no", "hello"},
		} {
			t.Run(fe.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				files, store := t.TempDir(), t.TempDir()
				path := filepath.Join(files, "a.txt")
				if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
					t.Fatal(err)
				}
				llm := llmScript()
				inst, drv := fe.instance(t, tc.answer)
				s := session(t, "s1", "", store, files, llm, inst)

				if err := drv.say(s, "fix the greeting"); err != nil {
					t.Fatal(err)
				}
				drv.reply(t)
				drv.done(t)
				s.Wait()

				if got := readFile(t, path); got != tc.want {
					t.Errorf("file = %q, want %q", got, tc.want)
				}
				if drv.asked() != 1 {
					t.Errorf("user was asked %d times, want once: the read needs no question", drv.asked())
				}
				if len(llm.Requests) != 3 {
					t.Errorf("model was called %d times, want 3", len(llm.Requests))
				}
			})
		}
	}
}

func TestStage1Resume(t *testing.T) {
	t.Parallel()

	files, store := t.TempDir(), t.TempDir()
	path := filepath.Join(files, "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := &test.Frontend{Answers: []string{"yes"}}
	s1 := session(t, "s1", "", store, files, llmScript(), aish.Instance{Name: "frontend.test", Module: first})
	if err := emitMessage(s1, "fix the greeting"); err != nil {
		t.Fatal(err)
	}
	s1.Wait()
	if err := s1.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A tool that ran again would overwrite or fail on this file.
	if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := &test.Frontend{}
	llm := &test.FakeLLM{Replies: []test.Reply{{Text: "welcome back"}}}
	s2 := session(t, "s1", "s1", store, files, llm, aish.Instance{Name: "frontend.test", Module: second})
	if err := emitMessage(s2, "and now?"); err != nil {
		t.Fatal(err)
	}
	s2.Wait()

	if got := readFile(t, path); got != "sentinel" {
		t.Errorf("file = %q: a restored tool call ran again", got)
	}
	if len(second.Asked) != 0 {
		t.Errorf("resumed session asked %d questions", len(second.Asked))
	}
	if len(llm.Requests) != 1 {
		t.Fatalf("model was called %d times, want 1", len(llm.Requests))
	}
	var results int
	msgs := llm.Requests[0].Messages
	for i := range msgs {
		for _, b := range msgs[i].Content {
			if b.Type == events.BlockToolResult {
				results++
			}
		}
	}
	if results != 2 || msgs[len(msgs)-1].Content[0].Text != "and now?" {
		t.Errorf("restored history has %d tool results, last message %+v", results, msgs[len(msgs)-1])
	}
}
