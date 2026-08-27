package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Question is one thing the agent needs from the user.
type Question struct {
	Prompt  string
	Options []string // empty: an open question
	Timeout time.Duration
}

// Answer is what came back. Dismissed means the user closed the dialog or
// skipped the question, which is an answer of its own: they chose not to say.
type Answer struct {
	Text      string
	Dismissed bool
}

// Asker puts a question in front of the user and waits for the reply: in the
// chat whose turn is asking, on the daemon's desktop dialog when no chat is
// behind the turn, or in the terminal a chat session already owns — wherever
// the user actually is.
type Asker interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}

// ErrAskUnavailable means there is no way to reach the user right now — a
// headless box, no dialog program, or a question already on screen.
var ErrAskUnavailable = errors.New("no way to put a question to the user")

const (
	defaultAskTimeout = 2 * time.Minute
	minAskTimeout     = 10 * time.Second
	maxAskTimeout     = 15 * time.Minute
	maxAskOptions     = 8
)

// AskTool asks the user a question mid-turn. The asker is swapped at the
// composition root: the CLI hands it the terminal, everything else routes by
// the asking turn — its own chat first, the desktop dialog as the fallback.
type AskTool struct {
	mu    sync.RWMutex
	asker Asker
}

func NewAskTool(a Asker) *AskTool { return &AskTool{asker: a} }

// SetAsker points the tool at a different way of reaching the user.
func (t *AskTool) SetAsker(a Asker) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.asker = a
}

func (t *AskTool) current() Asker {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.asker
}

func (t *AskTool) Name() string { return "ask_user" }

func (t *AskTool) Description() string {
	return "Ask the user a question and wait for their answer. Use it when the work cannot " +
		"go on without a decision only they can make: a detail you have no way to know, a " +
		"choice between real alternatives, or a go-ahead before something you cannot undo. " +
		"Give options when the answer is a pick from a short list; leave them out for an open " +
		"question. Ask one thing at a time, in their language. The reply comes back as text, " +
		"or as a note that no answer came. Never use it for something you can look up, and " +
		"never to confirm work you have already been asked to do."
}

func (t *AskTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "The question, as one plain sentence in the user's language",
			},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Up to 8 short answers to choose from; the user can still write their own",
			},
			"timeout_secs": map[string]any{
				"type":        "integer",
				"description": "How long to wait for an answer (default 120)",
			},
		},
		"required": []any{"question"},
	}
}

func (t *AskTool) Execute(ctx context.Context, args map[string]any) *Result {
	prompt := strings.TrimSpace(StringArg(args, "question"))
	if prompt == "" {
		return Errorf("question must not be empty")
	}
	options, err := askOptions(args)
	if err != nil {
		return Errorf("%v", err)
	}
	timeout := askTimeout(args)

	asker := t.current()
	if asker == nil {
		return Errorf("%s — ask in the conversation instead", ErrAskUnavailable)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	answer, err := asker.Ask(ctx, Question{Prompt: prompt, Options: options, Timeout: timeout})
	switch {
	case errors.Is(err, ErrAskUnavailable):
		return Errorf("%v — ask in the conversation instead", err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return Text(fmt.Sprintf("no answer after %s. That means only that this question went "+
			"unanswered — not that the user is away. Carry on without an answer, or leave the "+
			"question in your reply.", timeout))
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return Errorf("the turn was cancelled before the user answered")
	case err != nil:
		return Errorf("could not ask: %v", err)
	case answer.Dismissed:
		return Text("the user dismissed the question without answering. Do not ask again — " +
			"carry on without them, or leave the question in your reply.")
	}
	text := strings.TrimSpace(answer.Text)
	if text == "" {
		return Text("the user answered with nothing at all. Treat it as no answer.")
	}
	return Text("the user answered: " + text)
}

// askOptions reads the option list, keeping it short enough to read at a
// glance: a dialog with twenty buttons is a worse question, not a richer one.
func askOptions(args map[string]any) ([]string, error) {
	raw, ok := args["options"].([]any)
	if !ok {
		return nil, nil
	}
	var out []string
	for _, item := range raw {
		s, _ := item.(string)
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) > maxAskOptions {
		return nil, fmt.Errorf("at most %d options, got %d — ask a narrower question", maxAskOptions, len(out))
	}
	if len(out) == 1 {
		return nil, errors.New("one option is not a choice — give at least two, or none for an open question")
	}
	return out, nil
}

func askTimeout(args map[string]any) time.Duration {
	secs := IntArg(args, "timeout_secs", 0)
	if secs <= 0 {
		return defaultAskTimeout
	}
	d := time.Duration(secs) * time.Second
	if d < minAskTimeout {
		return minAskTimeout
	}
	if d > maxAskTimeout {
		return maxAskTimeout
	}
	return d
}

// MatchOption resolves what the user typed against the offered options: the
// number they were shown, or the label itself. Anything else is their own
// answer, which is always allowed — a question with a wrong list of options
// should not force a wrong answer.
func MatchOption(input string, options []string) string {
	input = strings.TrimSpace(input)
	if len(options) == 0 || input == "" {
		return input
	}
	if n, err := parseIndex(input); err == nil && n >= 1 && n <= len(options) {
		return options[n-1]
	}
	for _, o := range options {
		if strings.EqualFold(strings.TrimSpace(input), o) {
			return o
		}
	}
	return input
}

func parseIndex(s string) (int, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), ")")
	if s == "" {
		return 0, errors.New("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
