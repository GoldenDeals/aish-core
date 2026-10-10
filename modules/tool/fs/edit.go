package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	dirMode  = 0o755
	fileMode = 0o644
)

// Edit is the module tool.fs_edit.
type Edit struct{ tool }

func (e *Edit) Init(_ context.Context, h *aish.Host) error {
	e.spec = events.ToolSpec{
		Name: "fs_edit",
		Description: "Edit a file. command=write replaces the whole file with content (creating it); " +
			"command=str_replace replaces old_string, which must occur exactly once, with new_string.",
		Schema: json.RawMessage(`{"type":"object","required":["command","path"],"properties":{` +
			`"command":{"enum":["write","str_replace"]},"path":{"type":"string"},"content":{"type":"string"},` +
			`"old_string":{"type":"string"},"new_string":{"type":"string"}}}`),
	}
	e.action, e.run = "fs.write", e.edit

	return e.init(h)
}

func (e *Edit) edit(input json.RawMessage) (string, error) {
	var in struct {
		Command   string
		Path      string
		Content   string
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	path, ok := e.path(input)
	if !ok {
		return "", errors.New("path is required")
	}
	rel, err := e.rel(path)
	if err != nil {
		return "", err
	}

	switch in.Command {
	case "write":
		if err := e.root.MkdirAll(filepath.Dir(rel), dirMode); err != nil {
			return "", fmt.Errorf("create directory for %s: %w", in.Path, err)
		}
		if err := e.root.WriteFile(rel, []byte(in.Content), fileMode); err != nil {
			return "", fmt.Errorf("write %s: %w", in.Path, err)
		}

		return "wrote " + in.Path, nil
	case "str_replace":
		return e.replace(rel, in.Path, in.OldString, in.NewString)
	default:
		return "", fmt.Errorf("unknown command %q, want write or str_replace", in.Command)
	}
}

func (e *Edit) replace(rel, shown, oldText, newText string) (string, error) {
	if oldText == "" {
		return "", errors.New("old_string must not be empty")
	}
	data, err := e.root.ReadFile(rel)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", shown, err)
	}
	if n := strings.Count(string(data), oldText); n != 1 {
		return "", fmt.Errorf("old_string occurs %d times in %s, want exactly 1", n, shown)
	}
	updated := strings.Replace(string(data), oldText, newText, 1)
	if err := e.root.WriteFile(rel, []byte(updated), fileMode); err != nil {
		return "", fmt.Errorf("write %s: %w", shown, err)
	}

	return "edited " + shown, nil
}
