// Package jsonl is the session store: it appends every committed transaction
// to <Dir>/<session>.jsonl and restores the journal from that file on resume.
package jsonl

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/internal/rotate"
)

const (
	dirMode  = 0o755
	fileMode = 0o600
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "store.jsonl", New: func() aish.Module { return &Store{} }})
}

type Store struct {
	// Dir holds the session files; ~/.aish/sessions when empty.
	Dir string

	// Rotate starts a new segment file <session>.<unix time>.jsonl once this
	// much time has passed since the current one was opened; resume reads all
	// segments in order. Zero keeps one file per session.
	Rotate time.Duration

	h    *aish.Host
	root *os.Root
	file *rotate.Writer
}

// record is one line of the file. Trace is not stored: it is for the live
// session.
type record struct {
	Seq    uint64
	Status aish.TxStatus
	Events []eventRecord
}

type eventRecord struct {
	Meta  aish.Meta
	Kind  string
	Event json.RawMessage
}

// UnmarshalCaddyfile reads "store jsonl [dir] { rotate <duration> }".
func (s *Store) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	switch args := d.RemainingArgs(); len(args) {
	case 0:
	case 1:
		s.Dir = args[0]
	default:
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		if d.Val() != "rotate" {
			return d.Errf("unknown store jsonl option %q", d.Val())
		}
		p, err := rotate.Period(d)
		if err != nil {
			return err //nolint:wrapcheck // carries the Agentfile position
		}
		s.Rotate = p
	}

	return nil
}

func (s *Store) Init(_ context.Context, h *aish.Host) error {
	s.h = h
	if err := s.resolveDir(); err != nil {
		return err
	}
	// A resumed session continues the file it was restored from.
	id := h.Session().ID
	if resume := h.Options().Resume; resume != "" {
		id = resume
	}
	if id != filepath.Base(id) {
		return fmt.Errorf("aish: store.jsonl: invalid session ID %q", id)
	}

	if err := os.MkdirAll(s.Dir, dirMode); err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}
	s.root = root

	current := id + ".jsonl"
	if h.Options().Resume != "" {
		names, err := s.segments(id)
		if err != nil {
			return err
		}
		if err := s.restore(names); err != nil {
			return err
		}
		current = names[len(names)-1]
	}
	first := true
	s.file, err = rotate.Open(&rotate.Writer{
		Period: s.Rotate,
		Name: func(now time.Time) string {
			if first {
				first = false

				return current
			}

			return fmt.Sprintf("%s.%d.jsonl", id, now.Unix())
		},
		Create: func(name string) (io.WriteCloser, error) {
			return root.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		},
	})
	if err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}
	h.Observe("write", s.write)

	return nil
}

// segments lists the files of a session in write order: <id>.jsonl, then
// <id>.<unix time>.jsonl by time.
func (s *Store) segments(id string) ([]string, error) {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("aish: store.jsonl: resume: %w", err)
	}
	var names []string
	for _, e := range entries {
		stamp, ok := strings.CutPrefix(e.Name(), id+".")
		stamp, ok2 := strings.CutSuffix(stamp, ".jsonl")
		if e.Name() == id+".jsonl" || (ok && ok2 && stamp != "" && strings.Trim(stamp, "0123456789") == "") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("aish: store.jsonl: resume: no file for session %q in %s: %w", id, s.Dir, fs.ErrNotExist)
	}
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b))
	})

	return names, nil
}

// resolveDir fills in the default directory and expands a leading "~".
func (s *Store) resolveDir() error {
	if s.Dir != "" && s.Dir != "~" && !strings.HasPrefix(s.Dir, "~/") {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}
	if s.Dir == "" {
		s.Dir = filepath.Join(home, ".aish", "sessions")
	} else {
		s.Dir = filepath.Join(home, s.Dir[1:])
	}

	return nil
}

func (s *Store) Stop(context.Context) error {
	var errs []error
	if s.file != nil {
		errs = append(errs, s.file.Close())
	}
	if s.root != nil {
		errs = append(errs, s.root.Close())
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}

	return nil
}

func (s *Store) write(ctx context.Context, c aish.TxCommitted) {
	line, err := encode(c.Tx)
	if err == nil {
		_, err = s.file.Write(line)
	}
	if err != nil {
		s.h.Log().ErrorContext(ctx, "cannot store transaction", "seq", c.Tx.Seq, "err", err)

		return
	}
	s.h.Log().DebugContext(ctx, "transaction stored", "seq", c.Tx.Seq, "bytes", len(line))
}

func encode(tx aish.Tx) ([]byte, error) {
	rec := record{Seq: tx.Seq, Status: tx.Status}
	for _, env := range tx.Events {
		raw, err := json.Marshal(env.Event)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", env.Event.Kind(), err)
		}
		rec.Events = append(rec.Events, eventRecord{Meta: env.Meta, Kind: env.Event.Kind(), Event: raw})
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encode transaction %d: %w", tx.Seq, err)
	}

	return append(line, '\n'), nil
}

// restore seeds the journal from the files in Dir, oldest first. A truncated
// last line of the newest file (the process died while writing) ends it;
// events of unknown kind are skipped.
func (s *Store) restore(names []string) error {
	var txs []aish.Tx
	for i, name := range names {
		read, err := s.readFile(name, i == len(names)-1)
		if err != nil {
			return err
		}
		txs = append(txs, read...)
	}
	s.h.Log().Debug("restored session", "files", names, "transactions", len(txs))
	if err := s.h.Seed(txs...); err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}

	return nil
}

func (s *Store) readFile(name string, newest bool) ([]aish.Tx, error) {
	f, err := s.root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("aish: store.jsonl: resume: %w", err)
	}
	defer f.Close()

	var txs []aish.Tx
	dec := json.NewDecoder(f)
	for {
		var rec record
		err := dec.Decode(&rec)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) && newest {
			s.h.Log().Warn("dropping truncated last record", "file", name)
			if err := s.cutTail(name, dec.InputOffset()); err != nil {
				return nil, err
			}

			break
		}
		if err != nil {
			return nil, fmt.Errorf("aish: store.jsonl: read %s: %w", name, err)
		}
		tx, err := s.decode(rec)
		if err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}

	return txs, nil
}

// cutTail removes everything after the last whole record (the process died
// while writing) so that the next record starts on a line of its own.
func (s *Store) cutTail(name string, end int64) error {
	f, err := s.root.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}
	defer f.Close()

	if err := f.Truncate(end); err != nil {
		return fmt.Errorf("aish: store.jsonl: truncate %s: %w", name, err)
	}
	if _, err := f.WriteAt([]byte{'\n'}, end); err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}

	return nil
}

func (s *Store) decode(rec record) (aish.Tx, error) {
	tx := aish.Tx{Seq: rec.Seq, Status: rec.Status}
	for _, er := range rec.Events {
		ev, known, err := aish.DecodeEvent(er.Kind, er.Event)
		if err != nil {
			return tx, fmt.Errorf("aish: store.jsonl: transaction %d: %w", rec.Seq, err)
		}
		if !known {
			s.h.Log().Warn("skipping event of unknown kind", "kind", er.Kind, "seq", rec.Seq)

			continue
		}
		tx.Events = append(tx.Events, aish.Envelope{Meta: er.Meta, Event: ev})
	}

	return tx, nil
}
