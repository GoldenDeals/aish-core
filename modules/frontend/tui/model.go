package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/GoldenDeals/aish-core/events"
)

const (
	kindUser      = "user"
	kindAssistant = "assistant"
	kindThinking  = "thinking"
	kindTool      = "tool"
	kindResult    = "result"
	kindNote      = "note"
	kindError     = "error"

	// maxResult and maxResultLines are how much of a tool result the transcript shows.
	maxResult      = 400
	maxResultLines = 4
	// defaultWidth is used until the terminal reports its size.
	defaultWidth = 80
)

var (
	dim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	green    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	red      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	accent   = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	bold     = lipgloss.NewStyle().Bold(true)
	userBar  = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	boxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8")).Padding(0, 1)
	spinning = spinner.Spinner{Frames: []string{"✻", "✽", "✶", "✳", "✢"}, FPS: time.Second / 6}
)

// entry is one finished block of the transcript.
type entry struct {
	kind  string
	text  string
	agent string // set for what a subagent did: "<agent> <session tail>"
	cont  bool   // continues the assistant message above: no bullet
}

type (
	addMsg   struct{ entry }
	liveMsg  entry // the unfinished tail of the reply being streamed; zero value clears it
	busyMsg  bool
	usageMsg events.Usage
	askMsg   struct {
		q      events.UserQuestion
		answer chan<- events.UserAnswer
	}
)

type model struct {
	session, agent string
	history        func() []entry
	submit         func(string)
	cancel         func()
	width          *atomic.Int32 // read by hooks that print entries themselves

	in        textinput.Model
	spin      spinner.Model
	live      entry
	thinking  bool
	busy      bool
	busySince time.Time
	usage     events.Usage
	ask       *askMsg
	choice    int // selected option of the open question
	ready     bool
	hideSub   *atomic.Bool // read by hooks that print subagent entries themselves
}

func newModel(session, agent string, width *atomic.Int32, hideSub *atomic.Bool, history func() []entry, submit func(string), cancel func()) *model {
	in := textinput.New()
	in.Prompt = "> "
	in.Focus()

	return &model{
		session: session, agent: agent, width: width, hideSub: hideSub, history: history, submit: submit, cancel: cancel,
		in: in, spin: spinner.New(spinner.WithSpinner(spinning), spinner.WithStyle(accent)),
	}
}

func (m *model) Init() tea.Cmd { return m.spin.Tick }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width.Store(int32(msg.Width)) //nolint:gosec // a terminal width fits int32
		m.in.SetWidth(max(msg.Width-len(m.in.Prompt)-6, 1))
		if !m.ready {
			m.ready = true

			cmd := m.printAll(append([]entry{{kind: kindNote, text: m.banner()}}, m.history()...))

			return m, cmd
		}
	case addMsg:
		return m, tea.Println(m.render(msg.entry))
	case liveMsg:
		m.thinking = msg.kind == kindThinking
		m.live = entry(msg)
		if m.thinking {
			m.live = entry{}
		}
	case busyMsg:
		if bool(msg) && !m.busy {
			m.busySince = time.Now()
		}
		m.busy = bool(msg)
		if !m.busy {
			m.live = entry{}
		}
	case usageMsg:
		m.usage = events.Usage(msg)
	case askMsg:
		m.ask, m.choice = &msg, 0
		m.in.Reset()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)

		return m, cmd
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(msg)

	return m, cmd
}

func (m *model) printAll(es []entry) tea.Cmd {
	cmds := make([]tea.Cmd, len(es))
	for i, e := range es {
		cmds[i] = tea.Println(m.render(e))
	}

	return tea.Sequence(cmds...)
}

func (m *model) banner() string {
	dir, _ := os.Getwd()

	return fmt.Sprintf("Welcome to aish\n\n  agent %s · session %s\n  cwd: %s", m.agent, m.session, dir)
}

func (m *model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		if m.busy {
			m.interrupt()

			return m, nil
		}

		return m, tea.Quit
	case "esc":
		m.interrupt()

		return m, nil
	case "ctrl+t":
		m.hideSub.Store(!m.hideSub.Load())

		return m, nil
	case "ctrl+d":
		return m, tea.Quit
	}
	if m.ask != nil && len(m.ask.q.Options) > 0 {
		cmd := m.pick(k)

		return m, cmd
	}
	if k.String() == "enter" {
		cmd := m.enter()

		return m, cmd
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(k)

	return m, cmd
}

func (m *model) interrupt() {
	if m.busy {
		m.cancel()
	}
	m.ask = nil
}

// pick drives the option menu: arrows and digits choose, enter confirms, a
// letter picks the option it starts, esc declines with the last option.
func (m *model) pick(k tea.KeyPressMsg) tea.Cmd {
	opts := m.ask.q.Options
	switch s := k.String(); s {
	case "up", "shift+tab":
		m.choice = (m.choice + len(opts) - 1) % len(opts)
	case "down", "tab":
		m.choice = (m.choice + 1) % len(opts)
	case "enter":
		return m.reply(opts[m.choice])
	default:
		if len(s) != 1 {
			break
		}
		if i := int(s[0]) - '1'; i >= 0 && i < len(opts) {
			return m.reply(opts[i])
		}
		if a, ok := answer(m.ask.q, s); ok {
			return m.reply(a.Choice)
		}
	}

	return nil
}

func (m *model) reply(choice string) tea.Cmd {
	a := events.UserAnswer{QuestionID: m.ask.q.ID, Choice: choice}
	m.ask.answer <- a
	m.ask = nil

	return nil
}

// enter answers a free-text question or sends a message.
func (m *model) enter() tea.Cmd {
	line := strings.TrimSpace(m.in.Value())
	if line == "" {
		return nil
	}
	m.in.Reset()
	if m.ask != nil {
		m.ask.answer <- events.UserAnswer{QuestionID: m.ask.q.ID, Text: line}
		m.ask = nil

		return nil
	}
	text := line
	if m.busy {
		text += dim.Render("  (queued: the agent sees it after the current step)")
	}
	m.submit(line)

	return tea.Println(m.render(entry{kind: kindUser, text: text}))
}

// answer turns the line into the answer to q. With options the line must name
// one by its full text or first letter; without, any line is the text.
func answer(q events.UserQuestion, line string) (events.UserAnswer, bool) {
	if len(q.Options) == 0 {
		return events.UserAnswer{QuestionID: q.ID, Text: line}, true
	}
	for _, o := range q.Options {
		if o != "" && (strings.EqualFold(line, o) || strings.EqualFold(line, o[:1])) {
			return events.UserAnswer{QuestionID: q.ID, Choice: o}, true
		}
	}

	return events.UserAnswer{}, false
}

func (m *model) w() int {
	if w := int(m.width.Load()); w > 0 {
		return w
	}

	return defaultWidth
}

func (m *model) render(e entry) string { return render(e, m.w()) }

// render draws e the way Claude Code does: a bullet for the agent's text and
// calls, a hanging "⎿" for results, a gray bar for what the user typed.
func render(e entry, width int) string {
	text := printable(e.text)
	indent := ""
	if e.agent != "" {
		indent = "  "
		text = "[" + e.agent + "] " + text
	}
	avail := max(width-len(indent), 10)

	var first, rest string
	switch e.kind {
	case kindUser:
		return userBar.Width(width).Render("> " + text)
	case kindAssistant:
		first, rest = "● ", "  "
	case kindTool:
		first, rest, text = green.Render("● "), "  ", bold.Render(text)
	case kindResult:
		first, rest, text = dim.Render("  ⎿  "), "     ", dim.Render(clipLines(text))
	case kindError:
		first, rest, text = red.Render("● "), "  ", red.Render(text)
	default:
		first, rest, text = accent.Render("✻ "), "  ", dim.Render(text)
	}
	if e.cont {
		first = rest
	}
	body := lipgloss.NewStyle().Width(avail - ansi.StringWidth(rest)).Render(text)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		lines[i] = indent + p + strings.TrimRight(l, " ")
	}

	return strings.Join(lines, "\n")
}

func clipLines(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxResultLines {
		return s
	}

	return strings.Join(lines[:maxResultLines], "\n") + fmt.Sprintf("\n… +%d lines", len(lines)-maxResultLines)
}

func (m *model) View() tea.View {
	w := m.w()
	var parts []string
	if m.live.text != "" {
		parts = append(parts, m.render(m.live))
	}
	if m.busy && m.ask == nil {
		parts = append(parts, m.status())
	}
	parts = append(parts, m.prompt(w), m.footer(w))

	return tea.NewView(strings.Join(parts, "\n"))
}

func (m *model) status() string {
	label := "Working…"
	if m.thinking {
		label = "Thinking…"
	}

	return fmt.Sprintf("%s %s %s", m.spin.View(), accent.Render(label),
		dim.Render(fmt.Sprintf("(esc to interrupt · %ds · ↓ %s tokens)", int(time.Since(m.busySince).Seconds()), compact(m.usage.OutputTokens))))
}

// prompt is the input box, or the question with its options in its place.
func (m *model) prompt(w int) string {
	box := boxStyle.Width(w)
	if m.ask == nil {
		return box.Render(m.in.View())
	}
	var b strings.Builder
	b.WriteString(bold.Render(printable(m.ask.q.Text)))
	for i, o := range m.ask.q.Options {
		cursor := "  "
		if i == m.choice {
			cursor = accent.Render("❯ ")
		}
		fmt.Fprintf(&b, "\n%s%d. %s", cursor, i+1, o)
	}
	if len(m.ask.q.Options) == 0 {
		b.WriteString("\n" + m.in.View())
	}

	return boxStyle.BorderForeground(lipgloss.Color("208")).Width(w).Render(b.String())
}

func (m *model) footer(w int) string {
	subs := "on"
	if m.hideSub.Load() {
		subs = "off"
	}
	hint := "esc interrupt · ctrl+t subagents " + subs + " · ctrl+d quit"
	if m.ask != nil && len(m.ask.q.Options) > 0 {
		hint = "↑/↓ choose · enter confirm · esc " + last(m.ask.q.Options)
	}
	line := fmt.Sprintf("  %s · %s · ↑ %s ↓ %s tokens · %s", m.agent, m.session,
		compact(m.usage.InputTokens), compact(m.usage.OutputTokens), hint)

	return dim.Render(ansi.Truncate(line, w, "…"))
}

func compact(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}

	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// clip shortens a tool result to what the transcript shows.
func clip(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxResult {
		return s
	}

	return string(r[:maxResult]) + "…"
}

// printable removes what would move the cursor or recolour the screen: tool
// output carries escape sequences, carriage returns and tabs.
func printable(s string) string {
	s = strings.ReplaceAll(ansi.Strip(s), "\t", "    ")

	return strings.Map(func(r rune) rune {
		if r == '\n' || r >= ' ' && r != 0x7f {
			return r
		}

		return -1
	}, s)
}
