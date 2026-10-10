package main

import "testing"

func TestGreet(t *testing.T) {
	t.Parallel()

	if got, want := greet("aish"), "Hello, aish!"; got != want {
		t.Errorf("greet() = %q, want %q", got, want)
	}
}
