package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootsDropsEmptyEntries(t *testing.T) {
	roots := NewLoader("/workspace/skills", "", "/global/skills").Roots()
	if len(roots) != 2 || roots[0] != "/workspace/skills" || roots[1] != "/global/skills" {
		t.Errorf("Roots() = %v, want the two non-empty roots in order", roots)
	}
	if got := NewLoader("").Roots(); len(got) != 0 {
		t.Errorf("Roots() = %v, want empty for a blank root", got)
	}
	if got := NewLoader().Roots(); len(got) != 0 {
		t.Errorf("Roots() = %v, want empty for no roots", got)
	}
}

func TestListSkipsMissingRootsAndNonSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "real", "---\nname: real\ndescription: a real skill\n---\n")
	if err := os.MkdirAll(filepath.Join(root, "no-skill-md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose.md"), []byte("not a skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	list := NewLoader(filepath.Join(root, "does-not-exist"), root).List()
	if len(list) != 1 || list[0].Name != "real" {
		t.Fatalf("list = %+v, want only the directory with a SKILL.md", list)
	}
}

func TestListFallsBackToDirectoryName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "unnamed-skill", "---\ndescription: frontmatter without a name\n---\n")
	list := NewLoader(root).List()
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Name != "unnamed-skill" {
		t.Errorf("Name = %q, want the directory name", list[0].Name)
	}
	if list[0].Description != "frontmatter without a name" {
		t.Errorf("Description = %q", list[0].Description)
	}
}

func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantMeta map[string]string
		wantBody string
	}{
		{
			name:     "no frontmatter leaves the content untouched",
			content:  "# Backup helper\n\nRuns rsync nightly.\n",
			wantMeta: map[string]string{},
			wantBody: "# Backup helper\n\nRuns rsync nightly.\n",
		},
		{
			name:     "unterminated fence is not frontmatter",
			content:  "---\nname: orphan\ndescription: the fence is never closed\n",
			wantMeta: map[string]string{},
			wantBody: "---\nname: orphan\ndescription: the fence is never closed\n",
		},
		{
			name:     "indented and colon-less lines are ignored",
			content:  "---\nname: deploy\n  nested: ignored\n\ttabbed: ignored\nno colon here\n---\nbody\n",
			wantMeta: map[string]string{"name": "deploy"},
			wantBody: "\nbody\n",
		},
		{
			name:     "quoted values are unquoted",
			content:  "---\nname: \"weather\"\ndescription: 'Get forecasts'\n---\nbody\n",
			wantMeta: map[string]string{"name": "weather", "description": "Get forecasts"},
			wantBody: "\nbody\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta, body := parseFrontmatter(c.content)
			if len(meta) != len(c.wantMeta) {
				t.Fatalf("meta = %v, want %v", meta, c.wantMeta)
			}
			for k, want := range c.wantMeta {
				if meta[k] != want {
					t.Errorf("meta[%q] = %q, want %q", k, meta[k], want)
				}
			}
			if body != c.wantBody {
				t.Errorf("body = %q, want %q", body, c.wantBody)
			}
		})
	}
}

func TestFirstParagraph(t *testing.T) {
	long := strings.Repeat("x", 250)
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "skips headings and blank lines",
			body: "\n\n# Heading\n\n## Subheading\n\nThe actual description.\nand more\n",
			want: "The actual description.",
		},
		{
			name: "truncates at 200 characters",
			body: long,
			want: long[:200],
		},
		{
			name: "empty body",
			body: "",
			want: "(no description)",
		},
		{
			name: "headings only",
			body: "# One\n\n## Two\n\n",
			want: "(no description)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstParagraph(c.body); got != c.want {
				t.Errorf("firstParagraph() = %q, want %q", got, c.want)
			}
		})
	}
}
