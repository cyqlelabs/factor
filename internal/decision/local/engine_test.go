package local

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAutoPicksTheStudentWhereLayaCannotRun(t *testing.T) {
	cases := map[string]struct {
		avx2      bool
		available int
		known     bool
		want      string
	}{
		"modern machine": {true, 8000, true, EngineLaya},
		"no avx2":        {false, 8000, true, EngineStudent},
		"memory short":   {true, 512, true, EngineStudent},
		"memory unknown": {true, 0, false, EngineLaya},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			restoreCPU, restoreMem := hasAVX2, availableMB
			defer func() { hasAVX2, availableMB = restoreCPU, restoreMem }()
			hasAVX2 = func() bool { return c.avx2 }
			availableMB = func() (int, bool) { return c.available, c.known }
			b := New(Config{}, t.TempDir())
			if got := b.engine(); got != c.want {
				t.Errorf("engine = %q, want %q", got, c.want)
			}
			// Decided once: a later change in the machine does not move it.
			hasAVX2 = func() bool { return !c.avx2 }
			if got := b.engine(); got != c.want {
				t.Errorf("engine moved to %q after being decided as %q", got, c.want)
			}
		})
	}
}

func TestTheStudentIsServedBySmrtiOnTheDecisionPort(t *testing.T) {
	b := New(Config{Engine: EngineStudent, Port: 8731,
		Student: func(context.Context) (string, error) { return "/opt/bin/smrti", nil }}, t.TempDir())
	if b.Name() != EngineStudent {
		t.Errorf("name = %q", b.Name())
	}
	args := strings.Join(b.serveArgs(), " ")
	if !strings.HasPrefix(args, "serve decisions --host 127.0.0.1 --port 8731 --threads ") {
		t.Errorf("args = %q", args)
	}
	cmd, err := b.resolveCommand(context.Background())
	if err != nil || cmd != "/opt/bin/smrti" || b.serverCommand(cmd) != cmd {
		t.Errorf("command = %q err=%v", cmd, err)
	}
	// Explicit engine, explicit command: the command wins, as it does for Laya.
	b = New(Config{Engine: EngineStudent, Command: "definitely-not-a-binary"}, t.TempDir())
	if _, err := b.resolveCommand(context.Background()); err == nil {
		t.Error("an unresolvable command was accepted")
	}
	// No smrti to run it: reported, not spawned.
	b = New(Config{Engine: EngineStudent}, t.TempDir())
	if _, err := b.resolveCommand(context.Background()); err == nil || !strings.Contains(err.Error(), "smrti") {
		t.Errorf("err = %v", err)
	}
	b = New(Config{Engine: EngineStudent, Student: func(context.Context) (string, error) {
		return "", errors.New("not installed yet")
	}}, t.TempDir())
	if _, err := b.resolveCommand(context.Background()); err == nil {
		t.Error("a missing smrti was not reported")
	}
}
