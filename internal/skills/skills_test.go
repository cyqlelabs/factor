package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

func writeSkill(t *testing.T, root, dir, content string) {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListAndFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "weather", "---\nname: weather\ndescription: Get forecasts\n---\n\nUse wttr.in curl calls.")
	writeSkill(t, root, "no-meta", "# Backup helper\n\nRuns rsync backups nightly.")

	list := NewLoader(root).List()
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	byName := map[string]Skill{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if byName["weather"].Description != "Get forecasts" {
		t.Errorf("weather = %+v", byName["weather"])
	}
	if byName["no-meta"].Description != "Runs rsync backups nightly." {
		t.Errorf("fallback description = %+v", byName["no-meta"])
	}
}

func TestRootPrecedence(t *testing.T) {
	ws, global := t.TempDir(), t.TempDir()
	writeSkill(t, ws, "deploy", "---\nname: deploy\ndescription: workspace version\n---\n")
	writeSkill(t, global, "deploy", "---\nname: deploy\ndescription: global version\n---\n")
	list := NewLoader(ws, global).List()
	if len(list) != 1 || list[0].Description != "workspace version" {
		t.Errorf("precedence broken: %+v", list)
	}
}

func TestSummaryProgressiveDisclosure(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "weather", "---\nname: weather\ndescription: Get forecasts\n---\n\nSECRET-BODY-DETAIL")
	summary := NewLoader(root).Summary()
	if !strings.Contains(summary, "weather: Get forecasts") {
		t.Errorf("summary = %q", summary)
	}
	if strings.Contains(summary, "SECRET-BODY-DETAIL") {
		t.Error("skill body leaked into summary")
	}
	if empty := NewLoader(t.TempDir()).Summary(); !strings.Contains(empty, "skill_write") {
		t.Errorf("empty catalog must still teach skill_write: %q", empty)
	}
}

func TestWriteToolRoundTrip(t *testing.T) {
	root := t.TempDir()
	write := &WriteTool{Root: root}

	res := write.Execute(context.Background(), map[string]any{
		"name": "cast_media", "description": "Cast a video\nto the TV", "content": "Run cast.py",
	})
	if res.IsError {
		t.Fatalf("write: %+v", res)
	}
	list := NewLoader(root).List()
	if len(list) != 1 || list[0].Description != "Cast a video to the TV" {
		t.Fatalf("written skill not indexed: %+v", list)
	}

	// update replaces in place
	res = write.Execute(context.Background(), map[string]any{
		"name": "cast_media", "description": "Cast media", "content": "Run cast_media.py",
	})
	if res.IsError || !strings.Contains(res.ForLLM, "Updated") {
		t.Fatalf("update: %+v", res)
	}
	if list = NewLoader(root).List(); len(list) != 1 || list[0].Description != "Cast media" {
		t.Fatalf("update did not replace: %+v", list)
	}

	for _, bad := range []map[string]any{
		{"name": "../escape", "description": "d", "content": "c"},
		{"name": "ok", "description": "", "content": "c"},
		{"name": "ok", "description": "d", "content": "  "},
		{"name": "ok", "description": "d", "content": "---\nname: ok\n---\n"},
	} {
		if res := write.Execute(context.Background(), bad); !res.IsError {
			t.Errorf("accepted bad args %v", bad)
		}
	}
}

// A skill named in Spanish keeps its spelling: an ASCII-only rule sent the
// model back with an asciified name, which wrote a second directory beside the
// one it had already filled.
func TestWriteToolKeepsAnAccentedNameInOneDirectory(t *testing.T) {
	root := t.TempDir()
	write := &WriteTool{Root: root}
	const name = "acompañamiento-psicoanalítico"

	res := write.Execute(context.Background(), map[string]any{
		"name": name, "description": "Escucha con encuadre", "content": "Leer system_prompt.md",
	})
	if res.IsError {
		t.Fatalf("write: %+v", res)
	}
	if res = write.Execute(context.Background(), map[string]any{
		"name": name, "description": "Escucha con encuadre", "content": "Leer ethics.md",
	}); res.IsError || !strings.Contains(res.ForLLM, "Updated") {
		t.Fatalf("second write did not land in the same directory: %+v", res)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("skills root = %+v, want one %q", entries, name)
	}
}

func TestValidSkillName(t *testing.T) {
	for _, ok := range []string{"deploy", "cast_media", "a1-b_2", "acompañamiento-psicoanalítico", "日本語", "Ünicode"} {
		if !validSkillName(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "../escape", "a/b", `a\b`, ".hidden", "-leading", "_leading", "has space", "trailing\n", "nul\x00byte", "dot.name", "\xff\xfe"} {
		if validSkillName(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Content that already carries frontmatter must not be wrapped in a second
// block: the catalog reads the outer one, so the author's own name and
// description silently lose to it.
func TestWriteToolDoesNotStackFrontmatter(t *testing.T) {
	root := t.TempDir()
	write := &WriteTool{Root: root}
	res := write.Execute(context.Background(), map[string]any{
		"name":        "psy",
		"description": "Catalog line",
		"content":     "---\nname: psy\ndescription: authored line\n---\n\n# Body\n\nRead system_prompt.md.",
	})
	if res.IsError {
		t.Fatalf("write: %+v", res)
	}
	doc, err := os.ReadFile(filepath.Join(root, "psy", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(doc), "\n---\n"); n != 1 {
		t.Errorf("%d frontmatter fences in:\n%s", n, doc)
	}
	if strings.Contains(string(doc), "authored line") {
		t.Errorf("stale frontmatter survived:\n%s", doc)
	}
	if list := NewLoader(root).List(); len(list) != 1 || list[0].Description != "Catalog line" {
		t.Fatalf("catalog = %+v", list)
	}
}

func TestRemoveTool(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "stale", "---\nname: stale\ndescription: old\n---\n")
	remove := &RemoveTool{Root: root}

	if res := remove.Execute(context.Background(), map[string]any{"name": "stale"}); res.IsError {
		t.Fatalf("remove: %+v", res)
	}
	if list := NewLoader(root).List(); len(list) != 0 {
		t.Errorf("skill still listed: %+v", list)
	}
	if res := remove.Execute(context.Background(), map[string]any{"name": "stale"}); !res.IsError {
		t.Error("removing a missing skill must error")
	}
	// a non-skill directory under the root is not ours to delete recursively
	if err := os.MkdirAll(filepath.Join(root, "notaskill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := remove.Execute(context.Background(), map[string]any{"name": "notaskill"}); !res.IsError {
		t.Error("removed a directory with no SKILL.md")
	}
	if _, err := os.Stat(filepath.Join(root, "notaskill")); err != nil {
		t.Error("non-skill directory was deleted")
	}
	if res := remove.Execute(context.Background(), map[string]any{"name": "../escape"}); !res.IsError {
		t.Error("path-traversal name accepted")
	}
}

func TestInstallToolLocalDir(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tool := &InstallTool{Root: root}

	res := tool.Execute(context.Background(), map[string]any{"source": src, "name": "installed"})
	if res.IsError {
		t.Fatalf("install: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "installed", "SKILL.md")); err != nil {
		t.Error("skill not installed")
	}
	// duplicate rejected
	res = tool.Execute(context.Background(), map[string]any{"source": src, "name": "installed"})
	if !res.IsError {
		t.Error("duplicate install accepted")
	}
	// non-skill dir rejected and cleaned up
	res = tool.Execute(context.Background(), map[string]any{"source": t.TempDir(), "name": "junk"})
	if !res.IsError {
		t.Error("dir without SKILL.md accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "junk")); !os.IsNotExist(err) {
		t.Error("failed install not cleaned up")
	}
	// bad names rejected
	res = tool.Execute(context.Background(), map[string]any{"source": src, "name": "../escape"})
	if !res.IsError {
		t.Error("path-traversal name accepted")
	}
}

// Every skill tool is handed to the model as a name, a description and a JSON
// schema. A schema that requires a field it never describes, or a tool that
// arrives without a description, is unusable at the far end — and nothing on
// the way there checks it.
func TestSkillToolsDeclareUsableSchemas(t *testing.T) {
	root := t.TempDir()
	suite := []tools.Tool{
		&WriteTool{Root: root},
		&RemoveTool{Root: root},
		&InstallTool{Root: root},
		&FindTool{Registry: &Registry{}},
	}
	seen := map[string]bool{}
	for _, tool := range suite {
		name := tool.Name()
		if !strings.HasPrefix(name, "skill_") {
			t.Errorf("tool %q does not belong to the skill_ family", name)
		}
		if seen[name] {
			t.Errorf("two tools answer to %q; the later one would win silently", name)
		}
		seen[name] = true

		if len(strings.TrimSpace(tool.Description())) < 20 {
			t.Errorf("%s has no description the model can route on", name)
		}
		params := tool.Parameters()
		if params["type"] != "object" {
			t.Errorf("%s schema type = %v, want object", name, params["type"])
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s declares no properties map", name)
		}
		for key, raw := range props {
			spec, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("%s property %q is %T, want map[string]any", name, key, raw)
				continue
			}
			if desc, _ := spec["description"].(string); strings.TrimSpace(desc) == "" {
				t.Errorf("%s property %q has no description", name, key)
			}
		}
		for _, req := range tools.SchemaStrings(params["required"]) {
			if _, described := props[req]; !described {
				t.Errorf("%s requires %q but never describes it", name, req)
			}
		}
	}
}
