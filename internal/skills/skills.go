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
	"unicode"

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
// It always renders, even with no skills installed: the closing note is how the
// model learns that reusable work belongs in a skill, written with skill_write.
func (l *Loader) Summary() string {
	var b strings.Builder
	b.WriteString("# Skills\n\n")
	if list := l.List(); len(list) == 0 {
		b.WriteString("No skills yet.\n")
	} else {
		b.WriteString("Available skills (read the SKILL.md with read_file before using one):\n")
		for _, s := range list {
			fmt.Fprintf(&b, "- %s: %s (%s)\n", s.Name, s.Description, s.Path)
		}
	}
	b.WriteString("\nA skill is a directory here holding a SKILL.md, and skill_write is how you create one and update it in place — assembling the directory by hand is how one skill ends up as two. Nothing here for the job at hand? skill_find searches the public registry before you build one from nothing.\n")
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

// validSkillName accepts a skill directory name: letters and digits in any
// script — a skill named in Spanish is spelled the way its author spells it,
// and an ASCII-only rule is how the same skill ends up in two directories —
// plus - and _ after the first character. Everything that could name a
// different file than it looks like (separators, dots, spaces, control
// characters, invalid UTF-8) is out, combining marks included: with no
// normalization available here, allowing them would let "n"+U+0303 and "ñ"
// name two directories that render identically — the duplicate this rule
// exists to prevent, in a form nobody can see.
func validSkillName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		if (r == '-' || r == '_') && i > 0 {
			continue
		}
		return false
	}
	return true
}

// registrySlugRe matches an owner/repo/skill slug as skill_find returns it.
// Three segments and no scheme is what separates a registry slug from the
// other two sources; an existing directory of that shape still wins, since a
// path the user can point at is never a guess.
var registrySlugRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+$`)

// InstallTool installs a skill from the public registry, a git URL, or a
// local directory into the workspace skills root.
type InstallTool struct {
	Root     string // workspace/skills
	Registry *Registry
}

func (t *InstallTool) Name() string { return "skill_install" }
func (t *InstallTool) Description() string {
	return "Install a skill into the workspace from an owner/repo/skill slug returned by skill_find, a git repository URL whose root holds a SKILL.md, or a local directory."
}
func (t *InstallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source": map[string]any{"type": "string", "description": "owner/repo/skill slug from skill_find, git URL (https://...), or local directory path"},
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
	if !validSkillName(name) {
		return tools.Errorf("invalid skill name %q: letters, digits, - and _, starting with a letter or digit", name)
	}
	dest := filepath.Join(t.Root, name)
	if _, err := os.Stat(dest); err == nil {
		return tools.Errorf("skill %q already exists at %s", name, dest)
	}

	switch info, statErr := os.Stat(source); {
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "git@"):
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", source, dest)
		if out, err := cmd.CombinedOutput(); err != nil {
			return tools.Errorf("git clone failed: %v\n%s", err, out)
		}
	case statErr != nil && registrySlugRe.MatchString(source):
		if err := t.installFromRegistry(ctx, source, dest); err != nil {
			_ = os.RemoveAll(dest)
			return tools.Errorf("registry install failed: %v", err)
		}
	default:
		if statErr != nil || !info.IsDir() {
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

// installFromRegistry writes a registry snapshot into dest. A snapshot is
// files chosen by a stranger's repository and carried over HTTP, so every path
// is resolved under dest before anything is written: it is data, not a say in
// where on the disk it lands.
func (t *InstallTool) installFromRegistry(ctx context.Context, slug, dest string) error {
	reg := t.Registry
	if reg == nil {
		reg = NewRegistry("")
	}
	files, err := reg.Download(ctx, slug)
	if err != nil {
		return err
	}
	for _, f := range files {
		rel := filepath.Clean(filepath.FromSlash(strings.TrimSpace(f.Path)))
		up := ".." + string(filepath.Separator)
		if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, up) {
			return fmt.Errorf("snapshot file %q would be written outside the skill directory", f.Path)
		}
		full := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(rel), err)
		}
		if err := os.WriteFile(full, []byte(f.Contents), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

// WriteTool authors a skill in place. Without it the model writes loose scripts
// into the skills root, which the directory+SKILL.md scan never indexes, so the
// work vanishes from the catalog the moment the session ends.
type WriteTool struct {
	Root string // workspace/skills
}

func (t *WriteTool) Name() string { return "skill_write" }
func (t *WriteTool) Description() string {
	return "Create or update a skill so it is indexed and listed in your prompt on every later turn. Use this for anything reusable you build (scripts, procedures, recipes) instead of writing files into the skills directory directly. Writing an existing name replaces its SKILL.md; helper scripts belong in the returned directory."
}
func (t *WriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "Skill directory name (letters, digits, - and _; accented and non-Latin letters are fine)"},
			"description": map[string]any{"type": "string", "description": "One line describing when to use this skill; this is what you see in the catalog"},
			"content":     map[string]any{"type": "string", "description": "Markdown body: what the skill does, how to run it, which files it uses"},
		},
		"required": []any{"name", "description", "content"},
	}
}

func (t *WriteTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	name := strings.TrimSpace(tools.StringArg(args, "name"))
	if !validSkillName(name) {
		return tools.Errorf("invalid skill name %q: letters, digits, - and _, starting with a letter or digit", name)
	}
	desc := strings.Join(strings.Fields(tools.StringArg(args, "description")), " ")
	if desc == "" {
		return tools.Errorf("description is required: it is the only thing you see in the catalog")
	}
	content := strings.TrimSpace(tools.StringArg(args, "content"))
	// A model handing over a whole SKILL.md already wrote its own frontmatter;
	// wrapping it again stacks two blocks and the catalog reads the outer one.
	if _, body := parseFrontmatter(content); body != content {
		content = strings.TrimSpace(body)
	}
	if content == "" {
		return tools.Errorf("content is required")
	}

	dir := filepath.Join(t.Root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tools.Errorf("create skill dir: %v", err)
	}
	path := filepath.Join(dir, "SKILL.md")
	_, statErr := os.Stat(path)
	existed := statErr == nil
	if err := os.WriteFile(path, []byte(renderSkillDoc(name, desc, content, false)), 0o644); err != nil {
		return tools.Errorf("write SKILL.md: %v", err)
	}

	verb := "Created"
	if existed {
		verb = "Updated"
	}
	return tools.Textf("%s skill %q (%s). Put helper scripts in %s and reference them from the body.", verb, name, path, dir)
}

// RemoveTool deletes a skill directory so retired work leaves the catalog.
type RemoveTool struct {
	Root string // workspace/skills
}

func (t *RemoveTool) Name() string { return "skill_remove" }
func (t *RemoveTool) Description() string {
	return "Delete a skill and its directory, removing it from your catalog. Use when a skill is obsolete or wrong."
}
func (t *RemoveTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "Skill directory name"},
		},
		"required": []any{"name"},
	}
}

func (t *RemoveTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	name := strings.TrimSpace(tools.StringArg(args, "name"))
	if !validSkillName(name) {
		return tools.Errorf("invalid skill name %q: letters, digits, - and _, starting with a letter or digit", name)
	}
	dir := filepath.Join(t.Root, name)
	// Confirm it is a skill before a recursive delete: a directory without
	// SKILL.md is something else that happens to live under the skills root.
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return tools.Errorf("no skill %q to remove (no %s)", name, filepath.Join(dir, "SKILL.md"))
	}
	if err := os.RemoveAll(dir); err != nil {
		return tools.Errorf("remove skill: %v", err)
	}
	return tools.Textf("Removed skill %q", name)
}
