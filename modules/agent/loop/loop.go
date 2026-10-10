// Package loop is the agent loop module: it watches committed transactions
// and queues the next llm.request while the agent has something to answer.
package loop

import (
	"cmp"
	"context"
	"strconv"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/transcript"
)

const (
	defaultMaxSteps = 50
	policyPriority  = 1000
	// normalizePriority is where tools describe their calls; it is above the
	// policy band, and those hooks are not policies.
	normalizePriority = 2000
	// messagesPriority is above every other llm.request hook that reads
	// Messages.
	messagesPriority = 3000
	provider         = "default"
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "agent.loop", New: func() aish.Module { return &Loop{} }})
	aish.RegisterDirective("max_steps", unmarshalMaxSteps)
}

// Settings are the agent options of the Agentfile directive max_steps.
type Settings struct {
	MaxSteps int
}

func unmarshalMaxSteps(d *caddyfile.Dispenser, v *aish.Values, _ aish.Scope) error {
	var arg string
	if !d.Args(&arg) || len(d.RemainingArgs()) > 0 {
		return d.ArgErr()
	}
	steps, err := strconv.Atoi(arg)
	if err != nil || steps < 1 {
		return d.Errf("max_steps wants a positive number, got %q", arg)
	}
	aish.Set(v, Settings{MaxSteps: steps})

	return nil
}

type Loop struct {
	// MaxSteps limits the llm.requests after one user.message. Zero takes the
	// max_steps of the agent block, then of the global block, then 50.
	MaxSteps int

	h       *aish.Host
	steps   int
	pending bool // an llm.request is queued but has not started yet
}

func (l *Loop) Init(_ context.Context, h *aish.Host) error {
	l.h = h
	if l.MaxSteps == 0 {
		spec := h.Agent()
		own, _ := aish.Get[Settings](spec.Own)
		global, _ := aish.Get[Settings](spec.Global)
		l.MaxSteps = cmp.Or(own.MaxSteps, global.MaxSteps)
	}
	if l.MaxSteps == 0 {
		l.MaxSteps = defaultMaxSteps
	}
	// Messages are built when the transaction starts, not when the request
	// is queued, so the request sees everything committed in between.
	h.Mutate("messages", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		l.pending = false
		m.Event().Messages = transcript.Build(h.Journal().Txs())

		return nil
	}, aish.Priority(messagesPriority))
	h.Observe("continue", l.onCommitted)

	return nil
}

// Start warns when no policy can intercept tool calls.
func (l *Loop) Start(context.Context) error {
	for _, hk := range l.h.Hooks[events.ToolCall]() {
		if hk.Mode == aish.ModeMutate && hk.Priority >= policyPriority && hk.Priority < normalizePriority {
			return nil
		}
	}
	l.h.Log().Warn("no policy intercepts tool.call: every tool call will run unchecked")

	return nil
}

func (l *Loop) onCommitted(ctx context.Context, c aish.TxCommitted) {
	trigger, fresh, ok := continuation(c.Tx)
	if !ok {
		l.h.Log().DebugContext(ctx, "agent has nothing to answer", "tx", c.Tx.Seq, "status", c.Tx.Status)

		return
	}
	if l.pending {
		l.h.Log().DebugContext(ctx, "LLM request already queued", "tx", c.Tx.Seq)

		// The queued request will carry this message too, as the first step
		// after it.
		if fresh {
			l.steps = 1
		}

		return
	}
	if fresh {
		l.steps = 0
	}
	if l.steps >= l.MaxSteps {
		l.h.Log().InfoContext(ctx, "agent stopped", "reason", "max_steps", "steps", l.steps)
		l.emit(ctx, events.LoopStopped{Reason: "max_steps"})

		return
	}
	l.steps++
	l.pending = true
	l.h.Log().DebugContext(ctx, "requesting the LLM", "step", l.steps, "max_steps", l.MaxSteps, "trigger", trigger.ID, "fresh", fresh)
	l.emit(ctx, events.LLMRequest{Provider: provider, Trigger: trigger})
}

func (l *Loop) emit(ctx context.Context, ev aish.Event) {
	// Emit from an Observe hook only queues the transaction, so an error
	// means the session is closing.
	if err := l.h.Emit(ctx, ev); err != nil {
		l.h.Log().ErrorContext(ctx, "emit failed", "event", ev.Kind(), "err", err)
	}
}

// continuation reports whether the agent must answer tx: it holds a
// user.message or tool.result, is not cancelled, and none of its tool calls
// is left without a result. trigger is the last such event; fresh says the
// transaction has a user.message, which restarts the step count.
func continuation(tx aish.Tx) (trigger aish.Meta, fresh, ok bool) {
	if tx.Status == aish.StatusCancelled {
		return trigger, false, false
	}
	calls, results := map[string]bool{}, map[string]bool{}
	for _, env := range tx.Events {
		switch e := env.Event.(type) {
		case events.UserMessage:
			trigger, fresh, ok = env.Meta, true, true
		case events.AgentReturn:
			trigger, ok = env.Meta, true
		case events.ToolResult:
			trigger, ok = env.Meta, true
			results[e.CallID] = true
		case events.ToolCall:
			calls[e.ID] = true
		}
	}
	for id := range calls {
		if !results[id] {
			return trigger, fresh, false
		}
	}

	return trigger, fresh, ok
}
