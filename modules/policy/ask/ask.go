// Package ask is the default policy: read-only tool calls run, any other call
// waits for the user's answer.
package ask

import (
	"context"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/policy/internal/gate"
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "policy.ask", New: func() aish.Module { return &Policy{} }})
}

type Policy struct {
	gate gate.Gate
}

func (p *Policy) Init(_ context.Context, h *aish.Host) error {
	p.gate.Init(h)
	h.Mutate("authorize", p.authorize, aish.Priority(gate.Priority))

	return nil
}

func (p *Policy) authorize(ctx context.Context, m *aish.Mut[events.ToolCall]) error {
	if m.Event().Effects.ReadOnly {
		return nil
	}

	return p.gate.Ask(ctx, m)
}
