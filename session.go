package aish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/GoldenDeals/aish-core/internal/deepcopy"
)

const (
	maxDepth  = 16   // nested Emit levels per transaction
	maxEvents = 1000 // events per transaction

	// defaultWarnAfter is how long an Emit may wait in the queue while a hook
	// of the same instance runs before the core suspects a deadlock.
	defaultWarnAfter = 10 * time.Second
)

// ctxKey marks contexts issued by the core. Emit accepts no other.
type ctxKey struct{}

type ctxInfo struct {
	sess    *session
	tx      *txState // Mutate and Handle hooks: the open transaction
	frame   *frame   // Mutate and Handle hooks: the invocation that owns ctx
	hook    *hook    // the hook that owns ctx; nil for the Start context
	observe bool     // ctx of an Observe hook
	init    bool     // ctx of Init: Emit is not allowed yet
	meta    Meta     // event being delivered to the hook
}

func infoOf(ctx context.Context) *ctxInfo {
	info, _ := ctx.Value(ctxKey{}).(*ctxInfo)

	return info
}

// pending is a transaction waiting in the session queue.
type pending struct {
	events  []Event
	opts    EmitOpts
	sender  *hook  // Observe hook that emitted the root events, if any
	source  string // Meta.Source of the root events
	done    chan error
	started bool // guarded by session.mu
}

type sessionConfig struct {
	info      SessionInfo
	log       *slog.Logger
	now       func() time.Time
	warnAfter time.Duration

	streamTimeout time.Duration
}

// session runs transactions one at a time. A single runner goroutine
// executes every Mutate, Handle and Observe hook, so hooks of a session
// never run concurrently.
type session struct {
	info      SessionInfo
	ctx       context.Context
	cancel    context.CancelFunc
	log       *slog.Logger
	now       func() time.Time
	warnAfter time.Duration

	streamTimeout time.Duration

	hooks   hookRegistry
	journal Journal

	mu      sync.Mutex
	queue   []*pending
	cur     *txState
	lastSeq uint64
	running bool // transactions may run; Seed is no longer allowed
	busy    bool // the runner took a transaction and has not finished it
	closed  bool
	idle    chan struct{} // closed and replaced whenever the session becomes idle

	wake    chan struct{}
	stopped chan struct{}
}

func newSession(cfg sessionConfig) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		info:      cfg.info,
		ctx:       ctx,
		cancel:    cancel,
		now:       cfg.now,
		warnAfter: cfg.warnAfter,

		streamTimeout: cfg.streamTimeout,
		idle:          make(chan struct{}),
		wake:          make(chan struct{}, 1),
		stopped:       make(chan struct{}),
	}
	base := slog.DiscardHandler
	if cfg.log != nil {
		base = cfg.log.Handler()
	}
	s.log = slog.New(ctxHandler{base}).With("session", cfg.info.ID)
	if s.now == nil {
		s.now = time.Now
	}
	if s.warnAfter == 0 {
		s.warnAfter = defaultWarnAfter
	}

	return s
}

func (s *session) host(instance string) *Host {
	return &Host{sess: s, instance: instance, log: s.log.With("instance", instance)}
}

// initContext is the context given to Init: Seed works, Emit does not.
func (s *session) initContext() context.Context {
	return context.WithValue(s.ctx, ctxKey{}, &ctxInfo{sess: s, init: true})
}

// emitCore queues a transaction with an event of the core itself.
func (s *session) emitCore(ev Event) error {
	return s.enqueue(s.ctx, &pending{events: []Event{ev}, source: sourceCore})
}

// waitIdle blocks until no transaction is open or queued. Observe hooks run
// inside the transaction they observe, so a transaction they queue is seen
// before the session looks idle.
func (s *session) waitIdle(ctx context.Context) error {
	for {
		s.mu.Lock()
		if s.closed || (!s.busy && len(s.queue) == 0) {
			s.mu.Unlock()

			return nil
		}
		idle := s.idle
		s.mu.Unlock()

		select {
		case <-idle:
		case <-ctx.Done():
			return fmt.Errorf("aish: wait: %w", ctx.Err())
		}
	}
}

// becameIdle wakes waitIdle; the caller holds s.mu.
func (s *session) becameIdle() {
	close(s.idle)
	s.idle = make(chan struct{})
}

// startContext is the context given to Start: Emit with it queues a
// transaction and waits for it.
func (s *session) startContext() context.Context {
	return context.WithValue(s.ctx, ctxKey{}, &ctxInfo{sess: s})
}

// Emit before start only queues.
func (s *session) start() {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	go s.run()
}

// seed appends restored transactions to the journal without running hooks.
// It is allowed only before the session starts, so no transaction is open
// and sequence numbers continue after the seeded ones.
func (s *session) seed(txs []Tx) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return errors.New("aish: Seed is allowed only in Init")
	}
	last := s.lastSeq
	for _, tx := range txs {
		if tx.Seq <= last {
			return fmt.Errorf("aish: seed: transaction %d after %d; sequence numbers must increase", tx.Seq, last)
		}
		last = tx.Seq
	}
	for _, tx := range txs {
		s.journal.append(tx)
	}
	s.lastSeq = last

	return nil
}

// onRunner reports whether ctx belongs to a hook, which runs on the runner
// goroutine. Waiting for the runner from there would wait for itself.
func (s *session) onRunner(ctx context.Context) bool {
	info := infoOf(ctx)

	return info != nil && info.sess == s && info.hook != nil
}

// close cancels the open transaction, waits for it to commit and fails
// everything still queued with ErrClosed. Called from a hook it does not wait:
// the runner exits when the hook returns. If ctx ends first, it returns the
// ctx error and the runner stops on its own once the stuck hook returns.
func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	wasRunning := s.running
	s.closed = true
	s.becameIdle()
	s.mu.Unlock()
	s.cancel()
	s.signal()
	if !wasRunning || s.onRunner(ctx) {
		return nil
	}

	select {
	case <-s.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("aish: close: %w", ctx.Err())
	}
}

func (s *session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *session) run() {
	defer close(s.stopped)

	for {
		p := s.next()
		if p == nil {
			return
		}
		s.runTx(p)

		s.mu.Lock()
		s.busy = false
		if len(s.queue) == 0 {
			s.becameIdle()
		}
		s.mu.Unlock()
	}
}

// next blocks until a transaction is queued. It returns nil once the session
// is closed.
func (s *session) next() *pending {
	for {
		s.mu.Lock()
		if s.closed {
			queued := s.queue
			s.queue = nil
			s.mu.Unlock()
			for _, p := range queued {
				if p.done != nil {
					p.done <- ErrClosed
				}
			}

			return nil
		}
		if len(s.queue) > 0 {
			p := s.queue[0]
			s.queue = s.queue[1:]
			p.started = true
			s.busy = true
			s.mu.Unlock()

			return p
		}
		s.mu.Unlock()
		<-s.wake
	}
}

// enqueue adds a transaction to the queue. With p.done set it waits until
// the transaction and its Observe hooks finished.
func (s *session) enqueue(ctx context.Context, p *pending) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return ErrClosed
	}
	s.queue = append(s.queue, p)
	s.mu.Unlock()
	s.signal()

	if p.done == nil {
		return nil
	}

	return s.await(ctx, p)
}

func (s *session) await(ctx context.Context, p *pending) error {
	warn := time.NewTimer(s.warnAfter)
	defer warn.Stop()

	for {
		select {
		case err := <-p.done:
			return err
		case <-ctx.Done():
			s.mu.Lock()
			if !p.started {
				s.queue = slices.DeleteFunc(s.queue, func(q *pending) bool { return q == p })
				if !s.busy && len(s.queue) == 0 {
					s.becameIdle()
				}
			}
			s.mu.Unlock()

			return fmt.Errorf("aish: emit: %w", ctx.Err())
		case <-warn.C:
			// Once its own transaction runs, a hook of the same instance is
			// expected (a frontend showing the message it emitted).
			s.mu.Lock()
			queued := !p.started
			s.mu.Unlock()
			if queued {
				s.warnPossibleDeadlock(p.source)
			}
		}
	}
}

// warnPossibleDeadlock reports an Emit that waits in the queue while a hook
// of the same instance runs in the open transaction: most likely the hook
// called Emit with a stored Start context instead of its own.
func (s *session) warnPossibleDeadlock(instance string) {
	s.mu.Lock()
	tx := s.cur
	s.mu.Unlock()
	if tx == nil {
		return
	}

	tx.mu.Lock()
	var hookID string
	for _, f := range tx.stack {
		if f.hook.info.Instance == instance {
			hookID = f.hook.info.ID
		}
	}
	msg := fmt.Sprintf("%s waits in the queue for over %s while its hook %s runs; "+
		"a hook must emit with its own ctx", instance, s.warnAfter, hookID)
	if hookID != "" && !tx.closed {
		tx.trace = append(tx.trace, TraceEntry{Hook: hookID, Action: TraceWarn, Err: msg})
	}
	tx.mu.Unlock()

	if hookID != "" {
		s.log.Warn("possible deadlock", "instance", instance, "hook", hookID, "tx", tx.seq)
	}
}

func (s *session) runTx(p *pending) {
	tx := s.beginTx()
	s.observe(TxStarted{Seq: tx.seq, Root: p.events}, Meta{Tx: tx.seq, Time: s.now(), Source: sourceCore})

	errs := make([]error, 0, len(p.events))
	for _, ev := range p.events {
		errs = append(errs, tx.deliver(ev, p.opts, p.sender, p.source, 1))
	}
	errs = append(errs, tx.awaitStreams())
	err := errors.Join(errs...)
	if p.done == nil {
		tx.recordUnreturned(p.source, err)
	}

	committed := tx.commit()
	for _, env := range committed.Events {
		s.observe(env.Event, env.Meta)
	}
	s.observe(TxCommitted{Tx: committed}, Meta{Tx: committed.Seq, Time: s.now(), Source: sourceCore})

	if p.done != nil {
		p.done <- err
	}
}

func (s *session) beginTx() *txState {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastSeq++
	ctx, cancel := context.WithCancelCause(s.ctx)
	tx := &txState{
		s:      s,
		seq:    s.lastSeq,
		ctx:    ctx,
		cancel: cancel,
		seen:   map[seenKey]struct{}{},
	}
	s.cur = tx

	return tx
}

// observe delivers a committed event to Observe hooks, one at a time.
func (s *session) observe(ev Event, meta Meta) {
	for _, hk := range s.hooks.snapshot(ev.Kind(), ModeObserve) {
		err := safeCall(func() error {
			if hk.where != nil && !hk.where(ev) {
				return nil
			}
			// The ctx ends with the hook, so Stream.Chunks readers do not outlive it.
			ctx, cancel := context.WithCancel(s.ctx)
			defer cancel()
			ctx = context.WithValue(ctx, ctxKey{}, &ctxInfo{sess: s, hook: hk, observe: true, meta: meta})
			hk.observe(ctx, deepcopy.Copy(ev))

			return nil
		})
		if err != nil {
			s.log.Error("observe hook failed", "hook", hk.info.ID, "event", meta.ID, "err", err)
		}
	}
}

// safeCall turns a panic in module code into an error.
func safeCall(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	return fn()
}
