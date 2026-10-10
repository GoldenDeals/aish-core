// Package gate is the part policies share: ask the user about a tool call and
// continue it on "yes", or deny it so the model sees the refusal (§6).
package gate

import (
	"context"
	"errors"
	"fmt"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

// Priority is the policy band (§4.4).
const Priority = 1000

// Gate is initialised by the policy in its Init and used from its Mutate hook.
type Gate struct {
	h       *aish.Host
	waiting map[string]aish.Envelope // by question ID, which is the call's Meta.ID
}

// Init subscribes the Handle hook that takes the user's answers.
func (g *Gate) Init(h *aish.Host) {
	g.h, g.waiting = h, map[string]aish.Envelope{}
	h.Handle("answer", g.answer)
}

// Ask removes the call and asks the user; the answer re-emits or denies it.
// Call it from the policy's Mutate hook with the hook's ctx.
func (g *Gate) Ask(ctx context.Context, m *aish.Mut[events.ToolCall]) error {
	m.Remove()
	call := aish.Envelope{Meta: m.Meta(), Event: *m.Event()}
	g.waiting[call.Meta.ID] = call

	err := g.h.Emit(ctx, events.UserQuestion{
		ID: call.Meta.ID, Text: fmt.Sprintf("Allow %s %s?", m.Event().Name, m.Event().Input), Options: []string{"yes", "no"}, Ref: call.Meta.ID,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, aish.ErrUnhandled):
		delete(g.waiting, call.Meta.ID)

		return g.deny(ctx, call, "nobody to ask")
	default:
		// Fail closed: the call is already removed, so it must not vanish
		// without an answer for the model.
		delete(g.waiting, call.Meta.ID)

		return errors.Join(err, g.deny(ctx, call, "policy error"))
	}
}

// Deny removes the call and denies it with the reason.
func (g *Gate) Deny(ctx context.Context, m *aish.Mut[events.ToolCall], reason string) error {
	m.Remove()

	return g.deny(ctx, aish.Envelope{Meta: m.Meta(), Event: *m.Event()}, reason)
}

func (g *Gate) answer(ctx context.Context, a events.UserAnswer) error {
	call, ok := g.waiting[a.QuestionID]
	if !ok {
		return nil
	}
	delete(g.waiting, a.QuestionID)
	if a.Choice == "yes" {
		if err := g.h.EmitWith(ctx, aish.Below(Priority), call); err != nil {
			return fmt.Errorf("aish: policy: %w", err)
		}

		return nil
	}

	return g.deny(ctx, call, "the user said no")
}

// deny records the call as denied below executors and answers the model.
func (g *Gate) deny(ctx context.Context, call aish.Envelope, reason string) error {
	c, _ := call.Event.(events.ToolCall)
	c.Denied = true
	text := []events.Block{{Type: events.BlockText, Text: "Tool call denied: " + reason}}

	return errors.Join(
		ignoreUnhandled(g.h.EmitWith(ctx, aish.Below(aish.DefaultPriority), aish.Envelope{Meta: call.Meta, Event: c})),
		ignoreUnhandled(g.h.Emit(ctx, events.ToolResult{CallID: c.ID, IsError: true, Content: text})),
	)
}

func ignoreUnhandled(err error) error {
	if errors.Is(err, aish.ErrUnhandled) {
		return nil
	}

	return err
}
