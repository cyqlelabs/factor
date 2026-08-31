package cost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// ChatProvider is the seam the meter wraps: the failover chain, or anything
// else that answers a request.
type ChatProvider interface {
	Chat(ctx context.Context, req *provider.Request) (*provider.Response, error)
}

// Meter sits between the agent loop and the provider chain. Every call that
// comes back is priced and billed to the session that made it — including
// the ones the user never asked for, like compaction — and every call that
// goes out is checked against the budget first, because the only useful
// moment to stop is before the money is spent.
type Meter struct {
	inner   ChatProvider
	catalog *Catalog
	ledger  *Ledger
	budget  config.BudgetConfig
	active  bool

	onCharge func(sessionKey, model string, t Totals, cacheWrite int)
}

// NewMeter wires a meter around inner. It stays a pass-through unless
// tracking is on or a cap is set — a cap with tracking switched off would
// otherwise be a cap that never counts anything.
func NewMeter(inner ChatProvider, catalog *Catalog, ledger *Ledger, cfg config.CostConfig) *Meter {
	return &Meter{
		inner:   inner,
		catalog: catalog,
		ledger:  ledger,
		budget:  cfg.Budget,
		active:  cfg.Track || !cfg.Budget.Off(),
	}
}

// OnCharge is told what every priced call cost, so a trace can carry the
// money beside the tools it was spent on. The meter is the only place that
// sees the model that actually answered and the price it was billed at.
func (m *Meter) OnCharge(fn func(sessionKey, model string, t Totals, cacheWrite int)) *Meter {
	if m != nil {
		m.onCharge = fn
	}
	return m
}

// Active reports whether anything is being counted.
func (m *Meter) Active() bool { return m != nil && m.active }

func (m *Meter) Chat(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if !m.active {
		return m.inner.Chat(ctx, req)
	}
	key := tools.ToolContextFrom(ctx).SessionKey
	if err := m.check(key); err != nil {
		return nil, err
	}
	resp, err := m.inner.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	m.record(key, resp)
	return resp, nil
}

// check refuses the call when a cap is already met. Spend is compared before
// the call rather than after it, so the cap is a line the agent stops at
// instead of one it discovers it has crossed.
func (m *Meter) check(sessionKey string) error {
	if m.budget.Off() {
		return nil
	}
	s := m.ledger.Snapshot(sessionKey)
	if limit := m.budget.SessionUSD; limit > 0 && s.Session.USD >= limit {
		return &BudgetError{Scope: "session", Spent: s.Session.USD, Limit: limit}
	}
	if limit := m.budget.GlobalUSD; limit > 0 {
		if spent := s.Spent(m.budget.Period).USD; spent >= limit {
			return &BudgetError{Scope: m.budget.Period, Spent: spent, Limit: limit}
		}
	}
	return nil
}

func (m *Meter) record(sessionKey string, resp *provider.Response) {
	u := resp.Usage
	if u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return // a provider that reports nothing leaves nothing to bill
	}
	t := Totals{Input: u.PromptTokens, Output: u.CompletionTokens, Cached: u.CacheReadTokens, Calls: 1}
	if p, ok := m.catalog.Price(resp.Model); ok {
		t.USD = usdUsage(p, u)
	}
	slog.Debug("call priced", "model", resp.Model, "session", sessionKey,
		"input", u.PromptTokens, "output", u.CompletionTokens,
		"cache_read", u.CacheReadTokens, "cache_write", u.CacheWriteTokens, "usd", t.USD)
	if m.onCharge != nil {
		m.onCharge(sessionKey, resp.Model, t, u.CacheWriteTokens)
	}
	m.ledger.Record(sessionKey, resp.Model, t)
}

// Snapshot reads the ledger as one session sees it.
func (m *Meter) Snapshot(sessionKey string) Snapshot {
	return m.ledger.Snapshot(sessionKey)
}

// Budget returns the caps in force.
func (m *Meter) Budget() config.BudgetConfig { return m.budget }

// Unpriced names the models that have been used but that nothing prices —
// the reason a total can read lower than the invoice.
func (m *Meter) Unpriced(s Snapshot) []string {
	var out []string
	for model := range s.Models {
		if _, ok := m.catalog.Price(model); !ok {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out
}

// BudgetError is a call refused because a cap was met.
type BudgetError struct {
	Scope string // session | day | month | total
	Spent float64
	Limit float64
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("budget cap reached: %s of a %s %s cap", FormatUSD(e.Spent), FormatUSD(e.Limit), e.Scope)
}

// Message is the sentence the user reads instead of an answer: what was
// spent, against which cap, and the two ways out of it.
func (e *BudgetError) Message() string {
	if e.Scope == "session" {
		return fmt.Sprintf("Budget cap reached: this session has spent %s of its %s cap. "+
			"Start a fresh session, or raise cost.budget.session_usd.", FormatUSD(e.Spent), FormatUSD(e.Limit))
	}
	msg := fmt.Sprintf("Budget cap reached: %s spent %s, against a %s cap. Raise cost.budget.global_usd",
		FormatUSD(e.Spent), periodWords(e.Scope), FormatUSD(e.Limit))
	switch e.Scope {
	case "day":
		return msg + ", or wait for tomorrow."
	case "month":
		return msg + ", or wait for next month."
	}
	return msg + "."
}

// BudgetStop returns the sentence to answer with when err is a budget
// refusal, and "" for every other error. A cap is a decision the user made,
// not a failure to report as one.
func BudgetStop(err error) string {
	var be *BudgetError
	if errors.As(err, &be) {
		return be.Message()
	}
	return ""
}

// periodWords says when a global bucket covers, in the words a sentence
// wants rather than the key the config uses.
func periodWords(period string) string {
	switch period {
	case "day":
		return "today"
	case "total":
		return "in total"
	}
	return "this month"
}

// ---- rendering -------------------------------------------------------------

// FormatUSD writes an amount at the precision it deserves: cents once there
// are cents to see, four places while a turn still costs less than one.
func FormatUSD(v float64) string {
	if v > 0 && v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// FormatTokens abbreviates a token count for a line that has no room to
// spell it out.
func FormatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// spendText renders one bucket: money when something priced it, raw tokens
// when nothing did, and nothing at all when nothing has been spent.
func spendText(t Totals, limit float64) string {
	switch {
	case t.USD > 0 && limit > 0:
		return FormatUSD(t.USD) + " of " + FormatUSD(limit)
	case t.USD > 0:
		return FormatUSD(t.USD)
	case t.Tokens() > 0:
		return FormatTokens(t.Tokens()) + " tok"
	}
	return ""
}

// SessionLine is the status-bar segment: what this conversation has cost,
// and the cap it is spending against when there is one.
func (m *Meter) SessionLine(sessionKey string) string {
	if !m.Active() {
		return ""
	}
	return spendText(m.ledger.Snapshot(sessionKey).Session, m.budget.SessionUSD)
}

// OverviewLine is the tray row: today against all time, or against the
// global cap when one is set — the same numbers, said the way the cap makes
// relevant.
func (m *Meter) OverviewLine() string {
	if !m.Active() {
		return ""
	}
	s := m.ledger.Snapshot("")
	if limit := m.budget.GlobalUSD; limit > 0 {
		return "spend: " + spendText(s.Spent(m.budget.Period), limit) + " " + periodWords(m.budget.Period)
	}
	total := spendText(s.Total, 0)
	if total == "" {
		return ""
	}
	day := spendText(s.Day, 0)
	if day == "" {
		day = FormatUSD(0)
	}
	return "spend: " + day + " today · " + total + " all-time"
}

// Report renders the full usage picture for the agent and the CLI.
func (m *Meter) Report(sessionKey string) string {
	if !m.Active() {
		return "Cost tracking is off (cost.track = false)."
	}
	s := m.Snapshot(sessionKey)
	var b strings.Builder
	fmt.Fprintf(&b, "This session (%s): %s\n", sessionKey, bucketWords(s.Session))
	fmt.Fprintf(&b, "Today: %s\nThis month: %s\nAll time: %s\n", bucketWords(s.Day), bucketWords(s.Month), bucketWords(s.Total))

	if caps := m.budget; !caps.Off() {
		if caps.SessionUSD > 0 {
			fmt.Fprintf(&b, "Session cap: %s, %s left\n", FormatUSD(caps.SessionUSD), FormatUSD(max(0, caps.SessionUSD-s.Session.USD)))
		}
		if caps.GlobalUSD > 0 {
			spent := s.Spent(caps.Period).USD
			fmt.Fprintf(&b, "Global cap (%s): %s, %s left\n", periodWords(caps.Period), FormatUSD(caps.GlobalUSD), FormatUSD(max(0, caps.GlobalUSD-spent)))
		}
	} else {
		b.WriteString("No budget cap is set.\n")
	}

	if models := bySpend(s.Models); len(models) > 0 {
		b.WriteString("By model:\n")
		for _, name := range models {
			fmt.Fprintf(&b, "  %s: %s\n", name, bucketWords(s.Models[name]))
		}
	}
	if unpriced := m.Unpriced(s); len(unpriced) > 0 {
		fmt.Fprintf(&b, "Unpriced (counted in tokens only): %s\n", strings.Join(unpriced, ", "))
	}
	// Only worth saying while the money column really is at zero — an
	// override can price a locally served model, and a report that shows
	// $2.00 and then claims nothing costs anything is worse than silent.
	if !m.catalog.Paid() && s.Total.USD == 0 {
		b.WriteString("Every configured model is served locally, so the money column stays at zero.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// bucketWords spells out one bucket in full: money, traffic, and how many
// calls it took.
func bucketWords(t Totals) string {
	calls := fmt.Sprintf("%d calls", t.Calls)
	if t.Calls == 1 {
		calls = "1 call"
	}
	// The cache share is worth a few characters because it is the only place
	// a broken prompt prefix shows up as a number. Everything that keeps the
	// prefix byte-stable fails silently; this ratio is what falls when it does.
	cached := ""
	if t.Cached > 0 {
		cached = fmt.Sprintf(" (%.0f%% cached)", t.CacheHitRate()*100)
	}
	return fmt.Sprintf("%s · %s in%s / %s out · %s",
		FormatUSD(t.USD), FormatTokens(t.Input), cached, FormatTokens(t.Output), calls)
}

// bySpend orders models by what they cost, falling back to traffic so the
// unpriced ones still sort sensibly among themselves.
func bySpend(models map[string]Totals) []string {
	out := make([]string, 0, len(models))
	for name := range models {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := models[out[i]], models[out[j]]
		if a.USD != b.USD {
			return a.USD > b.USD
		}
		if a.Tokens() != b.Tokens() {
			return a.Tokens() > b.Tokens()
		}
		return out[i] < out[j]
	})
	return out
}

// Watch keeps the price book current while ctx runs. A fetch that fails is a
// warning and nothing more: yesterday's prices are far better than none.
func (c *Catalog) Watch(ctx context.Context) {
	for {
		if err := c.Refresh(ctx); err != nil {
			slog.Warn("could not refresh model prices", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.after(c.ttl):
		}
	}
}
