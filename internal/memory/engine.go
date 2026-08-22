// Package memory is the soul of Factor: long-term memory backed by smrti
// (github.com/cyqlelabs/smrti), an AtomSpace-inspired engine with Bayesian
// truth values, attention economics, and emotional valence, reached over a
// localhost REST sidecar. Everything degrades gracefully: when smrti is
// unreachable, recalls come back empty and stores are dropped with a log
// line — the agent keeps working, just without its long-term memory.
package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

type Severity string

const (
	SeverityCriticalWarning  Severity = "critical_warning"
	SeverityKnownAntipattern Severity = "known_antipattern"
	SeverityContext          Severity = "context"
)

type Memory struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Content     string   `json:"content"`
	Type        string   `json:"type"`
	Probability float64  `json:"probability"`
	Confidence  float64  `json:"confidence"`
	STI         float64  `json:"sti"`
	LTI         float64  `json:"lti"`
	Valence     float64  `json:"valence"`
	Intensity   float64  `json:"intensity"`
	Severity    Severity `json:"severity"`
	Salience    float64  `json:"salience"`
	Similarity  float64  `json:"similarity"`
	Space       string   `json:"space"`
}

// Source values for RememberRequest. Anything the assistant authored must be
// marked SourceAgent: smrti extracts those turns conservatively, weights them
// below what the user said, and lets them decay unless the user picks them up.
// An unmarked reply is stored as if the user had stated it, so a single
// suggestion-laden answer mints dozens of permanent entities.
const (
	SourceUser  = "user"
	SourceAgent = "agent"
)

type RememberRequest struct {
	Content     string
	Type        string // episode | belief | goal (default episode)
	Probability float64
	Valence     *float64 // nil = smrti auto-estimates from content
	Evidence    string   // beliefs only
	Source      string   // user | agent (empty means user)
	Space       string   // memory space to write to (empty = engine default)
}

// Scope names the space a call writes to and the overlay a recall reads.
// The zero value means "the engine's configured default"; with it every
// request payload stays byte-identical to pre-space builds. The client drops
// the fields entirely when the engine has not advertised space support, so a
// non-zero Scope against an old engine degrades to today's single space
// instead of silently misrouting.
type Scope struct {
	Space      string
	ReadSpaces []string
}

// SpacePolicy decides which memory space a turn writes to and which overlay
// it reads, keyed by the channel the turn arrived on and by who can hear the
// reply. Real conversations (cli, telegram, phone) write to Main;
// machine-originated turns (cron, jobs, heartbeat) write to System so
// operational chatter stops crowding conversational recall — and each side
// still reads the other as an overlay. Strategy "single" (or a zero policy)
// turns the split off.
//
// Shared is the one overlay that is deliberately one-directional. A turn
// somebody else can hear writes there and reads only there, so nothing said
// in private is spoken back into a room with a guest in it; a private turn
// reads Shared too, because the user was there for all of it and should not
// have to be alone to remember their own conversation. That asymmetry is the
// whole feature, and the engine enforces the half that matters: a space reads
// its entire overlay but only ever mutates its own write space, so a private
// turn physically cannot write back into Shared.
type SpacePolicy struct {
	Strategy string // "origin" (default) or "single"
	Main     string
	System   string
	// Shared holds what was said with company present. Empty means the split
	// is unavailable, which callers must read as "cannot isolate" rather than
	// as "isolate into Main".
	Shared string
}

// NewSpacePolicy validates the configured strategy. An unrecognised value is
// an error rather than a silent fallback: defaulting it to origin would turn
// the split on for someone who wrote "single" with a typo, which is the
// opposite of what they asked for.
func NewSpacePolicy(strategy, main, system, shared string) (SpacePolicy, error) {
	switch strategy {
	case "", "origin", "single":
	default:
		return SpacePolicy{}, fmt.Errorf("unknown space_strategy %q (want origin or single)", strategy)
	}
	// Two spaces sharing one name is not a partition, and the case that
	// silently costs privacy is shared == main: every guest turn would read
	// and write the private graph while the config says it is isolated.
	if shared != "" && (shared == main || shared == system) {
		return SpacePolicy{}, fmt.Errorf("shared_space %q must differ from space and system_space", shared)
	}
	return SpacePolicy{Strategy: strategy, Main: main, System: system, Shared: shared}, nil
}

// Scope resolves the space a turn writes to and the overlay it reads. It
// reports ok=false when the turn must not recall at all — an audience this
// policy cannot isolate. Serving such a turn from the one space that holds
// everything is precisely the leak the split exists to prevent, so recall is
// skipped instead of quietly widened.
func (p SpacePolicy) Scope(channel, audience string) (Scope, bool) {
	if p.Strategy == "single" || p.Main == "" || p.System == "" {
		return Scope{}, audience != tools.AudienceShared
	}
	switch channel {
	case "cron", "job", "system":
		return Scope{Space: p.System, ReadSpaces: []string{p.System, p.Main}}, true
	}
	if audience == tools.AudienceShared {
		if p.Shared == "" {
			return Scope{}, false
		}
		return Scope{Space: p.Shared, ReadSpaces: []string{p.Shared}}, true
	}
	read := []string{p.Main, p.System}
	if p.Shared != "" {
		read = []string{p.Main, p.Shared, p.System}
	}
	return Scope{Space: p.Main, ReadSpaces: read}, true
}

// scopeFor resolves a turn's scope against what the engine can actually do.
// Splitting spaces is only safe when the engine both routes them and already
// writes to the space this policy calls Main: an engine writing elsewhere
// (an external smrti configured with its own SMRTI_SPACE) would see every
// routed write land beside the graph it has been building, so the split is
// abandoned in favour of the engine's own space.
func scopeFor(engine Engine, p SpacePolicy, channel, audience string) (Scope, bool) {
	ok, engineSpace := engine.SpaceSupport()
	if !ok || engineSpace != p.Main {
		// No routing means one space holding everything. A private turn is
		// served from it exactly as before spaces existed; a shared one
		// cannot be, since reading it would hand a guest the private graph.
		return Scope{}, audience != tools.AudienceShared
	}
	return p.Scope(channel, audience)
}

// Engine is the memory seam. The production implementation talks to smrti;
// tests use fakes; "off" mode uses Noop.
type Engine interface {
	Remember(ctx context.Context, req RememberRequest) (string, error)
	Recall(ctx context.Context, query string, topK int, minConfidence float64, scope Scope) ([]Memory, error)
	Forget(ctx context.Context, query, reason, space string) error
	Reflect(ctx context.Context) (map[string]any, error)
	Status(ctx context.Context) (map[string]any, error)
	Enabled() bool // false only for the disabled (off-mode) engine
	Healthy() bool // reachable right now
	// SpaceSupport reports whether the engine routes per-request memory
	// spaces and, when it does, the space it writes to by default. Both come
	// from the last status probe, so before the first probe it reads as no
	// support — space fields are then omitted and behavior matches an engine
	// that never had them.
	SpaceSupport() (bool, string)
	Close() error
}

// UpgradeQuiet is how long the graph must have gone untouched before the
// engine may be restarted under a running Factor. Long enough that a turn's
// trailing store has landed, short enough that an idle minute offers a window.
const UpgradeQuiet = 15 * time.Second

// IdleFunc adapts an engine to the gate the upgrade path waits on. An engine
// that cannot report activity — Noop, a test fake — reads as idle: there is
// nothing of its own to interrupt.
func IdleFunc(e Engine, quiet time.Duration) func() bool {
	idler, ok := e.(interface{ Idle(time.Duration) bool })
	if !ok {
		return func() bool { return true }
	}
	return func() bool { return idler.Idle(quiet) }
}

// Noop is the disabled-memory engine.
type Noop struct{}

func (Noop) Remember(context.Context, RememberRequest) (string, error) { return "", nil }
func (Noop) Recall(context.Context, string, int, float64, Scope) ([]Memory, error) {
	return nil, nil
}
func (Noop) Forget(context.Context, string, string, string) error { return nil }
func (Noop) Reflect(context.Context) (map[string]any, error)      { return map[string]any{}, nil }
func (Noop) Status(context.Context) (map[string]any, error) {
	return map[string]any{"mode": "off"}, nil
}
func (Noop) Enabled() bool                { return false }
func (Noop) Healthy() bool                { return false }
func (Noop) SpaceSupport() (bool, string) { return false, "" }
func (Noop) Close() error                 { return nil }
