// Package mcp makes the tools of one MCP server tools of the agent. Each
// instance is one server; its tools are named mcp__<server>__<tool>.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	describePriority = 2000
	// toolsPriority is the enrichment band (§4.4).
	toolsPriority = 500
	prefix        = "mcp__"
)

var serverName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "tool.mcp", New: func() aish.Module { return &MCP{} }})
}

// MCP is the module tool.mcp. Server names must not contain "_", so the tool
// name mcp__<server>__<tool> splits unambiguously.
type MCP struct {
	Server  string
	Command []string // stdio server
	URL     string   // streamable HTTP server
	// Transport replaces Command and URL, for tests.
	Transport sdk.Transport

	h       *aish.Host
	session *sdk.ClientSession
	ctx     context.Context //nolint:containedctx // the Start context, needed to relist tools when the server asks

	mu    sync.Mutex
	tools map[string]*sdk.Tool // by full tool name
	specs []events.ToolSpec
}

// UnmarshalCaddyfile reads "tool mcp <server> { command <argv>...; url <url> }".
func (m *MCP) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	args := d.RemainingArgs()
	if len(args) != 1 {
		return d.ArgErr()
	}
	m.Server = args[0]
	for d.NextBlock(0) {
		switch d.Val() {
		case "command":
			if m.Command = d.RemainingArgs(); len(m.Command) == 0 {
				return d.ArgErr()
			}
		case "url":
			if !d.Args(&m.URL) {
				return d.ArgErr()
			}
		default:
			return d.Errf("unknown option %q", d.Val())
		}
	}

	return nil
}

// InstanceName makes the loader name the instance tool.mcp/<server>.
func (m *MCP) InstanceName() string { return m.Server }

func (m *MCP) Validate() error {
	switch {
	case !serverName.MatchString(m.Server):
		return fmt.Errorf("aish: tool.mcp: server name %q must be letters, digits and '-'", m.Server)
	case m.Transport == nil && (len(m.Command) == 0) == (m.URL == ""):
		return fmt.Errorf("aish: tool.mcp/%s: set either command or url", m.Server)
	}

	return nil
}

func (m *MCP) Init(_ context.Context, h *aish.Host) error {
	m.h, m.tools = h, map[string]*sdk.Tool{}
	own := aish.Where(func(c events.ToolCall) bool { return strings.HasPrefix(c.Name, m.prefix()) })

	h.Mutate("advertise", func(_ context.Context, r *aish.Mut[events.LLMRequest]) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, s := range m.specs {
			r.Event().AppendTool(s)
		}

		return nil
	}, aish.Priority(toolsPriority))
	h.Mutate("effects", func(_ context.Context, c *aish.Mut[events.ToolCall]) error {
		call := c.Event()
		t := m.tool(call.Name)
		if t == nil {
			return nil
		}
		call.Effects = events.Effects{
			ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint,
			Access:   []events.Access{{Action: "mcp.call", Resource: "mcp:" + m.Server + "/" + t.Name}},
		}

		return nil
	}, aish.Priority(describePriority), own)
	h.Handle("call", m.call, own)

	return nil
}

func (m *MCP) prefix() string { return prefix + m.Server + "__" }

func (m *MCP) tool(full string) *sdk.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.tools[full]
}

func (m *MCP) Start(ctx context.Context) error {
	client := sdk.NewClient(&sdk.Implementation{Name: "aish", Version: "0"}, &sdk.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, _ *sdk.ToolListChangedRequest) {
			if err := m.refresh(ctx); err != nil {
				m.h.Log().ErrorContext(ctx, "cannot relist MCP tools", "server", m.Server, "err", err)
			}
		},
	})
	m.ctx = ctx
	var err error
	m.session, err = client.Connect(ctx, m.transport(), nil)
	if err != nil {
		return fmt.Errorf("aish: tool.mcp/%s: %w", m.Server, err)
	}

	return m.refresh(ctx)
}

func (m *MCP) Stop(context.Context) error {
	if m.session == nil {
		return nil
	}
	if err := m.session.Close(); err != nil {
		return fmt.Errorf("aish: tool.mcp/%s: %w", m.Server, err)
	}

	return nil
}

func (m *MCP) transport() sdk.Transport {
	switch {
	case m.Transport != nil:
		return m.Transport
	case m.URL != "":
		return &sdk.StreamableClientTransport{Endpoint: m.URL}
	}

	return &sdk.CommandTransport{Command: exec.Command(m.Command[0], m.Command[1:]...)} //nolint:gosec // running the configured server is the purpose of the module
}

// refresh replaces the tool list. A changed list changes the request prompt,
// which resets the prompt cache, so it is logged.
func (m *MCP) refresh(ctx context.Context) error {
	tools := map[string]*sdk.Tool{}
	var specs []events.ToolSpec
	for t, err := range m.session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("aish: tool.mcp/%s: list tools: %w", m.Server, err)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return fmt.Errorf("aish: tool.mcp/%s: schema of %s: %w", m.Server, t.Name, err)
		}
		full := m.prefix() + t.Name
		tools[full] = t
		specs = append(specs, events.ToolSpec{Name: full, Description: t.Description, Schema: schema})
	}

	m.mu.Lock()
	m.tools, m.specs = tools, specs
	m.mu.Unlock()
	m.h.Log().InfoContext(ctx, "MCP tools listed", "server", m.Server, "tools", len(specs))

	return nil
}

func (m *MCP) call(ctx context.Context, c events.ToolCall) error {
	res := events.ToolResult{CallID: c.ID}
	text, isErr := m.invoke(ctx, c)
	res.IsError, res.Content = isErr, []events.Block{{Type: events.BlockText, Text: text}}
	if err := m.h.Emit(ctx, res); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		return fmt.Errorf("aish: tool.mcp/%s: %w", m.Server, err)
	}

	return nil
}

func (m *MCP) invoke(ctx context.Context, c events.ToolCall) (string, bool) {
	t := m.tool(c.Name)
	if t == nil {
		return fmt.Sprintf("unknown MCP tool %q", c.Name), true
	}
	var args any
	if len(c.Input) > 0 {
		if err := json.Unmarshal(c.Input, &args); err != nil {
			return "invalid input: " + err.Error(), true
		}
	}
	out, err := m.session.CallTool(ctx, &sdk.CallToolParams{Name: t.Name, Arguments: args})
	if err != nil {
		return err.Error(), true
	}

	return render(out), out.IsError
}

// render joins the text of the result; other content is named, not embedded.
func render(r *sdk.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, t.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[%T content is not supported]", c))
		}
	}
	if len(parts) == 0 && r.StructuredContent != nil {
		if b, err := json.Marshal(r.StructuredContent); err == nil {
			return string(b)
		}
	}

	return strings.Join(parts, "\n")
}
