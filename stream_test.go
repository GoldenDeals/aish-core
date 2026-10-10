package aish

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type message struct {
	Text Stream[string]
	Stop string
}

func (message) Kind() string { return "test.message" }

type output struct {
	Live *Stream[[]byte]
}

func (output) Kind() string { return "test.output" }

func drain[C any](ch <-chan C) []C {
	var out []C
	for c := range ch {
		out = append(out, c)
	}

	return out
}

func TestStreamReadersGetEverythingFromTheStart(t *testing.T) {
	t.Parallel()
	s, w := NewStream[string]()
	w.Write("a")
	early := s.Chunks(t.Context())
	w.Write("b")
	w.Close(nil)
	w.Write("dropped")
	late := s.Chunks(t.Context())

	want := []string{"a", "b"}
	if got := drain(early); !slices.Equal(got, want) {
		t.Errorf("early reader got %v, want %v", got, want)
	}
	if got := drain(late); !slices.Equal(got, want) {
		t.Errorf("late reader got %v, want %v", got, want)
	}
	if got := s.Body(); !slices.Equal(got, want) || s.Err() != nil {
		t.Errorf("Body = %v, Err = %v; want %v and nil", got, s.Err(), want)
	}

	failed, fw := NewStream[string]()
	boom := errors.New("boom")
	fw.Close(boom)
	fw.Close(nil)
	if !errors.Is(failed.Err(), boom) {
		t.Errorf("Err = %v, want the first Close error", failed.Err())
	}

	var zero Stream[string]
	if drain(zero.Chunks(t.Context())) != nil || zero.Body() != nil || zero.Err() != nil {
		t.Error("zero Stream is not closed and empty")
	}
	<-zero.Done()
}

func TestSlowReaderDoesNotBlockProducer(t *testing.T) {
	t.Parallel()
	s, w := NewStream[int]()
	stalled := s.Chunks(t.Context()) // never read until the producer is done

	done := make(chan struct{})
	go func() {
		for i := range 10000 {
			w.Write(i)
		}
		w.Close(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked by a reader that does not read")
	}

	if got := drain(stalled); len(got) != 10000 || got[9999] != 9999 {
		t.Errorf("stalled reader got %d chunks, want all 10000 in order", len(got))
	}
}

func TestStreamReaderStopsWithItsContext(t *testing.T) {
	t.Parallel()
	s, w := NewStream[string]()
	w.Write("a")
	ctx, cancel := context.WithCancel(t.Context())
	ch := s.Chunks(ctx)
	<-ch
	cancel()
	drain(ch)
	if s.Err() != nil {
		t.Error("a reader closed the stream")
	}
}

func TestStreamChunksAreCopies(t *testing.T) {
	t.Parallel()
	s, w := NewStream[[]byte]()
	w.Write([]byte("abc"))
	w.Close(nil)

	for c := range s.Chunks(t.Context()) {
		c[0] = 'X'
	}
	s.Body()[0][1] = 'Y'
	if got := string(s.Body()[0]); got != "abc" {
		t.Errorf("body = %q; a reader changed the shared buffer", got)
	}
}

func TestCommitWaitsForStreams(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("llm.fake")

	var shown []string
	h.Handle("show", func(ctx context.Context, m message) error {
		shown = drain(m.Text.Chunks(ctx))

		return nil
	})
	var observed []string
	h.Observe("store", func(_ context.Context, m message) { observed = m.Text.Body() })

	stream, w := NewStream[string]()
	live, lw := NewStream[[]byte]()
	closed := make(chan struct{})
	go func() {
		w.Write("hel")
		w.Write("lo")
		w.Close(nil)
		time.Sleep(20 * time.Millisecond)
		if n := s.journal.Len(); n != 0 {
			t.Errorf("journal has %d transactions while a stream is open", n)
		}
		lw.Write([]byte("out"))
		lw.Close(nil)
		close(closed)
	}()

	err := h.Emit(ctx, message{Text: stream, Stop: "end_turn"}, output{Live: &live}, output{})
	if !errors.Is(err, ErrUnhandled) { // nobody handles output
		t.Fatalf("Emit = %v, want only ErrUnhandled for output", err)
	}
	<-closed

	want := []string{"hel", "lo"}
	if !slices.Equal(shown, want) || !slices.Equal(observed, want) {
		t.Errorf("Handle saw %v, Observe saw %v; want %v", shown, observed, want)
	}
	tx := s.lastTx(t)
	if tx.Status != StatusCommitted {
		t.Errorf("status = %s, want committed", tx.Status)
	}
	if got := tx.Events[1].Event.(output).Live.Body(); string(got[0]) != "out" {
		t.Errorf("journaled live output = %q, want out", got)
	}
}

func TestCancelClosesOpenStreams(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("llm.fake")

	stream, w := NewStream[string]()
	w.Write("partial")
	h.Observe("started", func(context.Context, TxStarted) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			h.Cancel() // Ctrl+C while the producer is stuck
		}()
	})

	err := h.Emit(ctx, message{Text: stream})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Emit = %v, want context.Canceled", err)
	}
	if !errors.Is(stream.Err(), context.Canceled) {
		t.Errorf("stream Err = %v, want the cancellation", stream.Err())
	}
	tx := s.lastTx(t)
	if tx.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", tx.Status)
	}
	if body := tx.Events[0].Event.(message).Text.Body(); !slices.Equal(body, []string{"partial"}) {
		t.Errorf("journaled body = %v, want what was written before cancellation", body)
	}
	if !slices.ContainsFunc(tx.Trace, func(e TraceEntry) bool { return e.Action == TraceAbort }) {
		t.Error("trace has no abort entry")
	}
}

// TestMutateMayReplaceStream: a Mutate hook may wrap a stream (to redact
// secrets, say); the commit then waits for the stream it put in.
func TestMutateMayReplaceStream(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	h.Mutate("redact", func(ctx context.Context, m *Mut[message]) error {
		in := m.Event().Text
		out, w := NewStream[string]()
		go func() {
			for c := range in.Chunks(ctx) {
				w.Write(strings.ReplaceAll(c, "secret", "***"))
			}
			w.Close(in.Err())
		}()
		m.Event().Text = out

		return nil
	})

	stream, w := NewStream[string]()
	go func() {
		w.Write("the secret is 42")
		w.Close(nil)
	}()
	if err := h.Emit(ctx, message{Text: stream}); !errors.Is(err, ErrUnhandled) && err != nil {
		t.Fatal(err)
	}

	body := s.lastTx(t).Events[0].Event.(message).Text.Body()
	if !slices.Equal(body, []string{"the *** is 42"}) {
		t.Errorf("journaled body = %v, want the redacted stream", body)
	}
}
