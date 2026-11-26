package wizard

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
)

func swarmWiz(t *testing.T, answers ...string) (*wiz, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	cfg := config.Default()
	w := &wiz{cfg: cfg, ui: NewPlain(strings.NewReader(strings.Join(answers, "\n")+"\n"), &out)}
	w.opts.SwarmJoined = func() (int, bool) { return 0, false }
	return w, &out
}

func TestSwarmStepPairsByShowingACode(t *testing.T) {
	w, out := swarmWiz(t, "y", "wss://rv.example", "y")
	var got config.SwarmConfig
	w.opts.JoinSwarm = func(_ context.Context, cfg config.SwarmConfig, show func(string)) (string, error) {
		got = cfg
		show("47-38291506")
		return "desk", nil
	}
	if err := w.stepSwarm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w.cfg.Swarm.Enabled || got.Rendezvous != "wss://rv.example" || w.cfg.Swarm.Rendezvous != "wss://rv.example" {
		t.Errorf("config %+v, joined with %+v", w.cfg.Swarm, got)
	}
	if !strings.Contains(out.String(), "47-3829-1506") || w.swarm != "on, paired with desk" {
		t.Errorf("output %q, summary %q", out.String(), w.swarm)
	}
}

func TestSwarmStepChoices(t *testing.T) {
	// No, with the swarm on, turns it off.
	w, _ := swarmWiz(t, "n")
	w.cfg.Swarm.Enabled = true
	if err := w.stepSwarm(context.Background()); err != nil || w.cfg.Swarm.Enabled {
		t.Errorf("no: %v %v", err, w.cfg.Swarm.Enabled)
	}
	// Already in a swarm: nothing to pair.
	w, out := swarmWiz(t, "y")
	w.opts.SwarmJoined = func() (int, bool) { return 3, true }
	if err := w.stepSwarm(context.Background()); err != nil || !strings.Contains(out.String(), "swarm of 3") {
		t.Errorf("joined: %v %q", err, out.String())
	}
	// No rendezvous: on, and unpaired.
	w, _ = swarmWiz(t, "y", "")
	if err := w.stepSwarm(context.Background()); err != nil || w.swarm != "on, not paired" || !w.cfg.Swarm.Enabled {
		t.Errorf("blank address: %v %q", err, w.swarm)
	}
	// Not now.
	w, _ = swarmWiz(t, "y", "wss://r", "n")
	if err := w.stepSwarm(context.Background()); err != nil || w.swarm != "on, not paired" {
		t.Errorf("later: %v %q", err, w.swarm)
	}
	// A failed pairing leaves the swarm on, to be paired later.
	w, out = swarmWiz(t, "y", "wss://r", "y")
	w.opts.JoinSwarm = func(context.Context, config.SwarmConfig, func(string)) (string, error) {
		return "", errors.New("the code expired")
	}
	if err := w.stepSwarm(context.Background()); err != nil || !strings.Contains(out.String(), "the code expired") {
		t.Errorf("failure: %v %q", err, out.String())
	}
	// With no way to pair, it says nothing it cannot do.
	w, _ = swarmWiz(t, "y", "wss://r", "y")
	if err := w.stepSwarm(context.Background()); err != nil || w.swarm != "on, not paired" {
		t.Errorf("no pairing: %v %q", err, w.swarm)
	}
}

func TestSwarmDefaults(t *testing.T) {
	o := Options{Home: t.TempDir()}
	o.defaults()
	if _, ok := o.SwarmJoined(); ok {
		t.Error("a fresh home reads as joined")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := o.JoinSwarm(ctx, config.SwarmConfig{}, func(string) {}); err == nil {
		t.Error("joined with no rendezvous")
	}
}
