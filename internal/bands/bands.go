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
}

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
		},
		{
			Name: "turn failures", Unit: "of turns", Dir: Above,
			Of: func(r trace.Record) (float64, bool) {
				if r.Failed() {
					return 1, true
				}
				return 0, true
			},
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
