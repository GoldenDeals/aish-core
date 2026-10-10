// Package bash provides the tool bash, which runs a command with "bash -c" in
// the working directory. It does not sandbox the command: that is the job of
// the policy.
package bash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	describePriority = 2000
	// toolsPriority is the enrichment band (§4.4).
	toolsPriority  = 500
	name           = "bash"
	defaultTimeout = 120 * time.Second
	maxOutputBytes = 1 << 20
	// killDelay bounds the wait for output pipes held open by orphaned children.
	killDelay = 2 * time.Second
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "tool.bash", New: func() aish.Module { return &Bash{} }})
}

// Bash is the module tool.bash.
type Bash struct {
	spec events.ToolSpec
}

type input struct {
	Command string
	Timeout int // seconds
}

func (b *Bash) Init(_ context.Context, h *aish.Host) error {
	b.spec = events.ToolSpec{
		Name: name,
		Description: "Run a command with bash -c in the working directory and return stdout and stderr combined. " +
			"Each call starts a new shell, so cd and exports do not persist.",
		Schema: json.RawMessage(`{"type":"object","required":["command"],"properties":{` +
			`"command":{"type":"string"},"timeout":{"type":"integer","minimum":1,"description":"seconds, default 120"}}}`),
	}

	h.Mutate("advertise", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		m.Event().AppendTool(b.spec)

		return nil
	}, aish.Priority(toolsPriority))
	h.Mutate("effects", func(_ context.Context, m *aish.Mut[events.ToolCall]) error {
		c := m.Event()
		// Unparsable input leaves Access empty and the call not read-only,
		// so a policy treats it as unknown.
		var in input
		if json.Unmarshal(c.Input, &in) == nil && in.Command != "" {
			c.Effects = events.Effects{Access: []events.Access{{Action: "shell.exec", Resource: "cmd:" + in.Command}}}
		}

		return nil
	}, aish.Priority(describePriority), aish.Where(isBash))
	h.Handle("exec", func(ctx context.Context, c events.ToolCall) error {
		h.Log().DebugContext(ctx, "running tool", "tool", c.Name, "call", c.ID)
		out, err := run(ctx, c.Input)
		h.Log().DebugContext(ctx, "tool finished", "tool", c.Name, "call", c.ID, "output_bytes", len(out), "err", err)
		res := events.ToolResult{CallID: c.ID, Content: []events.Block{{Type: events.BlockText, Text: out}}, IsError: err != nil}
		if err != nil {
			res.Content[0].Text += "\n[" + err.Error() + "]"
		}
		if err := h.Emit(ctx, res); err != nil && !errors.Is(err, aish.ErrUnhandled) {
			return fmt.Errorf("aish: %s: %w", h.Instance(), err)
		}

		return nil
	}, aish.Where(isBash))

	return nil
}

func isBash(c events.ToolCall) bool { return c.Name == name }

// run returns the combined output, cut at maxOutputBytes, and a non-nil error
// when the command could not run, failed, or timed out.
func run(ctx context.Context, raw json.RawMessage) (string, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if in.Command == "" {
		return "", errors.New("command is required")
	}
	timeout := defaultTimeout
	if in.Timeout > 0 {
		timeout = time.Duration(in.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := &capped{}
	cmd := exec.CommandContext(ctx, "bash", "-c", in.Command) //nolint:gosec // running the model's command is the purpose of the tool; the policy gates it
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = out, out, killDelay
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		err = fmt.Errorf("bash: %w", err)
	}

	return out.String(), err
}

// capped keeps the first maxOutputBytes bytes and drops the rest.
type capped struct {
	buf bytes.Buffer // not embedded: its ReadFrom would bypass Write
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	room := maxOutputBytes - c.buf.Len()
	if n > room {
		c.cut = true
		p = p[:max(room, 0)]
	}
	c.buf.Write(p)

	return n, nil // a short count would fail the command with ErrShortWrite
}

func (c *capped) String() string {
	if c.cut {
		return c.buf.String() + fmt.Sprintf("\n[output cut at %d bytes]", maxOutputBytes)
	}

	return c.buf.String()
}
