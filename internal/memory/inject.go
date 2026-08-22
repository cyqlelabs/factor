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
	// sharedOnce keeps the "cannot isolate this audience" warning to one
	// line: it would otherwise repeat on every turn a guest is present for.
	sharedOnce sync.Once

	// audienceMu guards lastAudience, which is the audience of the most
	// recent stored turn per channel — the only state needed to recognize a
	// gathering ending. bridgeNow carries that moment to WatchBridges;
	// buffered by one, because two gatherings ending before either merge
	// runs still only need one merge.
	audienceMu   sync.Mutex
	lastAudience map[string]string
	bridgeNow    chan struct{}
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
		lastAudience:   map[string]string{},
		bridgeNow:      make(chan struct{}, 1),
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
//
// The second return is whether this turn may recall at all. It is false only
// for a turn somebody else can hear that the policy cannot isolate, and that
// refusal is the point: recall exists to be spoken out loud, so a shared turn
// served from the one space holding everything would read the user's private
// graph into a room with a guest in it. Losing recall for that turn is the
// cheap failure; the other one cannot be taken back.
func (a *Ambient) scope(channel, audience string) (Scope, bool) {
	scope, recall := scopeFor(a.Engine, a.Spaces, channel, audience)
	if scope.Space == "" && a.Spaces.Strategy != "single" && a.Spaces.Main != "" {
		if ok, engineSpace := a.Engine.SpaceSupport(); ok && engineSpace != a.Spaces.Main {
			a.skewOnce.Do(func() {
				slog.Warn("memory space routing disabled: the engine writes to a different space",
					"engine_space", engineSpace, "configured", a.Spaces.Main,
					"fix", "set memory.space to the engine's space")
			})
		}
	}
	if !recall {
		a.sharedOnce.Do(func() {
			slog.Warn("recall skipped while somebody else is present: this memory cannot be isolated by audience",
				"fix", "set memory.shared_space and run a smrti that routes spaces")
		})
	}
	return scope, recall
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
	tc := tools.ToolContextFrom(ctx)
	scope, recall := a.scope(tc.Channel, tc.Audience)
	if !recall {
		return ""
	}
	return FormatMemories(
		a.recall(ctx, query, strings.TrimSpace(current), scope),
		a.InjectMaxChars,
	)
}

// recall asks twice and interleaves the answers: once with the turn's own
// message, once with the conversation tail behind it. Neither query serves
// both shapes of turn on its own. A message that changes the subject is a few
// words inside a tail about something else — 5% of the text on the turn that
// exposed this — so the tail's embedding buries it, and a question about a
// person returns nothing about them while the fact sits in the graph. A
// message that only refers back ("and her?", "look again") names no subject at
// all, and the tail is the only thing that says what it is about. Capping how
// much tail rides along cannot separate the two: measured against both, every
// cap that rescued one lost the other. Asking twice serves both, and the
// second query costs one more call to a sidecar on loopback.
//
// The two run concurrently because this sits on the turn's critical path,
// before the model is called.
func (a *Ambient) recall(ctx context.Context, query, current string, scope Scope) []Memory {
	if current == "" || current == query {
		return a.recallOne(ctx, query, scope)
	}
	var direct, contextual []Memory
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); direct = a.recallOne(ctx, current, scope) }()
	go func() { defer wg.Done(); contextual = a.recallOne(ctx, query, scope) }()
	wg.Wait()
	return interleave(direct, contextual, a.TopK)
}

// recallOne is best-effort like everything else on this path: a failed recall
// costs the turn its memory, never its reply.
func (a *Ambient) recallOne(ctx context.Context, query string, scope Scope) []Memory {
	mems, err := a.Engine.Recall(ctx, query, a.TopK, a.MinConfidence, scope)
	if err != nil {
		slog.Warn("memory recall failed", "error", err)
		return nil
	}
	return mems
}

// interleave merges two recalls, taking from the first list first so the
// turn's own words always hold the top slot, and dropping what both queries
// agreed on so a duplicate never costs a slot.
func interleave(first, second []Memory, limit int) []Memory {
	if limit <= 0 {
		limit = len(first) + len(second)
	}
	merged := make([]Memory, 0, limit)
	seen := make(map[string]struct{}, limit)
	add := func(m Memory) {
		if _, dup := seen[m.ID]; dup || len(merged) >= limit {
			return
		}
		seen[m.ID] = struct{}{}
		merged = append(merged, m)
	}
	for i := 0; i < len(first) || i < len(second); i++ {
		if i < len(first) {
			add(first[i])
		}
		if i < len(second) {
			add(second[i])
		}
	}
	return merged
}

// StoreExchange persists both sides of a completed turn as episodes, in the
// space the turn's channel writes to. Call it from a goroutine; it must never
// block a reply. If the memory engine is still cold-booting (first sidecar
// start downloads models), it waits for health rather than dropping the very
// first memories.
func (a *Ambient) StoreExchange(channel, audience, speaker, userText, assistantText string) {
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
	// A turn the room could hear is stored where the room can read it back.
	// The write happens whether or not recall was possible: what was said in
	// company is not a secret from anyone who was there, and dropping it
	// would lose the conversation for both of them.
	space, _ := a.scope(channel, audience)
	writeSpace := space.Space
	store := func(source, text string) {
		text = strings.TrimSpace(text)
		if text == "" || a.ignored(text) {
			return
		}
		if _, err := a.Engine.Remember(ctx, RememberRequest{Content: text, Source: source, Space: writeSpace}); err != nil {
			slog.Debug("memory store dropped", "error", err)
		}
	}
	store(SourceUser, attribute(speaker, userText))
	store(SourceAgent, assistantText)
	// The turn that ends a gathering is the moment the two spaces have
	// finished diverging, and the moment nobody is waiting on a reply.
	if a.gatheringEnded(channel, audience) {
		a.signalBridge()
	}
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
