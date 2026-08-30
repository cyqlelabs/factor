package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLearnedCreatesWithMarker(t *testing.T) {
	root := t.TempDir()
	path, existed, err := WriteLearned(root, "deploy-check", "verify a deploy landed", "1. call probe\n2. read the result")
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("reported existing on first write")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	if !strings.Contains(doc, "learned: true") {
		t.Fatalf("marker missing:\n%s", doc)
	}
	if !strings.Contains(doc, "name: deploy-check") || !strings.Contains(doc, "1. call probe") {
		t.Fatalf("content missing:\n%s", doc)
	}

	_, existed, err = WriteLearned(root, "deploy-check", "verify a deploy landed", "1. new steps\n2. still two")
	if err != nil || !existed {
		t.Fatalf("update: existed=%v err=%v", existed, err)
	}
}

func TestWriteLearnedValidation(t *testing.T) {
	root := t.TempDir()
	if _, _, err := WriteLearned(root, "bad/name", "d", "b"); err == nil {
		t.Fatal("accepted a name with a separator")
	}
	if _, _, err := WriteLearned(root, "ok", "   ", "b"); err == nil {
		t.Fatal("accepted an empty description")
	}
	if _, _, err := WriteLearned(root, "ok", "d", "  "); err == nil {
		t.Fatal("accepted an empty body")
	}
}

func TestWriteLearnedRefusesHandWrittenSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "manual", "---\nname: manual\ndescription: mine\n---\n\nbody\n")
	if _, existed, err := WriteLearned(root, "manual", "d", "b"); err == nil || !existed {
		t.Fatalf("expected refusal of a hand-written skill, got existed=%v err=%v", existed, err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "manual", "SKILL.md"))
	if !strings.Contains(string(data), "description: mine") {
		t.Fatal("hand-written skill was modified")
	}
}

func TestWriteLearnedStripsSuppliedFrontmatter(t *testing.T) {
	root := t.TempDir()
	path, _, err := WriteLearned(root, "wrapped", "outer wins", "---\nname: inner\ndescription: stale\n---\n\nreal body")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	doc := string(data)
	if strings.Contains(doc, "inner") || strings.Count(doc, "---") != 2 {
		t.Fatalf("inner frontmatter survived:\n%s", doc)
	}
	if !strings.Contains(doc, "real body") {
		t.Fatalf("body lost:\n%s", doc)
	}
}

func TestLearnedListsOnlyMarkedSkills(t *testing.T) {
	root := t.TempDir()
	if _, _, err := WriteLearned(root, "auto", "a learned one", "the body"); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "manual", "---\nname: manual\ndescription: mine\n---\n\nbody\n")
	got := Learned(root)
	if len(got) != 1 || got[0].Name != "auto" || got[0].Description != "a learned one" {
		t.Fatalf("Learned() = %+v", got)
	}
	if Learned(filepath.Join(root, "missing")) != nil {
		t.Fatal("missing root should list nothing")
	}
}

func TestSkillWriteDropsLearnedMarker(t *testing.T) {
	root := t.TempDir()
	if _, _, err := WriteLearned(root, "auto", "learned", "old body"); err != nil {
		t.Fatal(err)
	}
	res := (&WriteTool{Root: root}).Execute(context.Background(), map[string]any{
		"name": "auto", "description": "curated now", "content": "new body",
	})
	if res.IsError {
		t.Fatalf("skill_write failed: %s", res.ForLLM)
	}
	data, _ := os.ReadFile(filepath.Join(root, "auto", "SKILL.md"))
	if strings.Contains(string(data), "learned: true") {
		t.Fatalf("marker survived a deliberate rewrite:\n%s", data)
	}
	// Once curated, induction may not touch it again.
	if _, _, err := WriteLearned(root, "auto", "d", "b"); err == nil {
		t.Fatal("induction replaced a curated skill")
	}
}
