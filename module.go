// Package aish is the core of the aish agent harness: the event bus with
// transactions, the journal of committed transactions, the module registry
// and the shared event types. Everything else is a module.
package aish

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/coredns/caddy/caddyfile"
)

// Module is the only interface every module implements. In Init a module
// subscribes to events; that is what defines what it does.
type Module interface {
	Init(ctx context.Context, h *Host) error
}

// CaddyfileUnmarshaler reads the module's own directive. The Dispenser stands
// on its first token and is limited to its block.
type CaddyfileUnmarshaler interface {
	UnmarshalCaddyfile(d *caddyfile.Dispenser) error
}

// Validator checks configuration and must have no side effects: it runs
// before any module starts.
type Validator interface {
	Validate() error
}

// Starter is implemented by long-running modules (pty, mcp, webchat).
// Emit may be called with the Start context and contexts derived from it.
type Starter interface {
	Start(ctx context.Context) error
}

// Stopper is called in reverse order of Start.
type Stopper interface {
	Stop(ctx context.Context) error
}

type ModuleInfo struct {
	// ID is "<namespace>.<name>", for example "frontend.pty".
	ID  string
	New func() Module
}

var moduleIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*\.[a-z][a-z0-9_-]*$`)

var modules = struct {
	sync.RWMutex
	byID map[string]ModuleInfo
}{byID: map[string]ModuleInfo{}}

// RegisterModule makes a module available to sessions. It is meant to be
// called from init() and panics on an invalid or duplicate registration.
func RegisterModule(info ModuleInfo) {
	if !moduleIDPattern.MatchString(info.ID) {
		panic(fmt.Sprintf("aish: invalid module ID %q, want <namespace>.<name>", info.ID))
	}
	if info.New == nil {
		panic(fmt.Sprintf("aish: module %s: New is nil", info.ID))
	}

	modules.Lock()
	defer modules.Unlock()

	if _, dup := modules.byID[info.ID]; dup {
		panic(fmt.Sprintf("aish: module %s registered twice", info.ID))
	}
	modules.byID[info.ID] = info
}

func LookupModule(id string) (ModuleInfo, bool) {
	modules.RLock()
	defer modules.RUnlock()

	info, ok := modules.byID[id]

	return info, ok
}

// Modules returns the registered modules sorted by ID.
func Modules() []ModuleInfo {
	modules.RLock()
	defer modules.RUnlock()

	out := make([]ModuleInfo, 0, len(modules.byID))
	for _, info := range modules.byID {
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b ModuleInfo) int { return strings.Compare(a.ID, b.ID) })

	return out
}
