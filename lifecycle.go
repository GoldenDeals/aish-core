package aish

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"
)

type SessionInfo struct {
	ID     string
	Agent  string // agent block: "main", "reviewer"
	Parent string // parent session ID of a subagent
	Depth  int    // 0 for the main agent, parent's Depth+1 for a subagent
}

// Instance is a configured module instance of a session.
type Instance struct {
	// Name is the module ID, with "/<name>" when the config names the
	// instance: "llm.anthropic/fast". Hook IDs are "<Name>:<hook>".
	Name   string
	Module Module
}

type SessionConfig struct {
	ID     string // generated when empty
	Agent  string
	Parent string
	Depth  int // Parent's Depth+1 for a subagent
	// Instances are in config order: Init and Start run in this order, Stop
	// in reverse.
	Instances []Instance
	// Log receives the records of the core and of every module; they are
	// discarded when it is nil.
	Log *slog.Logger
	// StreamTimeout limits how long a transaction waits for its streams to
	// close once its hooks have run. When it passes, the transaction is
	// cancelled with ErrStreamStalled and the open streams are closed with
	// that error. Zero means no limit.
	StreamTimeout time.Duration
	Options       Options
	// Agents are all agent blocks of the config, for Host.Agents; a
	// subagent gets the same list.
	Agents []AgentSpec
	// Spawner creates the module instances of a new session of the named
	// agent block; Host.Spawn calls it. Without it Spawn fails. Every call
	// must return fresh instances.
	Spawner func(agent string) ([]Instance, error)
}

// Session is a set of module instances and their journal (§4.1).
type Session struct {
	s         *session
	instances []Instance
	hosts     []*Host

	mu      sync.Mutex
	started bool
	closed  bool
	inited  int // instances[:inited] passed Init and get Stop on Close
}

// Emit queues events in a new transaction of the session and waits until it
// has committed, like a frontend emitting from its Start goroutine. ctx only
// bounds the wait. It is how a parent talks to a session it spawned.
func (s *Session) Emit(ctx context.Context, evs ...Event) error {
	for _, ev := range evs {
		if err := s.s.checkEmit(ev); err != nil {
			return err
		}
	}
	if len(evs) == 0 {
		return nil
	}
	p := &pending{events: slices.Clone(evs), source: sourceExternal, done: make(chan error, 1)}

	return s.s.enqueue(ctx, p)
}

var instanceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*\.[a-z][a-z0-9_-]*(/[A-Za-z0-9_.-]+)?$`)

// NewSession checks the instances and runs Validate on them. It has no other
// side effects: a config error shows up before any module starts.
func NewSession(cfg SessionConfig) (*Session, error) {
	return newSessionIn(context.Background(), cfg)
}

// newSessionIn is NewSession for a session whose context ends with parent:
// a subagent stops when the session that spawned it does.
func newSessionIn(parent context.Context, cfg SessionConfig) (*Session, error) {
	var errs []error
	seen := map[string]bool{}
	for _, inst := range cfg.Instances {
		switch {
		case !instanceNamePattern.MatchString(inst.Name):
			errs = append(errs, fmt.Errorf("aish: invalid instance name %q, want <namespace>.<name>[/<instance>]", inst.Name))
		case seen[inst.Name]:
			errs = append(errs, fmt.Errorf("aish: instance %s configured twice", inst.Name))
		case inst.Module == nil:
			errs = append(errs, fmt.Errorf("aish: instance %s has no module", inst.Name))
		default:
			if v, ok := inst.Module.(Validator); ok {
				if err := safeCall(v.Validate); err != nil {
					errs = append(errs, fmt.Errorf("aish: %s: %w", inst.Name, err))
				}
			}
		}
		seen[inst.Name] = true
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	info := SessionInfo{ID: cfg.ID, Agent: cfg.Agent, Parent: cfg.Parent, Depth: cfg.Depth}
	if info.ID == "" {
		info.ID = rand.Text()
	}
	s := &Session{
		s:         newSession(sessionConfig{parent: parent, info: info, log: cfg.Log, streamTimeout: cfg.StreamTimeout, spawner: cfg.Spawner, agents: slices.Clone(cfg.Agents)}),
		instances: slices.Clone(cfg.Instances),
	}
	s.s.owner = s
	for _, inst := range s.instances {
		h := s.s.host(inst.Name)
		h.options = cfg.Options
		s.hosts = append(s.hosts, h)
	}

	return s, nil
}

func (s *Session) Info() SessionInfo { return s.s.info }

func (s *Session) Journal() *Journal { return &s.s.journal }

// Start runs Init on every instance (subscriptions and Seed; Emit returns
// ErrNotStarted), starts the bus, runs Start on the instances that have it
// and queues session.started. The Start context lives until Close.
//
// On error the session is closed: instances that passed Init get Stop.
func (s *Session) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.closed:
		return ErrClosed
	case s.started:
		return errors.New("aish: session started twice")
	}
	s.started = true

	if err := s.initLocked(); err != nil {
		return err
	}

	names := make([]string, len(s.instances))
	for i, inst := range s.instances {
		names[i] = inst.Name
	}
	s.s.log.Debug("session starting", "agent", s.s.info.Agent, "parent", s.s.info.Parent, "depth", s.s.info.Depth,
		"instances", names)
	s.s.start()
	startCtx := s.s.startContext()
	for _, inst := range s.instances {
		st, ok := inst.Module.(Starter)
		if !ok {
			continue
		}
		if err := safeCall(func() error { return st.Start(startCtx) }); err != nil {
			stopErr := s.closeLocked(context.Background())

			return errors.Join(fmt.Errorf("aish: start %s: %w", inst.Name, err), stopErr)
		}
		s.s.log.Debug("instance started", "instance", inst.Name)
	}

	return s.s.emitCore(SessionStarted(s.s.info))
}

func (s *Session) initLocked() error {
	initCtx := s.s.initContext()
	for i, inst := range s.instances {
		if err := safeCall(func() error { return inst.Module.Init(initCtx, s.hosts[i]) }); err != nil {
			stopErr := s.closeLocked(context.Background())

			return errors.Join(fmt.Errorf("aish: init %s: %w", inst.Name, err), stopErr)
		}
		s.s.log.Debug("instance initialized", "instance", inst.Name)
		s.inited = i + 1
	}

	return nil
}

// Hooks runs Init on every instance without starting the bus or the modules,
// returns all subscriptions and closes the session. It backs `aish list-hooks`.
func (s *Session) Hooks() ([]HookInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.closed:
		return nil, ErrClosed
	case s.started:
		return nil, errors.New("aish: session started twice")
	}
	s.started = true
	if err := s.initLocked(); err != nil {
		return nil, err
	}
	hooks := s.s.hooks.infos("", true)

	return hooks, s.closeLocked(context.Background())
}

// Wait blocks until the session is idle: no transaction is open or queued.
// Modules' own goroutines (a frontend reading input) may still emit later.
func (s *Session) Wait(ctx context.Context) error {
	return s.s.waitIdle(ctx)
}

// Close cancels the open transaction, waits for it to commit, fails queued
// Emits with ErrClosed and then runs Stop with ctx, in reverse order, on
// every instance that passed Init; Stop must cope with a Start that never
// ran. Closing twice is a no-op.
//
// If ctx ends while the transaction is still running (a hook ignores
// cancellation), Close stops waiting, still runs Stop and returns the ctx
// error joined with the Stop errors.
//
// A hook may call Close with its own ctx: Close then does not wait for the
// transaction, which ends when the hook returns, and Stop gets ctx without
// its cancellation.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closeLocked(ctx)
}

func (s *Session) closeLocked(ctx context.Context) error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.s.log.Debug("session closing")
	var errs []error
	for _, child := range s.s.takeChildren() {
		errs = append(errs, child.Close(ctx))
	}
	if err := s.s.close(ctx); err != nil {
		errs = append(errs, err)
	}
	if s.s.onRunner(ctx) {
		ctx = context.WithoutCancel(ctx) // the hook ctx was just cancelled
	}

	for _, inst := range slices.Backward(s.instances[:s.inited]) {
		st, ok := inst.Module.(Stopper)
		if !ok {
			continue
		}
		if err := safeCall(func() error { return st.Stop(ctx) }); err != nil {
			errs = append(errs, fmt.Errorf("aish: stop %s: %w", inst.Name, err))
		}
		s.s.log.Debug("instance stopped", "instance", inst.Name)
	}

	return errors.Join(errs...)
}
