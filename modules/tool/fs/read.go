package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	maxFileBytes = 1 << 20
	defaultLines = 2000
	binarySniff  = 8000
)

// Read is the module tool.fs_read.
type Read struct{ tool }

func (r *Read) Init(_ context.Context, h *aish.Host) error {
	r.spec = events.ToolSpec{
		Name:        "fs_read",
		Description: "Read a text file. offset is the first line (1-based), limit the number of lines.",
		Schema: json.RawMessage(`{"type":"object","required":["path"],"properties":{` +
			`"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}}}`),
	}
	r.readOnly, r.action, r.run = true, "fs.read", r.read

	return r.init(h)
}

func (r *Read) read(input json.RawMessage) (string, error) {
	var in struct {
		Path          string
		Offset, Limit int
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	path, ok := r.path(input)
	if !ok {
		return "", errors.New("path is required")
	}
	rel, err := r.rel(path)
	if err != nil {
		return "", err
	}
	data, err := r.root.ReadFile(rel)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", in.Path, err)
	}
	if len(data) > maxFileBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d byte limit", in.Path, len(data), maxFileBytes)
	}
	if bytes.IndexByte(data[:min(len(data), binarySniff)], 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file", in.Path)
	}

	return lines(string(data), in.Offset, in.Limit), nil
}

// lines returns up to limit lines from the 1-based offset and says when it
// cut the file.
func lines(text string, offset, limit int) string {
	all := strings.SplitAfter(text, "\n")
	if all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	first := max(offset, 1) - 1
	if limit <= 0 {
		limit = defaultLines
	}
	if first >= len(all) {
		return fmt.Sprintf("[file has %d lines]", len(all))
	}
	last := min(first+limit, len(all))
	out := strings.Join(all[first:last], "")
	if first > 0 || last < len(all) {
		out += fmt.Sprintf("\n[lines %d-%d of %d]", first+1, last, len(all))
	}

	return out
}
