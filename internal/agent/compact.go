package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
)

// estimateTokens is a cheap chars/4 heuristic with per-message overhead —
// good enough to trigger compaction well before real limits.
func estimateTokens(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)/4 + 8
		for _, tc := range m.ToolCalls {
			total += len(tc.Name)/4 + 32
		}
	}
	return total
}

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
				fmt.Fprintf(&transcript, "assistant used tool: %s\n", tc.Name)
			}
		}
	}
	prior := l.sessions.Summary(sessionKey)
	prompt := "Summarize this conversation compactly for future context. Keep decisions, open tasks, user preferences, and hard facts; drop pleasantries. Reply with the summary only."
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
