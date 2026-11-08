package skills

import (
	"context"
	"errors"
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

func TestLearnedFileKnowsOnlyLearnedSkills(t *testing.T) {
	root := t.TempDir()
	path, _, err := WriteLearned(root, "radio", "play the radio", "1. call media")
	if err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "mine", "---\nname: mine\ndescription: by hand\n---\n\nbody\n")

	dir, hash, ok := LearnedFile(root, path)
	if !ok || dir != "radio" || len(hash) != 12 {
		t.Fatalf("LearnedFile = %q %q %v", dir, hash, ok)
	}
	for _, other := range []string{
		filepath.Join(root, "mine", "SKILL.md"),
		filepath.Join(root, "radio", "notes.md"),
		filepath.Join(filepath.Dir(root), "SKILL.md"),
		filepath.Join(root, "gone", "SKILL.md"),
	} {
		if _, _, ok := LearnedFile(root, other); ok {
			t.Errorf("LearnedFile(%s) claimed a learned skill", other)
		}
	}
	if _, _, err := WriteLearned(root, "radio", "play the radio", "1. call media\n2. check status"); err != nil {
		t.Fatal(err)
	}
	if _, again, _ := LearnedFile(root, path); again == hash {
		t.Error("a rewritten skill hashed the same")
	}
}

// A quarantined skill leaves the catalog and the learned list but keeps its
// file, with the reason, for review; a skill somebody wrote is refused.
func TestQuarantineTakesALearnedSkillOutOfTheCatalog(t *testing.T) {
	root := t.TempDir()
	path, _, err := WriteLearned(root, "radio", "play the radio", "1. call media")
	if err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "mine", "---\nname: mine\ndescription: by hand\n---\n\nbody\n")

	_, hash, _ := LearnedFile(root, path)
	if err := Quarantine(root, "radio", hash, "corrected twice,\n in a row"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "quarantined: corrected twice, in a row\n") || !strings.Contains(string(data), "1. call media") {
		t.Fatalf("file after quarantine:\n%s", data)
	}
	for _, s := range NewLoader(root).List() {
		if s.Name == "radio" {
			t.Error("a quarantined skill is still in the catalog")
		}
	}
	if got := Learned(root); len(got) != 0 {
		t.Errorf("Learned() = %+v", got)
	}
	if _, _, ok := LearnedFile(root, path); ok {
		t.Error("a quarantined skill still reads as a learned one")
	}
	if err := Quarantine(root, "mine", hash, "x"); err == nil {
		t.Error("quarantined a hand-written skill")
	}
	if err := Quarantine(root, "gone", hash, "x"); err == nil {
		t.Error("quarantined a skill that does not exist")
	}
	// Induction writing it again is how it comes back.
	if _, _, err := WriteLearned(root, "radio", "play the radio", "1. call media"); err != nil {
		t.Fatal(err)
	}
	if got := Learned(root); len(got) != 1 {
		t.Errorf("a rewritten skill did not come back: %+v", got)
	}
}

// A skill rewritten since it was judged is a version nobody has used yet,
// and is not quarantined for its predecessor.
func TestQuarantineRefusesAVersionItDidNotJudge(t *testing.T) {
	root := t.TempDir()
	path, _, err := WriteLearned(root, "radio", "play the radio", "1. call exec")
	if err != nil {
		t.Fatal(err)
	}
	_, old, _ := LearnedFile(root, path)
	if _, _, err := WriteLearned(root, "radio", "play the radio", "1. call media"); err != nil {
		t.Fatal(err)
	}
	if err := Quarantine(root, "radio", old, "x"); !errors.Is(err, ErrSkillChanged) {
		t.Fatalf("err = %v, want ErrSkillChanged", err)
	}
	if got := Learned(root); len(got) != 1 {
		t.Errorf("the new version left the catalog: %+v", got)
	}
}
