package local

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/decision"
)

// The one test that runs the model Factor actually ships: it installs the
// runtime into a private virtualenv, fetches the published weights, starts
// the server and asks it a real question.
//
// It is behind an environment variable because it needs the network and a
// 250 MB artifact, which no ordinary test run should pay for. Everything
// else in this package stands in for the model; this is what says the
// install, the download and the wire shape are right.
//
//	FACTOR_TEST_LAYA=1 go test ./internal/decision/local -run TestLive -v -timeout 30m
func TestLiveModelInstallsLoadsAndAnswers(t *testing.T) {
	if os.Getenv("FACTOR_TEST_LAYA") == "" {
		t.Skip("set FACTOR_TEST_LAYA=1 to install and drive the real model")
	}
	home := os.Getenv("FACTOR_TEST_LAYA_HOME")
	if home == "" {
		home = t.TempDir()
	}
	b := New(Config{Port: freePort(t)}, home)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	b.Provision(ctx)
	b.Start(ctx)
	t.Cleanup(b.Stop)

	deadline := time.Now().Add(20 * time.Minute)
	for !b.Healthy() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
	}
	if !b.Healthy() {
		t.Fatalf("the model never became ready: %s", b.Down())
	}
	limits := b.Limits()
	t.Logf("ready: limits=%+v", limits)
	if limits.MaxCandidates <= 0 || limits.MaxStateChars <= 0 {
		t.Errorf("the model reported no window: %+v", limits)
	}

	// Asked in Spanish, because the checkpoint is chosen for exactly this:
	// Factor answers in whatever language its user speaks.
	started := time.Now()
	resp, err := b.Decide(ctx, &decision.Request{
		State: map[string]any{
			"page": map[string]any{
				"title": "Búsqueda de vuelos",
				"text":  "Vuelos de Zúrich a Londres: 3 opciones disponibles",
			},
		},
		Questions: map[string]decision.Question{
			"operation": {
				Criteria: map[string]any{
					"CLICK": "Hacer clic en un elemento de la página",
					"DONE":  "Todos los requisitos del objetivo ya se ven cumplidos en esta página",
				},
				Instructions: map[string]any{
					"goal": "Encontrar vuelos de Zúrich a Londres; listo cuando se vean los resultados",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	answer := resp.Answers["operation"]
	t.Logf("answered %q at %.3f in %s (%d tokens, model %s): %v",
		answer.Choice, answer.Confidence, time.Since(started).Round(time.Millisecond),
		resp.Usage.InputTokens, resp.Model, answer.Probabilities)

	// The contract, on a real reply rather than a fake one.
	if err := decision.Validate(answer, []string{"CLICK", "DONE"}); err != nil {
		t.Fatalf("the model's own answer does not satisfy the contract: %v", err)
	}
	if resp.Usage.InputTokens <= 0 {
		t.Errorf("no token count came back: %+v", resp.Usage)
	}
	// The page plainly shows the results the goal asked for, so this is the
	// judgement the whole feature rests on being able to make.
	if answer.Choice != "DONE" {
		t.Errorf("a page showing the results answered %q", answer.Choice)
	}
}
