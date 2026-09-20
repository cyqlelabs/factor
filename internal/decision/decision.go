// Package decision is the seam for typed, bounded judgements — the kind a
// System One model answers in one forward pass rather than the kind a chat
// model reasons its way to.
//
// The loop's ChatProvider is generative: it plans, writes, and recovers by
// thinking. Most of what it is asked mid-turn is not that. Which of these six
// controls to click next, whether a reply's "done" is backed by a tool
// result, whether a trajectory is worth a skill, what to do when the same
// call has returned the same page three times — every one of those is a
// choice among candidates the code already enumerated, and asking a frontier
// model to pick one costs a whole request against the conversation's context.
//
// The model that answers them runs on the user's own machine (see
// decision/local), which is what makes asking cheap enough to be worth doing
// at all: no key, no network, no bill, and an answer in the time a hosted
// round trip would have spent on DNS.
//
// A Question here is a typed choice over named candidates, answered with a
// probability per candidate and a confidence. The answer is validated, not
// trusted: a choice outside the candidate set, probabilities that do not sum
// to one, a confidence off the unit interval — any of them is a refused
// response, and a refused response executes nothing. On top of validation
// sits abstention: a decision under its kind's confidence bar, or one whose
// winner is not clear of the runner-up, is reported as unsure so the caller
// yields to the generative path it already had. The model never gains
// authority over anything: it cannot widen a tool's allow-list, a memory
// scope or a budget, and its "DONE" is a completion candidate for the code to
// verify.
//
// Two modes matter operationally. Active decisions are acted on. Shadow
// decisions are asked, recorded on the trace, and then ignored, which is how
// the thresholds get calibrated on this machine's own tasks before any of
// them changes what the agent does.
package decision

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Modes. Off is the zero value so an unconfigured install behaves exactly as
// it did before this package existed.
const (
	ModeOff    = "off"
	ModeShadow = "shadow"
	ModeActive = "active"
)

// Decision kinds. Each is calibrated separately — the cost of a wrong
// completion verdict and of a wrong click target are different sizes — and
// the trace carries the kind beside every answer so the calibration has data.
const (
	KindOperation  = "operation"
	KindTarget     = "target"
	KindCompletion = "completion"
	KindRecovery   = "recovery"
	KindInduction  = "induction"
)

// Question is one typed choice. Criteria maps a candidate id to what it
// means — a sentence, or an object describing the candidate — and
// Instructions carries the goal and the rules the choice is judged under.
type Question struct {
	Criteria     map[string]any `json:"criteria"`
	Instructions any            `json:"instructions,omitempty"`
}

// Request is one round trip: shared state and the questions asked of it.
// Several questions ride one request when they depend on the same observed
// state — the operation and a target per operation, say — so a decision cycle
// is one network hop. They are speculative: a target head that the chosen
// operation does not use is never executed.
type Request struct {
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Answer is one question's typed reply.
type Answer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// Usage is what the backend billed the request at.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is the backend's reply: one answer per question asked.
type Response struct {
	Answers map[string]Answer
	Model   string
	Usage   Usage
	Latency time.Duration
	// billed marks the usage as already reported: one request carries
	// several heads, and the tokens it cost are charged with the first
	// outcome reported against it and not again with each.
	billed bool
}

// usageOnce hands the response's usage to the first outcome that asks and
// nothing to the ones after.
func (r *Response) usageOnce() Usage {
	if r == nil || r.billed {
		return Usage{}
	}
	r.billed = true
	return r.Usage
}

// Backend answers requests: the managed local model is the one Factor runs,
// and tests script others.
type Backend interface {
	Decide(ctx context.Context, req *Request) (*Response, error)
	Name() string
}

// Limits is what one backend can be asked in a single request. A hosted
// frontier-scale model has no practical limit and reports none; a 322M
// encoder running on the user's own machine has a fixed window and a fixed
// budget for a question's options, and exceeding either is not a slightly
// worse answer but a refused request or a state truncated inside the model
// where the caller cannot see it happen.
//
// Zero means unknown, which every caller reads as "no limit": that is the
// hosted case, and it is also what a local backend reports before its first
// health probe has answered.
type Limits struct {
	// MaxCandidates is how many options one question may offer.
	MaxCandidates int
	// MaxStateChars is how much state may ride one request.
	MaxStateChars int
}

// Candidates bounds a candidate count against the limit, returning n when
// nothing is known.
func (l Limits) Candidates(n int) int {
	if l.MaxCandidates <= 0 {
		return n
	}
	return min(n, l.MaxCandidates)
}

// ClipState bounds a rendered state against the limit, taking the caller's
// own budget when it is the tighter of the two.
func (l Limits) ClipState(s string, want int) string {
	limit := want
	if l.MaxStateChars > 0 && (limit <= 0 || l.MaxStateChars < limit) {
		limit = l.MaxStateChars
	}
	if limit <= 0 {
		return s
	}
	return Clip(s, limit)
}

// Limited is the optional capability a backend declares when its window
// bounds what may be asked of it. A backend that does not implement it is
// unbounded as far as any caller is concerned.
type Limited interface {
	Limits() Limits
}

// ErrInvalid is a backend reply that failed validation. Nothing executes on
// it, and the caller falls back to whatever it did before.
var ErrInvalid = errors.New("invalid decision response")

// ErrUnavailable is a backend that did not answer in time, or at all.
var ErrUnavailable = errors.New("decision backend unavailable")

// probabilityTolerance is how far a distribution may sum from one before it
// is refused. The reference implementation uses the same slack.
const probabilityTolerance = 0.02

// Validate checks one answer against the candidate set it was asked over,
// ported from the reference implementation's validate_choice: the choice is
// a candidate, every candidate has a probability and nothing else does, the
// numbers are finite and on the unit interval, the distribution sums to one,
// and the choice carries the maximum probability. Anything less is
// ErrInvalid, because a decision that fails these is not a low-confidence
// answer but a malformed one.
func Validate(a Answer, ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("%w: no candidates", ErrInvalid)
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	if !want[a.Choice] {
		return fmt.Errorf("%w: choice %q is not a candidate", ErrInvalid, a.Choice)
	}
	if len(a.Probabilities) != len(want) {
		return fmt.Errorf("%w: %d probabilities for %d candidates", ErrInvalid, len(a.Probabilities), len(want))
	}
	sum := 0.0
	maxP := math.Inf(-1)
	for id, p := range a.Probabilities {
		if !want[id] {
			return fmt.Errorf("%w: probability for %q, which is not a candidate", ErrInvalid, id)
		}
		if !unit(p) {
			return fmt.Errorf("%w: probability %v for %q is not on [0,1]", ErrInvalid, p, id)
		}
		sum += p
		maxP = math.Max(maxP, p)
	}
	if math.Abs(sum-1) > probabilityTolerance {
		return fmt.Errorf("%w: probabilities sum to %.3f", ErrInvalid, sum)
	}
	if !unit(a.Confidence) {
		return fmt.Errorf("%w: confidence %v is not on [0,1]", ErrInvalid, a.Confidence)
	}
	if a.Probabilities[a.Choice] < maxP-1e-6 {
		return fmt.Errorf("%w: choice %q is not the most probable candidate", ErrInvalid, a.Choice)
	}
	return nil
}

func unit(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 && x <= 1
}

// Verdict is what a caller gets back from Choose: the validated answer, and
// whether it clears the bar to act on. Unsure is the abstention: a verdict
// the caller reads as "no opinion", not as the runner-up.
type Verdict struct {
	Kind          string
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
	// Unsure is set when the answer was valid but too weak to act on: under
	// the kind's confidence bar, or not clear of the second candidate.
	Unsure bool
	// Why says what held the verdict back, for the log and the trace.
	Why string
	// Shadow marks a verdict recorded but not to be acted on.
	Shadow  bool
	Model   string
	Usage   Usage
	Latency time.Duration
}

// Actionable reports whether the caller may change what it does on the
// strength of this verdict. Shadow and unsure verdicts are both no.
func (v Verdict) Actionable() bool { return !v.Shadow && !v.Unsure }

// Margin is how far the chosen candidate must sit above the runner-up. Two
// candidates neck and neck is a coin flip whatever the confidence says.
const Margin = 0.1

// Outcome is what a decision came to, in the words the trace and the bands
// count: acted, unsure, shadow, or a fallback with its reason.
type Outcome struct {
	Kind    string
	Choice  string
	Result  string // acted | unsure | shadow | fallback
	Reason  string
	Model   string
	Usage   Usage
	Latency time.Duration
}

// Decider is the gate every caller goes through: it asks the backend, validates
// the answers, applies the per-kind bar, and reports the outcome to whoever
// is counting. A nil Decider is off, and every method is nil-safe.
type Decider struct {
	backend    Backend
	mode       string
	timeout    time.Duration
	minConf    float64
	thresholds map[string]float64
	onOutcome  func(ctx context.Context, o Outcome)
}

// Options configures a Decider.
type Options struct {
	Mode string
	// Timeout bounds one request. A decision is worth having while the
	// caller is still deciding; a slow one is worse than none, because the
	// generative path it was meant to replace is already waiting behind it.
	Timeout time.Duration
	// MinConfidence is the bar a kind falls back to when Thresholds names
	// none for it.
	MinConfidence float64
	// Thresholds is the bar per kind, calibrated separately: a wrong
	// completion verdict and a wrong click are not the same size of mistake.
	Thresholds map[string]float64
}

// New wires a decider. Nil backend or an off mode yields nil, which every
// caller reads as "decide nothing".
func New(backend Backend, opts Options) *Decider {
	if backend == nil {
		return nil
	}
	switch opts.Mode {
	case ModeShadow, ModeActive:
	default:
		return nil
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 4 * time.Second
	}
	if opts.MinConfidence <= 0 {
		opts.MinConfidence = 0.6
	}
	return &Decider{backend: backend, mode: opts.Mode, timeout: opts.Timeout,
		minConf: opts.MinConfidence, thresholds: opts.Thresholds}
}

// OnOutcome is told what every decision came to — acted on, held back, or
// fallen back from — so the trace can carry the record and the bands can
// count fallbacks. The same callback carries the usage the backend billed.
func (d *Decider) OnOutcome(fn func(ctx context.Context, o Outcome)) *Decider {
	if d != nil {
		d.onOutcome = fn
	}
	return d
}

// Enabled reports whether decisions are asked at all (shadow included).
func (d *Decider) Enabled() bool { return d != nil }

// Active reports whether decisions may change what the caller does.
func (d *Decider) Active() bool { return d != nil && d.mode == ModeActive }

// Mode names the configured mode, or off.
func (d *Decider) Mode() string {
	if d == nil {
		return ModeOff
	}
	return d.mode
}

// Limits reports what the backend behind this decider can be asked in one
// request, so a caller can size what it offers instead of finding out from a
// refusal. A nil decider and a backend that declares nothing both answer the
// zero value, which means no limit.
func (d *Decider) Limits() Limits {
	if d == nil {
		return Limits{}
	}
	limited, ok := d.backend.(Limited)
	if !ok {
		return Limits{}
	}
	return limited.Limits()
}

// Backend names what answers this decider's questions, for the log and the
// status line. A nil decider names nothing.
func (d *Decider) Backend() string {
	if d == nil || d.backend == nil {
		return ""
	}
	return d.backend.Name()
}

// Threshold is the confidence bar for one kind.
func (d *Decider) Threshold(kind string) float64 {
	if d == nil {
		return 1
	}
	if t, ok := d.thresholds[kind]; ok && t > 0 {
		return t
	}
	return d.minConf
}

// Decide asks the backend one request and validates every answer against
// the candidate ids its question offered. It returns the raw validated
// answers; Judge applies the bar to one of them. Callers that ask several
// questions at once — the browser's operation and target heads — use this
// pair directly; everything else goes through Choose.
func (d *Decider) Decide(ctx context.Context, kind string, req *Request) (*Response, error) {
	if d == nil {
		return nil, ErrUnavailable
	}
	if len(req.Questions) == 0 {
		return nil, fmt.Errorf("%w: no questions", ErrInvalid)
	}
	cctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	started := time.Now()
	resp, err := d.backend.Decide(cctx, req)
	if err != nil {
		reason := "unavailable"
		if errors.Is(err, ErrInvalid) {
			reason = "invalid"
		}
		d.report(ctx, Outcome{Kind: kind, Result: "fallback", Reason: reason + ": " + err.Error(),
			Latency: time.Since(started)})
		if errors.Is(err, ErrInvalid) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.Latency == 0 {
		resp.Latency = time.Since(started)
	}
	for name, q := range req.Questions {
		a, ok := resp.Answers[name]
		if !ok {
			err := fmt.Errorf("%w: no answer to %q", ErrInvalid, name)
			d.report(ctx, Outcome{Kind: kind, Result: "fallback", Reason: err.Error(), Model: resp.Model,
				Usage: resp.usageOnce(), Latency: resp.Latency})
			return nil, err
		}
		if err := Validate(a, Candidates(q)); err != nil {
			err = fmt.Errorf("%q: %w", name, err)
			d.report(ctx, Outcome{Kind: kind, Result: "fallback", Reason: err.Error(), Model: resp.Model,
				Usage: resp.usageOnce(), Latency: resp.Latency})
			return nil, err
		}
	}
	return resp, nil
}

// Judge turns one validated answer into a verdict under the kind's bar, and
// reports the outcome. The bar has two halves: the confidence the backend
// states, and the margin between the winner and the runner-up, because a
// confident answer over two near-equal candidates is still a coin flip.
func (d *Decider) Judge(ctx context.Context, kind string, a Answer, resp *Response) Verdict {
	v := Verdict{Kind: kind, Choice: a.Choice, Confidence: a.Confidence, Probabilities: a.Probabilities,
		Shadow: d == nil || d.mode != ModeActive}
	if resp != nil {
		v.Model, v.Usage, v.Latency = resp.Model, resp.Usage, resp.Latency
	}
	if bar := d.Threshold(kind); a.Confidence < bar {
		v.Unsure = true
		v.Why = fmt.Sprintf("confidence %.2f under the %.2f bar", a.Confidence, bar)
	} else if lead := runnerUpLead(a); lead < Margin {
		v.Unsure = true
		v.Why = fmt.Sprintf("%q leads the runner-up by only %.2f", a.Choice, lead)
	}
	o := Outcome{Kind: kind, Choice: a.Choice, Model: v.Model, Usage: resp.usageOnce(), Latency: v.Latency}
	switch {
	case v.Unsure:
		o.Result, o.Reason = "unsure", v.Why
	case v.Shadow:
		o.Result = "shadow"
	default:
		o.Result = "acted"
	}
	d.report(ctx, o)
	return v
}

// Choose asks one question and judges its answer: the common case. An error
// is a fallback already reported — the caller carries on as it would have
// without a decider.
func (d *Decider) Choose(ctx context.Context, kind string, state any, q Question) (Verdict, error) {
	resp, err := d.Decide(ctx, kind, &Request{State: state, Questions: map[string]Question{kind: q}})
	if err != nil {
		return Verdict{}, err
	}
	return d.Judge(ctx, kind, resp.Answers[kind], resp), nil
}

func (d *Decider) report(ctx context.Context, o Outcome) {
	if d != nil && d.onOutcome != nil {
		d.onOutcome(ctx, o)
	}
}

// runnerUpLead is how far the chosen candidate's probability sits above the
// next one. A lone candidate leads by its whole probability.
func runnerUpLead(a Answer) float64 {
	best := a.Probabilities[a.Choice]
	second := 0.0
	for id, p := range a.Probabilities {
		if id != a.Choice && p > second {
			second = p
		}
	}
	return best - second
}

// Candidates lists a question's candidate ids, sorted so a request and its
// validation agree on the set whatever order the map iterates in.
func Candidates(q Question) []string {
	ids := make([]string, 0, len(q.Criteria))
	for id := range q.Criteria {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Clip bounds a piece of state before it rides a request. Decisions are
// priced per input token and judged from the shape of a situation, not from
// the whole page behind it.
func Clip(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
