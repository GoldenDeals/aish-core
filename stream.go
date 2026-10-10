package aish

import (
	"context"
	"reflect"
	"sync"

	"github.com/GoldenDeals/aish-core/internal/deepcopy"
)

// Stream is the reading side of a stream of chunks carried by an event
// (§4.6). It is a handle: copies share one buffer, so every Handle and
// Observe hook reads the same stream without multiplying memory.
//
// An event carries a stream in a named field. A transaction commits only
// after every stream reachable from its events through exported fields is
// closed; on cancellation the core closes open streams with the
// cancellation error.
//
// The zero Stream is closed and empty.
type Stream[C any] struct {
	b *buffer[C]
}

// StreamWriter is the producing side. It is used from the producer's own
// goroutine: Emit of the stream event returns only after Handle hooks have
// read the stream to the end.
type StreamWriter[C any] struct {
	b *buffer[C]
}

func NewStream[C any]() (Stream[C], *StreamWriter[C]) {
	b := &buffer[C]{changed: make(chan struct{}), done: make(chan struct{})}

	return Stream[C]{b: b}, &StreamWriter[C]{b: b}
}

// Write appends a chunk. Writes after Close are dropped: the core may have
// closed the stream because the transaction was cancelled.
func (w *StreamWriter[C]) Write(c C) {
	w.b.mu.Lock()
	defer w.b.mu.Unlock()

	if w.b.closed {
		return
	}
	w.b.items = append(w.b.items, c)
	w.b.notify()
}

// Close finalizes the stream with err (nil on success). Only the first Close
// counts.
func (w *StreamWriter[C]) Close(err error) {
	w.b.close(err)
}

// Chunks returns the stream from its beginning: chunks already written, then
// new ones as they arrive. The channel is closed when the stream is closed
// and read to the end, or when ctx is done. Each chunk is a deep copy.
//
// The reading goroutine lives until then, so pass the hook ctx: the core
// cancels it when the transaction commits.
func (s Stream[C]) Chunks(ctx context.Context) <-chan C {
	out := make(chan C)
	if s.b == nil {
		close(out)

		return out
	}
	go s.b.read(ctx, out)

	return out
}

// Body returns deep copies of the chunks written so far; after Done it is
// the final body.
func (s Stream[C]) Body() []C {
	if s.b == nil {
		return nil
	}
	s.b.mu.Lock()
	items := s.b.items
	s.b.mu.Unlock()

	return deepcopy.Copy(items)
}

// Done is closed when the stream is closed.
func (s Stream[C]) Done() <-chan struct{} {
	if s.b == nil {
		return closedChan
	}

	return s.b.done
}

// Err returns the error the stream was closed with; nil while it is open.
func (s Stream[C]) Err() error {
	if s.b == nil {
		return nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()

	return s.b.err
}

func (s Stream[C]) buffer() streamBuffer {
	if s.b == nil {
		return nil
	}

	return s.b
}

var closedChan = func() chan struct{} {
	c := make(chan struct{})
	close(c)

	return c
}()

// buffer is the one copy of a stream. Written items are never modified, so
// readers may read them outside the lock.
type buffer[C any] struct {
	mu      sync.Mutex
	items   []C
	err     error
	closed  bool
	changed chan struct{} // closed and replaced on every Write and on Close
	done    chan struct{}
}

func (b *buffer[C]) notify() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *buffer[C]) close(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed, b.err = true, err
	b.notify()
	close(b.done)
}

func (b *buffer[C]) read(ctx context.Context, out chan<- C) {
	defer close(out)

	next := 0
	for {
		b.mu.Lock()
		items, closed, changed := b.items[next:], b.closed, b.changed
		b.mu.Unlock()

		for _, c := range items {
			select {
			case out <- deepcopy.Copy(c):
			case <-ctx.Done():
				return
			}
		}
		next += len(items)
		if closed {
			return
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

func (b *buffer[C]) finished() <-chan struct{} { return b.done }

func (b *buffer[C]) abort(err error) { b.close(err) }

// streamBuffer is a stream as the transaction sees it, whatever its chunk
// type.
type streamBuffer interface {
	finished() <-chan struct{}
	abort(err error)
}

// streamer is implemented by Stream (and by a type that embeds it, which
// returns the same buffer; streamsIn deduplicates).
type streamer interface {
	buffer() streamBuffer
}

var streamerType = reflect.TypeFor[streamer]()

// streamsIn finds the open streams reachable from ev through exported
// fields, pointers, interfaces, slices, arrays and maps.
func streamsIn(ev Event) []streamBuffer {
	w := streamWalk{seen: map[streamBuffer]bool{}, visited: map[uintptr]bool{}}
	w.walk(reflect.ValueOf(ev))

	return w.found
}

type streamWalk struct {
	found   []streamBuffer
	seen    map[streamBuffer]bool
	visited map[uintptr]bool
}

func (w *streamWalk) walk(v reflect.Value) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		if v.Kind() == reflect.Pointer {
			if w.visited[v.Pointer()] {
				return
			}
			w.visited[v.Pointer()] = true
		}
	}
	w.collect(v)

	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		w.walk(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				w.walk(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		if isScalar(v.Type().Elem().Kind()) {
			return
		}
		for i := range v.Len() {
			w.walk(v.Index(i))
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			w.walk(it.Value())
		}
	default:
	}
}

func (w *streamWalk) collect(v reflect.Value) {
	if !v.Type().Implements(streamerType) || !v.CanInterface() {
		return
	}
	// A Stream embedded by nil pointer panics in the promoted method; such a
	// field holds no stream.
	defer func() { _ = recover() }()
	s, ok := v.Interface().(streamer)
	if !ok {
		return
	}
	if b := s.buffer(); b != nil && !w.seen[b] {
		w.seen[b] = true
		w.found = append(w.found, b)
	}
}

func isScalar(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String:
		return true
	default:
		return false
	}
}
