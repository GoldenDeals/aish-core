package aish

import (
	"encoding/json"
	"testing"
)

type node struct {
	Name string
	Next *node
}

type cloneProbe struct {
	Map     map[string][]int
	Ptr     *node
	Any     any
	Raw     json.RawMessage
	Arr     [2][]string
	Custom  custom
	private []int
}

func (cloneProbe) Kind() string { return "test.clone" }

type custom struct{ Calls *int }

func (c custom) Clone() custom {
	*c.Calls++

	return c
}

func TestCloneEventIsDeep(t *testing.T) {
	t.Parallel()

	cycle := &node{Name: "a"}
	cycle.Next = cycle
	calls := 0
	orig := cloneProbe{
		Map:     map[string][]int{"k": {1}},
		Ptr:     cycle,
		Any:     map[string]any{"nested": []any{"x"}},
		Raw:     json.RawMessage(`{"a":1}`),
		Arr:     [2][]string{{"s"}},
		Custom:  custom{Calls: &calls},
		private: []int{7},
	}

	c, ok := cloneEvent(orig).(cloneProbe)
	if !ok {
		t.Fatalf("clone has type %T", cloneEvent(orig))
	}
	c.Map["k"][0] = 2
	c.Ptr.Name = "b"
	c.Any.(map[string]any)["nested"].([]any)[0] = "y"
	c.Raw[2] = 'b'
	c.Arr[0][0] = "t"

	switch {
	case orig.Map["k"][0] != 1:
		t.Error("map of slices is shared")
	case orig.Ptr.Name != "a":
		t.Error("pointer target is shared")
	case orig.Any.(map[string]any)["nested"].([]any)[0] != "x":
		t.Error("value behind interface is shared")
	case string(orig.Raw) != `{"a":1}`:
		t.Error("json.RawMessage is shared")
	case orig.Arr[0][0] != "s":
		t.Error("slice inside array is shared")
	}
	if c.Ptr.Next != c.Ptr {
		t.Error("pointer cycle is not preserved")
	}
	if calls != 1 {
		t.Errorf("Clone method called %d times, want 1", calls)
	}
	if &c.private[0] != &orig.private[0] {
		t.Error("unexported field was deep-copied; it should stay shared")
	}
}
