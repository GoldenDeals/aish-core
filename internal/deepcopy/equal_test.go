package deepcopy

import "testing"

type guarded struct{ items []int }

type equalProbe struct {
	Name  string
	Tags  []string
	Meta  map[string]any
	Next  *node
	State *guarded // exported pointer: compared deeply
	priv  *guarded // unexported: compared by identity
}

func TestEqual(t *testing.T) {
	t.Parallel()

	shared := &guarded{items: []int{1}}
	cycle := &node{Name: "a"}
	cycle.Next = cycle
	base := equalProbe{
		Name: "x", Tags: []string{"a"}, Meta: map[string]any{"k": []any{1.0}},
		Next: cycle, State: &guarded{}, priv: shared,
	}

	if !Equal(base, Copy(base)) {
		t.Error("a value differs from its copy")
	}

	for name, change := range map[string]func(*equalProbe){
		"exported string":     func(p *equalProbe) { p.Name = "y" },
		"slice element":       func(p *equalProbe) { p.Tags = []string{"b"} },
		"nested map value":    func(p *equalProbe) { p.Meta = map[string]any{"k": []any{2.0}} },
		"unexported identity": func(p *equalProbe) { p.priv = &guarded{items: []int{1}} },
	} {
		other := Copy(base)
		change(&other)
		if Equal(base, other) {
			t.Errorf("%s: change not detected", name)
		}
	}

	other := Copy(base)
	shared.items = append(shared.items, 2) // behind an unexported pointer: not compared
	if !Equal(base, other) {
		t.Error("state behind an unexported pointer was compared")
	}
}
