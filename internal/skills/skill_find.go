package skills

import (
	"context"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/tools"
)

const (
	findDefaultLimit = 10
	findMaxLimit     = 25
)

// FindTool searches the public registry for a skill someone else already
// wrote. The catalog in the prompt only ever holds what is installed here, so
// without this the answer to "is there a skill for this?" is always no and the
// agent rebuilds from scratch what thousands of repositories already carry.
type FindTool struct {
	Registry  *Registry
	Installed *Loader // optional: marks hits that are already on disk
}

func (t *FindTool) Name() string { return "skill_find" }
func (t *FindTool) Description() string {
	return "Search the public skill registry (skills.sh) for a published skill: document formats, API and framework recipes, deploy procedures, tool-specific conventions. Use it before writing a procedure from scratch — an installed skill is a starting point you can edit. This searches the registry, not the skills you already have (those are in your prompt catalog). Install a result with skill_install using the slug it returns."
}

func (t *FindTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "What the skill should do, in the words a repository would use, e.g. \"fill pdf forms\" or \"stripe webhooks\"",
			},
			"owner": map[string]any{
				"type":        "string",
				"description": "Optional GitHub owner to search within, e.g. \"anthropics\"",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum results (default 10, max 25)",
			},
		},
		"required": []any{"query"},
	}
}

func (t *FindTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	query := strings.TrimSpace(tools.StringArg(args, "query"))
	if query == "" {
		return tools.Errorf("query is required: say what the skill should do")
	}
	owner := strings.ToLower(strings.TrimSpace(tools.StringArg(args, "owner")))
	limit := tools.IntArg(args, "limit", findDefaultLimit)
	if limit < 1 {
		limit = findDefaultLimit
	}
	if limit > findMaxLimit {
		limit = findMaxLimit
	}

	reg := t.Registry
	if reg == nil {
		reg = NewRegistry("")
	}
	hits, err := reg.Search(ctx, query, owner, limit)
	if err != nil {
		return tools.Errorf("skill search failed: %v", err)
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}

	scope := ""
	if owner != "" {
		scope = fmt.Sprintf(" from %s", owner)
	}
	if len(hits) == 0 {
		return tools.Textf("No skill%s on %s matches %q. Try the words a repository would use, or write the skill yourself with skill_write.",
			scope, reg.base(), query)
	}

	installed := t.installedNames()
	var b strings.Builder
	fmt.Fprintf(&b, "%d skill(s)%s matching %q on %s, in the registry's own ranking:\n\n", len(hits), scope, query, reg.base())
	for _, s := range hits {
		name, slug := clean(s.Name, 80), clean(s.ID, 120)
		if name == "" {
			name = slug
		}
		fmt.Fprintf(&b, "- %s", name)
		if source := clean(s.Source, 80); source != "" {
			fmt.Fprintf(&b, " (%s)", source)
		}
		if s.Installs > 0 {
			fmt.Fprintf(&b, ", %d installs", s.Installs)
		}
		fmt.Fprintf(&b, "\n  slug: %s", slug)
		if installed[strings.ToLower(name)] {
			b.WriteString("  [a skill of this name is already installed]")
		}
		b.WriteString("\n")
		if desc := clean(s.Description, 200); desc != "" {
			fmt.Fprintf(&b, "  %s\n", desc)
		}
	}
	b.WriteString("\nInstall one with skill_install source=\"<slug>\", then read its SKILL.md before you rely on it: this is someone else's code, not yours.\n")
	return tools.Text(b.String())
}

// installedNames indexes what is already on disk so a hit the agent installed
// last week is not installed again under a suffixed name.
func (t *FindTool) installedNames() map[string]bool {
	names := map[string]bool{}
	if t.Installed == nil {
		return names
	}
	for _, s := range t.Installed.List() {
		names[strings.ToLower(s.Name)] = true
	}
	return names
}

// clean reduces one registry string to a single bounded line of printable
// text. Names, slugs and descriptions are a stranger's free text on its way
// into the prompt: a newline in one of them is a line of tool output nobody
// wrote, so the line ends where the registry's first one does.
func clean(s string, max int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.TrimSpace(strings.ToValidUTF8(s[:max], "")) + "…"
	}
	return s
}
