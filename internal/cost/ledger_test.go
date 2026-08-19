package cost

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func atClock(l *Ledger, t time.Time) { l.now = func() time.Time { return t } }

func TestLedgerBillsEveryBucketAndSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	l := NewLedger(path)
	day := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	atClock(l, day)
	l.Record("cli:main", "a/model", Totals{Input: 1000, Output: 500, USD: 0.02, Calls: 1})
	l.Record("cli:main", "a/model", Totals{Input: 500, Output: 100, USD: 0.01, Calls: 1})
	l.Record("telegram:7", "b/model", Totals{Input: 10, Output: 10, Calls: 1})

	s := l.Snapshot("cli:main")
	if s.Session.Calls != 2 || s.Session.Input != 1500 || s.Session.USD != 0.03 {
		t.Errorf("session totals = %+v", s.Session)
	}
	if s.Day.Calls != 3 || s.Month.Calls != 3 || s.Total.Calls != 3 {
		t.Errorf("day/month/total = %+v %+v %+v", s.Day, s.Month, s.Total)
	}
	if s.Models["a/model"].USD != 0.03 || s.Models["b/model"].Tokens() != 20 {
		t.Errorf("per-model totals = %+v", s.Models)
	}
	if s.Sessions["telegram:7"].Calls != 1 {
		t.Errorf("per-session totals = %+v", s.Sessions)
	}

	// A later day opens its own bucket while the month and the total keep counting.
	next := NewLedger(path)
	atClock(next, day.AddDate(0, 0, 1))
	next.Record("cli:main", "a/model", Totals{Input: 1, Output: 1, USD: 0.5, Calls: 1})
	s = next.Snapshot("cli:main")
	if s.Day.Calls != 1 || s.Month.Calls != 4 || s.Total.Calls != 4 {
		t.Errorf("after a day boundary: day=%+v month=%+v total=%+v", s.Day, s.Month, s.Total)
	}
	if s.Session.USD != 0.53 {
		t.Errorf("session spend did not survive the reopen: %+v", s.Session)
	}
}

func TestLedgerMergesRatherThanOverwritingAnotherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	gateway, terminal := NewLedger(path), NewLedger(path)
	gateway.Record("telegram:7", "m", Totals{Input: 100, USD: 1, Calls: 1})
	terminal.Record("cli:main", "m", Totals{Input: 100, USD: 2, Calls: 1})

	// The terminal read the gateway's write before adding its own.
	if got := terminal.Snapshot("").Total; got.USD != 3 || got.Calls != 2 {
		t.Errorf("merged total = %+v, want both processes counted", got)
	}
	if got := NewLedger(path).Snapshot("telegram:7").Session; got.USD != 1 {
		t.Errorf("the gateway's session was overwritten: %+v", got)
	}
}

func TestLedgerKeepsPendingCallsWhenTheWriteFails(t *testing.T) {
	l := NewLedger(filepath.Join(t.TempDir(), "nope", "usage.json"))
	l.Record("cli:main", "m", Totals{Input: 10, USD: 1, Calls: 1})
	if len(l.pending) != 1 {
		t.Errorf("a failed write dropped the call: pending=%d", len(l.pending))
	}
	// In-memory numbers are still right, so the status bar does not lie.
	if got := l.Snapshot("cli:main").Session; got.USD != 1 {
		t.Errorf("session totals after a failed write = %+v", got)
	}
	if err := l.Flush(); err == nil {
		t.Error("flushing to an unwritable path reported success")
	}
	// The warning is said once, not on every call for as long as the disk
	// stays broken.
	if !l.warned {
		t.Error("a ledger that cannot write said nothing about it")
	}
	l.Record("cli:main", "m", Totals{Input: 10, USD: 1, Calls: 1})
	if len(l.pending) != 2 {
		t.Errorf("pending = %d, want both calls held", len(l.pending))
	}
}

func TestLedgerWithoutAPathCountsButPersistsNothing(t *testing.T) {
	l := NewLedger("")
	l.Record("cli:main", "m", Totals{Input: 10, USD: 1, Calls: 1})
	if got := l.Snapshot("cli:main").Session; got.USD != 1 {
		t.Errorf("in-memory totals = %+v", got)
	}
	if err := l.Flush(); err != nil {
		t.Errorf("flush without a path: %v", err)
	}
}

func TestLedgerStartsCleanOnACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := NewLedger(path)
	if got := l.Snapshot("").Total; got != (Totals{}) {
		t.Errorf("a corrupt ledger produced totals: %+v", got)
	}
	l.Record("cli:main", "m", Totals{Input: 1, Calls: 1})
	if got := NewLedger(path).Snapshot("").Total.Calls; got != 1 {
		t.Errorf("the file was not rewritten cleanly: calls=%d", got)
	}
}

func TestLedgerPrunesWhatNobodyAsksAbout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	l := NewLedger(path)
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < keepDays+10; i++ {
		atClock(l, start.AddDate(0, 0, i))
		l.Record(fmt.Sprintf("cli:s%d", i), "m", Totals{Input: 1, Calls: 1})
	}
	for i := 0; i < keepSessions+5; i++ {
		l.Record(fmt.Sprintf("cli:extra%d", i), "m", Totals{Input: 1, Calls: 1})
	}
	var b book
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Days) != keepDays {
		t.Errorf("days kept = %d, want %d", len(b.Days), keepDays)
	}
	if len(b.Months) > keepMonths {
		t.Errorf("months kept = %d, want at most %d", len(b.Months), keepMonths)
	}
	if len(b.Sessions) != keepSessions {
		t.Errorf("sessions kept = %d, want %d", len(b.Sessions), keepSessions)
	}
	// The all-time total is never pruned: it counts every call ever billed.
	if b.Total.Calls != keepDays+10+keepSessions+5 {
		t.Errorf("total calls = %d", b.Total.Calls)
	}
	// The newest sessions are the ones that survived.
	if _, ok := b.Sessions[fmt.Sprintf("cli:extra%d", keepSessions+4)]; !ok {
		t.Error("the most recent session was pruned")
	}
}

func TestLedgerPrunesMonthsOnce24AreOnFile(t *testing.T) {
	l := NewLedger(filepath.Join(t.TempDir(), "usage.json"))
	start := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < keepMonths+6; i++ {
		atClock(l, start.AddDate(0, i, 0))
		l.Record("cli:main", "m", Totals{Input: 1, Calls: 1})
	}
	if got := len(l.book.Months); got != keepMonths {
		t.Errorf("months kept = %d, want %d", got, keepMonths)
	}
}

func TestSpentPicksTheBucketThePeriodNames(t *testing.T) {
	s := Snapshot{
		Day:   Totals{USD: 1},
		Month: Totals{USD: 2},
		Total: Totals{USD: 3},
	}
	for period, want := range map[string]float64{"day": 1, "month": 2, "total": 3, "": 2} {
		if got := s.Spent(period).USD; got != want {
			t.Errorf("Spent(%q) = %v, want %v", period, got, want)
		}
	}
}
