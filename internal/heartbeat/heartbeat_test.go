package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHasActionable(t *testing.T) {
	cases := []struct {
		content string
		want    bool
	}{
		{"# Heartbeat\n\nIf nothing, reply HEARTBEAT_OK.\n", false},
		{"# Heartbeat\n\n- check the backup drive\n", true},
		{"* watch the CI\n", true},
		{"- [x] already done\n", false},
		{"- [ ] pending thing\n", true},
		{"", false},
	}
	for _, c := range cases {
		if got := HasActionable(c.content); got != c.want {
			t.Errorf("HasActionable(%q) = %v, want %v", c.content, got, c.want)
		}
	}
}

func write(t *testing.T, ws, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTickSkipsWithoutTasks(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "# Heartbeat\n\nnothing here\n")
	called := false
	s := NewService(ws, time.Minute, func(context.Context, string) (string, error) {
		called = true
		return "", nil
	}, nil)
	s.Tick(context.Background())
	if called {
		t.Error("LLM called for an empty heartbeat file")
	}
}

func TestTickSuppressesOK(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check things\n")
	var delivered []string
	s := NewService(ws,
		time.Minute,
		func(_ context.Context, prompt string) (string, error) { return "HEARTBEAT_OK", nil },
		func(content string) bool { delivered = append(delivered, content); return true },
	)
	s.Tick(context.Background())
	if len(delivered) != 0 {
		t.Errorf("HEARTBEAT_OK delivered: %v", delivered)
	}
}

func TestTickDeliversFindings(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check disk space\n")
	var delivered []string
	s := NewService(ws,
		time.Minute,
		func(_ context.Context, prompt string) (string, error) {
			return "Disk is 95% full — you should clean up.", nil
		},
		func(content string) bool { delivered = append(delivered, content); return true },
	)
	s.Tick(context.Background())
	if len(delivered) != 1 || delivered[0] != "Disk is 95% full — you should clean up." {
		t.Errorf("delivered = %v", delivered)
	}
}

// The model is asked for exactly the token and routinely writes its diagnosis
// first and the token after. That is still its verdict that nothing needs the
// user, and it was being read out to them as a finding.
func TestTickSuppressesAVerdictThatEndsWithOK(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check things\n")
	var delivered []string
	s := NewService(ws,
		time.Minute,
		func(_ context.Context, prompt string) (string, error) {
			return "The tool error rate spike is real but transient — nothing systemic.\n\nHEARTBEAT_OK", nil
		},
		func(content string) bool { delivered = append(delivered, content); return true },
	)
	s.Tick(context.Background())
	if len(delivered) != 0 {
		t.Errorf("a verdict of HEARTBEAT_OK was delivered: %v", delivered)
	}
}
