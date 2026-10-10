// Package transcript turns committed transactions into the messages of a
// model request (§4.7). It uses only the public API of the core: another
// agent loop may build its history differently.
package transcript

import (
	"fmt"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/internal/deepcopy"
)

// MissingResult is the text of the error result Build puts in for a tool
// call that has none (the transaction was cancelled or the tool failed):
// the provider rejects a tool_use without a tool_result.
const MissingResult = "No result: the tool call was interrupted."

// Build returns the conversation recorded in txs (usually Journal.Txs()):
//
//   - user.message becomes a user message: its text, then its blocks;
//   - assistant.message and the tool.call events of the same transaction
//     become one assistant message: thinking and text blocks, then tool_use;
//   - tool.result becomes a tool_result block of the next user message;
//   - the last compacted{UpTo, Summary} replaces transactions up to UpTo
//     with a user message holding Summary;
//   - other kinds are ignored.
//
// A tool_use without a result gets an error result with MissingResult; a
// result is matched only to a call of its own transaction, extra and
// orphaned results are dropped. Consecutive messages of one role are merged,
// so tool results and a message the user typed during the step go to the
// model together, results first. Empty messages are dropped.
//
// The messages share nothing with txs: Mutate hooks on llm.request may change
// them without touching the journal.
func Build(txs []aish.Tx) []events.Message {
	var b builder
	upTo, summary := lastCompaction(txs)
	if summary != "" {
		b.add(events.RoleUser, events.Block{Type: events.BlockText, Text: summary})
	}
	for _, tx := range txs {
		if tx.Seq > upTo {
			b.tx(tx)
		}
	}

	return deepcopy.Copy(b.msgs)
}

func lastCompaction(txs []aish.Tx) (upTo uint64, summary string) {
	for _, tx := range txs {
		for _, env := range tx.Events {
			if c, ok := env.Event.(events.Compacted); ok {
				upTo, summary = c.UpTo, c.Summary
			}
		}
	}

	return upTo, summary
}

type builder struct {
	msgs []events.Message
}

// add appends blocks as a message of role, merging with the previous
// message of the same role.
func (b *builder) add(role string, blocks ...events.Block) {
	if len(blocks) == 0 {
		return
	}
	if n := len(b.msgs); n > 0 && b.msgs[n-1].Role == role {
		b.msgs[n-1].Content = append(b.msgs[n-1].Content, blocks...)

		return
	}
	b.msgs = append(b.msgs, events.Message{Role: role, Content: blocks})
}

func (b *builder) tx(tx aish.Tx) {
	results := map[string]events.ToolResult{}
	for _, env := range tx.Events {
		if r, ok := env.Event.(events.ToolResult); ok {
			if _, dup := results[r.CallID]; !dup {
				results[r.CallID] = r
			}
		}
	}

	calls := map[string]events.ToolCall{}
	for _, env := range tx.Events {
		if c, ok := env.Event.(events.ToolCall); ok {
			calls[c.ID] = c
		}
	}
	placed := map[string]bool{} // calls already in the message at their stream position

	var pending []events.ToolCall // calls of the last assistant message
	flush := func() {
		for _, c := range pending {
			b.add(events.RoleUser, resultBlock(c, results))
		}
		pending = nil
	}

	for _, env := range tx.Events {
		switch ev := env.Event.(type) {
		case events.UserMessage:
			flush()
			b.add(events.RoleUser, userBlocks(ev)...)
		case events.AgentReturn:
			flush()
			b.add(events.RoleUser, agentReturnBlock(ev))
		case events.AssistantMessage:
			flush()
			b.add(events.RoleAssistant, placeCalls(ev.Blocks(), calls, placed)...)
		case events.ToolCall:
			if !placed[ev.ID] {
				b.add(events.RoleAssistant, events.Block{Type: events.BlockToolUse, ID: ev.ID, Name: ev.Name, Input: ev.Input})
			}
			pending = append(pending, ev)
		default:
		}
	}
	flush()
}

// placeCalls fills in the tool_use blocks the stream marked with their calls
// and drops a marker whose call is gone (the provider discarded it). Calls
// without a marker, from providers that do not write one, are added after the
// message by the caller.
func placeCalls(blocks []events.Block, calls map[string]events.ToolCall, placed map[string]bool) []events.Block {
	out := blocks[:0:0]
	for i := range blocks {
		b := blocks[i]
		if b.Type == events.BlockToolUse {
			c, ok := calls[b.ID]
			if !ok {
				continue
			}
			b.Name, b.Input = c.Name, c.Input
			placed[c.ID] = true
		}
		out = append(out, b)
	}

	return out
}

func userBlocks(m events.UserMessage) []events.Block {
	var out []events.Block
	if m.Text != "" {
		out = append(out, events.Block{Type: events.BlockText, Text: m.Text})
	}

	return append(out, m.Blocks...)
}

// agentReturnBlock wraps the answer in a tag so the model can tell it from
// text the user wrote.
func agentReturnBlock(r events.AgentReturn) events.Block {
	attrs := fmt.Sprintf("agent=%q call=%q", r.Agent, r.CallID)
	if r.IsError {
		attrs += " error=\"true\""
	}

	return events.Block{Type: events.BlockText, Text: "<agent_return " + attrs + ">\n" + r.Text + "\n</agent_return>"}
}

func resultBlock(c events.ToolCall, results map[string]events.ToolResult) events.Block {
	r, ok := results[c.ID]
	if !ok {
		return events.Block{
			Type: events.BlockToolResult, ToolUseID: c.ID, IsError: true,
			Content: []events.Block{{Type: events.BlockText, Text: MissingResult}},
		}
	}

	return events.Block{Type: events.BlockToolResult, ToolUseID: c.ID, Content: r.Content, IsError: r.IsError}
}
