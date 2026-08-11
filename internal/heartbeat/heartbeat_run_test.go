package heartbeat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewServiceDefaultsInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		s := NewService(t.TempDir(), interval, nil, nil)
		if s.interval != 30*time.Minute {
			t.Errorf("NewService(%v).interval = %v, want 30m", interval, s.interval)
		}
	}
	if s := NewService(t.TempDir(), 90*time.Second, nil, nil); s.interval != 90*time.Second {
		t.Errorf("explicit interval overwritten: %v", s.interval)
	}
}

func TestRunTicksThenReturnsOnCancellation(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check the queue\n")
	ticks := make(chan struct{}, 1)
	s := NewService(ws, time.Millisecond, func(context.Context, string) (string, error) {
		select {
		case ticks <- struct{}{}:
		default:
		}
		return OKToken, nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	select {
	case <-ticks:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never fired a heartbeat tick")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestTickWithoutHeartbeatFile(t *testing.T) {
	called := false
	s := NewService(t.TempDir(), time.Minute,
		func(context.Context, string) (string, error) {
			called = true
			return "something", nil
		},
		func(string) bool {
			t.Error("delivered a result although there is no HEARTBEAT.md")
			return true
		},
	)
	s.Tick(context.Background())
	if called {
		t.Error("LLM called although HEARTBEAT.md is missing")
	}
}

func TestTickPromptCarriesTasksAndOKToken(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check disk space\n")
	var prompt string
	s := NewService(ws, time.Minute,
		func(_ context.Context, p string) (string, error) {
			prompt = p
			return OKToken, nil
		}, nil)
	s.Tick(context.Background())

	if !strings.Contains(prompt, "check disk space") {
		t.Errorf("prompt omits the heartbeat tasks: %q", prompt)
	}
	if !strings.Contains(prompt, OKToken) {
		t.Errorf("prompt omits the %s opt-out: %q", OKToken, prompt)
	}
}

func TestTickIgnoresRunnerError(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check the queue\n")
	var delivered []string
	s := NewService(ws, time.Minute,
		func(context.Context, string) (string, error) {
			return "half-written output", errors.New("model unavailable")
		},
		func(content string) bool {
			delivered = append(delivered, content)
			return true
		},
	)
	s.Tick(context.Background())
	if len(delivered) != 0 {
		t.Errorf("a failed turn was delivered to the user: %v", delivered)
	}
}

func TestTickSuppressesOKPrefixAndBlankReplies(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check the queue\n")
	for _, reply := range []string{
		OKToken + " — nothing needs attention",
		"  " + OKToken + "\n",
		"",
		"   \n\t",
	} {
		var delivered []string
		s := NewService(ws, time.Minute,
			func(context.Context, string) (string, error) { return reply, nil },
			func(content string) bool {
				delivered = append(delivered, content)
				return true
			},
		)
		s.Tick(context.Background())
		if len(delivered) != 0 {
			t.Errorf("reply %q was delivered: %v", reply, delivered)
		}
	}
}

func TestTickToleratesUndeliverableResult(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check the queue\n")
	calls := 0
	s := NewService(ws, time.Minute,
		func(context.Context, string) (string, error) { return "disk is nearly full", nil },
		func(string) bool {
			calls++
			return false // no active channel to deliver to
		},
	)
	s.Tick(context.Background())
	if calls != 1 {
		t.Errorf("deliver called %d times, want 1", calls)
	}
}

func TestTickWithNilDeliver(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "- check the queue\n")
	s := NewService(ws, time.Minute,
		func(context.Context, string) (string, error) { return "disk is nearly full", nil }, nil)
	s.Tick(context.Background()) // must not panic
}
