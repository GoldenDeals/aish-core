// Package tui is a full-screen terminal frontend: a scrollable transcript with
// streamed replies and thinking, tool calls and results, a status bar, and an
// input line that also answers the agent's questions.
package tui

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/subagents/core"
)

const (
	source = "frontend.tui"
	// lineQueue is how many messages typed during a step wait for their turn.
	lineQueue = 64
	// maxCallArgs is how much of a call's input the transcript shows.
	maxCallArgs = 100
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "frontend.tui", New: func() aish.Module { return &TUI{} }})
}

type TUI struct {
	// In and Out default to the terminal.
	In  io.Reader
	Out io.Writer

	h       *aish.Host
	p       *tea.Program
	width   atomic.Int32
	hideSub atomic.Bool
	done    chan struct{}
	msgs    chan string

	mu    sync.Mutex
	usage events.Usage
}

func (t *TUI) Init(_ context.Context, h *aish.Host) error {
	t.h, t.done, t.msgs = h, make(chan struct{}), make(chan string, lineQueue)
	opts := []tea.ProgramOption{}
	if t.In != nil {
		opts = append(opts, tea.WithInput(t.In))
	}
	if t.Out != nil {
		opts = append(opts, tea.WithOutput(t.Out))
	}
	m := newModel(h.Session().ID, h.Agent().Name, &t.width, &t.hideSub, t.history, t.submit, h.Cancel)
	t.p = tea.NewProgram(m, opts...)

	h.Handle("show-message", t.showMessage)
	h.Handle("ask", t.ask)
	h.Handle("show-call", func(_ context.Context, c events.ToolCall) error {
		t.print(callEntry(c))

		return nil
	}, aish.Priority(0))
	h.Handle("show-stopped", func(_ context.Context, e events.LoopStopped) error {
		t.print(entry{kind: kindError, text: "stopped: " + e.Reason})

		return nil
	})
	h.Handle("show-agent-return", func(_ context.Context, e events.AgentReturn) error {
		kind, word := kindNote, "returned"
		if e.IsError {
			kind, word = kindError, "failed: "+e.Text
		}
		t.print(entry{kind: kind, text: fmt.Sprintf("[agent %s] %s", e.Agent, word)})

		return nil
	})
	h.Handle("show-agent-message", func(_ context.Context, e events.AgentMessage) error {
		t.print(entry{kind: kindNote, text: fmt.Sprintf("[agent %s] message", e.Agent)})

		return nil
	})
	h.Observe("show-result", func(_ context.Context, r events.ToolResult) { t.print(resultEntry(r)) })
	h.Observe("show-subagent-start", func(_ context.Context, e core.Started) {
		t.print(entry{kind: kindNote, agent: agentLabel(e.Agent, e.Session), text: "started"})
	})
	h.Observe("show-subagent-activity", func(_ context.Context, a core.Activity) { t.print(activityEntry(a)) })
	h.Observe("show-error", func(_ context.Context, e aish.ModuleError) {
		if !e.Nested {
			t.print(entry{kind: kindError, text: fmt.Sprintf("%s: %s", cmp.Or(e.Hook, e.Instance, "core"), e.Error)})
		}
	})
	h.Observe("busy-start", func(context.Context, aish.TxStarted) { t.send(busyMsg(true)) })
	h.Observe("busy-end", func(context.Context, aish.TxCommitted) { t.send(busyMsg(false)) })

	return nil
}

// Done is closed once the user has quit and every message has been sent. The
// agent may still be working then: wait for the session to be idle.
func (t *TUI) Done() <-chan struct{} { return t.done }

func (t *TUI) Start(ctx context.Context) error {
	go t.sendMessages(ctx)
	go func() {
		defer close(t.done)
		defer close(t.msgs)
		if _, err := t.p.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "aish: frontend.tui:", err)
		}
	}()

	return nil
}

func (t *TUI) Stop(context.Context) error {
	t.p.Quit()

	return nil
}

func (t *TUI) send(m tea.Msg) { t.p.Send(m) }

// print writes e to the scrollback from the calling hook, so entries keep the
// order of the hooks. Program.Println blocks once the program has ended.
func (t *TUI) print(e entry) {
	if e.agent != "" && t.hideSub.Load() {
		return
	}
	select {
	case <-t.done:
	default:
		t.p.Println(render(e, cmp.Or(int(t.width.Load()), defaultWidth)))
	}
}

func (t *TUI) submit(text string) { t.msgs <- text }

// sendMessages emits the messages one at a time: Emit returns when the message
// has committed, so a message typed during a step waits for the end of it.
func (t *TUI) sendMessages(ctx context.Context) {
	for text := range t.msgs {
		err := t.h.Emit(ctx, events.UserMessage{Text: text, Source: source})
		if err != nil && !errors.Is(err, aish.ErrUnhandled) {
			t.h.Log().Error("cannot send message", "err", err)
		}
	}
}

func (t *TUI) history() []entry {
	var out []entry
	for _, tx := range t.h.Journal().Txs() {
		for _, env := range tx.Events {
			switch e := env.Event.(type) {
			case events.UserMessage:
				out = append(out, entry{kind: kindUser, text: e.Text})
			case events.AssistantMessage:
				out = append(out, entry{kind: kindAssistant, text: e.Text()})
			case events.ToolCall:
				out = append(out, callEntry(e))
			case events.ToolResult:
				out = append(out, resultEntry(e))
			case core.Started:
				out = append(out, entry{kind: kindNote, agent: agentLabel(e.Agent, e.Session), text: "started"})
			case core.Activity:
				out = append(out, activityEntry(e))
			default:
			}
		}
	}

	return out
}

// showMessage prints the reply as it streams: every finished paragraph goes
// to the scrollback, the unfinished tail stays in the live area.
func (t *TUI) showMessage(ctx context.Context, m events.AssistantMessage) error {
	var tail string
	printed := false
	flush := func() {
		if strings.TrimSpace(tail) != "" {
			t.print(entry{kind: kindAssistant, text: strings.TrimSpace(tail), cont: printed})
			printed = true
		}
		tail = ""
	}
	for d := range m.Content.Chunks(ctx) {
		switch d.Type {
		case events.DeltaText:
			tail += d.Text
			for {
				para, rest, found := strings.Cut(tail, "\n\n")
				if !found {
					break
				}
				tail = para
				flush()
				tail = rest
			}
			t.send(liveMsg{kind: kindAssistant, text: strings.TrimSpace(tail), cont: printed})
		case events.DeltaThinking:
			t.send(liveMsg{kind: kindThinking})
		case events.DeltaStop:
			t.addUsage(d.Usage)
		default:
		}
	}
	flush()
	t.send(liveMsg{})
	if err := m.Content.Err(); err != nil {
		return fmt.Errorf("aish: frontend.tui: %w", err)
	}

	return nil
}

func (t *TUI) addUsage(u *events.Usage) {
	if u == nil {
		return
	}
	t.mu.Lock()
	t.usage.InputTokens += u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	t.usage.OutputTokens += u.OutputTokens
	total := t.usage
	t.mu.Unlock()
	t.send(usageMsg(total))
}

// ask puts the question in the transcript and waits for the model to deliver
// the answer. When the user quits the answer is the last option, like end of
// input in the cli frontend.
func (t *TUI) ask(ctx context.Context, q events.UserQuestion) error {
	answer := make(chan events.UserAnswer, 1)
	t.send(askMsg{q: q, answer: answer})

	var a events.UserAnswer
	select {
	case a = <-answer:
	case <-t.done:
		a = events.UserAnswer{QuestionID: q.ID, Choice: last(q.Options)}
	case <-ctx.Done():
		return fmt.Errorf("aish: frontend.tui: %w", context.Cause(ctx))
	}
	t.print(entry{kind: kindNote, text: fmt.Sprintf("%s → %s", q.Text, cmp.Or(a.Choice, a.Text))})
	if err := t.h.Emit(ctx, a); err != nil {
		return fmt.Errorf("aish: frontend.tui: %w", err)
	}

	return nil
}

func last(options []string) string {
	if len(options) == 0 {
		return ""
	}

	return options[len(options)-1]
}

// agentLabel names a subagent by its block and the tail of its session ID.
func agentLabel(agent, session string) string {
	return agent + " " + session[strings.LastIndex(session, ".")+1:]
}

var activityKinds = map[string]string{
	core.ActivityPrompt:  kindNote,
	core.ActivityMessage: kindAssistant,
	core.ActivityTool:    kindTool,
	core.ActivityDenied:  kindError,
	core.ActivityResult:  kindResult,
	core.ActivityError:   kindError,
}

func activityEntry(a core.Activity) entry {
	return entry{kind: activityKinds[a.Type], agent: agentLabel(a.Agent, a.Session), text: a.Text}
}

func callEntry(c events.ToolCall) entry {
	call := fmt.Sprintf("%s(%s)", c.Name, compactJSON(c.Input))
	if c.Denied {
		return entry{kind: kindError, text: "Denied " + call}
	}

	return entry{kind: kindTool, text: call}
}

// compactJSON puts a call's input on one short line.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return clipRunes(string(raw), maxCallArgs)
	}

	return clipRunes(b.String(), maxCallArgs)
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}

	return s
}

func resultEntry(r events.ToolResult) entry {
	var b strings.Builder
	for i := range r.Content {
		b.WriteString(r.Content[i].Text)
	}
	kind := kindResult
	if r.IsError {
		kind = kindError
	}

	return entry{kind: kind, text: clip(b.String())}
}

// NoInherit keeps TUI out of merging blocks: the user talks to the main agent only.
func (*TUI) NoInherit() {}
