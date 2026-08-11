// Package skills implements progressive-disclosure markdown skills: only
// name + description enter the system prompt; the model reads the full
// SKILL.md with read_file when it decides a skill applies.
package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

type Skill struct {
	Name        string
	Description string
	Path        string // full path to SKILL.md
}

// Loader scans one or more roots; earlier roots win on name collisions
// (workspace overrides global).
type Loader struct {
	roots []string
}

func NewLoader(roots ...string) *Loader {
	var kept []string
	for _, r := range roots {
		if r != "" {
			kept = append(kept, r)
		}
	}
	return &Loader{roots: kept}
}

// Roots exposes the scan roots (the prompt cache stamps their SKILL.md files).
func (l *Loader) Roots() []string { return l.roots }

func (l *Loader) List() []Skill {
	seen := map[string]bool{}
	var out []Skill
	for _, root := range l.roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			path := filepath.Join(root, e.Name(), "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			meta, body := parseFrontmatter(string(data))
			name := meta["name"]
			if name == "" {
				name = e.Name()
			}
			desc := meta["description"]
			if desc == "" {
				desc = firstParagraph(body)
			}
			seen[e.Name()] = true
			out = append(out, Skill{Name: name, Description: desc, Path: path})
		}
	}
	return out
}

// Summary renders the prompt catalog: summaries only, full skill on demand.
func (l *Loader) Summary() string {
	list := l.List()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Skills\n\nAvailable skills (read the SKILL.md with read_file before using one):\n")
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s (%s)\n", s.Name, s.Description, s.Path)
	}
	return b.String()
}

// parseFrontmatter reads a minimal `key: value` YAML block between --- fences.
// Hand-rolled on purpose: no YAML dependency for a hot-path parse.
func parseFrontmatter(content string) (map[string]string, string) {
	meta := map[string]string{}
	if !strings.HasPrefix(content, "---\n") {
		return meta, content
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return meta, content
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		meta[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return meta, rest[end+4:]
}

func firstParagraph(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > 200 {
			line = line[:200]
		}
		return line
	}
	return "(no description)"
}

var skillNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// InstallTool installs a skill from a git URL or local directory into the
// workspace skills root.
type InstallTool struct {
	Root string // workspace/skills
}

func (t *InstallTool) Name() string { return "skill_install" }
func (t *InstallTool) Description() string {
	return "Install a skill into the workspace from a git repository URL or a local directory containing SKILL.md."
}
func (t *InstallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source": map[string]any{"type": "string", "description": "git URL (https://...) or local directory path"},
			"name":   map[string]any{"type": "string", "description": "Skill directory name (defaults to source basename)"},
		},
		"required": []any{"source"},
	}
}

func (t *InstallTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	source := strings.TrimSpace(tools.StringArg(args, "source"))
	name := strings.TrimSpace(tools.StringArg(args, "name"))
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(source), ".git")
	}
	if !skillNameRe.MatchString(name) {
		return tools.Errorf("invalid skill name %q", name)
	}
	dest := filepath.Join(t.Root, name)
	if _, err := os.Stat(dest); err == nil {
		return tools.Errorf("skill %q already exists at %s", name, dest)
	}

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "git@") {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", source, dest)
		if out, err := cmd.CombinedOutput(); err != nil {
			return tools.Errorf("git clone failed: %v\n%s", err, out)
		}
	} else {
		info, err := os.Stat(source)
		if err != nil || !info.IsDir() {
			return tools.Errorf("source %q is not a directory", source)
		}
		if err := os.CopyFS(dest, os.DirFS(source)); err != nil {
			return tools.Errorf("copy failed: %v", err)
		}
	}

	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		_ = os.RemoveAll(dest)
		return tools.Errorf("source has no SKILL.md at its root; not a skill")
	}
	return tools.Textf("Installed skill %q to %s", name, dest)
}
