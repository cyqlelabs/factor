package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/tui"
)

// termAsker asks in the terminal the chat session already owns, the way a
// dialog would ask on the screen. The REPL is the only reader of stdin, so
// the question is printed there and the next line the user submits is routed
// here instead of to the agent.
type termAsker struct {
	con *tui.Console

	mu      sync.Mutex
	waiting chan string
}

func newTermAsker(con *tui.Console) *termAsker { return &termAsker{con: con} }

func (a *termAsker) Ask(ctx context.Context, q tools.Question) (tools.Answer, error) {
	ch := make(chan string, 1)
	a.mu.Lock()
	if a.waiting != nil {
		a.mu.Unlock()
		return tools.Answer{}, fmt.Errorf("%w: another question is already waiting", tools.ErrAskUnavailable)
	}
	a.waiting = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.waiting = nil
		a.mu.Unlock()
	}()

	a.con.Printf("%s", askLines(q))
	select {
	case line := <-ch:
		if strings.EqualFold(strings.TrimSpace(line), "/skip") {
			return tools.Answer{Dismissed: true}, nil
		}
		return tools.Answer{Text: tools.MatchOption(line, q.Options)}, nil
	case <-ctx.Done():
		a.con.Printf("(no answer — carrying on without it)")
		return tools.Answer{}, ctx.Err()
	}
}

// deliver routes one submitted line to the question waiting for it, and
// reports whether it took the line. Nothing waiting: the line is an ordinary
// message and belongs to the agent.
func (a *termAsker) deliver(line string) bool {
	a.mu.Lock()
	ch := a.waiting
	a.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- line:
		return true
	default:
		return false
	}
}

// askLines renders the question: what is being asked, the answers on offer,
// and how to reply. The options are numbered because a number is the shortest
// thing to type and the least to get wrong.
func askLines(q tools.Question) string {
	var b strings.Builder
	fmt.Fprintf(&b, "factor asks: %s", strings.TrimSpace(q.Prompt))
	for i, o := range q.Options {
		fmt.Fprintf(&b, "\n  %d) %s", i+1, o)
	}
	if len(q.Options) > 0 {
		b.WriteString("\nType a number, or your own answer. /skip to pass.")
	} else {
		b.WriteString("\nType your answer. /skip to pass.")
	}
	return b.String()
}
