package tui

import (
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/GoldenDeals/aish-core/events"
)

type harness struct {
	m         *model
	sent      []string
	cancelled bool
}

func newHarness() *harness {
	h := &harness{}
	h.m = newModel("s1", "main", &atomic.Int32{}, &atomic.Bool{},
		func() []entry { return nil }, func(s string) { h.sent = append(h.sent, s) }, func() { h.cancelled = true })
	h.m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})

	return h
}

func (h *harness) typeLine(line string) tea.Cmd {
	h.m.in.SetValue(line)
	_, cmd := h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	return cmd
}

func TestMessageIsSentAndEchoed(t *testing.T) {
	t.Parallel()

	h := newHarness()
	cmd := h.typeLine("hello")

	if len(h.sent) != 1 || h.sent[0] != "hello" || cmd == nil {
		t.Fatalf("sent %v, cmd %v", h.sent, cmd)
	}
}

func TestLiveTailIsShownInView(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.m.Update(busyMsg(true))
	h.m.Update(liveMsg{kind: kindAssistant, text: "partial answer"})

	view := ansi.Strip(h.m.View().Content)
	if !strings.Contains(view, "● partial answer") || !strings.Contains(view, "esc to interrupt") {
		t.Fatalf("view:\n%s", view)
	}
	h.m.Update(liveMsg{})
	if strings.Contains(h.m.View().Content, "partial answer") {
		t.Fatal("an empty liveMsg must clear the tail")
	}
}

func TestQuestionMenu(t *testing.T) {
	t.Parallel()

	h := newHarness()
	answer := make(chan events.UserAnswer, 1)
	h.m.Update(askMsg{q: events.UserQuestion{ID: "q1", Text: "Allow fs_edit?", Options: []string{"yes", "no"}}, answer: answer})

	if view := ansi.Strip(h.m.View().Content); !strings.Contains(view, "❯ 1. yes") || !strings.Contains(view, "2. no") {
		t.Fatalf("menu not shown:\n%s", view)
	}
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := <-answer; got.Choice != "no" || got.QuestionID != "q1" || h.m.ask != nil {
		t.Fatalf("answer %+v", got)
	}

	h.m.Update(askMsg{q: events.UserQuestion{ID: "q2", Text: "Allow?", Options: []string{"yes", "no"}}, answer: answer})
	h.m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if got := <-answer; got.Choice != "yes" {
		t.Fatalf("a letter must pick its option: %+v", got)
	}
}

func TestFreeTextQuestion(t *testing.T) {
	t.Parallel()

	h := newHarness()
	answer := make(chan events.UserAnswer, 1)
	h.m.Update(askMsg{q: events.UserQuestion{ID: "q1", Text: "Name?"}, answer: answer})
	h.typeLine("bob")

	if got := <-answer; got.Text != "bob" || len(h.sent) != 0 {
		t.Fatalf("answer %+v, sent %v", got, h.sent)
	}
}

func TestInterrupt(t *testing.T) {
	t.Parallel()

	h := newHarness()
	_, cmd := h.m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil || h.cancelled {
		t.Fatal("idle ctrl+c must quit")
	}
	h.m.Update(busyMsg(true))
	_, cmd = h.m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil || !h.cancelled {
		t.Fatal("esc must cancel the transaction")
	}
}

func TestPaste(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.m.Update(tea.PasteMsg{Content: "pasted"})
	if h.m.in.Value() != "pasted" {
		t.Fatalf("input %q", h.m.in.Value())
	}
}

func TestPrintable(t *testing.T) {
	t.Parallel()

	if got := printable("\x1b[31mred\x1b[0m\r\nA\tB\x07"); got != "red\nA    B" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderFitsTheWidth(t *testing.T) {
	t.Parallel()

	for _, e := range []entry{
		{kind: kindAssistant, text: strings.Repeat("word ", 40)},
		{kind: kindResult, text: strings.Repeat("x", 200) + "\n1\n2\n3\n4\n5"},
		{kind: kindTool, agent: "coder X1", text: "fs_read({})"},
	} {
		for _, line := range strings.Split(render(e, 40), "\n") {
			if w := len([]rune(stripANSI(line))); w > 40 {
				t.Errorf("%s line is %d wide: %q", e.kind, w, line)
			}
		}
	}
	if out := render(entry{kind: kindResult, text: "1\n2\n3\n4\n5\n6"}, 40); !strings.Contains(out, "+2 lines") {
		t.Errorf("long results must be cut:\n%s", out)
	}
}

func stripANSI(s string) string { return ansi.Strip(s) }
