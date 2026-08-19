package cost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// catalogJSON renders the shape the model catalog publishes: rates as USD
// per token, in decimal strings.
func catalogJSON(models map[string][2]string) string {
	type pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	}
	type model struct {
		ID      string  `json:"id"`
		Pricing pricing `json:"pricing"`
	}
	var body struct {
		Data []model `json:"data"`
	}
	for id, p := range models {
		body.Data = append(body.Data, model{ID: id, Pricing: pricing{Prompt: p[0], Completion: p[1]}})
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func priceServer(t *testing.T, payload string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestLocalReadsTheEndpointNotJustTheType(t *testing.T) {
	for _, tc := range []struct {
		name string
		cand config.Candidate
		want bool
	}{
		{"a type factor knows is local", config.Candidate{Type: "ollama", Model: "qwen3:8b"}, true},
		{"lm studio", config.Candidate{Type: "lmstudio", Model: "m"}, true},
		{"a custom endpoint on loopback", config.Candidate{Type: "custom", APIBase: "http://127.0.0.1:9000/v1"}, true},
		{"a custom endpoint named localhost", config.Candidate{Type: "custom", APIBase: "http://localhost:9000/v1"}, true},
		{"a box on the LAN", config.Candidate{Type: "custom", APIBase: "http://192.168.1.9:8000/v1"}, true},
		{"an mdns name", config.Candidate{Type: "custom", APIBase: "http://tower.local:8000/v1"}, true},
		{"a hosted provider", config.Candidate{Type: "openrouter", Model: "vendor/model"}, false},
		{"a custom endpoint on the internet", config.Candidate{Type: "custom", APIBase: "https://api.example.com/v1"}, false},
		{"an unparseable endpoint", config.Candidate{Type: "custom", APIBase: "::nonsense"}, false},
		{"a type with no default base", config.Candidate{Type: "custom"}, false},
	} {
		if got := Local(tc.cand); got != tc.want {
			t.Errorf("%s: Local = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestShortIDDropsWhatVendorsDecorateWith(t *testing.T) {
	for in, want := range map[string]string{
		"anthropic/claude-sonnet-4-5":       "claude-sonnet-4-5",
		"claude-sonnet-4-5-20250929":        "claude-sonnet-4-5",
		"openai/gpt-4o-2024-11-20":          "gpt-4o",
		"deepseek/deepseek-r1:free":         "deepseek-r1",
		"Google/Gemini-3.1-Pro-Preview":     "gemini-3.1-pro-preview",
		"anthropic/claude-3-5-haiku-latest": "claude-3-5-haiku",
	} {
		if got := shortID(in); got != want {
			t.Errorf("shortID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCatalogPrefersOverridesThenLocalThenTheFetchedBook(t *testing.T) {
	srv, _ := priceServer(t, catalogJSON(map[string][2]string{
		"anthropic/claude-sonnet-4-5": {"0.000003", "0.000015"},
		"vendor/house-model":          {"0.000001", "0.000002"},
	}))
	cfg := config.CostConfig{
		Track:     true,
		PricesURL: srv.URL,
		Prices:    map[string]config.Price{"Vendor/House-Model": {Input: 99, Output: 99}},
	}
	cands := []config.Candidate{
		{Type: "openrouter", Model: "vendor/house-model"},
		{Type: "ollama", Model: "qwen3:8b"},
	}
	c := NewCatalog(cfg, cands, filepath.Join(t.TempDir(), "pricing.json"))
	if !c.Paid() {
		t.Fatal("a hosted candidate did not mark the configuration as paid")
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The override wins over the fetched rate, whatever case it was written in.
	if p, ok := c.Price("vendor/house-model"); !ok || p.Input != 99 {
		t.Errorf("override lost: %+v ok=%v", p, ok)
	}
	// A locally served model is free, and says so rather than going unpriced.
	if p, ok := c.Price("qwen3:8b"); !ok || p != (Price{}) {
		t.Errorf("local model priced %+v ok=%v", p, ok)
	}
	// Per-token rates become per-million.
	if p, ok := c.Price("anthropic/claude-sonnet-4-5"); !ok || p.Input != 3 || p.Output != 15 {
		t.Errorf("fetched price = %+v ok=%v, want $3/$15 per million", p, ok)
	}
	// A native model id finds its catalog twin through the short form.
	if p, ok := c.Price("claude-sonnet-4-5-20250929"); !ok || p.Input != 3 {
		t.Errorf("dated model id = %+v ok=%v", p, ok)
	}
	if _, ok := c.Price("some/model-nobody-lists"); ok {
		t.Error("an unlisted model came back priced")
	}
	if _, ok := c.Price("  "); ok {
		t.Error("an empty model id came back priced")
	}
}

func TestCatalogDropsShortNamesTwoVendorsClaimDifferently(t *testing.T) {
	srv, _ := priceServer(t, catalogJSON(map[string][2]string{
		"vendora/llama-3": {"0.000001", "0.000002"},
		"vendorb/llama-3": {"0.000009", "0.000009"},
		"vendorc/llama-3": {"0.000009", "0.000009"},
	}))
	c := NewCatalog(config.CostConfig{PricesURL: srv.URL}, []config.Candidate{{Type: "openrouter", Model: "x"}}, "")
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Price("llama-3"); ok {
		t.Error("a short name two vendors price differently was answered anyway")
	}
	if p, ok := c.Price("vendora/llama-3"); !ok || p.Input != 1 {
		t.Errorf("the exact id must still resolve: %+v ok=%v", p, ok)
	}
}

func TestCatalogCachesToDiskAndSkipsAFreshFetch(t *testing.T) {
	srv, hits := priceServer(t, catalogJSON(map[string][2]string{"a/b": {"0.000002", "0.000004"}}))
	path := filepath.Join(t.TempDir(), "pricing.json")
	cands := []config.Candidate{{Type: "openrouter", Model: "a/b"}}
	cfg := config.CostConfig{PricesURL: srv.URL, RefreshHours: 24}

	first := NewCatalog(cfg, cands, path)
	if err := first.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", hits.Load())
	}
	// A second refresh inside the TTL is free.
	if err := first.Refresh(context.Background()); err != nil || hits.Load() != 1 {
		t.Errorf("a fresh catalog was refetched: hits=%d err=%v", hits.Load(), err)
	}

	// A new process prices correctly before touching the network.
	second := NewCatalog(cfg, cands, path)
	if p, ok := second.Price("a/b"); !ok || p.Output != 4 {
		t.Errorf("cached price = %+v ok=%v", p, ok)
	}

	// Once the cache ages out, the next refresh goes back to the wire.
	second.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if err := second.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Errorf("a stale catalog was not refetched: hits=%d", hits.Load())
	}
}

func TestCatalogLeavesTheWireAloneWhenEverythingIsLocal(t *testing.T) {
	srv, hits := priceServer(t, catalogJSON(map[string][2]string{"a/b": {"0.000002", "0.000004"}}))
	c := NewCatalog(config.CostConfig{PricesURL: srv.URL},
		[]config.Candidate{{Type: "ollama", Model: "qwen3:8b"}, {Model: ""}}, "")
	if c.Paid() {
		t.Error("a local-only configuration reported itself as paid")
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Errorf("a local-only configuration fetched prices %d times", hits.Load())
	}
}

func TestCatalogSurvivesEveryWayTheFetchCanFail(t *testing.T) {
	cands := []config.Candidate{{Type: "openrouter", Model: "a/b"}}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	for _, tc := range []struct{ name, url, payload string }{
		{"a server that refuses", bad.URL, ""},
		{"a body that is not json", "", "not json at all"},
		{"a catalog with no priced models", "", catalogJSON(map[string][2]string{"a/b": {"free", "free"}})},
		{"a catalog with no models", "", `{"data":[]}`},
	} {
		url := tc.url
		if url == "" {
			srv, _ := priceServer(t, tc.payload)
			url = srv.URL
		}
		c := NewCatalog(config.CostConfig{PricesURL: url}, cands, "")
		if err := c.Refresh(context.Background()); err == nil {
			t.Errorf("%s: refresh reported success", tc.name)
		}
		if _, ok := c.Price("a/b"); ok {
			t.Errorf("%s: a failed fetch left prices behind", tc.name)
		}
	}

	// An unbuildable request fails before any connection is attempted.
	c := NewCatalog(config.CostConfig{PricesURL: "://"}, cands, "")
	if err := c.Refresh(context.Background()); err == nil {
		t.Error("an unparseable url was accepted")
	}
}

func TestCatalogIgnoresAnUnreadableCacheAndAnUnwritableOne(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(garbage, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := NewCatalog(config.CostConfig{}, nil, garbage); len(c.byID) != 0 {
		t.Error("a corrupt cache was adopted")
	}
	if c := NewCatalog(config.CostConfig{}, nil, filepath.Join(dir, "missing.json")); len(c.byID) != 0 {
		t.Error("a missing cache produced prices")
	}

	// A cache path inside a directory that does not exist fails the save but
	// not the prices already in memory.
	srv, _ := priceServer(t, catalogJSON(map[string][2]string{"a/b": {"0.000002", "0.000004"}}))
	c := NewCatalog(config.CostConfig{PricesURL: srv.URL},
		[]config.Candidate{{Type: "openrouter", Model: "a/b"}}, filepath.Join(dir, "nope", "pricing.json"))
	if err := c.Refresh(context.Background()); err == nil {
		t.Error("an unwritable cache path reported a clean refresh")
	}
	if _, ok := c.Price("a/b"); !ok {
		t.Error("a failed cache write threw away the fetched prices")
	}
}

func TestWatchRefreshesUntilTheContextEnds(t *testing.T) {
	payload := catalogJSON(map[string][2]string{"a/b": {"0.000002", "0.000004"}})
	fetched := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case fetched <- struct{}{}:
		default:
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	c := NewCatalog(config.CostConfig{PricesURL: srv.URL},
		[]config.Candidate{{Type: "openrouter", Model: "a/b"}}, "")
	// A clock that jumps two days per reading, so nothing is ever fresh.
	var clock atomic.Int64
	c.now = func() time.Time { return time.Unix(0, clock.Add(int64(48*time.Hour))) }
	ticks := make(chan time.Time, 1)
	ticks <- time.Now() // one wake-up; the cancel wins the next round
	c.after = func(time.Duration) <-chan time.Time { return ticks }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Watch(ctx); close(done) }()
	<-fetched // at startup
	<-fetched // and again when the tick lands
	cancel()
	<-done
}

func TestWatchKeepsGoingWhenTheFetchFails(t *testing.T) {
	c := NewCatalog(config.CostConfig{PricesURL: "http://127.0.0.1:1/nothing"},
		[]config.Candidate{{Type: "openrouter", Model: "a/b"}}, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.after = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	c.Watch(ctx) // returns rather than hanging or panicking
}
