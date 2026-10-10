// Package ask is the default policy: read-only tool calls run, any other call
// waits for the user's answer.
package ask

import (
	"context"
	"errors"
	"fmt"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const policyPriority = 1000

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "policy.ask", New: func() aish.Module { return &Policy{} }})
}

type Policy struct {
	h       *aish.Host
	waiting map[string]aish.Envelope // by question ID, which is the call's Meta.ID
}

func (p *Policy) Init(_ context.Context, h *aish.Host) error {
	p.h, p.waiting = h, map[string]aish.Envelope{}
	h.Mutate("authorize", p.authorize, aish.Priority(policyPriority))
	h.Handle("answer", p.answer)

	return nil
}

func (p *Policy) authorize(ctx context.Context, m *aish.Mut[events.ToolCall]) error {
	if m.Event().Effects.ReadOnly {
		return nil
	}
	m.Remove()
	call := aish.Envelope{Meta: m.Meta(), Event: *m.Event()}
	p.waiting[call.Meta.ID] = call

	err := p.h.Emit(ctx, events.UserQuestion{
		ID: call.Meta.ID, Text: "Allow " + m.Event().Name + "?", Options: []string{"yes", "no"}, Ref: call.Meta.ID,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, aish.ErrUnhandled):
		delete(p.waiting, call.Meta.ID)

		return p.deny(ctx, call, "nobody to ask")
	default:
		// Fail closed: the call is already removed, so it must not vanish
		// without an answer for the model.
		delete(p.waiting, call.Meta.ID)

		return errors.Join(err, p.deny(ctx, call, "policy error"))
	}
}

func (p *Policy) answer(ctx context.Context, a events.UserAnswer) error {
	call, ok := p.waiting[a.QuestionID]
	if !ok {
		return nil
	}
	delete(p.waiting, a.QuestionID)
	if a.Choice == "yes" {
		if err := p.h.EmitWith(ctx, aish.Below(policyPriority), call); err != nil {
			return fmt.Errorf("aish: policy.ask: %w", err)
		}

		return nil
	}

	return p.deny(ctx, call, "the user said no")
}

// deny records the call as denied below executors and answers the model.
func (p *Policy) deny(ctx context.Context, call aish.Envelope, reason string) error {
	c, _ := call.Event.(events.ToolCall)
	c.Denied = true
	text := []events.Block{{Type: events.BlockText, Text: "Tool call denied: " + reason}}

	return errors.Join(
		ignoreUnhandled(p.h.EmitWith(ctx, aish.Below(aish.DefaultPriority), aish.Envelope{Meta: call.Meta, Event: c})),
		ignoreUnhandled(p.h.Emit(ctx, events.ToolResult{CallID: c.ID, IsError: true, Content: text})),
	)
}

func ignoreUnhandled(err error) error {
	if errors.Is(err, aish.ErrUnhandled) {
		return nil
	}

	return err
}
