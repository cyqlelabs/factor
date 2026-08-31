package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
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

// defaultContextWindow is the assumption of last resort, for a model neither
// the catalog nor the config can size.
const defaultContextWindow = 65536

// contextWindow is the context length compaction budgets against, resolved on
// every check: the models in play can gain a catalog entry after startup, and
// the answer must follow. The catalog's answer wins; config can only shrink
// it, the same clamp-only rule Claude Code applies, because a config larger
// than the model does not make the model larger.
func (l *Loop) contextWindow() int {
	window := 0
	if l.windowFor != nil {
		window = l.windowFor()
	}
	configured := l.cfg.Agent.ContextWindowTokens
	switch {
	case window > 0 && configured > 0 && configured < window:
		return configured
	case window > 0:
		return window
	case configured > 0:
		return configured
	default:
		return defaultContextWindow
	}
}

// defaultMaxContextTokens is the working ceiling when config does not set
// one. It is far below every current model's window on purpose: recall and
// instruction-following measurably decay with input length well before a
// window fills, so the window is the wrong number to budget against.
const defaultMaxContextTokens = 48000

// budget is the token ceiling one assembled request may reach before the
// session is compacted: a share of what the model accepts, floored by the
// working ceiling, whichever is smaller.
func (l *Loop) budget() int {
	budget := l.contextWindow() * l.cfg.Agent.SummarizeAtPercent / 100
	ceiling := l.cfg.Agent.MaxContextTokens
	if ceiling <= 0 {
		ceiling = defaultMaxContextTokens
	}
	if budget <= 0 || budget > ceiling {
		return ceiling
	}
	return budget
}

// overhead is what a request carries before a single message of history: the
// system prompt and the schema of every registered tool.
//
// Counting it is not a detail. Factor ships dozens of tools, and their
// schemas plus the persona, workspace files and skills catalog are a fixed
// five-figure cost on every call — budgeting the history alone declares a
// session small while the request that carries it is already large.
func (l *Loop) overhead() int {
	total := l.toolSchemaTokens()
	if l.builder != nil {
		total += len(l.builder.SystemPrompt()) / 4
	}
	return total
}

// toolSchemaTokens is the schema half of the overhead on its own, for the
// callers that are already holding the system prompt as a message and would
// otherwise count it twice.
func (l *Loop) toolSchemaTokens() int {
	if l.registry == nil {
		return 0
	}
	total := 0
	for _, def := range l.registry.Definitions() {
		total += len(def.Name)/4 + len(def.Description)/4 + 16
		if schema, err := json.Marshal(def.Parameters); err == nil {
			total += len(schema) / 4
		}
	}
	return total
}

// trimInFlight masks the older tool results of a turn that has outgrown the
// budget while it is still running.
//
// Masking otherwise happens once, when the request is assembled, and nothing
// touches the turn that grows behind it: twenty tool iterations at four
// thousand tokens a result is more than the whole budget, and the only thing
// that notices is the provider refusing the request outright. The working
// ceiling exists because recall and instruction-following decay long before a
// window fills, so a turn allowed to run past it is precisely where the rules
// at the head of the prompt stop being read and the core tools start being
// ignored.
//
// Under budget nothing is touched. A turn reading eight files to compare them
// needs all eight, and clearing half of them to save tokens that were
// affordable would be answering with half the evidence.
func (l *Loop) trimInFlight(messages []provider.Message) []provider.Message {
	if estimateTokens(messages)+l.toolSchemaTokens() <= l.budget() {
		return messages
	}
	return maskOldToolResults(messages)
}

// needsCompaction reports whether the session has outgrown the budget. It
// measures the history as the request will actually carry it — masked — not
// as it sits on disk. Measuring the raw log would spend a summarizing call on
// tokens that were never going to be sent, which is most of them in a session
// that has been reading web pages.
func (l *Loop) needsCompaction(history []provider.Message) bool {
	return l.overhead()+estimateTokens(maskOldToolResults(history)) > l.budget()
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
		// Compaction is spent on this session's behalf, so it is billed to
		// it: the turn that ended is gone, but the tool context is what
		// tells the meter whose spend this is.
		if err := l.compact(tools.WithToolContext(ctx, in.toolCtx), in.sessionKey); err != nil {
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
	req := &provider.Request{
		Messages: []provider.Message{
			{Role: "system", Content: prompt},
			{Role: "user", Content: transcript.String()},
		},
		MaxTokens:   1024,
		NoReasoning: true,
	}
	resp, err := l.utilityChat().Chat(ctx, req)
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}
	// A summary that hit the token cap is cut mid-sentence, and the cut falls
	// on the newest facts — the part compaction exists to keep. Ask once for
	// a version that fits; if that also overflows, salvage the draft at its
	// last complete line so no half-written fact is stored as truth.
	if summaryTruncated(resp.FinishReason) {
		req.Messages[0].Content = prompt + "\n\nYour summary was cut off by the length limit. Rewrite it at half the length: keep identifiers, decisions, and open tasks; drop everything else."
		if retry, rerr := l.utilityChat().Chat(ctx, req); rerr == nil && strings.TrimSpace(retry.Content) != "" && !summaryTruncated(retry.FinishReason) {
			resp = retry
		} else if i := strings.LastIndexByte(strings.TrimRight(resp.Content, "\n"), '\n'); i > 0 {
			resp.Content = resp.Content[:i]
		}
	}

	// Truncation is irreversible for the live history, so an empty summary is
	// not a summary to store: it would trade the conversation for nothing.
	// Leaving it uncompacted costs one oversized request and the next idle
	// sweep tries again.
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return fmt.Errorf("summarize: the model returned no summary (finish reason %q)", resp.FinishReason)
	}
	if err := l.sessions.SetSummaryAt(sessionKey, summary, priorSkip+cut); err != nil {
		return err
	}
	return l.sessions.Compact(sessionKey)
}

// summaryTruncated reports whether a finish reason means the reply hit the
// token cap: OpenAI-compatible dialects say "length", native Anthropic says
// "max_tokens" (stop reasons pass through the chain untranslated).
func summaryTruncated(reason string) bool {
	return reason == "length" || reason == "max_tokens"
}
