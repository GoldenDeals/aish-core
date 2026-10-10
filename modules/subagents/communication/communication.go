// Package communication provides the module subagents.communication. It adds
// the tools send_message and list_agents, which let the agents of one process
// talk to each other. Sessions share nothing but the registry of this
// package, so a message is delivered by the target's own instance.
package communication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	describePriority = 2000
	// toolsPriority is the enrichment band (§4.4).
	toolsPriority = 500
	sendName      = "send_message"
	listName      = "list_agents"
	parentAlias   = "parent"
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "subagents.communication", New: func() aish.Module { return &Communication{} }})
}

// running maps a session ID to the instance that delivers messages to it.
var (
	mu      sync.Mutex
	running = map[string]*Communication{}
)

// Communication is the module subagents.communication.
type Communication struct {
	h   *aish.Host
	ctx context.Context //nolint:containedctx // the Start context is how another session's call emits outside hooks
	wg  sync.WaitGroup
}

type sendInput struct {
	To   string
	Text string
}

func (c *Communication) Init(_ context.Context, h *aish.Host) error {
	c.h = h

	h.Mutate("advertise", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		m.Event().AppendTool(events.ToolSpec{
			Name: sendName,
			Description: "Send a message to another running agent: its session ID from list_agents or the task result, " +
				"or \"parent\" for the agent that started you. The message reaches it as a message tagged " +
				"<agent_message>; the call does not wait for delivery or a reply.",
			Schema: json.RawMessage(`{"type":"object","required":["to","text"],"properties":{` +
				`"to":{"type":"string"},"text":{"type":"string"}}}`),
		})
		m.Event().AppendTool(events.ToolSpec{
			Name:        listName,
			Description: "List the running agents with their session IDs.",
			Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		})

		return nil
	}, aish.Priority(toolsPriority))
	h.Mutate("effects", func(_ context.Context, m *aish.Mut[events.ToolCall]) error {
		call := m.Event()
		if call.Name == listName {
			call.Effects = events.Effects{ReadOnly: true}

			return nil
		}
		var in sendInput
		if json.Unmarshal(call.Input, &in) == nil && in.To != "" {
			call.Effects = events.Effects{Access: []events.Access{{Action: "agent.message", Resource: "session:" + c.target(in.To)}}}
		}

		return nil
	}, aish.Priority(describePriority), aish.Where(isOurs))
	h.Handle("run", c.run, aish.Where(isOurs))

	return nil
}

// Start registers the session as reachable; Stop withdraws it before the
// context is gone, so no sender emits into a closed session.
func (c *Communication) Start(ctx context.Context) error {
	c.ctx = ctx
	mu.Lock()
	defer mu.Unlock()
	running[c.h.Session().ID] = c

	return nil
}

// Stop waits for the deliveries; the cancelled Start context ends them.
func (c *Communication) Stop(ctx context.Context) error {
	mu.Lock()
	delete(running, c.h.Session().ID)
	mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("aish: %s: waiting for deliveries: %w", c.h.Instance(), ctx.Err())
	}
}

func isOurs(call events.ToolCall) bool { return call.Name == sendName || call.Name == listName }

// target resolves the "parent" alias.
func (c *Communication) target(to string) string {
	if to == parentAlias {
		return c.h.Session().Parent
	}

	return to
}

func (c *Communication) run(ctx context.Context, call events.ToolCall) error {
	c.h.Log().InfoContext(ctx, "running tool", "tool", call.Name, "call", call.ID)
	text, err := c.dispatch(call)
	if err != nil {
		c.h.Log().WarnContext(ctx, "tool failed", "tool", call.Name, "call", call.ID, "err", err)
		text = err.Error()
	}
	res := events.ToolResult{CallID: call.ID, Content: []events.Block{{Type: events.BlockText, Text: text}}, IsError: err != nil}
	if err := c.h.Emit(ctx, res); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		return fmt.Errorf("aish: %s: %w", c.h.Instance(), err)
	}

	return nil
}

func (c *Communication) dispatch(call events.ToolCall) (string, error) {
	if call.Name == listName {
		return c.list(), nil
	}
	var in sendInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	id := c.target(in.To)
	if id == c.h.Session().ID {
		return "", errors.New("cannot send a message to yourself")
	}
	if err := c.deliver(id, in.Text); err != nil {
		return "", err
	}

	return "Message sent to " + id + ".", nil
}

func (c *Communication) list() string {
	mu.Lock()
	defer mu.Unlock()
	var b strings.Builder
	for _, id := range slices.Sorted(maps.Keys(running)) {
		info := running[id].h.Session()
		fmt.Fprintf(&b, "- %s: agent %s", id, info.Agent)
		if id == c.h.Session().ID {
			b.WriteString(" (you)")
		}
		if info.Parent != "" {
			b.WriteString(", parent " + info.Parent)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// deliver queues the message in the target session from a goroutine on the
// target's own context: the sender's one belongs to its session
// (aish.ErrWouldDeadlock), and waiting for the commit would deadlock two
// agents that message each other while both run a tool.
func (c *Communication) deliver(id, text string) error {
	mu.Lock()
	t, ok := running[id]
	if ok {
		t.wg.Add(1) // under mu, so Stop of t cannot be past its wait
	}
	mu.Unlock()
	if !ok {
		return fmt.Errorf("agent %q is not running", id)
	}
	from := c.h.Session()
	go func() {
		defer t.wg.Done()
		// Nobody handles agent.message itself: the agent loop watches the commit.
		err := t.h.Emit(t.ctx, events.AgentMessage{From: from.ID, Agent: from.Agent, Text: text})
		if err != nil && !errors.Is(err, aish.ErrUnhandled) && !errors.Is(err, aish.ErrClosed) && !errors.Is(err, context.Canceled) {
			c.h.Log().Warn("delivering message", "to", id, "err", err)
		}
	}()

	return nil
}
