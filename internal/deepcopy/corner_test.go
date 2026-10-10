package deepcopy

import (
	"math"
	"testing"
)

type containers struct {
	NilSlice   []int
	EmptySlice []int
	NilMap     map[string]int
	EmptyMap   map[string]int
}

func TestCopyKeepsNilAndEmptyApart(t *testing.T) {
	t.Parallel()

	c := Copy(containers{EmptySlice: []int{}, EmptyMap: map[string]int{}})
	if c.NilSlice != nil || c.NilMap != nil {
		t.Error("nil became non-nil")
	}
	if c.EmptySlice == nil || c.EmptyMap == nil {
		t.Error("empty became nil")
	}
}

type wrongClone struct{ Items []int }

// Clone returns another type, so Copy must not use it.
func (wrongClone) Clone() int { return 0 }

type ptrClone struct{ calls *int }

func (p *ptrClone) Clone() *ptrClone {
	*p.calls++

	return &ptrClone{calls: p.calls}
}

func TestCopyUsesOnlyMatchingClone(t *testing.T) {
	t.Parallel()

	orig := wrongClone{Items: []int{1}}
	c := Copy(orig)
	c.Items[0] = 2
	if orig.Items[0] != 1 {
		t.Error("a Clone of another type was used instead of a deep copy")
	}

	calls := 0
	p := &ptrClone{calls: &calls}
	if Copy(p) == p || calls != 1 {
		t.Errorf("Clone on a pointer receiver called %d times, want 1 for a pointer value", calls)
	}
}

type shared struct {
	A, B *node
	Fn   func() int
	Ch   chan int
}

func TestCopyKeepsSharingWithinTheValue(t *testing.T) {
	t.Parallel()

	n := &node{Name: "x"}
	ch := make(chan int)
	orig := shared{A: n, B: n, Fn: func() int { return 1 }, Ch: ch}
	c := Copy(orig)
	if c.A != c.B || c.A == n {
		t.Error("two fields pointing to one object must point to one new object")
	}
	if c.Ch != ch || c.Fn() != 1 {
		t.Error("funcs and channels must be copied by reference")
	}
}

type outer struct {
	inner struct{ S []int }
	Arr   [2]struct{ S []int }
}

func TestCopyNestedStructs(t *testing.T) {
	t.Parallel()

	var orig outer
	orig.inner.S = []int{1}
	orig.Arr[1].S = []int{1}
	c := Copy(orig)
	c.Arr[1].S[0] = 2
	if orig.Arr[1].S[0] != 1 {
		t.Error("a slice inside an array of structs is shared")
	}
	if &c.inner.S[0] != &orig.inner.S[0] {
		t.Error("an unexported struct field was deep-copied")
	}
}

type withFunc struct{ Fn func() }

type private struct {
	slice []int
	iface any
	inner struct{ m map[string]int }
	arr   [1]*int
}

// TestEqualComparesUnexportedFieldsByIdentity: same backing data is equal,
// equal contents behind other references are not.
func TestEqualComparesUnexportedFieldsByIdentity(t *testing.T) {
	t.Parallel()

	n := 1
	base := private{slice: []int{1}, iface: &n, arr: [1]*int{&n}}
	base.inner.m = map[string]int{"k": 1}
	if !Equal(base, base) {
		t.Error("a value differs from itself")
	}

	m := 1
	for name, change := range map[string]func(*private){
		"slice":         func(p *private) { p.slice = []int{1} },
		"shorter slice": func(p *private) { p.slice = p.slice[:0] },
		"interface":     func(p *private) { p.iface = &m },
		"interface nil": func(p *private) { p.iface = nil },
		"interface type": func(p *private) {
			p.iface = 1
		},
		"nested map": func(p *private) { p.inner.m = map[string]int{"k": 1} },
		"array":      func(p *private) { p.arr[0] = &m },
	} {
		other := base
		change(&other)
		if Equal(base, other) {
			t.Errorf("%s: equal contents behind another reference compared equal", name)
		}
	}
}

func TestEqualCorners(t *testing.T) {
	t.Parallel()

	cycleA := &node{Name: "a"}
	cycleA.Next = cycleA
	cycleB := &node{Name: "a"}
	cycleB.Next = cycleB
	fn := func() {}

	for _, tc := range []struct {
		name  string
		a, b  any
		equal bool
	}{
		{name: "nil and empty slice", a: []int(nil), b: []int{}, equal: false},
		{name: "nil and empty map", a: map[string]int(nil), b: map[string]int{}, equal: false},
		{name: "maps with other keys", a: map[string]int{"a": 1}, b: map[string]int{"b": 1}, equal: false},
		{name: "NaN", a: math.NaN(), b: math.NaN(), equal: false},
		{name: "nil funcs", a: withFunc{}, b: withFunc{}, equal: true},
		{name: "same func", a: withFunc{Fn: fn}, b: withFunc{Fn: fn}, equal: false},
		{name: "other dynamic types", a: any(1), b: any(int64(1)), equal: false},
		{name: "distinct equal cycles", a: cycleA, b: cycleB, equal: true},
		{name: "arrays", a: [2]string{"a", "b"}, b: [2]string{"a", "c"}, equal: false},
		{name: "nil interfaces", a: nil, b: nil, equal: true},
	} {
		if got := Equal(tc.a, tc.b); got != tc.equal {
			t.Errorf("%s: Equal = %v, want %v", tc.name, got, tc.equal)
		}
	}
}
