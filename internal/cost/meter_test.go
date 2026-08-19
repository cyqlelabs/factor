package cost

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

type fakeChat struct {
	calls int
	resp  *provider.Response
	err   error
}

func (f *fakeChat) Chat(context.Context, *provider.Request) (*provider.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// pricedCatalog charges $1 per 1k prompt tokens and $2 per 1k completion
// tokens for "a/model", so the arithmetic in a test is readable.
func pricedCatalog() *Catalog {
	return NewCatalog(config.CostConfig{
		Prices: map[string]config.Price{"a/model": {Input: 1000, Output: 2000}},
	}, []config.Candidate{{Type: "openrouter", Model: "a/model"}}, "")
}

func answered(model string, in, out int) *provider.Response {
	return &provider.Response{
		Content: "done",
		Model:   model,
		Usage:   provider.Usage{PromptTokens: in, CompletionTokens: out},
	}
}

func meterFor(t *testing.T, cfg config.CostConfig, resp *provider.Response) (*Meter, *fakeChat) {
	t.Helper()
	inner := &fakeChat{resp: resp}
	return NewMeter(inner, pricedCatalog(), NewLedger(filepath.Join(t.TempDir(), "usage.json")), cfg), inner
}

func inSession(key string) context.Context {
	return tools.WithToolContext(context.Background(), tools.ToolContext{SessionKey: key})
}

func TestMeterBillsTheSessionThatMadeTheCall(t *testing.T) {
	m, inner := meterFor(t, config.CostConfig{Track: true}, answered("a/model", 1000, 500))
	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot("cli:main")
	if s.Session.USD != 2 || s.Session.Input != 1000 || s.Session.Calls != 1 {
		t.Errorf("session totals = %+v, want $2 for 1000 in / 500 out", s.Session)
	}
	if s.Total.USD != 2 || s.Models["a/model"].Calls != 1 {
		t.Errorf("global totals = %+v models=%+v", s.Total, s.Models)
	}
	if inner.calls != 1 {
		t.Errorf("inner calls = %d", inner.calls)
	}
}

func TestMeterCountsTokensItCannotPrice(t *testing.T) {
	m, _ := meterFor(t, config.CostConfig{Track: true}, answered("nobody/lists-this", 100, 100))
	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot("cli:main")
	if s.Session.Tokens() != 200 || s.Session.USD != 0 {
		t.Errorf("unpriced totals = %+v, want tokens without money", s.Session)
	}
	if got := m.Unpriced(s); len(got) != 1 || got[0] != "nobody/lists-this" {
		t.Errorf("unpriced models = %v", got)
	}
}

func TestMeterStaysOutOfTheWayWhenNothingIsCounted(t *testing.T) {
	m, inner := meterFor(t, config.CostConfig{}, answered("a/model", 1000, 500))
	if m.Active() {
		t.Fatal("a meter with tracking off and no cap reported itself active")
	}
	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Errorf("inner calls = %d", inner.calls)
	}
	if got := m.Snapshot("cli:main").Total; got != (Totals{}) {
		t.Errorf("an inactive meter billed %+v", got)
	}
	if m.SessionLine("cli:main") != "" || m.OverviewLine() != "" {
		t.Errorf("an inactive meter drew status text: %q / %q", m.SessionLine("cli:main"), m.OverviewLine())
	}
	if !strings.Contains(m.Report("cli:main"), "off") {
		t.Errorf("report = %q", m.Report("cli:main"))
	}
	if got := NewTool(m); got != nil {
		t.Errorf("an inactive meter registered %d tools", len(got))
	}
}

func TestMeterStaysActiveForACapEvenWithTrackingOff(t *testing.T) {
	m, _ := meterFor(t, config.CostConfig{Budget: config.BudgetConfig{SessionUSD: 5, Period: "month"}},
		answered("a/model", 1000, 500))
	if !m.Active() {
		t.Error("a cap with tracking off counts nothing and can never fire")
	}
}

func TestMeterBillsNothingForACallThatFailedOrReportedNothing(t *testing.T) {
	m, _ := meterFor(t, config.CostConfig{Track: true}, answered("a/model", 0, 0))
	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot("cli:main").Total; got != (Totals{}) {
		t.Errorf("a usage-free response billed %+v", got)
	}

	boom := errors.New("upstream is down")
	failing, _ := meterFor(t, config.CostConfig{Track: true}, nil)
	failing.inner.(*fakeChat).err = boom
	if _, err := failing.Chat(inSession("cli:main"), &provider.Request{}); !errors.Is(err, boom) {
		t.Errorf("error = %v, want the provider's own", err)
	}
	if got := failing.Snapshot("cli:main").Total; got != (Totals{}) {
		t.Errorf("a failed call billed %+v", got)
	}
}

func TestSessionCapStopsTheNextCallRatherThanTheLastOne(t *testing.T) {
	m, inner := meterFor(t, config.CostConfig{
		Track:  true,
		Budget: config.BudgetConfig{SessionUSD: 1.5, Period: "month"},
	}, answered("a/model", 1000, 500))

	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Chat(inSession("cli:main"), &provider.Request{})
	var be *BudgetError
	if !errors.As(err, &be) || be.Scope != "session" {
		t.Fatalf("second call error = %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("the provider was called %d times past the cap", inner.calls)
	}
	// Another conversation has its own allowance.
	if _, err := m.Chat(inSession("telegram:7"), &provider.Request{}); err != nil {
		t.Errorf("a different session was refused: %v", err)
	}
}

func TestGlobalCapCountsThePeriodItWasGiven(t *testing.T) {
	for _, period := range []string{"day", "month", "total"} {
		m, inner := meterFor(t, config.CostConfig{
			Track:  true,
			Budget: config.BudgetConfig{GlobalUSD: 1.5, Period: period},
		}, answered("a/model", 1000, 500))
		if _, err := m.Chat(inSession("cli:one"), &provider.Request{}); err != nil {
			t.Fatal(err)
		}
		// A fresh session does not escape a global cap.
		_, err := m.Chat(inSession("cli:two"), &provider.Request{})
		var be *BudgetError
		if !errors.As(err, &be) || be.Scope != period {
			t.Errorf("%s: error = %v", period, err)
		}
		if inner.calls != 1 {
			t.Errorf("%s: provider called %d times", period, inner.calls)
		}
	}
}

func TestBudgetStopSpeaksToTheUserAndIgnoresOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		scope string
		want  []string
	}{
		{"session", []string{"this session", "session_usd", "$2.00", "$1.50"}},
		{"day", []string{"today", "tomorrow", "global_usd"}},
		{"month", []string{"this month", "next month", "global_usd"}},
		{"total", []string{"in total", "global_usd"}},
	} {
		got := BudgetStop(&BudgetError{Scope: tc.scope, Spent: 2, Limit: 1.5})
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s stop = %q, want it to mention %q", tc.scope, got, want)
			}
		}
	}
	if got := BudgetStop(errors.New("something else")); got != "" {
		t.Errorf("an ordinary failure was dressed up as a cap: %q", got)
	}
	if got := BudgetStop(nil); got != "" {
		t.Errorf("no error produced a stop: %q", got)
	}
	if got := (&BudgetError{Scope: "session", Spent: 2, Limit: 1.5}).Error(); !strings.Contains(got, "budget cap reached") {
		t.Errorf("Error() = %q", got)
	}
}

func TestFormattingSaysTheNumberAtThePrecisionItDeserves(t *testing.T) {
	for v, want := range map[float64]string{0: "$0.00", 0.0023: "$0.0023", 0.01: "$0.01", 12.345: "$12.35"} {
		if got := FormatUSD(v); got != want {
			t.Errorf("FormatUSD(%v) = %q, want %q", v, got, want)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1500: "1.5k", 2_400_000: "2.4M"} {
		if got := FormatTokens(n); got != want {
			t.Errorf("FormatTokens(%d) = %q, want %q", n, got, want)
		}
	}
	if got := spendText(Totals{}, 0); got != "" {
		t.Errorf("nothing spent rendered as %q", got)
	}
	if got := spendText(Totals{Input: 1200}, 0); got != "1.2k tok" {
		t.Errorf("unpriced traffic rendered as %q", got)
	}
	if got := spendText(Totals{USD: 0.5}, 2); got != "$0.50 of $2.00" {
		t.Errorf("capped spend rendered as %q", got)
	}
}

func TestStatusTextSaysWhatTheBarAndTheTrayHaveRoomFor(t *testing.T) {
	m, _ := meterFor(t, config.CostConfig{Track: true}, answered("a/model", 1000, 500))
	if got := m.OverviewLine(); got != "" {
		t.Errorf("an unspent gateway drew %q", got)
	}
	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := m.SessionLine("cli:main"); got != "$2.00" {
		t.Errorf("session line = %q", got)
	}
	if got := m.OverviewLine(); !strings.Contains(got, "today") || !strings.Contains(got, "all-time") {
		t.Errorf("overview line = %q", got)
	}

	capped, _ := meterFor(t, config.CostConfig{
		Track:  true,
		Budget: config.BudgetConfig{SessionUSD: 10, GlobalUSD: 20, Period: "month"},
	}, answered("a/model", 1000, 500))
	if _, err := capped.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := capped.SessionLine("cli:main"); got != "$2.00 of $10.00" {
		t.Errorf("capped session line = %q", got)
	}
	if got := capped.OverviewLine(); got != "spend: $2.00 of $20.00 this month" {
		t.Errorf("capped overview line = %q", got)
	}
}

func TestOverviewLineFallsBackToTokensWhenNothingIsPriced(t *testing.T) {
	m, _ := meterFor(t, config.CostConfig{Track: true}, answered("nobody/lists-this", 600, 600))
	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	got := m.OverviewLine()
	if !strings.Contains(got, "1.2k tok") {
		t.Errorf("overview line = %q, want a token count", got)
	}
}

func TestReportAnswersWhatThisHasCost(t *testing.T) {
	m, _ := meterFor(t, config.CostConfig{
		Track:  true,
		Budget: config.BudgetConfig{SessionUSD: 10, GlobalUSD: 20, Period: "day"},
	}, answered("a/model", 1000, 500))
	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	m.inner.(*fakeChat).resp = answered("nobody/lists-this", 100, 100)
	if _, err := m.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}

	report := m.Report("cli:main")
	for _, want := range []string{
		"This session (cli:main)", "Today:", "This month:", "All time:",
		"Session cap: $10.00, $8.00 left",
		"Global cap (today): $20.00, $18.00 left",
		"By model:", "a/model", "Unpriced", "nobody/lists-this",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}
	// Models sort by spend, so the one that cost money leads.
	if got := bySpend(m.Snapshot("cli:main").Models); got[0] != "a/model" {
		t.Errorf("model order = %v", got)
	}
}

func TestReportSaysWhenNoCapAndNoBillIsPossible(t *testing.T) {
	local := NewMeter(&fakeChat{resp: answered("qwen3:8b", 100, 100)},
		NewCatalog(config.CostConfig{}, []config.Candidate{{Type: "ollama", Model: "qwen3:8b"}}, ""),
		NewLedger(""), config.CostConfig{Track: true})
	if _, err := local.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	report := local.Report("cli:main")
	if !strings.Contains(report, "No budget cap is set.") {
		t.Errorf("report = %q", report)
	}
	if !strings.Contains(report, "served locally") {
		t.Errorf("a local-only setup did not explain its zero: %q", report)
	}
	if !strings.Contains(report, "1 call") || strings.Contains(report, "1 calls") {
		t.Errorf("call count reads as %q", report)
	}

	// An override can price a model this machine serves, and then the note
	// about a zero money column would contradict the number above it.
	priced := NewMeter(&fakeChat{resp: answered("qwen3:8b", 1000, 500)},
		NewCatalog(config.CostConfig{Prices: map[string]config.Price{"qwen3:8b": {Input: 1000, Output: 2000}}},
			[]config.Candidate{{Type: "ollama", Model: "qwen3:8b"}}, ""),
		NewLedger(""), config.CostConfig{Track: true})
	if _, err := priced.Chat(inSession("cli:main"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := priced.Report("cli:main"); strings.Contains(got, "served locally") {
		t.Errorf("a report showing $2.00 also claimed nothing costs anything:\n%s", got)
	}
	if got := local.Budget(); !got.Off() {
		t.Errorf("budget = %+v, want no caps", got)
	}
}

func TestBySpendBreaksTiesPredictably(t *testing.T) {
	got := bySpend(map[string]Totals{
		"c": {Input: 5},
		"a": {Input: 5},
		"b": {Input: 50},
		"d": {USD: 1},
	})
	if strings.Join(got, ",") != "d,b,a,c" {
		t.Errorf("order = %v", got)
	}
}
