// Package transcript turns committed transactions into the messages of a
// model request (§4.7). It uses only the public API of the core: another
// agent loop may build its history differently.
package transcript

import (
	aish "github.com/GoldenDeals/aish-core"
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
func Build(txs []aish.Tx) []aish.Message {
	var b builder
	upTo, summary := lastCompaction(txs)
	if summary != "" {
		b.add(aish.RoleUser, aish.Block{Type: aish.BlockText, Text: summary})
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
			if c, ok := env.Event.(aish.Compacted); ok {
				upTo, summary = c.UpTo, c.Summary
			}
		}
	}

	return upTo, summary
}

type builder struct {
	msgs []aish.Message
}

// add appends blocks as a message of role, merging with the previous
// message of the same role.
func (b *builder) add(role string, blocks ...aish.Block) {
	if len(blocks) == 0 {
		return
	}
	if n := len(b.msgs); n > 0 && b.msgs[n-1].Role == role {
		b.msgs[n-1].Content = append(b.msgs[n-1].Content, blocks...)

		return
	}
	b.msgs = append(b.msgs, aish.Message{Role: role, Content: blocks})
}

func (b *builder) tx(tx aish.Tx) {
	results := map[string]aish.ToolResult{}
	for _, env := range tx.Events {
		if r, ok := env.Event.(aish.ToolResult); ok {
			if _, dup := results[r.CallID]; !dup {
				results[r.CallID] = r
			}
		}
	}

	var pending []aish.ToolCall // calls of the last assistant message
	flush := func() {
		for _, c := range pending {
			b.add(aish.RoleUser, resultBlock(c, results))
		}
		pending = nil
	}

	for _, env := range tx.Events {
		switch ev := env.Event.(type) {
		case aish.UserMessage:
			flush()
			b.add(aish.RoleUser, userBlocks(ev)...)
		case aish.AssistantMessage:
			flush()
			b.add(aish.RoleAssistant, ev.Blocks()...)
		case aish.ToolCall:
			b.add(aish.RoleAssistant, aish.Block{Type: aish.BlockToolUse, ID: ev.ID, Name: ev.Name, Input: ev.Input})
			pending = append(pending, ev)
		default:
		}
	}
	flush()
}

func userBlocks(m aish.UserMessage) []aish.Block {
	var out []aish.Block
	if m.Text != "" {
		out = append(out, aish.Block{Type: aish.BlockText, Text: m.Text})
	}

	return append(out, m.Blocks...)
}

func resultBlock(c aish.ToolCall, results map[string]aish.ToolResult) aish.Block {
	r, ok := results[c.ID]
	if !ok {
		return aish.Block{
			Type: aish.BlockToolResult, ToolUseID: c.ID, IsError: true,
			Content: []aish.Block{{Type: aish.BlockText, Text: MissingResult}},
		}
	}

	return aish.Block{Type: aish.BlockToolResult, ToolUseID: c.ID, Content: r.Content, IsError: r.IsError}
}
