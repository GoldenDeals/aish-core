package aish

import (
	"encoding/json"
	"strings"
)

// Shared event types (§5). They are the public API between modules:
// changing them breaks compatibility.

type UserMessage struct {
	Text   string
	Blocks []Block // attachments added by Mutate hooks: terminal activity, images
	// Source is who wrote it: a frontend instance, or the parent session
	// of a subagent.
	Source string
}

// UserQuestion asks a human. ID is chosen by the asker and comes back in
// UserAnswer.QuestionID; an Emit that returns ErrUnhandled means nobody can
// answer.
type UserQuestion struct {
	ID      string
	Text    string
	Options []string // empty: free-form answer
	Ref     string   // Meta.ID of the event the question is about
}

type UserAnswer struct {
	QuestionID string
	Choice     string // one of UserQuestion.Options
	Text       string
}

// LLMRequest asks the provider instance Provider for one model turn. Mutate
// hooks assemble it: the order of System sections and Tools is the order of
// the hooks that add them, which keeps the prompt cache stable.
type LLMRequest struct {
	Provider string // provider instance: "default", "fast"
	Trigger  Meta   // what caused the request: user.message or tool.result
	System   []*Section
	Tools    []ToolSpec
	Messages []Message
	Params   Params
}

// Section is a named part of the system prompt: "base", "skills", "terminal".
type Section struct {
	Name string
	Text string
}

type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema of the input
}

type Params struct {
	Model       string
	MaxTokens   int
	Temperature *float64
	Extra       map[string]any // provider-specific
}

func (r *LLMRequest) AppendTool(t ToolSpec) {
	r.Tools = append(r.Tools, t)
}

// Section returns the system section with the given name, appending an empty
// one if there is none.
func (r *LLMRequest) Section(name string) *Section {
	for _, s := range r.System {
		if s.Name == name {
			return s
		}
	}
	s := &Section{Name: name}
	r.System = append(r.System, s)

	return s
}

// Message is one message of the model conversation, as transcript.Build
// builds it.
type Message struct {
	Role    string // RoleUser or RoleAssistant
	Content []Block
}

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Block is a content block. Type says which fields are set.
type Block struct {
	Type string

	Text      string // BlockText; BlockThinking: the thinking text
	Signature string // BlockThinking: the provider signs thinking and needs it back unchanged

	MediaType string // BlockImage
	Data      []byte // BlockImage; BlockRedactedThinking: opaque provider data

	ID    string          // BlockToolUse: ToolCall.ID
	Name  string          // BlockToolUse
	Input json.RawMessage // BlockToolUse

	ToolUseID string  // BlockToolResult
	Content   []Block // BlockToolResult
	IsError   bool    // BlockToolResult
}

const (
	BlockText             = "text"
	BlockThinking         = "thinking"
	BlockRedactedThinking = "redacted_thinking"
	BlockImage            = "image"
	BlockToolUse          = "tool_use"
	BlockToolResult       = "tool_result"
)

// AssistantMessage is the streamed text of one model turn. Tool calls of the
// turn are separate tool.call events of the same transaction.
type AssistantMessage struct {
	Provider string
	Model    string
	Content  Stream[ContentDelta]
}

// ContentDelta is one chunk of an assistant message.
//
// Consecutive DeltaText chunks form one text block and consecutive
// DeltaThinking chunks one thinking block; DeltaSignature signs and closes
// the open thinking block. DeltaRedactedThinking is a whole block. The
// provider ends the stream with a DeltaStop chunk carrying the stop reason
// and usage, which are known only at the end.
type ContentDelta struct {
	Type       string
	Text       string // DeltaText, DeltaThinking
	Signature  string // DeltaSignature
	Data       []byte // DeltaRedactedThinking
	StopReason string // DeltaStop: "end_turn", "tool_use", "max_tokens"
	Usage      *Usage // DeltaStop
}

const (
	DeltaText             = "text"
	DeltaThinking         = "thinking"
	DeltaSignature        = "signature"
	DeltaRedactedThinking = "redacted_thinking"
	DeltaStop             = "stop"
)

type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Text returns the text written so far, without thinking.
func (m AssistantMessage) Text() string {
	var b strings.Builder
	for _, d := range m.Content.Body() {
		if d.Type == DeltaText {
			b.WriteString(d.Text)
		}
	}

	return b.String()
}

// Blocks assembles the chunks written so far into text, thinking and
// redacted thinking blocks, in stream order. A thinking block without a
// signature (the turn was cut off) is dropped: the provider would reject it.
// Empty text blocks are dropped too.
func (m AssistantMessage) Blocks() []Block {
	var out []Block
	open := "" // BlockText or BlockThinking while chunks append to text
	var text strings.Builder
	finish := func(signature string) {
		switch {
		case open == BlockText && text.Len() > 0:
			out = append(out, Block{Type: BlockText, Text: text.String()})
		case open == BlockThinking && signature != "":
			out = append(out, Block{Type: BlockThinking, Text: text.String(), Signature: signature})
		}
		open = ""
		text.Reset()
	}

	for _, d := range m.Content.Body() {
		switch d.Type {
		case DeltaText, DeltaThinking:
			typ := BlockText
			if d.Type == DeltaThinking {
				typ = BlockThinking
			}
			if open != typ {
				finish("")
				open = typ
			}
			text.WriteString(d.Text)
		case DeltaSignature:
			if open == BlockThinking {
				finish(d.Signature)
			}
		case DeltaRedactedThinking:
			finish("")
			out = append(out, Block{Type: BlockRedactedThinking, Data: d.Data})
		default: // DeltaStop and unknown chunks carry no content
		}
	}
	finish("")

	return out
}

// Stop returns the final DeltaStop chunk, if the stream has one.
func (m AssistantMessage) Stop() (ContentDelta, bool) {
	body := m.Content.Body()
	if len(body) == 0 || body[len(body)-1].Type != DeltaStop {
		return ContentDelta{}, false
	}

	return body[len(body)-1], true
}

// ToolCall is a call the model made. ID is the provider's tool_use id;
// ToolResult.CallID refers to it.
type ToolCall struct {
	ID      string
	Name    string
	Input   json.RawMessage
	Effects Effects
	// Denied marks a call a policy refused. The policy re-emits it with
	// Below(DefaultPriority), so executors never get it; it stays in the
	// journal so the model sees the call next to its error result.
	Denied bool
}

// Effects describes what a call does. The tool fills it in with a Mutate hook
// at priority 2000, before policies look at it.
type Effects struct {
	ReadOnly bool
	Access   []Access
}

type Access struct {
	Action   string // "fs.write", "shell.exec"
	Resource string // "file:/abs/path", "cmd:make test"
	Attrs    map[string]any
}

type ToolResult struct {
	CallID  string // ToolCall.ID
	Content []Block
	IsError bool
	// Live is the output while the tool runs, for frontends; the model sees
	// only Content.
	Live *Stream[[]byte]
}

// Compacted replaces everything up to transaction UpTo with Summary when
// transcript.Build builds messages.
type Compacted struct {
	UpTo    uint64
	Summary string
}

func (UserMessage) Kind() string      { return "user.message" }
func (UserQuestion) Kind() string     { return "user.question" }
func (UserAnswer) Kind() string       { return "user.answer" }
func (LLMRequest) Kind() string       { return "llm.request" }
func (AssistantMessage) Kind() string { return "assistant.message" }
func (ToolCall) Kind() string         { return "tool.call" }
func (ToolResult) Kind() string       { return "tool.result" }
func (Compacted) Kind() string        { return "compacted" }
