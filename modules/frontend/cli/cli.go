// Package cli is a line-based REPL frontend: every input line is a user
// message, except while the agent asks a question, when the next line is the
// answer.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	source = "frontend.cli"
	// lineQueue is how many lines typed during a step wait for their turn.
	lineQueue = 64
	// maxLine is the longest input line.
	maxLine = 1 << 20
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "frontend.cli", New: func() aish.Module { return &CLI{} }})
}

type CLI struct {
	// In and Out default to os.Stdin and os.Stdout. Ctrl+C cancels the open
	// transaction only when In is os.Stdin.
	In  io.Reader
	Out io.Writer

	h    *aish.Host
	done chan struct{}

	mu     sync.Mutex
	answer chan string // set while a question waits for its answer
	eof    bool
}

func (c *CLI) Init(_ context.Context, h *aish.Host) error {
	c.h, c.done = h, make(chan struct{})
	if c.Out == nil {
		c.Out = os.Stdout
	}
	h.Handle("show-message", c.showMessage)
	h.Handle("ask", c.ask)
	h.Handle("show-call", c.showCall, aish.Priority(0))

	return nil
}

// Done is closed once the input has ended and every line has been sent. The
// agent may still be working then: wait for the session to be idle.
func (c *CLI) Done() <-chan struct{} { return c.done }

func (c *CLI) Start(ctx context.Context) error {
	in := c.In
	if in == nil {
		in = os.Stdin
		c.cancelOnInterrupt(ctx)
	}
	c.printHistory()

	lines := make(chan string)
	go readLines(in, lines)
	msgs := make(chan string, lineQueue)
	go c.dispatch(lines, msgs)
	go c.send(ctx, msgs)

	return nil
}

// cancelOnInterrupt turns Ctrl+C into cancelling the open transaction; the
// session ends with end of input.
func (c *CLI) cancelOnInterrupt(ctx context.Context) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-sig:
				c.h.Cancel()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// readLines blocks on in, which cannot be interrupted: after Close the
// goroutine lives until the process exits or in ends.
func readLines(in io.Reader, lines chan<- string) {
	defer close(lines)
	sc := bufio.NewScanner(in)
	sc.Buffer(nil, maxLine)
	for sc.Scan() {
		lines <- sc.Text()
	}
}

// dispatch hands each line to the pending question, or else queues it as a
// message.
func (c *CLI) dispatch(lines <-chan string, msgs chan<- string) {
	for line := range lines {
		c.mu.Lock()
		answer := c.answer
		c.mu.Unlock()
		if answer != nil {
			answer <- line

			continue
		}
		msgs <- line
	}
	c.mu.Lock()
	c.eof = true
	if c.answer != nil {
		close(c.answer)
	}
	c.mu.Unlock()
	close(msgs)
}

// send emits the messages one at a time: Emit returns when the message has
// committed, so a line typed during a step waits for the end of that step.
func (c *CLI) send(ctx context.Context, msgs <-chan string) {
	defer close(c.done)
	for text := range msgs {
		if strings.TrimSpace(text) == "" {
			continue
		}
		err := c.h.Emit(ctx, events.UserMessage{Text: text, Source: source})
		if err != nil && !errors.Is(err, aish.ErrUnhandled) {
			c.h.Log().Error("cannot send message", "err", err)
		}
	}
}

func (c *CLI) printHistory() {
	for _, tx := range c.h.Journal().Txs() {
		for _, env := range tx.Events {
			switch e := env.Event.(type) {
			case events.UserMessage:
				c.printf("> %s\n", e.Text)
			case events.AssistantMessage:
				c.printf("%s\n", e.Text())
			case events.ToolCall:
				c.printCall(e)
			default:
			}
		}
	}
}

func (c *CLI) showMessage(ctx context.Context, m events.AssistantMessage) error {
	for d := range m.Content.Chunks(ctx) {
		if d.Type == events.DeltaText {
			c.printf("%s", d.Text)
		}
	}
	c.printf("\n")
	if err := m.Content.Err(); err != nil {
		return fmt.Errorf("aish: frontend.cli: %w", err)
	}

	return nil
}

func (c *CLI) showCall(_ context.Context, call events.ToolCall) error {
	c.printCall(call)

	return nil
}

func (c *CLI) printCall(call events.ToolCall) {
	label := "tool"
	if call.Denied {
		label = "denied"
	}
	c.printf("[%s] %s %s\n", label, call.Name, call.Input)
}

// ask prints the question and takes the next input line as the answer. It
// asks again until the line matches an option; without options any line is
// the text of the answer. At end of input the answer is the last option.
func (c *CLI) ask(ctx context.Context, q events.UserQuestion) error {
	answer := make(chan string, 1)
	c.mu.Lock()
	if c.eof {
		close(answer)
	}
	c.answer = answer
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.answer = nil
		c.mu.Unlock()
	}()

	for {
		c.printf("%s [%s] ", q.Text, strings.Join(q.Options, "/"))
		select {
		case line, open := <-answer:
			if !open {
				return c.reply(ctx, events.UserAnswer{QuestionID: q.ID, Choice: last(q.Options)})
			}
			if len(q.Options) == 0 {
				return c.reply(ctx, events.UserAnswer{QuestionID: q.ID, Text: line})
			}
			if choice, ok := match(q.Options, line); ok {
				return c.reply(ctx, events.UserAnswer{QuestionID: q.ID, Choice: choice})
			}
		case <-ctx.Done():
			return fmt.Errorf("aish: frontend.cli: %w", context.Cause(ctx))
		}
	}
}

func (c *CLI) reply(ctx context.Context, a events.UserAnswer) error {
	c.printf("\n")
	if err := c.h.Emit(ctx, a); err != nil {
		return fmt.Errorf("aish: frontend.cli: %w", err)
	}

	return nil
}

// match finds the option the line names, by its full text or first letter.
func match(options []string, line string) (string, bool) {
	line = strings.TrimSpace(line)
	for _, o := range options {
		if line != "" && o != "" && (strings.EqualFold(line, o) || strings.EqualFold(line, o[:1])) {
			return o, true
		}
	}

	return "", false
}

func last(options []string) string {
	if len(options) == 0 {
		return ""
	}

	return options[len(options)-1]
}

func (c *CLI) printf(format string, args ...any) {
	if _, err := fmt.Fprintf(c.Out, format, args...); err != nil {
		c.h.Log().Error("cannot write output", "err", err)
	}
}
