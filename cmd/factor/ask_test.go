package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/tui"
)

// pipedAsker wires a terminal asker to a pipe so its output can be read back.
func pipedAsker(t *testing.T) (*termAsker, func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return newTermAsker(tui.NewChat(nil, w)), func() string {
		_ = w.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		return string(out)
	}
}

// answerWith waits for the question to appear, then submits one line the way
// the REPL does. It runs on its own goroutine, so it reports rather than
// aborts; the caller's context deadline is what stops a stuck test.
func answerWith(t *testing.T, a *termAsker, line string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.deliver(line) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("nothing ever waited for %q", line)
}

func TestTermAskerNumberedChoice(t *testing.T) {
	asker, output := pipedAsker(t)
	go answerWith(t, asker, "2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := asker.Ask(ctx, tools.Question{
		Prompt:  "Which database?",
		Options: []string{"Postgres", "SQLite"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "SQLite" || answer.Dismissed {
		t.Errorf("answer = %+v, want the second option", answer)
	}
	out := output()
	for _, want := range []string{"factor asks: Which database?", "1) Postgres", "2) SQLite", "Type a number"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestTermAskerFreeText(t *testing.T) {
	asker, output := pipedAsker(t)
	go answerWith(t, asker, "call it Roxana")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := asker.Ask(ctx, tools.Question{Prompt: "What name?"})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "call it Roxana" {
		t.Errorf("answer = %q", answer.Text)
	}
	if out := output(); !strings.Contains(out, "Type your answer") || strings.Contains(out, "1)") {
		t.Errorf("open question output:\n%s", out)
	}
}

func TestTermAskerSkip(t *testing.T) {
	asker, output := pipedAsker(t)
	defer output()
	go answerWith(t, asker, "/skip")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := asker.Ask(ctx, tools.Question{Prompt: "Which?", Options: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if !answer.Dismissed {
		t.Errorf("answer = %+v, want it dismissed", answer)
	}
}

func TestTermAskerIgnoresLinesWithNoQuestion(t *testing.T) {
	asker, output := pipedAsker(t)
	defer output()
	if asker.deliver("hello") {
		t.Error("a line with no question waiting belongs to the agent")
	}
}

func TestTermAskerOneQuestionAtATime(t *testing.T) {
	asker, output := pipedAsker(t)
	defer output()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = asker.Ask(ctx, tools.Question{Prompt: "First?"})
	}()
	<-started
	// Wait until the first question is really on screen.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		asker.mu.Lock()
		waiting := asker.waiting != nil
		asker.mu.Unlock()
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}

	_, err := asker.Ask(ctx, tools.Question{Prompt: "Second?"})
	if !errors.Is(err, tools.ErrAskUnavailable) {
		t.Errorf("err = %v, want unavailable while another question waits", err)
	}
}

func TestTermAskerGivesUpWithTheTurn(t *testing.T) {
	asker, output := pipedAsker(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	if _, err := asker.Ask(ctx, tools.Question{Prompt: "Which?"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context error", err)
	}
	if out := output(); !strings.Contains(out, "carrying on without it") {
		t.Errorf("output does not say it moved on:\n%s", out)
	}
}
