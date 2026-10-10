package aish

import "slices"

// EmitOpts restricts which hooks get the emitted events. The zero value
// delivers to every subscribed hook except the emitting one. Build it with
// To, Except and Below, which also chain: Below(1000).Except("a:b").
type EmitOpts struct {
	to       []string
	except   []string
	below    int
	hasBelow bool
}

// To delivers only to the hooks with the given IDs ("<instance>:<name>").
func To(hooks ...string) EmitOpts { return EmitOpts{}.To(hooks...) }

// Except delivers to every hook except the given ones. The emitting hook
// is always excluded.
func Except(hooks ...string) EmitOpts { return EmitOpts{}.Except(hooks...) }

// Below delivers only to Mutate and Handle hooks with priority below p. It
// means "continue the chain after me": a policy re-emits an approved call
// with Below(1000), so executors (DefaultPriority) run it, and records a
// denied call with Below(DefaultPriority), so executors do not.
func Below(p int) EmitOpts { return EmitOpts{}.Below(p) }

func (o EmitOpts) To(hooks ...string) EmitOpts {
	o.to = append(slices.Clip(o.to), hooks...)

	return o
}

func (o EmitOpts) Except(hooks ...string) EmitOpts {
	o.except = append(slices.Clip(o.except), hooks...)

	return o
}

func (o EmitOpts) Below(p int) EmitOpts {
	o.below, o.hasBelow = p, true

	return o
}

func (o EmitOpts) admits(hk, sender *hook) bool {
	switch {
	case hk == sender:
		return false
	case len(o.to) > 0 && !slices.Contains(o.to, hk.info.ID):
		return false
	case slices.Contains(o.except, hk.info.ID):
		return false
	case o.hasBelow && hk.info.Priority >= o.below:
		return false
	default:
		return true
	}
}
