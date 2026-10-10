// Package skills offers Agent Skills (directories with a SKILL.md) to the
// model: a catalog in the system prompt and the tool skill that returns a
// skill's full text.
package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/events"
)

const (
	// SectionName is the system prompt section with the catalog.
	SectionName = "skills"
	toolName    = "skill"
	// enrichPriority is the enrichment band (§4.4); effectsPriority is the description band.
	enrichPriority  = 500
	effectsPriority = 2000
	fileName        = "SKILL.md"
)

func init() {
	aish.RegisterModule(aish.ModuleInfo{ID: "context.skills", New: func() aish.Module { return &Skills{} }})
}

// Skills is the module context.skills.
type Skills struct {
	// Dirs hold one directory per skill, each with a SKILL.md. For a skill
	// name found in several Dirs the later one wins.
	Dirs []string

	skills []skill
}

type skill struct {
	name, description, dir, body string
}

// UnmarshalCaddyfile reads "context skills <dir>...".
func (s *Skills) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.NextArg() // module name
	s.Dirs = d.RemainingArgs()
	if d.NextBlock(0) {
		return d.Errf("unexpected block")
	}

	return nil
}

func (s *Skills) Validate() error {
	if len(s.Dirs) == 0 {
		return errors.New("aish: context.skills: no skill directories")
	}

	return nil
}

func (s *Skills) Init(_ context.Context, h *aish.Host) error {
	found, err := load(s.Dirs)
	if err != nil {
		return err
	}
	s.skills = found
	h.Log().Debug("skills found", "dirs", s.Dirs, "count", len(found))
	if len(found) == 0 {
		return nil
	}
	spec := events.ToolSpec{
		Name:        toolName,
		Description: "Load the full instructions of a skill from the catalog in the system prompt.",
		Schema:      json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`),
	}
	catalog := s.catalog()

	h.Mutate("advertise", func(_ context.Context, m *aish.Mut[events.LLMRequest]) error {
		r := m.Event()
		r.Section(SectionName).Text = catalog
		r.AppendTool(spec)

		return nil
	}, aish.Priority(enrichPriority))
	h.Mutate("effects", func(_ context.Context, m *aish.Mut[events.ToolCall]) error {
		c := m.Event()
		if name, ok := skillName(c.Input); ok {
			c.Effects = events.Effects{ReadOnly: true, Access: []events.Access{{Action: "skill.read", Resource: "skill:" + name}}}
		}

		return nil
	}, aish.Priority(effectsPriority), aish.Where(isSkill))
	h.Handle("load", func(ctx context.Context, c events.ToolCall) error {
		res := events.ToolResult{CallID: c.ID}
		text, err := s.text(c.Input)
		h.Log().DebugContext(ctx, "skill loaded", "call", c.ID, "bytes", len(text), "err", err)
		if err != nil {
			res.IsError, text = true, err.Error()
		}
		res.Content = []events.Block{{Type: events.BlockText, Text: text}}
		if err := h.Emit(ctx, res); err != nil && !errors.Is(err, aish.ErrUnhandled) {
			return fmt.Errorf("aish: %s: %w", h.Instance(), err)
		}

		return nil
	}, aish.Where(isSkill))

	return nil
}

func isSkill(c events.ToolCall) bool { return c.Name == toolName }

func skillName(input json.RawMessage) (string, bool) {
	var in struct{ Name string }

	return in.Name, json.Unmarshal(input, &in) == nil && in.Name != ""
}

func (s *Skills) catalog() string {
	var b strings.Builder
	b.WriteString("Skills give you specialised instructions. When a task matches a skill, call the skill tool with its name before starting.\n")
	for _, k := range s.skills {
		fmt.Fprintf(&b, "\n- %s: %s", k.name, k.description)
	}

	return b.String()
}

func (s *Skills) text(input json.RawMessage) (string, error) {
	name, ok := skillName(input)
	if !ok {
		return "", errors.New("name is required")
	}
	for _, k := range s.skills {
		if k.name == name {
			return fmt.Sprintf("Skill directory: %s\n\n%s", k.dir, k.body), nil
		}
	}

	return "", fmt.Errorf("unknown skill %q", name)
}

// load reads every <dir>/*/SKILL.md; missing directories are skipped so the
// same config works with and without a project-level skills directory.
func load(dirs []string) ([]skill, error) {
	var found []skill
	for _, dir := range dirs {
		dir, err := expand(dir)
		if err != nil {
			return nil, err
		}
		files, err := filepath.Glob(filepath.Join(dir, "*", fileName))
		if err != nil {
			return nil, fmt.Errorf("aish: context.skills: %w", err)
		}
		slices.Sort(files)
		for _, file := range files {
			k, err := parse(file)
			if err != nil {
				return nil, err
			}
			found = slices.DeleteFunc(found, func(o skill) bool { return o.name == k.name })
			found = append(found, k)
		}
	}

	return found, nil
}

func expand(dir string) (string, error) {
	rest, ok := strings.CutPrefix(dir, "~")
	if !ok {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("aish: context.skills: %w", err)
	}

	return filepath.Join(home, rest), nil
}

func parse(file string) (skill, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // found under a configured directory
	if err != nil {
		return skill{}, fmt.Errorf("aish: context.skills: %w", err)
	}
	dir := filepath.Dir(file)
	meta, body := frontmatter(string(raw))
	k := skill{name: meta["name"], description: meta["description"], dir: dir, body: strings.TrimSpace(body)}
	if k.name == "" {
		k.name = filepath.Base(dir)
	}
	if k.description == "" {
		return skill{}, fmt.Errorf("aish: context.skills: %s: no description in the frontmatter", file)
	}

	return k, nil
}

// frontmatter splits a leading "---" block of "key: value" lines from the
// body. shortcut: values are single-line or a folded/literal block (> or |),
// no nested YAML; use a YAML parser when a skill needs more.
func frontmatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return meta, text
	}
	head, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return meta, text
	}
	_, body, _ = strings.Cut(body, "\n")

	key := ""
	for _, line := range strings.Split(head, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			key = strings.TrimSpace(k)
			if v = strings.TrimSpace(v); v != ">" && v != "|" && v != ">-" && v != "|-" {
				meta[key] = strings.Trim(v, `"'`)
			}

			continue
		}
		if key != "" && strings.TrimSpace(line) != "" {
			meta[key] = strings.TrimSpace(meta[key] + " " + strings.TrimSpace(line))
		}
	}

	return meta, body
}
