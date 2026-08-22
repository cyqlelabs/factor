package agent

import (
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/provider"
)

// keepRecentToolResults is how many tool results ride the request whole. It
// is small on purpose: what a page said stops informing the next action about
// as fast as the agent finishes reading it, and everything before that window
// is re-fetchable rather than lost.
const keepRecentToolResults = 4

// minMaskableResult is the size below which masking is not worth doing. A
// stub naming the tool and its arguments is not much shorter than a one-line
// result, and rewriting a message that was already cheap spends prefix cache
// for nothing.
const minMaskableResult = 400

// maskOldToolResults replaces the body of every tool result older than the
// last keepRecentToolResults with a stub naming the call that produced it.
//
// This is where a long session's tokens actually are: measured across real
// sessions, tool results are 79-91% of the bytes in the history, and browser
// reads alone are most of that. The conversation the user had is a rounding
// error beside the pages the agent read on their behalf.
//
// The stub is restorable, which is what makes the loss acceptable: it keeps
// the tool's name and a bounded rendering of its arguments — the URL, the
// path, the command — so anything dropped can be fetched again by repeating
// the call. What the agent concluded from a result is in its own replies and
// in the rolling summary; what the result literally said is reproducible.
//
// It runs on the assembled copy and never on the session log: the transcript
// on disk stays whole, and masking is a decision about this one request.
//
// Two things stay: a result the model has not had a turn to react to yet, and
// the fact that a call failed. Recent failures are what stop the agent
// repeating an approach that does not work, and an old failure's stack trace
// is worth far less than the sentence saying it failed.
func maskOldToolResults(history []provider.Message) []provider.Message {
	if len(history) == 0 {
		return history
	}
	out := make([]provider.Message, len(history))
	copy(out, history)

	calls := map[string]provider.ToolCall{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = tc
		}
	}

	seen := 0
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != "tool" {
			continue
		}
		seen++
		if seen <= keepRecentToolResults || len(out[i].Content) < minMaskableResult {
			continue
		}
		out[i].Content = maskedResult(calls[out[i].ToolCallID], out[i].Content)
	}
	return out
}

// maskedResult renders the stub that stands in for a cleared tool result.
func maskedResult(call provider.ToolCall, content string) string {
	name := call.Name
	if name == "" {
		name = "a tool"
	}
	outcome := "result"
	if strings.HasPrefix(content, "ERROR: ") {
		outcome = "failure"
	}
	return fmt.Sprintf("[The %s of %s%s was cleared to save context. Run it again if you need it.]",
		outcome, name, summarizeArgs(call.Args))
}
