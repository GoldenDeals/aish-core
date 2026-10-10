// Package anthropic is the model provider for the Anthropic Messages API: it
// answers llm.request with a streamed assistant.message and tool.call events.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	defaultModel     = "claude-opus-5-5"
	defaultMaxTokens = 64000 // the request streams, so a large cap does not risk HTTP timeouts
	provider         = "default"
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "llm.anthropic", New: func() aish.Module { return &Anthropic{} }})
}

type Anthropic struct {
	Model     string // claude-opus-5-5 when empty
	APIKey    string // ANTHROPIC_API_KEY or an `ant auth login` profile when empty
	BaseURL   string
	MaxTokens int // defaultMaxTokens when zero

	h      *aish.Host
	client sdk.Client
}

// UnmarshalCaddyfile reads "llm anthropic { model <id>; api_key <key>;
// max_tokens <n>; base_url <url> }".
func (a *Anthropic) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	if len(d.RemainingArgs()) > 0 {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		var value string
		opt := d.Val()
		if !d.Args(&value) {
			return d.ArgErr()
		}
		switch opt {
		case "model":
			a.Model = value
		case "api_key":
			a.APIKey = value
		case "base_url":
			a.BaseURL = value
		case "max_tokens":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return d.Errf("max_tokens wants a positive number, got %q", value)
			}
			a.MaxTokens = n
		default:
			return d.Errf("unknown option %q", opt)
		}
	}

	return nil
}

func (a *Anthropic) Init(_ context.Context, h *aish.Host) error {
	a.h = h
	var opts []option.RequestOption
	if a.APIKey != "" {
		opts = append(opts, option.WithAPIKey(a.APIKey))
	}
	if a.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(a.BaseURL))
	}
	a.client = sdk.NewClient(opts...)

	h.Handle("respond", a.respond, aish.Where(func(r events.LLMRequest) bool { return r.Provider == provider }))

	return nil
}

func (a *Anthropic) respond(ctx context.Context, r events.LLMRequest) error {
	params := a.params(r)
	a.h.Log().DebugContext(ctx, "sending LLM request", "model", params.Model, "messages", len(params.Messages),
		"tools", len(params.Tools), "max_tokens", params.MaxTokens)
	content, w := aish.NewStream[events.ContentDelta]()
	result := make(chan streamResult, 1)
	// The stream is written from its own goroutine: Emit below returns only
	// after the Handle hooks have read it to the end.
	go func() {
		res := a.stream(ctx, params, w)
		w.Close(res.err)
		result <- res
	}()

	err := a.h.Emit(ctx, events.AssistantMessage{Provider: "anthropic", Model: params.Model, Content: content})
	if err != nil && !errors.Is(err, aish.ErrUnhandled) {
		return fmt.Errorf("aish: llm.anthropic: %w", err)
	}
	res := <-result
	if res.err != nil {
		return fmt.Errorf("aish: llm.anthropic: %w", res.err)
	}
	if len(res.calls) == 0 {
		return nil
	}
	if err := a.h.Emit(ctx, res.calls...); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		return fmt.Errorf("aish: llm.anthropic: %w", err)
	}

	return nil
}

type streamResult struct {
	calls []aish.Event
	err   error
}

// stream runs the request and copies the deltas into w. It returns the tool
// calls of the finished message.
func (a *Anthropic) stream(ctx context.Context, params sdk.MessageNewParams, w *aish.StreamWriter[events.ContentDelta]) streamResult {
	st := a.client.Messages.NewStreaming(ctx, params)
	defer st.Close()

	var msg sdk.Message
	for st.Next() {
		ev := st.Current()
		if err := msg.Accumulate(ev); err != nil {
			return streamResult{err: fmt.Errorf("accumulate stream: %w", err)}
		}
		writeDelta(w, ev)
	}
	if err := st.Err(); err != nil {
		return streamResult{err: fmt.Errorf("stream: %w", err)}
	}
	a.h.Log().DebugContext(ctx, "LLM response received", "stop_reason", msg.StopReason,
		"input_tokens", msg.Usage.InputTokens, "output_tokens", msg.Usage.OutputTokens)
	w.Write(events.ContentDelta{
		Type:       events.DeltaStop,
		StopReason: string(msg.StopReason),
		Usage: &events.Usage{
			InputTokens:      int(msg.Usage.InputTokens),
			OutputTokens:     int(msg.Usage.OutputTokens),
			CacheReadTokens:  int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens: int(msg.Usage.CacheCreationInputTokens),
		},
	})

	return streamResult{calls: a.toolCalls(msg)}
}

func writeDelta(w *aish.StreamWriter[events.ContentDelta], ev sdk.MessageStreamEventUnion) {
	switch e := ev.AsAny().(type) {
	case sdk.ContentBlockStartEvent:
		switch e.ContentBlock.Type {
		case "redacted_thinking":
			w.Write(events.ContentDelta{Type: events.DeltaRedactedThinking, Data: []byte(e.ContentBlock.Data)})
		case "thinking":
			// With omitted display no thinking_delta follows, only the
			// signature: open the block so the signature has one to close.
			w.Write(events.ContentDelta{Type: events.DeltaThinking})
		case "tool_use":
			w.Write(events.ContentDelta{Type: events.DeltaToolUse, ToolID: e.ContentBlock.ID})
		default:
		}
	case sdk.ContentBlockDeltaEvent:
		switch d := e.Delta.AsAny().(type) {
		case sdk.TextDelta:
			w.Write(events.ContentDelta{Type: events.DeltaText, Text: d.Text})
		case sdk.ThinkingDelta:
			w.Write(events.ContentDelta{Type: events.DeltaThinking, Text: d.Thinking})
		case sdk.SignatureDelta:
			w.Write(events.ContentDelta{Type: events.DeltaSignature, Signature: d.Signature})
		default:
		}
	default:
	}
}

// toolCalls turns the tool_use blocks into events. A call whose input is not
// valid JSON (the reply was cut off by max_tokens) is dropped: it cannot run.
func (a *Anthropic) toolCalls(msg sdk.Message) []aish.Event {
	var calls []aish.Event
	for i := range msg.Content {
		use, ok := msg.Content[i].AsAny().(sdk.ToolUseBlock)
		if !ok {
			continue
		}
		if !json.Valid(use.Input) {
			a.h.Log().Warn("dropping tool call with invalid input", "tool", use.Name, "stop_reason", msg.StopReason)

			continue
		}
		calls = append(calls, events.ToolCall{ID: use.ID, Name: use.Name, Input: use.Input})
	}

	return calls
}

func (a *Anthropic) params(r events.LLMRequest) sdk.MessageNewParams {
	model := firstNonEmpty(r.Params.Model, a.Model, defaultModel)
	maxTokens := r.Params.MaxTokens
	if maxTokens == 0 {
		maxTokens = a.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	p := sdk.MessageNewParams{
		Model:     model,
		MaxTokens: int64(maxTokens),
		System:    system(r.System),
		Tools:     tools(r.Tools),
		Messages:  messages(r.Messages),
	}
	if r.Params.Temperature != nil {
		p.Temperature = sdk.Float(*r.Params.Temperature)
	}

	return p
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// system sends the sections in order, with the cache breakpoint on the last
// one: tools and system are cached together.
func system(sections []*events.Section) []sdk.TextBlockParam {
	var out []sdk.TextBlockParam
	for _, s := range sections {
		if s.Text != "" {
			out = append(out, sdk.TextBlockParam{Text: s.Text})
		}
	}
	if len(out) > 0 {
		out[len(out)-1].CacheControl = sdk.NewCacheControlEphemeralParam()
	}

	return out
}

func tools(specs []events.ToolSpec) []sdk.ToolUnionParam {
	out := make([]sdk.ToolUnionParam, 0, len(specs))
	for _, t := range specs {
		var schema struct {
			Properties any
			Required   []string
		}
		_ = json.Unmarshal(t.Schema, &schema) // an unparsable schema becomes an object without properties
		out = append(out, sdk.ToolUnionParam{OfTool: &sdk.ToolParam{
			Name:        t.Name,
			Description: sdk.String(t.Description),
			InputSchema: sdk.ToolInputSchemaParam{Properties: schema.Properties, Required: schema.Required},
		}})
	}
	if n := len(out); n > 0 {
		out[n-1].OfTool.CacheControl = sdk.NewCacheControlEphemeralParam()
	}

	return out
}

func messages(msgs []events.Message) []sdk.MessageParam {
	out := make([]sdk.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]sdk.ContentBlockParamUnion, 0, len(m.Content))
		for i := range m.Content {
			if block, ok := contentBlock(m.Content[i]); ok {
				blocks = append(blocks, block)
			}
		}
		if m.Role == events.RoleAssistant {
			out = append(out, sdk.NewAssistantMessage(blocks...))
		} else {
			out = append(out, sdk.NewUserMessage(blocks...))
		}
	}

	return out
}

// contentBlock converts a block; images are not supported yet and are dropped.
func contentBlock(b events.Block) (sdk.ContentBlockParamUnion, bool) {
	switch b.Type {
	case events.BlockText:
		return sdk.NewTextBlock(b.Text), true
	case events.BlockThinking:
		return sdk.NewThinkingBlock(b.Signature, b.Text), true
	case events.BlockRedactedThinking:
		return sdk.NewRedactedThinkingBlock(string(b.Data)), true
	case events.BlockToolUse:
		return sdk.NewToolUseBlock(b.ID, b.Input, b.Name), true
	case events.BlockToolResult:
		var text strings.Builder
		for i := range b.Content {
			text.WriteString(b.Content[i].Text)
		}

		return sdk.NewToolResultBlock(b.ToolUseID, text.String(), b.IsError), true
	default:
		return sdk.ContentBlockParamUnion{}, false
	}
}
