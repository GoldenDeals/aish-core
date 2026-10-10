package test_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/test"
	"github.com/GoldenDeals/aish-core/trace"
	"github.com/GoldenDeals/aish-core/transcript"
)

// The stage 0 acceptance scenario (§11): a policy intercepts tool.call, asks
// through a frontend and re-emits the call; without a frontend the call is
// denied. The policy and the tool below are test doubles of policy ask and
// tool fs_edit (§8).

func textBlocks(s string) []aish.Block { return []aish.Block{{Type: aish.BlockText, Text: s}} }

func ignoreUnhandled(err error) error {
	if errors.Is(err, aish.ErrUnhandled) {
		return nil
	}

	return err
}

// fsTool describes its calls at priority 2000 and runs them as an executor.
type fsTool struct {
	ran []string
}

func isFS(c aish.ToolCall) bool { return strings.HasPrefix(c.Name, "fs_") }

func (f *fsTool) Init(_ context.Context, h *aish.Host) error {
	h.Mutate("effects", func(_ context.Context, m *aish.Mut[aish.ToolCall]) error {
		c := m.Event()
		if c.Name == "fs_read" {
			c.Effects = aish.Effects{ReadOnly: true, Access: []aish.Access{{Action: "fs.read", Resource: "file:/a.txt"}}}
		} else {
			c.Effects = aish.Effects{Access: []aish.Access{{Action: "fs.write", Resource: "file:/a.txt"}}}
		}

		return nil
	}, aish.Priority(2000), aish.Where(isFS))
	h.Handle("exec", func(ctx context.Context, c aish.ToolCall) error {
		f.ran = append(f.ran, c.ID)

		return ignoreUnhandled(h.Emit(ctx, aish.ToolResult{CallID: c.ID, Content: textBlocks("done")}))
	}, aish.Where(isFS))

	return nil
}

// askPolicy lets read-only calls through and asks about the rest.
type askPolicy struct {
	h       *aish.Host
	waiting map[string]aish.Envelope // by question ID, which is the call's Meta.ID
}

func (p *askPolicy) Init(_ context.Context, h *aish.Host) error {
	p.h, p.waiting = h, map[string]aish.Envelope{}
	h.Mutate("authorize", p.authorize, aish.Priority(1000))
	h.Handle("answer", p.answer)

	return nil
}

func (p *askPolicy) authorize(ctx context.Context, m *aish.Mut[aish.ToolCall]) error {
	if m.Event().Effects.ReadOnly {
		return nil
	}
	m.Remove()
	call := aish.Envelope{Meta: m.Meta(), Event: *m.Event()}
	p.waiting[call.Meta.ID] = call

	err := p.h.Emit(ctx, aish.UserQuestion{
		ID: call.Meta.ID, Text: "Allow " + m.Event().Name + "?", Options: []string{"yes", "no"}, Ref: call.Meta.ID,
	})
	if errors.Is(err, aish.ErrUnhandled) {
		delete(p.waiting, call.Meta.ID)

		return p.deny(ctx, call, "nobody to ask")
	}

	return err
}

func (p *askPolicy) answer(ctx context.Context, a aish.UserAnswer) error {
	call, ok := p.waiting[a.QuestionID]
	if !ok {
		return nil
	}
	delete(p.waiting, a.QuestionID)
	if a.Choice == "yes" {
		return p.h.EmitWith(ctx, aish.Below(1000), call)
	}

	return p.deny(ctx, call, "the user said no")
}

// deny records the call as denied below executors and answers the model.
func (p *askPolicy) deny(ctx context.Context, call aish.Envelope, reason string) error {
	c, _ := call.Event.(aish.ToolCall)
	c.Denied = true

	return errors.Join(
		ignoreUnhandled(p.h.EmitWith(ctx, aish.Below(aish.DefaultPriority), aish.Envelope{Meta: call.Meta, Event: c})),
		ignoreUnhandled(p.h.Emit(ctx, aish.ToolResult{CallID: c.ID, IsError: true, Content: textBlocks("Tool call denied: " + reason)})),
	)
}

func TestStage0(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		call     string
		frontend *test.Frontend // nil: no frontend in the session
		ran      bool
		result   string
		denied   bool
	}{
		{name: "approved", call: "fs_write", frontend: &test.Frontend{Answers: []string{"yes"}}, ran: true, result: "done"},
		{name: "refused", call: "fs_write", frontend: &test.Frontend{Answers: []string{"no"}}, result: "Tool call denied: the user said no", denied: true},
		{name: "nobody_to_ask", call: "fs_write", result: "Tool call denied: nobody to ask", denied: true},
		{name: "read_only", call: "fs_read", ran: true, result: "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tool := &fsTool{}
			instances := []aish.Instance{
				{Name: "tool.fs", Module: tool},
				{Name: "policy.test", Module: &askPolicy{}},
			}
			if tc.frontend != nil {
				instances = append(instances, aish.Instance{Name: "frontend.test", Module: tc.frontend})
			}
			s := test.NewSession(t, instances...)

			call := aish.ToolCall{ID: "toolu_1", Name: tc.call, Input: json.RawMessage(`{"path":"a.txt"}`)}
			if err := s.Emit(call); err != nil {
				t.Fatal(err)
			}
			s.Wait()
			tx := s.Last()

			if got := len(tool.ran) == 1; got != tc.ran {
				t.Errorf("tool ran %v times, want ran=%v", tool.ran, tc.ran)
			}
			var journaled aish.ToolCall
			var res aish.ToolResult
			for _, env := range tx.Events {
				switch ev := env.Event.(type) {
				case aish.ToolCall:
					journaled = ev
				case aish.ToolResult:
					res = ev
				default:
				}
			}
			if journaled.ID != "toolu_1" || journaled.Denied != tc.denied {
				t.Errorf("journaled call = %+v, want toolu_1 with Denied=%v", journaled, tc.denied)
			}
			if res.CallID != "toolu_1" || res.Content[0].Text != tc.result || res.IsError != tc.denied {
				t.Errorf("result = %+v, want %q", res, tc.result)
			}
			if tc.frontend != nil && tc.denied && len(tc.frontend.Denied) != 1 {
				t.Errorf("frontend was shown %d denied calls, want 1", len(tc.frontend.Denied))
			}

			// The model sees the call next to its result, denied or not.
			msgs := transcript.Build(s.Journal().Txs())
			if len(msgs) != 2 || msgs[0].Content[0].ID != "toolu_1" || msgs[1].Content[0].ToolUseID != "toolu_1" {
				t.Errorf("transcript = %+v, want tool_use and tool_result of toolu_1", msgs)
			}

			checkGolden(t, tc.name, tx)
		})
	}
}

// checkGolden compares the trace of tx with testdata/<name>.trace. Set
// AISH_UPDATE_GOLDEN=1 to rewrite the files.
func checkGolden(t *testing.T, name string, tx aish.Tx) {
	t.Helper()

	for i := range tx.Trace {
		tx.Trace[i].Dur = 0
	}
	var got bytes.Buffer
	if err := trace.Write(&got, tx); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join("testdata", name+".trace")
	if os.Getenv("AISH_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}

		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with AISH_UPDATE_GOLDEN=1 to create it)", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("trace differs from %s:\n%s", path, got.String())
	}
}
