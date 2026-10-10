// Package trace prints the trace of committed transactions for aish --trace
// (§2, principle 8): who got, changed, removed and emitted which events.
package trace

import (
	"fmt"
	"io"
	"strings"

	aish "github.com/GoldenDeals/aish-core"
)

// Write prints tx as a tree: every step is indented by its Emit depth, so
// the events a hook emitted sit under the hook. Zero durations are left out,
// which keeps the output of a trace with Dur cleared stable for golden files.
//
//	tx 2 committed
//	  emit      2.1 tool.call llm.fake
//	  remove    2.1 tool.call policy.ask:authorize (40µs)
//	    emit      2.2 user.question policy.ask:authorize
//	events: 2.2 user.question
func Write(w io.Writer, tx aish.Tx) error {
	var b strings.Builder
	fmt.Fprintf(&b, "tx %d %s\n", tx.Seq, tx.Status)
	for _, e := range tx.Trace {
		b.WriteString(strings.Repeat("  ", max(e.Depth, 1)))
		fmt.Fprintf(&b, "%-9s", e.Action)
		for _, field := range []string{e.Event, e.Kind, e.Hook} {
			if field != "" {
				b.WriteString(" " + field)
			}
		}
		if e.Dur > 0 {
			fmt.Fprintf(&b, " (%s)", e.Dur)
		}
		if e.Err != "" {
			b.WriteString(": " + e.Err)
		}
		b.WriteByte('\n')
	}

	events := make([]string, len(tx.Events))
	for i, env := range tx.Events {
		events[i] = env.Meta.ID + " " + env.Event.Kind()
	}
	fmt.Fprintf(&b, "events: %s\n", strings.Join(events, ", "))

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("aish: write trace: %w", err)
	}

	return nil
}
