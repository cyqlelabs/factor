package bands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/trace"
)

// writeTraces lays down records at given ages, so a test can build a baseline
// and a recent window without waiting for either.
func writeTraces(t *testing.T, dir string, recs []trace.Record) {
	t.Helper()
	files := map[string][]byte{}
	for _, r := range recs {
		day := r.Started.Format("2006-01-02")
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		files[day] = append(files[day], append(raw, '\n')...)
	}
	for day, body := range files {
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func testWatcher(dir string, now time.Time) *Watcher {
	w := New(dir)
	w.now = func() time.Time { return now }
	return w
}

func breachFor(bs []Breach, metric string) (Breach, bool) {
	for _, b := range bs {
		if b.Metric == metric {
			return b, true
		}
	}
	return Breach{}, false
}

// The baseline is what a normal hour looks like; a run of failures against it
// is the whole point of the exercise.
func TestRisingToolErrorsBreach(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 40; i++ {
		// Baseline: a little noise, mostly clean.
		errs := 0
		if i%8 == 0 {
			errs = 1
		}
		recs = append(recs, record(now.Add(-time.Duration(24+i)*time.Hour), 4, errs))
	}
	for i := 0; i < 6; i++ {
		recs = append(recs, record(now.Add(-time.Duration(i)*time.Minute), 4, 4))
	}
	writeTraces(t, dir, recs)

	got := testWatcher(dir, now).Check()
	b, ok := breachFor(got, "tool error rate")
	if !ok {
		t.Fatalf("no breach reported: %+v", got)
	}
	if b.Sigma < 1 {
		t.Errorf("sigma = %v", b.Sigma)
	}
	if b.Recent <= b.Baseline {
		t.Errorf("recent %v should be above baseline %v", b.Recent, b.Baseline)
	}
}

// A metric moving the harmless way is not news. Errors falling is the system
// getting better, and a watcher that reported it would be a watcher nobody
// reads.
func TestFallingErrorsAreNotABreach(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 40; i++ {
		recs = append(recs, record(now.Add(-time.Duration(24+i)*time.Hour), 4, 2))
	}
	for i := 0; i < 6; i++ {
		recs = append(recs, record(now.Add(-time.Duration(i)*time.Minute), 4, 0))
	}
	writeTraces(t, dir, recs)

	if _, ok := breachFor(testWatcher(dir, now).Check(), "tool error rate"); ok {
		t.Error("fewer errors reported as a breach")
	}
}

// The cache hit rate is the one metric where a fall is the problem: it is the
// only signal that the request prefix stopped being byte-stable.
func TestFallingCacheHitRateBreaches(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 40; i++ {
		cached := 800
		if i%5 == 0 {
			cached = 700
		}
		recs = append(recs, cacheRecord(now.Add(-time.Duration(24+i)*time.Hour), 1000, cached))
	}
	for i := 0; i < 6; i++ {
		recs = append(recs, cacheRecord(now.Add(-time.Duration(i)*time.Minute), 1000, 0))
	}
	writeTraces(t, dir, recs)

	b, ok := breachFor(testWatcher(dir, now).Check(), "cache hit rate")
	if !ok {
		t.Fatal("a collapsed cache hit rate went unnoticed")
	}
	if b.Recent >= b.Baseline {
		t.Errorf("recent %v should be below baseline %v", b.Recent, b.Baseline)
	}
}

// Two turns are not a baseline. A band that fires on them is one nobody
// trusts the third time.
func TestTooFewSamplesSaysNothing(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recs := []trace.Record{
		record(now.Add(-30*time.Hour), 4, 0),
		record(now.Add(-time.Minute), 4, 4),
	}
	writeTraces(t, dir, recs)
	if got := testWatcher(dir, now).Check(); len(got) != 0 {
		t.Errorf("reported %+v from two turns", got)
	}
}

// A metric that has never moved has no band to leave, and dividing by its
// zero deviation would report everything as infinitely wrong.
func TestNoVarianceInTheBaselineSaysNothing(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 30; i++ {
		recs = append(recs, record(now.Add(-time.Duration(24+i)*time.Hour), 4, 0))
	}
	for i := 0; i < 5; i++ {
		recs = append(recs, record(now.Add(-time.Duration(i)*time.Minute), 4, 0))
	}
	writeTraces(t, dir, recs)

	for _, b := range testWatcher(dir, now).Check() {
		if b.Sigma > 1e6 {
			t.Errorf("a flat baseline produced %v", b)
		}
	}
}

func TestNoTracesIsNotAnError(t *testing.T) {
	if got := New(t.TempDir()).Check(); len(got) != 0 {
		t.Errorf("got %+v from an empty directory", got)
	}
	if got := New("").Check(); got != nil {
		t.Errorf("got %+v from no directory at all", got)
	}
	var nilWatcher *Watcher
	if got := nilWatcher.Check(); got != nil {
		t.Errorf("got %+v from a nil watcher", got)
	}
}

func TestTierCapsAtThree(t *testing.T) {
	for _, tc := range []struct {
		sigma float64
		want  int
	}{{1.2, 1}, {2.7, 2}, {3.4, 3}, {40, 3}} {
		if got := (Breach{Sigma: tc.sigma}).Tier(); got != tc.want {
			t.Errorf("Tier(%v) = %d, want %d", tc.sigma, got, tc.want)
		}
	}
}

func TestLineReadsAsASentence(t *testing.T) {
	b := Breach{Metric: "cost per turn", Unit: "USD", Recent: 0.5, Baseline: 0.05, Sigma: 4.2, Samples: 7}
	got := b.Line()
	for _, want := range []string{"cost per turn", "$0.5000", "$0.0500", "4.2σ", "7 turns"} {
		if !contains(got, want) {
			t.Errorf("Line() = %q, missing %q", got, want)
		}
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

// A turn that called no tools says nothing about the tool error rate;
// averaging a zero in would drag the baseline toward whatever quiet turns did.
func TestSpecsSkipTurnsThatCarryNoReading(t *testing.T) {
	for _, spec := range Specs() {
		if _, ok := spec.Of(trace.Record{}); ok {
			switch spec.Name {
			case "turn failures", "provider failovers", "context overflows":
				// These are true of every turn: a turn that failed over zero
				// times is a real reading of zero.
			default:
				t.Errorf("%q read a value off an empty turn", spec.Name)
			}
		}
	}
}

func record(at time.Time, tools, errs int) trace.Record {
	r := trace.Record{Started: at, Session: "cli:x", Outcome: "ok", Duration: 1, USD: 0.01,
		InputTokens: 100, CachedTokens: 50}
	for i := 0; i < tools; i++ {
		r.Tools = append(r.Tools, trace.ToolCall{Name: "read_file", Error: i < errs})
	}
	return r
}

func cacheRecord(at time.Time, input, cached int) trace.Record {
	return trace.Record{Started: at, Session: "cli:x", Outcome: "ok", Duration: 1,
		InputTokens: input, CachedTokens: cached}
}
