package cost

import (
	"math"
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
)

func closeTo(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("%s = %.12f, want %.12f", what, got, want)
	}
}

// The whole point of the cache is that a hit is not billed like a miss. Pricing
// every prompt token at the plain rate over-reports a well-cached session, and
// over-reports it more the better the prefix hygiene gets.
func TestUsdUsageDiscountsCacheReads(t *testing.T) {
	p := Price{Input: 1, Output: 0}
	u := provider.Usage{PromptTokens: 1_000_000, CacheReadTokens: 900_000}

	// 100k at full rate + 900k at a tenth = 190k-equivalent, not 1M.
	closeTo(t, usdUsage(p, u), 0.19, "cached call")
	closeTo(t, usd(p, u.PromptTokens, u.CompletionTokens), 1.0, "the same call priced plainly")
}

func TestUsdUsageSurchargesCacheWrites(t *testing.T) {
	p := Price{Input: 1, Output: 0}
	u := provider.Usage{PromptTokens: 1_000_000, CacheWriteTokens: 1_000_000}
	closeTo(t, usdUsage(p, u), 1.25, "write-only call")
}

func TestUsdUsageMixesReadsWritesAndPlainInput(t *testing.T) {
	p := Price{Input: 2, Output: 4}
	u := provider.Usage{
		PromptTokens:     1_000_000,
		CompletionTokens: 500_000,
		CacheReadTokens:  600_000,
		CacheWriteTokens: 200_000,
	}
	// 200k plain + 600k×0.1 + 200k×1.25 = 510k input-equivalent.
	want := (510_000*2 + 500_000*4) / 1e6
	closeTo(t, usdUsage(p, u), want, "mixed call")
}

// A provider reporting no cache counters at all must price exactly as it did
// before any of this existed.
func TestUsdUsageWithoutCountersMatchesPlainPricing(t *testing.T) {
	p := Price{Input: 3, Output: 9}
	u := provider.Usage{PromptTokens: 4321, CompletionTokens: 765}
	closeTo(t, usdUsage(p, u), usd(p, u.PromptTokens, u.CompletionTokens), "uncached call")
}

// Counters that exceed their own total would price negative input. A provider
// that contradicts itself gets billed plainly rather than credited.
func TestUsdUsageRejectsCountersThatExceedTheTotal(t *testing.T) {
	p := Price{Input: 1, Output: 1}
	u := provider.Usage{PromptTokens: 100, CacheReadTokens: 900, CompletionTokens: 10}
	got := usdUsage(p, u)
	if got < 0 {
		t.Fatalf("priced negative: %v", got)
	}
	closeTo(t, got, usd(p, 100, 10), "inconsistent call")
}

func TestCacheHitRate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Totals
		want float64
	}{
		{"nothing recorded", Totals{}, 0},
		{"all cached", Totals{Input: 100, Cached: 100}, 1},
		{"none cached", Totals{Input: 100}, 0},
		{"most cached", Totals{Input: 1000, Cached: 750}, 0.75},
	} {
		if got := tc.in.CacheHitRate(); got != tc.want {
			t.Errorf("%s: hit rate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Cached is a subset of Input, so a status line must not count it twice.
func TestTokensExcludesCached(t *testing.T) {
	tot := Totals{Input: 100, Output: 10, Cached: 90}
	if got := tot.Tokens(); got != 110 {
		t.Errorf("Tokens() = %d, want 110", got)
	}
}

func TestTotalsAddAccumulatesCached(t *testing.T) {
	a := Totals{Input: 10, Cached: 4}
	a.add(Totals{Input: 20, Cached: 15})
	if a.Input != 30 || a.Cached != 19 {
		t.Errorf("add = %+v", a)
	}
}
