// Package bands watches Factor's own numbers and says when one has drifted.
//
// The heartbeat is a model call on a timer, gated on whether the user wrote
// something task-shaped in HEARTBEAT.md. That gate has the right instinct —
// nothing to do, nothing to spend — but it can only notice what somebody
// thought to write down in advance, which is never the thing that actually
// went wrong. Meanwhile the trace records everything worth noticing and
// nobody reads it.
//
// So detection here is deterministic and no model is involved in it: mean and
// standard deviation over a rolling baseline, compared against the last hour,
// per metric, with a direction because a falling cache hit rate and a rising
// error rate are both bad and only one of them is a rise. A model is spent
// only once a band has actually broken, and then on diagnosing something real
// rather than on confirming that nothing changed.
//
// The tiers are deliberately shallow for a single-user agent: notice it, tell
// the model about it on the next heartbeat, tell the user. Nothing here acts
// on its own.
package bands

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/cyqlelabs/factor/internal/trace"
)

// Direction says which way a metric has to move to be a problem.
type Direction int

const (
	// Above: a rise is the problem — errors, spend, latency.
	Above Direction = iota
	// Below: a fall is the problem — the cache hit rate, which is the only
	// signal that the request prefix stopped being byte-stable.
	Below
)

// Spec is one watched number: how to read it off a turn, and which way is bad.
type Spec struct {
	Name string
	Unit string
	Dir  Direction
	// Of reads the metric from one turn. The second return is false when the
	// turn does not carry it — a turn that called no tools says nothing about
	// the tool error rate, and averaging a zero in would drag the baseline
	// toward whatever the quiet turns did.
	Of func(trace.Record) (float64, bool)
	// Evidence gathers what stood behind a breach, from every record in the
	// baseline and the recent window (cut is where the two meet). Nil for a
	// metric that is only a number.
	Evidence func(records []trace.Record, cut time.Time) Evidence
}

// Evidence is what a breach can be diagnosed from. A rate names no tool and
// no cause: the model handed one on its own went looking in the journal,
// which holds no tool outcomes, and called a feature that had never once
// worked "transient". So a breach carries the calls that failed, with what
// they said, and each failing tool's record across the whole baseline —
// which is what tells an hour's bad luck from a fault that is always there.
type Evidence struct {
	// Since is where the baseline starts, so the history can be dated.
	Since time.Time
	// Failures are the tool calls that failed in the recent window, newest
	// last, bounded.
	Failures []Failure
	// History is the baseline-wide record of every tool in Failures, the
	// worst first.
	History []ToolRecord
	// Turns are the turns that ended in error in the recent window.
	Turns []TurnFailure
}

// Failure is one tool call that failed.
type Failure struct {
	At      time.Time
	Session string
	Tool    string
	Fault   string
}

// ToolRecord is one tool's history: how often it ran, how often it failed,
// and when it first and last did.
type ToolRecord struct {
	Tool        string
	Calls       int
	Fails       int
	First, Last time.Time
}

// TurnFailure is one turn that ended in error.
type TurnFailure struct {
	At      time.Time
	Session string
	Error   string
}

// Bounds on the evidence a breach carries. The point is the cause, and eight
// failures name it or nothing will.
const (
	maxFailures = 8
	maxTurns    = 5
)

// Specs is what Factor watches about itself. Every one of these is already
// recorded and, until now, read by nobody.
func Specs() []Spec {
	return []Spec{
		{
			Name: "tool error rate", Unit: "of calls", Dir: Above,
			Of: func(r trace.Record) (float64, bool) {
				if len(r.Tools) == 0 {
					return 0, false
				}
				return float64(r.ToolErrors()) / float64(len(r.Tools)), true
			},
			Evidence: toolEvidence,
		},
		{
			Name: "turn failures", Unit: "of turns", Dir: Above,
			Of: func(r trace.Record) (float64, bool) {
				if r.Failed() {
					return 1, true
				}
				return 0, true
			},
			Evidence: turnEvidence,
		},
		{
			Name: "cost per turn", Unit: "USD", Dir: Above,
			Of: func(r trace.Record) (float64, bool) {
				if r.USD <= 0 {
					return 0, false // unpriced models say nothing about spend
				}
				return r.USD, true
			},
		},
		{
			Name: "seconds per turn", Unit: "s", Dir: Above,
			Of: func(r trace.Record) (float64, bool) {
				if r.Duration <= 0 {
					return 0, false
				}
				return r.Duration, true
			},
		},
		{
			Name: "provider failovers", Unit: "per turn", Dir: Above,
			Of: func(r trace.Record) (float64, bool) {
				return float64(r.Count(trace.EventFailover)), true
			},
		},
		{
			Name: "context overflows", Unit: "per turn", Dir: Above,
			Of: func(r trace.Record) (float64, bool) {
				return float64(r.Count(trace.EventOverflow)), true
			},
		},
		{
			// The one that only falls. Everything that keeps the request
			// prefix stable fails silently; this ratio is what moves.
			Name: "cache hit rate", Unit: "of input", Dir: Below,
			Of: func(r trace.Record) (float64, bool) {
				if r.InputTokens <= 0 {
					return 0, false
				}
				return r.CacheHitRate(), true
			},
		},
	}
}

// Breach is one band that has broken.
type Breach struct {
	Metric   string
	Unit     string
	Recent   float64
	Baseline float64
	Sigma    float64
	Samples  int
	Evidence Evidence
}

// Tier is how far out the reading is, in whole standard deviations, capped at
// three. It is what decides whether a breach is worth a log line, a mention to
// the model on the next heartbeat, or the user's attention.
func (b Breach) Tier() int {
	t := int(b.Sigma)
	if t > 3 {
		return 3
	}
	return t
}

// Line renders the breach the way a person reads it: what moved, from what to
// what, and how unusual that is.
func (b Breach) Line() string {
	return fmt.Sprintf("%s is %s (was %s), %.1fσ from the baseline over %d turns",
		b.Metric, format(b.Recent, b.Unit), format(b.Baseline, b.Unit), b.Sigma, b.Samples)
}

// Details renders the evidence, one line each: the failures as they
// happened, then each failing tool's record over the baseline.
func (b Breach) Details() []string {
	e := b.Evidence
	out := make([]string, 0, len(e.Failures)+len(e.History)+len(e.Turns))
	for _, f := range e.Failures {
		line := fmt.Sprintf("%s %s %s failed", f.At.Format("2006-01-02 15:04"), f.Session, f.Tool)
		if f.Fault != "" {
			line += ": " + f.Fault
		}
		out = append(out, line)
	}
	for _, h := range e.History {
		out = append(out, fmt.Sprintf("%s has failed %d of %d calls since %s (first %s, last %s)",
			h.Tool, h.Fails, h.Calls, e.Since.Format("2006-01-02"),
			h.First.Format("2006-01-02 15:04"), h.Last.Format("2006-01-02 15:04")))
	}
	for _, t := range e.Turns {
		line := fmt.Sprintf("%s %s turn failed", t.At.Format("2006-01-02 15:04"), t.Session)
		if t.Error != "" {
			line += ": " + t.Error
		}
		out = append(out, line)
	}
	return out
}

// toolEvidence lists the recent failed calls and the baseline-wide record of
// every tool among them.
func toolEvidence(records []trace.Record, cut time.Time) Evidence {
	var e Evidence
	failing := map[string]bool{}
	for _, r := range records {
		if r.Started.Before(cut) {
			continue
		}
		for _, t := range r.Tools {
			if t.Error {
				e.Failures = append(e.Failures, Failure{At: r.Started, Session: r.Session, Tool: t.Name, Fault: t.Fault})
				failing[t.Name] = true
			}
		}
	}
	if len(e.Failures) > maxFailures {
		e.Failures = e.Failures[len(e.Failures)-maxFailures:]
	}
	history := map[string]*ToolRecord{}
	for _, r := range records {
		for _, t := range r.Tools {
			if !failing[t.Name] {
				continue
			}
			h := history[t.Name]
			if h == nil {
				h = &ToolRecord{Tool: t.Name}
				history[t.Name] = h
			}
			h.Calls++
			if t.Error {
				h.Fails++
				if h.First.IsZero() {
					h.First = r.Started
				}
				h.Last = r.Started
			}
		}
	}
	for _, h := range history {
		e.History = append(e.History, *h)
	}
	sort.Slice(e.History, func(i, j int) bool {
		if e.History[i].Fails != e.History[j].Fails {
			return e.History[i].Fails > e.History[j].Fails
		}
		return e.History[i].Tool < e.History[j].Tool
	})
	return e
}

// turnEvidence lists the recent turns that ended in error, with what they
// died of.
func turnEvidence(records []trace.Record, cut time.Time) Evidence {
	var e Evidence
	for _, r := range records {
		if r.Started.Before(cut) || !r.Failed() {
			continue
		}
		e.Turns = append(e.Turns, TurnFailure{At: r.Started, Session: r.Session, Error: r.Error})
	}
	if len(e.Turns) > maxTurns {
		e.Turns = e.Turns[len(e.Turns)-maxTurns:]
	}
	return e
}

func format(v float64, unit string) string {
	switch unit {
	case "USD":
		return fmt.Sprintf("$%.4f", v)
	case "of calls", "of turns", "of input":
		return fmt.Sprintf("%.0f%%", v*100)
	case "s":
		return fmt.Sprintf("%.1fs", v)
	}
	return fmt.Sprintf("%.2f %s", v, unit)
}

// Watcher compares a recent window against a rolling baseline.
type Watcher struct {
	dir      string
	specs    []Spec
	baseline time.Duration
	recent   time.Duration
	// minSamples is the floor under saying anything. Two turns are not a
	// baseline, and a band that fires on them is a band nobody will trust
	// the third time.
	minSamples int
	now        func() time.Time
}

// New builds a watcher over a trace directory with the defaults.
func New(dir string) *Watcher {
	return &Watcher{
		dir:        dir,
		specs:      Specs(),
		baseline:   7 * 24 * time.Hour,
		recent:     time.Hour,
		minSamples: 10,
		now:        time.Now,
	}
}

// Check reads the traces and returns the broken bands, worst first. It is
// entirely deterministic: no model is asked anything here, which is the point.
// An unreadable or empty trace directory is not an error — it means nothing
// is known yet, and nothing is claimed.
func (w *Watcher) Check() []Breach {
	if w == nil || w.dir == "" {
		return nil
	}
	now := w.now()
	records, err := trace.Since(w.dir, now.Add(-w.baseline))
	if err != nil || len(records) == 0 {
		return nil
	}
	cut := now.Add(-w.recent)

	var out []Breach
	for _, spec := range w.specs {
		var base, recent []float64
		for _, r := range records {
			v, ok := spec.Of(r)
			if !ok {
				continue
			}
			if r.Started.Before(cut) {
				base = append(base, v)
			} else {
				recent = append(recent, v)
			}
		}
		if b, ok := judge(spec, base, recent, w.minSamples); ok {
			if spec.Evidence != nil {
				b.Evidence = spec.Evidence(records, cut)
				b.Evidence.Since = now.Add(-w.baseline)
			}
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sigma > out[j].Sigma })
	return out
}

// judge decides whether the recent window has left the baseline's band.
func judge(spec Spec, base, recent []float64, minSamples int) (Breach, bool) {
	if len(base) < minSamples || len(recent) == 0 {
		return Breach{}, false
	}
	mean, sd := stats(base)
	if sd <= 0 {
		// No variance in the baseline means no band to leave. A metric that
		// has never moved cannot be said to have moved unusually, and
		// dividing by zero would report every metric as infinitely wrong.
		return Breach{}, false
	}
	got, _ := stats(recent)
	delta := got - mean
	if spec.Dir == Below {
		delta = -delta
	}
	if delta <= 0 {
		return Breach{}, false // moved the harmless way
	}
	sigma := delta / sd
	if sigma < 1 {
		return Breach{}, false
	}
	return Breach{
		Metric: spec.Name, Unit: spec.Unit,
		Recent: got, Baseline: mean, Sigma: sigma, Samples: len(recent),
	}, true
}

// stats returns the mean and the population standard deviation.
func stats(xs []float64) (mean, sd float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	for _, x := range xs {
		d := x - mean
		sd += d * d
	}
	return mean, math.Sqrt(sd / float64(len(xs)))
}
