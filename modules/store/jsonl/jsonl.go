// Package jsonl is the session store: it appends every committed transaction
// to <Dir>/<session>.jsonl and restores the journal from that file on resume.
package jsonl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/coredns/caddy/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
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

	h    *aish.Host
	root *os.Root
	file *os.File
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

// UnmarshalCaddyfile reads "store jsonl [dir]".
func (s *Store) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	switch args := d.RemainingArgs(); len(args) {
	case 0:
	case 1:
		s.Dir = args[0]
	default:
		return d.ArgErr()
	}
	if d.NextBlock() {
		return d.Err("store jsonl takes no block")
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
	name := id + ".jsonl"

	if err := os.MkdirAll(s.Dir, dirMode); err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}
	s.root = root

	if h.Options().Resume != "" {
		if err := s.restore(name); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}
	s.file = f
	h.Observe("write", s.write)

	return nil
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
	}
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

// restore seeds the journal from the file name in Dir. A truncated last line
// (the process died while writing) ends the file; events of unknown kind are
// skipped.
func (s *Store) restore(name string) error {
	f, err := s.root.Open(name)
	if err != nil {
		return fmt.Errorf("aish: store.jsonl: resume: %w", err)
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
		if errors.Is(err, io.ErrUnexpectedEOF) {
			s.h.Log().Warn("dropping truncated last record", "file", name)
			if err := s.cutTail(name, dec.InputOffset()); err != nil {
				return err
			}

			break
		}
		if err != nil {
			return fmt.Errorf("aish: store.jsonl: read %s: %w", name, err)
		}
		tx, err := s.decode(rec)
		if err != nil {
			return err
		}
		txs = append(txs, tx)
	}
	if err := s.h.Seed(txs...); err != nil {
		return fmt.Errorf("aish: store.jsonl: %w", err)
	}

	return nil
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
