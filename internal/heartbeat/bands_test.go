package heartbeat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bands"
)

type spy struct {
	prompts   []string
	delivered []string
}

func (s *spy) run(_ context.Context, prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	return OKToken, nil
}

func (s *spy) deliver(content string) bool {
	s.delivered = append(s.delivered, content)
	return true
}

func serviceWith(t *testing.T, heartbeatMD string, breaches []bands.Breach) (*Service, *spy) {
	t.Helper()
	ws := t.TempDir()
	if heartbeatMD != "" {
		if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte(heartbeatMD), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &spy{}
	svc := NewService(ws, 0, s.run, s.deliver)
	if breaches != nil {
		svc = svc.WithBands(func() []bands.Breach { return breaches })
	}
	return svc, s
}

// The gate that was already there stays: nothing written down and nothing
// drifting means no model call.
func TestNothingToLookAtSpendsNothing(t *testing.T) {
	svc, s := serviceWith(t, "# notes\n\nnothing here\n", []bands.Breach{})
	svc.Tick(context.Background())
	if len(s.prompts) != 0 {
		t.Errorf("spent a call on nothing: %v", s.prompts)
	}
}

// The point of the bands: a user who never wrote a HEARTBEAT.md still wants
// to hear that their spend tripled.
func TestABreachStartsACheckWithNoHeartbeatFile(t *testing.T) {
	svc, s := serviceWith(t, "", []bands.Breach{
		{Metric: "cost per turn", Unit: "USD", Recent: 0.5, Baseline: 0.05, Sigma: 4, Samples: 9},
	})
	svc.Tick(context.Background())
	if len(s.prompts) != 1 {
		t.Fatalf("prompts = %v", s.prompts)
	}
	if !strings.Contains(s.prompts[0], "cost per turn") {
		t.Errorf("the breach did not reach the prompt:\n%s", s.prompts[0])
	}
}

// The detection is arithmetic and says only that something moved. Whether it
// matters is the judgement the model is being spent on, so the prompt must not
// hand it a conclusion.
func TestBandSectionStatesTheNumbersWithoutDiagnosing(t *testing.T) {
	got := bandSection([]bands.Breach{
		{Metric: "provider failovers", Unit: "per turn", Recent: 2, Baseline: 0.1, Sigma: 3.5, Samples: 8},
	})
	if !strings.Contains(got, "measured, not diagnosed") {
		t.Errorf("section = %q", got)
	}
	if !strings.Contains(got, "well outside the usual range") {
		t.Error("a tier-3 breach should be called out as such")
	}
	if !strings.Contains(got, "Say something only if it does") {
		t.Error("the prompt should not require the model to report every reading")
	}
}

func TestBandSectionIsEmptyWithoutBreaches(t *testing.T) {
	if got := bandSection(nil); got != "" {
		t.Errorf("section = %q", got)
	}
}

// A tier-1 reading is unusual but not yet a story; it should not be dressed
// up as one.
func TestLowTierBreachIsNotCalledOut(t *testing.T) {
	got := bandSection([]bands.Breach{{Metric: "seconds per turn", Unit: "s", Sigma: 1.2}})
	if strings.Contains(got, "well outside") {
		t.Errorf("a 1.2σ reading was reported as extreme: %q", got)
	}
}

// Tasks in HEARTBEAT.md still start a check on their own, with no bands wired.
func TestActionableFileStillWorksWithoutBands(t *testing.T) {
	svc, s := serviceWith(t, "- check the mail\n", nil)
	svc.Tick(context.Background())
	if len(s.prompts) != 1 {
		t.Fatalf("prompts = %v", s.prompts)
	}
	if strings.Contains(s.prompts[0], "Drifted metrics") {
		t.Error("no bands were configured, so none should be described")
	}
}

// toolBreach is the breach the first real one was: a search and the fast
// path failing this hour, and the fast path having failed every time it ran.
func toolBreach() bands.Breach {
	now := time.Now()
	return bands.Breach{Metric: "tool error rate", Unit: "of calls", Recent: 0.53, Baseline: 0.05, Sigma: 3.6, Samples: 3,
		Evidence: bands.Evidence{
			Since: now.Add(-7 * 24 * time.Hour),
			Failures: []bands.Failure{
				{At: now.Add(-20 * time.Minute), Session: "voice:local", Tool: "web_search", Fault: "search failed: 403 from the engine"},
				{At: now.Add(-20 * time.Minute), Session: "voice:local", Tool: "browser_fetch", Fault: "lightpanda exited"},
			},
			History: []bands.ToolRecord{
				{Tool: "browser_fetch", Calls: 4, Fails: 4, First: now.Add(-3 * 24 * time.Hour), Last: now.Add(-20 * time.Minute)},
			},
		}}
}

// A breach arrives with what stood behind it and with what to do about it:
// the calls that failed and what they said, each failing tool's record, the
// repair levers by name, and the one lever not to pull.
func TestBreachPromptCarriesEvidenceAndRepairGuidance(t *testing.T) {
	svc, s := serviceWith(t, "", []bands.Breach{toolBreach()})
	svc.Tick(context.Background())
	if len(s.prompts) != 1 {
		t.Fatalf("prompts = %v", s.prompts)
	}
	for _, want := range []string{
		"web_search failed: search failed: 403 from the engine",
		"browser_fetch has failed 4 of 4 calls since",
		"journal holds no tool outcomes",
		"config_set",
		"Never kill or restart processes with exec",
		"Do not write HEARTBEAT_OK in a reply that reports a change",
	} {
		if !strings.Contains(s.prompts[0], want) {
			t.Errorf("the prompt lacks %q:\n%s", want, s.prompts[0])
		}
	}
}

// What a check concluded is shown to the next check on the same metric, and
// only there: a shrug that repeats is the pattern this exists to make
// visible, and a verdict on spend says nothing about tool errors.
func TestVerdictsAreShownToTheNextCheckOnTheSameMetric(t *testing.T) {
	ws := t.TempDir()
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	var prompts []string
	reply := "The spike is real but transient — it should self-correct.\n\nHEARTBEAT_OK"
	run := func(_ context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return reply, nil
	}
	var delivered []string
	deliver := func(content string) bool { delivered = append(delivered, content); return true }
	breaches := []bands.Breach{toolBreach()}
	svc := NewService(ws, 0, run, deliver).
		WithBands(func() []bands.Breach { return breaches }).
		WithVerdicts(log)

	svc.Tick(context.Background())
	if strings.Contains(prompts[0], "concluded the last times") {
		t.Errorf("the first check was shown a verdict nobody had made:\n%s", prompts[0])
	}
	if len(delivered) != 0 {
		t.Errorf("a verdict of nothing-to-report was delivered: %v", delivered)
	}

	svc.Tick(context.Background())
	if !strings.Contains(prompts[1], "concluded the last times") || !strings.Contains(prompts[1], "real but transient") {
		t.Errorf("the second check was not shown the first verdict:\n%s", prompts[1])
	}
	if !strings.Contains(prompts[1], "keeps coming back is not transient") {
		t.Error("the second check was not told what a repeat means")
	}

	// A different metric drifting is judged on its own record.
	breaches = []bands.Breach{{Metric: "cost per turn", Unit: "USD", Recent: 0.5, Baseline: 0.05, Sigma: 4, Samples: 9}}
	svc.Tick(context.Background())
	if strings.Contains(prompts[2], "real but transient") {
		t.Errorf("a verdict on tool errors was shown to a check on spend:\n%s", prompts[2])
	}

	// A fix reported without the token is delivered, and remembered.
	breaches = []bands.Breach{toolBreach()}
	reply = "Switched browser.fast_path off: browser_fetch had failed every one of its four calls since Sunday."
	svc.Tick(context.Background())
	if len(delivered) != 1 || !strings.HasPrefix(delivered[0], "Switched") {
		t.Errorf("delivered = %v, want the repair report", delivered)
	}
	svc.Tick(context.Background())
	if !strings.Contains(prompts[4], "Switched browser.fast_path off") {
		t.Errorf("the repair was not remembered:\n%s", prompts[4])
	}

	// A check with nothing drifting records nothing: there is no breach to
	// have a verdict on.
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- check the mail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	breaches = nil
	before, _ := os.ReadFile(log)
	svc.Tick(context.Background())
	after, _ := os.ReadFile(log)
	if string(before) != string(after) {
		t.Error("a check with no breach wrote a verdict")
	}
}

// The log is bounded on disk and in the prompt, and a torn line costs one
// verdict rather than all of them.
func TestVerdictLogIsBoundedAndForgiving(t *testing.T) {
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	n := 0
	run := func(context.Context, string) (string, error) {
		n++
		return fmt.Sprintf("verdict %d HEARTBEAT_OK", n), nil
	}
	svc := NewService(t.TempDir(), 0, run, nil).
		WithBands(func() []bands.Breach { return []bands.Breach{toolBreach()} }).
		WithVerdicts(log)
	for i := 0; i < keepVerdicts+10; i++ {
		svc.Tick(context.Background())
	}
	raw, _ := os.ReadFile(log)
	if lines := strings.Count(string(raw), "\n"); lines != keepVerdicts {
		t.Errorf("log holds %d verdicts, want %d", lines, keepVerdicts)
	}
	if err := os.WriteFile(log, append([]byte("{torn"), raw...), 0o600); err != nil {
		t.Fatal(err)
	}
	shown := svc.earlierVerdicts([]bands.Breach{toolBreach()})
	if got := strings.Count(shown, "\n- "); got != showVerdicts {
		t.Errorf("prompt shows %d verdicts, want %d:\n%s", got, showVerdicts, shown)
	}
	if !strings.Contains(shown, fmt.Sprintf("verdict %d ", keepVerdicts+10)) {
		t.Errorf("the newest verdict is missing:\n%s", shown)
	}
}

// Without a log there is nothing to show and nothing to keep, and a check
// runs exactly as it did before verdicts existed.
func TestNoVerdictLogMeansNoVerdicts(t *testing.T) {
	svc, s := serviceWith(t, "", []bands.Breach{toolBreach()})
	svc.Tick(context.Background())
	svc.Tick(context.Background())
	if strings.Contains(s.prompts[1], "concluded the last times") {
		t.Errorf("a verdict appeared with no log configured:\n%s", s.prompts[1])
	}
}
