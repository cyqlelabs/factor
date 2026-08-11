package memory

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
)

// BuildRecallQuery assembles the recall query from recent conversation
// context (not just the last message), keeping the most recent tail when
// truncating — mirrors smrti's own proxy semantics.
func BuildRecallQuery(history []provider.Message, current string, contextMsgs, maxChars int) string {
	if contextMsgs <= 0 {
		contextMsgs = 5
	}
	if maxChars <= 0 {
		maxChars = 500
	}
	var parts []string
	for _, m := range history {
		if (m.Role == "user" || m.Role == "assistant") && strings.TrimSpace(m.Content) != "" {
			parts = append(parts, strings.TrimSpace(m.Content))
		}
	}
	if len(parts) > contextMsgs {
		parts = parts[len(parts)-contextMsgs:]
	}
	if strings.TrimSpace(current) != "" {
		parts = append(parts, strings.TrimSpace(current))
	}
	query := strings.Join(parts, "\n")
	if len(query) > maxChars {
		query = query[len(query)-maxChars:]
	}
	return query
}

// FormatMemories renders recalled memories for the system prompt: severe
// memories become behavioral constraints, the rest background notes, each
// with a confidence qualifier.
func FormatMemories(mems []Memory, maxCharsEach int) string {
	if len(mems) == 0 {
		return ""
	}
	if maxCharsEach <= 0 {
		maxCharsEach = 500
	}
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > maxCharsEach {
			s = s[:maxCharsEach] + "…"
		}
		return s
	}
	var constraints, background []string
	for _, m := range mems {
		conf := int(m.Confidence*100 + 0.5)
		switch m.Severity {
		case SeverityCriticalWarning:
			constraints = append(constraints,
				fmt.Sprintf("- YOU MUST NOT repeat this past mistake (%d%% confidence): %s", conf, clip(m.Content)))
		case SeverityKnownAntipattern:
			constraints = append(constraints,
				fmt.Sprintf("- AVOID this known antipattern (%d%% confidence): %s", conf, clip(m.Content)))
		default:
			background = append(background,
				fmt.Sprintf("- Note (%d%% confidence): %s", conf, clip(m.Content)))
		}
	}
	var b strings.Builder
	b.WriteString("# Memory\n\nRecalled from long-term memory (smrti):\n")
	if len(constraints) > 0 {
		b.WriteString("\nBehavioral constraints:\n")
		b.WriteString(strings.Join(constraints, "\n"))
		b.WriteString("\n")
	}
	if len(background) > 0 {
		b.WriteString("\nBackground:\n")
		b.WriteString(strings.Join(background, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// Ambient stores conversation exchanges and recalls context for each turn.
type Ambient struct {
	Engine         Engine
	TopK           int
	MinConfidence  float64
	QueryMsgs      int
	QueryMaxChars  int
	InjectMaxChars int
	ignore         []*regexp.Regexp
}

func NewAmbient(engine Engine, topK int, minConfidence float64, queryMsgs, queryMaxChars, injectMaxChars int, ignorePatterns []string) *Ambient {
	a := &Ambient{
		Engine:         engine,
		TopK:           topK,
		MinConfidence:  minConfidence,
		QueryMsgs:      queryMsgs,
		QueryMaxChars:  queryMaxChars,
		InjectMaxChars: injectMaxChars,
	}
	for _, p := range ignorePatterns {
		if re, err := regexp.Compile(p); err == nil {
			a.ignore = append(a.ignore, re)
		} else {
			slog.Warn("bad memory ignore pattern", "pattern", p, "error", err)
		}
	}
	return a
}

func (a *Ambient) ignored(content string) bool {
	for _, re := range a.ignore {
		if re.MatchString(content) {
			return true
		}
	}
	return false
}

// MemoryPrompt recalls context for the upcoming turn. Best-effort: failures
// log and return "" so a memory outage never blocks a reply.
func (a *Ambient) MemoryPrompt(ctx context.Context, history []provider.Message, current string) string {
	if a == nil || a.Engine == nil || !a.Engine.Healthy() {
		return ""
	}
	query := BuildRecallQuery(history, current, a.QueryMsgs, a.QueryMaxChars)
	if query == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mems, err := a.Engine.Recall(ctx, query, a.TopK, a.MinConfidence)
	if err != nil {
		slog.Warn("memory recall failed", "error", err)
		return ""
	}
	return FormatMemories(mems, a.InjectMaxChars)
}

// StoreExchange persists both sides of a completed turn as episodes.
// Call it from a goroutine; it must never block a reply.
func (a *Ambient) StoreExchange(userText, assistantText string) {
	if a == nil || a.Engine == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := func(prefix, text string) {
		text = strings.TrimSpace(text)
		if text == "" || a.ignored(text) {
			return
		}
		if _, err := a.Engine.Remember(ctx, RememberRequest{Content: prefix + text}); err != nil {
			slog.Debug("memory store dropped", "error", err)
		}
	}
	store("User said: ", userText)
	store("Assistant replied: ", assistantText)
}
