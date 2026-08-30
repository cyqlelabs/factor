package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/cost"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

func learnReply(name string) func(*provider.Request) (*provider.Response, error) {
	return final("LEARN\nname: " + name + "\ndescription: when testing the probe\n---\n1. call probe\n2. read the result")
}

// probeTurn builds a persisted-shape turn tail: the opening user message,
// n assistant tool-call iterations with their results, and a final answer.
func probeTurn(n int) ([]provider.Message, int) {
	msgs := []provider.Message{{Role: "user", Content: "do the thing"}}
	for i := 0; i < n; i++ {
		msgs = append(msgs,
			provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t", Name: "probe", Args: map[string]any{"value": "x"}}}},
			provider.Message{Role: "tool", ToolCallID: "t", Content: "probe saw x"},
		)
	}
	msgs = append(msgs, provider.Message{Role: "assistant", Content: "done"})
	return msgs, len(msgs)
}

func skillsRoot(h *harness) string {
	return filepath.Join(h.loop.cfg.Agent.Workspace, "skills")
}

// ageSession backdates the session file so LastActivity reads as idle.
func ageSession(t *testing.T, h *harness, fileStem string) {
	t.Helper()
	past := time.Now().Add(-induceIdleAfter - time.Minute)
	path := filepath.Join(h.loop.cfg.Agent.Workspace, "sessions", fileStem+".jsonl")
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}

func TestParseInduction(t *testing.T) {
	cases := []struct {
		in    string
		learn bool
		name  string
		fails bool
	}{
		{in: "SKIP", learn: false},
		{in: "  SKIP — a one-off errand\n", learn: false},
		{in: "LEARN\nname: probe-routine\ndescription: run the probe\n---\n1. a\n2. b", learn: true, name: "probe-routine"},
		{in: "```\nLEARN\nname: fenced\ndescription: d\n---\nbody\n```", learn: true, name: "fenced"},
		{in: "LEARN\nname: no-separator\ndescription: d\nbody", fails: true},
		{in: "LEARN\ndescription: missing name\n---\nbody", fails: true},
		{in: "LEARN\nname: empty-body\ndescription: d\n---\n   ", fails: true},
		{in: "sure, here is a skill", fails: true},
	}
	for _, c := range cases {
		name, desc, body, learn, err := parseInduction(c.in)
		if c.fails {
			if err == nil {
				t.Errorf("%q: expected error, got learn=%v name=%q", c.in, learn, name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if learn != c.learn || name != c.name {
			t.Errorf("%q: learn=%v name=%q", c.in, learn, name)
		}
		if learn && (desc == "" || body == "") {
			t.Errorf("%q: empty desc or body", c.in)
		}
	}
}

func TestRenderTurnClipsAndBounds(t *testing.T) {
	turn := []provider.Message{
		{Role: "user", Content: strings.Repeat("u", induceMaxTextChars+100)},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "probe", Args: map[string]any{"value": "x"}}}},
		{Role: "tool", Content: strings.Repeat("r", induceMaxResultChars+100)},
		{Role: "assistant", Content: "done"},
	}
	out := renderTurn(turn)
	if !strings.Contains(out, "tool call: probe(value=x)") {
		t.Fatalf("tool call missing:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("r", induceMaxResultChars+1)) {
		t.Fatal("tool result not clipped")
	}
	if !strings.Contains(out, "…") {
		t.Fatal("no clip marker")
	}

	var huge []provider.Message
	for i := 0; i < 200; i++ {
		huge = append(huge, provider.Message{Role: "tool", Content: strings.Repeat("x", induceMaxResultChars)})
	}
	bounded := renderTurn(huge)
	if !strings.Contains(bounded, "(rest of the turn omitted)") {
		t.Fatal("oversized turn not bounded")
	}
	if len(bounded) > induceMaxTranscript+induceMaxResultChars+100 {
		t.Fatalf("transcript too large: %d", len(bounded))
	}
}

func TestNoteInduceCandidateFilters(t *testing.T) {
	h := newHarness(t)
	in := turnInput{sessionKey: "cli:x", toolCtx: tools.ToolContext{SessionKey: "cli:x"}}
	msgs, n := probeTurn(induceMinToolCalls)

	pending := func() int {
		h.loop.induceMu.Lock()
		defer h.loop.induceMu.Unlock()
		return len(h.loop.pendingInduce)
	}

	few, fewN := probeTurn(induceMinToolCalls - 1)
	h.loop.noteInduceCandidate(in, few, fewN, "done")
	if pending() != 0 {
		t.Fatal("registered a turn below the tool-call floor")
	}
	h.loop.noteInduceCandidate(turnInput{sessionKey: "cli:x", ephemeral: true}, msgs, n, "done")
	if pending() != 0 {
		t.Fatal("registered an ephemeral turn")
	}
	h.loop.noteInduceCandidate(in, msgs, n, "   ")
	if pending() != 0 {
		t.Fatal("registered a turn with no reply")
	}
	h.loop.noteInduceCandidate(in, msgs, 0, "done")
	if pending() != 0 {
		t.Fatal("registered a turn with nothing recorded")
	}
	h.loop.cfg.Agent.LearnSkills = false
	h.loop.noteInduceCandidate(in, msgs, n, "done")
	if pending() != 0 {
		t.Fatal("registered with learning disabled")
	}
	h.loop.cfg.Agent.LearnSkills = true

	h.loop.noteInduceCandidate(in, msgs, n, "done")
	if pending() != 1 {
		t.Fatal("qualifying turn not registered")
	}
	h.loop.induceMu.Lock()
	cand := h.loop.pendingInduce["cli:x"]
	h.loop.induceMu.Unlock()
	if cand.task != "do the thing" || !strings.Contains(cand.transcript, "tool call: probe") {
		t.Fatalf("candidate = %+v", cand)
	}
}

func TestInduceWritesSkillAndPromptCarriesCase(t *testing.T) {
	h := newHarness(t, learnReply("probe-routine"))
	root := skillsRoot(h)
	writeSkillDir(t, root, "manual", "---\nname: manual\ndescription: installed by hand\n---\n\nbody\n")
	cand := induceCandidate{
		toolCtx:    tools.ToolContext{SessionKey: "cli:x"},
		task:       "check the probe",
		transcript: "user: check the probe\ntool call: probe(value=1)\nresult: ok\n",
	}
	if err := h.loop.induce(context.Background(), "cli:x", cand); err != nil {
		t.Fatal(err)
	}

	learned := skills.Learned(root)
	if len(learned) != 1 || learned[0].Name != "probe-routine" {
		t.Fatalf("Learned() = %+v", learned)
	}
	data, _ := os.ReadFile(learned[0].Path)
	if !strings.Contains(string(data), "learned: true") || !strings.Contains(string(data), "1. call probe") {
		t.Fatalf("skill content:\n%s", data)
	}

	req := h.chat.requests[0]
	if len(req.Tools) != 0 {
		t.Fatal("induction call should carry no tools")
	}
	input := req.Messages[1].Content
	for _, want := range []string{"check the probe", "tool call: probe(value=1)", "manual: installed by hand", "(none yet)"} {
		if !strings.Contains(input, want) {
			t.Fatalf("induction input missing %q:\n%s", want, input)
		}
	}
}

func TestInduceSkipWritesNothing(t *testing.T) {
	h := newHarness(t, final("SKIP"))
	if err := h.loop.induce(context.Background(), "cli:x", induceCandidate{task: "t", transcript: "tr"}); err != nil {
		t.Fatal(err)
	}
	if got := skills.Learned(skillsRoot(h)); len(got) != 0 {
		t.Fatalf("SKIP wrote a skill: %+v", got)
	}
}

func TestInduceRefusesHandWrittenName(t *testing.T) {
	h := newHarness(t, learnReply("manual"))
	root := skillsRoot(h)
	writeSkillDir(t, root, "manual", "---\nname: manual\ndescription: installed by hand\n---\n\nbody\n")
	if err := h.loop.induce(context.Background(), "cli:x", induceCandidate{task: "t", transcript: "tr"}); err == nil {
		t.Fatal("overwrote a hand-written skill")
	}
	data, _ := os.ReadFile(filepath.Join(root, "manual", "SKILL.md"))
	if !strings.Contains(string(data), "installed by hand") {
		t.Fatal("hand-written skill was modified")
	}
}

func TestInduceCapBlocksNewAllowsUpdate(t *testing.T) {
	defer func(old int) { maxLearnedSkills = old }(maxLearnedSkills)
	maxLearnedSkills = 1

	h := newHarness(t, learnReply("newbie"), learnReply("existing"))
	root := skillsRoot(h)
	if _, _, err := skills.WriteLearned(root, "existing", "already here", "old body"); err != nil {
		t.Fatal(err)
	}
	if err := h.loop.induce(context.Background(), "cli:x", induceCandidate{task: "t", transcript: "tr"}); err == nil {
		t.Fatal("cap did not block a new skill")
	}
	if err := h.loop.induce(context.Background(), "cli:x", induceCandidate{task: "t", transcript: "tr"}); err != nil {
		t.Fatalf("cap blocked an update: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "existing", "SKILL.md"))
	if !strings.Contains(string(data), "1. call probe") {
		t.Fatal("update did not land")
	}
	if !strings.Contains(h.chat.requests[0].Messages[1].Content, "library is full") {
		t.Fatal("cap not stated in the prompt")
	}
}

func TestInduceDropsTruncatedAndBudgetStopped(t *testing.T) {
	h := newHarness(t,
		func(*provider.Request) (*provider.Response, error) {
			return &provider.Response{Content: "LEARN\nname: cut\ndescription: d\n---\nbody", FinishReason: "length"}, nil
		},
		func(*provider.Request) (*provider.Response, error) {
			return nil, &cost.BudgetError{Scope: "session", Spent: 1, Limit: 1}
		},
	)
	if err := h.loop.induce(context.Background(), "cli:x", induceCandidate{task: "t", transcript: "tr"}); err == nil {
		t.Fatal("saved a truncated skill")
	}
	if err := h.loop.induce(context.Background(), "cli:x", induceCandidate{task: "t", transcript: "tr"}); err != nil {
		t.Fatalf("a budget stop is a decision, not a failure: %v", err)
	}
	if got := skills.Learned(skillsRoot(h)); len(got) != 0 {
		t.Fatalf("wrote a skill anyway: %+v", got)
	}
}

func TestDueInductionsRespectsIdleAndActive(t *testing.T) {
	h := newHarness(t, final("ok"))
	if _, err := h.loop.ProcessDirect(context.Background(), "hi", "cli:test"); err != nil {
		t.Fatal(err)
	}
	register := func() {
		h.loop.induceMu.Lock()
		h.loop.pendingInduce["cli:test"] = induceCandidate{task: "t", transcript: "tr"}
		h.loop.induceMu.Unlock()
	}

	register()
	if due := h.loop.dueInductions(); len(due) != 0 {
		t.Fatal("a session active a moment ago is not idle")
	}

	ageSession(t, h, "cli_test")
	h.loop.mu.Lock()
	h.loop.active["cli:test"] = &turn{}
	h.loop.mu.Unlock()
	if due := h.loop.dueInductions(); len(due) != 0 {
		t.Fatal("a session mid-turn was claimed")
	}
	h.loop.mu.Lock()
	delete(h.loop.active, "cli:test")
	h.loop.mu.Unlock()

	due := h.loop.dueInductions()
	if len(due) != 1 {
		t.Fatalf("idle candidate not claimed: %v", due)
	}
	h.loop.induceMu.Lock()
	left := len(h.loop.pendingInduce)
	h.loop.induceMu.Unlock()
	if left != 0 {
		t.Fatal("claimed candidate still pending")
	}

	// A candidate whose session file is gone can never come due; it must be
	// dropped rather than held forever.
	h.loop.induceMu.Lock()
	h.loop.pendingInduce["cli:vanished"] = induceCandidate{task: "t", transcript: "tr"}
	h.loop.induceMu.Unlock()
	if due := h.loop.dueInductions(); len(due) != 0 {
		t.Fatal("claimed a candidate with no session behind it")
	}
	h.loop.induceMu.Lock()
	_, still := h.loop.pendingInduce["cli:vanished"]
	h.loop.induceMu.Unlock()
	if still {
		t.Fatal("orphaned candidate not dropped")
	}
}

func TestTurnRegistersAndSweepLearns(t *testing.T) {
	script := []func(*provider.Request) (*provider.Response, error){}
	for i := 0; i < induceMinToolCalls; i++ {
		script = append(script, toolCall("probe", map[string]any{"value": "x"}))
	}
	script = append(script, final("all done"), learnReply("probe-routine"))
	h := newHarness(t, script...)

	if _, err := h.loop.ProcessDirect(context.Background(), "run the probes", "cli:test"); err != nil {
		t.Fatal(err)
	}
	h.loop.induceMu.Lock()
	cand, ok := h.loop.pendingInduce["cli:test"]
	h.loop.induceMu.Unlock()
	if !ok || !strings.Contains(cand.transcript, "tool call: probe") {
		t.Fatalf("turn did not register a candidate: %+v", cand)
	}

	ageSession(t, h, "cli_test")
	h.loop.induceIdleSessions(context.Background())
	h.loop.WaitBackground(10 * time.Second)

	root := skillsRoot(h)
	if got := skills.Learned(root); len(got) != 1 || got[0].Name != "probe-routine" {
		t.Fatalf("sweep did not learn: %+v", got)
	}
	// The catalog the next turn reads lists it like any other skill.
	list := skills.NewLoader(root).List()
	found := false
	for _, s := range list {
		found = found || s.Name == "probe-routine"
	}
	if !found {
		t.Fatalf("learned skill missing from catalog: %+v", list)
	}
}

func writeSkillDir(t *testing.T, root, dir, content string) {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
