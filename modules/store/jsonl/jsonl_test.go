package jsonl_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
	"github.com/GoldenDeals/aish-core/modules/store/jsonl"
	"github.com/GoldenDeals/aish-core/test"
)

// counter counts the user messages its hook sees.
type counter struct{ n int }

func (c *counter) Init(_ context.Context, h *aish.Host) error {
	h.Handle("count", func(context.Context, events.UserMessage) error {
		c.n++

		return nil
	})

	return nil
}

func start(t *testing.T, dir, id, resume string, c *counter) *test.Session {
	t.Helper()

	return test.NewSessionWith(t, aish.SessionConfig{
		ID:      id,
		Options: aish.Options{Resume: resume},
		Instances: []aish.Instance{
			{Name: "store.jsonl", Module: &jsonl.Store{Dir: dir}},
			{Name: "test.counter", Module: c},
		},
	})
}

func TestResume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := &counter{}
	s1 := start(t, dir, "sess", "", first)
	if err := s1.Emit(events.UserMessage{Text: "hi"}); err != nil && !errors.Is(err, aish.ErrUnhandled) {
		t.Fatal(err)
	}
	s1.Wait()
	if err := s1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := s1.Journal().Txs()

	path := filepath.Join(dir, "sess.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(before, []byte("\n")); got != len(want) {
		t.Fatalf("file has %d records, journal %d", got, len(want))
	}

	second := &counter{}
	s2 := start(t, dir, "sess", "sess", second)
	s2.Wait()
	got := s2.Journal().Txs()
	if len(got) != len(want)+1 {
		t.Fatalf("resumed journal has %d txs, want %d restored + session.started", len(got), len(want))
	}
	for i, tx := range want {
		if got[i].Seq != tx.Seq || len(got[i].Events) != len(tx.Events) {
			t.Errorf("tx %d differs after resume: %+v vs %+v", i, got[i], tx)
		}
	}
	if last := got[len(got)-1]; last.Seq != want[len(want)-1].Seq+1 {
		t.Errorf("new tx seq = %d, want continuation of %d", last.Seq, want[len(want)-1].Seq)
	}
	if second.n != 0 {
		t.Errorf("hooks ran %d times for restored transactions", second.n)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) || bytes.Count(after, []byte("\n")) != len(want)+1 {
		t.Errorf("file must only grow by the new transaction")
	}
}

func TestResumeMissingFile(t *testing.T) {
	t.Parallel()

	s, err := aish.NewSession(aish.SessionConfig{
		Options:   aish.Options{Resume: "nope"},
		Instances: []aish.Instance{{Name: "store.jsonl", Module: &jsonl.Store{Dir: t.TempDir()}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Start = %v, want ErrNotExist", err)
	}
}
