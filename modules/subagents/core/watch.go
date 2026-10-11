package core

import (
	"context"
	"fmt"
	"strings"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	// activityQueue is how many entries wait for the parent while it is busy.
	activityQueue = 256
	// maxActivityText is the longest Activity.Text, in runes.
	maxActivityText = 400
)

// Activity types.
const (
	ActivityPrompt  = "prompt"
	ActivityMessage = "message"
	ActivityTool    = "tool"
	ActivityDenied  = "denied"
	ActivityResult  = "result"
	ActivityError   = "error"
)

// Activity is a clipped summary of one event the agent of CallID committed,
// for frontends that show what a subagent is doing.
type Activity struct {
	CallID  string
	Agent   string
	Session string
	Type    string
	Text    string
}

// watcher is an extra instance of the child session: it hands the child's
// committed events to the parent through out.
type watcher struct {
	callID, agent string
	out           chan<- Activity
}

func (w *watcher) Init(_ context.Context, h *aish.Host) error {
	h.Observe("watch", func(_ context.Context, c aish.TxCommitted) {
		for _, a := range summarize(c.Tx) {
			a.CallID, a.Agent, a.Session = w.callID, w.agent, h.Session().ID
			// shortcut: a parent that falls activityQueue entries behind loses entries, never stalls the child.
			select {
			case w.out <- a:
			default:
			}
		}
	})

	return nil
}

func summarize(tx aish.Tx) []Activity {
	var out []Activity
	for _, env := range tx.Events {
		switch e := env.Event.(type) {
		case events.UserMessage:
			out = append(out, Activity{Type: ActivityPrompt, Text: clip(e.Text)})
		case events.AssistantMessage:
			if text := e.Text(); text != "" {
				out = append(out, Activity{Type: ActivityMessage, Text: clip(text)})
			}
		case events.ToolCall:
			typ := ActivityTool
			if e.Denied {
				typ = ActivityDenied
			}
			out = append(out, Activity{Type: typ, Text: clip(fmt.Sprintf("%s %s", e.Name, e.Input))})
		case events.ToolResult:
			typ := ActivityResult
			if e.IsError {
				typ = ActivityError
			}
			out = append(out, Activity{Type: typ, Text: clip(blocksText(e.Content))})
		default:
		}
	}

	return out
}

func blocksText(blocks []events.Block) string {
	var b strings.Builder
	for i := range blocks {
		b.WriteString(blocks[i].Text)
	}

	return b.String()
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxActivityText {
		return string(r[:maxActivityText]) + "…"
	}

	return s
}
