package decision

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

type scriptedBackend struct {
	answers map[string]Answer
	err     error
	model   string
	calls   int
	delay   time.Duration
	last    *Request
}

func (b *scriptedBackend) Decide(ctx context.Context, req *Request) (*Response, error) {
	b.calls++
	b.last = req
	if b.delay > 0 {
		select {
		case <-time.After(b.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if b.err != nil {
		return nil, b.err
	}
	return &Response{Answers: b.answers, Model: b.model, Usage: Usage{InputTokens: 40}}, nil
}

func (b *scriptedBackend) Name() string { return "scripted" }

func twoWay(choice string, p, conf float64) Answer {
	other := "b"
	if choice == "b" {
		other = "a"
	}
	return Answer{Choice: choice, Confidence: conf, Probabilities: map[string]float64{choice: p, other: 1 - p}}
}

var abQuestion = Question{Criteria: map[string]any{"a": "first", "b": "second"}}

// Validation is the whole of what stands between a malformed reply and an
// action. Every clause the reference implementation checks is checked here.
func TestValidateRefusesEveryMalformation(t *testing.T) {
	ids := []string{"a", "b"}
	good := twoWay("a", 0.8, 0.9)
	if err := Validate(good, ids); err != nil {
		t.Fatalf("a well-formed answer was refused: %v", err)
	}
	bad := map[string]Answer{
		"choice outside the set": {Choice: "z", Confidence: 0.9, Probabilities: map[string]float64{"a": 0.5, "b": 0.5}},
		"missing a candidate":    {Choice: "a", Confidence: 0.9, Probabilities: map[string]float64{"a": 1}},
		"extra candidate":        {Choice: "a", Confidence: 0.9, Probabilities: map[string]float64{"a": 0.5, "b": 0.3, "c": 0.2}},
		"does not sum to one":    {Choice: "a", Confidence: 0.9, Probabilities: map[string]float64{"a": 0.5, "b": 0.3}},
		"probability off range":  {Choice: "a", Confidence: 0.9, Probabilities: map[string]float64{"a": 1.5, "b": -0.5}},
		"confidence off range":   {Choice: "a", Confidence: 1.2, Probabilities: map[string]float64{"a": 0.8, "b": 0.2}},
		"nan confidence":         {Choice: "a", Confidence: math.NaN(), Probabilities: map[string]float64{"a": 0.8, "b": 0.2}},
		"choice not the maximum": {Choice: "a", Confidence: 0.9, Probabilities: map[string]float64{"a": 0.2, "b": 0.8}},
	}
	for name, a := range bad {
		if err := Validate(a, ids); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if err := Validate(good, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("no candidates: err = %v", err)
	}
}

func TestNewIsNilWhenOffOrBackendless(t *testing.T) {
	if d := New(nil, Options{Mode: ModeActive}); d != nil {
		t.Error("a decider with no backend should be nil")
	}
	if d := New(&scriptedBackend{}, Options{Mode: ModeOff}); d != nil {
		t.Error("mode off should yield a nil decider")
	}
	if d := New(&scriptedBackend{}, Options{Mode: "banana"}); d != nil {
		t.Error("an unknown mode should yield a nil decider")
	}
	var d *Decider
	if d.Enabled() || d.Active() || d.Mode() != ModeOff || d.Threshold("x") != 1 {
		t.Error("a nil decider must read as off everywhere")
	}
	if _, err := d.Choose(context.Background(), KindCompletion, nil, abQuestion); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil Choose err = %v", err)
	}
	v := d.Judge(context.Background(), KindCompletion, twoWay("a", 0.9, 0.9), nil)
	if v.Actionable() {
		t.Error("a nil decider's judgement must never be actionable")
	}
}

func TestChooseActsOnAConfidentClearWinner(t *testing.T) {
	var outcomes []Outcome
	b := &scriptedBackend{answers: map[string]Answer{KindCompletion: twoWay("a", 0.9, 0.85)}, model: "jev-1"}
	d := New(b, Options{Mode: ModeActive}).OnOutcome(func(_ context.Context, o Outcome) { outcomes = append(outcomes, o) })
	v, err := d.Choose(context.Background(), KindCompletion, map[string]any{"x": 1}, abQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Actionable() || v.Choice != "a" || v.Model != "jev-1" || v.Usage.InputTokens != 40 {
		t.Errorf("verdict = %+v", v)
	}
	if len(outcomes) != 1 || outcomes[0].Result != "acted" || outcomes[0].Kind != KindCompletion {
		t.Errorf("outcomes = %+v", outcomes)
	}
	if b.last == nil || b.last.Questions[KindCompletion].Criteria["a"] != "first" {
		t.Errorf("the question did not reach the backend as asked: %+v", b.last)
	}
}

func TestShadowVerdictsAreRecordedButNotActionable(t *testing.T) {
	var outcomes []Outcome
	b := &scriptedBackend{answers: map[string]Answer{KindRecovery: twoWay("b", 0.95, 0.9)}}
	d := New(b, Options{Mode: ModeShadow}).OnOutcome(func(_ context.Context, o Outcome) { outcomes = append(outcomes, o) })
	v, err := d.Choose(context.Background(), KindRecovery, nil, abQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if v.Actionable() || !v.Shadow || v.Choice != "b" {
		t.Errorf("verdict = %+v", v)
	}
	if d.Active() || d.Mode() != ModeShadow {
		t.Error("shadow mode reads as active")
	}
	if len(outcomes) != 1 || outcomes[0].Result != "shadow" {
		t.Errorf("outcomes = %+v", outcomes)
	}
}

// The bar has two halves. A confident answer under the threshold is unsure;
// so is a confident answer whose winner barely leads.
func TestUnsureUnderTheBarOrWithoutAMargin(t *testing.T) {
	cases := map[string]struct {
		answer Answer
		opts   Options
	}{
		"under the default bar":    {twoWay("a", 0.9, 0.5), Options{Mode: ModeActive}},
		"under the kind's own bar": {twoWay("a", 0.9, 0.7), Options{Mode: ModeActive, Thresholds: map[string]float64{KindTarget: 0.8}}},
		"no margin over runner-up": {twoWay("a", 0.52, 0.9), Options{Mode: ModeActive}},
	}
	for name, c := range cases {
		var outcomes []Outcome
		b := &scriptedBackend{answers: map[string]Answer{KindTarget: c.answer}}
		d := New(b, c.opts).OnOutcome(func(_ context.Context, o Outcome) { outcomes = append(outcomes, o) })
		v, err := d.Choose(context.Background(), KindTarget, nil, abQuestion)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !v.Unsure || v.Actionable() || v.Why == "" {
			t.Errorf("%s: verdict = %+v", name, v)
		}
		if len(outcomes) != 1 || outcomes[0].Result != "unsure" {
			t.Errorf("%s: outcomes = %+v", name, outcomes)
		}
	}
	d := New(&scriptedBackend{}, Options{Mode: ModeActive, MinConfidence: 0.7, Thresholds: map[string]float64{KindTarget: 0.9}})
	if d.Threshold(KindTarget) != 0.9 || d.Threshold(KindOperation) != 0.7 {
		t.Errorf("thresholds: target=%v operation=%v", d.Threshold(KindTarget), d.Threshold(KindOperation))
	}
}

func TestBackendFailuresAreReportedFallbacks(t *testing.T) {
	cases := map[string]struct {
		backend *scriptedBackend
		want    error
		reason  string
	}{
		"transport error": {&scriptedBackend{err: errors.New("dial tcp: refused")}, ErrUnavailable, "unavailable"},
		"invalid reply":   {&scriptedBackend{err: errors.New("bad json"), answers: nil}, ErrUnavailable, "unavailable"},
		"marked invalid":  {&scriptedBackend{err: ErrInvalid}, ErrInvalid, "invalid"},
		"missing answer":  {&scriptedBackend{answers: map[string]Answer{"other": twoWay("a", 0.9, 0.9)}}, ErrInvalid, "no answer"},
		"malformed answer": {&scriptedBackend{answers: map[string]Answer{KindOperation: {Choice: "z", Confidence: 0.9,
			Probabilities: map[string]float64{"a": 0.5, "b": 0.5}}}}, ErrInvalid, "not a candidate"},
		"timeout": {&scriptedBackend{delay: time.Second, answers: map[string]Answer{KindOperation: twoWay("a", 0.9, 0.9)}},
			ErrUnavailable, "unavailable"},
	}
	for name, c := range cases {
		var outcomes []Outcome
		d := New(c.backend, Options{Mode: ModeActive, Timeout: 20 * time.Millisecond}).
			OnOutcome(func(_ context.Context, o Outcome) { outcomes = append(outcomes, o) })
		_, err := d.Choose(context.Background(), KindOperation, nil, abQuestion)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
		if len(outcomes) != 1 || outcomes[0].Result != "fallback" || !contains(outcomes[0].Reason, c.reason) {
			t.Errorf("%s: outcomes = %+v", name, outcomes)
		}
	}
}

func TestDecideRefusesAnEmptyRequest(t *testing.T) {
	d := New(&scriptedBackend{}, Options{Mode: ModeActive})
	if _, err := d.Decide(context.Background(), KindOperation, &Request{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v", err)
	}
}

// Several questions ride one request and every one is validated against its
// own candidate set, because a speculative target head the operation never
// uses must still not be malformed silently.
func TestDecideValidatesEveryQuestionInTheRequest(t *testing.T) {
	b := &scriptedBackend{answers: map[string]Answer{
		"operation":    {Choice: "CLICK", Confidence: 0.9, Probabilities: map[string]float64{"CLICK": 0.9, "DONE": 0.1}},
		"click_target": {Choice: "2", Confidence: 0.8, Probabilities: map[string]float64{"1": 0.3, "2": 0.7}},
	}}
	d := New(b, Options{Mode: ModeActive})
	req := &Request{Questions: map[string]Question{
		"operation":    {Criteria: map[string]any{"CLICK": "click", "DONE": "done"}},
		"click_target": {Criteria: map[string]any{"1": "x", "2": "y"}},
	}}
	resp, err := d.Decide(context.Background(), KindOperation, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Answers["click_target"].Choice != "2" {
		t.Errorf("answers = %+v", resp.Answers)
	}
	// Break the head the operation would not even use.
	b.answers["click_target"] = Answer{Choice: "9", Confidence: 0.8, Probabilities: map[string]float64{"1": 0.3, "2": 0.7}}
	if _, err := d.Decide(context.Background(), KindOperation, req); !errors.Is(err, ErrInvalid) {
		t.Errorf("a malformed speculative head passed: %v", err)
	}
}

func TestCandidatesAndClip(t *testing.T) {
	ids := Candidates(Question{Criteria: map[string]any{"b": 1, "a": 2, "c": 3}})
	if len(ids) != 3 || ids[0] != "a" || ids[2] != "c" {
		t.Errorf("ids = %v", ids)
	}
	if got := Clip("  hello world  ", 5); got != "hello…" {
		t.Errorf("Clip = %q", got)
	}
	if got := Clip("short", 10); got != "short" {
		t.Errorf("Clip = %q", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// One request answers several heads, and its tokens are billed with the first
// outcome reported against it and never again.
func TestUsageIsBilledOncePerRequest(t *testing.T) {
	var outcomes []Outcome
	b := &scriptedBackend{answers: map[string]Answer{
		"operation":    {Choice: "CLICK", Confidence: 0.9, Probabilities: map[string]float64{"CLICK": 0.9, "DONE": 0.1}},
		"click_target": {Choice: "2", Confidence: 0.8, Probabilities: map[string]float64{"1": 0.3, "2": 0.7}},
	}}
	d := New(b, Options{Mode: ModeActive}).OnOutcome(func(_ context.Context, o Outcome) { outcomes = append(outcomes, o) })
	req := &Request{Questions: map[string]Question{
		"operation":    {Criteria: map[string]any{"CLICK": "click", "DONE": "done"}},
		"click_target": {Criteria: map[string]any{"1": "x", "2": "y"}},
	}}
	resp, err := d.Decide(context.Background(), KindOperation, req)
	if err != nil {
		t.Fatal(err)
	}
	d.Judge(context.Background(), KindOperation, resp.Answers["operation"], resp)
	d.Judge(context.Background(), KindTarget, resp.Answers["click_target"], resp)
	billed := 0
	for _, o := range outcomes {
		billed += o.Usage.InputTokens
	}
	if len(outcomes) != 2 || billed != 40 {
		t.Errorf("outcomes = %+v (billed %d tokens, want 40 once)", outcomes, billed)
	}
	var none *Response
	if none.usageOnce() != (Usage{}) {
		t.Error("a nil response has no usage")
	}
}
