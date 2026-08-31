package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
