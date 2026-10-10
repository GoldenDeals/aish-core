package aish

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// Corner cases of streams.

type holder struct {
	Map    map[string]Stream[string]
	List   []Stream[string]
	Any    any
	Nil    *Stream[string]
	hidden Stream[string]
}

func (holder) Kind() string { return "test.holder" }

type embedsNil struct {
	*Stream[string]
}

func (embedsNil) Kind() string { return "test.embeds_nil" }

func isDone[C any](s Stream[C]) bool {
	select {
	case <-s.Done():
		return true
	default:
		return false
	}
}

func TestStreamWritesAfterAbortAreDropped(t *testing.T) {
	t.Parallel()
	s, w := NewStream[string]()
	w.Write("kept")
	s.b.abort(context.Canceled)
	w.Write("dropped")
	w.Close(nil)

	if !slices.Equal(s.Body(), []string{"kept"}) || !errors.Is(s.Err(), context.Canceled) {
		t.Errorf("Body = %v, Err = %v; want what came before the abort and its error", s.Body(), s.Err())
	}
}

func TestStreamStateBeforeClose(t *testing.T) {
	t.Parallel()
	s, w := NewStream[string]()
	w.Write("a")
	if isDone(s) || s.Err() != nil || !slices.Equal(s.Body(), []string{"a"}) {
		t.Errorf("open stream: done=%v err=%v body=%v; want open with the partial body", isDone(s), s.Err(), s.Body())
	}
	boom := errors.New("boom")
	w.Close(boom)
	if got := drain(s.Chunks(t.Context())); !slices.Equal(got, []string{"a"}) || !errors.Is(s.Err(), boom) || !isDone(s) {
		t.Errorf("failed stream: chunks %v, err %v; want the chunks, then the error", got, s.Err())
	}
}

func TestManyReadersWhileWriting(t *testing.T) {
	t.Parallel()
	s, w := NewStream[int]()
	const readers, chunks = 8, 500

	var wg sync.WaitGroup
	got := make([][]int, readers)
	for i := range readers {
		wg.Go(func() { got[i] = drain(s.Chunks(t.Context())) })
	}
	for i := range chunks {
		w.Write(i)
	}
	w.Close(nil)
	wg.Wait()

	for i, g := range got {
		if len(g) != chunks || g[0] != 0 || g[chunks-1] != chunks-1 {
			t.Errorf("reader %d got %d chunks, want all %d in order", i, len(g), chunks)
		}
	}
}

func TestCommitFindsStreamsInContainers(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	inMap, wm := NewStream[string]()
	inList, wl := NewStream[string]()
	inAny, wa := NewStream[string]()
	hidden, _ := NewStream[string]() // never closed: unexported fields are not awaited
	go func() {
		for _, w := range []*StreamWriter[string]{wm, wl, wa} {
			time.Sleep(5 * time.Millisecond)
			w.Close(nil)
		}
	}()

	ev := holder{
		Map:    map[string]Stream[string]{"k": inMap},
		List:   []Stream[string]{inList, inList}, // the same stream twice is awaited once
		Any:    inAny,
		hidden: hidden,
	}
	if err := h.Emit(ctx, ev, embedsNil{}); !errors.Is(err, ErrUnhandled) {
		t.Fatal(err)
	}
	for name, st := range map[string]Stream[string]{"map": inMap, "list": inList, "any": inAny} {
		if !isDone(st) {
			t.Errorf("the transaction committed before the stream in %s closed", name)
		}
	}
	if s.lastTx(t).Status != StatusCommitted {
		t.Error("transaction did not commit")
	}
}

func TestRemovedEventsStreamsAreNotAwaited(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")
	h.Mutate("drop", func(_ context.Context, m *Mut[message]) error { m.Remove(); return nil })

	never, _ := NewStream[string]()
	done := make(chan error, 1)
	go func() { done <- h.Emit(ctx, message{Text: never}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the commit waits for a stream of a removed event")
	}
}

func TestStreamCopiesShareTheBuffer(t *testing.T) {
	t.Parallel()
	s, ctx := newTestSession(t)
	h := s.host("m")

	stream, w := NewStream[string]()
	var handled, observed []string
	h.Handle("first", func(ctx context.Context, m message) error {
		w.Write("x")
		w.Close(nil)
		handled = drain(m.Text.Chunks(ctx))

		return nil
	})
	h.Observe("obs", func(_ context.Context, m message) { observed = m.Text.Body() })

	if err := h.Emit(ctx, message{Text: stream}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(handled, []string{"x"}) || !slices.Equal(observed, []string{"x"}) {
		t.Errorf("handle saw %v, observe saw %v; the copies must read the same buffer", handled, observed)
	}
}
