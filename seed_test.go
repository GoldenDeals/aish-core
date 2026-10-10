package aish

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type seedCall struct{ ID, Name string }

func (seedCall) Kind() string { return "test.seed-call" }

type seedResult struct{ CallID string }

func (seedResult) Kind() string { return "test.seed-result" }

func TestSeedRestoresWithoutHooks(t *testing.T) {
	t.Parallel()

	// Record a session; its journal stands in for what a store module
	// restored from disk.
	src, ctx := newTestSession(t)
	h := src.host("tool.fs")
	h.Handle("exec", func(ctx context.Context, c seedCall) error {
		return ignoreUnhandled(h.Emit(ctx, seedResult{CallID: c.ID}))
	})
	for _, id := range []string{"a", "b"} {
		if err := h.Emit(ctx, seedCall{ID: id, Name: "fs_read"}); err != nil {
			t.Fatal(err)
		}
	}
	restored := src.journal.Txs()

	// Seed it into a fresh session, as a store does in Init.
	s := newSession(sessionConfig{})
	t.Cleanup(func() { _ = s.close(context.Background()) })
	store := s.host("store.jsonl")
	var ran, observed int
	s.host("tool.fs").Handle("exec", func(context.Context, seedCall) error { ran++; return nil })
	store.Observe("save", func(context.Context, TxCommitted) { observed++ })
	if err := store.Seed(restored...); err != nil {
		t.Fatal(err)
	}
	if err := store.Seed(restored[0]); err == nil {
		t.Error("Seed accepted a sequence number that does not increase")
	}

	s.start()
	if err := store.Seed(); err == nil {
		t.Error("Seed succeeded after the session started")
	}
	if err := store.Emit(s.startContext(), seedResult{CallID: "late"}); !errors.Is(err, ErrUnhandled) {
		t.Fatal(err)
	}

	if ran != 0 {
		t.Errorf("a seeded tool call ran %d times", ran)
	}
	if observed != 1 {
		t.Errorf("store observed %d commits, want only the new one", observed)
	}
	txs := s.journal.Txs()
	seqs := make([]uint64, len(txs))
	for i, tx := range txs {
		seqs[i] = tx.Seq
	}
	if !slices.Equal(seqs, []uint64{1, 2, 3}) {
		t.Errorf("journal seqs = %v, want the seeded 1, 2 and the new 3", seqs)
	}
	if c := txs[1].Events[0].Event.(seedCall); c.ID != "b" {
		t.Errorf("seeded tx 2 starts with %+v, want call b", c)
	}
}
