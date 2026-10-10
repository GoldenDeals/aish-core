package trace

import (
	"errors"
	"strings"
	"testing"
	"time"

	aish "github.com/GoldenDeals/aish-core"
)

func TestWrite(t *testing.T) {
	t.Parallel()

	tx := aish.Tx{
		Seq:    3,
		Status: aish.StatusCancelled,
		Trace: []aish.TraceEntry{
			{Event: "3.1", Kind: "tool.call", Hook: "llm.fake", Action: aish.TraceEmit, Depth: 1},
			{Event: "3.1", Kind: "tool.call", Hook: "tool.fs:exec", Action: aish.TraceHandle, Depth: 1, Dur: 1500 * time.Microsecond, Err: "boom"},
			{Event: "3.2", Kind: "tool.result", Action: aish.TraceLimit, Depth: 17, Err: "limit"},
			{Action: aish.TraceWarn, Hook: "frontend.cli:ask", Err: "possible deadlock"},
		},
		Events: []aish.Envelope{{Meta: aish.Meta{ID: "3.1"}, Event: aish.UserMessage{}}},
	}
	var b strings.Builder
	if err := Write(&b, tx); err != nil {
		t.Fatal(err)
	}

	want := "tx 3 cancelled\n" +
		"  emit      3.1 tool.call llm.fake\n" +
		"  handle    3.1 tool.call tool.fs:exec (1.5ms): boom\n" +
		strings.Repeat("  ", 17) + "limit     3.2 tool.result: limit\n" +
		"  warn      frontend.cli:ask: possible deadlock\n" + // depth 0 entries print at depth 1
		"events: 3.1 user.message\n"
	if b.String() != want {
		t.Errorf("Write =\n%s\nwant\n%s", b.String(), want)
	}
}

func TestWriteEmptyTransaction(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if err := Write(&b, aish.Tx{Seq: 1, Status: aish.StatusCommitted}); err != nil {
		t.Fatal(err)
	}
	if b.String() != "tx 1 committed\nevents: \n" {
		t.Errorf("Write = %q", b.String())
	}
}

type failingWriter struct{}

var errDisk = errors.New("disk full")

func (failingWriter) Write([]byte) (int, error) { return 0, errDisk }

func TestWriteError(t *testing.T) {
	t.Parallel()
	if err := Write(failingWriter{}, aish.Tx{}); !errors.Is(err, errDisk) {
		t.Errorf("Write = %v, want the writer's error", err)
	}
}
