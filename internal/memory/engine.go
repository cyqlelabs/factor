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
// it reads, keyed by the channel the turn arrived on. Real conversations
// (cli, telegram, phone) write to Main; machine-originated turns (cron, jobs,
// heartbeat) write to System so operational chatter stops crowding
// conversational recall — and each side still reads the other as an overlay.
// Strategy "single" (or a zero policy) turns the split off.
type SpacePolicy struct {
	Strategy string // "origin" (default) or "single"
	Main     string
	System   string
}

// NewSpacePolicy validates the configured strategy. An unrecognised value is
// an error rather than a silent fallback: defaulting it to origin would turn
// the split on for someone who wrote "single" with a typo, which is the
// opposite of what they asked for.
func NewSpacePolicy(strategy, main, system string) (SpacePolicy, error) {
	switch strategy {
	case "", "origin", "single":
		return SpacePolicy{Strategy: strategy, Main: main, System: system}, nil
	default:
		return SpacePolicy{}, fmt.Errorf("unknown space_strategy %q (want origin or single)", strategy)
	}
}

func (p SpacePolicy) Scope(channel string) Scope {
	if p.Strategy == "single" || p.Main == "" || p.System == "" {
		return Scope{}
	}
	switch channel {
	case "cron", "job", "system":
		return Scope{Space: p.System, ReadSpaces: []string{p.System, p.Main}}
	default:
		return Scope{Space: p.Main, ReadSpaces: []string{p.Main, p.System}}
	}
}

// scopeFor resolves a turn's scope against what the engine can actually do.
// Splitting spaces is only safe when the engine both routes them and already
// writes to the space this policy calls Main: an engine writing elsewhere
// (an external smrti configured with its own SMRTI_SPACE) would see every
// routed write land beside the graph it has been building, so the split is
// abandoned in favour of the engine's own space.
func scopeFor(engine Engine, p SpacePolicy, channel string) Scope {
	ok, engineSpace := engine.SpaceSupport()
	if !ok || engineSpace != p.Main {
		return Scope{}
	}
	return p.Scope(channel)
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
