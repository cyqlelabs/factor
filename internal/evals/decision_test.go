package evals

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/provider"
)

// judge plays the typed-decision model: every question gets the same choice
// at the same confidence, and what it was asked is kept for the checks.
type judge struct {
	mu         sync.Mutex
	choice     string
	confidence float64
	asked      []*decision.Request
}

func (j *judge) Decide(_ context.Context, req *decision.Request) (*decision.Response, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.asked = append(j.asked, req)
	answers := map[string]decision.Answer{}
	for name, q := range req.Questions {
		ids := decision.Candidates(q)
		probs := map[string]float64{}
		for _, id := range ids {
			probs[id] = 0.05 / float64(max(len(ids)-1, 1))
		}
		probs[j.choice] = 0.95
		answers[name] = decision.Answer{Choice: j.choice, Confidence: j.confidence, Probabilities: probs}
	}
	return &decision.Response{Answers: answers, Model: "jev-eval"}, nil
}

func (j *judge) Name() string { return "judge" }

func (j *judge) questions() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.asked)
}

func (e *env) decide(j *judge, mode string) {
	e.loop.WithDecider(decision.New(j, decision.Options{Mode: mode}),
		agent.Policies{Verify: true, Recover: true, Induce: true})
}

func readFile(path string) *provider.Response {
	return &provider.Response{ToolCalls: []provider.ToolCall{{ID: "r1", Name: "read_file", Args: map[string]any{"path": path}}}}
}

// A reply that reports work its tool results do not show is held once, on a
// user message that names itself as the system speaking, and the model's
// corrected answer is the one the user reads. The check is asked with the
// task, the trajectory and the reply, and the trajectory is the calls, not
// the pages.
func TestOverclaimedReplyIsHeldWithTheEvidence(t *testing.T) {
	var held provider.Message
	e := newEnv(t, func(step int, req *provider.Request) *provider.Response {
		switch step {
		case 0:
			return readFile("missing.txt")
		case 1:
			return &provider.Response{Content: "I read the file and emailed you its contents."}
		}
		held = req.Messages[len(req.Messages)-1]
		return &provider.Response{Content: "Correction: the file does not exist and nothing was emailed."}
	})
	j := &judge{choice: "overclaimed", confidence: 0.9}
	e.decide(j, decision.ModeActive)

	reply, err := e.say("cli:hold", "email me the file")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Correction:") {
		t.Fatalf("reply = %q", reply)
	}
	if held.Role != "user" || !strings.Contains(held.Content, "[Verification from the system") {
		t.Errorf("the hold did not arrive as a system-framed user message: %+v", held)
	}
	if j.questions() != 1 {
		t.Fatalf("%d decisions asked, want one completion check", j.questions())
	}
	// The state is read back the way the model reads it: as the JSON it
	// rides the request in.
	var state map[string]any
	if raw, err := json.Marshal(j.asked[0].State); err != nil || json.Unmarshal(raw, &state) != nil {
		t.Fatalf("completion state does not round-trip: %v", err)
	}
	if state["task"] != "email me the file" || !strings.Contains(state["reply"].(string), "emailed") {
		t.Errorf("the check was not asked with the task and the reply: %+v", state)
	}
	if traj := state["trajectory"].(string); !strings.Contains(traj, "tool call: read_file(path=missing.txt)") || !strings.Contains(traj, "result: ERROR") {
		t.Errorf("the trajectory does not carry the call and its failure:\n%s", traj)
	}
	if _, ok := j.asked[0].Questions[decision.KindCompletion].Criteria["overclaimed"]; !ok {
		t.Error("the completion question does not offer the overclaimed verdict")
	}
}

// In shadow mode the same reply stands, and the question was still asked:
// that record is what calibrates the bar before anything acts on it.
func TestShadowModeRecordsWithoutHolding(t *testing.T) {
	e := newEnv(t, func(step int, _ *provider.Request) *provider.Response {
		if step == 0 {
			return readFile("missing.txt")
		}
		return &provider.Response{Content: "All done."}
	})
	j := &judge{choice: "overclaimed", confidence: 0.9}
	e.decide(j, decision.ModeShadow)
	reply, err := e.say("cli:shadow", "do it")
	if err != nil || reply != "All done." {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if j.questions() != 1 || len(e.chat.seen()) != 2 {
		t.Errorf("questions=%d model requests=%d", j.questions(), len(e.chat.seen()))
	}
}

// The same call returning the same result three times is a stall, said on a
// system-framed message after the batch, with the decided kind of recovery.
func TestStalledCallIsNamedAndTheRecoveryDecided(t *testing.T) {
	var nudge string
	e := newEnv(t, func(step int, req *provider.Request) *provider.Response {
		if step < 3 {
			return readFile("nowhere.txt")
		}
		for _, m := range req.Messages {
			if m.Role == "user" && strings.Contains(m.Content, "[Note from the system") {
				nudge = m.Content
			}
		}
		return &provider.Response{Content: "The file is not there; I will ask where it is."}
	})
	j := &judge{choice: "escalate_to_user", confidence: 0.9}
	e.decide(j, decision.ModeActive)
	if _, err := e.say("cli:stall", "read nowhere.txt"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nudge, "read_file(path=nowhere.txt) has now returned the same result 3 times") ||
		!strings.Contains(nudge, "what you need from the user") {
		t.Errorf("nudge = %q", nudge)
	}
	// The stall was decided, and then the reply was checked: two questions.
	if j.questions() != 2 {
		t.Errorf("%d decisions, want recovery + completion", j.questions())
	}
	if _, ok := j.asked[0].Questions[decision.KindRecovery]; !ok {
		t.Error("the first question was not the recovery")
	}
}

// Without a decider the loop still notices the stall and says the general
// thing — the deterministic half runs on every install.
func TestStallIsNudgedWithNoDeciderAtAll(t *testing.T) {
	var nudge string
	e := newEnv(t, func(step int, req *provider.Request) *provider.Response {
		if step < 3 {
			return readFile("nowhere.txt")
		}
		for _, m := range req.Messages {
			if m.Role == "user" && strings.Contains(m.Content, "[Note from the system") {
				nudge = m.Content
			}
		}
		return &provider.Response{Content: "ok"}
	})
	if _, err := e.say("cli:plain", "read nowhere.txt"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nudge, "Change approach") {
		t.Errorf("nudge = %q", nudge)
	}
}

// The prefix stays byte-identical with the policies on: a hold and a nudge
// land after the cache marks, in the turn's own tail, never in the head.
func TestPoliciesDoNotMoveTheSystemPrompt(t *testing.T) {
	e := newEnv(t, func(step int, _ *provider.Request) *provider.Response {
		if step%2 == 0 {
			return readFile("x.txt")
		}
		return &provider.Response{Content: "done"}
	})
	e.decide(&judge{choice: "verified", confidence: 0.9}, decision.ModeActive)
	if _, err := e.say("cli:p", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.say("cli:p", "two"); err != nil {
		t.Fatal(err)
	}
	reqs := e.chat.seen()
	first := systemText(reqs[0])
	for i, r := range reqs[1:] {
		if systemText(r) != first {
			t.Errorf("request %d changed the system text", i+1)
		}
	}
}
