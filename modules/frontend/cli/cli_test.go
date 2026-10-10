package cli_test

import (
	"context"
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
	"github.com/GoldenDeals/aish-core/modules/tool/fs"
	"github.com/GoldenDeals/aish-core/test"
)

// output is a goroutine-safe buffer the test can poll.
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

func scenario(t *testing.T, answer string) (out, file string, llm *test.FakeLLM) {
	t.Helper()

	dir := t.TempDir()
	edit := json.RawMessage(`{"command":"write","path":"a.txt","content":"hi"}`)
	llm = &test.FakeLLM{Replies: []test.Reply{
		{Text: "writing", Calls: []events.ToolCall{{ID: "c1", Name: "fs_edit", Input: edit}}},
		{Text: "finished"},
	}}
	editor := &fs.Edit{}
	editor.Root = dir
	pr, pw := io.Pipe()
	o := &output{}
	front := &cli.CLI{In: pr, Out: o}
	s := test.NewSession(t,
		aish.Instance{Name: "agent.loop", Module: &loop.Loop{}},
		aish.Instance{Name: "llm.fake", Module: llm},
		aish.Instance{Name: "tool.fs_edit", Module: editor},
		aish.Instance{Name: "policy.ask", Module: &ask.Policy{}},
		aish.Instance{Name: "frontend.cli", Module: front},
	)

	if _, err := io.WriteString(pw, "write a file\n"); err != nil {
		t.Fatal(err)
	}
	o.waitFor(t, "Allow fs_edit")
	if _, err := io.WriteString(pw, answer+"\n"); err != nil {
		t.Fatal(err)
	}
	o.waitFor(t, "finished")
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	<-front.Done()
	s.Wait()

	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))

	return o.String(), string(data), llm
}

func TestApproved(t *testing.T) {
	t.Parallel()

	out, file, llm := scenario(t, "y")
	if file != "hi" || !strings.Contains(out, "[tool] fs_edit") || len(llm.Requests) != 2 {
		t.Fatalf("file %q, requests %d, output:\n%s", file, len(llm.Requests), out)
	}
}

func TestRefused(t *testing.T) {
	t.Parallel()

	out, file, llm := scenario(t, "n")
	if file != "" || !strings.Contains(out, "[denied] fs_edit") || len(llm.Requests) != 2 {
		t.Fatalf("file %q, requests %d, output:\n%s", file, len(llm.Requests), out)
	}
}

func TestInvalidAnswerAsksAgain(t *testing.T) {
	t.Parallel()

	// "maybe" matches no option, so the question is repeated and the input
	// "n" afterwards still decides.
	out, file, _ := scenario(t, "maybe\nn")
	if file != "" || strings.Count(out, "Allow fs_edit") != 2 {
		t.Fatalf("file %q, output:\n%s", file, out)
	}
}

// gatedLLM holds every llm.request until gate is closed, so a test can type
// while the agent is busy.
type gatedLLM struct {
	started chan struct{}
	gate    chan struct{}
}

func (g *gatedLLM) Init(_ context.Context, h *aish.Host) error {
	h.Handle("respond", func(ctx context.Context, _ events.LLMRequest) error {
		select {
		case g.started <- struct{}{}:
		default:
		}
		<-g.gate
		content, w := aish.NewStream[events.ContentDelta]()
		w.Write(events.ContentDelta{Type: events.DeltaText, Text: "ok"})
		w.Close(nil)
		err := h.Emit(ctx, events.AssistantMessage{Content: content})
		if errors.Is(err, aish.ErrUnhandled) {
			return nil
		}

		return err
	})

	return nil
}

func TestQueuedMessageIsMarked(t *testing.T) {
	t.Parallel()

	llm := &gatedLLM{started: make(chan struct{}, 1), gate: make(chan struct{})}
	pr, pw := io.Pipe()
	o := &output{}
	front := &cli.CLI{In: pr, Out: o}
	s := test.NewSession(t,
		aish.Instance{Name: "agent.loop", Module: &loop.Loop{}},
		aish.Instance{Name: "llm.gated", Module: llm},
		aish.Instance{Name: "frontend.cli", Module: front},
	)

	if _, err := io.WriteString(pw, "one\n"); err != nil {
		t.Fatal(err)
	}
	<-llm.started
	if _, err := io.WriteString(pw, "two\n"); err != nil {
		t.Fatal(err)
	}
	o.waitFor(t, "(queued")
	close(llm.gate)
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	<-front.Done()
	s.Wait()

	if !strings.HasPrefix(o.String(), "session ") {
		t.Errorf("output must start with the session ID:\n%s", o.String())
	}
}

func TestLoopStoppedIsShown(t *testing.T) {
	t.Parallel()

	reader := &fs.Read{}
	reader.Root = t.TempDir()
	llm := &test.FakeLLM{Replies: []test.Reply{
		{Calls: []events.ToolCall{{ID: "c1", Name: "fs_read", Input: json.RawMessage(`{"path":"x"}`)}}},
	}}
	pr, pw := io.Pipe()
	o := &output{}
	front := &cli.CLI{In: pr, Out: o}
	s := test.NewSession(t,
		aish.Instance{Name: "agent.loop", Module: &loop.Loop{MaxSteps: 1}},
		aish.Instance{Name: "llm.fake", Module: llm},
		aish.Instance{Name: "tool.fs_read", Module: reader},
		aish.Instance{Name: "frontend.cli", Module: front},
	)

	if _, err := io.WriteString(pw, "go\n"); err != nil {
		t.Fatal(err)
	}
	o.waitFor(t, "[stopped] max_steps")
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	<-front.Done()
	s.Wait()
}
