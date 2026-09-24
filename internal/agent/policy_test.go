package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

// fakeBackend plays the decision model: it answers every question with the
// same choice at the same confidence, or fails, and remembers what it saw.
type fakeBackend struct {
	mu         sync.Mutex
	choice     string
	confidence float64
	err        error
	requests   []*decision.Request
}

func (f *fakeBackend) Decide(_ context.Context, req *decision.Request) (*decision.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	answers := map[string]decision.Answer{}
	for name, q := range req.Questions {
		ids := decision.Candidates(q)
		probs := map[string]float64{}
		for _, id := range ids {
			probs[id] = 0.05 / float64(max(len(ids)-1, 1))
		}
		probs[f.choice] = 0.95
		answers[name] = decision.Answer{Choice: f.choice, Confidence: f.confidence, Probabilities: probs}
	}
	return &decision.Response{Answers: answers, Model: "jev-fake", Usage: decision.Usage{InputTokens: 30}}, nil
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeBackend) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		for name := range r.Questions {
			out = append(out, name)
		}
	}
	return out
}

func withDecider(h *harness, b *fakeBackend, mode string, p Policies) *fakeBackend {
	h.loop.WithDecider(decision.New(b, decision.Options{Mode: mode}), p)
	return b
}

var allPolicies = Policies{Verify: true, Recover: true, Induce: true}

func TestStallTrackerCountsIdenticalCallAndResultPairs(t *testing.T) {
	s := newStallTracker()
	call := provider.ToolCall{Name: "probe", Args: map[string]any{"value": "x"}}
	other := provider.ToolCall{Name: "probe", Args: map[string]any{"value": "y"}}
	first := s.note(call, "same")
	second := s.note(call, "same")
	if first || second {
		t.Fatal("two identical calls are not yet a stall")
	}
	if s.note(other, "same") {
		t.Fatal("a different call does not count toward another's stall")
	}
	if !s.note(call, "same") {
		t.Fatal("the third identical call is the stall")
	}
	if s.note(call, "same") {
		t.Fatal("a stall is reported once, not on every repeat")
	}
	// A result that changes is progress and resets the count.
	s2 := newStallTracker()
	s2.note(call, "a")
	s2.note(call, "b")
	if s2.note(call, "b") || !s2.note(call, "b") {
		t.Fatal("the count must restart when the result changes")
	}
}

// The deterministic half: a model repeating one call three times with the
// same result is told so on a system-framed message, decider or none.
func TestRepeatedIdenticalCallIsNudgedWithoutADecider(t *testing.T) {
	repeat := toolCall("probe", map[string]any{"value": "loop"})
	var nudge string
	h := newHarness(t, repeat, repeat, repeat,
		func(req *provider.Request) (*provider.Response, error) {
			for _, m := range req.Messages {
				if m.Role == "user" && strings.Contains(m.Content, "returned the same result") {
					nudge = m.Content
				}
			}
			return &provider.Response{Content: "changing tack"}, nil
		})
	reply, err := h.loop.ProcessDirect(context.Background(), "loop", "cli:s")
	if err != nil || reply != "changing tack" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if !strings.Contains(nudge, "[Note from the system") || !strings.Contains(nudge, "probe(value=loop)") ||
		!strings.Contains(nudge, "3 times") || !strings.Contains(nudge, "Change approach") {
		t.Errorf("nudge = %q", nudge)
	}
	history, _ := h.store.History("cli:s")
	persisted := 0
	for _, m := range history {
		if m.Role == "user" && strings.Contains(m.Content, "returned the same result") {
			persisted++
		}
	}
	if persisted != 1 {
		t.Errorf("the nudge should be persisted exactly once, found %d", persisted)
	}
}

func TestRecoveryNudgeCarriesTheDecidedKind(t *testing.T) {
	for choice, want := range map[string]string{
		recoverWait:      "wait a moment",
		recoverRefresh:   "re-read what the call depends on",
		recoverAlternate: "another way to the same end",
		recoverEscalate:  "what you need from the user",
	} {
		repeat := toolCall("probe", map[string]any{"value": "loop"})
		var nudge string
		h := newHarness(t, repeat, repeat, repeat,
			func(req *provider.Request) (*provider.Response, error) {
				for _, m := range req.Messages {
					if m.Role == "user" && strings.Contains(m.Content, "returned the same result") {
						nudge = m.Content
					}
				}
				return &provider.Response{Content: "ok"}, nil
			})
		b := withDecider(h, &fakeBackend{choice: choice, confidence: 0.9}, decision.ModeActive, allPolicies)
		if _, err := h.loop.ProcessDirect(context.Background(), "loop", "cli:r"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(nudge, want) {
			t.Errorf("%s: nudge = %q", choice, nudge)
		}
		state := b.requests[0].State.(map[string]any)
		if state["repeated_call"] != "probe(value=loop)" || state["task"] != "loop" {
			t.Errorf("%s: recovery state = %+v", choice, state)
		}
	}
}

// Shadow, unsure and a dead backend all leave the deterministic nudge as it
// was: the decision is recorded, or not had, and nothing changes.
func TestRecoveryFallsBackToTheGenericNudge(t *testing.T) {
	cases := map[string]struct {
		backend *fakeBackend
		mode    string
	}{
		"shadow":  {&fakeBackend{choice: recoverWait, confidence: 0.9}, decision.ModeShadow},
		"unsure":  {&fakeBackend{choice: recoverWait, confidence: 0.2}, decision.ModeActive},
		"down":    {&fakeBackend{err: errors.New("refused")}, decision.ModeActive},
		"invalid": {&fakeBackend{choice: "not_a_choice", confidence: 0.9}, decision.ModeActive},
	}
	for name, c := range cases {
		repeat := toolCall("probe", map[string]any{"value": "loop"})
		var nudge string
		h := newHarness(t, repeat, repeat, repeat,
			func(req *provider.Request) (*provider.Response, error) {
				for _, m := range req.Messages {
					if m.Role == "user" && strings.Contains(m.Content, "returned the same result") {
						nudge = m.Content
					}
				}
				return &provider.Response{Content: "ok"}, nil
			})
		withDecider(h, c.backend, c.mode, allPolicies)
		if _, err := h.loop.ProcessDirect(context.Background(), "loop", "cli:f"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(nudge, "Change approach: another tool") || strings.Contains(nudge, "wait a moment") {
			t.Errorf("%s: nudge = %q", name, nudge)
		}
		recoveries := 0
		for _, k := range c.backend.kinds() {
			if k == decision.KindRecovery {
				recoveries++
			}
		}
		if recoveries != 1 {
			t.Errorf("%s: %d recovery decisions, want 1", name, recoveries)
		}
	}
	// Recovery switched off asks nothing.
	repeat := toolCall("probe", map[string]any{"value": "loop"})
	h := newHarness(t, repeat, repeat, repeat, final("ok"))
	b := withDecider(h, &fakeBackend{choice: recoverWait, confidence: 0.9}, decision.ModeActive, Policies{Verify: true})
	if _, err := h.loop.ProcessDirect(context.Background(), "loop", "cli:off"); err != nil {
		t.Fatal(err)
	}
	for _, k := range b.kinds() {
		if k == decision.KindRecovery {
			t.Error("recovery asked with the policy off")
		}
	}
}

// The completion check: a reply after tool use that overclaims is handed
// back once with the finding, and the corrected answer is what the user
// gets. A reply the check accepts stands.
func TestOverclaimedReplyIsHeldOnceAndCorrected(t *testing.T) {
	var held string
	h := newHarness(t,
		toolCall("probe", map[string]any{"value": "x"}),
		final("I sent the report and it was delivered."),
		func(req *provider.Request) (*provider.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if last.Role == "user" && strings.Contains(last.Content, "[Verification from the system") {
				held = last.Content
			}
			return &provider.Response{Content: "Correction: the probe ran, but nothing was sent."}, nil
		})
	b := withDecider(h, &fakeBackend{choice: completionOverclaimed, confidence: 0.9}, decision.ModeActive, allPolicies)
	reply, err := h.loop.ProcessDirect(context.Background(), "send the report", "cli:v")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Correction:") {
		t.Errorf("reply = %q, want the corrected answer", reply)
	}
	if !strings.Contains(held, "tool results do not show") || !strings.Contains(held, "not a message from the user") {
		t.Errorf("held = %q", held)
	}
	if b.calls() != 1 {
		t.Errorf("%d completion checks, want exactly one per turn", b.calls())
	}
	state := b.requests[0].State.(completionState)
	if state.Task != "send the report" || !strings.Contains(state.Reply, "delivered") ||
		!strings.Contains(state.Trajectory, "tool call: probe") {
		t.Errorf("completion state = %+v", state)
	}
	history, _ := h.store.History("cli:v")
	// user, assistant(call), tool, assistant(overclaim), user(verification), assistant(correction)
	if len(history) != 6 || history[4].Role != "user" || history[5].Role != "assistant" {
		t.Errorf("history shape = %d messages", len(history))
	}
}

func TestVerifiedReplyStandsAndOnlyToolTurnsAreChecked(t *testing.T) {
	h := newHarness(t, toolCall("probe", map[string]any{"value": "x"}), final("The probe saw x."))
	b := withDecider(h, &fakeBackend{choice: completionVerified, confidence: 0.9}, decision.ModeActive, allPolicies)
	reply, err := h.loop.ProcessDirect(context.Background(), "probe x", "cli:ok")
	if err != nil || reply != "The probe saw x." {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if b.calls() != 1 {
		t.Errorf("checks = %d", b.calls())
	}
	// No tool, no claim a trajectory could contradict, no check.
	h2 := newHarness(t, final("hello"))
	b2 := withDecider(h2, &fakeBackend{choice: completionOverclaimed, confidence: 0.9}, decision.ModeActive, allPolicies)
	if reply, err := h2.loop.ProcessDirect(context.Background(), "hi", "cli:plain"); err != nil || reply != "hello" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if b2.calls() != 0 {
		t.Error("a turn with no tools was checked")
	}
	// A heartbeat is never held: nobody is there to read the correction.
	h3 := newHarness(t, toolCall("probe", map[string]any{"value": "x"}), final("all fine"))
	b3 := withDecider(h3, &fakeBackend{choice: completionOverclaimed, confidence: 0.9}, decision.ModeActive, allPolicies)
	if reply, err := h3.loop.ProcessEphemeral(context.Background(), "check"); err != nil || reply != "all fine" {
		t.Fatalf("ephemeral reply=%q err=%v", reply, err)
	}
	if b3.calls() != 0 {
		t.Error("an ephemeral turn was checked")
	}
}

func TestCompletionCheckFallsBackQuietly(t *testing.T) {
	cases := map[string]struct {
		backend *fakeBackend
		mode    string
		policy  Policies
		asked   int
	}{
		"shadow":       {&fakeBackend{choice: completionOverclaimed, confidence: 0.9}, decision.ModeShadow, allPolicies, 1},
		"unsure":       {&fakeBackend{choice: completionOverclaimed, confidence: 0.3}, decision.ModeActive, allPolicies, 1},
		"down":         {&fakeBackend{err: errors.New("timeout")}, decision.ModeActive, allPolicies, 1},
		"insufficient": {&fakeBackend{choice: completionInsufficient, confidence: 0.9}, decision.ModeActive, allPolicies, 1},
		"policy off":   {&fakeBackend{choice: completionOverclaimed, confidence: 0.9}, decision.ModeActive, Policies{Recover: true}, 0},
	}
	for name, c := range cases {
		h := newHarness(t, toolCall("probe", map[string]any{"value": "x"}), final("done and dusted"))
		withDecider(h, c.backend, c.mode, c.policy)
		reply, err := h.loop.ProcessDirect(context.Background(), "do it", "cli:q")
		if err != nil || reply != "done and dusted" {
			t.Errorf("%s: reply=%q err=%v", name, reply, err)
		}
		if c.backend.calls() != c.asked {
			t.Errorf("%s: %d checks, want %d", name, c.backend.calls(), c.asked)
		}
		if len(h.chat.requests) != 2 {
			t.Errorf("%s: %d model requests, want 2 (no re-ask)", name, len(h.chat.requests))
		}
	}
}

// The state a completion check sends fits the model's window, keeps the tail
// of the trajectory, and puts it ahead of the reply: a state the model cuts
// at its head must lose the reply last.
func TestCompletionStateFitsTheModelWindow(t *testing.T) {
	limits := decision.Limits{MaxStateChars: 600}
	trajectory := strings.Repeat("call ", 400) + "LAST RESULT"
	s := newCompletionState(limits, strings.Repeat("task ", 100), trajectory, strings.Repeat("reply ", 100))
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > limits.MaxStateChars {
		t.Errorf("state is %d chars, window is %d", len(b), limits.MaxStateChars)
	}
	if !strings.HasSuffix(s.Trajectory, "LAST RESULT") || len(s.Trajectory) < 200 {
		t.Errorf("trajectory tail lost or starved: %d chars ending %q", len(s.Trajectory), s.Trajectory[max(0, len(s.Trajectory)-20):])
	}
	body := string(b)
	if strings.Index(body, `"trajectory"`) > strings.Index(body, `"reply"`) {
		t.Error("reply rides ahead of the trajectory")
	}

	unbounded := newCompletionState(decision.Limits{}, "t", trajectory, "r")
	if unbounded.Trajectory != trajectory {
		t.Error("an unknown window clipped a trajectory under the caller's own budget")
	}
}

// A model that overclaims again after being held is not held a second time:
// the check is one re-ask per turn, not a loop.
func TestOverclaimIsHeldAtMostOncePerTurn(t *testing.T) {
	h := newHarness(t, toolCall("probe", map[string]any{"value": "x"}), final("sent!"), final("sent, really!"))
	b := withDecider(h, &fakeBackend{choice: completionOverclaimed, confidence: 0.9}, decision.ModeActive, allPolicies)
	reply, err := h.loop.ProcessDirect(context.Background(), "send", "cli:twice")
	if err != nil || reply != "sent, really!" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if b.calls() != 1 || len(h.chat.requests) != 3 {
		t.Errorf("checks=%d requests=%d", b.calls(), len(h.chat.requests))
	}
}

// Induction screening: SKIP spares the utility call, CREATE and UPDATE pay
// it with a hint, CREATE against a full library is a SKIP, and every
// fallback leaves the call exactly as it was.
func TestInductionScreeningGatesTheUtilityCall(t *testing.T) {
	cand := induceCandidate{toolCtx: tools.ToolContext{SessionKey: "cli:x"}, task: "check the probe",
		transcript: "user: check the probe\ntool call: probe(value=1)\nresult: ok\n"}

	skip := newHarness(t, learnReply("probe-routine"))
	withDecider(skip, &fakeBackend{choice: inductSkip, confidence: 0.9}, decision.ModeActive, allPolicies)
	if err := skip.loop.induce(context.Background(), "cli:x", cand); err != nil {
		t.Fatal(err)
	}
	if len(skip.chat.requests) != 0 {
		t.Error("SKIP still paid the utility call")
	}
	if len(skills.Learned(skillsRoot(skip))) != 0 {
		t.Error("SKIP wrote a skill")
	}

	for choice, hint := range map[string]string{inductCreate: "worth a new skill", inductUpdate: "refinement of one of the learned skills"} {
		h := newHarness(t, learnReply("probe-routine"))
		withDecider(h, &fakeBackend{choice: choice, confidence: 0.9}, decision.ModeActive, allPolicies)
		if err := h.loop.induce(context.Background(), "cli:x", cand); err != nil {
			t.Fatal(err)
		}
		if len(h.chat.requests) != 1 || !strings.Contains(h.chat.requests[0].Messages[1].Content, hint) {
			t.Errorf("%s: utility call not made with the hint", choice)
		}
		if len(skills.Learned(skillsRoot(h))) != 1 {
			t.Errorf("%s: skill not written", choice)
		}
	}

	full := newHarness(t, learnReply("probe-routine"))
	old := maxLearnedSkills
	maxLearnedSkills = 0
	t.Cleanup(func() { maxLearnedSkills = old })
	withDecider(full, &fakeBackend{choice: inductCreate, confidence: 0.9}, decision.ModeActive, allPolicies)
	if err := full.loop.induce(context.Background(), "cli:x", cand); err != nil {
		t.Fatal(err)
	}
	if len(full.chat.requests) != 0 {
		t.Error("CREATE against a full library still paid the utility call")
	}
	maxLearnedSkills = old

	for name, c := range map[string]struct {
		backend *fakeBackend
		mode    string
		policy  Policies
	}{
		"shadow":     {&fakeBackend{choice: inductSkip, confidence: 0.9}, decision.ModeShadow, allPolicies},
		"unsure":     {&fakeBackend{choice: inductSkip, confidence: 0.4}, decision.ModeActive, allPolicies},
		"down":       {&fakeBackend{err: errors.New("no")}, decision.ModeActive, allPolicies},
		"policy off": {&fakeBackend{choice: inductSkip, confidence: 0.9}, decision.ModeActive, Policies{Verify: true, Recover: true}},
	} {
		h := newHarness(t, learnReply("probe-routine"))
		withDecider(h, c.backend, c.mode, c.policy)
		if err := h.loop.induce(context.Background(), "cli:x", cand); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(h.chat.requests) != 1 || strings.Contains(h.chat.requests[0].Messages[1].Content, "A screening judged") {
			t.Errorf("%s: the utility call should run unhinted", name)
		}
	}
}

// The interrupt seam: a tool running under a turn can tell whether steering
// has arrived, and a context without one is never interrupted.
func TestInterruptSeamReadsTheSteeringQueue(t *testing.T) {
	if tools.Interrupted(context.Background()) {
		t.Fatal("a bare context reads as interrupted")
	}
	if tools.WithInterrupt(context.Background(), nil) != context.Background() {
		t.Fatal("a nil check should leave the context alone")
	}
	var seen bool
	probeCtx := make(chan context.Context, 1)
	h := newHarness(t,
		toolCall("probe", map[string]any{"value": "x"}),
		func(*provider.Request) (*provider.Response, error) { return &provider.Response{Content: "done"}, nil },
	)
	// The probe records the context it was executed under.
	h.registry.Register(&ctxProbe{ch: probeCtx})
	h.chat.script[0] = toolCall("ctxprobe", map[string]any{})
	if _, err := h.loop.ProcessDirect(context.Background(), "go", "cli:i"); err != nil {
		t.Fatal(err)
	}
	select {
	case ctx := <-probeCtx:
		seen = true
		if tools.Interrupted(ctx) {
			t.Error("read as interrupted with an empty steering queue")
		}
	default:
	}
	if !seen {
		t.Fatal("the probe never ran")
	}
}

type ctxProbe struct{ ch chan context.Context }

func (p *ctxProbe) Name() string        { return "ctxprobe" }
func (p *ctxProbe) Description() string { return "records its context" }
func (p *ctxProbe) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (p *ctxProbe) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	p.ch <- ctx
	return tools.Text("ok")
}
