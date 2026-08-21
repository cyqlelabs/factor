package memory

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
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
		if strings.TrimSpace(m.Content) == "" {
			// concept atoms carry their text in the label
			m.Content = m.Label
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
		}
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
	Spaces         SpacePolicy
	ignore         []*regexp.Regexp
	skewOnce       sync.Once
}

func NewAmbient(engine Engine, topK int, minConfidence float64, queryMsgs, queryMaxChars, injectMaxChars int, ignorePatterns []string, spaces SpacePolicy) *Ambient {
	a := &Ambient{
		Engine:         engine,
		TopK:           topK,
		MinConfidence:  minConfidence,
		QueryMsgs:      queryMsgs,
		QueryMaxChars:  queryMaxChars,
		InjectMaxChars: injectMaxChars,
		Spaces:         spaces,
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

// scope resolves the turn's space scope, saying once why routing is off when
// the engine writes somewhere other than the configured space — otherwise the
// split silently does nothing and the config looks like it took effect.
func (a *Ambient) scope(channel string) Scope {
	scope := scopeFor(a.Engine, a.Spaces, channel)
	if scope.Space == "" && a.Spaces.Strategy != "single" && a.Spaces.Main != "" {
		if ok, engineSpace := a.Engine.SpaceSupport(); ok && engineSpace != a.Spaces.Main {
			a.skewOnce.Do(func() {
				slog.Warn("memory space routing disabled: the engine writes to a different space",
					"engine_space", engineSpace, "configured", a.Spaces.Main,
					"fix", "set memory.space to the engine's space")
			})
		}
	}
	return scope
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
	// The loop stamps the turn's ToolContext on ctx before building the
	// system prompt, so the channel is already here — no signature change.
	mems, err := a.Engine.Recall(ctx, query, a.TopK, a.MinConfidence, a.scope(tools.ToolContextFrom(ctx).Channel))
	if err != nil {
		slog.Warn("memory recall failed", "error", err)
		return ""
	}
	return FormatMemories(mems, a.InjectMaxChars)
}

// StoreExchange persists both sides of a completed turn as episodes, in the
// space the turn's channel writes to. Call it from a goroutine; it must never
// block a reply. If the memory engine is still cold-booting (first sidecar
// start downloads models), it waits for health rather than dropping the very
// first memories.
func (a *Ambient) StoreExchange(channel, speaker, userText, assistantText string) {
	if a == nil || a.Engine == nil || !a.Engine.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	healthDeadline := time.Now().Add(90 * time.Second)
	for !a.Engine.Healthy() {
		if time.Now().After(healthDeadline) || ctx.Err() != nil {
			slog.Warn("memory store dropped; engine never became healthy")
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	// Who said it travels as the Source field, not as an English prefix on the
	// content. A prefix is invisible to smrti's scoring and decay — it reads as
	// ordinary text — so both sides of the turn used to be stored with equal
	// standing. It is also untranslated text injected into a multilingual graph.
	space := a.scope(channel).Space
	store := func(source, text string) {
		text = strings.TrimSpace(text)
		if text == "" || a.ignored(text) {
			return
		}
		if _, err := a.Engine.Remember(ctx, RememberRequest{Content: text, Source: source, Space: space}); err != nil {
			slog.Debug("memory store dropped", "error", err)
		}
	}
	store(SourceUser, attribute(speaker, userText))
	store(SourceAgent, assistantText)
}

// attribute names the person a memory came from, where the channel could tell
// one from another — a house with several voices, where "likes coffee without
// sugar" recorded against nobody in particular is a fact the agent will hand
// back to the wrong person.
//
// It goes in the content, which is not the compromise it looks like. Source
// carries standing (a human asserted this, or the agent did) and is a closed
// set the engine reasons about — smrti's own tool normalizes anything else
// away — so a name cannot ride there. Nor is a name the untranslated English
// the prefix rule warns about: it is a proper noun, which reads the same in
// every language the graph holds. Real per-person retrieval needs memory
// spaces, which is a decision about whether a household shares what it knows,
// not something to settle here.
func attribute(speaker, text string) string {
	if speaker == "" || strings.TrimSpace(text) == "" {
		return text
	}
	return speaker + ": " + text
}
