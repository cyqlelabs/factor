package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/trace"
)

const learnedSkill = "---\nname: play-radio\ndescription: play a radio station\nlearned: true\n---\n\n1. call media\n2. check status\n"

func learnedSkillHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	writeSkillDir(t, skillsRoot(h), "play-radio", learnedSkill)
	return h, filepath.Join(skillsRoot(h), "play-radio", "SKILL.md")
}

func catalogHas(h *harness, name string) bool {
	for _, s := range skills.NewLoader(skillsRoot(h)).List() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// A turn that reads a learned skill and ends well is recorded on the trace
// and leaves the skill with a clean record.
func TestATurnThatReadsALearnedSkillIsTracedAndJudged(t *testing.T) {
	h, path := learnedSkillHarness(t)
	h.registry.Register(tools.NewFSTools(tools.NewPathGuard(h.loop.cfg.Agent.Workspace, true, false, nil))...)
	h.chat.script = append(h.chat.script, toolCall("read_file", map[string]any{"path": path}), final("playing"))
	dir := t.TempDir()
	rec := trace.NewRecorder(dir, trace.Config{Enabled: true, KeepDays: 14}, nil)
	t.Cleanup(func() { _ = rec.Close() })
	h.loop.WithTracer(rec)

	if _, err := h.loop.ProcessDirect(context.Background(), "put the radio on", "cli:x"); err != nil {
		t.Fatal(err)
	}
	recs, err := trace.Since(dir, time.Now().Add(-time.Minute))
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	if recs[0].Count(trace.EventSkillRead) != 1 {
		t.Errorf("the read was not traced: %+v", recs[0].Events)
	}
	uses := loadSkillUses()
	if u, ok := uses["play-radio"]; !ok || u.Bad != 0 || u.Hash == "" {
		t.Errorf("uses = %+v", uses)
	}
}

// Two corrected uses in a row take a learned skill out of the catalog and
// leave the file, with the reason, where it was.
func TestTwoCorrectedUsesQuarantineALearnedSkill(t *testing.T) {
	h, path := learnedSkillHarness(t)
	_, hash, ok := skills.LearnedFile(skillsRoot(h), path)
	if !ok {
		t.Fatal("the fixture is not a learned skill")
	}
	read := map[string]skillRead{"play-radio": {hash: hash, steeredAt: 0}}

	h.loop.judgeSkillUses(read, 1, "ok")
	if !catalogHas(h, "play-radio") {
		t.Fatal("one corrected use quarantined the skill")
	}
	h.loop.judgeSkillUses(read, 1, "ok")
	if catalogHas(h, "play-radio") {
		t.Fatal("two corrected uses in a row left the skill in the catalog")
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "quarantined: corrected in 2 uses in a row") {
		t.Fatalf("the file does not say why it left the catalog: %s %v", data, err)
	}
	if _, ok := loadSkillUses()["play-radio"]; ok {
		t.Error("a quarantined skill kept its record")
	}
}

// A turn that ended without an answer says nothing about the skill; one that
// ended well clears its record, even if the user corrected it before the
// skill was read, since that correction was about something else.
func TestOnlyACorrectionAfterTheReadCounts(t *testing.T) {
	h, path := learnedSkillHarness(t)
	_, hash, _ := skills.LearnedFile(skillsRoot(h), path)
	read := map[string]skillRead{"play-radio": {hash: hash, steeredAt: 0}}
	bad := func() int { return loadSkillUses()["play-radio"].Bad }

	h.loop.judgeSkillUses(read, 1, "ok")
	h.loop.judgeSkillUses(read, 0, "interrupted")
	if bad() != 1 {
		t.Fatalf("bad = %d after a correction and an interrupted turn, want 1", bad())
	}
	h.loop.judgeSkillUses(map[string]skillRead{"play-radio": {hash: hash, steeredAt: 1}}, 1, "ok")
	if bad() != 0 {
		t.Fatalf("bad = %d after a use corrected only before the read, want 0", bad())
	}
	h.loop.judgeSkillUses(read, 1, "ok")
	if !catalogHas(h, "play-radio") {
		t.Fatal("two corrections with a good use between them quarantined the skill")
	}
}

// A rewritten skill is a new skill: what the old version did is not held
// against it.
func TestANewVersionStartsClean(t *testing.T) {
	h, _ := learnedSkillHarness(t)
	h.loop.judgeSkillUses(map[string]skillRead{"play-radio": {hash: "old"}}, 1, "ok")
	h.loop.judgeSkillUses(map[string]skillRead{"play-radio": {hash: "new"}}, 1, "ok")
	if !catalogHas(h, "play-radio") {
		t.Fatal("a correction of the old version was held against the new one")
	}
}

// A skill somebody wrote is never quarantined, however its uses went.
func TestAHandWrittenSkillIsNeverQuarantined(t *testing.T) {
	h := newHarness(t)
	writeSkillDir(t, skillsRoot(h), "mine", "---\nname: mine\ndescription: by hand\n---\n\nbody\n")
	read := map[string]skillRead{"mine": {hash: "h"}}
	h.loop.judgeSkillUses(read, 1, "ok")
	h.loop.judgeSkillUses(read, 1, "ok")
	if !catalogHas(h, "mine") {
		t.Fatal("a hand-written skill left the catalog")
	}
}

func TestNoteSkillReadsSkipsWhatIsNotALearnedSkillRead(t *testing.T) {
	h, path := learnedSkillHarness(t)
	other := filepath.Join(h.loop.cfg.Agent.Workspace, "USER.md")
	reads := map[string]skillRead{}
	calls := []provider.ToolCall{
		{Name: "read_file", Args: map[string]any{"path": path}},
		{Name: "read_file", Args: map[string]any{"path": path}},
		{Name: "read_file", Args: map[string]any{"path": other}},
		{Name: "exec", Args: map[string]any{"path": path}},
		{Name: "read_file", Args: map[string]any{}},
	}
	outcomes := []toolOutcome{{content: "ERROR: denied"}, {content: "body"}, {content: "body"}, {content: "body"}, {content: "body"}}
	h.loop.noteSkillReads(nil, reads, calls, outcomes, 3)
	if len(reads) != 1 || reads["play-radio"].steeredAt != 3 {
		t.Errorf("reads = %+v", reads)
	}
	// A relative path resolves against the workspace, as read_file does.
	rel := map[string]skillRead{}
	h.loop.noteSkillReads(nil, rel, []provider.ToolCall{{Name: "read_file", Args: map[string]any{"path": "skills/play-radio/SKILL.md"}}},
		[]toolOutcome{{content: "body"}}, 0)
	if _, ok := rel["play-radio"]; !ok {
		t.Errorf("a relative path was not recognised: %+v", rel)
	}
}

func TestAnUnreadableUseRecordStartsOver(t *testing.T) {
	newHarness(t)
	if err := os.WriteFile(skillUsePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadSkillUses(); len(got) != 0 {
		t.Errorf("uses = %+v", got)
	}
}

// A skill corrected under one version and replaced before the second
// correction landed is not quarantined for its predecessor.
func TestARewrittenSkillIsNotQuarantinedForItsPredecessor(t *testing.T) {
	h, path := learnedSkillHarness(t)
	_, old, _ := skills.LearnedFile(skillsRoot(h), path)
	read := map[string]skillRead{"play-radio": {hash: old}}
	h.loop.judgeSkillUses(read, 1, "ok")
	if _, _, err := skills.WriteLearned(skillsRoot(h), "play-radio", "play a radio station", "1. call media\n2. say what is on"); err != nil {
		t.Fatal(err)
	}
	h.loop.judgeSkillUses(read, 1, "ok")
	if !catalogHas(h, "play-radio") {
		t.Fatal("the new version was quarantined for the old one's corrections")
	}
}

// A workspace configured as a relative path still recognises its skills.
func TestSkillReadsUnderARelativeWorkspace(t *testing.T) {
	h, _ := learnedSkillHarness(t)
	ws := h.loop.cfg.Agent.Workspace
	t.Chdir(filepath.Dir(ws))
	h.loop.cfg.Agent.Workspace = filepath.Base(ws)
	reads := map[string]skillRead{}
	h.loop.noteSkillReads(nil, reads, []provider.ToolCall{{Name: "read_file", Args: map[string]any{"path": "skills/play-radio/SKILL.md"}}},
		[]toolOutcome{{content: "body"}}, 0)
	if _, ok := reads["play-radio"]; !ok {
		t.Errorf("a relative workspace hid the read: %+v", reads)
	}
}
