package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if NewLoader(t.TempDir()).Summary() != "" {
		t.Error("empty roots must produce empty summary")
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
