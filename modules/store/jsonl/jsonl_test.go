package jsonl_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func start(t *testing.T, dir, resume string, c *counter) *test.Session {
	t.Helper()

	return test.NewSessionWith(t, aish.SessionConfig{
		ID:      "sess",
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
	s1 := start(t, dir, "", first)
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
	s2 := start(t, dir, "sess", second)
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

// A process that died while writing leaves half a record. Resume must cut it
// off, or the next record would be glued to it and break the following resume.
func TestResumeCutsTruncatedTail(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s1 := start(t, dir, "", &counter{})
	s1.Wait()
	if err := s1.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sess.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"Seq":9,"Sta`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := start(t, dir, "sess", &counter{})
	s2.Wait()
	if err := s2.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The third session reads what the second one appended.
	s3 := start(t, dir, "sess", &counter{})
	s3.Wait()
	if got := s3.Journal().Len(); got != 3 {
		t.Fatalf("journal has %d transactions, want 3 session.started", got)
	}
}

func TestRotatedSegmentsResume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := func(resume string) *test.Session {
		return test.NewSessionWith(t, aish.SessionConfig{
			ID:        "sess",
			Options:   aish.Options{Resume: resume},
			Instances: []aish.Instance{{Name: "store.jsonl", Module: &jsonl.Store{Dir: dir, Rotate: time.Millisecond}}},
		})
	}
	s1 := cfg("")
	for range 2 {
		time.Sleep(1100 * time.Millisecond) // segment names carry whole seconds
		_ = s1.Emit(events.UserMessage{Text: "hi"})
	}
	s1.Wait()
	if err := s1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	segments, _ := filepath.Glob(filepath.Join(dir, "sess*.jsonl"))
	if len(segments) < 2 {
		t.Fatalf("want rotated segments, got %v", segments)
	}

	want := len(s1.Journal().Txs())
	s2 := cfg("sess")
	s2.Wait()
	if got := len(s2.Journal().Txs()); got != want+1 {
		t.Fatalf("resumed %d txs, want %d + session.started", got, want)
	}
}
