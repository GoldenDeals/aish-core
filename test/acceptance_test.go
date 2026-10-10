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
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/policy/ask"
	"github.com/GoldenDeals/aish-core/test"
	"github.com/GoldenDeals/aish-core/trace"
	"github.com/GoldenDeals/aish-core/transcript"
)

// The stage 0 acceptance scenario (§11): a policy intercepts tool.call, asks
// through a frontend and re-emits the call; without a frontend the call is
// denied. The tool below is a test double of tool fs_edit (§8).

func textBlocks(s string) []events.Block { return []events.Block{{Type: events.BlockText, Text: s}} }

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

func isFS(c events.ToolCall) bool { return strings.HasPrefix(c.Name, "fs_") }

func (f *fsTool) Init(_ context.Context, h *aish.Host) error {
	h.Mutate("effects", func(_ context.Context, m *aish.Mut[events.ToolCall]) error {
		c := m.Event()
		if c.Name == "fs_read" {
			c.Effects = events.Effects{ReadOnly: true, Access: []events.Access{{Action: "fs.read", Resource: "file:/a.txt"}}}
		} else {
			c.Effects = events.Effects{Access: []events.Access{{Action: "fs.write", Resource: "file:/a.txt"}}}
		}

		return nil
	}, aish.Priority(2000), aish.Where(isFS))
	h.Handle("exec", func(ctx context.Context, c events.ToolCall) error {
		f.ran = append(f.ran, c.ID)

		return ignoreUnhandled(h.Emit(ctx, events.ToolResult{CallID: c.ID, Content: textBlocks("done")}))
	}, aish.Where(isFS))

	return nil
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
				{Name: "policy.test", Module: &ask.Policy{}},
			}
			if tc.frontend != nil {
				instances = append(instances, aish.Instance{Name: "frontend.test", Module: tc.frontend})
			}
			s := test.NewSession(t, instances...)

			call := events.ToolCall{ID: "toolu_1", Name: tc.call, Input: json.RawMessage(`{"path":"a.txt"}`)}
			if err := s.Emit(call); err != nil {
				t.Fatal(err)
			}
			s.Wait()
			tx := s.Last()

			if got := len(tool.ran) == 1; got != tc.ran {
				t.Errorf("tool ran %v times, want ran=%v", tool.ran, tc.ran)
			}
			var journaled events.ToolCall
			var res events.ToolResult
			for _, env := range tx.Events {
				switch ev := env.Event.(type) {
				case events.ToolCall:
					journaled = ev
				case events.ToolResult:
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
