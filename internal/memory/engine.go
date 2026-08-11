// Package memory is the soul of Factor: long-term memory backed by smrti
// (github.com/cyqlelabs/smrti), an AtomSpace-inspired engine with Bayesian
// truth values, attention economics, and emotional valence, reached over a
// localhost REST sidecar. Everything degrades gracefully: when smrti is
// unreachable, recalls come back empty and stores are dropped with a log
// line — the agent keeps working, just without its long-term memory.
package memory

import "context"

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

type RememberRequest struct {
	Content     string
	Type        string // episode | belief | goal (default episode)
	Probability float64
	Valence     *float64 // nil = smrti auto-estimates from content
	Evidence    string   // beliefs only
}

// Engine is the memory seam. The production implementation talks to smrti;
// tests use fakes; "off" mode uses Noop.
type Engine interface {
	Remember(ctx context.Context, req RememberRequest) (string, error)
	Recall(ctx context.Context, query string, topK int, minConfidence float64) ([]Memory, error)
	Forget(ctx context.Context, query, reason string) error
	Reflect(ctx context.Context) (map[string]any, error)
	Status(ctx context.Context) (map[string]any, error)
	Enabled() bool // false only for the disabled (off-mode) engine
	Healthy() bool // reachable right now
	Close() error
}

// Noop is the disabled-memory engine.
type Noop struct{}

func (Noop) Remember(context.Context, RememberRequest) (string, error) { return "", nil }
func (Noop) Recall(context.Context, string, int, float64) ([]Memory, error) {
	return nil, nil
}
func (Noop) Forget(context.Context, string, string) error    { return nil }
func (Noop) Reflect(context.Context) (map[string]any, error) { return map[string]any{}, nil }
func (Noop) Status(context.Context) (map[string]any, error) {
	return map[string]any{"mode": "off"}, nil
}
func (Noop) Enabled() bool { return false }
func (Noop) Healthy() bool { return false }
func (Noop) Close() error  { return nil }
