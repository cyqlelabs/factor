package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedAsker answers whatever the test tells it to.
type scriptedAsker struct {
	answer Answer
	err    error
	got    Question
	block  bool // wait for the context instead of answering
}

func (s *scriptedAsker) Ask(ctx context.Context, q Question) (Answer, error) {
	s.got = q
	if s.block {
		<-ctx.Done()
		return Answer{}, ctx.Err()
	}
	return s.answer, s.err
}

func askArgs(question string, options ...string) map[string]any {
	args := map[string]any{"question": question}
	if len(options) > 0 {
		opts := make([]any, 0, len(options))
		for _, o := range options {
			opts = append(opts, o)
		}
		args["options"] = opts
	}
	return args
}

func TestAskToolAnswers(t *testing.T) {
	asker := &scriptedAsker{answer: Answer{Text: "SQLite"}}
	tool := NewAskTool(asker)
	res := tool.Execute(context.Background(), askArgs("Which database?", "Postgres", "SQLite"))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "SQLite") {
		t.Errorf("answer not reported: %q", res.ForLLM)
	}
	if len(asker.got.Options) != 2 || asker.got.Prompt != "Which database?" {
		t.Errorf("question not passed through: %+v", asker.got)
	}
	if asker.got.Timeout != defaultAskTimeout {
		t.Errorf("timeout = %s, want the default", asker.got.Timeout)
	}
	if tool.Name() != "ask_user" || tool.Description() == "" || tool.Parameters() == nil {
		t.Error("tool metadata is incomplete")
	}
}

func TestAskToolRejectsBadQuestions(t *testing.T) {
	tool := NewAskTool(&scriptedAsker{})
	cases := map[string]map[string]any{
		"empty":      askArgs("   "),
		"one option": askArgs("Which?", "only"),
		"too many":   askArgs("Which?", "a", "b", "c", "d", "e", "f", "g", "h", "i"),
	}
	for name, args := range cases {
		if res := tool.Execute(context.Background(), args); !res.IsError {
			t.Errorf("%s: expected an error, got %q", name, res.ForLLM)
		}
	}
	// Blank options are dropped rather than shown as empty buttons.
	asker := &scriptedAsker{answer: Answer{Text: "a"}}
	tool.SetAsker(asker)
	tool.Execute(context.Background(), map[string]any{
		"question": "Which?",
		"options":  []any{"a", "  ", "b", 7},
	})
	if len(asker.got.Options) != 2 {
		t.Errorf("options = %q, want the two real ones", asker.got.Options)
	}
}

func TestAskToolOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		asker   *scriptedAsker
		isError bool
		want    string
	}{
		{"dismissed", &scriptedAsker{answer: Answer{Dismissed: true}}, false, "dismissed"},
		{"empty answer", &scriptedAsker{answer: Answer{Text: "  "}}, false, "nothing at all"},
		{"unavailable", &scriptedAsker{err: ErrAskUnavailable}, true, "ask in the conversation"},
		{"broken", &scriptedAsker{err: errors.New("dialog exploded")}, true, "dialog exploded"},
		{"timed out", &scriptedAsker{err: context.DeadlineExceeded}, false, "no answer after"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := NewAskTool(tc.asker).Execute(context.Background(), askArgs("Which?"))
			if res.IsError != tc.isError {
				t.Errorf("IsError = %v, want %v (%q)", res.IsError, tc.isError, res.ForLLM)
			}
			if !strings.Contains(res.ForLLM, tc.want) {
				t.Errorf("result = %q, want it to mention %q", res.ForLLM, tc.want)
			}
		})
	}
}

func TestAskToolWithoutAsker(t *testing.T) {
	res := NewAskTool(nil).Execute(context.Background(), askArgs("Which?"))
	if !res.IsError || !strings.Contains(res.ForLLM, "conversation") {
		t.Errorf("result = %q, want an actionable error", res.ForLLM)
	}
}

func TestAskToolCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := NewAskTool(&scriptedAsker{block: true}).Execute(ctx, askArgs("Which?"))
	if !res.IsError || !strings.Contains(res.ForLLM, "cancelled") {
		t.Errorf("result = %q, want the cancellation error", res.ForLLM)
	}
}

func TestAskTimeoutClamped(t *testing.T) {
	tests := []struct {
		secs int
		want time.Duration
	}{
		{0, defaultAskTimeout},
		{-5, defaultAskTimeout},
		{1, minAskTimeout},
		{60, time.Minute},
		{99999, maxAskTimeout},
	}
	for _, tc := range tests {
		if got := askTimeout(map[string]any{"timeout_secs": tc.secs}); got != tc.want {
			t.Errorf("askTimeout(%d) = %s, want %s", tc.secs, got, tc.want)
		}
	}
}

func TestMatchOption(t *testing.T) {
	options := []string{"Postgres", "SQLite"}
	tests := []struct{ in, want string }{
		{"2", "SQLite"},
		{"1)", "Postgres"},
		{" sqlite ", "SQLite"},
		{"3", "3"},         // out of range: their own answer
		{"0", "0"},         // no zeroth option
		{"MySQL", "MySQL"}, // never on the list, still allowed
		{"", ""},           // nothing typed
		{"12x", "12x"},     // not a number
	}
	for _, tc := range tests {
		if got := MatchOption(tc.in, options); got != tc.want {
			t.Errorf("MatchOption(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := MatchOption("2", nil); got != "2" {
		t.Errorf("open question should keep the text, got %q", got)
	}
}

func TestAskToolSetAsker(t *testing.T) {
	first := &scriptedAsker{answer: Answer{Text: "one"}}
	second := &scriptedAsker{answer: Answer{Text: "two"}}
	tool := NewAskTool(first)
	tool.SetAsker(second)
	if res := tool.Execute(context.Background(), askArgs("Which?")); !strings.Contains(res.ForLLM, "two") {
		t.Errorf("result = %q, want the second asker's answer", res.ForLLM)
	}
}
