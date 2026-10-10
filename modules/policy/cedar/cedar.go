// Package cedar is a policy that decides tool calls with Cedar policies. The
// effect ask(...) is a dialect extension: the user is asked, as policy.ask does.
package cedar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	cedar "github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/policy/internal/gate"
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "policy.cedar", New: func() aish.Module { return &Policy{} }})
}

// Policy is the module policy.cedar. Every Access of a call is checked as
// Agent::"<agent>" doing Action::"<Access.Action>" to a resource entity built
// from Access.Resource "<kind>:<id>" (type Kind, attribute path/command/id).
// A call without Access is checked as Action::"tool" on Tool::"<name>".
type Policy struct {
	Globs []string

	gate  gate.Gate
	set   *cedar.PolicySet
	agent string
	log   *slog.Logger
}

type decision int

const (
	permit decision = iota
	ask
	forbid
)

// UnmarshalCaddyfile reads "policy cedar <glob>...".
func (p *Policy) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	p.Globs = d.RemainingArgs()
	if d.NextBlock(0) {
		return d.Errf("unexpected block")
	}

	return nil
}

func (p *Policy) Validate() error {
	if len(p.Globs) == 0 {
		return errors.New("aish: policy.cedar: no policy files")
	}

	return nil
}

func (p *Policy) Init(_ context.Context, h *aish.Host) error {
	set, err := load(p.Globs)
	if err != nil {
		return err
	}
	p.set, p.agent, p.log = set, h.Session().Agent, h.Log()
	p.gate.Init(h)
	h.Mutate("authorize", p.authorize, aish.Priority(gate.Priority))

	return nil
}

func load(globs []string) (*cedar.PolicySet, error) {
	set := cedar.NewPolicySet()
	for _, glob := range globs {
		files, err := filepath.Glob(glob)
		if err != nil {
			return nil, fmt.Errorf("aish: policy.cedar: %w", err)
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("aish: policy.cedar: no files match %q", glob)
		}
		for _, file := range files {
			src, err := os.ReadFile(file) //nolint:gosec // the path comes from the config
			if err != nil {
				return nil, fmt.Errorf("aish: policy.cedar: %w", err)
			}
			one, err := cedar.NewPolicySetFromBytes(file, []byte(rewriteAsk(string(src))))
			if err != nil {
				return nil, fmt.Errorf("aish: policy.cedar: %w", err)
			}
			for id, pol := range one.All() {
				set.Add(cedar.PolicyID(file+":"+string(id)), pol)
			}
		}
	}

	return set, nil
}

func (p *Policy) authorize(ctx context.Context, m *aish.Mut[events.ToolCall]) error {
	d, reason, err := p.decide(ctx, *m.Event())
	p.log.DebugContext(ctx, "cedar decision", "tool", m.Event().Name, "decision", d, "reason", reason, "err", err)
	switch {
	case err != nil:
		// Fail closed.
		return p.gate.Deny(ctx, m, "policy error: "+err.Error())
	case d == forbid:
		return p.gate.Deny(ctx, m, reason)
	case d == ask:
		return p.gate.Ask(ctx, m)
	}

	return nil
}

// decide combines the decisions for all accesses: forbid beats ask beats permit.
func (p *Policy) decide(ctx context.Context, call events.ToolCall) (decision, string, error) {
	input, err := inputValue(call.Input)
	if err != nil {
		return forbid, "", err
	}
	reqs := p.requests(call, input)
	worst, why := permit, ""
	for _, r := range reqs {
		d, reason := p.check(ctx, r.req, r.entities)
		if d == forbid {
			return forbid, reason, nil
		}
		if d > worst {
			worst, why = d, reason
		}
	}

	return worst, why, nil
}

type request struct {
	req      cedar.Request
	entities types.EntityMap
}

func (p *Policy) requests(call events.ToolCall, input types.Value) []request {
	principal := cedar.NewEntityUID("Agent", cedar.String(p.agent))
	if len(call.Effects.Access) == 0 {
		return []request{{req: cedar.Request{
			Principal: principal,
			Action:    cedar.NewEntityUID("Action", "tool"),
			Resource:  cedar.NewEntityUID("Tool", cedar.String(call.Name)),
			Context:   cedar.NewRecord(cedar.RecordMap{"input": input}),
		}}}
	}

	reqs := make([]request, len(call.Effects.Access))
	for i, a := range call.Effects.Access {
		res, entity := resource(a)
		attrs := toRecord(a.Attrs)
		reqs[i] = request{
			req: cedar.Request{
				Principal: principal,
				Action:    cedar.NewEntityUID("Action", cedar.String(a.Action)),
				Resource:  res,
				Context:   cedar.NewRecord(mergeInput(attrs, input)),
			},
			entities: types.EntityMap{res: entity},
		}
	}

	return reqs
}

// check returns the decision of the policy set for one request and the IDs of
// the policies that decided it.
//
// A policy that fails to evaluate (resource.path on a command, say) does not
// apply, as in Cedar itself; the error goes to the log. Guard attribute access
// with "has" to make a forbid apply to every resource.
func (p *Policy) check(ctx context.Context, req cedar.Request, entities types.EntityMap) (decision, string) {
	dec, diag := cedar.Authorize(p.set, entities, req)
	for _, e := range diag.Errors {
		p.log.WarnContext(ctx, "cedar policy skipped", "err", e.String())
	}
	ids := make([]string, len(diag.Reasons))
	for i, r := range diag.Reasons {
		ids[i] = string(r.PolicyID)
	}
	reason := strings.Join(ids, ", ")
	if dec == cedar.Deny {
		if reason == "" {
			return forbid, "no policy permits it"
		}

		return forbid, "forbidden by " + reason
	}
	for _, r := range diag.Reasons {
		if v, ok := p.set.Get(r.PolicyID).Annotations()["decision"]; ok && v == "ask" {
			return ask, reason
		}
	}

	return permit, reason
}

// resource turns "file:/abs/path" into File::"/abs/path" with the attribute
// path (command for cmd, id otherwise).
func resource(a events.Access) (cedar.EntityUID, cedar.Entity) {
	kind, id, ok := strings.Cut(a.Resource, ":")
	if !ok {
		kind, id = "Resource", a.Resource
	}
	attr := map[string]string{"file": "path", "cmd": "command"}[kind]
	if attr == "" {
		attr = "id"
	}
	uid := cedar.NewEntityUID(cedar.EntityType(strings.ToUpper(kind[:1])+kind[1:]), cedar.String(id))

	return uid, cedar.Entity{UID: uid, Attributes: cedar.NewRecord(cedar.RecordMap{cedar.String(attr): cedar.String(id)})}
}

func mergeInput(attrs types.RecordMap, input types.Value) types.RecordMap {
	attrs["input"] = input

	return attrs
}

func inputValue(raw json.RawMessage) (types.Value, error) {
	if len(raw) == 0 {
		return cedar.NewRecord(nil), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("tool input: %w", err)
	}

	return toValue(v), nil
}

func toRecord(m map[string]any) types.RecordMap {
	out := make(types.RecordMap, len(m)+1)
	for k, v := range m {
		out[cedar.String(k)] = toValue(v)
	}

	return out
}

// toValue maps JSON-like values to Cedar. Cedar has no floats: a number with a
// fraction, null and anything unknown become strings.
func toValue(v any) types.Value {
	switch v := v.(type) {
	case string:
		return cedar.String(v)
	case bool:
		return cedar.Boolean(v)
	case int:
		return cedar.Long(v)
	case int64:
		return cedar.Long(v)
	case float64:
		if v == float64(int64(v)) {
			return cedar.Long(int64(v))
		}
	case []any:
		items := make([]types.Value, len(v))
		for i, x := range v {
			items[i] = toValue(x)
		}

		return cedar.NewSet(items...)
	case map[string]any:
		return cedar.NewRecord(toRecord(v))
	}

	return cedar.String(fmt.Sprint(v))
}
