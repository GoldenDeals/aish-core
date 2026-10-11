// Package core provides the module subagents.core. It adds the tool task,
// which hands a prompt to another agent block of the Agentfile and returns its
// last answer. The agent runs in its own session (Host.Spawn) with its own
// modules.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/transcript"
)

const (
	describePriority = 2000
	// toolsPriority is the enrichment band (§4.4).
	toolsPriority = 500
	name          = "task"
	// closeGrace bounds how long a cancelled child may take to stop.
	closeGrace = 10 * time.Second
	// defaultMaxDepth is the nesting limit when no block sets max_depth.
	defaultMaxDepth = 2
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "subagents.core", New: func() aish.Module { return &Subagent{} }})
	aish.RegisterEvent[Started]()
	aish.RegisterEvent[Finished]()
	aish.RegisterEvent[Activity]()
}

// Subagent is the module subagents.core.
type Subagent struct {
	h     *aish.Host
	limit int

	ctx context.Context //nolint:containedctx // the Start context is how a goroutine emits outside hooks
	wg  sync.WaitGroup
}

// ExtendConfig adds the agent options and the named blocks "<name> [merge|clean]"
// that describe the agents task can call.
func (*Subagent) ExtendConfig(c *aish.ConfigExt) {
	c.Directive("description", unmarshalDescription)
	c.Directive("inject", unmarshalInject)
	c.Directive("max_depth", unmarshalMaxDepth)
	c.Block(declareAgent)
}

// declareAgent reads the header "<name> [merge|clean]"; a block merges with
// the root unless it says clean.
func declareAgent(b *aish.Block) (bool, error) {
	inherit := true
	switch flags := b.Flags; {
	case len(flags) == 0 || (len(flags) == 1 && flags[0] == "merge"):
	case len(flags) == 1 && flags[0] == "clean":
		inherit = false
	default:
		return false, fmt.Errorf("want \"%s [merge|clean] {\", got flags %v", b.Name, flags)
	}

	if err := b.Declare(inherit); err != nil {
		return false, fmt.Errorf("declare: %w", err)
	}

	return true, nil
}

// Settings are the agent options of this module: how the agent presents
// itself to others and how deep its subagents may nest.
type Settings struct {
	Description string
	Hidden      bool // inject off: other agents are not told about this one
	MaxDepth    int
	DepthSet    bool // false: defaultMaxDepth
}

// Inherited drops what describes one agent: a block that merges with the root
// is not the root.
func (s Settings) Inherited() any {
	s.Description, s.Hidden = "", false

	return s
}

func update(v *aish.Values, change func(*Settings)) {
	s, _ := aish.Get[Settings](*v)
	change(&s)
	aish.Set(v, s)
}

func agentOnly(d *caddyfile.Dispenser, sec aish.Section) error {
	if sec.Root {
		return d.Errf("%s belongs to a named agent block", d.Val())
	}

	return nil
}

func unmarshalDescription(d *caddyfile.Dispenser, v *aish.Values, _ aish.Section) error {
	var text string
	if !d.Args(&text) || len(d.RemainingArgs()) > 0 {
		return d.ArgErr()
	}
	update(v, func(s *Settings) { s.Description = text })

	return nil
}

func unmarshalInject(d *caddyfile.Dispenser, v *aish.Values, sec aish.Section) error {
	if err := agentOnly(d, sec); err != nil {
		return err
	}
	var arg string
	if !d.Args(&arg) || len(d.RemainingArgs()) > 0 || (arg != "on" && arg != "off") {
		return d.Errf("inject wants on or off")
	}
	update(v, func(s *Settings) { s.Hidden = arg == "off" })

	return nil
}

func unmarshalMaxDepth(d *caddyfile.Dispenser, v *aish.Values, _ aish.Section) error {
	var arg string
	if !d.Args(&arg) || len(d.RemainingArgs()) > 0 {
		return d.ArgErr()
	}
	n, err := strconv.Atoi(arg)
	if err != nil {
		return d.Errf("max_depth wants a number, +n or -n, got %q", arg)
	}
	update(v, func(s *Settings) {
		if arg[0] == '+' || arg[0] == '-' {
			n = max(s.depth()+n, 0)
		}
		s.MaxDepth, s.DepthSet = n, true
	})

	return nil
}

// depth is the nesting limit: where a session of the agent stops offering
// task. A signed max_depth counts from the value the agent inherited.
func (s Settings) depth() int {
	if s.DepthSet {
		return s.MaxDepth
	}

	return defaultMaxDepth
}

// Started and Finished let a frontend show the nesting.
type Started struct {
	CallID  string
	Agent   string
	Session string
}

type Finished struct {
	CallID  string
	Agent   string
	Session string
	IsError bool
}

func (Started) Kind() string  { return "subagent.started" }
func (Finished) Kind() string { return "subagent.finished" }
func (Activity) Kind() string { return "subagent.activity" }

// agent is what the tool says about one callable agent.
type agent struct {
	name, description string
}

type input struct {
	Agent  string
	Prompt string
}

func (s *Subagent) Init(_ context.Context, h *aish.Host) error {
	s.h = h
	settings, _ := aish.Get[Settings](h.Agent().Values)
	s.limit = settings.depth()

	h.Mutate("advertise", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		if spec, ok := s.spec(); ok {
			m.Event().AppendTool(spec)
		}

		return nil
	}, aish.Priority(toolsPriority))
	h.Mutate("effects", func(_ context.Context, m *aish.Mut[events.ToolCall]) error {
		c := m.Event()
		var in input
		if json.Unmarshal(c.Input, &in) == nil && in.Agent != "" {
			c.Effects = events.Effects{Access: []events.Access{{Action: "subagent.run", Resource: "agent:" + in.Agent}}}
		}

		return nil
	}, aish.Priority(describePriority), aish.Where(isTask))
	h.Handle("run", s.run, aish.Where(isTask))

	return nil
}

// Start keeps the context the background goroutines emit with.
func (s *Subagent) Start(ctx context.Context) error {
	s.ctx = ctx

	return nil
}

// Stop waits for the goroutines; Close of the session has closed the agents
// and cancelled the Start context, so they are about to end.
func (s *Subagent) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("aish: %s: waiting for agents: %w", s.h.Instance(), ctx.Err())
	}
}

func isTask(c events.ToolCall) bool { return c.Name == name }

// callable returns the agents this session may call: the visible ones, and
// none once the session sits at the depth limit.
func (s *Subagent) callable() []agent {
	if s.h.Session().Depth >= s.limit {
		return nil
	}
	var agents []agent
	for _, spec := range s.h.Agents() {
		if own, _ := aish.Get[Settings](spec.Values); !own.Hidden {
			agents = append(agents, agent{name: spec.Name, description: own.Description})
		}
	}

	return agents
}

func (s *Subagent) allowed(name string) bool {
	for _, a := range s.callable() {
		if a.name == name {
			return true
		}
	}

	return false
}

func (s *Subagent) spec() (events.ToolSpec, bool) {
	agents := s.callable()
	if len(agents) == 0 {
		return events.ToolSpec{}, false
	}
	var desc strings.Builder
	desc.WriteString("Start another agent on a self-contained task. The call returns at once; the agent works in the " +
		"background and its final answer arrives later as a message tagged <agent_return>. " +
		"The agent starts with an empty context, so the prompt must contain everything it needs.\nAgents:")
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.name
		desc.WriteString("\n- " + a.name)
		if a.description != "" {
			desc.WriteString(": " + a.description)
		}
	}
	enum, _ := json.Marshal(names)

	return events.ToolSpec{
		Name:        name,
		Description: desc.String(),
		Schema: json.RawMessage(`{"type":"object","required":["agent","prompt"],"properties":{` +
			`"agent":{"type":"string","enum":` + string(enum) + `},"prompt":{"type":"string"}}}`),
	}, true
}

// run starts the agent and answers the call at once; the agent's answer
// comes back later as an agent.return in a transaction of its own, so the
// parent session is not held while the agent works.
func (s *Subagent) run(ctx context.Context, c events.ToolCall) error {
	s.h.Log().InfoContext(ctx, "running tool", "tool", c.Name, "call", c.ID)
	text, err := s.start(ctx, c)
	if err != nil {
		s.h.Log().WarnContext(ctx, "tool failed", "tool", c.Name, "call", c.ID, "err", err)
		text = err.Error()
	} else {
		s.h.Log().InfoContext(ctx, "tool finished", "tool", c.Name, "call", c.ID)
	}
	res := events.ToolResult{CallID: c.ID, Content: []events.Block{{Type: events.BlockText, Text: text}}, IsError: err != nil}
	if err := s.h.Emit(ctx, res); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		return fmt.Errorf("aish: %s: %w", s.h.Instance(), err)
	}

	return nil
}

// start spawns the agent and lets a goroutine wait for it. Its error is for
// the model, so the tool.result carries it.
func (s *Subagent) start(ctx context.Context, c events.ToolCall) (string, error) {
	var in input
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if !s.allowed(in.Agent) {
		return "", fmt.Errorf("agent %q is not available", in.Agent)
	}

	out := make(chan Activity, activityQueue)
	child, err := spawn(ctx, s.h, in.Agent, aish.Instance{
		Name:   "subagents.watch",
		Module: &watcher{callID: c.ID, agent: in.Agent, out: out},
	})
	if err != nil {
		return "", fmt.Errorf("start agent %s: %w", in.Agent, err)
	}
	id := child.Info().ID
	s.emit(ctx, Started{CallID: c.ID, Agent: in.Agent, Session: id})

	release := s.h.Hold()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer release()
		s.collect(child, c.ID, in, out)
	}()

	return fmt.Sprintf("Agent %s started (session %s) and is working in the background. "+
		"Its answer will arrive later as a message tagged <agent_return>; "+
		"carry on with other work or end your turn, and do not guess the result.", in.Agent, id), nil
}

// collect waits for the agent and delivers its answer to the parent in one
// transaction. It runs on the Start context: Close of the parent ends it.
func (s *Subagent) collect(child *aish.Session, callID string, in input, out <-chan Activity) {
	stop, pumped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(pumped)
		s.pump(out, stop)
	}()
	answer, askErr := ask(s.ctx, child, s.h.Session().ID, in.Prompt)
	s.closeChild(child, in.Agent)
	close(stop)
	<-pumped
	if askErr != nil {
		answer = askErr.Error()
	}
	id := child.Info().ID
	err := s.h.Emit(s.ctx,
		Finished{CallID: callID, Agent: in.Agent, Session: id, IsError: askErr != nil},
		events.AgentReturn{CallID: callID, Agent: in.Agent, Session: id, Text: answer, IsError: askErr != nil},
	)
	if err != nil && !errors.Is(err, aish.ErrUnhandled) && !errors.Is(err, aish.ErrClosed) && !errors.Is(err, context.Canceled) {
		s.h.Log().Warn("delivering agent.return", "agent", in.Agent, "call", callID, "err", err)
	}
}

// pump delivers the child's activity to the parent until stop is closed, then
// delivers what is left.
func (s *Subagent) pump(out <-chan Activity, stop <-chan struct{}) {
	for {
		select {
		case a := <-out:
			s.emit(s.ctx, a)
		case <-stop:
			for {
				select {
				case a := <-out:
					s.emit(s.ctx, a)
				default:
					return
				}
			}
		}
	}
}

func (s *Subagent) emit(ctx context.Context, ev aish.Event) {
	if err := s.h.Emit(ctx, ev); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		s.h.Log().WarnContext(ctx, "emitting subagent event", "kind", ev.Kind(), "err", err)
	}
}

// ask sends the prompt and waits until the child is idle, which is the end
// of its agent loop.
func ask(ctx context.Context, child *aish.Session, parent, prompt string) (string, error) {
	// Nobody handles user.message itself: the agent loop watches the commit.
	if err := child.Emit(ctx, events.UserMessage{Text: prompt, Source: parent}); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		return "", fmt.Errorf("send prompt: %w", err)
	}
	if err := child.Wait(ctx); err != nil {
		return "", fmt.Errorf("wait for agent: %w", err)
	}

	if tx, ok := child.Journal().Last(); ok && tx.Status == aish.StatusCancelled {
		return "", errors.New("agent was cancelled")
	}
	msgs := transcript.Build(child.Journal().Txs())
	for i := len(msgs) - 1; i >= 0; i-- {
		m := &msgs[i]
		if m.Role != events.RoleAssistant {
			continue
		}
		var text []string
		for j := range m.Content {
			if b := &m.Content[j]; b.Type == events.BlockText && b.Text != "" {
				text = append(text, b.Text)
			}
		}
		if len(text) > 0 {
			return strings.Join(text, "\n"), nil
		}
	}

	return "", errors.New("agent produced no answer")
}

func (s *Subagent) closeChild(child *aish.Session, agent string) {
	// The Start context may be cancelled already; the child must still stop.
	stop, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), closeGrace)
	defer cancel()
	if err := child.Close(stop); err != nil {
		s.h.Log().Warn("closing subagent", "agent", agent, "err", err)
	}
}

// spawn exists because staticcheck's nilness pass panics on a two-value call
// of Host.Spawn made directly from a method of this package.
func spawn(ctx context.Context, h *aish.Host, agent string, extra ...aish.Instance) (*aish.Session, error) {
	return h.Spawn(ctx, agent, extra...) //nolint:wrapcheck // delegate wraps it
}
