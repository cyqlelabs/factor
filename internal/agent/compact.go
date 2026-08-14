package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
)

// estimateTokens is a cheap chars/4 heuristic with per-message overhead —
// good enough to trigger compaction well before real limits.
//
// Tool arguments are counted, not just the tool name: a write_file call
// carries its entire payload to the provider, and charging that as a flat
// per-call constant undercounts the biggest turns badly enough to let the
// window overflow before compaction ever fires.
func estimateTokens(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)/4 + 8
		for _, tc := range m.ToolCalls {
			total += len(tc.Name)/4 + 32
			for k, v := range tc.Args {
				total += len(k) / 4
				if s, ok := v.(string); ok {
					total += len(s) / 4 // the case that actually gets large
					continue
				}
				total += 4
			}
		}
	}
	return total
}

// summarizeArgs renders tool-call arguments for the compaction transcript,
// bounded so a write_file body cannot crowd out the conversation it is meant
// to summarize. Long values are elided; the identifiers stay whole, since a
// truncated path is worse than no path at all.
func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		text := strings.Join(strings.Fields(fmt.Sprint(args[k])), " ")
		if text == "" {
			continue
		}
		if len(text) > maxArgChars {
			text = text[:maxArgChars] + "…"
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%s", k, text)
		if b.Len() > maxArgsPerCall {
			break
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "(" + b.String() + ")"
}

const (
	maxArgChars    = 120 // one value: enough for a path or a short command
	maxArgsPerCall = 300 // one call: enough for a handful of identifiers
)

// findSafeBoundary returns the smallest index >= target where the history
// can be cut without splitting an assistant tool-call from its results:
// only a plain user message opens a turn, so cut right before one.
func findSafeBoundary(msgs []provider.Message, target int) int {
	for i := target; i < len(msgs); i++ {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return -1
}

func (l *Loop) needsCompaction(history []provider.Message) bool {
	if len(history) > l.cfg.Agent.SummarizeAtMessages {
		return true
	}
	budget := l.cfg.Agent.ContextWindowTokens * l.cfg.Agent.SummarizeAtPercent / 100
	return budget > 0 && estimateTokens(history) > budget
}

func (l *Loop) maybeCompactAsync(in turnInput) {
	if in.ephemeral {
		return
	}
	history, err := l.sessions.History(in.sessionKey)
	if err != nil || !l.needsCompaction(history) {
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := l.compact(ctx, in.sessionKey); err != nil {
			slog.Warn("compaction failed", "session", in.sessionKey, "error", err)
		}
	}()
}

// compact summarizes the old portion of a session at a turn-safe boundary
// and truncates the live history to the recent tail. Compactions are
// serialized: the truncation offset is absolute (snapshot skip + cut), so
// messages appended during the summarize LLM call are never truncated and
// the cut cannot drift off its turn-safe boundary.
func (l *Loop) compact(ctx context.Context, sessionKey string) error {
	l.compactMu.Lock()
	defer l.compactMu.Unlock()
	priorSkip := l.sessions.Skip(sessionKey)
	history, err := l.sessions.History(sessionKey)
	if err != nil {
		return err
	}
	keep := l.cfg.Agent.KeepRecentMessages
	if keep <= 0 {
		keep = 8
	}
	if len(history) <= keep {
		return nil
	}
	cut := findSafeBoundary(history, len(history)-keep)
	if cut <= 0 {
		return nil // nothing safely cuttable
	}
	old := history[:cut]

	var transcript strings.Builder
	for _, m := range old {
		switch m.Role {
		case "user", "assistant":
			if strings.TrimSpace(m.Content) != "" {
				fmt.Fprintf(&transcript, "%s: %s\n", m.Role, m.Content)
			}
			for _, tc := range m.ToolCalls {
				// The tool name alone loses the thing worth keeping: which file
				// was written, which skill was created, which job was started.
				// Those identifiers are what the agent needs after the raw turns
				// are gone, so carry a bounded rendering of the arguments.
				fmt.Fprintf(&transcript, "assistant used tool: %s%s\n", tc.Name, summarizeArgs(tc.Args))
			}
		}
	}
	prior := l.sessions.Summary(sessionKey)
	prompt := "Summarize this conversation compactly for future context. Keep decisions, open tasks, user preferences, and hard facts; drop pleasantries. Preserve verbatim every identifier the work produced or depends on — file paths, skill names, job and cron ids, URLs, commands that worked — and note what was tried and failed so it is not repeated. Reply with the summary only."
	if prior != "" {
		prompt += "\n\nEarlier summary (fold it in):\n" + prior
	}
	resp, err := l.chat.Chat(ctx, &provider.Request{
		Messages: []provider.Message{
			{Role: "system", Content: prompt},
			{Role: "user", Content: transcript.String()},
		},
		MaxTokens: 1024,
	})
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}

	if err := l.sessions.SetSummaryAt(sessionKey, strings.TrimSpace(resp.Content), priorSkip+cut); err != nil {
		return err
	}
	return l.sessions.Compact(sessionKey)
}
